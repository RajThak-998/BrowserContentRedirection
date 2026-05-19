package engine

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type loopbackSession struct {
	bridgeID        string
	logf            func(format string, args ...any)
	onLoopbackOffer func(bridgeID, sdp string)

	onRequestKeyframe func(bridgeID string, ssrc uint32)
	onNACK            func(bridgeID string, ssrc uint32, missing []uint16)

	mu sync.Mutex
	pc *webrtc.PeerConnection

	// Map of PayloadType to the local track where we write decrypted RTP
	tracks map[uint8]*webrtc.TrackLocalStaticRTP

	// Map of VDI PayloadType to the RTPSender to retrieve assigned parameters
	senders map[uint8]*webrtc.RTPSender

	// Map of VDI PayloadType to Pion's dynamically assigned loopback PayloadType
	boundPTs map[uint8]webrtc.PayloadType

	// One-time logging guard for dropped RTP PTs with no local track.
	droppedPTs map[uint8]bool

	// Track the current codecs we know about to detect when to add tracks
	ptCodecMap map[uint8]CodecInfo

	// Whether the initial offer (with pre-created tracks) has been emitted.
	initialOfferSent bool

	// Renegotiation safety: coalesce requests while an offer is in-flight.
	renegotiateQueued bool

	// lastOfferSDP caches the most recent completed offer SDP (i.e., after
	// ICE gathering finished). Used by GetLastOffer() so ReEmitAllLoopbackOffers
	// can recover from cold-start timing races where the first offer fired
	// before the frontend's EventsOn listener was registered.
	lastOfferSDP string

	// writeErrCount tracks consecutive WriteRTP errors for sampled logging.
	writeErrCount uint64
}

func newLoopbackSession(
	bridgeID string,
	logf func(format string, args ...any),
	onLoopbackOffer func(bridgeID, sdp string),
	ptCodecMap map[uint8]CodecInfo,
	onRequestKeyframe func(bridgeID string, ssrc uint32),
	onNACK func(bridgeID string, ssrc uint32, missing []uint16),
) *loopbackSession {
	ls := &loopbackSession{
		bridgeID:          bridgeID,
		logf:              logf,
		onLoopbackOffer:   onLoopbackOffer,
		onRequestKeyframe: onRequestKeyframe,
		onNACK:            onNACK,
		tracks:            make(map[uint8]*webrtc.TrackLocalStaticRTP),
		senders:         make(map[uint8]*webrtc.RTPSender),
		boundPTs:        make(map[uint8]webrtc.PayloadType),
		droppedPTs:      make(map[uint8]bool),
		ptCodecMap:      ptCodecMap,
	}

	ls.initPC()
	return ls
}

func (ls *loopbackSession) initPC() {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	// ── PeerConnection for local loopback ───────────────────────────────────
	//
	// IMPORTANT: We use the standard webrtc.NewPeerConnection() with the
	// package-level default API. This ensures:
	//
	//   1. The default MediaEngine is used, which has ALL standard codecs
	//      (Opus, VP8, VP9, H264, etc.) registered with correct parameters.
	//      Using webrtc.NewAPI(WithSettingEngine(...)) without WithMediaEngine
	//      creates a BARE MediaEngine with ZERO codecs — the resulting SDP has
	//      no payload types and is rejected by the browser.
	//
	//   2. ICE candidates use the machine's real network interface IP (e.g.
	//      10.x.x.x), NOT 127.0.0.1. WebKit/Safari SILENTLY FILTERS loopback
	//      candidates as a security measure — setRemoteDescription hangs
	//      forever (never resolves/rejects) when the offer SDP contains only
	//      127.0.0.1 candidates. SetNAT1To1IPs(127.0.0.1) is explicitly
	//      documented as wrong usage by pion/webrtc.
	//
	//   3. No ICE servers are configured since both peers (Go Pion and Wails
	//      WebView) are on the same machine. Host candidates are sufficient.
	//
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to create PeerConnection: %v", ls.bridgeID, err)
		return
	}
	ls.pc = pc

	// ── Pre-create tracks for all known preferred codecs ────────────────────
	// Because the SDP codec pinning guarantees only H264 + Opus will arrive,
	// we create exactly the tracks we need upfront. This avoids mid-stream
	// renegotiation that destabilizes WebKit's decoder pipeline.
	preCreated := 0
	for pt, codecInfo := range ls.ptCodecMap {
		ls.addTrackLocked(pt, codecInfo)
		if _, ok := ls.tracks[pt]; ok {
			preCreated++
		}
	}

	if preCreated > 0 {
		ls.logf("[bcr_client][loopback][%s] pre-created %d track(s) from preferred codec map", ls.bridgeID, preCreated)
		ls.renegotiateLocked()
		ls.initialOfferSent = true
	} else {
		ls.logf("[bcr_client][loopback][%s] WARNING: no codecs in ptCodecMap — loopback session created with no tracks", ls.bridgeID)
	}
}

