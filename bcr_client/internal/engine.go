package engine

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

type shadowSession struct {
	pc                    *webrtc.PeerConnection
	generation            int // increments on PC rebuild; guards stale OnTrack goroutines
	updatedAt             time.Time
	pendingRemoteAnswer   *RTCShadowRemotePayload
	pendingLocalAnswer    *RTCShadowLocalPayload
	iceServers            []IceServer // captured from the browser's RTCPeerConnection config
	lastShadowOfferSDP    string      // shadow's own offer SDP, used for mid comparison when applying answers
	browserOfferSDP       string      // browser's offer SDP, used for aligned shadow PC creation
	lastLocalOfferHash    string      // hash of last accepted local offer; used for dedupe
	lastLocalOfferTime    time.Time   // timestamp of last accepted local offer; cooldown gate
	connectedAt           time.Time   // timestamp when connectionState reached connected; used for no-track watchdog
	iceConnectedAt        time.Time   // timestamp when iceConnectionState reached connected; used for no-track watchdog
	dtlsStartedAt         time.Time   // timestamp when DTLS handshake began (state=new→connecting transition)
	localDTLSFingerprint  string      // local DTLS fingerprint from shadow offer
	remoteDTLSFingerprint string      // remote DTLS fingerprint from Teams answer
}

type Engine struct {
	cfg Config
	cb  Callbacks

	upgrader websocket.Upgrader
	server   *http.Server

	bridgeMu    sync.RWMutex
	controlConn *websocket.Conn
	dataConn    *websocket.Conn

	shadowMu       sync.Mutex
	shadowSessions map[string]*shadowSession
	activeBridgeID string

	relayMu       sync.Mutex
	relaySessions map[string]*relaySession
}

func New(cfg Config, cb Callbacks) *Engine {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8081"
	}

	return &Engine{
		cfg: cfg,
		cb:  cb,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		shadowSessions: make(map[string]*shadowSession),
		relaySessions:  make(map[string]*relaySession),
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

		if e.cb.OnVideoChunk != nil {
			e.cb.OnVideoChunk(message)
		}
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

func (e *Engine) getOrCreateShadowSession(bridgeID string) (*shadowSession, error) {
	e.shadowMu.Lock()
	defer e.shadowMu.Unlock()

	if session, ok := e.shadowSessions[bridgeID]; ok {
		session.updatedAt = time.Now()
		return session, nil
	}

	pc, err := e.newShadowPeerConnection(bridgeID, nil)
	if err != nil {
		return nil, err
	}

	session := &shadowSession{pc: pc, updatedAt: time.Now()}
	e.shadowSessions[bridgeID] = session
	e.registerOnTrack(bridgeID, session) // register BEFORE unlock so OnTrack sees correct gen
	return session, nil
}

func (e *Engine) resetShadowSessionPC(bridgeID string, session *shadowSession) error {
	if session.pc != nil {
		_ = session.pc.Close()
	}

	pc, err := e.newShadowPeerConnection(bridgeID, session.iceServers)
	if err != nil {
		return err
	}

	session.pc = pc
	session.pendingRemoteAnswer = nil
	session.pendingLocalAnswer = nil
	session.updatedAt = time.Now()
	return nil
}

// createAndSetLocalDescription implements the correct Pion Create→Set pipeline.
// isOffer=true  → CreateOffer + SetLocalDescription (browser is the answerer, shadow mirrors as offerer)
// isOffer=false → CreateAnswer + SetLocalDescription (browser is the offerer, shadow creates answer)
// After SetLocalDescription succeeds, credentials are extracted and RTC_SHADOW_READY is sent.
// This function MUST be called instead of ever passing a foreign SDP to SetLocalDescription.
func (e *Engine) createAndSetLocalDescription(
	conn *websocket.Conn, bridgeID string, session *shadowSession, isOffer bool,
) bool {
	var desc webrtc.SessionDescription
	var err error

	// ── TIMING: overall pipeline entry ───────────────────────────────────────
	tPipelineStart := time.Now()
	e.logf("[bcr_client][shadow][timing] createAndSetLocalDescription ENTER isOffer=%v bridgeId=%s", isOffer, bridgeID)

	if isOffer {
		// If we have the browser's offer SDP, build an aligned shadow PC that
		// mirrors the browser's media sections (codec PTs, transceiver count,
		// direction). This ensures Teams' answer is directly compatible.
		if session.browserOfferSDP != "" {
			e.logf("[bcr_client][shadow][stage=create_aligned_pc] bridgeId=%s total_elapsed=%dms", bridgeID, time.Since(tPipelineStart).Milliseconds())
			tAlign := time.Now()
			alignedPC, alignErr := e.createAlignedShadowPC(bridgeID, session.browserOfferSDP, session.iceServers)
			e.logf("[bcr_client][shadow][timing] stage=create_aligned_pc_done bridgeId=%s alignElapsed=%dms total=%dms",
				bridgeID, time.Since(tAlign).Milliseconds(), time.Since(tPipelineStart).Milliseconds())
			if alignErr != nil {
				e.logf("[bcr_client][shadow] createAlignedShadowPC failed bridgeId=%s err=%v — falling back to generic", bridgeID, alignErr)
				// Fall through to generic transceiver setup below.
			} else {
				// Replace the session's PC with the aligned one.
				if session.pc != nil {
					_ = session.pc.Close()
				}
				session.pc = alignedPC
				session.generation++
				e.registerOnTrack(bridgeID, session)
			}
		}

		// Fallback: if no transceivers exist (no browser SDP or align failed),
		// add generic RecvOnly transceivers so the offer has media sections.
		if len(session.pc.GetTransceivers()) == 0 {
			if _, err = session.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionRecvonly,
			}); err != nil {
				return e.handleSDPApplyFailure(conn, bridgeID, session, "add_audio_transceiver", err, false)
			}
			if _, err = session.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionRecvonly,
			}); err != nil {
				return e.handleSDPApplyFailure(conn, bridgeID, session, "add_video_transceiver", err, false)
			}
		}

		e.logf("[bcr_client][shadow][stage=create_offer] bridgeId=%s transceivers=%d total_elapsed=%dms",
			bridgeID, len(session.pc.GetTransceivers()), time.Since(tPipelineStart).Milliseconds())
		tCreateOffer := time.Now()
		desc, err = session.pc.CreateOffer(nil)
		e.logf("[bcr_client][shadow][timing] stage=create_offer_call bridgeId=%s elapsed=%dms total=%dms",
			bridgeID, time.Since(tCreateOffer).Milliseconds(), time.Since(tPipelineStart).Milliseconds())
	} else {
		e.logf("[bcr_client][shadow][stage=create_answer] bridgeId=%s total_elapsed=%dms", bridgeID, time.Since(tPipelineStart).Milliseconds())
		tCreateAnswer := time.Now()
		desc, err = session.pc.CreateAnswer(nil)
		e.logf("[bcr_client][shadow][timing] stage=create_answer_call bridgeId=%s elapsed=%dms total=%dms",
			bridgeID, time.Since(tCreateAnswer).Milliseconds(), time.Since(tPipelineStart).Milliseconds())
	}
	if err != nil {
		return e.handleSDPApplyFailure(conn, bridgeID, session, "create_sdp", err, false)
	}

	if isOffer {
		e.logf("[bcr_client][shadow][stage=create_offer_ok] bridgeId=%s sdpLen=%d total_elapsed=%dms",
			bridgeID, len(desc.SDP), time.Since(tPipelineStart).Milliseconds())
		// Store shadow's own offer SDP for mid comparison when answer arrives.
		session.lastShadowOfferSDP = desc.SDP
	} else {
		e.logf("[bcr_client][shadow][stage=create_answer_ok] bridgeId=%s sdpLen=%d total_elapsed=%dms",
			bridgeID, len(desc.SDP), time.Since(tPipelineStart).Milliseconds())
	}

	tSetLocal := time.Now()
	if err = session.pc.SetLocalDescription(desc); err != nil {
		return e.handleSDPApplyFailure(conn, bridgeID, session, "set_local_desc", err, false)
	}
	e.logf("[bcr_client][shadow][timing] stage=set_local_desc_call bridgeId=%s elapsed=%dms total=%dms",
		bridgeID, time.Since(tSetLocal).Milliseconds(), time.Since(tPipelineStart).Milliseconds())

	session.updatedAt = time.Now()
	e.logf("[bcr_client][shadow][stage=set_local_desc_ok] bridgeId=%s signalingState=%s total_elapsed=%dms",
		bridgeID, session.pc.SignalingState().String(), time.Since(tPipelineStart).Milliseconds())

	// Wait for ICE gathering to complete before sending SHADOW_READY.
	// With TURN servers, gathering takes longer than host-only.
	// The 2.5 s timeout is a safety net.
	e.logf("[bcr_client][shadow][stage=ice_gathering] bridgeId=%s total_elapsed=%dms", bridgeID, time.Since(tPipelineStart).Milliseconds())
	tGather := time.Now()
	gatherDone := webrtc.GatheringCompletePromise(session.pc)
	gatherCtx, gatherCancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer gatherCancel()
	select {
	case <-gatherDone:
		e.logf("[bcr_client][shadow][stage=ice_gathered] bridgeId=%s gatherElapsed=%dms total=%dms",
			bridgeID, time.Since(tGather).Milliseconds(), time.Since(tPipelineStart).Milliseconds())
	case <-gatherCtx.Done():
		e.logf("[bcr_client][shadow][stage=ice_gather_timeout] bridgeId=%s gatherElapsed=%dms total=%dms (proceeding)",
			bridgeID, time.Since(tGather).Milliseconds(), time.Since(tPipelineStart).Milliseconds())
	}

	// Derive the SDP type tag for SHADOW_READY so the browser can distinguish
	// offer-generated from answer-generated READY responses.
	readySdpType := "answer"
	if isOffer {
		readySdpType = "offer"
	}
	tSendReady := time.Now()
	if err = e.sendShadowReady(conn, bridgeID, session.pc, readySdpType); err != nil {
		e.logf("[bcr_client][shadow] sendShadowReady failed bridgeId=%s err=%v", bridgeID, err)
		e.sendShadowError(conn, bridgeID, "send_ready", err, true)
		return false
	}
	e.logf("[bcr_client][shadow][timing] stage=send_ready_done bridgeId=%s elapsed=%dms total=%dms",
		bridgeID, time.Since(tSendReady).Milliseconds(), time.Since(tPipelineStart).Milliseconds())

	e.logf("[bcr_client][shadow][timing] createAndSetLocalDescription EXIT bridgeId=%s total=%dms",
		bridgeID, time.Since(tPipelineStart).Milliseconds())
	e.logf("[bcr_client][shadow][stage=ready_sent] bridgeId=%s", bridgeID)
	return true
}

