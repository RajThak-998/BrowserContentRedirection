package engine

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// SignalingConn defines the interface for bridge control messaging.
type SignalingConn interface {
	WriteMessage(msgType int, data []byte) error
	ReadMessage() (int, []byte, error)
	Close() error
}

// TCPSignalingConn adapts a raw net.Conn to frame and unframe messages
// using a [4-byte Big-Endian Length][JSON payload] format.
type TCPSignalingConn struct {
	net.Conn
}

func (c *TCPSignalingConn) WriteMessage(msgType int, data []byte) error {
	length := uint32(len(data))
	buf := make([]byte, 4+length)
	binary.BigEndian.PutUint32(buf[0:4], length)
	copy(buf[4:], data)
	_, err := c.Conn.Write(buf)
	return err
}

func (c *TCPSignalingConn) ReadMessage() (int, []byte, error) {
	var length uint32
	if err := binary.Read(c.Conn, binary.BigEndian, &length); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.Conn, payload); err != nil {
		return 0, nil, err
	}
	return 1, payload, nil // 1 = TextMessage
}

// Engine manages the dynamic virtual channel (or TCP loopback bridge) between the browser extension (via bcr_host)
// and the local raw ICE/DTLS/SRTP shadow transport + Pion relay to the Wails frontend.
type Engine struct {
	cfg Config
	cb  Callbacks

	bridgeMu    sync.RWMutex
	controlConn SignalingConn

	shadowMu       sync.Mutex
	rawSessions    map[string]*rawShadowSession
	activeBridgeID string

	relayMu          sync.Mutex
	loopbackSessions map[string]*loopbackSession

	connectMu   sync.Mutex
	bridgeRetry map[string]bridgeRetryState

	// Diagnostic: track unknown PTs to avoid log spam.
	unknownPTMu sync.Mutex
	unknownPTs  map[uint8]bool
}

func New(cfg Config, cb Callbacks) *Engine {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8081"
	}
	return &Engine{
		cfg:              cfg,
		cb:               cb,
		rawSessions:      make(map[string]*rawShadowSession),
		loopbackSessions: make(map[string]*loopbackSession),
		bridgeRetry:      make(map[string]bridgeRetryState),
	}
}

func (e *Engine) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", e.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to start TCP listener on %s: %w", e.cfg.ListenAddr, err)
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	e.logf("[bcr_client] TCP signaling server listening on %s", e.cfg.ListenAddr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				e.logf("[bcr_client] TCP accept error: %v", err)
				continue
			}
		}

		go e.handleTCPConnection(conn)
	}
}

func (e *Engine) handleTCPConnection(netConn net.Conn) {
	conn := &TCPSignalingConn{Conn: netConn}

	e.bridgeMu.Lock()
	if e.controlConn != nil {
		_ = e.controlConn.Close()
	}
	e.controlConn = conn
	e.bridgeMu.Unlock()

	e.logf("[bcr_client] TCP signaling connection accepted from %s", netConn.RemoteAddr())

	defer func() {
		e.bridgeMu.Lock()
		if e.controlConn == conn {
			e.controlConn = nil
		}
		e.bridgeMu.Unlock()
		_ = conn.Close()
		e.logf("[bcr_client] TCP signaling connection closed")
	}()

	e.readControlLoop(conn)
}

func (e *Engine) readControlLoop(conn SignalingConn) {
	mediaCh := make(chan []byte, 4096)
	defer close(mediaCh)

	go func() {
		for message := range mediaCh {
			e.handleMediaChunk(message)
		}
	}()

	for {
		mt, message, err := conn.ReadMessage()
		if err != nil {
			return
		}

		// Binary messages are media chunk frames: [u32 headerLen LE][headerJSON][rawChunk]
		if mt != 1 {
			select {
			case mediaCh <- message:
			default:
				go e.handleMediaChunk(message)
			}
			continue
		}

		// Text messages: check if it's a binary frame embedded as text (starts with non-'{').
		// The bridge sends binary frames with msgType=BinaryMessage, but we guard both paths.
		if len(message) >= 4 && message[0] != '{' {
			select {
			case mediaCh <- message:
			default:
				go e.handleMediaChunk(message)
			}
			continue
		}

		if e.handleShadowPacket(conn, message) {
			continue
		}

		if e.handleVideoUpdate(message) {
			continue
		}

		if e.handleYTDiag(message) {
			continue
		}

		e.handleVideoLifecycle(message)
	}
}

func (e *Engine) handleVideoUpdate(message []byte) bool {
	var evt VideoUpdate
	if err := json.Unmarshal(message, &evt); err != nil {
		return false
	}
	if evt.Type != "VIDEO_UPDATE" {
		return false
	}
	if e.cb.OnVideoUpdate != nil {
		e.cb.OnVideoUpdate(evt)
	}
	return true
}