// addTrackLocked creates a TrackLocalStaticRTP for the given codec and adds
// it to the PeerConnection. MUST be called with ls.mu held.
//
// IMPORTANT: We pass the FULL RTPCodecCapability (MimeType + ClockRate +
// Channels), not just MimeType. ClockRate is required by Pion to correctly
// match the track against the MediaEngine's registered codecs and to generate
// valid a=rtpmap lines in the SDP. Without ClockRate, the SDP may be
// malformed and rejected by the browser's setRemoteDescription.
func (ls *loopbackSession) addTrackLocked(pt uint8, codecInfo CodecInfo) {
	mimeType := ""
	lowerMime := strings.ToLower(codecInfo.MimeType)

	// Map the SFU's codec name to Pion's canonical MimeType constants.
	// Teams may send "X-H264UC" which is a proprietary H264 variant —
	// we map it to standard H264 since the RTP payload is compatible.
	switch {
	case strings.Contains(lowerMime, "vp8"):
		mimeType = webrtc.MimeTypeVP8
	case strings.Contains(lowerMime, "vp9"):
		mimeType = webrtc.MimeTypeVP9
	case strings.Contains(lowerMime, "h264"):
		mimeType = webrtc.MimeTypeH264
	case strings.Contains(lowerMime, "opus"):
		mimeType = webrtc.MimeTypeOpus
	default:
		ls.logf("[bcr_client][loopback][%s] skipping unsupported codec PT=%d mime=%s", ls.bridgeID, pt, codecInfo.MimeType)
		return
	}

	// Determine clock rate and channels from our CodecInfo (parsed from
	// the SDP). Fall back to standard defaults if the SDP didn't have them.
	clockRate := codecInfo.ClockRate
	if clockRate == 0 {
		if strings.HasPrefix(mimeType, "video/") {
			clockRate = 90000 // standard for all video codecs
		} else if mimeType == webrtc.MimeTypeOpus {
			clockRate = 48000
		}
	}
	channels := codecInfo.Channels
	if mimeType == webrtc.MimeTypeOpus && channels == 0 {
		channels = 2 // Opus is stereo by default
	}

	// Build the full codec capability with all required fields.
	capability := webrtc.RTPCodecCapability{
		MimeType:  mimeType,
		ClockRate: clockRate,
		Channels:  channels,
	}

	// Create a local track with a unique ID based on Payload Type
	trackID := fmt.Sprintf("track-%d-%s", pt, strings.ReplaceAll(lowerMime, "/", "-"))
	streamID := "stream-" + ls.bridgeID

	track, err := webrtc.NewTrackLocalStaticRTP(capability, trackID, streamID)
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to create local track for PT=%d mime=%s clockRate=%d: %v",
			ls.bridgeID, pt, mimeType, clockRate, err)
		return
	}

	sender, err := ls.pc.AddTrack(track)
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to add track to PC for PT=%d: %v", ls.bridgeID, pt, err)
		return
	}

	go ls.readRTCP(sender)

	ls.tracks[pt] = track
	ls.senders[pt] = sender
	ls.logf("[bcr_client][loopback][%s] added local track PT=%d mimeType=%s clockRate=%d channels=%d trackID=%s",
		ls.bridgeID, pt, mimeType, clockRate, channels, trackID)
}

// renegotiateLocked creates a new offer and emits it to the frontend.
// MUST be called with ls.mu held.
//
// We use webrtc.GatheringCompletePromise to wait for ICE candidate gathering
// to finish before emitting the offer SDP. This ensures the SDP contains
// actual host candidates (e.g. the machine's real IP), preventing the
// zero-candidate race where the frontend receives an offer with no candidates
// and ICE connectivity checks never start.
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

	// Register the gathering-complete promise BEFORE SetLocalDescription so
	// we don't miss the completion signal if gathering is very fast.
	gatherComplete := webrtc.GatheringCompletePromise(ls.pc)

	if err := ls.pc.SetLocalDescription(offer); err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to set local description: %v", ls.bridgeID, err)
		return
	}

	// Now that local description is set, Pion has bound the payload types.
	// We extract them from the senders to rewrite RTP packet headers later.
	for pt, sender := range ls.senders {
		params := sender.GetParameters()
		if len(params.Codecs) > 0 {
			ls.boundPTs[pt] = params.Codecs[0].PayloadType
			ls.logf("[bcr_client][loopback][%s] mapped VDI PT=%d to loopback PT=%d", ls.bridgeID, pt, params.Codecs[0].PayloadType)
		}
	}

	ls.renegotiateQueued = false

	// Capture fields needed by the goroutine before releasing the mutex.
	pc := ls.pc
	bridgeID := ls.bridgeID
	cb := ls.onLoopbackOffer
	logf := ls.logf

	ls.logf("[bcr_client][loopback][%s] waiting for ICE gathering to complete before emitting offer to frontend", ls.bridgeID)

	// Wait for ICE gathering in a separate goroutine so we do not hold ls.mu.
	go func() {
		<-gatherComplete

		ld := pc.LocalDescription()
		if ld == nil {
			logf("[bcr_client][loopback][%s] ERROR: LocalDescription is nil after ICE gathering complete", bridgeID)
			return
		}

		finalSDP := ld.SDP
		logf("[bcr_client][loopback][%s] ICE gathering complete — emitting offer sdpLen=%d to frontend", bridgeID, len(finalSDP))

		// Cache the completed offer SDP for re-emission on reconnect.
		ls.mu.Lock()
		ls.lastOfferSDP = finalSDP
		ls.mu.Unlock()

		cb(bridgeID, finalSDP)
	}()
}

