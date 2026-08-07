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
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/pion/stun/v3"
)

type rawSessionState int

const (
	stateNew rawSessionState = iota
	stateInit
	stateReady
	stateConnecting
	stateConnected
	stateClosed
)

// DisableTransportStateReuse is a diagnostic flag to disable DTLS certificate and ICE credential reuse across connections/reconnects.
var DisableTransportStateReuse = false

// Cached DTLS identity. The self-signed certificate is generated once per
// process and reused for every session, so its SHA-256 fingerprint — the value
// munged into the SDP the SFU receives — stays stable across reconnects and
// renegotiations. Generating a fresh certificate per session would change the
// advertised fingerprint mid-call and break the peer's verification.
var (
	globalCert            tls.Certificate
	globalCertFingerprint string
	globalCertErr         error
)

func init() {
	globalCert, globalCertErr = selfsign.GenerateSelfSigned()
	if globalCertErr == nil && len(globalCert.Certificate) > 0 {
		x509Cert, err := x509.ParseCertificate(globalCert.Certificate[0])
		if err == nil {
			globalCertFingerprint = sha256ColonHex(x509Cert.Raw)
		} else {
			globalCertErr = err
		}
	}
}

// ─── RTCP Tracking Types ─────────────────────────────────────────────────────

// srRecord stores the last Sender Report received from a given SSRC.
// Used to compute LSR and DLSR fields in our Receiver Reports.
type srRecord struct {
	ntpTime    uint64    // Full 64-bit NTP timestamp from the SR
	receivedAt time.Time // Wall-clock time when we received this SR
}

// seqTracker tracks the highest sequence number + cycle count for one SSRC
// per RFC 3550 §A.1. Also maintains statistics needed for RFC 3550 §6.4.1
// Receiver Report fields (fraction-lost, cumulative lost, jitter).
type seqTracker struct {
	maxSeq uint16 // Highest seq number received
	cycles uint16 // Number of seq number wrap-arounds
	total  uint32 // Total packets received (for diagnostics)

	// ── RFC 3550 §6.4.1 / §A.3 statistics for Receiver Report ───────
	baseSeq     uint16 // First sequence number received
	expectedPrior uint32 // expected packets at last RR
	receivedPrior uint32 // received packets at last RR
	received    uint32 // packets received (distinct from total to handle init)

	// Interarrival jitter (RFC 3550 §6.4.1 / §A.8)
	jitter      float64 // current jitter estimate (in timestamp units)
	lastTransit int64   // last transit time (arrivalTS - rtpTS) in clock units
	jitterInited bool
	clockRate   uint32  // RTP clock rate for this SSRC (e.g. 90000 for video, 48000 for audio)
}

const (
	// seqLateWrapThreshold defines how far behind maxSeq a sequence number
	// can be (via signed distance) before it's classified as a late wrap-around
	// rather than a reorder. RFC 3550 §A.1 uses MAX_MISORDER=100 for the
	// forward direction; for the backward direction we need to distinguish
	// "very old packet" from "genuine wrap" by checking if diff is in the
	// extreme negative range of int16 (i.e., seq is near 65535 and maxSeq
	// is near 0). Values below -(2^15 - 1000) ≈ -31768 indicate this.
	seqLateWrapThreshold = -(1<<15 - 1000) // -31768
)

// updateSeq updates the tracker with a new sequence number, handling wrap-around.
func (t *seqTracker) updateSeq(seq uint16) {
	if t.total == 0 {
		t.maxSeq = seq
		t.baseSeq = seq
		t.total = 1
		t.received = 1
		return
	}
	diff := int16(seq - t.maxSeq)
	if diff > 0 {
		// Forward progression (possibly with small gap)
		if seq < t.maxSeq {
			// Wrap-around: seq < maxSeq but diff > 0 means seq crossed 65535→0
			t.cycles++
		}
		t.maxSeq = seq
	} else if diff < int16(seqLateWrapThreshold) {
		// Late wrap-around: maxSeq is near 0, seq is near 65535
		// This is a very old packet — do not update maxSeq or cycles
	}
	// else: duplicate or reordered within window — ignore
	t.total++
	t.received++
}

// updateJitter computes interarrival jitter per RFC 3550 §A.8.
// arrivalTime is time.Now().UnixMicro(), rtpTimestamp is the RTP timestamp.
func (t *seqTracker) updateJitter(arrivalTimeUs int64, rtpTimestamp uint32) {
	if t.clockRate == 0 {
		return // can't compute without clock rate
	}
	// Convert arrival time to RTP timestamp units.
	// Use relative arrival in microseconds → convert to clock units.
	arrivalInClockUnits := arrivalTimeUs * int64(t.clockRate) / 1_000_000
	transit := arrivalInClockUnits - int64(rtpTimestamp)

	if !t.jitterInited {
		t.lastTransit = transit
		t.jitterInited = true
		return
	}

	d := transit - t.lastTransit
	t.lastTransit = transit
	if d < 0 {
		d = -d
	}
	// RFC 3550 §A.8: J(i) = J(i-1) + (|D(i-1,i)| - J(i-1))/16
	t.jitter += (float64(d) - t.jitter) / 16.0
}

// computeRRStats returns (fractionLost, cumulativeLost) per RFC 3550 §6.4.1.
// This should be called once per RR interval (every heartbeat tick).
func (t *seqTracker) computeRRStats() (fractionLost uint8, cumulativeLost uint32) {
	extendedMax := uint32(t.cycles)<<16 | uint32(t.maxSeq)
	expected := extendedMax - uint32(t.baseSeq) + 1

	// Interval calculations
	expectedInterval := expected - t.expectedPrior
	receivedInterval := t.received - t.receivedPrior

	// Save for next interval
	t.expectedPrior = expected
	t.receivedPrior = t.received

	// Cumulative lost (capped at 24-bit max per RFC)
	lostTotal := int64(expected) - int64(t.received)
	if lostTotal < 0 {
		lostTotal = 0
	}
	if lostTotal > 0x00FFFFFF {
		lostTotal = 0x00FFFFFF
	}
	cumulativeLost = uint32(lostTotal)

	// Fraction lost (this interval)
	if expectedInterval > 0 && receivedInterval <= expectedInterval {
		lostInterval := expectedInterval - receivedInterval
		fractionLost = uint8((lostInterval << 8) / expectedInterval)
	}

	return
}

// extended returns the 32-bit extended sequence number for RR.LastSequenceNumber.
func (t *seqTracker) extended() uint32 {
	return (uint32(t.cycles) << 16) | uint32(t.maxSeq)
}

// twccEntry records a single transport-wide sequence number and its arrival time.
type twccEntry struct {
	seq       uint16
	arrivalUs int64 // time.Now().UnixMicro() at reception
}

// twccTracker accumulates transport-wide sequence numbers between feedback
// ticks, then drains them into TWCC feedback packets.
type twccTracker struct {
	mu         sync.Mutex
	entries    []twccEntry
	fbPktCount uint8 // Monotonically incrementing per TWCC feedback sent
}

// add records a received transport-wide sequence number.
func (t *twccTracker) add(seq uint16, arrivalUs int64) {
	t.mu.Lock()
	t.entries = append(t.entries, twccEntry{seq: seq, arrivalUs: arrivalUs})
	t.mu.Unlock()
}

// drain returns all accumulated entries and clears the buffer.
// Returns the current fbPktCount and increments it.
func (t *twccTracker) drain() ([]twccEntry, uint8) {
	t.mu.Lock()
	pending := t.entries
	t.entries = make([]twccEntry, 0, cap(pending))
	fb := t.fbPktCount
	t.fbPktCount++
	t.mu.Unlock()
	return pending, fb
}

// rawShadowSession holds all transport state for one intercepted browser
// RTCPeerConnection. It is keyed by bridgeID in the engine's rawSessions map.
type rawShadowSession struct {
	bridgeID string

	// ── Phase 1 fields (set by Init) ───────────────────────────────────────
	cert     tls.Certificate // self-signed ECDSA P-256 cert; one per session
	iceAgent *ice.Agent

	// ── Phase 2 fields (set by Connect) ────────────────────────────────────
	iceConn  *ice.Conn
	splitter *iceConnSplitter
	dtlsConn *dtls.Conn
	srtpCtx  *srtp.Context
	srtcpCtx *srtp.Context

	// Video SSRC tracking for RTCP PLI (Picture Loss Indication).
	// PLI is required to trigger the SFU to send video keyframes.
	videoSSRCs   map[uint32]bool
	videoSSRCsMu sync.Mutex

	// Track the highest sequence number + cycles received for each incoming SSRC.
	// Used for constructing valid RTCP Receiver Reports with proper extended seq.
	incomingSeqs   map[uint32]*seqTracker
	incomingSeqsMu sync.Mutex

	// ── Sender Report tracking (for LSR/DLSR in our Receiver Reports) ─────
	lastSRBySSRC   map[uint32]srRecord
	lastSRBySSRCMu sync.Mutex

	// ── TWCC (Transport-Wide Congestion Control) ─────────────────────────
	twcc             *twccTracker
	transportCCExtID uint8  // RTP header extension ID for transport-cc

	// ── SDES CNAME (extracted from VDI browser's local SDP) ──────────────
	localCNAME string

	state rawSessionState

	// generation is an atomically-incrementing token attached to every async
	// operation. Any goroutine holding a stale token self-terminates.
	generation uint32

	// lastRemoteSDPHash is the SDP hash of the SDP most recently accepted into
	// stateConnecting.  Used to suppress duplicate SHADOW_REMOTE events.
	lastRemoteSDPHash string

	// ICE/DTLS role: true = Go was the oferer (controls ICE via Dial).
	// false = Go was the answerer (controlled via Accept).
	isOfferer bool

	// Remote offer SDP stored by handleShadowRemote when SFU is the oferer.
	// Used so the answerer-path Init() can call Connect() when init finishes.
	remoteOfferSDP string

	// connectedRemoteSDP is the remote SDP this session's ICE agent is actually
	// authenticating against — the one handed to Connect(). It is the correct
	// baseline for deciding whether a later renegotiation moved the peer's
	// transport, and unlike remoteOfferSDP it is set on both the offerer and the
	// answerer path.
	connectedRemoteSDP string

	// localDTLSSetup is the a=setup: value from the browser's own SDP — the
	// role we advertised to the SFU. Authoritative for choosing our DTLS role
	// whenever it is concrete ("active"/"passive"); see resolveDTLSRole.
	localDTLSSetup string

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

	// onLateCandidate is called for ICE candidates that arrive after SHADOW_READY
	// has been sent (e.g. srflx/relay candidates on slow networks). Set by the
	// engine before Init() is called so no candidates are missed.
	onLateCandidate func(candidate string)
	readySentCount  int // number of candidates included in SHADOW_READY

	// localCandidateKeys holds an addr/port/component key for every candidate
	// this session has gathered, so AddRemoteIceCandidate can recognise one of
	// our own coming back at us and refuse to pair it against itself.
	//
	// This guards a loop that actually happened: bootstrap.js relayed each
	// downstream RTC_SHADOW_ICE_CANDIDATE straight back upstream, so every
	// candidate we trickled returned 2-3ms later as a "remote" one. The relay
	// is fixed at the source, but a single misrouted message is enough to
	// poison the checklist silently, so the receiving end checks too.
	//
	// Keyed on the tuple rather than the raw line because the extension
	// rewrites raddr to 0.0.0.0 before dispatching (scrubCandidateRaddr), so a
	// string comparison would miss exactly the relay candidates that matter.
	localCandidateKeys map[string]bool

	// onGatheringComplete is called once, when no further candidates will be
	// trickled. The extension turns this into the end-of-candidates event that
	// tells Teams gathering is finished. Teams' JS may hold the offer until it
	// sees that event, so this MUST fire even if gathering never completes —
	// watchGatheringComplete enforces a deadline for exactly that reason.
	onGatheringComplete func()
	gatheringDoneOnce   sync.Once

	// localCredentials stores the generated ICE/DTLS transport credentials.
	// Used to respond to renegotiation SHADOW_LOCAL events.
	localCredentials *RTCShadowReadyPayload

	// localSenderSSRC is extracted from the local SDP to be used as the
	// SenderSSRC in outbound RTCP packets (typically the audio SSRC).
	localSenderSSRC uint32

	// localVideoSSRC is the video media SSRC from our local SDP answer.
	// Used as SenderSSRC in NACK RTCP for video streams so the SFU
	// recognises us as the video receiver (not the audio participant).
	localVideoSSRC uint32

	// srtcpWriteMu serialises all SRTCP encrypt+write operations.
	// The SRTCP context maintains an internal index counter; concurrent
	// EncryptRTCP calls (heartbeat, PLI, NACK) cause index collisions
	// that make the SFU reject packets silently.
	srtcpWriteMu sync.Mutex

	// ── Deep Observability (diag.go) ─────────────────────────────────────
	ssrcReg    *ssrcRegistry // per-SSRC stream state
	rtxMapping *rtxMap       // PT→apt RTX mappings from SDP
	nackSt     *nackState    // missing seq sets per SSRC

	// rtxSSRCtoMedia maps RTX SSRC → media SSRC from a=ssrc-group:FID lines.
	// Populated by ExtractFIDGroups on every SDP parse. Used by decapsulateRTX.
	rtxSSRCtoMedia   map[uint32]uint32
	rtxSSRCMu        sync.Mutex

	// RTCP counters for [MEDIA-SUMMARY] (incremented under rtcpMu).
	rtcpMu       sync.Mutex
	rtcpRRCount  int64
	rtcpREMBCount int64
	rtcpPLICount  int64
	rtcpNACKCount int64

	// pliLoggedSSRCs remembers which SSRCs have already had an on-demand PLI
	// reported, so the log carries one line per stream rather than one per PLI.
	pliLoggedSSRCs map[uint32]bool

	sessionStart time.Time // when SRTP came up

	logf func(string, ...any)
}

