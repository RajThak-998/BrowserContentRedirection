package engine

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// ── RTP Packet Cache (ring buffer) for instant local NACK retransmission ────
//
// WebKit's jitter buffer generates NACKs for packets that arrive slightly
// out-of-order or with short delays over the local loopback PeerConnection.
// Forwarding these NACKs to the remote SFU is too slow (hundreds of ms RTT)
// and the SFU's RTX retransmits arrive too late to be useful.
//
// Instead, we cache the last N RTP packets per loopback SSRC in a ring buffer
// and retransmit them instantly when WebKit NACKs them. This provides
// microsecond-level retransmission latency.

const (
	// rtpCacheSize is the number of RTP packets to cache per SSRC.
	// 512 packets at 30fps ≈ 17 seconds of video, more than enough for jitter.
	rtpCacheSize = 512

	// nackLogInterval controls how often NACK activity is logged.
	// Log a summary every N NACKs instead of spamming per-packet.
	nackLogInterval = 50

	// nackCooldownMs is the minimum interval (in ms) between forwarding
	// NACKs to the remote VDI for the same SSRC. Prevents flooding.
	nackCooldownMs = 200
)

// rtpRingBuffer is a lock-free (single-writer) ring buffer for caching RTP packets.
type rtpRingBuffer struct {
	buf  [rtpCacheSize]cachedRTPPacket
	mask uint16 // rtpCacheSize - 1, for fast modulo
}

type cachedRTPPacket struct {
	seq     uint16
	valid   bool
	payload []byte     // serialized RTP packet (header + payload)
}

func newRTPRingBuffer() *rtpRingBuffer {
	return &rtpRingBuffer{
		mask: rtpCacheSize - 1,
	}
}

// store caches a serialized RTP packet by its sequence number.
func (rb *rtpRingBuffer) store(seq uint16, raw []byte) {
	idx := seq & rb.mask
	// Copy the raw bytes so we don't hold references to the original buffer.
	dup := make([]byte, len(raw))
	copy(dup, raw)
	rb.buf[idx] = cachedRTPPacket{
		seq:     seq,
		valid:   true,
		payload: dup,
	}
}

// retrieve looks up a cached packet by sequence number.
// Returns the serialized packet bytes and true if found, nil and false otherwise.
func (rb *rtpRingBuffer) retrieve(seq uint16) ([]byte, bool) {
	idx := seq & rb.mask
	entry := &rb.buf[idx]
	if entry.valid && entry.seq == seq {
		return entry.payload, true
	}
	return nil, false
}

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

	// Map of VDI PayloadType to Pion's dynamically assigned loopback SSRC
	boundSSRCs map[uint8]uint32

	// Map of loopback SSRC to original VDI SSRC
	loopbackToVdiSSRC map[uint32]uint32

	// Local RTP packet cache per loopback SSRC for instant NACK retransmission.
	// Key: loopback SSRC, Value: ring buffer of recent packets.
	rtpCache map[uint32]*rtpRingBuffer

	// NACK statistics for sampled logging (avoids per-packet log spam).
	nackLocalHit   uint64 // NACKs satisfied from local cache
	nackLocalMiss  uint64 // NACKs that had to be forwarded to VDI
	nackTotal      uint64 // total NACK requests received
	nackLastLog    uint64 // nackTotal value at last log

	// Per-SSRC cooldown for forwarding NACKs to VDI.
	nackLastForward map[uint32]time.Time

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
		senders:           make(map[uint8]*webrtc.RTPSender),
		boundPTs:          make(map[uint8]webrtc.PayloadType),
		boundSSRCs:        make(map[uint8]uint32),
		loopbackToVdiSSRC: make(map[uint32]uint32),
		rtpCache:          make(map[uint32]*rtpRingBuffer),
		nackLastForward:   make(map[uint32]time.Time),
		droppedPTs:        make(map[uint8]bool),
		ptCodecMap:        ptCodecMap,
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
	ls.updateBindingsLocked()

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

	vdiPT := pkt.Header.PayloadType
	originalVdiSSRC := pkt.Header.SSRC

	// Rewrite PayloadType to match the loopback SDP, otherwise WebKit drops it.
	if boundPT, ok := ls.boundPTs[vdiPT]; ok {
		pkt.Header.PayloadType = uint8(boundPT)
	}

	// Strip RTP Header Extensions to prevent parsing errors in WebKit,
	// as these extensions (like MID, RID) were likely not negotiated in the loopback.
	pkt.Header.Extension = false
	pkt.Header.ExtensionProfile = 0
	pkt.Header.Extensions = nil

	// Rewrite SSRC to match the loopback track's negotiated SSRC, otherwise WebKit discards it.
	if boundSSRC, ok := ls.boundSSRCs[vdiPT]; ok && boundSSRC != 0 {
		pkt.Header.SSRC = boundSSRC
		ls.loopbackToVdiSSRC[boundSSRC] = originalVdiSSRC
	}

	// Cache the serialized packet BEFORE writing it to the track, so that
	// if WebKit immediately NACKs it (due to reorder), we can retransmit
	// from cache in microseconds rather than waiting for a VDI round-trip.
	loopSSRC := pkt.Header.SSRC
	if _, hasBuf := ls.rtpCache[loopSSRC]; !hasBuf {
		ls.rtpCache[loopSSRC] = newRTPRingBuffer()
	}
	if raw, marshalErr := pkt.Marshal(); marshalErr == nil {
		ls.rtpCache[loopSSRC].store(pkt.Header.SequenceNumber, raw)
	}

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

