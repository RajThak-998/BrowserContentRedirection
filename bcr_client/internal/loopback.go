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

	// Track the current codecs we know about to detect when to add tracks
	ptCodecMap map[uint8]CodecInfo

	// Whether the initial offer (with pre-created tracks) has been emitted.
	initialOfferSent bool
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
	// Because the SDP codec pinning guarantees only VP8 + Opus will arrive,
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

	offer, err := ls.pc.CreateOffer(nil)
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to create offer: %v", ls.bridgeID, err)
		return
	}

	if err := ls.pc.SetLocalDescription(offer); err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to set local description: %v", ls.bridgeID, err)
		return
	}

	ls.logf("[bcr_client][loopback][%s] generated local loopback offer, emitting to frontend", ls.bridgeID)

	// Emit to frontend (asynchronous to avoid blocking)
	go ls.onLoopbackOffer(ls.bridgeID, offer.SDP)
}

// WriteRTP routes incoming RTP packets to the correct local track.
// With codec pinning, tracks are pre-created so this is a fast-path lookup.
// If an unexpected PT arrives (fallback), it will dynamically add a track.
func (ls *loopbackSession) WriteRTP(pkt *rtp.Packet, currentCodecMap map[uint8]CodecInfo) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.pc == nil {
		return
	}

	track, ok := ls.tracks[pkt.Header.PayloadType]
	if !ok {
		// Fallback: This PT wasn't in the pre-created set. Check if it's in the codec map.
		codecInfo, known := currentCodecMap[pkt.Header.PayloadType]
		if !known {
			return // unknown codec entirely
		}

		// Dynamic track addition (fallback path — should be rare with codec pinning)
		ls.logf("[bcr_client][loopback][%s] fallback: dynamically adding track for unexpected PT=%d codec=%s",
			ls.bridgeID, pkt.Header.PayloadType, codecInfo.MimeType)
		ls.ptCodecMap[pkt.Header.PayloadType] = codecInfo
		ls.addTrackLocked(pkt.Header.PayloadType, codecInfo)
		ls.renegotiateLocked()

		track = ls.tracks[pkt.Header.PayloadType]
	}

	if track != nil {
		if err := track.WriteRTP(pkt); err != nil {
			// Don't log every error as it will flood if connection is closing
		}
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
