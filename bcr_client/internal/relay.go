package engine

// relay.go — Local Pion relay PC that bridges received RTP from the shadow PC
// to the Wails WebRTC frontend.
//
// Flow:
//   shadow PC (remote peer) ── OnTrack ──► relaySession ──► TrackLocalStaticRTP
//                                                                    │
//                                             Pion relay PC offer ◄─┘
//                                                    │
//                                         Wails WebView (onRelayOffer)
//                                                    │  answer
//                                         engine.HandleRelayAnswer ◄─┘
//                                                    │
//                              ICE+DTLS (localhost) ─┘
//                                 video.srcObject = stream

import (
	"fmt"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// relaySession holds the local Pion PeerConnection that streams media to the
// Wails WebView, plus bookkeeping for the settle-timer and offer state.
type relaySession struct {
	bridgeID    string
	pc          *webrtc.PeerConnection
	localTracks map[string]*webrtc.TrackLocalStaticRTP // trackID → local track

	mu          sync.Mutex
	settleTimer *time.Timer
	offerSent   bool
}

// newRelayPeerConnection creates a plain PeerConnection for localhost relay.
// No STUN/TURN needed — relay PC talks only to the Wails WebView on the same machine.
func (e *Engine) newRelayPeerConnection() (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("relay: registerDefaultCodecs: %w", err)
	}

	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return nil, fmt.Errorf("relay: registerDefaultInterceptors: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(ir),
	)
	return api.NewPeerConnection(webrtc.Configuration{})
}

// onShadowTrack is called by the shadow PC's OnTrack callback whenever the
// remote peer's media stream starts arriving. It creates (or reuses) a relay
// session, injects a TrackLocalStaticRTP per incoming track, and starts an
// RTP forwarding goroutine. The relay offer is sent to the Wails frontend after
// a 200 ms settle window (so audio + video tracks are both added before the
// offer SDP is generated).
func (e *Engine) onShadowTrack(bridgeID string, gen int, track *webrtc.TrackRemote) {
	e.logf("[bcr_client][relay] OnTrack kind=%s codec=%s ssrc=%d bridgeId=%s",
		track.Kind(), track.Codec().MimeType, track.SSRC(), bridgeID)

	// Create a local track that will carry RTP to the Wails frontend.
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		track.Codec().RTPCodecCapability,
		track.ID(),
		track.StreamID(),
	)
	if err != nil {
		e.logf("[bcr_client][relay] NewTrackLocalStaticRTP failed bridgeId=%s err=%v", bridgeID, err)
		return
	}

	// Get or create relay session.
	e.relayMu.Lock()
	session, ok := e.relaySessions[bridgeID]
	if !ok {
		relayPC, pcErr := e.newRelayPeerConnection()
		if pcErr != nil {
			e.relayMu.Unlock()
			e.logf("[bcr_client][relay] newRelayPeerConnection failed bridgeId=%s err=%v", bridgeID, pcErr)
			return
		}
		session = &relaySession{
			bridgeID:    bridgeID,
			pc:          relayPC,
			localTracks: make(map[string]*webrtc.TrackLocalStaticRTP),
		}
		e.relaySessions[bridgeID] = session
	}
	e.relayMu.Unlock()

	// Add the local track to the relay PC.
	session.mu.Lock()
	session.localTracks[track.ID()] = localTrack
	if _, addErr := session.pc.AddTrack(localTrack); addErr != nil {
		session.mu.Unlock()
		e.logf("[bcr_client][relay] AddTrack failed bridgeId=%s err=%v", bridgeID, addErr)
		return
	}

	// Settle timer: wait 200 ms after the last OnTrack before creating the offer.
	// In practice this window accommodates both audio and video arriving nearly
	// simultaneously, guaranteeing a complete multi-track SDP.
	if session.settleTimer != nil {
		session.settleTimer.Reset(200 * time.Millisecond)
	} else {
		session.settleTimer = time.AfterFunc(200*time.Millisecond, func() {
			e.createAndSendRelayOffer(bridgeID, session)
		})
	}
	session.mu.Unlock()

	// Start per-track RTP forwarding.
	go e.forwardRTP(bridgeID, gen, track, localTrack)
}

