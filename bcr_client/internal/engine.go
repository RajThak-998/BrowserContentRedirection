package engine

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
)

// Engine manages the WebSocket bridge between the browser extension (via bcr_host)
// and the local raw ICE/DTLS/SRTP shadow transport + Pion relay to the Wails frontend.
type Engine struct {
	cfg Config
	cb  Callbacks

	upgrader websocket.Upgrader
	server   *http.Server

	bridgeMu    sync.RWMutex
	controlConn *websocket.Conn
	dataConn    *websocket.Conn

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
	// Default preferred codecs: VP8 for video, Opus for audio.
	// The browser extension strips all other codecs from the SDP; this is
	// the Go-side safety net that drops any PTs that slip through.
	if cfg.PreferredCodecs.Video == "" {
		cfg.PreferredCodecs.Video = "VP8"
	}
	if cfg.PreferredCodecs.Audio == "" {
		cfg.PreferredCodecs.Audio = "opus"
	}

	return &Engine{
		cfg: cfg,
		cb:  cb,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		rawSessions:      make(map[string]*rawShadowSession),
		loopbackSessions: make(map[string]*loopbackSession),
		bridgeRetry:      make(map[string]bridgeRetryState),
	}
}

func (e *Engine) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", e.handleWS)

	e.server = &http.Server{
		Addr:    e.cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = e.server.Shutdown(shutdownCtx)
	}()

	e.logf("[bcr_client] websocket server listening on %s", e.cfg.ListenAddr)
	err := e.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (e *Engine) handleWS(w http.ResponseWriter, r *http.Request) {
	channel := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("channel")))
	if channel == "" {
		channel = "control"
	}

	if channel != "control" && channel != "data" {
		http.Error(w, "invalid channel", http.StatusBadRequest)
		return
	}

	conn, err := e.upgrader.Upgrade(w, r, nil)
	if err != nil {
		e.logf("[bcr_client] websocket upgrade failed: %v", err)
		return
	}

	previous := e.setBridgeConn(channel, conn)
	if previous != nil && previous != conn {
		_ = previous.Close()
	}

	e.logf("[bcr_client] websocket bridge connected channel=%s", channel)

	defer func() {
		e.clearBridgeConn(channel, conn)
		_ = conn.Close()
		e.logf("[bcr_client] websocket bridge disconnected channel=%s", channel)
	}()

	if channel == "data" {
		e.readDataLoop(conn)
		return
	}

	e.readControlLoop(conn)
}

func (e *Engine) setBridgeConn(channel string, conn *websocket.Conn) *websocket.Conn {
	e.bridgeMu.Lock()
	defer e.bridgeMu.Unlock()

	var previous *websocket.Conn
	if channel == "control" {
		previous = e.controlConn
		e.controlConn = conn
		return previous
	}

	previous = e.dataConn
	e.dataConn = conn
	return previous
}

func (e *Engine) clearBridgeConn(channel string, conn *websocket.Conn) {
	e.bridgeMu.Lock()
	defer e.bridgeMu.Unlock()

	if channel == "control" && e.controlConn == conn {
		e.controlConn = nil
	}

	if channel == "data" && e.dataConn == conn {
		e.dataConn = nil
	}
}

func (e *Engine) isDataConnected() bool {
	e.bridgeMu.RLock()
	defer e.bridgeMu.RUnlock()

	return e.dataConn != nil
}

func (e *Engine) waitForDataChannel(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if e.isDataConnected() {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(25 * time.Millisecond)
	}
}

func (e *Engine) readControlLoop(conn *websocket.Conn) {
	for {
		mt, message, err := conn.ReadMessage()
		if err != nil {
			return
		}

		if mt != websocket.TextMessage {
			continue
		}

		if e.handleShadowPacket(conn, message) {
			continue
		}

		if e.handleVideoUpdate(message) {
			continue
		}
	}
}

