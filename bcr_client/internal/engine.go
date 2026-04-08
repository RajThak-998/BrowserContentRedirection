package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

type shadowSession struct {
	pc        *webrtc.PeerConnection
	updatedAt time.Time
}

type Engine struct {
	cfg Config
	cb  Callbacks

	upgrader websocket.Upgrader
	server   *http.Server

	shadowMu       sync.Mutex
	shadowSessions map[string]*shadowSession
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
	conn, err := e.upgrader.Upgrade(w, r, nil)
	if err != nil {
		e.logf("[bcr_client] websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	e.logf("[bcr_client] websocket bridge connected")

	for {
		mt, message, err := conn.ReadMessage()
		if err != nil {
			e.logf("[bcr_client] websocket disconnected: %v", err)
			return
		}

		if mt == websocket.TextMessage {
			if e.handleShadowPacket(conn, message) {
				continue
			}
			if e.handleVideoUpdate(message) {
				continue
			}
			continue
		}

		if mt == websocket.BinaryMessage {
			if e.cb.OnVideoChunk != nil {
				e.cb.OnVideoChunk(message)
			}
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

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}

	session := &shadowSession{pc: pc, updatedAt: time.Now()}
	e.shadowSessions[bridgeID] = session
	return session, nil
}

func (e *Engine) closeShadowSession(bridgeID string) {
	e.shadowMu.Lock()
	session, ok := e.shadowSessions[bridgeID]
	if ok {
		delete(e.shadowSessions, bridgeID)
	}
	e.shadowMu.Unlock()

	if ok && session.pc != nil {
		_ = session.pc.Close()
	}
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

	pc := session.pc
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: mapSDPType(payload.SDPType), SDP: payload.SDP}); err != nil {
		e.sendShadowError(conn, payload.BridgeID, "set_remote", err, false)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		e.sendShadowError(conn, payload.BridgeID, "create_answer", err, true)
		return
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		e.sendShadowError(conn, payload.BridgeID, "set_local", err, true)
		return
	}

	select {
	case <-gatherDone:
	case <-time.After(1500 * time.Millisecond):
	}

	localDesc := pc.LocalDescription()
	if localDesc == nil {
		e.sendShadowError(conn, payload.BridgeID, "local_desc_nil", nil, true)
		return
	}

	ufrag, pwd, fingerprint := ExtractShadowCredentials(localDesc.SDP)
	if ufrag == "" || pwd == "" || fingerprint == "" {
		e.sendShadowError(conn, payload.BridgeID, "extract_credentials", nil, false)
		return
	}

	resp := RTCShadowReadyPayload{
		BridgeID:        payload.BridgeID,
		ICEUfrag:        ufrag,
		ICEPwd:          pwd,
		DTLSFingerprint: fingerprint,
		LocalIP:         detectLocalIPv4(),
		GeneratedAt:     time.Now().UnixMilli(),
		ExpiresAt:       time.Now().Add(60 * time.Second).UnixMilli(),
	}

	if err := writeJSONPacket(conn, "RTC_SHADOW_READY", resp); err != nil {
		e.logf("[bcr_client] RTC_SHADOW_READY send failed: %v", err)
		return
	}

	e.logf("[bcr_client] RTC_SHADOW_READY bridgeId=%s", payload.BridgeID)
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
	case "RTC_SHADOW_CLOSE":
		var payload RTCShadowClosePayload
		if err := json.Unmarshal(pkt.Payload, &payload); err == nil {
			e.closeShadowSession(payload.BridgeID)
			e.logf("[bcr_client] RTC_SHADOW_CLOSE bridgeId=%s", payload.BridgeID)
		}
		return true
	default:
		return false
	}
}

func (e *Engine) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if e.cb.OnLog != nil {
		e.cb.OnLog(msg)
		return
	}
	fmt.Println(msg)
}