// updateBindingsLocked extracts mapped Payload Types and SSRCs from RTPSenders.
// MUST be called with ls.mu held.
func (ls *loopbackSession) updateBindingsLocked() {
	for pt, sender := range ls.senders {
		params := sender.GetParameters()
		if len(params.Codecs) > 0 {
			ls.boundPTs[pt] = params.Codecs[0].PayloadType
			ls.logf("[bcr_client][loopback][%s] mapped VDI PT=%d to loopback PT=%d", ls.bridgeID, pt, params.Codecs[0].PayloadType)
		}
		if len(params.Encodings) > 0 && params.Encodings[0].SSRC != 0 {
			ls.boundSSRCs[pt] = uint32(params.Encodings[0].SSRC)
			ls.logf("[bcr_client][loopback][%s] mapped VDI PT=%d to loopback SSRC=%d", ls.bridgeID, pt, params.Encodings[0].SSRC)
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

	// Update bound payload types and SSRCs from senders post-remote answer.
	ls.updateBindingsLocked()

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

// readRTCP reads RTCP packets generated by the local Wails WebKit engine.
//
// For NACKs, we first attempt to retransmit the requested packets from
// our local RTP cache (microsecond latency). Only if the cache doesn't
// have the packet do we forward the NACK to the remote VDI SFU (which
// has hundreds of ms RTT and may retransmit via RTX).
//
// For PLIs (keyframe requests), we always forward to the VDI since we
// cannot generate keyframes locally.
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
				ls.mu.Lock()
				vdiSSRC, hasMapping := ls.loopbackToVdiSSRC[p.MediaSSRC]
				ls.mu.Unlock()

				targetSSRC := p.MediaSSRC
				if hasMapping {
					targetSSRC = vdiSSRC
				}
				ls.logf("[bcr_client][loopback][%s] PLI from Wails SSRC=%d → VDI SSRC=%d", ls.bridgeID, p.MediaSSRC, targetSSRC)

				if ls.onRequestKeyframe != nil {
					ls.onRequestKeyframe(ls.bridgeID, targetSSRC)
				}

			case *rtcp.TransportLayerNack:
				ls.handleNACK(p, sender)
			}
		}
	}
}

// handleNACK processes a single NACK packet from WebKit. It attempts to
// retransmit the requested packets from the local RTP cache first.
// Only cache misses are forwarded to the remote VDI as a fallback.
func (ls *loopbackSession) handleNACK(p *rtcp.TransportLayerNack, sender *webrtc.RTPSender) {
	ls.mu.Lock()
	vdiSSRC, hasMapping := ls.loopbackToVdiSSRC[p.MediaSSRC]
	cache := ls.rtpCache[p.MediaSSRC]

	// Get the track for this SSRC so we can retransmit directly.
	var retransmitTrack *webrtc.TrackLocalStaticRTP
	for _, track := range ls.tracks {
		// Any track on this sender will do — we write raw packets.
		retransmitTrack = track
		break
	}
	// Find the specific track by checking which bound SSRC matches.
	for pt, ssrc := range ls.boundSSRCs {
		if ssrc == p.MediaSSRC {
			if t, ok := ls.tracks[pt]; ok {
				retransmitTrack = t
			}
			break
		}
	}
	ls.mu.Unlock()

	targetSSRC := p.MediaSSRC
	if hasMapping {
		targetSSRC = vdiSSRC
	}

	// Collect all missing sequence numbers from the NACK packet.
	var allMissing []uint16
	for _, pair := range p.Nacks {
		allMissing = append(allMissing, pair.PacketList()...)
	}

	// Attempt local retransmission from cache.
	var localRetransmitted int
	var cacheMisses []uint16

	if cache != nil && retransmitTrack != nil {
		for _, seq := range allMissing {
			if raw, found := cache.retrieve(seq); found {
				// Parse the cached packet and write it to the track.
				var cachedPkt rtp.Packet
				if unmarshalErr := cachedPkt.Unmarshal(raw); unmarshalErr == nil {
					if writeErr := retransmitTrack.WriteRTP(&cachedPkt); writeErr == nil {
						localRetransmitted++
					}
				}
			} else {
				cacheMisses = append(cacheMisses, seq)
			}
		}
	} else {
		// No cache available — all are misses.
		cacheMisses = allMissing
	}

	// Update statistics.
	totalNACKed := uint64(len(allMissing))
	ls.mu.Lock()
	ls.nackTotal += totalNACKed
	ls.nackLocalHit += uint64(localRetransmitted)
	ls.nackLocalMiss += uint64(len(cacheMisses))
	currentTotal := ls.nackTotal
	lastLog := ls.nackLastLog
	hitTotal := ls.nackLocalHit
	missTotal := ls.nackLocalMiss
	ls.mu.Unlock()

	// Sampled logging: log every nackLogInterval NACKs.
	if currentTotal-lastLog >= nackLogInterval {
		ls.mu.Lock()
		ls.nackLastLog = currentTotal
		ls.mu.Unlock()
		ls.logf("[bcr_client][loopback][%s] NACK summary: total=%d localHit=%d localMiss=%d hitRate=%.1f%% (SSRC %d→%d)",
			ls.bridgeID, currentTotal, hitTotal, missTotal,
			float64(hitTotal)/float64(max(currentTotal, 1))*100.0,
			p.MediaSSRC, targetSSRC)
	}

	// Forward cache misses to the remote VDI as a fallback, with rate-limiting.
	if len(cacheMisses) > 0 && ls.onNACK != nil {
		now := time.Now()
		ls.mu.Lock()
		lastFwd := ls.nackLastForward[p.MediaSSRC]
		ls.mu.Unlock()

		if now.Sub(lastFwd).Milliseconds() >= nackCooldownMs {
			ls.mu.Lock()
			ls.nackLastForward[p.MediaSSRC] = now
			ls.mu.Unlock()
			ls.onNACK(ls.bridgeID, targetSSRC, cacheMisses)
		}
	}
}