func (e *Engine) iceServersToWebRTC(servers []IceServer) []webrtc.ICEServer {
	if len(servers) == 0 {
		return nil
	}
	out := make([]webrtc.ICEServer, 0, len(servers))
	for _, s := range servers {
		if len(s.URLs) == 0 {
			continue
		}
		ice := webrtc.ICEServer{URLs: s.URLs}
		if s.Username != "" {
			ice.Username = s.Username
		}
		if s.Credential != "" {
			ice.Credential = s.Credential
			ice.CredentialType = webrtc.ICECredentialTypePassword
		}
		// Log TURN URL + truncated username for TURN credential verification
		userPreview := s.Username
		if len(userPreview) > 20 {
			userPreview = userPreview[:20] + "..."
		}
		e.logf("[bcr_client][shadow] Applying ICE Server: %v username=%q hasCred=%v",
			s.URLs, userPreview, s.Credential != "")
		out = append(out, ice)
	}
	return out
}

func (e *Engine) newShadowPeerConnection(bridgeID string, servers []IceServer) (*webrtc.PeerConnection, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register default codecs: %w", err)
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return nil, fmt.Errorf("register default interceptors: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	)

	cfg := webrtc.Configuration{ICEServers: e.iceServersToWebRTC(servers)}
	pc, err := api.NewPeerConnection(cfg)
	if err == nil {
		e.attachPeerStateLogging(bridgeID, pc)
	}
	return pc, err
}

// newShadowPeerConnectionFromOffer creates a shadow PC whose MediaEngine is
// seeded with codecs extracted from the browser's offer SDP on top of Pion's
// built-in defaults. This ensures Pion can negotiate H.264 profile variants,
// VP9 profiles, and any other VDI-specific codec that falls outside the defaults.
// Each registration is best-effort: errors (e.g. duplicate PT) are silently ignored
// because Pion already registered a compatible entry via RegisterDefaultCodecs.
func (e *Engine) newShadowPeerConnectionFromOffer(bridgeID string, offerSDP string, servers []IceServer) (*webrtc.PeerConnection, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register default codecs: %w", err)
	}

	for _, entry := range parseSDPCodecs(offerSDP) {
		_ = mediaEngine.RegisterCodec(entry.params, entry.codecType)
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return nil, fmt.Errorf("register default interceptors: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	)

	cfg := webrtc.Configuration{ICEServers: e.iceServersToWebRTC(servers)}
	pc, err := api.NewPeerConnection(cfg)
	if err == nil {
		e.attachPeerStateLogging(bridgeID, pc)
	}
	return pc, err
}

// createAlignedShadowPC builds a shadow PeerConnection whose MediaEngine and
// transceiver layout exactly mirror the browser's offer SDP. This ensures the
// shadow's own offer (produced by CreateOffer) is structurally compatible with
// Teams' answer (which is based on the browser's offer sent via signaling).
//
// Key differences from newShadowPeerConnectionFromOffer:
//   - Registers ONLY codecs from the browser's offer (no RegisterDefaultCodecs).
//     This preserves exact payload type numbers so they match Teams' answer.
//   - Adds transceivers matching each media m= section (same kind + direction).
//   - Creates a dummy DataChannel for each m=application section so the BUNDLE
//     group includes the data channel mid.
//
// TIMING DIAGNOSTIC: Every major stage is bracketed with time.Now() / time.Since()
// so that the 29-second hang can be attributed to a specific sub-operation.
func (e *Engine) createAlignedShadowPC(bridgeID string, browserOfferSDP string, servers []IceServer) (*webrtc.PeerConnection, error) {
	// ── TIMING: overall entry ────────────────────────────────────────────────
	tAlignedStart := time.Now()
	e.logf("[bcr_client][shadow][timing] createAlignedShadowPC ENTER bridgeId=%s t=0ms", bridgeID)

	// ── Stage 1: Parse codecs from browser SDP ───────────────────────────────
	t1 := time.Now()
	// Parse codecs with strict PT + rtcp-fb preservation.
	strictCodecs := ParseSDPCodecsStrict(browserOfferSDP)
	e.logf("[bcr_client][shadow][timing] stage=parse_codecs bridgeId=%s codecs=%d elapsed=%dms total=%dms",
		bridgeID, len(strictCodecs), time.Since(t1).Milliseconds(), time.Since(tAlignedStart).Milliseconds())

	// ── Stage 2: Build MediaEngine ───────────────────────────────────────────
	t2 := time.Now()
	mediaEngine := &webrtc.MediaEngine{}

	if len(strictCodecs) == 0 {
		// Safety fallback: if parsing produced nothing, use defaults.
		e.logf("[bcr_client][shadow] createAlignedShadowPC: no codecs parsed, falling back to defaults bridgeId=%s", bridgeID)
		if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
			return nil, fmt.Errorf("register default codecs: %w", err)
		}
	} else {
		for _, entry := range strictCodecs {
			if err := mediaEngine.RegisterCodec(entry.params, entry.codecType); err != nil {
				// Log but continue — some codecs may already be registered or conflict.
				e.logf("[bcr_client][shadow] createAlignedShadowPC: codec register skip mime=%s pt=%d err=%v",
					entry.params.MimeType, entry.params.PayloadType, err)
			}
		}
		e.logf("[bcr_client][shadow] createAlignedShadowPC: registered %d codecs from browser offer bridgeId=%s",
			len(strictCodecs), bridgeID)
	}
	e.logf("[bcr_client][shadow][timing] stage=build_media_engine bridgeId=%s elapsed=%dms total=%dms",
		bridgeID, time.Since(t2).Milliseconds(), time.Since(tAlignedStart).Milliseconds())

	// ── Stage 3: Register default interceptors ───────────────────────────────
	t3 := time.Now()
	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return nil, fmt.Errorf("register default interceptors: %w", err)
	}
	e.logf("[bcr_client][shadow][timing] stage=register_interceptors bridgeId=%s elapsed=%dms total=%dms",
		bridgeID, time.Since(t3).Milliseconds(), time.Since(tAlignedStart).Milliseconds())

	// ── Stage 4: Create WebRTC API ───────────────────────────────────────────
	t4 := time.Now()
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	)
	e.logf("[bcr_client][shadow][timing] stage=new_api bridgeId=%s elapsed=%dms total=%dms",
		bridgeID, time.Since(t4).Milliseconds(), time.Since(tAlignedStart).Milliseconds())

	// ── Stage 5: Create PeerConnection ──────────────────────────────────────
	t5 := time.Now()
	cfg := webrtc.Configuration{ICEServers: e.iceServersToWebRTC(servers)}
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}
	e.logf("[bcr_client][shadow][timing] stage=new_peer_connection bridgeId=%s elapsed=%dms total=%dms",
		bridgeID, time.Since(t5).Milliseconds(), time.Since(tAlignedStart).Milliseconds())

	e.attachPeerStateLogging(bridgeID, pc)

	// ── Stage 6: Parse browser m= sections ──────────────────────────────────
	t6 := time.Now()
	sections := ParseOfferMediaSections(browserOfferSDP)
	e.logf("[bcr_client][shadow][timing] stage=parse_media_sections bridgeId=%s sections=%d elapsed=%dms total=%dms",
		bridgeID, len(sections), time.Since(t6).Milliseconds(), time.Since(tAlignedStart).Milliseconds())

	// ── Stage 7: Add transceivers / data channels ────────────────────────────
	t7 := time.Now()
	for i, sec := range sections {
		tSection := time.Now()
		switch sec.Kind {
		case "audio", "video":
			codecType := webrtc.RTPCodecTypeAudio
			if sec.Kind == "video" {
				codecType = webrtc.RTPCodecTypeVideo
			}

			dir := parseRTPDirection(sec.Direction)

			if _, tErr := pc.AddTransceiverFromKind(codecType, webrtc.RTPTransceiverInit{
				Direction: dir,
			}); tErr != nil {
				e.logf("[bcr_client][shadow] createAlignedShadowPC: transceiver add failed section=%d kind=%s dir=%s err=%v",
					i, sec.Kind, sec.Direction, tErr)
				_ = pc.Close()
				return nil, fmt.Errorf("add transceiver section %d: %w", i, tErr)
			}

		case "application":
			// Create a dummy data channel so the offer includes an m=application
			// section matching the browser's offer. This keeps the BUNDLE group
			// and mid assignment aligned.
			if _, dcErr := pc.CreateDataChannel("bcr-dummy", nil); dcErr != nil {
				e.logf("[bcr_client][shadow] createAlignedShadowPC: data channel failed section=%d err=%v", i, dcErr)
				// Non-fatal: proceed without data channel.
			}
		}
		e.logf("[bcr_client][shadow][timing] stage=add_section section=%d kind=%s bridgeId=%s elapsed=%dms",
			i, sec.Kind, bridgeID, time.Since(tSection).Milliseconds())
	}
	e.logf("[bcr_client][shadow][timing] stage=add_transceivers_done bridgeId=%s elapsed=%dms total=%dms",
		bridgeID, time.Since(t7).Milliseconds(), time.Since(tAlignedStart).Milliseconds())

	// ── TIMING: overall exit ─────────────────────────────────────────────────
	e.logf("[bcr_client][shadow][timing] createAlignedShadowPC EXIT bridgeId=%s total=%dms",
		bridgeID, time.Since(tAlignedStart).Milliseconds())

	return pc, nil
}