func newRawShadowSession(bridgeID string, logf func(string, ...any)) *rawShadowSession {
	return &rawShadowSession{
		bridgeID:           bridgeID,
		ptCodecMap:         make(map[uint8]CodecInfo),
		localCandidateKeys: make(map[string]bool),
		lastSRBySSRC:    make(map[uint32]srRecord),
		twcc:            &twccTracker{entries: make([]twccEntry, 0, 512)},
		logf:            logf,
		state:           stateNew,
		ssrcReg:         newSSRCRegistry(),
		rtxMapping:      newRTXMap(),
		nackSt:          newNACKState(),
		rtxSSRCtoMedia:  make(map[uint32]uint32),
		sessionStart:    time.Now(),
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
	s.mu.Lock()
	if s.state != stateNew {
		s.mu.Unlock()
		return nil, fmt.Errorf("Init called in wrong state: %v", s.state)
	}
	s.state = stateInit
	s.mu.Unlock()

	// ── Step 1: DTLS certificate generation or reuse ────────────────────────
	s.mu.Lock()
	cert := s.cert
	var reuseCreds bool
	var reuseUfrag, reusePwd string
	if DisableTransportStateReuse {
		s.logf("[raw][%s] [VDI-DEBUG] Transport state reuse is disabled, forcing fresh DTLS cert and ICE credentials", s.bridgeID)
	} else if len(cert.Certificate) > 0 {
		s.logf("[raw][%s] reusing existing DTLS certificate", s.bridgeID)
		if s.localCredentials != nil {
			reuseCreds = true
			reuseUfrag = s.localCredentials.ICEUfrag
			reusePwd = s.localCredentials.ICEPwd
		}
	}
	s.mu.Unlock()

	if !reuseCreds {
		if globalCertErr != nil {
			return nil, fmt.Errorf("pre-generated global cert failed: %w", globalCertErr)
		}
		cert = globalCert
		s.mu.Lock()
		s.cert = cert
		s.mu.Unlock()
	}

	// ── Step 2: SHA-256 fingerprint for the JS munge ─────────────────────────
	var fingerprint string
	if len(cert.Certificate) > 0 && len(globalCert.Certificate) > 0 && cert.PrivateKey == globalCert.PrivateKey {
		fingerprint = globalCertFingerprint
	} else {
		x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("parse self-signed cert: %w", err)
		}
		fingerprint = sha256ColonHex(x509Cert.Raw)
	}

	// ── Step 3: Convert IceServers → []*stun.URI ─────────────────────────────
	// Always merge bcr_client's OWN ICE servers with whatever Teams supplied, so a
	// routable srflx/relay candidate can be gathered even when Teams provided none
	// (iceServersCount=0 because edge.skype.com/trap/tokens was reset by the VDI).
	own := ownIceServers()
	merged := append(append([]IceServer{}, s.iceServers...), own...)
	stunURIs := iceServersToStunURIs(merged, s.logf, s.bridgeID)
	urlNames := make([]string, 0, len(stunURIs))
	for _, u := range stunURIs {
		urlNames = append(urlNames, u.String())
	}
	s.logf("[raw][%s] ICE servers: %d url(s) (browser=%d own=%d) %s",
		s.bridgeID, len(stunURIs), len(s.iceServers), len(own), strings.Join(urlNames, " "))

	// ── Step 4: Create ICE agent ──────────────────────────────────────────────
	gatherDone := make(chan struct{})
	// srflxReady closes as soon as the first server-reflexive candidate is in
	// the candidate list. That candidate is the only one the SFU can reach, so
	// it is the real signal that the offer is ready to send — see Step 8.
	srflxReady := make(chan struct{})
	var (
		candidateMu sync.Mutex
		candidates  []string
		seenSrflx   = make(map[string]bool) // dedupe srflx by mapped public IP
		srflxOnce   sync.Once
	)

	agentConfig := &ice.AgentConfig{
		Urls:         stunURIs,
		NetworkTypes: []ice.NetworkType{
			ice.NetworkTypeUDP4,
			ice.NetworkTypeTCP4,
		},
		// Keep link-local (APIPA, 169.254/16) interfaces out of the agent
		// entirely. Filtering them in OnCandidate only kept them out of the
		// SDP — pion had already bound a socket on the interface and kept it
		// as a local candidate, so every connectivity-check round tried to
		// send from it and Windows answered WSAENETUNREACH ("a socket
		// operation was attempted to an unreachable network") for each of the
		// ~7 remote candidates, several times a second, for the whole
		// checking phase. The address is unroutable off the machine, so no
		// pair using it could ever succeed.
		IPFilter: func(ip net.IP) bool {
			return !ip.IsLinkLocalUnicast()
		},
	}
	if reuseCreds {
		agentConfig.LocalUfrag = reuseUfrag
		agentConfig.LocalPwd = reusePwd
		s.logf("[raw][%s] reusing existing ICE credentials: ufrag=%s", s.bridgeID, reuseUfrag)
	}
	// pion/ice's own logging. At Debug it emits a line per candidate pair per
	// connectivity-check round (plus every turnc allocation step), which buries
	// our own trace — so it is opt-in via BCR_ICE_DEBUG=1 and otherwise reports
	// warnings and errors only.
	{
		lf := logging.NewDefaultLoggerFactory()
		lf.Writer = log.Default().Writer()
		if envEnabled("BCR_ICE_DEBUG") {
			lf.DefaultLogLevel = logging.LogLevelDebug
			s.logf("[raw][%s] verbose pion/ice tracing enabled (BCR_ICE_DEBUG)", s.bridgeID)
		} else {
			lf.DefaultLogLevel = logging.LogLevelWarn
		}
		agentConfig.LoggerFactory = lf
	}

	agent, err := ice.NewAgent(agentConfig)
	if err != nil {
		return nil, fmt.Errorf("new ice agent: %w", err)
	}
	s.iceAgent = agent

	agent.OnConnectionStateChange(func(state ice.ConnectionState) {
		s.logf("[raw][%s] ICE connection state changed: %s", s.bridgeID, state.String())
	})

	// Fires only when a pair is actually nominated, so it is silent on a failing
	// call and pairs with the checklist sampler: the sampler shows what was
	// tried, this shows what won.
	agent.OnSelectedCandidatePairChange(func(local, remote ice.Candidate) {
		s.logf("[raw][%s] ICE selected pair: %s -> %s", s.bridgeID, local.String(), remote.String())
	})

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
		// Link-local (APIPA, 169.254.0.0/16) addresses are not routable off the
		// machine. In the VDI the 169.254.x host candidate produced nothing but
		// "wsasendto: unreachable network" errors on every connectivity check,
		// so it only lengthens ICE checking and bloats the offer.
		// Defence in depth — agentConfig.IPFilter already keeps link-local
		// interfaces out of the agent, so this should never fire.
		if isLinkLocalCandidate(c.Address()) {
			return
		}

		// Dedupe srflx candidates by mapped public IP. Pointing at multiple public
		// STUN servers (Google/Cloudflare/Twilio) yields several srflx entries with
		// the SAME public IP but different ports — redundant. Keeping one per IP
		// keeps the offer small and Teams-like (a real Teams offer has one srflx),
		// avoids wasted ICE checks, and reduces the odd-looking candidate list that
		// could feed the flaky VDI DPI reset. Host and relay candidates are kept as-is.
		if c.Type() == ice.CandidateTypeServerReflexive {
			addr := c.Address()
			candidateMu.Lock()
			dup := seenSrflx[addr]
			if !dup {
				seenSrflx[addr] = true
			}
			candidateMu.Unlock()
			if dup {
				return
			}
		}

		line := "a=candidate:" + c.Marshal()
		candidateMu.Lock()
		idx := len(candidates)
		candidates = append(candidates, line)
		candidateMu.Unlock()

		// Signal only after the candidate is in the slice, so a waiter woken by
		// this always finds the srflx present in the payload it builds.
		if c.Type() == ice.CandidateTypeServerReflexive {
			srflxOnce.Do(func() { close(srflxReady) })
		}

		// Trickle late candidates — those arriving after SHADOW_READY was sent.
		s.mu.Lock()
		s.localCandidateKeys[candidateKey(c.Address(), c.Port(), c.Component())] = true
		cb := s.onLateCandidate
		readySent := s.state >= stateReady && s.readySentCount > 0
		s.mu.Unlock()
		late := readySent && idx >= s.readySentCount && cb != nil

		// One line per candidate, saying whether it shipped inline in the offer
		// or is being trickled behind it.
		disposition := "inline"
		if late {
			disposition = "trickled"
		}
		s.logf("[raw][%s] candidate (%s): %s", s.bridgeID, disposition, line)

		if late {
			cb(line)
		}
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

	// ── Step 8: Wait (bounded) for the first server-reflexive candidate ───────
	// bcr_client's host candidates are its local LAN and are UNREACHABLE from the
	// SFU. Its only routable candidate is the srflx (its public IP), which the SFU
	// pairs with its own relay (proven working: local=<srflx> remote=<SFU relay>).
	// So the offer MUST carry the srflx — but it need not carry anything else.
	//
	// Waiting for gathering to *complete* means waiting on every configured STUN
	// server, including any the network blackholes. In the VDI that cost a fixed
	// ~4s on every call (gathering never completed because stun.l.google.com was
	// unreachable) even though the srflx was in hand after 19ms. That stall sits
	// directly in the Teams offer path and is a prime suspect for the cpconv
	// resets, so we leave as soon as the srflx arrives and let the rest trickle
	// via onLateCandidate.
	//
	// BCR_GATHER_MODE=complete restores the old wait-for-completion behaviour.
	waitBudget := gatherWaitBudget()
	gatherStart := time.Now()
	gatherCtx, gatherCancel := context.WithTimeout(ctx, waitBudget)

	if gatherWaitForComplete() {
		select {
		case <-gatherDone:
			s.logf("[raw][%s] ICE gathering complete after %v", s.bridgeID, time.Since(gatherStart).Round(time.Millisecond))
		case <-gatherCtx.Done():
			s.logf("[raw][%s] ICE gather wait timed out after %v — continuing with %d candidate(s)",
				s.bridgeID, time.Since(gatherStart).Round(time.Millisecond), countCandidates(&candidateMu, &candidates))
		}
	} else {
		select {
		case <-srflxReady:
			s.logf("[raw][%s] srflx gathered after %v — sending offer now, later candidates will trickle",
				s.bridgeID, time.Since(gatherStart).Round(time.Millisecond))
		case <-gatherDone:
			s.logf("[raw][%s] ICE gathering complete after %v with no srflx — continuing with %d candidate(s)",
				s.bridgeID, time.Since(gatherStart).Round(time.Millisecond), countCandidates(&candidateMu, &candidates))
		case <-gatherCtx.Done():
			s.logf("[raw][%s] gather budget %v elapsed with no srflx — continuing with %d candidate(s); the SFU may have no reachable path",
				s.bridgeID, waitBudget, countCandidates(&candidateMu, &candidates))
		}
	}
	gatherCancel()

	candidateMu.Lock()
	candsCopy := append([]string(nil), candidates...)
	candidateMu.Unlock()

	// Did gathering already finish? If so the JS can signal end-of-candidates
	// straight away; if not, it must wait for our RTC_SHADOW_ICE_CANDIDATE with
	// endOfCandidates set, which watchGatheringComplete guarantees will arrive.
	gatheringComplete := false
	select {
	case <-gatherDone:
		gatheringComplete = true
	default:
	}

	s.mu.Lock()
	s.state = stateReady
	s.readySentCount = len(candsCopy) // candidates after this index are "late"
	s.mu.Unlock()

	if !gatheringComplete {
		go s.watchGatheringComplete(gatherDone)
	}

	ready := &RTCShadowReadyPayload{
		BridgeID:          s.bridgeID,
		SDPType:           sdpType,
		ICEUfrag:          ufrag,
		ICEPwd:            pwd,
		DTLSFingerprint:   "sha-256 " + fingerprint,
		LocalIP:           "0.0.0.0",
		Candidates:        candsCopy,
		GatheringComplete: gatheringComplete,
		GeneratedAt:       time.Now().UnixMilli(),
		ExpiresAt:         time.Now().Add(60 * time.Second).UnixMilli(),
	}

	s.mu.Lock()
	s.localCredentials = ready
	s.mu.Unlock()

	return ready, nil
}

