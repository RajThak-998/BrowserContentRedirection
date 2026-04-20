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

	// We intentionally DO NOT preload tracks here.
	// We use "Lazy Track Loading" - tracks will be dynamically added in WriteRTP
	// when the first actual RTP packet for a payload type arrives.
	// This prevents crashing WebKit with 10 empty tracks at startup.
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
// If a new PT is encountered, it will dynamically add a track and trigger renegotiation.
func (ls *loopbackSession) WriteRTP(pkt *rtp.Packet, currentCodecMap map[uint8]CodecInfo) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.pc == nil {
		return
	}

	track, ok := ls.tracks[pkt.Header.PayloadType]
	if !ok {
		// This PT is not tracked. Check if it's in the codec map.
		codecInfo, known := currentCodecMap[pkt.Header.PayloadType]
		if !known {
			return // unknown codec entirely
		}

		// Update our internal map and add the track
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
