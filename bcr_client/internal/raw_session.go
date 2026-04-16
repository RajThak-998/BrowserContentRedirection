package engine

// raw_session.go — "Dumb Pipe" ICE/DTLS/SRTP shadow session.
//
// This replaces the Pion PeerConnection-based shadowSession. Go no longer
// generates SDP, negotiates codecs, or runs a MediaEngine. Instead:
//
//   Phase 1 — Init():
//     Generate a self-signed DTLS certificate, start an ice.Agent, gather
//     candidates, and return a RTCShadowReadyPayload for the JS to munge into
//     the browser's SDP.  The SFU will then connect directly to Go's UDP port.
//
//   Phase 2 — Connect():
//     When the SFU's answer SDP arrives, extract remote ICE credentials + DTLS
//     fingerprint, perform the ICE handshake, run DTLS (role per a=setup:),
//     verify the fingerprint, derive SRTP keys, and start the decryption loop.
//
// SDP is treated as opaque text — never parsed for codecs or m= structure.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/pion/stun/v3"
)

// rawShadowSession holds all transport state for one intercepted browser
// RTCPeerConnection. It is keyed by bridgeID in the engine's rawSessions map.
type rawShadowSession struct {
	bridgeID string

	// ── Phase 1 fields (set by Init) ───────────────────────────────────────
	cert     tls.Certificate // self-signed ECDSA P-256 cert; one per session
	iceAgent *ice.Agent

	// ── Phase 2 fields (set by Connect) ────────────────────────────────────
	iceConn  *ice.Conn
	mux      *dtlsSRTPMux
	dtlsConn *dtls.Conn
	srtpCtx  *srtp.Context

	// ICE/DTLS role: true = Go was the oferer (controls ICE via Dial).
	// false = Go was the answerer (controlled via Accept).
	isOfferer bool

	// initDone is set to true (under mu) when Init() returns successfully.
	// Allows handleShadowRemote to call Connect() if Init finished first.
	initDone bool

	// Remote offer SDP stored by handleShadowRemote when SFU is the oferer.
	// Used so the answerer-path Init() can call Connect() when init finishes.
	remoteOfferSDP string

	// PT → codec capability map populated from the browser's offer/answer SDP.
	// Used by the relay layer to create TrackLocalStaticRTP with correct codecs.
	ptCodecMap map[uint8]CodecInfo

	// ICE servers captured from the browser's RTCPeerConnection config.
	iceServers []IceServer

	mu     sync.Mutex
	cancel context.CancelFunc

	// onRTPPacket is called for each successfully decrypted inbound RTP packet.
	// Set by the engine after session creation.
	onRTPPacket func(pkt *rtp.Packet)

	logf func(string, ...any)
}

func newRawShadowSession(bridgeID string, logf func(string, ...any)) *rawShadowSession {
	return &rawShadowSession{
		bridgeID:   bridgeID,
		ptCodecMap: make(map[uint8]CodecInfo),
		logf:       logf,
	}
}

// ─── Phase 1 ─────────────────────────────────────────────────────────────────