// endOfCandidatesDeadline bounds how long we let ICE gathering stay open before
// telling the JS that no more candidates are coming. Teams' JS may withhold the
// offer until it sees end-of-candidates, so this must never be open-ended — a
// blackholed STUN server would otherwise stall the call indefinitely.
const endOfCandidatesDeadline = 5 * time.Second

// watchGatheringComplete fires onGatheringComplete when ICE gathering finishes,
// or when endOfCandidatesDeadline elapses — whichever comes first.
func (s *rawShadowSession) watchGatheringComplete(gatherDone <-chan struct{}) {
	timer := time.NewTimer(endOfCandidatesDeadline)
	defer timer.Stop()

	select {
	case <-gatherDone:
		s.logf("[raw][%s] ICE gathering complete — signalling end-of-candidates", s.bridgeID)
	case <-timer.C:
		s.logf("[raw][%s] ICE gathering still open after %v — signalling end-of-candidates anyway so Teams does not wait on it",
			s.bridgeID, endOfCandidatesDeadline)
	}

	s.signalGatheringComplete()
}

// signalGatheringComplete invokes onGatheringComplete at most once per session.
func (s *rawShadowSession) signalGatheringComplete() {
	s.mu.Lock()
	cb := s.onGatheringComplete
	s.mu.Unlock()

	if cb == nil {
		return
	}
	s.gatheringDoneOnce.Do(cb)
}