// createAndSendRelayOffer generates the relay PC's local offer (after ICE
// gathering) and fires the OnRelayOffer callback so app.go can push the SDP
// to the Wails frontend via runtime.EventsEmit.
func (e *Engine) createAndSendRelayOffer(bridgeID string, session *relaySession) {
	session.mu.Lock()
	if session.offerSent {
		session.mu.Unlock()
		return
	}
	session.offerSent = true
	session.mu.Unlock()

	offer, err := session.pc.CreateOffer(nil)
	if err != nil {
		e.logf("[bcr_client][relay] CreateOffer failed bridgeId=%s err=%v", bridgeID, err)
		return
	}
	if err = session.pc.SetLocalDescription(offer); err != nil {
		e.logf("[bcr_client][relay] SetLocalDescription failed bridgeId=%s err=%v", bridgeID, err)
		return
	}

	// Relay PC connects to Wails WebView (localhost) — host candidates only, near-instant.
	gatherDone := webrtc.GatheringCompletePromise(session.pc)
	select {
	case <-gatherDone:
		e.logf("[bcr_client][relay] ICE gathered bridgeId=%s", bridgeID)
	case <-time.After(1500 * time.Millisecond):
		e.logf("[bcr_client][relay] ICE gather timeout bridgeId=%s (proceeding)", bridgeID)
	}

	sdp := session.pc.LocalDescription().SDP
	e.logf("[bcr_client][relay] relay offer ready sdpLen=%d bridgeId=%s", len(sdp), bridgeID)

	if e.cb.OnRelayOffer != nil {
		e.cb.OnRelayOffer(bridgeID, sdp)
	}
}

// HandleRelayAnswer is called by app.go when the Wails frontend returns its
// WebRTC answer (from createAnswer). It completes the relay PC handshake so
// ICE+DTLS can establish and media starts flowing to the <video> element.
func (e *Engine) HandleRelayAnswer(bridgeID, sdp string) error {
	e.relayMu.Lock()
	session, ok := e.relaySessions[bridgeID]
	e.relayMu.Unlock()

	if !ok {
		return fmt.Errorf("no relay session for bridgeId=%s", bridgeID)
	}

	if err := session.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		return fmt.Errorf("relay SetRemoteDescription failed: %w", err)
	}

	e.logf("[bcr_client][relay] relay answer applied — ICE/DTLS establishing bridgeId=%s", bridgeID)
	return nil
}

// forwardRTP reads RTP packets from the shadow track and writes them to the
// local relay track, making them available to the Wails WebView's ontrack stream.
// The goroutine exits when the shadow PC generation changes (PC was rebuilt) or
// when the remote track is closed.
func (e *Engine) forwardRTP(bridgeID string, gen int, remote *webrtc.TrackRemote, local *webrtc.TrackLocalStaticRTP) {
	e.logf("[bcr_client][relay][fwd] start kind=%s codec=%s gen=%d bridgeId=%s",
		remote.Kind(), remote.Codec().MimeType, gen, bridgeID)
	defer e.logf("[bcr_client][relay][fwd] stop kind=%s gen=%d bridgeId=%s",
		remote.Kind(), gen, bridgeID)

	for {
		// Guard: stop if the shadow PC was rebuilt (stale generation).
		e.shadowMu.Lock()
		sess, ok := e.shadowSessions[bridgeID]
		currentGen := 0
		if ok {
			currentGen = sess.generation
		}
		e.shadowMu.Unlock()

		if !ok || currentGen != gen {
			return
		}

		pkt, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		// Non-fatal write errors (relay PC not yet connected) — keep reading.
		_ = local.WriteRTP(pkt)
	}
}

// closeRelaySession closes the relay PC for a bridge and removes it from the map.
func (e *Engine) closeRelaySession(bridgeID string) {
	e.relayMu.Lock()
	session, ok := e.relaySessions[bridgeID]
	if ok {
		delete(e.relaySessions, bridgeID)
	}
	e.relayMu.Unlock()

	if ok && session.pc != nil {
		_ = session.pc.Close()
		e.logf("[bcr_client][relay] relay session closed bridgeId=%s", bridgeID)
	}
}