// parseRTPDirection overrides the SDP direction string for Pion's RTPTransceiver.
// Since the shadow PC simply consumes media locally and Teams handles sending,
// and because Pion strictly requires AddTransceiverFromKind to use recvonly
// (throwing errors on inactive), we force all simulated transceivers to recvonly.
func parseRTPDirection(dir string) webrtc.RTPTransceiverDirection {
	return webrtc.RTPTransceiverDirectionRecvonly
}

// attachPeerStateLogging hooks state-change callbacks onto the shadow PC for diagnostics.
// It also wires the DTLS transport state listener to track the new→connecting transition,
// which is the definitive signal that the DTLS handshake has begun.
func (e *Engine) attachPeerStateLogging(bridgeID string, pc *webrtc.PeerConnection) {
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		e.shadowMu.Lock()
		if session, ok := e.shadowSessions[bridgeID]; ok {
			// Log with DTLS state info for context
			dtlsState := "unknown"
			if session.dtlsStartedAt.IsZero() {
				dtlsState = "not_started"
			} else {
				dtlsAge := time.Since(session.dtlsStartedAt).Milliseconds()
				dtlsState = fmt.Sprintf("running_%dms", dtlsAge)
			}
			e.logf("[bcr_client][shadow] connectionState=%s bridgeId=%s dtls=%s localFp=%s remoteFp=%s",
				state.String(), bridgeID, dtlsState,
				truncateHash(session.localDTLSFingerprint),
				truncateHash(session.remoteDTLSFingerprint))
		}
		e.shadowMu.Unlock()

		switch state {
		case webrtc.PeerConnectionStateConnected:
			e.promoteActiveBridge(bridgeID, "pc_connected")
			e.shadowMu.Lock()
			if session, ok := e.shadowSessions[bridgeID]; ok {
				now := time.Now()
				if session.connectedAt.IsZero() {
					session.connectedAt = now
				}
			}
			e.shadowMu.Unlock()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			e.clearActiveBridgeIfMatch(bridgeID, "pc_"+state.String())
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		e.logf("[bcr_client][shadow] iceConnectionState=%s bridgeId=%s", state.String(), bridgeID)
		switch state {
		case webrtc.ICEConnectionStateConnected:
			e.promoteActiveBridge(bridgeID, "ice_connected")
			e.shadowMu.Lock()
			if session, ok := e.shadowSessions[bridgeID]; ok {
				now := time.Now()
				if session.iceConnectedAt.IsZero() {
					session.iceConnectedAt = now
					// Start a 3-second watchdog: if OnTrack never fires, emit diagnostic log.
					go e.watchForNoTrack(bridgeID, now)
				}
			}
			e.shadowMu.Unlock()

			// ── ICE-CONNECTED DIAGNOSTIC ──────────────────────────────────────
			// Dump the full state at the moment of connection so we can see
			// exactly whether DTLS starts and what the a=setup role is.
			localDesc := pc.LocalDescription()
			setupLine := "(no local desc)"
			if localDesc != nil {
				for _, line := range strings.Split(localDesc.SDP, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "a=setup:") {
						setupLine = strings.TrimSpace(line)
						break
					}
				}
			}
			e.logf("[bcr_client][shadow][diag] ICE_CONNECTED bridgeId=%s connState=%s sigState=%s setup=%s transceivers=%d",
				bridgeID, pc.ConnectionState().String(), pc.SignalingState().String(),
				setupLine, len(pc.GetTransceivers()))

			// Poll at 500ms, 2s, and 5s to track DTLS progress
			go func(bid string, p *webrtc.PeerConnection) {
				for _, delay := range []time.Duration{500 * time.Millisecond, 2 * time.Second, 5 * time.Second} {
					time.Sleep(delay)
					cs := p.ConnectionState()
					e.logf("[bcr_client][shadow][diag] DTLS_POLL bridgeId=%s connState=%s delay=%s",
						bid, cs.String(), delay)
					if cs == webrtc.PeerConnectionStateConnected ||
						cs == webrtc.PeerConnectionStateClosed ||
						cs == webrtc.PeerConnectionStateFailed {
						return
					}
				}
			}(bridgeID, pc)
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
			e.clearActiveBridgeIfMatch(bridgeID, "ice_"+state.String())
		}
	})

	// ── DTLS State Tracking ────────────────────────────────────────────────────
	// Pion exposes DTLS transport state through the SCTP transport or directly
	// via each transceiver's sender/receiver DTLSTransport. We hook the
	// PeerConnection-level OnSignalingStateChange as a fallback probe, but the
	// definitive hook is via OnConnectionStateChange correlated with dtlsStartedAt.
	//
	// The most reliable cross-version approach: poll the DTLS transport state
	// immediately after ICE reaches "connected" in a short goroutine, or register
	// a per-transceiver handler. We do both here.
	//
	// Pion v4 exposes pc.SCTP()?.Transport()?.State() for the data channel DTLS
	// path. For media transceivers, we inspect sender/receiver after ICE connects.
	pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		e.logf("[bcr_client][shadow][dtls] signalingState=%s bridgeId=%s", state.String(), bridgeID)
	})

	// Wire up a goroutine that watches the actual DTLS transport objects once ICE
	// is connected, and logs precisely when each transport moves new→connecting.
	// This fires once per PC and self-terminates once DTLS is confirmed started.
	go e.watchDTLSTransportStart(bridgeID, pc)
}