// GetTransportCredentials returns a copy of the active ICE/DTLS credentials,
// updated with the specified sdpType and fresh expiration timestamps.
func (s *rawShadowSession) GetTransportCredentials(sdpType string) (*RTCShadowReadyPayload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.localCredentials == nil {
		return nil, fmt.Errorf("no transport credentials available")
	}

	ready := *s.localCredentials
	ready.SDPType = sdpType
	ready.GeneratedAt = time.Now().UnixMilli()
	ready.ExpiresAt = time.Now().Add(60 * time.Second).UnixMilli()

	return &ready, nil
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
func (s *rawShadowSession) Connect(ctx context.Context, remoteSDP string, gen uint32) error {
	s.mu.Lock()
	// Only allow entry from stateReady. stateConnecting means we are already
	// connecting (glare reject); anything else is a logic error.
	if s.state != stateReady {
		s.mu.Unlock()
		return fmt.Errorf("ErrDuplicateConnect: state=%v", s.state)
	}
	// Stale-generation guard: the token we were issued must still be current.
	if gen != s.generation {
		s.mu.Unlock()
		return fmt.Errorf("ErrStaleGeneration: got=%d current=%d", gen, s.generation)
	}
	s.state = stateConnecting
	// Record the SDP this agent will authenticate against, so a later
	// renegotiation can be compared to it rather than assumed equivalent.
	s.connectedRemoteSDP = remoteSDP
	s.mu.Unlock()

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
	
	isRemoteIceLite := strings.Contains(remoteSDP, "a=ice-lite")
	isControlling := s.isOfferer || isRemoteIceLite

	// Snapshot what the agent is starting from. A checklist with no remote
	// candidates, or with only unroutable local ones, explains a stalled ICE on
	// its own and costs one line to rule out.
	localCands, _ := s.iceAgent.GetLocalCandidates()
	remoteCands, _ := s.iceAgent.GetRemoteCandidates()
	s.logf("[raw][%s] ICE start: local=%d remote=%d role=%s ice-lite=%v",
		s.bridgeID, len(localCands), len(remoteCands),
		map[bool]string{true: "controlling", false: "controlled"}[isControlling], isRemoteIceLite)

	// Sample the checklist while the handshake blocks.
	//
	// Until now a failed ICE produced exactly one line — "connecting canceled by
	// caller" after the full timeout — which cannot distinguish "our checks are
	// being answered but the peer never nominates" from "our checks are going
	// nowhere". ResponsesReceived is what separates them: >0 means the SFU is
	// talking to us and the problem is nomination or reachability; 0 across every
	// pair means it is not acting on our credentials at all.
	//
	// This is deliberately NOT env-gated. A healthy connect finishes well inside
	// the first tick and logs nothing; only a stalling one is ever heard from.
	stopSampler := s.startICEChecklistSampler()

	if isControlling {
		s.logf("[raw][%s] ICE Dial (controlling — Go was offerer or remote is ice-lite)", s.bridgeID)
		iceConn, err = s.iceAgent.Dial(ctx, remoteUfrag, remotePwd)
	} else {
		s.logf("[raw][%s] ICE Accept (controlled — Go was answerer)", s.bridgeID)
		iceConn, err = s.iceAgent.Accept(ctx, remoteUfrag, remotePwd)
	}
	stopSampler()
	if err != nil {
		return fmt.Errorf("ICE handshake: %w", err)
	}
	s.iceConn = iceConn
	s.logf("[raw][%s] ICE connected local=%s remote=%s", s.bridgeID, iceConn.LocalAddr(), iceConn.RemoteAddr())

	// ── Step 4: Inline SRTP Splitter ─────────────────────────────────────────
	splitter := newIceConnSplitter(iceConn, s.logf, s.bridgeID)
	s.splitter = splitter

	// Arm the inbound DTLS trace with the peer fingerprint from the SFU's SDP so
	// every inbound Certificate can be attributed to our peer or flagged foreign.
	splitter.SetExpectedPeerFingerprint(remoteFP)

	// NOTE: do not try to "drain" the ICE connection before starting DTLS.
	//
	// An earlier version did exactly that, to discard a stale DTLS flight that
	// could be sitting in the buffer. It was wrong twice over:
	//
	//  1. It bounded the drain with conn.SetReadDeadline, which is a no-op stub
	//     in pion/ice (transport.go: `func (c *Conn) SetReadDeadline(time.Time)
	//     error { return nil }`). Read() therefore blocked until the next packet
	//     instead of timing out, and since the SFU sends RTCP every 1-3s the loop
	//     never exited — the DTLS handshake was never reached at all.
	//  2. The premise was false. The SFU starts sending RTCP as soon as ICE
	//     completes, so the buffer legitimately contains media-plane traffic that
	//     belongs to iceConnSplitter's srtpCh, not to a discard loop.
	//
	// Stale flights are handled where they should be: iceConnSplitter drops any
	// handshake record that arrives before our ClientHello, or whose certificate
	// does not match the SFU's a=fingerprint. That needs no deadline and touches
	// nothing on the media plane.

	// ── Step 5: DTLS handshake ────────────────────────────────────────────────
	// Role comes from both a=setup: values — see resolveDTLSRole. Using the
	// remote one alone works for outgoing calls and deadlocks incoming ones.
	s.mu.Lock()
	localSetup := s.localDTLSSetup
	s.mu.Unlock()
	role := resolveDTLSRole(localSetup, remoteSDP)

	// One line for the whole role/identity decision. a=setup: and a=fingerprint:
	// repeat once per m-section (4–8 lines each on a Teams SDP) and are always
	// identical under BUNDLE, so only the parsed result is worth reporting; the
	// full SDP is already in the [SDP-INSPECT] dump if a line-level check is
	// ever needed.
	s.logf("[raw][%s] DTLS role=%s (local a=setup:%s, remote a=setup:%s, offerer=%v) peer_fp=%s",
		s.bridgeID, role, orNone(localSetup), orNone(firstSDPAttr(remoteSDP, "a=setup:")),
		s.isOfferer, safePrefix(normalizeFingerprint(remoteFP), 17))

	// Key material capture strategy:
	//
	// VerifyConnection is called DURING the handshake (before master secret
	// is finalized), so ExportKeyingMaterial cannot be called from within it.
	// We only use it to capture the peer certificate for fingerprint verification.
	//
	// After HandshakeContext() returns successfully, we call ConnectionState()
	// to export keying material — at that point the handshake is fully complete.
	var (
		capturedPeerCert []byte
		vcCalled         bool
	)
	const (
		exporterLabel = "EXTRACTOR-dtls_srtp"
	)

	// pion/dtls's internal logger. Trace prints every flight transition and
	// every record, which is what we needed while chasing the bad_certificate
	// alert but is pure duplication of our own [DTLS-TRACE] lines in a healthy
	// run. Opt in with BCR_DTLS_DEBUG=1 when a handshake actually fails —
	// Trace is the only level that gives the reason a certificate was rejected.
	pionLogFactory := logging.NewDefaultLoggerFactory()
	pionLogFactory.Writer = log.Writer()
	if envEnabled("BCR_DTLS_DEBUG") {
		pionLogFactory.DefaultLogLevel = logging.LogLevelTrace
	} else {
		pionLogFactory.DefaultLogLevel = logging.LogLevelWarn
	}

	dtlsCfg := &dtls.Config{
		Certificates:           []tls.Certificate{s.cert},
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{
			dtls.SRTP_AEAD_AES_128_GCM,
			dtls.SRTP_AEAD_AES_256_GCM,
			dtls.SRTP_AES128_CM_HMAC_SHA1_80,
			dtls.SRTP_AES128_CM_HMAC_SHA1_32,
		},
		InsecureSkipVerify:     true, // skip chain verify; fingerprint verified below
		LoggerFactory:          pionLogFactory,
		VerifyConnection: func(st *dtls.State) error {
			vcCalled = true
			if len(st.PeerCertificates) > 0 {
				capturedPeerCert = make([]byte, len(st.PeerCertificates[0]))
				copy(capturedPeerCert, st.PeerCertificates[0])
			}
			return nil
		},
	}

	rAddr := iceConn.RemoteAddr()

	var dtlsConn *dtls.Conn
	switch role {
	case "server":
		dtlsConn, err = dtls.Server(splitter, rAddr, dtlsCfg)
	default: // "client"
		dtlsConn, err = dtls.Client(splitter, rAddr, dtlsCfg)
	}
	if err != nil {
		return fmt.Errorf("DTLS conn create (%s): %w", role, err)
	}

	// pion/dtls v3 uses lazy handshake: Client()/Server() only create the
	// connection struct — the actual DTLS flight exchange does NOT start until
	// the first Read(), Write(), or explicit HandshakeContext() call.
	dtlsStart := time.Now()
	if err = dtlsConn.HandshakeContext(ctx); err != nil {
		s.logf("[raw][%s] DTLS handshake FAILED after %v (role=%s peer=%v vcCalled=%v): %v — rerun with BCR_DTLS_DEBUG=1 for flight-level detail",
			s.bridgeID, time.Since(dtlsStart).Round(time.Millisecond), role, rAddr, vcCalled, err)
		_ = dtlsConn.Close()
		return fmt.Errorf("DTLS handshake (%s): %w", role, err)
	}
	s.dtlsConn = dtlsConn

	// Stop filtering: from here records are encrypted (epoch > 0) and cannot be
	// inspected, and pion must receive everything.
	splitter.SetHandshakeComplete()

	// Disable deadlines on the splitter/connection post-handshake to prevent
	// subsequent read timeouts from freezing the media flow.
	splitter.SetIgnoreDeadlines(true)

	// ── Post-handshake: extract SRTP profile and keying material ─────────
	srtpProfile, profileOK := dtlsConn.SelectedSRTPProtectionProfile()
	s.logf("[raw][%s] DTLS handshake OK in %v: role=%s profile=%v peerCert=%v",
		s.bridgeID, time.Since(dtlsStart).Round(time.Millisecond), role,
		srtpProfile, len(capturedPeerCert) > 0)

	if !profileOK || srtpProfile == 0 {
		_ = dtlsConn.Close()
		return fmt.Errorf("DTLS handshake incomplete: no SRTP profile negotiated (profile=%v ok=%v)", srtpProfile, profileOK)
	}

	var keyLen, saltLen int
	switch srtpProfile {
	case dtls.SRTP_AES128_CM_HMAC_SHA1_80, dtls.SRTP_AES128_CM_HMAC_SHA1_32:
		keyLen = 16
		saltLen = 14
	case dtls.SRTP_AEAD_AES_128_GCM:
		keyLen = 16
		saltLen = 12
	case dtls.SRTP_AEAD_AES_256_GCM:
		keyLen = 32
		saltLen = 12
	default:
		_ = dtlsConn.Close()
		return fmt.Errorf("unsupported negotiated SRTP profile: %v", srtpProfile)
	}
	materialLen := (keyLen + saltLen) * 2

	// Export keying material from the completed connection state.
	st, stOK := dtlsConn.ConnectionState()
	if !stOK {
		_ = dtlsConn.Close()
		return fmt.Errorf("DTLS: ConnectionState() returned ok=false after successful HandshakeContext")
	}
	capturedKeyMaterial, err := st.ExportKeyingMaterial(exporterLabel, nil, materialLen)
	if err != nil {
		_ = dtlsConn.Close()
		return fmt.Errorf("DTLS ExportKeyingMaterial: %w", err)
	}
	if len(capturedKeyMaterial) != materialLen {
		_ = dtlsConn.Close()
		return fmt.Errorf("DTLS: key material length %d != expected %d", len(capturedKeyMaterial), materialLen)
	}
	// Grab peer cert from ConnectionState if VerifyConnection didn't capture it.
	if capturedPeerCert == nil && len(st.PeerCertificates) > 0 {
		capturedPeerCert = st.PeerCertificates[0]
	}

	// ── Step 7: Fingerprint verification ──────────────────────────────────────
	if err := s.verifyPeerCertByDER(capturedPeerCert, remoteFP); err != nil {
		_ = dtlsConn.Close()
		return fmt.Errorf("fingerprint verify: %w", err)
	}

	// ── Step 8: SRTP key derivation ────────────────────────────────────────────
	goIsClient := role == "client"
	srtpCtx, srtcpCtx, err := deriveSRTPContextFromMaterial(capturedKeyMaterial, goIsClient, srtp.ProtectionProfile(srtpProfile), keyLen, saltLen, s.logf, s.bridgeID)
	if err != nil {
		return fmt.Errorf("derive SRTP context: %w", err)
	}
	s.srtpCtx = srtpCtx
	s.srtcpCtx = srtcpCtx
	s.logf("[raw][%s] peer fingerprint verified, SRTP up (%v) — decrypting", s.bridgeID, srtp.ProtectionProfile(srtpProfile))

	// ── Step 9: SRTP read loop ─────────────────────────────────────────────────
	// IMPORTANT: Do NOT derive loopCtx from the Connect() ctx parameter.
	// That ctx carries a 30s timeout from triggerConnect(). When triggerConnect's
	// goroutine returns after Connect() succeeds, its defer cancel() fires and
	// kills any child context — which would immediately stop the SRTP read loop.
	// The loop must live for the lifetime of the session, not the connect attempt.
	loopCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.state = stateConnected
	s.mu.Unlock()
	go s.srtpReadLoop(loopCtx, splitter.SRTPChan())
	go s.rtcpHeartbeatLoop(loopCtx)
	go s.twccFeedbackLoop(loopCtx)
	go s.startupKeyframeLoop(loopCtx)
	go s.mediaSummaryLoop(loopCtx)

	// ── DTLS drain loop ─────────────────────────────────────────────────────
	// CRITICAL: pion/dtls v3's internal packet-reader goroutine only runs
	// while there is an active Read() call on the dtls.Conn. After the
	// handshake, nobody reads application data from the DTLS connection
	// (we use raw SRTP, not DTLS-data). Without an active Read(), the
	// internal reader exits after its flight-timer expires (~10-15s),
	// which stops calling splitter.ReadFrom() — the ONLY function that
	// reads from the ICE connection and feeds srtpCh. The result is
	// total media starvation despite a healthy ICE/SRTP session.
	//
	// This goroutine continuously calls dtlsConn.Read() to keep the
	// internal reader alive for the lifetime of the session. Any DTLS
	// application data received is discarded (none is expected in
	// SRTP-only mode).
	go func() {
		buf := make([]byte, 8192)
		for {
			_, err := dtlsConn.Read(buf)
			if err != nil {
				s.logf("[raw][%s] DTLS drain loop exited: %v", s.bridgeID, err)
				return
			}
		}
	}()

	return nil
}


// AddRemoteIceCandidate trickles a single candidate from the SFU into the agent.
// Called by handleShadowICECandidate when RTC_SHADOW_ICE_CANDIDATE arrives.
func (s *rawShadowSession) AddRemoteIceCandidate(raw string) {
	// Strip any SDP attribute prefix so we get the bare candidate string.
	raw = strings.TrimPrefix(raw, "a=candidate:")
	raw = strings.TrimPrefix(raw, "candidate:")
	raw = strings.TrimSpace(strings.TrimRight(raw, "\r"))

	// An empty candidate is the peer's end-of-candidates marker arriving via
	// addIceCandidate, not a malformed one. Nothing to add.
	if raw == "" {
		return
	}

	c, err := ice.UnmarshalCandidate(raw)
	if err != nil {
		s.logf("[raw][%s] UnmarshalCandidate failed: %v (raw=%q)", s.bridgeID, err, raw)
		return
	}

	// Refuse our own candidate handed back to us. Adding it would make pion pair
	// a local candidate against itself — hairpin pairs that can never carry SFU
	// media but are retried for the whole checking phase. See localCandidateKeys.
	s.mu.Lock()
	isOwn := s.localCandidateKeys[candidateKey(c.Address(), c.Port(), c.Component())]
	s.mu.Unlock()
	if isOwn {
		s.logf("[raw][%s] IGNORED echoed local candidate: %s", s.bridgeID, raw[:min(len(raw), 60)])
		return
	}

	if err := s.iceAgent.AddRemoteCandidate(c); err != nil {
		s.logf("[raw][%s] AddRemoteCandidate failed: %v", s.bridgeID, err)
		return
	}
	s.logf("[raw][%s] remote candidate: %s", s.bridgeID, raw[:min(len(raw), 60)])
}

func (s *rawShadowSession) UpdateIceServers(servers []IceServer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.iceServers = servers
	s.logf("[raw][%s] received dynamic ICE servers update: %d server(s)", s.bridgeID, len(servers))
}

// Close tears down the session. Safe to call multiple times.
func (s *rawShadowSession) Close() {
	s.mu.Lock()
	if s.state == stateClosed {
		s.mu.Unlock()
		return
	}
	s.state = stateClosed
	s.generation++ // Poison any in-flight goroutines
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

// trackVideoSSRC records a video SSRC for PLI generation.
func (s *rawShadowSession) trackVideoSSRC(ssrc uint32) {
	s.videoSSRCsMu.Lock()
	defer s.videoSSRCsMu.Unlock()
	if s.videoSSRCs == nil {
		s.videoSSRCs = make(map[uint32]bool)
	}
	if !s.videoSSRCs[ssrc] {
		s.videoSSRCs[ssrc] = true
		s.logf("[raw][%s] tracking video SSRC %d for PLI", s.bridgeID, ssrc)
	}
}

// getVideoSSRCs returns a snapshot of all known video SSRCs.
func (s *rawShadowSession) getVideoSSRCs() []uint32 {
	s.videoSSRCsMu.Lock()
	defer s.videoSSRCsMu.Unlock()
	result := make([]uint32, 0, len(s.videoSSRCs))
	for ssrc := range s.videoSSRCs {
		result = append(result, ssrc)
	}
	return result
}

// rtcpHeartbeatLoop sends a strict compound RTCP packet every 2 seconds:
//
//  1. ReceiverReport  (RR)  — per-SSRC reception statistics with LSR/DLSR
//  2. SourceDescription     (SDES / CNAME)
//  3. PictureLossIndication (PLI) — one per known video SSRC
//  4. ReceiverEstimatedMaximumBitrate (REMB) — only when TWCC is not active
//
// PLI is sent unconditionally on every tick. Because this proxy does not
// implement a full NACK engine, the Teams SFU's packet-loss estimator has no
// other way to learn that the receiver is alive. Without periodic PLI the SFU
// eventually halts the video track (~11 s). A PLI every 2 s is the correct
// keep-alive signal — it guarantees a fresh keyframe and pacifies the SFU's
// video watchdog without tripping its congestion limiter (which is triggered
// by continuous rapid-fire PLI, not by 2-second cadence).
//
// TWCC feedback is sent on its own 100ms loop (twccFeedbackLoop) to avoid
// MTU overflow and BWE starvation.
func (s *rawShadowSession) rtcpHeartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	heartbeatCount := uint64(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			heartbeatCount++

			s.mu.Lock()
			senderSSRC := s.localSenderSSRC
			cname := s.localCNAME
			s.mu.Unlock()
			if senderSSRC == 0 {
				senderSSRC = uint32(1)
			}
			if cname == "" {
				cname = "bcr-shadow"
			}

			// ── Build Receiver Reports with real statistics ──────────────
			var reports []rtcp.ReceptionReport
			s.incomingSeqsMu.Lock()
			for ssrc, tracker := range s.incomingSeqs {
				fractionLost, totalLost := tracker.computeRRStats()
				rr := rtcp.ReceptionReport{
					SSRC:               ssrc,
					FractionLost:       fractionLost,
					TotalLost:          totalLost,
					LastSequenceNumber: tracker.extended(),
					Jitter:             uint32(tracker.jitter),
				}
				// Reflect LSR/DLSR from the last Sender Report we received.
				s.lastSRBySSRCMu.Lock()
				if sr, ok := s.lastSRBySSRC[ssrc]; ok {
					// LSR = middle 32 bits of the 64-bit NTP timestamp.
					rr.LastSenderReport = uint32(sr.ntpTime >> 16)
					// DLSR = delay since SR received, in 1/65536 second units.
					delayUs := time.Since(sr.receivedAt).Microseconds()
					rr.Delay = uint32(delayUs * 65536 / 1_000_000)
				}
				s.lastSRBySSRCMu.Unlock()
				reports = append(reports, rr)
			}
			s.incomingSeqsMu.Unlock()

			// Snapshot video SSRCs once — used for both PLI and REMB.
			videoSSRCs := s.getVideoSSRCs()

			// ── Compound RTCP packet (RFC 3550 §6.1) ──────────────────────
			// Order: RR → SDES → PLI(s) → REMB
			pkts := []rtcp.Packet{
				&rtcp.ReceiverReport{
					SSRC:    senderSSRC,
					Reports: reports,
				},
				rtcp.NewCNAMESourceDescription(senderSSRC, cname),
			}

			// PLI — one per known video SSRC. Acts as both a keyframe
			// request and a SFU video-watchdog keep-alive signal.
			for _, ssrc := range videoSSRCs {
				pkts = append(pkts, &rtcp.PictureLossIndication{
					SenderSSRC: senderSSRC,
					MediaSSRC:  ssrc,
				})
			}

			// REMB — only when TWCC is not active (SFU rejected transport-cc).
			if s.transportCCExtID == 0 {
				pkts = append(pkts, &rtcp.ReceiverEstimatedMaximumBitrate{
					SenderSSRC: senderSSRC,
					Bitrate:    5_000_000,
					SSRCs:      videoSSRCs,
				})
			}




			// Increment RTCP counters for [MEDIA-SUMMARY].
			s.rtcpMu.Lock()
			s.rtcpRRCount++
			for _, p := range pkts {
				switch p.(type) {
				case *rtcp.PictureLossIndication:
					s.rtcpPLICount++
				case *rtcp.ReceiverEstimatedMaximumBitrate:
					s.rtcpREMBCount++
				}
			}
			s.rtcpMu.Unlock()

			s.sendCompoundRTCP(pkts, "heartbeat", heartbeatCount, senderSSRC, len(reports), len(videoSSRCs))
		}
	}
}