func (e *Engine) readDataLoop(conn *websocket.Conn) {
	for {
		mt, message, err := conn.ReadMessage()
		if err != nil {
			return
		}

		if mt != websocket.BinaryMessage {
			continue
		}

		e.logf("[bcr_client] dropped legacy binary media chunk (%d bytes) - loopback active", len(message))
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
		// Track video SSRCs so the RTCP heartbeat loop can send PLI.
		if codec, ok := session.ptCodecMap[pkt.Header.PayloadType]; ok {
			if strings.Contains(strings.ToUpper(codec.MimeType), "VIDEO") {
				session.trackVideoSSRC(pkt.SSRC)
			}
		}
		e.onRawRTPPacket(bridgeID, pkt, session.ptCodecMap)
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
	lastAttempt time.Time
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
func (e *Engine) triggerConnect(conn *websocket.Conn, bridgeID string, session *rawShadowSession, remoteSDP string, sdpType string, gen uint32) {
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
			session.mu.Unlock()

			e.closeShadowSession(bridgeID)
			newSession := e.getOrCreateRawSession(bridgeID)
			
			newSession.mu.Lock()
			newSession.iceServers = iceServers
			newSession.ptCodecMap = ptCodecMap
			newSession.isOfferer = (sdpType == "offer")
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

func (e *Engine) retryBridge(conn *websocket.Conn, bridgeID string, session *rawShadowSession, remoteSDP string, sdpType string, backoff time.Duration) {
	e.logf("[raw][%s] delaying retry attempt by %s", bridgeID, backoff)
	time.Sleep(backoff)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ready, err := session.Init(ctx, sdpType)
	if err != nil {
		e.logf("[raw][%s] retry Init failed: %v", bridgeID, err)
		e.sendShadowError(conn, bridgeID, "retry_init", err, true)
		return
	}

	if err := writeJSONPacket(conn, "RTC_SHADOW_READY", ready); err != nil {
		e.logf("[raw][%s] send SHADOW_READY failed: %v", bridgeID, err)
	}
	e.logf("[bcr_client] RTC_SHADOW_READY (retry) sent bridgeId=%s sdpType=%s", bridgeID, sdpType)

	session.mu.Lock()
	gen := session.generation
	session.mu.Unlock()

	e.triggerConnect(conn, bridgeID, session, remoteSDP, sdpType, gen)
}

// ─── handleShadowLocal ────────────────────────────────────────────────────────

// handleShadowLocal is called when the browser fires its local SDP event
// (setLocalDescription). This is phase 1 of the raw transport: generate a
// self-signed cert + ICE agent, gather candidates, and return SHADOW_READY.
func (e *Engine) handleShadowLocal(conn *websocket.Conn, payload RTCShadowLocalPayload) {
	if payload.BridgeID == "" || payload.SDP == "" {
		e.sendShadowError(conn, payload.BridgeID, "validate_local", nil, false)
		return
	}

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
	session.ptCodecMap = ParsePTCodecMap(payload.SDP)
	e.logf("[raw][%s] parsed %d codec(s) from local %s SDP", bridgeID, len(session.ptCodecMap), sdpType)

	// Extract video SSRCs from the local SDP so PLI can fire as soon as SRTP
	// is ready, before any video packets arrive (breaks the PLI deadlock).
	videoSSRCs := ExtractVideoSSRCs(payload.SDP)
	for _, ssrc := range videoSSRCs {
		session.trackVideoSSRC(ssrc)
	}
	if len(videoSSRCs) > 0 {
		e.logf("[raw][%s] extracted %d video SSRC(s) from local %s SDP for proactive PLI: %v",
			bridgeID, len(videoSSRCs), sdpType, videoSSRCs)
	}

	// Mark ICE role: offerer = controlling (Dial), answerer = controlled (Accept).
	session.isOfferer = sdpType == "offer"

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		ready, err := session.Init(ctx, sdpType)
		if err != nil {
			e.logf("[raw][%s] Init failed: %v", bridgeID, err)
			e.sendShadowError(conn, bridgeID, "init_failed", err, true)
			return
		}

		if err := writeJSONPacket(conn, "RTC_SHADOW_READY", ready); err != nil {
			e.logf("[raw][%s] send SHADOW_READY failed: %v", bridgeID, err)
		}
		e.logf("[bcr_client] RTC_SHADOW_READY sent bridgeId=%s sdpType=%s", bridgeID, sdpType)

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
		}
	}()
}

// ─── handleShadowRemote ───────────────────────────────────────────────────────

// handleShadowRemote is called when the browser receives and applies a remote
// SDP description (setRemoteDescription). For the offerer path this is the SFU's
// answer — trigger Connect(). For the answerer path this is the SFU's offer — store
// the remote SDP so Init(answer) can call Connect() after it finishes.
func (e *Engine) handleShadowRemote(conn *websocket.Conn, payload RTCShadowRemotePayload) {
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
		e.mergeCodecsAndSSRCs(session, bridgeID, "answer", payload.SDP)
		e.triggerConnect(conn, bridgeID, session, payload.SDP, "offer", gen)

	case "offer":
		// Answerer path: SFU is the oferer. Store the remote offer SDP so that
		// once Init(answer) finishes, Connect() can use it.
		e.logf("[bcr_client] RTC_SHADOW_REMOTE offer bridgeId=%s sdpLen=%d", bridgeID, len(payload.SDP))
		// Extract video SSRCs from the offer so PLI can fire as soon as SRTP is ready.
		e.mergeCodecsAndSSRCs(session, bridgeID, "offer", payload.SDP)
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

func (e *Engine) handleShadowPacket(conn *websocket.Conn, message []byte) bool {
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
	e.shadowMu.Unlock()

	e.logf("[bcr_client][bridge] active bridge switched old=%s new=%s reason=%s", oldActive, bridgeID, reason)

	if oldActive != "" && oldActive != bridgeID {
		e.closeShadowSession(oldActive)
	}
}

func (e *Engine) clearActiveBridgeIfMatch(bridgeID, reason string) {
	e.shadowMu.Lock()
	if e.activeBridgeID != bridgeID {
		e.shadowMu.Unlock()
		return
	}
	e.activeBridgeID = ""
	e.shadowMu.Unlock()

	e.logf("[bcr_client][bridge] active bridge cleared bridgeId=%s reason=%s", bridgeID, reason)
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
func (e *Engine) mergeCodecsAndSSRCs(session *rawShadowSession, bridgeID, sdpType, sdp string) {
	// Merge codec PTs — accept ALL codecs from remote SDPs without filtering.
	// The SFU may assign different PT numbers during renegotiation (e.g. VP8
	// at PT=107 instead of PT=96). We must accept any PT so the relay can
	// identify incoming packets. The extension's SDP pinning constrains what
	// the SFU sends; this map just provides the PT→codec lookup.
	newCodecs := ParsePTCodecMap(sdp)
	session.mu.Lock()
	added := 0
	for pt, info := range newCodecs {
		if _, exists := session.ptCodecMap[pt]; !exists {
			session.ptCodecMap[pt] = info
			added++
		}
	}
	session.mu.Unlock()
	if added > 0 {
		e.logf("[raw][%s] renegotiation %s: merged %d new codec(s) into ptCodecMap (total=%d)",
			bridgeID, sdpType, added, len(session.ptCodecMap))
	}

	// Extract video SSRCs and inject them for proactive PLI
	videoSSRCs := ExtractVideoSSRCs(sdp)
	for _, ssrc := range videoSSRCs {
		session.trackVideoSSRC(ssrc)
	}
	if len(videoSSRCs) > 0 {
		e.logf("[raw][%s] extracted %d video SSRC(s) from %s SDP for proactive PLI: %v",
			bridgeID, len(videoSSRCs), sdpType, videoSSRCs)
	}
}

// ─── Renegotiation Handlers ───────────────────────────────────────────────────

func (e *Engine) handleRenegotiationLocal(conn *websocket.Conn, session *rawShadowSession, bridgeID string, sdpType string, sdp string) {
	e.logf("[raw][%s] SHADOW_LOCAL %s during active session — treating as renegotiation", bridgeID, sdpType)
	
	// Update codecs and SSRCs from the new local SDP
	e.mergeCodecsAndSSRCs(session, bridgeID, sdpType, sdp)

	// Fetch existing transport credentials
	ready, err := session.GetTransportCredentials(sdpType)
	if err != nil {
		e.logf("[raw][%s] failed to get transport credentials for renegotiation: %v", bridgeID, err)
		return
	}

	// Send SHADOW_READY to unblock the frontend's createAnswer/createOffer await
	if err := writeJSONPacket(conn, "RTC_SHADOW_READY", ready); err != nil {
		e.logf("[raw][%s] send SHADOW_READY for renegotiation failed: %v", bridgeID, err)
		return
	}
	e.logf("[raw][%s] [Renegotiation Answer Sent] SHADOW_READY sent bridgeId=%s sdpType=%s", bridgeID, bridgeID, sdpType)
}

func (e *Engine) handleRenegotiationRemote(session *rawShadowSession, bridgeID string, sdpType string, sdp string) {
	e.logf("[raw][%s] [Renegotiation Offer/Answer Received] Parsing remote %s SDP", bridgeID, sdpType)
	
	// Log the exact track mapping and m-lines
	sections := ExtractMediaSections(sdp)
	for _, sec := range sections {
		e.logf("[raw][%s] [Renegotiation Track Mapping] Section: m=%s %s %s", bridgeID, sec.Type, sec.Port, sec.Protocol)
	}

	e.mergeCodecsAndSSRCs(session, bridgeID, sdpType, sdp)
}

// ─── Utility Functions ────────────────────────────────────────────────────────

func (e *Engine) sendShadowError(conn *websocket.Conn, bridgeID, stage string, err error, retryable bool) {
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

func writeJSONPacket(conn *websocket.Conn, packetType string, payload any) error {
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	out, err := json.Marshal(Packet{Type: packetType, Payload: payloadRaw})
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, out)
}

func detectLocalIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			return ip4.String()
		}
	}

	return ""
}

// hashSDP returns a 16-char MD5 hex prefix for dedup/logging.
func hashSDP(sdp string) string {
	h := md5.Sum([]byte(sdp))
	return hex.EncodeToString(h[:])[:16]
}

func truncateHash(hash string) string {
	if hash == "" {
		return "(none)"
	}
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func normalizeSDPType(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

func mapSDPType(t string) string {
	return normalizeSDPType(t)
}

func truncateSDP(sdp string, maxLen int) string {
	if len(sdp) <= maxLen {
		return sdp
	}
	return sdp[:maxLen] + "...(truncated)"
}

func (e *Engine) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if e.cb.OnLog != nil {
		e.cb.OnLog(msg)
		return
	}
	fmt.Println(msg)
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