// watchDTLSTransportStart polls the shadow PC's DTLS transport states until it
// observes the new→connecting transition or the PC closes. It writes the
// dtlsStartedAt timestamp on the session so other callbacks can calculate DTLS age.
//
// Polling is used because Pion v4 does not expose an OnDTLSTransportStateChange
// callback at the PeerConnection level; the DTLSTransport object itself has a
// state getter but no listener registration API in the public pion/webrtc v4 surface.
func (e *Engine) watchDTLSTransportStart(bridgeID string, pc *webrtc.PeerConnection) {
	const (
		pollInterval  = 50 * time.Millisecond
		giveUpTimeout = 90 * time.Second // well beyond any realistic handshake window
	)

	deadline := time.Now().Add(giveUpTimeout)
	var dtlsStartLogged bool

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		// Guard: stop if the PC has been closed or replaced.
		pcState := pc.ConnectionState()
		if pcState == webrtc.PeerConnectionStateClosed {
			return
		}

		// Walk all transceivers and check both sender and receiver DTLS transports.
		for _, t := range pc.GetTransceivers() {
			if sender := t.Sender(); sender != nil {
				if dt := sender.Transport(); dt != nil {
					state := dt.State()
					if !dtlsStartLogged && state == webrtc.DTLSTransportStateConnecting {
						dtlsStartLogged = true
						now := time.Now()
						e.logf("[bcr_client][shadow][dtls] DTLS state=connecting (new→connecting) bridgeId=%s at=%s",
							bridgeID, now.Format(time.RFC3339Nano))
						e.shadowMu.Lock()
						if session, ok := e.shadowSessions[bridgeID]; ok && session.dtlsStartedAt.IsZero() {
							session.dtlsStartedAt = now
						}
						e.shadowMu.Unlock()
					}
					if state == webrtc.DTLSTransportStateConnected {
						e.logf("[bcr_client][shadow][dtls] DTLS state=connected bridgeId=%s", bridgeID)
						return // handshake complete; nothing more to watch
					}
					if state == webrtc.DTLSTransportStateFailed || state == webrtc.DTLSTransportStateClosed {
						e.logf("[bcr_client][shadow][dtls] DTLS state=%s bridgeId=%s — handshake failed",
							state.String(), bridgeID)
						return
					}
				}
			}
		}
	}

	e.logf("[bcr_client][shadow][dtls] watchDTLSTransportStart gave up after %s bridgeId=%s dtlsStarted=%v",
		giveUpTimeout, bridgeID, dtlsStartLogged)
}