// requestKeyframe sends a single RTCP Picture Loss Indication (PLI) for the
// given video SSRC. This is a one-off, on-demand call — do NOT call it in a
// loop or on a timer. Intended for use when a massive sequence-number gap is
// detected, indicating the decoder needs a fresh keyframe to recover.
func (s *rawShadowSession) requestKeyframe(videoSSRC uint32) {
	s.mu.Lock()
	senderSSRC := s.localSenderSSRC
	s.mu.Unlock()
	if senderSSRC == 0 {
		senderSSRC = 1
	}

	pli := &rtcp.PictureLossIndication{
		SenderSSRC: senderSSRC,
		MediaSSRC:  videoSSRC,
	}
	raw, err := rtcp.Marshal([]rtcp.Packet{pli})
	if err != nil {
		s.logf("[raw][%s] requestKeyframe: marshal PLI failed: %v", s.bridgeID, err)
		return
	}
	s.srtcpWriteMu.Lock()
	encrypted, err := s.srtcpCtx.EncryptRTCP(nil, raw, nil)
	if err != nil {
		s.srtcpWriteMu.Unlock()
		s.logf("[raw][%s] requestKeyframe: encrypt PLI failed: %v", s.bridgeID, err)
		return
	}
	_, writeErr := s.iceConn.Write(encrypted)
	s.srtcpWriteMu.Unlock()
	if writeErr != nil {
		s.logf("[raw][%s] requestKeyframe: write PLI failed: %v", s.bridgeID, writeErr)
		return
	}

	// On-demand PLIs are frequent — WebKit asks for one whenever its decoder
	// stalls, which during a bad patch is several a second. Report the first
	// per SSRC only; the running total is in [MEDIA-SUMMARY] pli=.
	s.rtcpMu.Lock()
	s.rtcpPLICount++
	if s.pliLoggedSSRCs == nil {
		s.pliLoggedSSRCs = make(map[uint32]bool)
	}
	first := !s.pliLoggedSSRCs[videoSSRC]
	s.pliLoggedSSRCs[videoSSRC] = true
	s.rtcpMu.Unlock()

	if first {
		s.logf("[raw][%s] on-demand PLI for video SSRC %d (further PLIs counted in [MEDIA-SUMMARY])", s.bridgeID, videoSSRC)
	}
}

// startupKeyframeLoop fires a PLI for every known video SSRC shortly after
// the SRTP session comes up, satisfying the Teams SFU's first-keyframe-ack
// watchdog. Without this, the SFU sends a brief burst of video, waits for a
// PLI to confirm the receiver is healthy, and then suspends the track.
//
// Strategy:
//   - Wait 500ms for the first RTP burst and the renegotiation offer (which
//     delivers SSRC=14601 etc.) to arrive and populate videoSSRCs.
//   - Send one PLI per known video SSRC.
//   - Retry two more times at 3-second intervals to cover any SSRCs that are
//     registered after our first PLI round (e.g. late renegotiation).
//   - Exit after 3 rounds. On-demand PLIs after that use requestKeyframe().
func (s *rawShadowSession) startupKeyframeLoop(ctx context.Context) {
	const (
		initialDelay = 500 * time.Millisecond
		retryDelay   = 3 * time.Second
		maxRounds    = 3
	)

	// Track which SSRCs we have already sent a startup PLI for, so we don't
	// spam the same SSRC across rounds — only send for newly-seen ones.
	sentSSRCs := make(map[uint32]bool)

	sendForNewSSRCs := func(round int) {
		ssrcs := s.getVideoSSRCs()
		for _, ssrc := range ssrcs {
			if sentSSRCs[ssrc] {
				continue
			}
			sentSSRCs[ssrc] = true
			s.requestKeyframe(ssrc)
		}
	}

	// Round 1 — after the initial warm-up delay.
	select {
	case <-ctx.Done():
		return
	case <-time.After(initialDelay):
	}
	sendForNewSSRCs(1)

	// Rounds 2 & 3 — catch SSRCs registered by renegotiation after round 1.
	for round := 2; round <= maxRounds; round++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
		sendForNewSSRCs(round)
	}
}

// twccFeedbackLoop sends TWCC transport feedback every 100ms on a separate
// cadence from the RR/SDES/PLI heartbeat. This prevents MTU overflow in the
// compound packet and satisfies the SFU's bandwidth estimator.
func (s *rawShadowSession) twccFeedbackLoop(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	twccCount := uint64(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.transportCCExtID == 0 {
				continue // transport-cc not negotiated yet
			}

			s.mu.Lock()
			senderSSRC := s.localSenderSSRC
			s.mu.Unlock()
			if senderSSRC == 0 {
				continue
			}

			// Pick a media SSRC for the TWCC packet
			mediaSSRC := uint32(0)
			videoSSRCs := s.getVideoSSRCs()
			if len(videoSSRCs) > 0 {
				mediaSSRC = videoSSRCs[0]
			} else {
				// Fallback: use first known incoming SSRC
				s.incomingSeqsMu.Lock()
				for ssrc := range s.incomingSeqs {
					mediaSSRC = ssrc
					break
				}
				s.incomingSeqsMu.Unlock()
			}
			if mediaSSRC == 0 {
				continue
			}

			fb := s.buildTWCCFeedback(senderSSRC, mediaSSRC)
			if fb == nil {
				continue
			}

			twccCount++
			pkts := []rtcp.Packet{fb}
			raw, err := rtcp.Marshal(pkts)
			if err != nil {
				if twccCount <= 3 || twccCount%100 == 0 {
					s.logf("[raw][%s] [TWCC] marshal failed #%d: %v", s.bridgeID, twccCount, err)
				}
				continue
			}
			encrypted, err := s.srtcpCtx.EncryptRTCP(nil, raw, nil)
			if err != nil {
				if twccCount <= 3 || twccCount%100 == 0 {
					s.logf("[raw][%s] [TWCC] encrypt failed #%d: %v", s.bridgeID, twccCount, err)
				}
				continue
			}
			if s.iceConn != nil {
				_, _ = s.iceConn.Write(encrypted)
				if twccCount <= 5 || twccCount%200 == 0 {
					s.logf("[raw][%s] [TWCC] feedback #%d sent: seqs=%d", s.bridgeID, twccCount, fb.PacketStatusCount)
				}
			}
		}
	}
}