// Init generates the local transport identity (DTLS cert + ICE agent), gathers
// candidates, and returns the RTCShadowReadyPayload that the engine sends back
// to the JavaScript via RTC_SHADOW_READY.
//
// sdpType is "offer" or "answer" — it is echoed in the READY payload so the JS
// waiter can match the correct shadow response to the correct SDP flow.
func (s *rawShadowSession) Init(ctx context.Context, sdpType string) (*RTCShadowReadyPayload, error) {
	// ── Step 1: Self-signed DTLS certificate ────────────────────────────────
	// selfsign.GenerateSelfSigned uses ECDSA P-256 internally.
	// The certificate is single-use; we compute its SHA-256 fingerprint to
	// send to the JS, which munges it into the browser's SDP a=fingerprint line.
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, fmt.Errorf("generate cert: %w", err)
	}
	s.cert = cert

	// ── Step 2: SHA-256 fingerprint for the JS munge ─────────────────────────
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse self-signed cert: %w", err)
	}
	fingerprint := sha256ColonHex(x509Cert.Raw) // "AB:CD:EF:..." form

	// ── Step 3: Convert IceServers → []*stun.URI ─────────────────────────────
	stunURIs := iceServersToStunURIs(s.iceServers, s.logf, s.bridgeID)
	s.logf("[raw][%s] using %d ICE server(s)", s.bridgeID, len(stunURIs))

	// ── Step 4: Create ICE agent ──────────────────────────────────────────────
	gatherDone := make(chan struct{})
	var (
		candidateMu sync.Mutex
		candidates  []string
	)

	agent, err := ice.NewAgent(&ice.AgentConfig{
		Urls:         stunURIs,
		NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4},
	})
	if err != nil {
		return nil, fmt.Errorf("new ice agent: %w", err)
	}
	s.iceAgent = agent

	// ── Step 5: OnCandidate handler ───────────────────────────────────────────
	// A nil Candidate argument signals end-of-gathering per the pion/ice API.
	if err := agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			select {
			case <-gatherDone:
			default:
				close(gatherDone)
			}
			return
		}
		line := "a=candidate:" + c.Marshal()
		candidateMu.Lock()
		candidates = append(candidates, line)
		candidateMu.Unlock()
		s.logf("[raw][%s] gathered: %s", s.bridgeID, line)
	}); err != nil {
		return nil, fmt.Errorf("OnCandidate: %w", err)
	}

	// ── Step 6: Trigger gathering ─────────────────────────────────────────────
	if err := agent.GatherCandidates(); err != nil {
		return nil, fmt.Errorf("GatherCandidates: %w", err)
	}

	// ── Step 7: Extract local ufrag/pwd ──────────────────────────────────────
	// Pion generates these on agent creation; they are available immediately
	// before gathering completes.
	ufrag, pwd, err := agent.GetLocalUserCredentials()
	if err != nil {
		return nil, fmt.Errorf("GetLocalUserCredentials: %w", err)
	}
	s.logf("[raw][%s] local ufrag=%s sdpType=%s", s.bridgeID, ufrag, sdpType)

	// ── Step 8: Wait for gathering (3 s max) ─────────────────────────────────
	gatherCtx, gatherCancel := context.WithTimeout(ctx, 3*time.Second)
	defer gatherCancel()
	select {
	case <-gatherDone:
		s.logf("[raw][%s] ICE gathering complete", s.bridgeID)
	case <-gatherCtx.Done():
		s.logf("[raw][%s] ICE gather timeout — continuing with %d partial candidates",
			s.bridgeID, len(candidates))
	}

	candidateMu.Lock()
	candsCopy := append([]string(nil), candidates...)
	candidateMu.Unlock()

	s.mu.Lock()
	s.initDone = true
	s.mu.Unlock()

	return &RTCShadowReadyPayload{
		BridgeID:        s.bridgeID,
		SDPType:         sdpType,
		ICEUfrag:        ufrag,
		ICEPwd:          pwd,
		DTLSFingerprint: "sha-256 " + fingerprint,
		LocalIP:         detectLocalIPv4(),
		Candidates:      candsCopy,
		GeneratedAt:     time.Now().UnixMilli(),
		ExpiresAt:       time.Now().Add(60 * time.Second).UnixMilli(),
	}, nil
}

// ─── Phase 2 ─────────────────────────────────────────────────────────────────