// watchForNoTrack is a 3-second watchdog that fires if a bridge reaches ice_connected
// but never delivers an OnTrack callback. This indicates either media is disabled,
// sender is gating tracks, or the relay pathway has a break.
func (e *Engine) watchForNoTrack(bridgeID string, iceConnectedTime time.Time) {
	time.Sleep(3 * time.Second)

	e.shadowMu.Lock()
	session, ok := e.shadowSessions[bridgeID]
	e.shadowMu.Unlock()

	if !ok || session.pc == nil {
		return
	}

	// Check if any tracks have been delivered via OnTrack.
	// We infer this by checking the relay session state.
	e.relayMu.Lock()
	relay, relayOk := e.relaySessions[bridgeID]
	e.relayMu.Unlock()

	if relayOk && relay != nil && len(relay.localTracks) > 0 {
		// Tracks are arriving; watchdog is satisfied.
		return
	}

	// No tracks after 3 seconds of ice_connected:
	// Log diagnostic with current PC state for debugging media-plane issues.
	e.logf("[bcr_client][relay] connected_no_track diagnostic bridgeId=%s iceConnState=%s connState=%s age=%dms relayTracks=%d",
		bridgeID,
		session.pc.ICEConnectionState().String(),
		session.pc.ConnectionState().String(),
		time.Since(iceConnectedTime)/time.Millisecond,
		0) // No relay tracks present
}

// promoteActiveBridge elects the connected bridge as active and tears down the
// previous active bridge to avoid stale dual-bridge media pipelines.
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

// clearActiveBridgeIfMatch clears the active marker when the current active
// bridge has irrecoverably failed or closed.
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

// shouldProcessBridgeTrack returns true when media for bridgeID should be
// forwarded to the relay. The first observed media claims the active bridge
// when no active bridge has been elected yet.
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

func (e *Engine) handleSDPApplyFailure(conn *websocket.Conn, bridgeID string, session *shadowSession, stage string, err error, retryable bool) bool {
	e.logf("[bcr_client][shadow] SDP apply failure bridgeId=%s stage=%s err=%v", bridgeID, stage, err)

	if resetErr := e.resetShadowSessionPC(bridgeID, session); resetErr != nil {
		e.logf("[bcr_client][shadow] pc reset failed bridgeId=%s stage=%s err=%v", bridgeID, stage, resetErr)
		e.sendShadowError(conn, bridgeID, stage, fmt.Errorf("%v; reset_failed=%v", err, resetErr), true)
		return false
	}

	e.logf("[bcr_client][shadow] pc reset after failure bridgeId=%s stage=%s", bridgeID, stage)
	e.sendShadowError(conn, bridgeID, stage, err, retryable)
	return false
}

func (e *Engine) requestFreshOffer(conn *websocket.Conn, bridgeID string, session *shadowSession, reason string) bool {
	e.logf("[bcr_client][shadow] request fresh offer bridgeId=%s reason=%s", bridgeID, reason)

	if resetErr := e.resetShadowSessionPC(bridgeID, session); resetErr != nil {
		e.logf("[bcr_client][shadow] pc reset failed while requesting fresh offer bridgeId=%s err=%v", bridgeID, resetErr)
		e.sendShadowError(conn, bridgeID, "request_fresh_offer", fmt.Errorf("%s; reset_failed=%v", reason, resetErr), true)
		return false
	}

	e.sendShadowError(conn, bridgeID, "request_fresh_offer", errors.New(reason), true)
	return false
}

func (e *Engine) attemptImplicitRollback(session *shadowSession, bridgeID string) bool {
	state := session.pc.SignalingState()

	var err error
	switch state {
	case webrtc.SignalingStateHaveLocalOffer, webrtc.SignalingStateHaveLocalPranswer:
		err = session.pc.SetLocalDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeRollback})
	case webrtc.SignalingStateHaveRemoteOffer, webrtc.SignalingStateHaveRemotePranswer:
		err = session.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeRollback})
	default:
		return false
	}

	if err != nil {
		e.logf("[bcr_client][shadow] implicit rollback failed bridgeId=%s state=%s err=%v", bridgeID, state.String(), err)
		return false
	}

	session.pendingRemoteAnswer = nil
	session.pendingLocalAnswer = nil
	e.logf("[bcr_client][shadow] implicit rollback applied bridgeId=%s fromState=%s", bridgeID, state.String())
	return true
}

func (e *Engine) closeShadowSession(bridgeID string) {
	e.shadowMu.Lock()
	session, ok := e.shadowSessions[bridgeID]
	if ok {
		delete(e.shadowSessions, bridgeID)
	}
	if e.activeBridgeID == bridgeID {
		e.activeBridgeID = ""
	}
	e.shadowMu.Unlock()

	if ok && session.pc != nil {
		_ = session.pc.Close()
	}

	// Tear down the relay session as well — no more media from this bridge.
	e.closeRelaySession(bridgeID)
}

// registerOnTrack attaches the shadow PC's OnTrack callback, capturing the
// current session.generation so stale goroutines can self-terminate after a
// PC rebuild.
func (e *Engine) registerOnTrack(bridgeID string, session *shadowSession) {
	gen := session.generation
	session.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		e.onShadowTrack(bridgeID, gen, track)
	})
}

func mapSDPType(t string) webrtc.SDPType {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "offer":
		return webrtc.SDPTypeOffer
	case "answer":
		return webrtc.SDPTypeAnswer
	case "pranswer":
		return webrtc.SDPTypePranswer
	case "rollback":
		return webrtc.SDPTypeRollback
	default:
		return webrtc.SDPTypeOffer
	}
}