// sendCompoundRTCP encrypts and sends a compound RTCP packet via the ICE connection.
func (s *rawShadowSession) sendCompoundRTCP(pkts []rtcp.Packet, label string, count uint64, senderSSRC uint32, nReports, nPLIs int) {
	raw, err := rtcp.Marshal(pkts)
	if err != nil {
		s.logf("[raw][%s] [RTCP] marshal failed: %v", s.bridgeID, err)
		return
	}
	s.srtcpWriteMu.Lock()
	encrypted, err := s.srtcpCtx.EncryptRTCP(nil, raw, nil)
	if err != nil {
		s.srtcpWriteMu.Unlock()
		if count <= 3 || count%10 == 0 {
			s.logf("[raw][%s] [RTCP] encrypt failed #%d: %v", s.bridgeID, count, err)
		}
		return
	}
	if s.iceConn != nil {
		_, writeErr := s.iceConn.Write(encrypted)
		s.srtcpWriteMu.Unlock()
		if writeErr != nil {
			s.logf("[raw][%s] [RTCP] %s #%d write failed: %v", s.bridgeID, label, count, writeErr)
		}
	} else {
		s.srtcpWriteMu.Unlock()
	}
}

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
		return
	}

	// ── RFC 5761 Demux: SRTCP vs SRTP ────────────────────────────────────
	// Second byte (PT field) in range [192, 223] → RTCP.
	// This range covers SR(200), RR(201), SDES(202), BYE(203), APP(204),
	// RTPFB(205), PSFB(206), XR(207) and reserved types, without
	// overlapping with RTP dynamic PTs (96-127) or marker-set PTs (224-255).
	pt := encrypted[1]
	if pt >= 192 && pt <= 223 {
		s.processInboundSRTCP(encrypted)
		return
	}

	// ── SRTP path ────────────────────────────────────────────────────────
	var header rtp.Header
	if _, err := header.Unmarshal(encrypted); err != nil {
		return
	}

	decrypted, err := s.srtpCtx.DecryptRTP(nil, encrypted, &header)
	if err != nil {
		return
	}

	// Track sequence numbers with proper cycle counting (RFC 3550 §A.1).
	s.incomingSeqsMu.Lock()
	if s.incomingSeqs == nil {
		s.incomingSeqs = make(map[uint32]*seqTracker)
	}
	tracker := s.incomingSeqs[header.SSRC]
	if tracker == nil {
		tracker = &seqTracker{}
		// Set clock rate from codec map for jitter computation.
		s.mu.Lock()
		if ci, ok := s.ptCodecMap[header.PayloadType]; ok {
			tracker.clockRate = ci.ClockRate
		}
		s.mu.Unlock()
		s.incomingSeqs[header.SSRC] = tracker
	}
	tracker.updateSeq(header.SequenceNumber)
	// Update interarrival jitter (RFC 3550 §A.8).
	tracker.updateJitter(time.Now().UnixMicro(), header.Timestamp)
	s.incomingSeqsMu.Unlock()

	// ── Dynamic video SSRC tracking ───────────────────────────────────
	// Track video SSRCs directly from the decrypted wire so that PLI and
	// REMB in the heartbeat always use the real SSRC the SFU is sending
	// on — even if it differs from (or was missing in) the SDP.
	//
	// We look up the payload type in ptCodecMap (populated from SDP). If
	// the codec is known and is video, register the SSRC. trackVideoSSRC
	// is a no-op for SSRCs already known, so this is hot-path safe.
	s.mu.Lock()
	wireCodec, wireCodecKnown := s.ptCodecMap[header.PayloadType]
	s.mu.Unlock()
	if wireCodecKnown && strings.Contains(strings.ToUpper(wireCodec.MimeType), "VIDEO") {
		s.trackVideoSSRC(header.SSRC)
	}

	// Extract transport-wide sequence number for TWCC feedback.
	if s.transportCCExtID != 0 {
		if ext := header.GetExtension(s.transportCCExtID); len(ext) >= 2 {
			twccSeq := binary.BigEndian.Uint16(ext)
			s.twcc.add(twccSeq, time.Now().UnixMicro())
		}
	}

	// ── Unmarshal full RTP packet (used for H264 inspection + relay) ───
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(decrypted); err != nil {
		return
	}

	// ── Per-SSRC stream registration + gap detection ─────────────────
	if s.ssrcReg != nil {
		st, isNew := s.ssrcReg.getOrCreate(header.SSRC)
		st.mu.Lock()
		if isNew {
			st.pt = header.PayloadType
			st.role = roleUnknown
			s.mu.Lock()
			if ci, ok := s.ptCodecMap[header.PayloadType]; ok {
				if strings.Contains(strings.ToUpper(ci.MimeType), "VIDEO") {
					st.role = rolePrimVideo
				} else if strings.Contains(strings.ToUpper(ci.MimeType), "AUDIO") {
					st.role = roleAudio
				}
			}
			s.mu.Unlock()
			if _, ok := s.rtxMapping.isRTX(header.PayloadType); ok {
				st.role = roleRTXVideo
			}
		}
		// Gap / reorder detection.
		if st.seqInitiated {
			expected := st.highestSeq + 1
			delta := int16(header.SequenceNumber - expected)
			switch {
			case delta == 0: // normal
			case delta > 0:
				missing := s.nackSt.recordGap(header.SSRC, expected, header.SequenceNumber)
				st.totalLost += int64(len(missing))
				if len(missing) > 0 {
					s.rtcpMu.Lock()
					s.rtcpNACKCount++
					s.rtcpMu.Unlock()
					st.nackSent++
					// Only NACK primary media SSRCs, never the RTX repair stream itself.
					if st.role != roleRTXVideo {
						go s.sendNACK(header.SSRC, missing)
					}
				}
			case delta < 0: // reordered or duplicate — nothing to record
			}
		}
		if !st.seqInitiated || int16(header.SequenceNumber-st.highestSeq) > 0 {
			st.highestSeq = header.SequenceNumber
			st.seqInitiated = true
		}
		st.pktCount++
		st.byteCount += int64(len(decrypted))
		st.mu.Unlock()
	}

	// ── H264 NAL inspection ──────────────────────────────────────────
	s.mu.Lock()
	ci, ciOK := s.ptCodecMap[header.PayloadType]
	s.mu.Unlock()
	if ciOK && strings.Contains(strings.ToUpper(ci.MimeType), "H264") && len(pkt.Payload) > 0 {
		if IsH264Keyframe(pkt.Payload) {
			if st, _ := s.ssrcReg.getOrCreate(header.SSRC); st != nil {
				st.mu.Lock()
				st.keyframeCount++
				st.mu.Unlock()
			}
		}
	}

	if s.onRTPPacket == nil {
		return
	}

	// ── RTX decapsulation (RFC 4588) ─────────────────────────────────────────
	// RTX packets must be intercepted BEFORE the relay sees them. The relay
	// strict-drops any PT it doesn't recognise (PT=99). We decapsulate here
	// and feed the reconstructed packet back as the original media PT/SSRC.
	if mediaPT, isRTX := s.rtxMapping.isRTX(header.PayloadType); isRTX {
		s.decapsulateRTX(pkt, header.SSRC, mediaPT)
		return
	}

	s.onRTPPacket(pkt)
}

// processInboundSRTCP decrypts an incoming SRTCP packet and extracts Sender
// Reports so we can reflect LSR/DLSR in our Receiver Reports.
//
// NOTE: SRTCP from the SFU is decrypted with srtpCtx (derived from the
// server→client key material). srtcpCtx holds the client→server keys used
// for ENCRYPTING our outbound RTCP — using it to decrypt inbound SRTCP
// causes silent auth failures.
func (s *rawShadowSession) processInboundSRTCP(encrypted []byte) {
	// CRITICAL: Use srtpCtx (inbound / server→client key material) to decrypt
	// incoming SRTCP, NOT srtcpCtx which holds outbound / client→server keys.
	// srtcpCtx is for encrypting OUR RTCP; srtpCtx shares the server's keys
	// and can decrypt both SRTP and SRTCP from the SFU.
	decrypted, err := s.srtpCtx.DecryptRTCP(nil, encrypted, nil)
	if err != nil {
		s.logf("[RTCP-RX] SRTCP decrypt failed: %v (len=%d pt=%d)", err, len(encrypted), encrypted[1])
		return // auth failure or corrupt
	}

	pkts, err := rtcp.Unmarshal(decrypted)
	if err != nil {
		return
	}

	now := time.Now()
	for _, pkt := range pkts {
		switch v := pkt.(type) {
		case *rtcp.SenderReport:
			s.lastSRBySSRCMu.Lock()
			s.lastSRBySSRC[v.SSRC] = srRecord{
				ntpTime:    v.NTPTime,
				receivedAt: now,
			}
			s.lastSRBySSRCMu.Unlock()
		default:
			// RR, NACK, PLI, FIR, REMB — handled silently.
			// Health visible via [MEDIA-SUMMARY] and [RTCP-TX] logs.
			_ = v
		}
	}
}