// GetLastOffer returns the most recently gathered and cached offer SDP.
// Returns "" if no offer has been emitted yet (e.g. initPC failed or
// gathering has not completed). Used by ReEmitAllLoopbackOffers.
func (ls *loopbackSession) GetLastOffer() string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.lastOfferSDP
}

// SyncTracksFromCodecMap diffs the given codec map (already filtered to
// preferred codecs) against the loopback session's existing tracks. For every
// PT that is preferred but has no local track yet, a new TrackLocalStaticRTP
// is added and a Pion renegotiation offer is fired to the frontend.
//
// This handles the Teams 2-phase negotiation pattern where the SFU sends an
// initial audio-only answer (triggering SRTP ready + loopback creation) and
// immediately follows with a renegotiation offer adding video tracks.
func (ls *loopbackSession) SyncTracksFromCodecMap(filteredCodecMap map[uint8]CodecInfo) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.pc == nil {
		return
	}

	added := 0
	for pt, codecInfo := range filteredCodecMap {
		if _, exists := ls.tracks[pt]; exists {
			continue // track already present
		}
		ls.logf("[bcr_client][loopback][%s] renegotiation sync: adding missing track PT=%d codec=%s clockRate=%d",
			ls.bridgeID, pt, codecInfo.MimeType, codecInfo.ClockRate)
		ls.addTrackLocked(pt, codecInfo)
		if _, nowExists := ls.tracks[pt]; nowExists {
			added++
		}
	}

	if added > 0 {
		ls.logf("[bcr_client][loopback][%s] renegotiation sync: added %d new track(s) — triggering new Pion offer to frontend",
			ls.bridgeID, added)
		ls.renegotiateLocked()
	} else {
		ls.logf("[bcr_client][loopback][%s] renegotiation sync: no new tracks needed (all preferred PTs already present)",
			ls.bridgeID)
	}
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
			ls.logf("[bcr_client][loopback][%s] strict-drop PT=%d (no pre-created local track — check codec pinning)",
				ls.bridgeID, pkt.Header.PayloadType)
		}
		return
	}

	// Rewrite PayloadType to match the loopback SDP, otherwise WebKit drops it.
	if boundPT, ok := ls.boundPTs[pkt.Header.PayloadType]; ok {
		pkt.Header.PayloadType = uint8(boundPT)
	}

	// Strip RTP Header Extensions to prevent parsing errors in WebKit,
	// as these extensions (like MID, RID) were likely not negotiated in the loopback.
	pkt.Header.Extension = false
	pkt.Header.ExtensionProfile = 0
	pkt.Header.Extensions = nil

	if err := track.WriteRTP(pkt); err != nil {
		ls.writeErrCount++
		if ls.writeErrCount == 1 || ls.writeErrCount%500 == 0 {
			ls.logf("[bcr_client][loopback][%s] track.WriteRTP error #%d PT=%d: %v",
				ls.bridgeID, ls.writeErrCount, pkt.Header.PayloadType, err)
		}
	} else {
		ls.writeErrCount = 0
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

// readRTCP reads RTCP packets generated by the local Wails WebKit engine
// and forwards NACKs and PLIs to the VDI to recover from packet loss.
func (ls *loopbackSession) readRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}

		pkts, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}

		for _, pkt := range pkts {
			switch p := pkt.(type) {
			case *rtcp.PictureLossIndication:
				ls.logf("[bcr_client][loopback][%s] received PLI from Wails for SSRC=%d, forwarding to VDI", ls.bridgeID, p.MediaSSRC)
				if ls.onRequestKeyframe != nil {
					ls.onRequestKeyframe(ls.bridgeID, p.MediaSSRC)
				}
			case *rtcp.TransportLayerNack:
				ls.logf("[bcr_client][loopback][%s] received NACK from Wails for SSRC=%d, forwarding to VDI", ls.bridgeID, p.MediaSSRC)
				if ls.onNACK != nil {
					for _, pair := range p.Nacks {
						// Extract the missing sequence numbers from the packet list
						missing := pair.PacketList()
						ls.onNACK(ls.bridgeID, p.MediaSSRC, missing)
					}
				}
			}
		}
	}
}