func normalizeSDPType(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

// hashSDP returns a short hash of the SDP string for dedup detection.
// Uses MD5 for speed; truncated to 8 chars for log readability.
func hashSDP(sdp string) string {
	h := md5.Sum([]byte(sdp))
	return hex.EncodeToString(h[:])[:16]
}

// truncateHash returns first 12 chars of hash for log readability, or "" if empty.
func truncateHash(hash string) string {
	if hash == "" {
		return "(none)"
	}
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
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

func (e *Engine) sendShadowReady(conn *websocket.Conn, bridgeID string, pc *webrtc.PeerConnection, sdpType string) error {
	localDesc := pc.LocalDescription()
	if localDesc == nil {
		return errors.New("local description is nil")
	}

	ufrag, pwd, fingerprint := ExtractShadowCredentials(localDesc.SDP)
	if ufrag == "" || pwd == "" || fingerprint == "" {
		e.logf("[bcr_client][shadow] credential extraction failed bridgeId=%s sdpLen=%d ufrag=%q pwd=%q fp=%q sdpPreview=%q",
			bridgeID, len(localDesc.SDP), ufrag, pwd, fingerprint, truncateSDP(localDesc.SDP, 500))
		return errors.New("failed to extract shadow credentials")
	}

	// Capture local DTLS fingerprint for later diagnostics
	e.shadowMu.Lock()
	if session, ok := e.shadowSessions[bridgeID]; ok {
		session.localDTLSFingerprint = fingerprint
	}
	e.shadowMu.Unlock()

	candidates := ExtractShadowCandidates(localDesc.SDP)
	// ── DIAGNOSTIC: classify candidate types so we can verify TURN allocation ──
	hostCount, srflxCount, relayCount := 0, 0, 0
	for _, c := range candidates {
		switch {
		case strings.Contains(c, " relay "):
			relayCount++
		case strings.Contains(c, " srflx "):
			srflxCount++
		case strings.Contains(c, " host "):
			hostCount++
		}
	}
	e.logf("[bcr_client][shadow] gathered %d candidate(s) bridgeId=%s host=%d srflx=%d relay=%d",
		len(candidates), bridgeID, hostCount, srflxCount, relayCount)

	resp := RTCShadowReadyPayload{
		BridgeID:        bridgeID,
		SDPType:         sdpType,
		ICEUfrag:        ufrag,
		ICEPwd:          pwd,
		DTLSFingerprint: fingerprint,
		LocalIP:         detectLocalIPv4(),
		Candidates:      candidates,
		GeneratedAt:     time.Now().UnixMilli(),
		ExpiresAt:       time.Now().Add(60 * time.Second).UnixMilli(),
	}

	if err := writeJSONPacket(conn, "RTC_SHADOW_READY", resp); err != nil {
		return err
	}

	e.logf("[bcr_client] RTC_SHADOW_READY bridgeId=%s sdpType=%s", bridgeID, sdpType)
	return nil
}

func (e *Engine) applyRemoteDescription(conn *websocket.Conn, bridgeID string, session *shadowSession, payload RTCShadowRemotePayload) bool {
	sdpType := normalizeSDPType(payload.SDPType)
	state := session.pc.SignalingState()

	e.logf("[bcr_client][shadow] remote sdpType=%s bridgeId=%s signalingState=%s", sdpType, bridgeID, state.String())

	switch sdpType {
	case "offer":
		// Step 0: Normalize the browser SDP before any Pion call.
		// Legacy VDI browsers omit a=mid; Pion hard-fails without it.
		normalizedSDP := normalizeSDP(payload.SDP)

		// Step 1: Rebuild the shadow PC with a MediaEngine seeded from THIS offer's
		// codecs. This replaces any old PC (wrong codec set, stale state) so that
		// CreateAnswer always finds matching codecs and never produces an empty SDP.
		// Rebuild when the PC is new, gone, or dead. connState=new means the session
		// was just created with default codecs and MUST be rebuilt with offer codecs.
		needsRebuild := session.pc == nil ||
			session.pc.ConnectionState() == webrtc.PeerConnectionStateClosed ||
			session.pc.ConnectionState() == webrtc.PeerConnectionStateFailed ||
			session.pc.ConnectionState() == webrtc.PeerConnectionStateNew

		if needsRebuild {
			e.logf("[bcr_client][shadow][stage=rebuild_pc] bridgeId=%s connState=%s", bridgeID, session.pc.ConnectionState().String())
			if session.pc != nil {
				_ = session.pc.Close()
			}
			newPC, pcErr := e.newShadowPeerConnectionFromOffer(bridgeID, normalizedSDP, session.iceServers)
			if pcErr != nil {
				e.logf("[bcr_client][shadow] rebuild pc failed bridgeId=%s err=%v", bridgeID, pcErr)
				e.sendShadowError(conn, bridgeID, "rebuild_pc", pcErr, true)
				return false
			}
			session.pc = newPC
			session.generation++
			session.pendingRemoteAnswer = nil
			session.pendingLocalAnswer = nil
			e.registerOnTrack(bridgeID, session) // fresh OnTrack for this generation
		} else {
			// ── MVP GUARD: Don't renegotiate while DTLS handshake is pending ──
			// If ICE hasn't connected (and thus DTLS hasn't started), applying a
			// renegotiation offer to Pion can disrupt the DTLS transport setup,
			// causing the handshake to never initiate. We skip this renegotiation
			// and let the browser handle it natively (fail-open). Teams always
			// sends follow-up renegotiations which will succeed after DTLS completes.
			if session.iceConnectedAt.IsZero() {
				e.logf("[bcr_client][shadow] skipping renegotiation (ICE/DTLS setup in progress) bridgeId=%s connState=%s",
					bridgeID, session.pc.ConnectionState().String())
				e.sendShadowError(conn, bridgeID, "renegotiate_deferred",
					errors.New("ICE not connected yet, deferring renegotiation"), true)
				return true
			}
			e.logf("[bcr_client][shadow][stage=renegotiate_in_place] bridgeId=%s connState=%s",
				bridgeID, session.pc.ConnectionState().String())
		}

		if err := session.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: normalizedSDP}); err != nil {
			return e.handleSDPApplyFailure(conn, bridgeID, session, "set_remote_offer", err, false)
		}

		session.updatedAt = time.Now()
		e.logf("[bcr_client][shadow][stage=remote_offer_applied] bridgeId=%s signalingState=%s",
			bridgeID, session.pc.SignalingState().String())

		// Create a Pion-generated answer and set it as local description.
		// This is the ONLY correct way to call SetLocalDescription — Pion
		// must be the one that generates the SDP it then commits.
		// Credentials are extracted and RTC_SHADOW_READY is sent inside.
		return e.createAndSetLocalDescription(conn, bridgeID, session, false /*answer*/)

	case "answer":
		if state == webrtc.SignalingStateStable {
			return e.requestFreshOffer(conn, bridgeID, session, "remote answer received while stable; need fresh offer")
		}

		if state != webrtc.SignalingStateHaveLocalOffer {
			session.pendingRemoteAnswer = &payload
			session.updatedAt = time.Now()
			e.logf("[bcr_client][shadow] queued remote answer bridgeId=%s state=%s waiting_for=local_offer", bridgeID, state.String())
			return true
		}

		// Translate the answer's mid references to match the shadow's offer mids.
		// This handles the case where browser assigns different mids than Pion.
		answerSDP := payload.SDP
		if session.lastShadowOfferSDP != "" {
			translated := TranslateAnswerMids(answerSDP, session.lastShadowOfferSDP)
			if translated != answerSDP {
				e.logf("[bcr_client][shadow] answer mids translated bridgeId=%s", bridgeID)
				answerSDP = translated
			}

			// Teams SFU aggressively changes payload types entirely for stuff like RTX
			// and injects them in renegotiation Answers. To prevent Pion from dropping
			// the call on "payload type not found", rigorously strip unadvertised ghost codecs.
			scrubbed := ScrubGhostPayloadTypes(answerSDP, session.lastShadowOfferSDP)
			if scrubbed != answerSDP {
				e.logf("[bcr_client][shadow] answer scrubbed unregistered payload types bridgeId=%s", bridgeID)
				answerSDP = scrubbed
			}
		}

		// Extract remote DTLS fingerprint for diagnostics
		_, _, remoteFp := ExtractShadowCredentials(answerSDP)
		session.remoteDTLSFingerprint = remoteFp
		e.logf("[bcr_client][shadow] answer DTLS fingerprint bridgeId=%s local=%s remote=%s",
			bridgeID, truncateHash(session.localDTLSFingerprint), truncateHash(remoteFp))

		// ── DTLS ROLE DIAGNOSTIC: Extract a=setup: from the answer ──────────
		// This determines who initiates the DTLS handshake:
		//   active  → remote (Teams) sends ClientHello, we wait (passive)
		//   passive → we send ClientHello, remote waits
		//   actpass → ambiguous (shouldn't appear in answer per RFC 8842)
		//   missing → DTLS deadlock (neither side initiates)
		remoteSetup := "(missing)"
		remoteCandCount := 0
		for _, line := range strings.Split(answerSDP, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "a=setup:") {
				remoteSetup = trimmed
			}
			if strings.HasPrefix(trimmed, "a=candidate:") {
				remoteCandCount++
			}
		}
		e.logf("[bcr_client][shadow][diag] ANSWER_SETUP bridgeId=%s remoteSetup=%s remoteCandidates=%d answerLen=%d",
			bridgeID, remoteSetup, remoteCandCount, len(answerSDP))

		if err := session.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
			e.logf("[bcr_client][shadow] SetRemoteDescription(answer) failed bridgeId=%s err=%v", bridgeID, err)
			return e.handleSDPApplyFailure(conn, bridgeID, session, "set_remote_answer", err, false)
		}

		session.updatedAt = time.Now()
		e.logf("[bcr_client][shadow] remote answer applied bridgeId=%s state=%s", bridgeID, session.pc.SignalingState().String())
		return true

	default:
		err := fmt.Errorf("unsupported remote sdpType=%q", payload.SDPType)
		e.sendShadowError(conn, bridgeID, "invalid_remote_sdp_type", err, false)
		return false
	}
}