// buildTWCCFeedback drains the TWCC tracker and builds a TransportLayerCC
// feedback packet. Returns nil if there are no pending entries.
//
// Uses StatusVectorChunk with 2-bit symbols for packet status encoding.
// This is simpler and more robust than manual RunLengthChunk building,
// and each chunk carries exactly 7 packet statuses (2-bit mode, 14 bits).
func (s *rawShadowSession) buildTWCCFeedback(senderSSRC, mediaSSRC uint32) *rtcp.TransportLayerCC {
	pending, fbCount := s.twcc.drain()
	if len(pending) == 0 {
		return nil
	}

	// Sort with modulo-arithmetic to handle 65535→0 wrap-around safely.
	sort.Slice(pending, func(i, j int) bool {
		return int16(pending[i].seq-pending[j].seq) < 0
	})

	baseSeq := pending[0].seq
	maxSeq := pending[len(pending)-1].seq
	packetCount := int(uint16(maxSeq-baseSeq)) + 1

	// Safety cap: if the sequence gap is absurdly large (e.g. from a
	// stale/reordered packet creating a 60,000-entry span), cap it to
	// prevent allocating a massive status array. In practice, 100ms of
	// media at 1000 pps produces ~100 packets.
	const maxTWCCSpan = 2000
	if packetCount > maxTWCCSpan {
		s.logf("[raw][%s] [TWCC] degenerate span base=%d max=%d count=%d — capping to %d",
			s.bridgeID, baseSeq, maxSeq, packetCount, maxTWCCSpan)
		packetCount = maxTWCCSpan
	}

	// Reference time: first packet's arrival truncated to 64ms units (24-bit).
	refTimeUs := pending[0].arrivalUs
	refTime64ms := refTimeUs / 64000

	// Build lookup: seq → arrivalUs
	received := make(map[uint16]int64, len(pending))
	for _, e := range pending {
		received[e.seq] = e.arrivalUs
	}

	// Classify each packet and compute receive deltas.
	// Pion's RecvDelta.Delta is in microseconds; Pion divides by 250 internally
	// during Marshal() (see rtcp.TypeTCCDeltaScaleFactor = 250).
	symbolList := make([]uint16, 0, packetCount)
	var recvDeltas []*rtcp.RecvDelta

	// prevArrival starts at the reference time boundary (in µs).
	prevArrivalUs := refTime64ms * 64000

	for i := 0; i < packetCount; i++ {
		seq := baseSeq + uint16(i)
		arrival, ok := received[seq]
		if !ok {
			symbolList = append(symbolList, rtcp.TypeTCCPacketNotReceived)
			continue
		}

		deltaUs := arrival - prevArrivalUs
		// Small delta: [0, 63750]µs (fits uint8 after ÷250)
		// Large delta: [-8192000, 8191750]µs (fits int16 after ÷250)
		if deltaUs >= 0 && deltaUs <= 63750 {
			symbolList = append(symbolList, rtcp.TypeTCCPacketReceivedSmallDelta)
			recvDeltas = append(recvDeltas, &rtcp.RecvDelta{
				Type:  rtcp.TypeTCCPacketReceivedSmallDelta,
				Delta: deltaUs,
			})
		} else {
			symbolList = append(symbolList, rtcp.TypeTCCPacketReceivedLargeDelta)
			recvDeltas = append(recvDeltas, &rtcp.RecvDelta{
				Type:  rtcp.TypeTCCPacketReceivedLargeDelta,
				Delta: deltaUs,
			})
		}
		prevArrivalUs = arrival
	}

	// Build packet status chunks using StatusVectorChunks with 2-bit symbols.
	// Each StatusVectorChunk holds up to 7 symbols in 2-bit mode (14 bits total).
	// This is simpler and avoids potential run-length encoding edge cases.
	const symbolsPerChunk = 7
	chunks := make([]rtcp.PacketStatusChunk, 0, (len(symbolList)+symbolsPerChunk-1)/symbolsPerChunk)
	for i := 0; i < len(symbolList); i += symbolsPerChunk {
		end := i + symbolsPerChunk
		if end > len(symbolList) {
			end = len(symbolList)
		}
		chunks = append(chunks, &rtcp.StatusVectorChunk{
			SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
			SymbolList: symbolList[i:end],
		})
	}

	return &rtcp.TransportLayerCC{
		SenderSSRC:         senderSSRC,
		MediaSSRC:          mediaSSRC,
		BaseSequenceNumber: baseSeq,
		PacketStatusCount:  uint16(packetCount),
		ReferenceTime:      uint32(refTime64ms) & 0xFFFFFF,
		FbPktCount:         fbCount,
		PacketChunks:       chunks,
		RecvDeltas:         recvDeltas,
	}
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

// firstSDPAttr returns the value of the first line with the given attribute
// prefix, or "" if absent. Used for one-line diagnostics of attributes that
// BUNDLE repeats identically in every m-section.
func firstSDPAttr(sdp, prefix string) string {
	for _, line := range strings.Split(sdp, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}
	return ""
}

// resolveDTLSRole decides whether Go acts as DTLS client or server, per
// RFC 5763 §5.
//
// The LOCAL a=setup: is authoritative whenever it names a concrete role,
// because it is the value the SFU actually received and is acting on:
//
//	local a=setup:active  → we promised to send the ClientHello → Go is client
//	local a=setup:passive → we promised to wait for it          → Go is server
//
// Only when the local side is "actpass" — which means we were the offerer and
// left the choice open — does the remote's answer decide:
//
//	remote a=setup:passive → remote waits → Go is client
//	remote a=setup:active  → remote initiates → Go is server
//
// Reading only the remote SDP is what broke incoming calls. On an outgoing call
// the remote is the SFU's ANSWER, which carries a concrete "passive", so the
// old logic happened to be right. On an incoming call the remote is the SFU's
// OFFER, which carries "actpass" — and the old default mapped that to "server"
// while the browser's answer, which we forward to the SFU untouched, said
// "active". Both ends then waited for a ClientHello that neither would send,
// and the handshake simply never started.
func resolveDTLSRole(localSetup, remoteSDP string) string {
	switch strings.ToLower(strings.TrimSpace(localSetup)) {
	case "active":
		return "client"
	case "passive":
		return "server"
	}

	// Local is actpass (or unknown): we offered, so the remote answer decides.
	switch strings.ToLower(firstSDPAttr(remoteSDP, "a=setup:")) {
	case "passive":
		return "client"
	default:
		// "active", "actpass" or absent. Teams answers "passive" in practice,
		// so reaching here means the remote left it open too; defaulting to
		// server matches the offerer-waits convention.
		return "server"
	}
}

// verifyPeerCertByDER checks the DTLS peer cert (raw DER bytes captured via
// VerifyPeerCertificate callback) against the SDP a=fingerprint value.
// remoteFP may include the algorithm prefix ("sha-256 AB:CD:...") or be bare.
//
// If no cert was captured (e.g. peer didn't send one, very unusual in WebRTC),
// the check is skipped with a warning rather than failing hard.
func (s *rawShadowSession) verifyPeerCertByDER(certDER []byte, remoteFP string) error {
	if len(certDER) == 0 {
		s.logf("[raw][%s] WARNING: VerifyPeerCertificate callback captured no cert; skipping fingerprint check", s.bridgeID)
		return nil
	}

	// certDER is the raw DER bytes received from VerifyPeerCertificate callback.
	parsed, err := x509.ParseCertificate(certDER)
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

// deriveSRTPContextFromMaterial builds SRTP/SRTCP contexts from pre-exported
// key material based on the negotiated SRTP profile.
// This is the primary path: material is exported after HandshakeContext completes.
func deriveSRTPContextFromMaterial(material []byte, goIsClient bool, profile srtp.ProtectionProfile, keyLen, saltLen int, logf func(string, ...any), bridgeID string) (*srtp.Context, *srtp.Context, error) {
	expectedLen := (keyLen + saltLen) * 2
	if len(material) != expectedLen {
		return nil, nil, fmt.Errorf("unexpected material length: got %d, want %d", len(material), expectedLen)
	}

	var inboundKey, inboundSalt []byte
	var outboundKey, outboundSalt []byte
	if goIsClient {
		// RFC 5764 §4.2: client_write_key | server_write_key | client_write_salt | server_write_salt
		outboundKey = material[0:keyLen]
		inboundKey = material[keyLen : keyLen*2]
		outboundSalt = material[keyLen*2 : keyLen*2+saltLen]
		inboundSalt = material[keyLen*2+saltLen : expectedLen]
	} else {
		// Go is DTLS server (answerer)
		inboundKey = material[0:keyLen]
		outboundKey = material[keyLen : keyLen*2]
		inboundSalt = material[keyLen*2 : keyLen*2+saltLen]
		outboundSalt = material[keyLen*2+saltLen : expectedLen]
	}

	inboundCtx, err := srtp.CreateContext(
		inboundKey, inboundSalt,
		profile,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("srtp.CreateContext (inbound): %w", err)
	}

	outboundCtx, err := srtp.CreateContext(
		outboundKey, outboundSalt,
		profile,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("srtp.CreateContext (outbound): %w", err)
	}

	return inboundCtx, outboundCtx, nil
}

// iceServersToStunURIs converts engine IceServer entries to []*stun.URI for
// ice.AgentConfig.Urls. Malformed URLs are skipped with a warning.
// ownIceServers returns bcr_client's OWN STUN/TURN servers, independent of the
// ones Teams supplies. bcr_client's host candidates are its local LAN and are
// unreachable from Microsoft's SFU, so it MUST obtain a routable srflx/relay
// candidate — and it cannot depend on Teams' TURN creds, which come from
// edge.skype.com/trap/tokens and are reset intermittently by the VDI (→
// iceServersCount=0 → host-only gathering → ICE never connects).
//
// Configured via env vars so a deployment can point at a reliable TURN server
// (strongly recommended on restrictive/VDI networks where a direct srflx path
// may be blocked — prefer a TURNS server on tcp/443 to survive UDP blocks):
//
//	BCR_STUN_URLS        comma-separated STUN URLs (overrides the default set)
//	BCR_TURN_URL         a TURN/TURNS URL, e.g. turns:turn.example.com:443?transport=tcp
//	BCR_TURN_USERNAME    TURN username
//	BCR_TURN_CREDENTIAL  TURN password
func ownIceServers() []IceServer {
	var servers []IceServer

	var stunURLs []string
	if env := strings.TrimSpace(os.Getenv("BCR_STUN_URLS")); env != "" {
		for _, u := range strings.Split(env, ",") {
			if u = strings.TrimSpace(u); u != "" {
				stunURLs = append(stunURLs, u)
			}
		}
	} else {
		// Only servers that actually answer on the target networks. The list used
		// to include stun.l.google.com and global.stun.twilio.com "for diversity",
		// but in the VDI both blackholed: they never produced a candidate and
		// never completed, so gathering ran to its full timeout on every call
		// while cloudflare had already answered in ~19ms. Unreachable STUN
		// servers cost latency and buy nothing.
		//
		// Set BCR_STUN_URLS (comma-separated) to override for a given deployment.
		stunURLs = []string{
			"stun:stun.cloudflare.com:3478",
		}
	}
	if len(stunURLs) > 0 {
		servers = append(servers, IceServer{URLs: stunURLs})
	}

	if turnURL := strings.TrimSpace(os.Getenv("BCR_TURN_URL")); turnURL != "" {
		servers = append(servers, IceServer{
			URLs:       []string{turnURL},
			Username:   os.Getenv("BCR_TURN_USERNAME"),
			Credential: os.Getenv("BCR_TURN_CREDENTIAL"),
		})
	}

	return servers
}

// defaultGatherWait bounds how long Init() blocks before returning the offer.
// The srflx normally arrives in tens of milliseconds; this is the ceiling for
// the case where STUN is slow or unreachable, not the expected cost.
const defaultGatherWait = 1200 * time.Millisecond

// gatherWaitBudget returns the maximum time Init() will block waiting for a
// server-reflexive candidate. Override with BCR_GATHER_WAIT_MS.
func gatherWaitBudget() time.Duration {
	if env := strings.TrimSpace(os.Getenv("BCR_GATHER_WAIT_MS")); env != "" {
		if ms, err := strconv.Atoi(env); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return defaultGatherWait
}

// envEnabled reports whether an opt-in diagnostic env var is switched on.
// Accepts "1", "true", "yes", "on" (case-insensitive).
func envEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// gatherWaitForComplete reports whether to wait for ICE gathering to finish
// entirely rather than leaving as soon as the srflx is in hand. Set
// BCR_GATHER_MODE=complete to restore the old (slow) behaviour for A/B testing.
func gatherWaitForComplete() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("BCR_GATHER_MODE")), "complete")
}

