package engine

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type loopbackSession struct {
	bridgeID        string
	logf            func(format string, args ...any)
	onLoopbackOffer func(bridgeID, sdp string)

	mu sync.Mutex
	pc *webrtc.PeerConnection

	// Map of PayloadType to the local track where we write decrypted RTP
	tracks map[uint8]*webrtc.TrackLocalStaticRTP

	// One-time logging guard for dropped RTP PTs with no local track.
	droppedPTs map[uint8]bool

	// Track the current codecs we know about to detect when to add tracks
	ptCodecMap map[uint8]CodecInfo

	// Whether the initial offer (with pre-created tracks) has been emitted.
	initialOfferSent bool

	// Renegotiation safety: coalesce requests while an offer is in-flight.
	renegotiateQueued bool
}

func newLoopbackSession(
	bridgeID string,
	logf func(format string, args ...any),
	onLoopbackOffer func(bridgeID, sdp string),
	ptCodecMap map[uint8]CodecInfo,
) *loopbackSession {
	ls := &loopbackSession{
		bridgeID:        bridgeID,
		logf:            logf,
		onLoopbackOffer: onLoopbackOffer,
		tracks:          make(map[uint8]*webrtc.TrackLocalStaticRTP),
		droppedPTs:      make(map[uint8]bool),
		ptCodecMap:      ptCodecMap,
	}

	ls.initPC()
	return ls
}

func (ls *loopbackSession) initPC() {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	// Configuration with no ICE servers because this is entirely local loopback.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to create PeerConnection: %v", ls.bridgeID, err)
		return
	}
	ls.pc = pc

	// ── Pre-create tracks for all known codecs ──────────────────────────────
	// Because the SDP codec pinning guarantees only preferred video + Opus will arrive,
	// we can create exactly the tracks we need upfront. This avoids mid-stream
	// renegotiation that destabilizes WebKit's decoder pipeline.
	//
	// If ptCodecMap is empty (shouldn't happen with codec pinning), we fall
	// back to lazy track loading in WriteRTP.
	preCreated := 0
	for pt, codecInfo := range ls.ptCodecMap {
		ls.addTrackLocked(pt, codecInfo)
		if _, ok := ls.tracks[pt]; ok {
			preCreated++
		}
	}

	if preCreated > 0 {
		ls.logf("[bcr_client][loopback][%s] pre-created %d track(s) from pinned codec map", ls.bridgeID, preCreated)
		ls.renegotiateLocked()
		ls.initialOfferSent = true
	} else {
		ls.logf("[bcr_client][loopback][%s] no codecs in ptCodecMap — will lazy-load tracks on first RTP", ls.bridgeID)
	}
}

func (ls *loopbackSession) addTrackLocked(pt uint8, codecInfo CodecInfo) {
	mimeType := ""
	lowerMime := strings.ToLower(codecInfo.MimeType)

	// Determine the base Pion MimeType
	if strings.Contains(lowerMime, "vp8") {
		mimeType = webrtc.MimeTypeVP8
	} else if strings.Contains(lowerMime, "vp9") {
		mimeType = webrtc.MimeTypeVP9
	} else if strings.Contains(lowerMime, "h264") {
		mimeType = webrtc.MimeTypeH264
	} else if strings.Contains(lowerMime, "opus") {
		mimeType = webrtc.MimeTypeOpus
	} else {
		// Unsupported codec for loopback, just skip it.
		// Pion supports G722, PCMU, PCMA, etc., but we only care about modern codecs.
		return
	}

	// Create a local track with a unique ID based on Payload Type to prevent SDP duplicate track ID crashes
	trackID := fmt.Sprintf("track-%d-%s", pt, strings.ReplaceAll(lowerMime, "/", "-"))
	streamID := "stream-" + ls.bridgeID

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: mimeType}, trackID, streamID)
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to create local track for PT=%d: %v", ls.bridgeID, pt, err)
		return
	}

	if _, err := ls.pc.AddTrack(track); err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to add track to PC: %v", ls.bridgeID, err)
		return
	}

	ls.tracks[pt] = track
	ls.logf("[bcr_client][loopback][%s] added local track PT=%d MimeType=%s", ls.bridgeID, pt, mimeType)
}

func (ls *loopbackSession) renegotiateLocked() {
	if ls.pc == nil {
		return
	}

	if ls.pc.SignalingState() != webrtc.SignalingStateStable {
		// Queue a single follow-up renegotiation and avoid re-entering SetLocal.
		ls.renegotiateQueued = true
		ls.logf("[bcr_client][loopback][%s] renegotiation deferred: signalingState=%s", ls.bridgeID, ls.pc.SignalingState().String())
		return
	}

	offer, err := ls.pc.CreateOffer(nil)
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to create offer: %v", ls.bridgeID, err)
		return
	}

	if err := ls.pc.SetLocalDescription(offer); err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to set local description: %v", ls.bridgeID, err)
		return
	}

	ls.renegotiateQueued = false

	ls.logf("[bcr_client][loopback][%s] generated local loopback offer, emitting to frontend", ls.bridgeID)

	// Emit to frontend (asynchronous to avoid blocking)
	go ls.onLoopbackOffer(ls.bridgeID, offer.SDP)
}

// WriteRTP routes incoming RTP packets to the correct local track.
// With strict codec pinning, tracks are pre-created and unexpected PTs are
// dropped immediately (no dynamic AddTrack/renegotiation fallback).
func (ls *loopbackSession) WriteRTP(pkt *rtp.Packet) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.pc == nil {
		return
	}

	track, ok := ls.tracks[pkt.Header.PayloadType]
	if !ok {
		if !ls.droppedPTs[pkt.Header.PayloadType] {
			ls.droppedPTs[pkt.Header.PayloadType] = true
			ls.logf("[bcr_client][loopback][%s] strict-drop PT=%d (no pre-created local track)",
				ls.bridgeID, pkt.Header.PayloadType)
		}
		return
	}

	if err := track.WriteRTP(pkt); err != nil {
		// Don't log every error as it will flood if connection is closing
	}
}

// SetRemoteDescription applies the SDP answer from the frontend
func (ls *loopbackSession) SetRemoteDescription(sdp string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.pc == nil {
		return
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}

	if err := ls.pc.SetRemoteDescription(answer); err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to set remote description from frontend: %v", ls.bridgeID, err)
		return
	}

	ls.logf("[bcr_client][loopback][%s] applied remote description from frontend successfully", ls.bridgeID)

	if ls.renegotiateQueued {
		ls.logf("[bcr_client][loopback][%s] processing queued renegotiation after remote answer", ls.bridgeID)
		ls.renegotiateLocked()
	}
}

func (ls *loopbackSession) Close() error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.pc != nil {
		err := ls.pc.Close()
		ls.pc = nil
		return err
	}
	return nil
}