// Connect runs after the SFU's SDP (offer or answer) arrives. It:
//  1. Extracts the remote ICE credentials from remoteSDP.
//  2. Performs the ICE handshake (Dial if Go is offerer, Accept if answerer).
//  3. Parses remote candidates from remoteSDP and adds them to the agent.
//  4. Starts the DTLS/SRTP packet mux.
//  5. Performs the DTLS handshake at the role dictated by a=setup: in remoteSDP.
//  6. Verifies the remote DTLS fingerprint.
//  7. Derives SRTP inbound keys and starts the decryption goroutine.
func (s *rawShadowSession) Connect(ctx context.Context, remoteSDP string) error {
	// ── Step 1: Remote ICE credentials ───────────────────────────────────────
	remoteUfrag, remotePwd, remoteFP := ExtractShadowCredentials(remoteSDP)
	if remoteUfrag == "" || remotePwd == "" {
		return fmt.Errorf("could not extract remote ICE credentials from SDP")
	}
	s.logf("[raw][%s] remote ufrag=%s fp_prefix=%s...", s.bridgeID, remoteUfrag, safePrefix(remoteFP, 16))

	// ── Step 2: Add remote candidates parsed from the SDP ────────────────────
	// Teams SFU typically embeds its candidates inline in the answer (non-trickle).
	// Trickle candidates arrive later via RTC_SHADOW_ICE_CANDIDATE packets.
	s.addRemoteSdpCandidates(remoteSDP)

	// ── Step 3: ICE handshake ─────────────────────────────────────────────────
	var (
		iceConn *ice.Conn
		err     error
	)
	if s.isOfferer {
		s.logf("[raw][%s] ICE Dial (controlling — Go was oferer)", s.bridgeID)
		iceConn, err = s.iceAgent.Dial(ctx, remoteUfrag, remotePwd)
	} else {
		s.logf("[raw][%s] ICE Accept (controlled — Go was answerer)", s.bridgeID)
		iceConn, err = s.iceAgent.Accept(ctx, remoteUfrag, remotePwd)
	}
	if err != nil {
		return fmt.Errorf("ICE handshake: %w", err)
	}
	s.iceConn = iceConn
	s.logf("[raw][%s] ICE connected local=%s remote=%s", s.bridgeID, iceConn.LocalAddr(), iceConn.RemoteAddr())

	// ── Step 4: DTLS/SRTP mux ────────────────────────────────────────────────
	mux := newDTLSSRTPMux(iceConn, s.logf, s.bridgeID)
	s.mux = mux

	// ── Step 5: DTLS handshake ────────────────────────────────────────────────
	// Role is determined by a=setup: in the remote SDP:
	//   a=setup:active  → remote initiates ClientHello → Go is Server
	//   a=setup:passive → Go initiates ClientHello     → Go is Client
	//   default         → Server (Teams almost always advertises "active")
	role := parseDTLSRole(remoteSDP)
	s.logf("[raw][%s] DTLS role: %s (from a=setup: in remote SDP)", s.bridgeID, role)

	dtlsCfg := &dtls.Config{
		Certificates:           []tls.Certificate{s.cert},
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{dtls.SRTP_AES128_CM_HMAC_SHA1_80},
		InsecureSkipVerify:     true, // fingerprint verified manually in Step 6
	}

	dtlsPipe := mux.DTLSPipe()
	rAddr := iceConn.RemoteAddr()

	var dtlsConn *dtls.Conn
	switch role {
	case "server":
		dtlsConn, err = dtls.Server(dtlsPipe, rAddr, dtlsCfg)
	default: // "client"
		dtlsConn, err = dtls.Client(dtlsPipe, rAddr, dtlsCfg)
	}
	if err != nil {
		return fmt.Errorf("DTLS handshake (%s): %w", role, err)
	}
	s.dtlsConn = dtlsConn
	s.logf("[raw][%s] DTLS handshake complete", s.bridgeID)

	// ── Step 6: Fingerprint verification ─────────────────────────────────────
	if err := s.verifyRemoteFingerprint(dtlsConn, remoteFP); err != nil {
		_ = dtlsConn.Close()
		return fmt.Errorf("fingerprint verify: %w", err)
	}
	s.logf("[raw][%s] remote DTLS fingerprint verified", s.bridgeID)

	// ── Step 7: SRTP key derivation ───────────────────────────────────────────
	// ExportKeyingMaterial is on the dtls.State value returned by ConnectionState().
	state, ok := dtlsConn.ConnectionState()
	if !ok {
		return fmt.Errorf("DTLS connection state not available after handshake")
	}

	goIsClient := role == "client"
	srtpCtx, err := deriveSRTPContext(&state, goIsClient, s.logf, s.bridgeID)
	if err != nil {
		return fmt.Errorf("derive SRTP context: %w", err)
	}
	s.srtpCtx = srtpCtx
	s.logf("[raw][%s] SRTP context ready, starting read loop", s.bridgeID)

	// ── Step 8: SRTP read loop ────────────────────────────────────────────────
	loopCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	go s.srtpReadLoop(loopCtx, mux.SRTPChan())

	return nil
}

// AddRemoteIceCandidate trickles a single candidate from the SFU into the agent.
// Called by handleShadowICECandidate when RTC_SHADOW_ICE_CANDIDATE arrives.
func (s *rawShadowSession) AddRemoteIceCandidate(raw string) {
	// Strip any SDP attribute prefix so we get the bare candidate string.
	raw = strings.TrimPrefix(raw, "a=candidate:")
	raw = strings.TrimPrefix(raw, "candidate:")
	raw = strings.TrimRight(raw, "\r")

	c, err := ice.UnmarshalCandidate(raw)
	if err != nil {
		s.logf("[raw][%s] UnmarshalCandidate failed: %v (raw=%q)", s.bridgeID, err, raw)
		return
	}
	if err := s.iceAgent.AddRemoteCandidate(c); err != nil {
		s.logf("[raw][%s] AddRemoteCandidate failed: %v", s.bridgeID, err)
		return
	}
	s.logf("[raw][%s] remote ICE candidate added: %s", s.bridgeID, raw[:min(len(raw), 60)])
}