// isLinkLocalCandidate reports whether an ICE candidate address is an APIPA /
// link-local address, which cannot be reached from off the machine.
func isLinkLocalCandidate(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLinkLocalUnicast()
}

// countCandidates reads the shared candidate slice under its mutex. Used only
// for log messages emitted from the gather-wait select.
func countCandidates(mu *sync.Mutex, candidates *[]string) int {
	mu.Lock()
	defer mu.Unlock()
	return len(*candidates)
}

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

// iceSamplerInterval is how often the checklist is summarised while ICE is
// still connecting. A healthy connect completes well inside one interval, so a
// successful call logs nothing; a 30s failure logs six lines.
const iceSamplerInterval = 5 * time.Second

// startICEChecklistSampler logs a one-line summary of pion's candidate-pair
// checklist every iceSamplerInterval until the returned stop function is called.
// Stop is idempotent and blocks until the sampler goroutine has exited, so no
// line can appear after the handshake has been reported.
//
// GetCandidatePairsStats runs on the agent's own loop, so it is safe to call
// while Dial/Accept is blocked.
func (s *rawShadowSession) startICEChecklistSampler() func() {
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		ticker := time.NewTicker(iceSamplerInterval)
		defer ticker.Stop()

		start := time.Now()
		reportedResponders := false

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}

			pairs := s.iceAgent.GetCandidatePairsStats()

			var waiting, inProgress, succeeded, failed, nominated int
			var responses uint64
			for _, p := range pairs {
				switch p.State {
				case ice.CandidatePairStateWaiting:
					waiting++
				case ice.CandidatePairStateInProgress:
					inProgress++
				case ice.CandidatePairStateSucceeded:
					succeeded++
				case ice.CandidatePairStateFailed:
					failed++
				}
				if p.Nominated {
					nominated++
				}
				responses += p.ResponsesReceived
			}

			selected := "none"
			if sel, ok := s.iceAgent.GetSelectedCandidatePairStats(); ok {
				selected = sel.LocalCandidateID + "->" + sel.RemoteCandidateID
			}

			s.logf("[raw][%s] ICE checklist t=%v pairs=%d waiting=%d inprogress=%d succeeded=%d failed=%d nominated=%d respRecv=%d selected=%s",
				s.bridgeID, time.Since(start).Round(time.Second), len(pairs),
				waiting, inProgress, succeeded, failed, nominated, responses, selected)

			// Name the pairs the SFU actually answered on, once. This is the line
			// that says "the transport works, we are waiting to be nominated".
			if responses > 0 && !reportedResponders {
				reportedResponders = true
				for _, p := range pairs {
					if p.ResponsesReceived > 0 {
						s.logf("[raw][%s] ICE responding pair: %s -> %s state=%s nominated=%v responses=%d",
							s.bridgeID, p.LocalCandidateID, p.RemoteCandidateID,
							p.State, p.Nominated, p.ResponsesReceived)
					}
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-finished
	}
}

// candidateKey identifies an ICE candidate by the only fields that survive the
// round trip through the extension: transport address, port and component.
// Foundation and priority are stable too, but raddr is rewritten to 0.0.0.0 by
// scrubCandidateRaddr before dispatch, so the full candidate line is not a
// reliable key.
func candidateKey(address string, port int, component uint16) string {
	return fmt.Sprintf("%s:%d/%d", strings.ToLower(address), port, component)
}

// orNone renders an absent SDP attribute as "(none)" so a missing value is
// distinguishable from an empty one in the log.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// sendNACK transmits an RTCP TransportLayerNack for the given mediaSSRC and
// missing sequence numbers. This tells the SFU to retransmit the lost packets
// via the RTX stream. Without this, the SFU only sends empty RTX probe packets.
//
// RFC 4585 §6.2.1: the NACK carries (PID, BLP) pairs. PID is a single seq,
// BLP is a 16-bit bitmask of the next 16 seqs after PID. We build these
// pairs from the missing list.
func (s *rawShadowSession) sendNACK(mediaSSRC uint32, missing []uint16) {
	if s.srtcpCtx == nil || s.iceConn == nil {
		return
	}

	// Cap to avoid MTU overflow (each NackPair is 4 bytes; 100 pairs = 400 bytes).
	// Capping is routine under loss and fires several times a second when the
	// ingress leg is bad, so it is counted rather than logged; the NACK totals
	// appear in [MEDIA-SUMMARY].
	const maxNACKSeqs = 100
	if len(missing) > maxNACKSeqs {
		missing = missing[:maxNACKSeqs]
	}

	// Build NackPair list: group sequences into (PID, BLP) pairs.
	// Sort first to enable BLP grouping.
	sorted := make([]uint16, len(missing))
	copy(sorted, missing)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && int16(sorted[j]-sorted[j-1]) < 0; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	var pairs []rtcp.NackPair
	i := 0
	for i < len(sorted) {
		pid := sorted[i]
		blp := uint16(0)
		i++
		for i < len(sorted) {
			diff := sorted[i] - pid
			if diff >= 1 && diff <= 16 {
				blp |= 1 << (diff - 1)
				i++
			} else {
				break
			}
		}
		pairs = append(pairs, rtcp.NackPair{PacketID: pid, LostPackets: rtcp.PacketBitmap(blp)})
	}

	// Use localVideoSSRC as SenderSSRC for video NACKs so the SFU
	// recognises us as the correct participant in the video m-section.
	// Fall back to localSenderSSRC (audio) if video SSRC is not yet known.
	s.mu.Lock()
	senderSSRC := s.localVideoSSRC
	if senderSSRC == 0 {
		senderSSRC = s.localSenderSSRC
	}
	s.mu.Unlock()
	if senderSSRC == 0 {
		senderSSRC = 1
	}

	nack := &rtcp.TransportLayerNack{
		SenderSSRC: senderSSRC,
		MediaSSRC:  mediaSSRC,
		Nacks:      pairs,
	}

	raw, err := rtcp.Marshal([]rtcp.Packet{nack})
	if err != nil {
		s.logf("[NACK-TX] marshal failed SSRC=%d: %v", mediaSSRC, err)
		return
	}
	s.srtcpWriteMu.Lock()
	encrypted, err := s.srtcpCtx.EncryptRTCP(nil, raw, nil)
	if err != nil {
		s.srtcpWriteMu.Unlock()
		s.logf("[NACK-TX] encrypt failed SSRC=%d: %v", mediaSSRC, err)
		return
	}
	_, writeErr := s.iceConn.Write(encrypted)
	s.srtcpWriteMu.Unlock()
	if writeErr != nil {
		s.logf("[NACK-TX] write failed SSRC=%d: %v", mediaSSRC, writeErr)
		return
	}

}

// decapsulateRTX implements RFC 4588 RTX packet processing.
//
// RTX payload layout:
//
//	[0:2]  Original Sequence Number (OSN) — 2 bytes, big-endian
//	[2:]   Original RTP payload
//
// We reconstruct the original packet (PT=mediaPT, SSRC=mediaSSRC, seq=OSN)
// and dispatch it to onRTPPacket as if it arrived normally.
func (s *rawShadowSession) decapsulateRTX(rtxPkt *rtp.Packet, rtxSSRC uint32, mediaPT uint8) {
	if len(rtxPkt.Payload) < 2 {
		// 0-byte RTX payloads are padding probes (bandwidth estimation). Normal and high-frequency.
		return
	}

	originalSeq := binary.BigEndian.Uint16(rtxPkt.Payload[0:2])

	// Resolve the media SSRC from the FID group map.
	s.rtxSSRCMu.Lock()
	mediaSSRC, hasMap := s.rtxSSRCtoMedia[rtxSSRC]
	s.rtxSSRCMu.Unlock()

	if !hasMap {
		// Fallback: scan known video SSRCs for the first one that is NOT itself RTX.
		for _, ssrc := range s.getVideoSSRCs() {
			if ssrc == rtxSSRC {
				continue
			}
			if _, isAlsoRTX := s.rtxMapping.isRTX(s.getPTForSSRC(ssrc)); !isAlsoRTX {
				mediaSSRC = ssrc
				hasMap = true
				break
			}
		}
	}

	if !hasMap {
		s.logf("[RTX] rtxSSRC=%d rtxPT=%d apt=%d recoveredSeq=%d — mediaSSRC unknown, dropping",
			rtxSSRC, rtxPkt.Header.PayloadType, mediaPT, originalSeq)
		return
	}

	// Check if this seq was in our NACK missing set.
	wasExpected := s.nackSt.recover(mediaSSRC, originalSeq)

	if wasExpected {
		if st, _ := s.ssrcReg.getOrCreate(rtxSSRC); st != nil {
			st.mu.Lock()
			st.nackRecovered++
			st.mu.Unlock()
		}
	}

	// Reconstruct the original RTP packet.
	reconstructed := &rtp.Packet{
		Header: rtp.Header{
			Version:        rtxPkt.Header.Version,
			Padding:        rtxPkt.Header.Padding,
			Extension:      rtxPkt.Header.Extension,
			Marker:         rtxPkt.Header.Marker,
			PayloadType:    mediaPT,
			SequenceNumber: originalSeq,
			Timestamp:      rtxPkt.Header.Timestamp,
			SSRC:           mediaSSRC,
			CSRC:           rtxPkt.Header.CSRC,
		},
		Payload: rtxPkt.Payload[2:], // strip 2-byte OSN
	}

	if s.onRTPPacket != nil {
		s.onRTPPacket(reconstructed)
	}
}

// getPTForSSRC returns the payload type last seen for a given SSRC, or 0 if unknown.
// Used only in RTX fallback path to detect whether a candidate media SSRC is itself RTX.
func (s *rawShadowSession) getPTForSSRC(ssrc uint32) uint8 {
	if s.ssrcReg == nil {
		return 0
	}
	s.ssrcReg.mu.Lock()
	defer s.ssrcReg.mu.Unlock()
	if st, ok := s.ssrcReg.stats[ssrc]; ok {
		return st.pt
	}
	return 0
}

// mediaSummaryLoop kept as a placeholder — logging disabled for clean output.
// Re-enable by uncommenting the EmitMediaSummary call below.
func (s *rawShadowSession) mediaSummaryLoop(ctx context.Context) {
	const interval = 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	baseline := newSummaryBaseline()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-enabled: 5-second ingress (SFU→bcr_client) health summary so we can
			// measure per-role packets/bitrate/keyframes/LOST/nack/recovered — this is
			// how we quantify the video packet loss on the media leg.
			s.mu.Lock()
			ptMap := make(map[uint8]CodecInfo, len(s.ptCodecMap))
			for k, v := range s.ptCodecMap {
				ptMap[k] = v
			}
			s.mu.Unlock()
			s.rtcpMu.Lock()
			rr, remb, pli, nack := s.rtcpRRCount, s.rtcpREMBCount, s.rtcpPLICount, s.rtcpNACKCount
			s.rtcpMu.Unlock()
			EmitMediaSummary(s.bridgeID, s.ssrcReg, ptMap, s.rtxMapping, rr, remb, pli, nack, interval.Seconds(), baseline, s.logf)
		}
	}
}