func (e *Engine) applyLocalDescription(conn *websocket.Conn, bridgeID string, session *shadowSession, payload RTCShadowLocalPayload) bool {
	sdpType := normalizeSDPType(payload.SDPType)
	state := session.pc.SignalingState()

	e.logf("[bcr_client][shadow] local sdpType=%s bridgeId=%s signalingState=%s", sdpType, bridgeID, state.String())

	switch sdpType {
	case "offer":
		// The browser is acting as the offerer: it called setLocalDescription(offer).
		// The shadow PC must generate its OWN offer via CreateOffer so that Pion
		// owns the SDP it will commit with SetLocalDescription.
		// We do NOT use the intercepted browser SDP as input to SetLocalDescription.
		// However, we DO use it to create an aligned shadow PC with matching codecs,
		// transceivers, and directions so Teams' answer is directly compatible.

		// Deduplicate same-bridge local offers: check SDP hash + cooldown.
		// If the same bridge sends near-identical SDP within 2 seconds (before state stabilizes),
		// skip processing to avoid churn-loop resets and media interruptions.
		currentOfferHash := hashSDP(payload.SDP)
		now := time.Now()
		if session.lastLocalOfferHash == currentOfferHash &&
			now.Sub(session.lastLocalOfferTime) < 2*time.Second &&
			(state == webrtc.SignalingStateHaveLocalOffer) {
			e.logf("[bcr_client][shadow] dedup same-bridge local offer bridgeId=%s hash=%s age=%dms state=%s",
				bridgeID, currentOfferHash[:12], now.Sub(session.lastLocalOfferTime).Milliseconds(), state.String())
			return true // Skip processing but report success
		}
		session.lastLocalOfferHash = currentOfferHash
		session.lastLocalOfferTime = now
		session.browserOfferSDP = payload.SDP

		if state != webrtc.SignalingStateStable {
			// Only attempt rollback if we're in a state that rollback can handle.
			// If rollback fails or is invalid, avoid double-reset; just proceed to controlled reset.
			if state == webrtc.SignalingStateHaveLocalOffer || state == webrtc.SignalingStateHaveLocalPranswer ||
				state == webrtc.SignalingStateHaveRemoteOffer || state == webrtc.SignalingStateHaveRemotePranswer {
				if !e.attemptImplicitRollback(session, bridgeID) {
					// Rollback failed: proceed to controlled reset without retry.
					e.logf("[bcr_client][shadow] rollback failed / skipped bridgeId=%s state=%s; performing controlled reset",
						bridgeID, state.String())
					if err := e.resetShadowSessionPC(bridgeID, session); err != nil {
						e.logf("[bcr_client][shadow] reset shadow pc failed bridgeId=%s: %v", bridgeID, err)
						e.sendShadowError(conn, bridgeID, "reset_pc", err, true)
						return false
					}
				}
			} else {
				// State not rollback-able: proceed directly to controlled reset.
				e.logf("[bcr_client][shadow] local offer in non-rollback state=%s bridgeId=%s; performing reset",
					state.String(), bridgeID)
				if err := e.resetShadowSessionPC(bridgeID, session); err != nil {
					e.logf("[bcr_client][shadow] reset shadow pc failed bridgeId=%s: %v", bridgeID, err)
					e.sendShadowError(conn, bridgeID, "reset_pc", err, true)
					return false
				}
			}
		}

		// createAndSetLocalDescription will: create aligned PC → CreateOffer → SetLocalDescription → sendShadowReady.
		return e.createAndSetLocalDescription(conn, bridgeID, session, true /*offer*/)

	case "answer":
		// The browser is acting as the answerer: it called setLocalDescription(answer).
		// RTC_SHADOW_READY was already sent when we processed the remote offer
		// (applyRemoteDescription "offer" case) via createAndSetLocalDescription.
		// There is nothing left to do for credential extraction here.
		e.logf("[bcr_client][shadow] local answer received — credentials already delivered bridgeId=%s state=%s",
			bridgeID, state.String())
		session.updatedAt = time.Now()
		return true

	default:
		err := fmt.Errorf("unsupported local sdpType=%q", payload.SDPType)
		e.sendShadowError(conn, bridgeID, "invalid_local_sdp_type", err, false)
		return false
	}
}

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