// Close tears down the session. Safe to call multiple times.
func (s *rawShadowSession) Close() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if s.dtlsConn != nil {
		_ = s.dtlsConn.Close()
	}
	if s.iceAgent != nil {
		_ = s.iceAgent.Close()
	}
	s.logf("[raw][%s] session closed", s.bridgeID)
}

// ─── SRTP Read Loop ───────────────────────────────────────────────────────────

func (s *rawShadowSession) srtpReadLoop(ctx context.Context, srtpCh <-chan []byte) {
	s.logf("[raw][%s] SRTP read loop started", s.bridgeID)
	defer s.logf("[raw][%s] SRTP read loop stopped", s.bridgeID)

	for {
		select {
		case <-ctx.Done():
			return
		case encrypted, ok := <-srtpCh:
			if !ok {
				return
			}
			s.decryptAndDispatch(encrypted)
		}
	}
}

func (s *rawShadowSession) decryptAndDispatch(encrypted []byte) {
	if len(encrypted) < 12 {
		return // too short to be a valid RTP packet
	}

	// Parse the header first — DecryptRTP needs it to locate the auth tag.
	var header rtp.Header
	if _, err := header.Unmarshal(encrypted); err != nil {
		// Likely RTCP (PT 200-207) or corrupt packet — drop silently.
		return
	}

	decrypted, err := s.srtpCtx.DecryptRTP(nil, encrypted, &header)
	if err != nil {
		// Auth failures on window-edge packets are expected; drop silently.
		return
	}

	if s.onRTPPacket == nil {
		return
	}

	// Unmarshal the full decrypted RTP packet and hand it to the relay layer.
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(decrypted); err != nil {
		return
	}
	s.onRTPPacket(pkt)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// addRemoteSdpCandidates parses a=candidate: lines from an SDP blob and adds
// them to the ice.Agent. Teams SFU typically includes all candidates inline in
// its answer (non-trickle), so this must be called before agent.Dial/Accept.
func (s *rawShadowSession) addRemoteSdpCandidates(sdp string) {
	added := 0
	for _, line := range strings.Split(sdp, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "a=candidate:") {
			continue
		}
		raw := strings.TrimPrefix(trimmed, "a=candidate:")
		raw = strings.TrimRight(raw, "\r")
		c, err := ice.UnmarshalCandidate(raw)
		if err != nil {
			s.logf("[raw][%s] addRemoteSdpCandidates: unmarshal failed: %v", s.bridgeID, err)
			continue
		}
		if err := s.iceAgent.AddRemoteCandidate(c); err != nil {
			s.logf("[raw][%s] addRemoteSdpCandidates: AddRemoteCandidate failed: %v", s.bridgeID, err)
			continue
		}
		added++
	}
	s.logf("[raw][%s] added %d remote SDP candidates", s.bridgeID, added)
}

// parseDTLSRole reads a=setup: from the SDP and returns the DTLS role Go should use.
//   a=setup:active  → remote initiates ClientHello → Go is Server  ("server")
//   a=setup:passive → Go initiates ClientHello     → Go is Client  ("client")
//   a=setup:actpass → ambiguous, default to server (Teams usually sends "active")
//   missing         → default to server
func parseDTLSRole(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "a=setup:") {
			continue
		}
		val := strings.ToLower(strings.TrimRight(
			strings.TrimPrefix(trimmed, "a=setup:"), "\r"))
		switch val {
		case "passive":
			return "client"
		case "active":
			return "server"
		default:
			return "server" // actpass or holdon → default to server
		}
	}
	return "server" // no a=setup: found — Teams is almost always active
}

