package engine

// relay.go — Local Pion relay PC that bridges raw decrypted RTP from the shadow
// transport to the Wails WebRTC frontend.
//
// Flow (new raw transport architecture):
//
//   rawShadowSession (SRTP decrypted) ─── onRTPPacket callback ───►
//   engine.onRawRTPPacket ──► relaySession (TrackLocalStaticRTP per SSRC)
//                                             │  relay PC offer
//                                   Wails WebView (onRelayOffer)
//                                             │  answer
//                              engine.HandleRelayAnswer ◄─┘
//                                             │
//                           ICE+DTLS (localhost) ─┘
//                              video.srcObject = stream

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// relaySession holds the local Pion PeerConnection that streams media to the
// Wails WebView. localTracks is keyed by SSRC string so each media stream gets
// its own track regardless of codec PT changes.
type relaySession struct {
	bridgeID    string
	pc          *webrtc.PeerConnection
	localTracks map[string]*webrtc.TrackLocalStaticRTP // ssrcKey → local track

	mu          sync.Mutex
	settleTimer *time.Timer
	offerSent   bool
}

// newRelayPeerConnectionWithCodecs creates a relay PeerConnection whose MediaEngine
// is seeded with the exact PT numbers from the Teams SFU negotiation (captured
// from the browser's SDP). By using Teams' PTs in the relay offer, the Wails
// frontend will receive packets with matching PT numbers and decode them correctly.
func (e *Engine) newRelayPeerConnectionWithCodecs(codecs map[uint8]CodecInfo) (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}

	registered := 0
	for pt, codec := range codecs {
		codecType := webrtc.RTPCodecTypeAudio
		if strings.Contains(strings.ToUpper(codec.MimeType), "VIDEO") {
			codecType = webrtc.RTPCodecTypeVideo
		}
		err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  codec.MimeType,
				ClockRate: codec.ClockRate,
				Channels:  codec.Channels,
			},
			PayloadType: webrtc.PayloadType(pt),
		}, codecType)
		if err != nil {
			e.logf("[bcr_client][relay] registerCodec skip pt=%d mime=%s: %v", pt, codec.MimeType, err)
		} else {
			registered++
		}
	}

	if registered == 0 {
		// Safety fallback: no codecs from SDP — use Pion defaults.
		e.logf("[bcr_client][relay] no codecs from ptMap — falling back to RegisterDefaultCodecs")
		if err := me.RegisterDefaultCodecs(); err != nil {
			return nil, fmt.Errorf("relay: registerDefaultCodecs: %w", err)
		}
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

// onRawRTPPacket is the entry point for each inbound decrypted RTP packet from
// the rawShadowSession's SRTP read loop. It routes the packet to the relay layer:
//
//  1. Looks up the codec for the packet's payload type in ptCodecMap.
//  2. Creates the relay session (with Teams' PTs) on first call for a bridge.
//  3. Creates a TrackLocalStaticRTP per unique SSRC (first packet for each track).
//  4. Starts the 200ms settle timer after the last new track before offer creation.
//  5. Writes every subsequent packet to the existing local track.
func (e *Engine) onRawRTPPacket(bridgeID string, pkt *rtp.Packet, ptCodecMap map[uint8]CodecInfo) {
	if !e.shouldProcessBridgeTrack(bridgeID) {
		return
	}

	// Look up codec for this packet's payload type.
	codec, ok := ptCodecMap[pkt.Header.PayloadType]
	if !ok {
		// RTX, padding-only, or unknown PT — drop silently.
		return
	}

	// Get or create relay session.
	e.relayMu.Lock()
	session, ok := e.relaySessions[bridgeID]
	if !ok {
		relayPC, err := e.newRelayPeerConnectionWithCodecs(ptCodecMap)
		if err != nil {
			e.relayMu.Unlock()
			e.logf("[bcr_client][relay] newRelayPeerConnectionWithCodecs failed bridgeId=%s: %v", bridgeID, err)
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

	ssrcKey := fmt.Sprintf("%d", pkt.SSRC)

	session.mu.Lock()
	localTrack, hasTrack := session.localTracks[ssrcKey]
	if !hasTrack {
		// First packet for this SSRC → create a dedicated local track.
		cap := codecInfoToCapability(codec)
		var trackErr error
		localTrack, trackErr = webrtc.NewTrackLocalStaticRTP(cap, ssrcKey, bridgeID)
		if trackErr != nil {
			session.mu.Unlock()
			e.logf("[bcr_client][relay] NewTrackLocalStaticRTP failed ssrc=%d bridgeId=%s: %v",
				pkt.SSRC, bridgeID, trackErr)
			return
		}
		session.localTracks[ssrcKey] = localTrack

		if _, addErr := session.pc.AddTrack(localTrack); addErr != nil {
			session.mu.Unlock()
			e.logf("[bcr_client][relay] AddTrack failed ssrc=%d bridgeId=%s: %v",
				pkt.SSRC, bridgeID, addErr)
			return
		}
		e.logf("[bcr_client][relay] new track ssrc=%d pt=%d mime=%s bridgeId=%s",
			pkt.SSRC, pkt.Header.PayloadType, codec.MimeType, bridgeID)

		// Settle timer: wait 200ms after the last new track before issuing the
		// offer, so both audio and video tracks are in the SDP.
		if session.settleTimer != nil {
			session.settleTimer.Reset(200 * time.Millisecond)
		} else {
			session.settleTimer = time.AfterFunc(200*time.Millisecond, func() {
				e.createAndSendRelayOffer(bridgeID, session)
			})
		}
	}
	session.mu.Unlock()

	// Forward the decrypted RTP packet to the local relay track.
	// WriteRTP sends the packet as-is; the frontend decodes using the PT that
	// was negotiated from the Teams-sourced relay offer (same PT).
	_ = localTrack.WriteRTP(pkt)
}

// codecInfoToCapability converts a CodecInfo (from sdp.go) to the
// webrtc.RTPCodecCapability needed by webrtc.NewTrackLocalStaticRTP.
// PayloadType is NOT part of RTPCodecCapability — it lives in RTPCodecParameters.
// The actual PT in transmitted RTP packets comes from pkt.Header.PayloadType.
func codecInfoToCapability(c CodecInfo) webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{
		MimeType:  c.MimeType,
		ClockRate: c.ClockRate,
		Channels:  c.Channels,
	}
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