func (e *Engine) handleShadowRemote(conn *websocket.Conn, payload RTCShadowRemotePayload) {
	if payload.BridgeID == "" || payload.SDP == "" {
		e.sendShadowError(conn, payload.BridgeID, "validate", nil, false)
		return
	}

	session, err := e.getOrCreateShadowSession(payload.BridgeID)
	if err != nil {
		e.sendShadowError(conn, payload.BridgeID, "create_pc", err, true)
		return
	}

	// ── TURN Credential Injection (answerer-path timing fix) ────────────────
	// On the answerer path the browser fires:
	//   setRemoteDescription(offer)  → BCR_RTC_SHADOW_REMOTE   ← we are here
	//   createAnswer()               → BCR_RTC_SHADOW_LOCAL     ← arrives later
	//   setLocalDescription(answer)
	//
	// Without this block, newShadowPeerConnectionFromOffer (called moments later
	// inside applyRemoteDescription) would create the shadow PC with nil ICE
	// servers. ICE gathering would complete without TURN, and the subsequent
	// SetConfiguration call (in handleShadowLocal) arrives too late — Pion
	// silently ignores TURN servers added after gathering has concluded.
	//
	// By storing the TURN credentials here (payload.IceServers carries the same
	// values the constructor hook captured), session.iceServers is non-nil when
	// newShadowPeerConnectionFromOffer reads it, and the very first ICE gather
	// attempt uses the authenticated relay servers.
	if len(payload.IceServers) > 0 {
		// Only update if we don't already have a richer set (e.g. from a prior
		// RTC_SHADOW_LOCAL on the same session).
		if len(session.iceServers) == 0 {
			session.iceServers = payload.IceServers
			e.logf("[bcr_client][shadow] stored %d TURN server(s) from remote payload bridgeId=%s",
				len(payload.IceServers), payload.BridgeID)
			// Also push to the existing PC created by getOrCreateShadowSession so
			// that any gathering that started before createAlignedShadowPC replaces
			// it can also use TURN (belt-and-suspenders).
			if session.pc != nil {
				_ = session.pc.SetConfiguration(webrtc.Configuration{
					ICEServers: e.iceServersToWebRTC(session.iceServers),
				})
			}
		} else {
			e.logf("[bcr_client][shadow] remote payload ICE servers ignored (session already has %d server(s)) bridgeId=%s",
				len(session.iceServers), payload.BridgeID)
		}
	}
	// ────────────────────────────────────────────────────────────────────────

	e.applyRemoteDescription(conn, payload.BridgeID, session, payload)
}


func (e *Engine) handleShadowLocal(conn *websocket.Conn, payload RTCShadowLocalPayload) {
	if payload.BridgeID == "" || payload.SDP == "" {
		e.sendShadowError(conn, payload.BridgeID, "validate", nil, false)
		return
	}

	session, err := e.getOrCreateShadowSession(payload.BridgeID)
	if err != nil {
		e.sendShadowError(conn, payload.BridgeID, "create_pc", err, true)
		return
	}

	// Store ICE servers from the browser's RTCPeerConnection configuration.
	// These are needed for the shadow PC to reach remote peers via STUN/TURN.
	if len(payload.IceServers) > 0 {
		session.iceServers = payload.IceServers
		e.logf("[bcr_client][shadow] stored %d ICE server(s) bridgeId=%s", len(payload.IceServers), payload.BridgeID)

		// Ensure the actively bound PC adopts these servers immediately before any Gathering begins!
		if session.pc != nil {
			_ = session.pc.SetConfiguration(webrtc.Configuration{
				ICEServers: e.iceServersToWebRTC(session.iceServers),
			})
		}
	}

	e.applyLocalDescription(conn, payload.BridgeID, session, payload)
}

func (e *Engine) handleShadowPacket(conn *websocket.Conn, message []byte) bool {
	var pkt Packet
	if err := json.Unmarshal(message, &pkt); err != nil {
		return false
	}

	switch pkt.Type {
	case "RTC_SHADOW_REMOTE":
		var payload RTCShadowRemotePayload
		if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
			e.sendShadowError(conn, "", "decode_remote", err, false)
			return true
		}
		e.logf("[bcr_client] RTC_SHADOW_REMOTE bridgeId=%s sdpType=%s sdpLen=%d", payload.BridgeID, payload.SDPType, len(payload.SDP))
		e.handleShadowRemote(conn, payload)
		return true
	case "RTC_SHADOW_LOCAL":
		var payload RTCShadowLocalPayload
		if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
			e.sendShadowError(conn, "", "decode_local", err, false)
			return true
		}
		e.logf("[bcr_client] RTC_SHADOW_LOCAL bridgeId=%s sdpType=%s sdpLen=%d", payload.BridgeID, payload.SDPType, len(payload.SDP))
		e.handleShadowLocal(conn, payload)
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

// handleShadowICECandidate applies a remote peer's ICE candidate to the shadow PC.
// The extension forwards candidates that the VDI signaling channel delivers to
// the browser via addIceCandidate(), so the shadow PC can complete ICE connectivity.
func (e *Engine) handleShadowICECandidate(payload RTCShadowCandidatePayload) {
	e.shadowMu.Lock()
	session, ok := e.shadowSessions[payload.BridgeID]
	e.shadowMu.Unlock()

	if !ok || session.pc == nil {
		e.logf("[bcr_client][shadow] ICE candidate for unknown session bridgeId=%s — ignored", payload.BridgeID)
		return
	}

	// Pion expects the raw candidate value without any "a=candidate:" or "candidate:" prefix.
	cand := strings.TrimPrefix(payload.Candidate, "a=candidate:")
	cand = strings.TrimPrefix(cand, "candidate:")
	cand = strings.TrimRight(cand, "\r")

	sdpMid := payload.SDPMid
	if sdpMid == "" {
		sdpMid = "0"
	}

	if err := session.pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate: cand,
		SDPMid:    &sdpMid,
	}); err != nil {
		e.logf("[bcr_client][shadow] AddICECandidate failed bridgeId=%s err=%v", payload.BridgeID, err)
	} else {
		e.logf("[bcr_client][shadow] remote ICE candidate added bridgeId=%s", payload.BridgeID)
	}
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