// verifyRemoteFingerprint checks the DTLS peer cert against the SDP fingerprint.
// remoteFP may include the algorithm prefix ("sha-256 AB:CD:...") or be bare.
func (s *rawShadowSession) verifyRemoteFingerprint(conn *dtls.Conn, remoteFP string) error {
	state, ok := conn.ConnectionState()
	if !ok {
		return fmt.Errorf("ConnectionState not available for fingerprint check")
	}
	if len(state.PeerCertificates) == 0 {
		return fmt.Errorf("no peer certificates in DTLS state")
	}

	// state.PeerCertificates entries are raw DER bytes ([][]byte).
	parsed, err := x509.ParseCertificate(state.PeerCertificates[0])
	if err != nil {
		return fmt.Errorf("parse peer cert DER: %w", err)
	}

	actualFP := sha256ColonHex(parsed.Raw)

	// Normalise expected fingerprint: strip algorithm prefix, uppercase.
	expectedFP := remoteFP
	if idx := strings.Index(remoteFP, " "); idx != -1 {
		expectedFP = remoteFP[idx+1:]
	}
	expectedFP = strings.ToUpper(strings.TrimSpace(expectedFP))

	if !strings.EqualFold(actualFP, expectedFP) {
		return fmt.Errorf("DTLS fingerprint mismatch:\n  got:  %s\n  want: %s",
			safePrefix(actualFP, 40), safePrefix(expectedFP, 40))
	}
	return nil
}

// sha256ColonHex computes SHA-256 of data and formats as "AB:CD:EF:..."
// (uppercase, colon-separated byte pairs) matching the SDP a=fingerprint format.
func sha256ColonHex(data []byte) string {
	digest := sha256.Sum256(data)
	pairs := make([]string, len(digest))
	for i, b := range digest {
		pairs[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(pairs, ":")
}

// deriveSRTPContext extracts SRTP master keys from the DTLS connection state
// per RFC 5764 §4.2 using the TLS keying material exporter and creates a
// pion/srtp decryption context for inbound media.
//
// Key material layout for AES_128_CM_HMAC_SHA1_80 (60 bytes):
//
//	material[0:16]  client_write_SRTP_master_key
//	material[16:32] server_write_SRTP_master_key
//	material[32:46] client_write_SRTP_master_salt
//	material[46:60] server_write_SRTP_master_salt
//
// To decrypt inbound media FROM the SFU (which is the sender):
//
//	Go is DTLS client: SFU is DTLS server → SFU encrypts with server_write_key/salt
//	Go is DTLS server: SFU is DTLS client → SFU encrypts with client_write_key/salt
func deriveSRTPContext(state *dtls.State, goIsClient bool, logf func(string, ...any), bridgeID string) (*srtp.Context, error) {
	const (
		exporterLabel = "EXTRACTOR-dtls_srtp"
		materialLen   = 60 // 2×(16-byte key + 14-byte salt)
	)

	material, err := state.ExportKeyingMaterial(exporterLabel, nil, materialLen)
	if err != nil {
		return nil, fmt.Errorf("ExportKeyingMaterial: %w", err)
	}
	if len(material) != materialLen {
		return nil, fmt.Errorf("unexpected material length: got %d, want %d", len(material), materialLen)
	}

	var inboundKey, inboundSalt []byte
	if goIsClient {
		// SFU is DTLS server; it sends with server_write_key + server_write_salt.
		inboundKey = material[16:32]
		inboundSalt = material[46:60]
	} else {
		// SFU is DTLS client; it sends with client_write_key + client_write_salt.
		inboundKey = material[0:16]
		inboundSalt = material[32:46]
	}

	logf("[raw][%s] SRTP inbound key[0:4]=%x salt[0:4]=%x goIsClient=%v",
		bridgeID, inboundKey[:4], inboundSalt[:4], goIsClient)

	ctx, err := srtp.CreateContext(
		inboundKey, inboundSalt,
		srtp.ProtectionProfileAes128CmHmacSha1_80,
	)
	if err != nil {
		return nil, fmt.Errorf("srtp.CreateContext: %w", err)
	}
	return ctx, nil
}

// iceServersToStunURIs converts engine IceServer entries to []*stun.URI for
// ice.AgentConfig.Urls. Malformed URLs are skipped with a warning.
func iceServersToStunURIs(servers []IceServer, logf func(string, ...any), bridgeID string) []*stun.URI {
	var out []*stun.URI
	for _, srv := range servers {
		for _, rawURL := range srv.URLs {
			uri, err := stun.ParseURI(rawURL)
			if err != nil {
				logf("[raw][%s] skipping malformed ICE URL %q: %v", bridgeID, rawURL, err)
				continue
			}
			if srv.Username != "" {
				uri.Username = srv.Username
			}
			if srv.Credential != "" {
				uri.Password = srv.Credential
			}
			out = append(out, uri)
		}
	}
	return out
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