// handleYTDiag routes the extension's YouTube virtual-clock diagnostic line to the
// OnYTDiag callback so it can be written into bcr_client.log (the interceptor's own
// logs only reach the VDI browser console otherwise).
func (e *Engine) handleYTDiag(message []byte) bool {
	var pkt struct {
		Type    string `json:"type"`
		Payload struct {
			Message string `json:"message"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(message, &pkt); err != nil {
		return false
	}
	if pkt.Type != "YT_DIAG" {
		return false
	}
	if e.cb.OnYTDiag != nil {
		e.cb.OnYTDiag(pkt.Payload.Message)
	}
	return true
}

func (e *Engine) handleVideoLifecycle(message []byte) bool {
	var evt VideoLifecycle
	if err := json.Unmarshal(message, &evt); err != nil {
		return false
	}
	if evt.Type != "VIDEO_ADDED" && evt.Type != "VIDEO_REMOVED" && evt.Type != "VIDEO_SOURCE_GONE" {
		return false
	}
	if e.cb.OnVideoLifecycle != nil {
		e.cb.OnVideoLifecycle(evt.Type, evt.Payload.ID)
	}
	return true
}

// handleMediaChunk parses a binary media frame and dispatches the chunk
// to the OnMediaChunk callback. Frame format: [u32 headerLen LE][headerJSON][rawChunk]
func (e *Engine) handleMediaChunk(data []byte) {
	if e.cb.OnMediaChunk == nil {
		return
	}
	if len(data) < 4 {
		e.logf("[bcr_client][media] binary frame too small (%d bytes) — dropped", len(data))
		return
	}

	headerLen := int(binary.LittleEndian.Uint32(data[:4]))
	if headerLen <= 0 || 4+headerLen > len(data) {
		e.logf("[bcr_client][media] invalid header length=%d frame=%d — dropped", headerLen, len(data))
		return
	}

	headerBytes := data[4 : 4+headerLen]
	chunkBytes := data[4+headerLen:]

	// The outer frame has type MEDIA_CHUNK with a nested payload.
	// Unmarshal via the framed header struct.
	type frameHeader struct {
		Type    string          `json:"type"`
		Payload MediaChunkHeader `json:"payload"`
	}
	var fh frameHeader
	if err := json.Unmarshal(headerBytes, &fh); err != nil {
		e.logf("[bcr_client][media] header decode failed: %v", err)
		return
	}
	if fh.Type != "MEDIA_CHUNK" {
		e.logf("[bcr_client][media] unexpected binary frame type=%q — dropped", fh.Type)
		return
	}

	e.cb.OnMediaChunk(fh.Payload, chunkBytes)
}

// ─── Raw Session Lifecycle ────────────────────────────────────────────────────

// getOrCreateRawSession returns an existing session or creates a fresh one.
// The onRTPPacket callback is wired here so it is always set before Init runs.
func (e *Engine) getOrCreateRawSession(bridgeID string) *rawShadowSession {
	e.shadowMu.Lock()
	defer e.shadowMu.Unlock()

	if session, ok := e.rawSessions[bridgeID]; ok {
		return session
	}

	session := newRawShadowSession(bridgeID, e.logf)
	// Callback is set once at creation; it is safe to read ptCodecMap since
	// that map is populated before Init() starts (in handleShadowLocal).
	session.onRTPPacket = func(pkt *rtp.Packet) {
		// ── Snapshot ptCodecMap under lock (Bug-4 fix) ─────────────────────
		// mergeCodecsAndSSRCs writes this map from the WebSocket goroutine
		// while srtpReadLoop reads it from the SRTP goroutine — a data race
		// that can cause a concurrent-map panic under renegotiation. We take
		// a cheap copy here (typically 2–4 entries) to give each packet its
		// own immutable view of the codec table.
		session.mu.Lock()
		ptMap := make(map[uint8]CodecInfo, len(session.ptCodecMap))
		for k, v := range session.ptCodecMap {
			ptMap[k] = v
		}
		session.mu.Unlock()

		// Track video SSRCs so the RTCP heartbeat loop can send PLI.
		if codec, ok := ptMap[pkt.Header.PayloadType]; ok {
			if strings.Contains(strings.ToUpper(codec.MimeType), "VIDEO") {
				session.trackVideoSSRC(pkt.SSRC)
			}
		}
		e.onRawRTPPacket(bridgeID, pkt, ptMap)
	}
	e.rawSessions[bridgeID] = session
	return session
}

// closeShadowSession tears down the raw session and associated relay session
// for the given bridgeID.
func (e *Engine) closeShadowSession(bridgeID string) {
	e.shadowMu.Lock()
	session, ok := e.rawSessions[bridgeID]
	if ok {
		delete(e.rawSessions, bridgeID)
	}
	if e.activeBridgeID == bridgeID {
		e.activeBridgeID = ""
	}
	e.shadowMu.Unlock()

	if ok {
		session.Close()
	}

	e.closeLoopbackSession(bridgeID)
}

func (e *Engine) closeLoopbackSession(bridgeID string) {
	e.relayMu.Lock()
	session, ok := e.loopbackSessions[bridgeID]
	if ok {
		delete(e.loopbackSessions, bridgeID)
	}
	e.relayMu.Unlock()

	if ok && session != nil {
		session.Close()
		e.logf("[bcr_client][loopback] session closed bridgeId=%s", bridgeID)
	}
}

type bridgeRetryState struct {
	attempts    int
	cooldown    bool
	cooldownEnd time.Time
}

const (
	maxConnectAttempts = 3
	retryBaseDelay     = 500 * time.Millisecond
	cooldownDuration   = 10 * time.Second
)

// triggerConnect starts a Connect attempt for the given generation token.
// It is the *only* call-site that transitions a session into stateConnecting.
func (e *Engine) triggerConnect(conn SignalingConn, bridgeID string, session *rawShadowSession, remoteSDP string, sdpType string, gen uint32) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		e.logf("[raw][%s] triggering ICE+DTLS+SRTP connect gen=%d", bridgeID, gen)
		err := session.Connect(ctx, remoteSDP, gen)
		if err != nil {
			if strings.Contains(err.Error(), "ErrDuplicateConnect") || strings.Contains(err.Error(), "ErrStaleGeneration") {
				e.logf("[raw][%s] glare/stale-gen rejected connect gen=%d: %v", bridgeID, gen, err)
				return
			}

			e.logf("[raw][%s] Connect failed gen=%d: %v", bridgeID, gen, err)

			e.connectMu.Lock()
			rs := e.bridgeRetry[bridgeID]
			rs.attempts++
			if rs.attempts >= maxConnectAttempts {
				rs.cooldown = true
				rs.cooldownEnd = time.Now().Add(cooldownDuration)
				e.bridgeRetry[bridgeID] = rs
				e.connectMu.Unlock()

				e.logf("[raw][%s] max attempts (%d) reached — entering %s cool-down",
					bridgeID, maxConnectAttempts, cooldownDuration)
				e.sendShadowError(conn, bridgeID, "cooldown", err, false)
				e.closeShadowSession(bridgeID)
				return
			}
			backoff := retryBaseDelay * time.Duration(1<<rs.attempts)
			e.bridgeRetry[bridgeID] = rs
			e.connectMu.Unlock()

			// Cache state to pass to new session
			session.mu.Lock()
			iceServers := session.iceServers
			ptCodecMap := session.ptCodecMap
			// Carried over so a retried INCOMING call still knows which DTLS
			// role we advertised; without it Connect() would fall back to the
			// SFU offer's "actpass" and pick the wrong side again.
			localDTLSSetup := session.localDTLSSetup
			
			var cert tls.Certificate
			var localCreds *RTCShadowReadyPayload
			if !DisableTransportStateReuse {
				cert = session.cert
				localCreds = session.localCredentials
			} else {
				e.logf("[raw][%s] [VDI-DEBUG] retry: bypassing caching of cert and local credentials due to DisableTransportStateReuse", bridgeID)
			}
			session.mu.Unlock()

			e.closeShadowSession(bridgeID)
			newSession := e.getOrCreateRawSession(bridgeID)

			newSession.mu.Lock()
			newSession.iceServers = iceServers
			newSession.ptCodecMap = ptCodecMap
			if !DisableTransportStateReuse {
				newSession.cert = cert
				newSession.localCredentials = localCreds
			}
			newSession.isOfferer = (sdpType == "offer")
			newSession.localDTLSSetup = localDTLSSetup
			newSession.mu.Unlock()

			go e.retryBridge(conn, bridgeID, newSession, remoteSDP, sdpType, backoff)
			return
		}

		e.connectMu.Lock()
		rs := e.bridgeRetry[bridgeID]
		rs.attempts = 0
		rs.cooldown = false
		e.bridgeRetry[bridgeID] = rs
		e.connectMu.Unlock()

		e.promoteActiveBridge(bridgeID, "srtp_ready")
		e.logf("[raw][%s] transport up — SRTP decryption active", bridgeID)
	}()
}

func (e *Engine) retryBridge(conn SignalingConn, bridgeID string, session *rawShadowSession, remoteSDP string, sdpType string, backoff time.Duration) {
	e.logf("[raw][%s] delaying retry attempt by %s", bridgeID, backoff)
	time.Sleep(backoff)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Set up late candidate trickle BEFORE Init.
	session.mu.Lock()
	session.onLateCandidate = func(candidate string) {
		payload := RTCShadowCandidatePayload{
			BridgeID:  bridgeID,
			Candidate: candidate,
			Timestamp: time.Now().UnixMilli(),
		}
		if err := writeJSONPacket(conn, "RTC_SHADOW_ICE_CANDIDATE", payload); err != nil {
			e.logf("[raw][%s] late ICE candidate trickle failed: %v", bridgeID, err)
		}
	}
	session.onGatheringComplete = func() {
		payload := RTCShadowCandidatePayload{
			BridgeID:        bridgeID,
			EndOfCandidates: true,
			Timestamp:       time.Now().UnixMilli(),
		}
		if err := writeJSONPacket(conn, "RTC_SHADOW_ICE_CANDIDATE", payload); err != nil {
			e.logf("[raw][%s] end-of-candidates signal failed: %v", bridgeID, err)
			return
		}
		e.logf("[raw][%s] end-of-candidates sent", bridgeID)
	}
	session.mu.Unlock()

	ready, err := session.Init(ctx, sdpType)
	if err != nil {
		e.logf("[raw][%s] retry Init failed: %v", bridgeID, err)
		e.sendShadowError(conn, bridgeID, "retry_init", err, true)
		return
	}

	if err := writeJSONPacket(conn, "RTC_SHADOW_READY", ready); err != nil {
		e.logf("[raw][%s] send SHADOW_READY failed: %v", bridgeID, err)
	}
	e.logf("[bcr_client] RTC_SHADOW_READY (retry) sent bridgeId=%s sdpType=%s ufrag=%s cands=%d",
		bridgeID, sdpType, ready.ICEUfrag, len(ready.Candidates))

	session.mu.Lock()
	gen := session.generation
	session.mu.Unlock()

	e.triggerConnect(conn, bridgeID, session, remoteSDP, sdpType, gen)
}

// ─── handleShadowLocal ────────────────────────────────────────────────────────

// handleShadowLocal is called when the browser fires its local SDP event
// (setLocalDescription). This is phase 1 of the raw transport: generate a
// self-signed cert + ICE agent, gather candidates, and return SHADOW_READY.
func (e *Engine) handleShadowLocal(conn SignalingConn, payload RTCShadowLocalPayload) {
	if payload.BridgeID == "" || payload.SDP == "" {
		e.sendShadowError(conn, payload.BridgeID, "validate_local", nil, false)
		return
	}

	// Full SDP summary goes to the log file only — keeps the terminal readable.
	e.vlogf("[bcr_client] [SDP-INSPECT] bridgeId=%s type=%s %s",
		payload.BridgeID, payload.SDPType, ShortenSDP(payload.SDP))


	sdpType := normalizeSDPType(payload.SDPType)
	bridgeID := payload.BridgeID

	session := e.getOrCreateRawSession(bridgeID)

	// Guard: if Init() is already in progress or done, skip (dedup).
	session.mu.Lock()
	if session.state != stateNew {
		currentState := session.state
		session.mu.Unlock()

		if currentState == stateConnected || currentState == stateConnecting || currentState == stateReady {
			e.handleRenegotiationLocal(conn, session, bridgeID, sdpType, payload.SDP)
			return
		}

		e.logf("[raw][%s] Init already in progress — ignoring duplicate %s sdp (state=%v)", bridgeID, sdpType, currentState)
		return
	}
	session.mu.Unlock()

	// Store ICE servers from browser's RTCPeerConnection.
	if len(payload.IceServers) > 0 {
		session.iceServers = payload.IceServers
		e.logf("[raw][%s] stored %d ICE server(s)", bridgeID, len(payload.IceServers))
	}

	// Parse PT → codec map from the LOCAL SDP. The browser's SDP contains the
	// codec PTs that the remote SFU will use to send media — exactly what we
	// need to decrypt and relay.
	//
	// NOTE: We store the FULL codec map (no preferred filter). The SFU may
	// renegotiate with different PT numbers, and we must accept any PT it sends.
	// The extension's SDP codec pinning constrains WHAT the SFU sends;
	// this map just lets us identify the packets when they arrive.
	//
	// Merged rather than replaced: on an INCOMING call the SFU's offer arrives
	// first and handleShadowRemote has already populated this map from it.
	// Assigning a fresh map here would discard that.
	localCodecs := ParsePTCodecMap(payload.SDP)
	session.mu.Lock()
	for pt, info := range localCodecs {
		session.ptCodecMap[pt] = info
	}
	codecCount := len(session.ptCodecMap)
	// The a=setup: role we are advertising to the SFU. Captured here because
	// Connect() only receives the remote SDP, and on an incoming call the
	// remote side says "actpass" — leaving the choice to this value.
	session.localDTLSSetup = firstSDPAttr(payload.SDP, "a=setup:")
	localSetup := session.localDTLSSetup
	session.mu.Unlock()
	e.logf("[raw][%s] local %s SDP: %d codec(s), a=setup:%s", bridgeID, sdpType, codecCount, orNone(localSetup))
	// Populate RTX mappings from SDP fmtp:apt= lines.
	for rtxPT, mediaPT := range extractFmtpAPT(payload.SDP) {
		session.rtxMapping.set(rtxPT, mediaPT)
	}
	// Populate RTX SSRC→media SSRC mapping from a=ssrc-group:FID lines.
	fidGroups := ExtractFIDGroups(payload.SDP)
	if len(fidGroups) > 0 {
		session.rtxSSRCMu.Lock()
		for rtxSSRC, mediaSSRC := range fidGroups {
			session.rtxSSRCtoMedia[rtxSSRC] = mediaSSRC
		}
		session.rtxSSRCMu.Unlock()
	}

	allLocalSSRCs := ExtractAllSSRCs(payload.SDP)
	if len(allLocalSSRCs) > 0 {
		session.mu.Lock()
		session.localSenderSSRC = allLocalSSRCs[0]
		session.mu.Unlock()
	}
	for rtxSSRC, mediaSSRC := range fidGroups {
		_ = rtxSSRC
		session.mu.Lock()
		session.localVideoSSRC = mediaSSRC
		session.mu.Unlock()
		break
	}
	if cname := ExtractCNAME(payload.SDP); cname != "" {
		session.mu.Lock()
		session.localCNAME = cname
		session.mu.Unlock()
	}
	if extID := ExtractTransportCCExtID(payload.SDP); extID != 0 {
		session.transportCCExtID = extID
	}

	// Mark ICE role: offerer = controlling (Dial), answerer = controlled (Accept).
	session.mu.Lock()
	session.isOfferer = sdpType == "offer"
	session.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Set up late candidate trickle BEFORE Init so no candidates are missed.
		session.mu.Lock()
		session.onLateCandidate = func(candidate string) {
			payload := RTCShadowCandidatePayload{
				BridgeID:  bridgeID,
				Candidate: candidate,
				Timestamp: time.Now().UnixMilli(),
			}
			if err := writeJSONPacket(conn, "RTC_SHADOW_ICE_CANDIDATE", payload); err != nil {
				e.logf("[raw][%s] late ICE candidate trickle failed: %v", bridgeID, err)
			}
		}
		session.onGatheringComplete = func() {
			payload := RTCShadowCandidatePayload{
				BridgeID:        bridgeID,
				EndOfCandidates: true,
				Timestamp:       time.Now().UnixMilli(),
			}
			if err := writeJSONPacket(conn, "RTC_SHADOW_ICE_CANDIDATE", payload); err != nil {
				e.logf("[raw][%s] end-of-candidates signal failed: %v", bridgeID, err)
				return
			}
			e.logf("[raw][%s] end-of-candidates sent", bridgeID)
		}
		session.mu.Unlock()

		// The ICE agent is created here, at call time. Init() blocks only until the
		// first server-reflexive candidate is in hand (see gatherWaitBudget), then
		// returns so the offer can go out; everything gathered afterwards is
		// trickled via onLateCandidate, and onGatheringComplete closes the stream.
		ready, err := session.Init(ctx, sdpType)
		if err != nil {
			e.logf("[raw][%s] Init failed: %v", bridgeID, err)
			e.sendShadowError(conn, bridgeID, "init_failed", err, true)
			return
		}

		// Reconstruct the munged SDP the browser will return to Teams and dump it
		// verbatim, for pasting into chrome://webrtc-internals or diffing against
		// the cpconv request body.
		//
		// It is a ~200-line block per negotiation — several times the size of the
		// rest of a healthy session's log — so it is opt-in via BCR_SDP_DUMP=1.
		// The one-line [SDP-INSPECT] summary above always runs and carries the
		// ufrag / fingerprint / c= / mids that most questions actually need.
		if envEnabled("BCR_SDP_DUMP") {
			munged := MungeSDPTransport(payload.SDP, ready.ICEUfrag, ready.ICEPwd, ready.DTLSFingerprint)
			e.vlogf("[bcr_client] [SDP-MUNGED] bridgeId=%s type=%s\n--- BEGIN MUNGED SDP ---\n%s\n--- END MUNGED SDP ---",
				bridgeID, sdpType, munged)
		}

		if err := writeJSONPacket(conn, "RTC_SHADOW_READY", ready); err != nil {
			e.logf("[raw][%s] send SHADOW_READY failed: %v", bridgeID, err)
		}
		e.logf("[bcr_client] RTC_SHADOW_READY sent bridgeId=%s sdpType=%s ufrag=%s cands=%d",
			bridgeID, sdpType, ready.ICEUfrag, len(ready.Candidates))

		// Answerer path: if SHADOW_REMOTE(offer) already arrived, connect now.
		// Offerer path: Connect() will be triggered by SHADOW_REMOTE(answer).
		if sdpType == "answer" {
			session.mu.Lock()
			rSDP := session.remoteOfferSDP
			gen := session.generation
			session.mu.Unlock()

			if rSDP != "" {
				e.triggerConnect(conn, bridgeID, session, rSDP, sdpType, gen)
			} else {
				e.logf("[raw][%s] Init(answer) done — waiting for remote offer SDP", bridgeID)
			}
		} else {
			// Offerer path: nothing more happens until the SFU's answer round-trips
			// back through Teams' signaling and arrives as RTC_SHADOW_REMOTE. If the
			// "RTC_SHADOW_REMOTE ... sdpType=answer" line never appears below, the SFU
			// answer never reached us (offer/answer exchange with the SFU failed).
			e.logf("[raw][%s] SHADOW_READY(offer) sent — now AWAITING RTC_SHADOW_REMOTE (SFU answer)", bridgeID)
		}
	}()
}

// ─── handleShadowRemote ───────────────────────────────────────────────────────

// handleShadowRemote is called when the browser receives and applies a remote
// SDP description (setRemoteDescription). For the offerer path this is the SFU's
// answer — trigger Connect(). For the answerer path this is the SFU's offer — store
// the remote SDP so Init(answer) can call Connect() after it finishes.
func (e *Engine) handleShadowRemote(conn SignalingConn, payload RTCShadowRemotePayload) {
	if payload.BridgeID == "" || payload.SDP == "" {
		e.sendShadowError(conn, payload.BridgeID, "validate_remote", nil, false)
		return
	}

	sdpType := normalizeSDPType(payload.SDPType)
	bridgeID := payload.BridgeID

	// ── Cool-down guard ──────────────────────────────────────────────────────
	e.connectMu.Lock()
	rs := e.bridgeRetry[bridgeID]
	if rs.cooldown && time.Now().Before(rs.cooldownEnd) {
		remaining := time.Until(rs.cooldownEnd).Round(time.Millisecond)
		e.connectMu.Unlock()
		e.logf("[raw][%s] SHADOW_REMOTE rejected — in cool-down for %s more", bridgeID, remaining)
		return
	}
	if rs.cooldown && time.Now().After(rs.cooldownEnd) {
		rs.cooldown = false
		rs.attempts = 0
		e.bridgeRetry[bridgeID] = rs
	}
	e.connectMu.Unlock()

	session := e.getOrCreateRawSession(bridgeID)

	// ── Glare guard ──────────────────────────────────────────────────────────
	session.mu.Lock()
	currentState := session.state
	sdpHash := hashSDP(payload.SDP)
	isDuplicate := sdpHash == session.lastRemoteSDPHash
	gen := session.generation
	session.mu.Unlock()

	switch currentState {
	case stateConnecting, stateConnected:
		if sdpType == "offer" || sdpType == "answer" {
			e.handleRenegotiationRemote(session, bridgeID, sdpType, payload.SDP)
		}
		e.logf("[raw][%s] SHADOW_REMOTE %s handled — session is %v (codec map updated, transport unchanged)",
			bridgeID, sdpType, currentState)
		return
	case stateClosed:
		e.logf("[raw][%s] SHADOW_REMOTE %s rejected — session is CLOSED", bridgeID, sdpType)
		return
	}

	if isDuplicate {
		e.logf("[raw][%s] SHADOW_REMOTE %s rejected — duplicate SDP (hash=%s)", bridgeID, sdpType, sdpHash[:8])
		return
	}

	session.mu.Lock()
	session.lastRemoteSDPHash = sdpHash
	session.mu.Unlock()

	// Store ICE servers if the remote payload carries them (answerer-path timing).
	if len(payload.IceServers) > 0 && len(session.iceServers) == 0 {
		session.iceServers = payload.IceServers
		e.logf("[raw][%s] stored %d ICE server(s) from remote payload", bridgeID, len(payload.IceServers))
	}

	switch sdpType {
	case "answer":
		// Offerer path: SFU answered our (munged) offer. Trigger ICE+DTLS+SRTP.
		e.logf("[bcr_client] RTC_SHADOW_REMOTE answer bridgeId=%s sdpLen=%d", bridgeID, len(payload.SDP))
		// Extract video SSRCs from the answer so PLI can fire as soon as SRTP is ready.
		e.mergeCodecsAndSSRCs(session, bridgeID, "answer", payload.SDP, false)
		e.triggerConnect(conn, bridgeID, session, payload.SDP, "offer", gen)

	case "offer":
		// Answerer path: SFU is the oferer. Store the remote offer SDP so that
		// once Init(answer) finishes, Connect() can use it.
		e.logf("[bcr_client] RTC_SHADOW_REMOTE offer bridgeId=%s sdpLen=%d", bridgeID, len(payload.SDP))
		// Extract video SSRCs from the offer so PLI can fire as soon as SRTP is ready.
		e.mergeCodecsAndSSRCs(session, bridgeID, "offer", payload.SDP, false)
		session.mu.Lock()
		session.remoteOfferSDP = payload.SDP
		isReady := session.state == stateReady
		session.mu.Unlock()

		if isReady {
			// Init(answer) already completed before this offer arrived — connect now.
			e.triggerConnect(conn, bridgeID, session, payload.SDP, "answer", gen)
		}

	default:
		e.logf("[raw][%s] unsupported remote sdpType=%q", bridgeID, payload.SDPType)
	}
}

// ─── handleShadowIceServers ───────────────────────────────────────────────────

func (e *Engine) handleShadowIceServers(payload RTCShadowIceServersPayload) {
	e.shadowMu.Lock()
	session, ok := e.rawSessions[payload.BridgeID]
	e.shadowMu.Unlock()

	if !ok {
		e.logf("[raw][%s] ICE servers for unknown session — ignored", payload.BridgeID)
		return
	}

	session.UpdateIceServers(payload.IceServers)
}

// ─── handleShadowICECandidate ─────────────────────────────────────────────────

func (e *Engine) handleShadowICECandidate(payload RTCShadowCandidatePayload) {
	e.shadowMu.Lock()
	session, ok := e.rawSessions[payload.BridgeID]
	e.shadowMu.Unlock()

	if !ok {
		e.logf("[raw][%s] ICE candidate for unknown session — ignored", payload.BridgeID)
		return
	}

	session.AddRemoteIceCandidate(payload.Candidate)
}

// ─── handleShadowPacket ───────────────────────────────────────────────────────

func (e *Engine) handleShadowPacket(conn SignalingConn, message []byte) bool {
	var pkt Packet
	if err := json.Unmarshal(message, &pkt); err != nil {
		return false
	}

	switch pkt.Type {
	case "RTC_SHADOW_LOCAL":
		var payload RTCShadowLocalPayload
		if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
			e.sendShadowError(conn, "", "decode_local", err, false)
			return true
		}
		e.logf("[bcr_client] RTC_SHADOW_LOCAL bridgeId=%s sdpType=%s sdpLen=%d",
			payload.BridgeID, payload.SDPType, len(payload.SDP))
		e.handleShadowLocal(conn, payload)
		return true

	case "RTC_SHADOW_REMOTE":
		var payload RTCShadowRemotePayload
		if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
			e.sendShadowError(conn, "", "decode_remote", err, false)
			return true
		}
		e.logf("[bcr_client] RTC_SHADOW_REMOTE bridgeId=%s sdpType=%s sdpLen=%d",
			payload.BridgeID, payload.SDPType, len(payload.SDP))
		e.handleShadowRemote(conn, payload)
		return true

	case "RTC_SHADOW_CLOSE":
		var payload RTCShadowClosePayload
		if err := json.Unmarshal(pkt.Payload, &payload); err == nil {
			e.closeShadowSession(payload.BridgeID)
			e.logf("[bcr_client] RTC_SHADOW_CLOSE bridgeId=%s", payload.BridgeID)
		}
		return true

	case "RTC_SHADOW_ICE_CANDIDATE":
		var payload RTCShadowCandidatePayload
		if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
			e.logf("[bcr_client] RTC_SHADOW_ICE_CANDIDATE decode failed: %v", err)
			return true
		}
		e.handleShadowICECandidate(payload)
		return true

	case "RTC_SHADOW_ICE_SERVERS":
		var payload RTCShadowIceServersPayload
		if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
			e.logf("[bcr_client] RTC_SHADOW_ICE_SERVERS decode failed: %v", err)
			return true
		}
		e.handleShadowIceServers(payload)
		return true

	default:
		return false
	}
}

// ─── Bridge Promotion ─────────────────────────────────────────────────────────

func (e *Engine) promoteActiveBridge(bridgeID, reason string) {
	e.shadowMu.Lock()
	if e.activeBridgeID == bridgeID {
		e.shadowMu.Unlock()
		return
	}
	oldActive := e.activeBridgeID
	e.activeBridgeID = bridgeID
	// Grab the session reference while holding shadowMu so we can pass it
	// to ensureLoopbackSession without a second map lookup under relayMu.
	session := e.rawSessions[bridgeID]
	e.shadowMu.Unlock()

	e.logf("[bcr_client][bridge] active bridge switched old=%s new=%s reason=%s", oldActive, bridgeID, reason)

	if oldActive != "" && oldActive != bridgeID {
		e.closeShadowSession(oldActive)
	}

	// ── Eager loopback creation (Bug-1 fix) ──────────────────────────────
	// At this point the remote answer SDP has already been merged into
	// session.ptCodecMap via mergeCodecsAndSSRCs. Creating the loopback now
	// (rather than on the first arriving RTP packet) guarantees the track
	// pre-creation uses the correct remote payload types, and fires the Pion
	// offer to the frontend ~200ms before media floods in.
	if session != nil {
		e.ensureLoopbackSession(bridgeID, session)
	}
}

// ensureLoopbackSession creates the Pion loopback PeerConnection (and fires
// the offer to the frontend) if one does not already exist for bridgeID.
// It snapshots ptCodecMap under the session mutex to be race-safe.
func (e *Engine) ensureLoopbackSession(bridgeID string, session *rawShadowSession) {
	// Fast-path: already exists (checked without lock first for performance).
	e.relayMu.Lock()
	_, alreadyExists := e.loopbackSessions[bridgeID]
	e.relayMu.Unlock()
	if alreadyExists {
		e.logf("[bcr_client][loopback] session already exists for bridgeId=%s — skipping eager creation", bridgeID)
		return
	}

	// Snapshot ptCodecMap under lock (same race-safety as the onRTPPacket closure).
	session.mu.Lock()
	ptMap := make(map[uint8]CodecInfo, len(session.ptCodecMap))
	for k, v := range session.ptCodecMap {
		ptMap[k] = v
	}
	session.mu.Unlock()

	e.logf("[bcr_client][loopback] creating loopback session bridgeId=%s codecs=%d", bridgeID, len(ptMap))

	e.relayMu.Lock()
	// Double-check under lock to prevent TOCTOU race if two goroutines race here.
	if _, alreadyExists = e.loopbackSessions[bridgeID]; alreadyExists {
		e.relayMu.Unlock()
		e.logf("[bcr_client][loopback] session created concurrently for bridgeId=%s — skipping", bridgeID)
		return
	}
	ls := newLoopbackSession(
		bridgeID,
		e.logf,
		e.cb.OnLoopbackOffer,
		ptMap,
		// onRequestKeyframe
		func(bID string, ssrc uint32) {
			e.shadowMu.Lock()
			session := e.rawSessions[bID]
			e.shadowMu.Unlock()
			if session != nil {
				session.requestKeyframe(ssrc)
			}
		},
		// onNACK
		func(bID string, ssrc uint32, missing []uint16) {
			e.shadowMu.Lock()
			session := e.rawSessions[bID]
			e.shadowMu.Unlock()
			if session != nil {
				session.sendNACK(ssrc, missing)
			}
		},
	)
	e.loopbackSessions[bridgeID] = ls
	e.relayMu.Unlock()

	e.logf("[bcr_client][loopback] loopback session eagerly created bridgeId=%s", bridgeID)
}

// ReEmitAllLoopbackOffers re-fires the cached loopback offer SDP for every
// active session. Called when the Wails frontend signals it is ready to
// receive offers (recovering from cold-start timing races where the offer
// fired before EventsOn("onLocalLoopbackOffer") was registered).
func (e *Engine) ReEmitAllLoopbackOffers() {
	e.relayMu.Lock()
	snapshot := make(map[string]*loopbackSession, len(e.loopbackSessions))
	for id, s := range e.loopbackSessions {
		snapshot[id] = s
	}
	e.relayMu.Unlock()

	for bridgeID, session := range snapshot {
		if session == nil {
			continue
		}
		offer := session.GetLastOffer()
		if offer == "" {
			e.logf("[bcr_client][loopback] re-emit requested for bridgeId=%s but no offer SDP cached yet", bridgeID)
			continue
		}
		e.logf("[bcr_client][loopback] re-emitting loopback offer bridgeId=%s sdpLen=%d", bridgeID, len(offer))
		if e.cb.OnLoopbackOffer != nil {
			go e.cb.OnLoopbackOffer(bridgeID, offer)
		}
	}
}

func (e *Engine) shouldProcessBridgeTrack(bridgeID string) bool {
	e.shadowMu.Lock()
	defer e.shadowMu.Unlock()

	if e.activeBridgeID == "" {
		e.activeBridgeID = bridgeID
		e.logf("[bcr_client][bridge] active bridge claimed by first media bridgeId=%s", bridgeID)
		return true
	}

	return e.activeBridgeID == bridgeID
}

// ─── Codec & SSRC Merging ─────────────────────────────────────────────────────

// mergeCodecsAndSSRCs parses an SDP for new codec PTs and video SSRCs,
// merging them into the session. This is called for both initial answers
// and renegotiation offers/answers.
//
// The video SSRC extraction is critical: it breaks the PLI chicken-and-egg
// deadlock where the SFU won't send video without PLI, but we can't send
// PLI without knowing the SSRC. By parsing SSRCs from the SDP, the RTCP
// heartbeat loop can proactively send PLI before any video packets arrive.
func (e *Engine) mergeCodecsAndSSRCs(session *rawShadowSession, bridgeID, sdpType, sdp string, isLocal bool) {
	// Merge codec PTs — accept ALL codecs from remote SDPs without filtering.
	// The SFU may assign different PT numbers during renegotiation (e.g. H264
	// at PT=107 instead of PT=96). We must accept any PT so the relay can
	// identify incoming packets. The extension's SDP pinning constrains what
	// the SFU sends; this map just provides the PT→codec lookup.
	newCodecs := ParsePTCodecMap(sdp)
	// Populate RTX mappings into session.
	for rtxPT, mediaPT := range extractFmtpAPT(sdp) {
		session.rtxMapping.set(rtxPT, mediaPT)
	}
	// Populate RTX SSRC→media SSRC mapping from FID groups.
	for rtxSSRC, mediaSSRC := range ExtractFIDGroups(sdp) {
		session.rtxSSRCMu.Lock()
		session.rtxSSRCtoMedia[rtxSSRC] = mediaSSRC
		session.rtxSSRCMu.Unlock()
	}
	session.mu.Lock()
	added := 0
	for pt, info := range newCodecs {
		if _, exists := session.ptCodecMap[pt]; !exists {
			session.ptCodecMap[pt] = info
			added++
		}
	}
	session.mu.Unlock()

	if isLocal {
		allSSRCs := ExtractAllSSRCs(sdp)
		if len(allSSRCs) > 0 {
			session.mu.Lock()
			session.localSenderSSRC = allSSRCs[0]
			session.mu.Unlock()
		}
		for _, mediaSSRC := range ExtractFIDGroups(sdp) {
			session.mu.Lock()
			session.localVideoSSRC = mediaSSRC
			session.mu.Unlock()
			break
		}
		if cname := ExtractCNAME(sdp); cname != "" {
			session.mu.Lock()
			session.localCNAME = cname
			session.mu.Unlock()
		}
		if extID := ExtractTransportCCExtID(sdp); extID != 0 {
			session.transportCCExtID = extID
		}
	} else {
		videoSSRCs := ExtractVideoSSRCs(sdp)
		for _, ssrc := range videoSSRCs {
			session.trackVideoSSRC(ssrc)
		}
		// TWCC Asymmetry Fix: re-extract from remote SDP.
		remoteExtID := ExtractTransportCCExtID(sdp)
		session.transportCCExtID = remoteExtID
	}
}

// ─── Renegotiation Handlers ───────────────────────────────────────────────────

func (e *Engine) handleRenegotiationLocal(conn SignalingConn, session *rawShadowSession, bridgeID string, sdpType string, sdp string) {
	// Update codecs and SSRCs from the new local SDP
	e.mergeCodecsAndSSRCs(session, bridgeID, sdpType, sdp, true)
	e.syncLoopbackTracks(bridgeID, session)

	ready, err := session.GetTransportCredentials(sdpType)
	if err != nil {
		e.logf("[raw][%s] renegotiation credentials failed: %v", bridgeID, err)
		return
	}
	if err := writeJSONPacket(conn, "RTC_SHADOW_READY", ready); err != nil {
		e.logf("[raw][%s] renegotiation SHADOW_READY send failed: %v", bridgeID, err)
		return
	}
	// Success used to be silent here, so bcr_host.log could show a READY going
	// down with nothing in bcr_client.log to account for it. The candidate count
	// is the interesting part: it is what the extension will inject inline into
	// the munged SDP, and anything gathered after Init() is missing from it.
	e.logf("[raw][%s] renegotiation SHADOW_READY sent sdpType=%s ufrag=%s cands=%d",
		bridgeID, sdpType, ready.ICEUfrag, len(ready.Candidates))
}

// syncLoopbackTracks snapshots the session's current codec map under the
// session mutex, filters it to preferred codecs, and calls
// SyncTracksFromCodecMap on the active loopback session for bridgeID.
// It is a no-op if no loopback session exists yet for that bridgeID.
func (e *Engine) syncLoopbackTracks(bridgeID string, session *rawShadowSession) {
	e.relayMu.Lock()
	ls := e.loopbackSessions[bridgeID]
	e.relayMu.Unlock()

	if ls == nil {
		return
	}

	session.mu.Lock()
	ptMap := make(map[uint8]CodecInfo, len(session.ptCodecMap))
	for k, v := range session.ptCodecMap {
		ptMap[k] = v
	}
	session.mu.Unlock()

	ls.SyncTracksFromCodecMap(ptMap)
}

func (e *Engine) handleRenegotiationRemote(session *rawShadowSession, bridgeID string, sdpType string, sdp string) {
	e.logf("[raw][%s] [Renegotiation Offer/Answer Received] Parsing remote %s SDP", bridgeID, sdpType)

	// Log the exact track mapping and m-lines
	sections := ExtractMediaSections(sdp)
	for _, sec := range sections {
		e.logf("[raw][%s] [Renegotiation Track Mapping] Section: m=%s %s %s", bridgeID, sec.Type, sec.Port, sec.Protocol)
	}

	// Say whether the peer's transport actually moved, rather than assuming it
	// did not. CHANGED=true means the live ICE agent is authenticating against
	// credentials the peer has already abandoned, and no amount of retrying the
	// current agent can recover — see transportDiff.
	session.mu.Lock()
	prevRemote := session.connectedRemoteSDP
	session.mu.Unlock()
	diff, changed := transportDiff(prevRemote, sdp)
	e.logf("[raw][%s] renegotiation %s transport: %s CHANGED=%v", bridgeID, sdpType, diff, changed)

	e.mergeCodecsAndSSRCs(session, bridgeID, sdpType, sdp, false)

	// ── Sync loopback tracks with newly merged codecs ─────────────────────────
	// Teams SFU uses a 2-phase pattern: audio-only initial answer followed
	// immediately by a renegotiation offer adding video tracks. The loopback
	// session is created at SRTP-ready time (after the initial answer), so
	// it only knows about audio. This call propagates the newly merged
	// video codec PTs (e.g. H264 PT=107) to the loopback, adding tracks
	// and firing a new Pion offer to the frontend.
	e.syncLoopbackTracks(bridgeID, session)
}

// ─── Utility Functions ────────────────────────────────────────────────────────

func (e *Engine) sendShadowError(conn SignalingConn, bridgeID, stage string, err error, retryable bool) {
	reason := "unknown"
	if err != nil {
		reason = err.Error()
	}
	_ = writeJSONPacket(conn, "RTC_SHADOW_ERROR", RTCShadowErrorPayload{
		BridgeID:  bridgeID,
		Stage:     stage,
		Reason:    reason,
		Retryable: retryable,
		Timestamp: time.Now().UnixMilli(),
	})
}

func writeJSONPacket(conn SignalingConn, packetType string, payload any) error {
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	out, err := json.Marshal(Packet{Type: packetType, Payload: payloadRaw})
	if err != nil {
		return err
	}
	return conn.WriteMessage(1, out) // 1 = TextMessage
}

// transportDiff compares the transport plane — ICE credentials, DTLS
// fingerprint, candidate count, a=setup: — of a newly arrived SDP against the
// one the live session was built from, and reports whether the peer moved.
//
// This exists because handleRenegotiationRemote used to log "transport
// unchanged" without ever checking. On an incoming call the post-pickup
// renegotiation is exactly where a transport change would appear, and if the
// SFU rotates its ICE credentials there we would keep authenticating every
// connectivity check against the pre-pickup ufrag forever.
//
// changed is driven by ufrag/pwd/fingerprint only. A differing candidate count
// is normal (the peer may simply have gathered more) and does not by itself
// mean the transport moved.
//
// The password is never logged; hashSDP gives a stable token that can be
// compared across lines without exposing the credential.
func transportDiff(prevSDP, newSDP string) (summary string, changed bool) {
	newUfrag, newPwd, newFP := ExtractShadowCredentials(newSDP)
	newFP = normalizeFingerprint(newFP)
	newCands := len(ExtractShadowCandidates(newSDP))
	newSetup := firstSDPAttr(newSDP, "a=setup:")

	if strings.TrimSpace(prevSDP) == "" {
		return fmt.Sprintf("ufrag=%s pwd_hash=%s fp=%s cands=%d setup=%s (no prior transport recorded)",
			orNone(newUfrag), safePrefix(hashSDP(newPwd), 8), safePrefix(newFP, 17),
			newCands, orNone(newSetup)), false
	}

	oldUfrag, oldPwd, oldFP := ExtractShadowCredentials(prevSDP)
	oldFP = normalizeFingerprint(oldFP)

	was := func(now, before string) string {
		if now == before {
			return ""
		}
		return "(was " + orNone(before) + ")"
	}

	changed = newUfrag != oldUfrag || newPwd != oldPwd || newFP != oldFP

	return fmt.Sprintf("ufrag=%s%s pwd_hash=%s%s fp=%s%s cands=%d(was %d) setup=%s",
		orNone(newUfrag), was(newUfrag, oldUfrag),
		safePrefix(hashSDP(newPwd), 8), was(safePrefix(hashSDP(newPwd), 8), safePrefix(hashSDP(oldPwd), 8)),
		safePrefix(newFP, 17), was(safePrefix(newFP, 17), safePrefix(oldFP, 17)),
		newCands, len(ExtractShadowCandidates(prevSDP)),
		orNone(newSetup)), changed
}

// hashSDP returns a 16-char MD5 hex prefix for dedup/logging.
func hashSDP(sdp string) string {
	h := md5.Sum([]byte(sdp))
	return hex.EncodeToString(h[:])[:16]
}

func normalizeSDPType(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

// vlogf emits a verbose diagnostic that goes to the log file only (never the
// terminal). Use it for high-volume output such as full SDP dumps.
func (e *Engine) vlogf(format string, args ...any) {
	if e.cb.OnVerboseLog == nil {
		return
	}
	e.cb.OnVerboseLog(fmt.Sprintf(format, args...))
}

func (e *Engine) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if e.cb.OnLog != nil {
		e.cb.OnLog(msg)
		return
	}
	fmt.Println(msg)
}

// SendYTPlayback forwards the frontend's MSE buffer state (bufferedEnd + playhead)
// upstream to bcr_host over the control connection, so it can be relayed to the
// extension interceptor to drive the YouTube seek pulse. Best-effort (dropped if no
// control connection is currently attached).
func (e *Engine) SendYTPlayback(bufferedEnd, playhead float64) {
	e.bridgeMu.RLock()
	conn := e.controlConn
	e.bridgeMu.RUnlock()
	if conn == nil {
		return
	}
	_ = writeJSONPacket(conn, "YT_PLAYBACK", map[string]float64{
		"bufferedEnd": bufferedEnd,
		"playhead":    playhead,
	})
}

// SetLoopbackAnswer applies the frontend's answer to the local loopback PC
func (e *Engine) SetLoopbackAnswer(bridgeID string, sdp string) {
	e.relayMu.Lock()
	session, ok := e.loopbackSessions[bridgeID]
	e.relayMu.Unlock()

	if ok && session != nil {
		session.SetRemoteDescription(sdp)
	} else {
		e.logf("[bcr_client][loopback] no active loopback session for bridgeId=%s to set answer", bridgeID)
	}
}
