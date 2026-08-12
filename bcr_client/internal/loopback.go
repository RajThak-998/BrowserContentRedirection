package engine

import (
	"fmt"
	"sort"
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

	// nackLogMinInterval is the minimum wall-clock gap between NACK-summary log
	// lines. Time-based (not per-N-NACKs) so heavy loss can't turn logging into a
	// self-inflicted CPU/IO storm that starves the SRTP read loop. Matched to the
	// [MEDIA-SUMMARY] cadence so the two can be read as one picture.
	nackLogMinInterval = 5 * time.Second
)

// rtpRingBuffer is a single-writer ring buffer for caching RTP packets.
// Serialised by loopbackSession.mu, which both WriteRTP and handleNACK hold.
type rtpRingBuffer struct {
	buf  [rtpCacheSize]cachedRTPPacket
	mask uint16 // rtpCacheSize - 1, for fast modulo

	// newestSeq / initialised track the highest sequence number stored, so an
	// out-of-order write can be recognised and refused. See store.
	newestSeq   uint16
	initialised bool
}

type cachedRTPPacket struct {
	seq   uint16
	valid bool

	// payload is the serialised RTP packet (header + payload). Backed by a
	// per-slot buffer that is reused across stores rather than reallocated:
	// this is written once per outbound packet, over a thousand times a second.
	payload []byte
}

func newRTPRingBuffer() *rtpRingBuffer {
	return &rtpRingBuffer{
		mask: rtpCacheSize - 1,
	}
}

// store caches a serialized RTP packet by its sequence number.
//
// Out-of-order writes are refused. Slots are indexed by seq modulo the buffer
// size, so a retransmission arriving late — an RTX-recovered packet, say —
// carries an old sequence number that maps onto the slot of a packet 512 ahead
// of it. Storing it would evict a current packet in favour of one already
// delivered, and the NACK most likely to arrive next is for the current one.
// The cache exists to answer that NACK, so it must not overwrite itself
// backwards.
func (rb *rtpRingBuffer) store(seq uint16, raw []byte) {
	if rb.initialised && int16(seq-rb.newestSeq) <= 0 {
		return
	}
	rb.newestSeq = seq
	rb.initialised = true

	idx := seq & rb.mask
	entry := &rb.buf[idx]
	// Reuse the slot's existing allocation when it is large enough. The copy is
	// required either way — raw belongs to the caller — but the allocation is
	// not, and this runs on the media path.
	if cap(entry.payload) >= len(raw) {
		entry.payload = entry.payload[:len(raw)]
	} else {
		entry.payload = make([]byte, len(raw))
	}
	copy(entry.payload, raw)
	entry.seq = seq
	entry.valid = true
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

// trackKind is the media kind a loopback track carries. The loopback has
// exactly one track per kind — see loopbackSession.tracks.
type trackKind string

const (
	kindVideo trackKind = "video"
	kindAudio trackKind = "audio"
)

// loopbackTrack is one outbound track on the loopback PeerConnection, together
// with everything needed to rewrite inbound SFU packets onto it.
type loopbackTrack struct {
	kind   trackKind
	track  *webrtc.TrackLocalStaticRTP
	sender *webrtc.RTPSender

	// mime is the canonical Pion MimeType the track was created with. Packets
	// whose codec maps to a different mime cannot be written here — the
	// receiver negotiated one decoder for this track.
	mime string

	// boundPT / boundSSRC are what Pion assigned on the loopback side, taken
	// from the sender's parameters after SetLocalDescription. Every packet is
	// rewritten to these before being written to the track.
	boundPT   webrtc.PayloadType
	boundSSRC uint32

	// srcSSRC is the single inbound (SFU-side) SSRC pinned to this track,
	// claimed by the first packet of this kind to arrive. Later packets from
	// other SSRCs are dropped rather than interleaved: two encoders muxed onto
	// one SSRC produce a stream no decoder can make sense of.
	srcSSRC uint32

	// cache holds recent packets as written, so a NACK from the receiver can be
	// answered locally instead of waiting a round trip to the SFU.
	cache *rtpRingBuffer

	// codecSwitches counts how many times this track has been rebuilt because
	// the arriving codec did not match the one it was created with. Bounded by
	// maxCodecSwitchesPerKind so an SFU that alternates codecs cannot put the
	// loopback into a renegotiation loop.
	codecSwitches int
}

// maxCodecSwitchesPerKind bounds self-healing. One switch is the expected case
// (the initial codec was guessed from the offer, the SFU chose another); more
// than a couple means something is flapping, and continuing to renegotiate
// would be worse than settling on what we have.
const maxCodecSwitchesPerKind = 3

type loopbackSession struct {
	bridgeID        string
	logf            func(format string, args ...any)
	onLoopbackOffer func(bridgeID, sdp string)

	onRequestKeyframe func(bridgeID string, ssrc uint32)

	mu sync.Mutex
	pc *webrtc.PeerConnection

	// Exactly one track per media kind.
	//
	// This used to be one track per inbound payload type, which meant a Teams
	// offer (seven H264 PTs differing only in packetization-mode and
	// profile-level-id, plus Opus) produced eight tracks — seven of them video,
	// six of which never carried a packet. The frontend binds
	// videoElement.srcObject = event.streams[0], and a <video> renders the FIRST
	// video track in the stream, so whether anything appeared depended on which
	// track Pion happened to add first. That order comes from Go map iteration,
	// which is randomised per process: the same build rendered on one run and
	// showed nothing on the next.
	//
	// The loopback has its own payload-type space anyway — all seven H264 PTs
	// were bound to loopback PT=96 regardless — so per-PT tracks bought nothing
	// even when they worked.
	tracks map[trackKind]*loopbackTrack

	// loopbackToVdiSSRC maps our outbound SSRC back to the SFU-side SSRC, so
	// RTCP arriving from the receiver can be addressed to the right remote
	// stream.
	loopbackToVdiSSRC map[uint32]uint32

	// NACK statistics for sampled logging (avoids per-packet log spam).
	nackLocalHit  uint64 // NACKs satisfied from local cache
	nackLocalMiss uint64 // NACKs the cache could not answer
	nackTotal     uint64 // total NACK requests received

	nackLastLogTime time.Time // wall-clock of last NACK-summary log (time throttle)

	// One-time logging guard for dropped RTP PTs with no local track.
	droppedPTs map[uint8]bool

	// Track the current codecs we know about to detect when to add tracks
	ptCodecMap map[uint8]CodecInfo

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
) *loopbackSession {
	ls := &loopbackSession{
		bridgeID:          bridgeID,
		logf:              logf,
		onLoopbackOffer:   onLoopbackOffer,
		onRequestKeyframe: onRequestKeyframe,
		tracks:            make(map[trackKind]*loopbackTrack),
		loopbackToVdiSSRC: make(map[uint32]uint32),
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

	// ── Create the one video and one audio track ────────────────────────────
	// Eagerly, before any media arrives, so the offer reaches the frontend
	// while the SFU is still bringing the stream up rather than after it.
	if ls.ensureTracksLocked() > 0 {
		ls.renegotiateLocked()
	} else {
		ls.logf("[bcr_client][loopback][%s] WARNING: no usable codecs in ptCodecMap — loopback created with no tracks", ls.bridgeID)
	}
}

// ensureTracksLocked creates whichever of the video/audio tracks do not exist
// yet, guessing each kind's codec from the SDP codec map. Returns how many were
// added. MUST be called with ls.mu held.
//
// This is only a guess, and it cannot be anything better. The codec map is the
// union of every codec in the offer AND the answer, so it does not record which
// one the SFU actually selected — with the browser's full codec list that union
// holds VP8, VP9, AV1 and H264 at once. The guess is ranked by what Teams
// overwhelmingly picks in practice, and WriteRTP repairs it if the first packet
// proves it wrong, so being wrong costs one renegotiation rather than the whole
// call.
func (ls *loopbackSession) ensureTracksLocked() int {
	added := 0
	for _, kind := range []trackKind{kindVideo, kindAudio} {
		if _, exists := ls.tracks[kind]; exists {
			continue
		}
		mime, info, ok := ls.guessCodecForKindLocked(kind)
		if !ok {
			continue
		}
		if ls.addTrackLocked(kind, mime, info) {
			added++
		}
	}
	return added
}

// codecPreference ranks canonical mime types per kind, most likely first. Used
// only to choose the codec a track is created with before any media has
// arrived.
var codecPreference = map[trackKind][]string{
	kindVideo: {webrtc.MimeTypeH264, webrtc.MimeTypeVP8, webrtc.MimeTypeVP9, webrtc.MimeTypeAV1, webrtc.MimeTypeH265},
	kindAudio: {webrtc.MimeTypeOpus, webrtc.MimeTypeG722, webrtc.MimeTypePCMU, webrtc.MimeTypePCMA},
}

// guessCodecForKindLocked picks the most likely codec of the given kind out of
// the codec map. MUST be called with ls.mu held.
func (ls *loopbackSession) guessCodecForKindLocked(kind trackKind) (string, CodecInfo, bool) {
	// Index the map by canonical mime. Where several payload types map to the
	// same codec — Teams offers H264 under seven, differing only in
	// packetization-mode and profile-level-id — take the lowest, so the choice
	// is identical on every run.
	byMime := make(map[string]CodecInfo)
	pts := make([]uint8, 0, len(ls.ptCodecMap))
	for pt := range ls.ptCodecMap {
		pts = append(pts, pt)
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })

	for _, pt := range pts {
		info := ls.ptCodecMap[pt]
		k, mime, ok := canonicalCodec(info.MimeType)
		if !ok || k != kind {
			continue
		}
		if _, seen := byMime[mime]; !seen {
			byMime[mime] = info
		}
	}

	for _, mime := range codecPreference[kind] {
		if info, ok := byMime[mime]; ok {
			return mime, info, true
		}
	}
	return "", CodecInfo{}, false
}

// switchTrackCodecLocked rebuilds a track around a different codec and fires a
// fresh offer, so a wrong initial guess costs one renegotiation instead of the
// call. Returns false when the switch budget is spent. MUST be called with
// ls.mu held.
func (ls *loopbackSession) switchTrackCodecLocked(lt *loopbackTrack, newMime string, info CodecInfo) bool {
	if lt.codecSwitches >= maxCodecSwitchesPerKind {
		return false
	}

	kind := lt.kind
	switches := lt.codecSwitches + 1
	ls.logf("[bcr_client][loopback][%s] %s arrived as %s but track carries %s — rebuilding the track (switch %d/%d)",
		ls.bridgeID, kind, newMime, lt.mime, switches, maxCodecSwitchesPerKind)

	if err := ls.pc.RemoveTrack(lt.sender); err != nil {
		ls.logf("[bcr_client][loopback][%s] could not remove %s track to switch codec: %v", ls.bridgeID, kind, err)
		return false
	}
	delete(ls.loopbackToVdiSSRC, lt.boundSSRC)
	delete(ls.tracks, kind)

	if !ls.addTrackLocked(kind, newMime, info) {
		return false
	}
	// srcSSRC deliberately starts unset on the new track: a codec change
	// normally comes with a new stream, so the next packet re-pins it.
	ls.tracks[kind].codecSwitches = switches

	ls.renegotiateLocked()
	return true
}

// canonicalCodec maps an SDP codec mime ("video/H264", "audio/opus", and Teams'
// proprietary "video/X-H264UC", whose RTP payload format is standard H264) onto
// the media kind and the canonical Pion MimeType constant.
//
// RTX ("video/rtx") is deliberately not mapped: retransmissions are
// decapsulated back into their original payload type upstream in
// rawShadowSession.decapsulateRTX, so the loopback never sees an RTX packet and
// must not create a track for one.
func canonicalCodec(sdpMime string) (trackKind, string, bool) {
	lower := strings.ToLower(sdpMime)
	switch {
	case strings.Contains(lower, "rtx"):
		return "", "", false
	case strings.Contains(lower, "vp8"):
		return kindVideo, webrtc.MimeTypeVP8, true
	case strings.Contains(lower, "vp9"):
		return kindVideo, webrtc.MimeTypeVP9, true
	case strings.Contains(lower, "av1"):
		return kindVideo, webrtc.MimeTypeAV1, true
	case strings.Contains(lower, "h265"):
		return kindVideo, webrtc.MimeTypeH265, true
	case strings.Contains(lower, "h264"):
		return kindVideo, webrtc.MimeTypeH264, true
	case strings.Contains(lower, "opus"):
		return kindAudio, webrtc.MimeTypeOpus, true
	case strings.Contains(lower, "g722"):
		return kindAudio, webrtc.MimeTypeG722, true
	case strings.Contains(lower, "pcmu"):
		return kindAudio, webrtc.MimeTypePCMU, true
	case strings.Contains(lower, "pcma"):
		return kindAudio, webrtc.MimeTypePCMA, true
	default:
		return "", "", false
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
func (ls *loopbackSession) addTrackLocked(kind trackKind, mimeType string, codecInfo CodecInfo) bool {
	// Determine clock rate and channels from our CodecInfo (parsed from
	// the SDP). Fall back to standard defaults if the SDP didn't have them.
	clockRate := codecInfo.ClockRate
	if clockRate == 0 {
		if kind == kindVideo {
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

	// Track IDs are per-kind and stable. They used to embed the payload type,
	// which made them differ run to run for no benefit — and made the log look
	// like the tracks were meaningfully distinct when they were not.
	trackID := "bcr-" + string(kind)
	streamID := "stream-" + ls.bridgeID

	track, err := webrtc.NewTrackLocalStaticRTP(capability, trackID, streamID)
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to create %s track mime=%s clockRate=%d: %v",
			ls.bridgeID, kind, mimeType, clockRate, err)
		return false
	}

	sender, err := ls.pc.AddTrack(track)
	if err != nil {
		ls.logf("[bcr_client][loopback][%s] failed to add %s track to PC: %v", ls.bridgeID, kind, err)
		return false
	}

	go ls.readRTCP(sender)

	ls.tracks[kind] = &loopbackTrack{
		kind:   kind,
		track:  track,
		sender: sender,
		mime:   mimeType,
		cache:  newRTPRingBuffer(),
	}
	ls.logf("[bcr_client][loopback][%s] %s track: mime=%s clockRate=%d channels=%d (from SDP %s)",
		ls.bridgeID, kind, mimeType, clockRate, channels, codecInfo.MimeType)
	return true
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

// SyncTracksFromCodecMap adds whichever of the video/audio tracks is still
// missing after a renegotiation, and fires a fresh Pion offer if it added one.
//
// This handles the Teams 2-phase negotiation pattern where the SFU sends an
// initial audio-only answer (triggering SRTP ready + loopback creation) and
// immediately follows with a renegotiation offer adding video.
//
// With one track per kind this is a no-op in the common case — new payload
// types for a kind we already carry no longer add tracks or trigger an offer,
// so a Teams call that renegotiates repeatedly no longer walks the loopback
// through a renegotiation per payload type.
func (ls *loopbackSession) SyncTracksFromCodecMap(codecMap map[uint8]CodecInfo) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.pc == nil {
		return
	}

	// Merge new codecs in so ensureTracksLocked can see them.
	for pt, info := range codecMap {
		if _, exists := ls.ptCodecMap[pt]; !exists {
			ls.ptCodecMap[pt] = info
		}
	}

	if added := ls.ensureTracksLocked(); added > 0 {
		ls.logf("[bcr_client][loopback][%s] renegotiation added %d track(s) — new offer to frontend", ls.bridgeID, added)
		ls.renegotiateLocked()
	}
}

// WriteRTP rewrites one decrypted SFU packet onto the loopback track for its
// media kind and writes it.
//
// codec is the packet's entry from the SDP codec map, resolved by the caller —
// it is what decides which track the packet belongs to. Routing by kind rather
// than by payload type is what makes rendering deterministic; see the comment
// on loopbackSession.tracks.
func (ls *loopbackSession) WriteRTP(pkt *rtp.Packet, codec CodecInfo) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.pc == nil {
		return
	}

	kind, mime, ok := canonicalCodec(codec.MimeType)
	if !ok {
		ls.warnDropOnceLocked(pkt.Header.PayloadType, "codec %q has no loopback mapping", codec.MimeType)
		return
	}

	lt := ls.tracks[kind]
	if lt == nil {
		ls.warnDropOnceLocked(pkt.Header.PayloadType, "no %s track on the loopback", kind)
		return
	}

	// A packet whose codec differs from the one this track was negotiated with
	// cannot be written to it: the receiver has one decoder per track, and
	// feeding it another codec's payload corrupts rather than fails cleanly.
	//
	// The track's codec was guessed from the offer before any media arrived
	// (see ensureTracksLocked), so this is the expected way to discover the
	// guess was wrong — rebuild the track around what actually arrived instead
	// of dropping the call's media forever. This packet is lost either way; the
	// next ones flow once the frontend answers the new offer.
	if lt.mime != mime {
		if !ls.switchTrackCodecLocked(lt, mime, codec) {
			ls.warnDropOnceLocked(pkt.Header.PayloadType,
				"%s track carries %s, packet is %s, and the codec-switch budget is spent", kind, lt.mime, mime)
		}
		return
	}

	// boundPT/boundSSRC come from the sender's parameters, which only exist
	// once SetLocalDescription has run. Reaching here without them means the
	// offer failed to build; writing the packet anyway would put it on SSRC 0,
	// which the receiver silently discards — a failure mode indistinguishable
	// from "no media arrived".
	if lt.boundSSRC == 0 {
		ls.warnDropOnceLocked(pkt.Header.PayloadType, "%s track has no negotiated SSRC yet (loopback offer did not complete)", kind)
		return
	}

	// Pin the track to the first SSRC that actually sends for this kind. A
	// Teams video m-section advertises several SSRCs (simulcast layers, other
	// participants) but only one is live at a time; muxing two of them onto our
	// single outbound SSRC would interleave two encoders' output into a stream
	// no decoder can follow.
	srcSSRC := pkt.Header.SSRC
	if lt.srcSSRC == 0 {
		lt.srcSSRC = srcSSRC
		ls.loopbackToVdiSSRC[lt.boundSSRC] = srcSSRC
		ls.logf("[bcr_client][loopback][%s] %s track pinned to SFU SSRC=%d (PT=%d %s) → loopback SSRC=%d PT=%d",
			ls.bridgeID, kind, srcSSRC, pkt.Header.PayloadType, codec.MimeType, lt.boundSSRC, lt.boundPT)
	} else if lt.srcSSRC != srcSSRC {
		ls.warnDropOnceLocked(pkt.Header.PayloadType, "%s track is pinned to SSRC=%d, packet is from SSRC=%d", kind, lt.srcSSRC, srcSSRC)
		return
	}

	// Rewrite PayloadType and SSRC to the loopback's own negotiated values —
	// WebKit drops anything it did not negotiate.
	pkt.Header.PayloadType = uint8(lt.boundPT)
	pkt.Header.SSRC = lt.boundSSRC

	// Strip RTP header extensions: MID/RID/abs-send-time were negotiated with
	// the SFU, not with the loopback, and WebKit rejects extensions it has no
	// extmap for.
	pkt.Header.Extension = false
	pkt.Header.ExtensionProfile = 0
	pkt.Header.Extensions = nil

	// Cache before writing, so a NACK arriving immediately after (WebKit is on
	// the same machine — its NACK can beat our next packet) can be answered
	// from memory rather than by asking the SFU across the internet.
	if raw, marshalErr := pkt.Marshal(); marshalErr == nil {
		lt.cache.store(pkt.Header.SequenceNumber, raw)
	}

	if err := lt.track.WriteRTP(pkt); err != nil {
		ls.writeErrCount++
		if ls.writeErrCount == 1 || ls.writeErrCount%500 == 0 {
			ls.logf("[bcr_client][loopback][%s] %s track WriteRTP error #%d: %v",
				ls.bridgeID, kind, ls.writeErrCount, err)
		}
	} else {
		ls.writeErrCount = 0
	}
}

// warnDropOnceLocked reports the first dropped packet for a given payload type
// and stays quiet for the rest. Drops here are per-packet events on the media
// hot path; one line each would be thousands a second. MUST be called with
// ls.mu held.
func (ls *loopbackSession) warnDropOnceLocked(pt uint8, format string, args ...any) {
	if ls.droppedPTs[pt] {
		return
	}
	ls.droppedPTs[pt] = true
	ls.logf("[bcr_client][loopback][%s] dropping PT=%d and further packets like it: %s",
		ls.bridgeID, pt, fmt.Sprintf(format, args...))
}

// updateBindingsLocked extracts mapped Payload Types and SSRCs from RTPSenders.
// MUST be called with ls.mu held.
func (ls *loopbackSession) updateBindingsLocked() {
	mappings := make([]string, 0, len(ls.tracks))
	for _, kind := range []trackKind{kindVideo, kindAudio} {
		lt := ls.tracks[kind]
		if lt == nil {
			continue
		}
		params := lt.sender.GetParameters()
		if len(params.Codecs) > 0 {
			lt.boundPT = params.Codecs[0].PayloadType
		}
		if len(params.Encodings) > 0 && params.Encodings[0].SSRC != 0 {
			lt.boundSSRC = uint32(params.Encodings[0].SSRC)
			if lt.srcSSRC != 0 {
				ls.loopbackToVdiSSRC[lt.boundSSRC] = lt.srcSSRC
			}
		}
		mappings = append(mappings, fmt.Sprintf("%s=PT%d/SSRC%d", kind, lt.boundPT, lt.boundSSRC))
	}

	if len(mappings) > 0 {
		ls.logf("[bcr_client][loopback][%s] loopback bindings: %s", ls.bridgeID, strings.Join(mappings, " "))
	}
}

// trackByBoundSSRCLocked finds the track carrying a given outbound SSRC — the
// SSRC the receiver names in its RTCP. MUST be called with ls.mu held.
func (ls *loopbackSession) trackByBoundSSRCLocked(ssrc uint32) *loopbackTrack {
	for _, lt := range ls.tracks {
		if lt.boundSSRC == ssrc {
			return lt
		}
	}
	return nil
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
				// requestKeyframe logs the first PLI per SSRC and counts the
				// rest; logging here as well doubled every line.
				if ls.onRequestKeyframe != nil {
					ls.onRequestKeyframe(ls.bridgeID, targetSSRC)
				}

			case *rtcp.TransportLayerNack:
				ls.handleNACK(p)
			}
		}
	}
}

// handleNACK processes a single NACK packet from the receiver, retransmitting
// what it can from this track's local cache.
//
// Cache misses are counted but deliberately NOT forwarded to the SFU any more.
//
// Sequence numbers pass through this leg unchanged — WriteRTP rewrites the
// payload type and SSRC but keeps the SFU's sequence number — so a NACK from
// the receiver names the same number the ingress leg tracks. Which means a miss
// here is almost always a packet the SFU never delivered, and the NACK engine
// (nack.go) already has it pending, has already asked, and will ask again on
// its own timer. Forwarding was a second, uncoordinated request for the same
// packet from a different goroutine.
//
// It was also mostly theatre: the forward path was rate-limited to one NACK per
// SSRC per 200ms, so under the sustained loss where repair actually matters it
// dropped the overwhelming majority of what it was handed. The remaining case —
// a packet we did receive and write, but which the cache no longer holds — is
// not something the SFU can help with either, since it is a local eviction.
//
// The receiver's other feedback, PLI, IS still forwarded: only the SFU can
// produce a keyframe.
func (ls *loopbackSession) handleNACK(p *rtcp.TransportLayerNack) {
	// Collect all missing sequence numbers from the NACK packet.
	var allMissing []uint16
	for _, pair := range p.Nacks {
		allMissing = append(allMissing, pair.PacketList()...)
	}
	if len(allMissing) == 0 {
		return
	}

	// The whole cache lookup and retransmit runs under ls.mu. It used to take
	// the ring-buffer pointer under the lock and then read from it outside,
	// racing WriteRTP's stores into the same buffer on the SRTP goroutine.
	ls.mu.Lock()

	lt := ls.trackByBoundSSRCLocked(p.MediaSSRC)
	if lt == nil {
		// A NACK for an SSRC we do not send. Nothing to retransmit and nothing
		// sensible to forward — the SFU does not know this SSRC either.
		ls.mu.Unlock()
		return
	}

	targetSSRC := lt.srcSSRC
	if targetSSRC == 0 {
		targetSSRC = p.MediaSSRC
	}

	var localRetransmitted, cacheMisses int
	for _, seq := range allMissing {
		raw, found := lt.cache.retrieve(seq)
		if !found {
			cacheMisses++
			continue
		}
		var cachedPkt rtp.Packet
		if err := cachedPkt.Unmarshal(raw); err != nil {
			cacheMisses++
			continue
		}
		if err := lt.track.WriteRTP(&cachedPkt); err != nil {
			cacheMisses++
			continue
		}
		localRetransmitted++
	}

	ls.nackTotal += uint64(len(allMissing))
	ls.nackLocalHit += uint64(localRetransmitted)
	ls.nackLocalMiss += uint64(cacheMisses)
	currentTotal, hitTotal, missTotal := ls.nackTotal, ls.nackLocalHit, ls.nackLocalMiss

	// Time-throttled logging: at most once every nackLogMinInterval, regardless of
	// NACK volume. Under heavy loss the old every-50-NACKs rule produced ~1000s of
	// lines/sec, whose synchronous file+stdout writes could starve the SRTP read
	// loop and cause MORE loss. Keep the counters accurate; just log sparsely.
	shouldLog := time.Since(ls.nackLastLogTime) >= nackLogMinInterval
	if shouldLog {
		ls.nackLastLogTime = time.Now()
	}

	kind := lt.kind
	ls.mu.Unlock()

	if shouldLog {
		ls.logf("[bcr_client][loopback][%s] %s NACK: total=%d servedLocally=%d cacheMiss=%d hitRate=%.1f%% (loopback SSRC %d → SFU SSRC %d)",
			ls.bridgeID, kind, currentTotal, hitTotal, missTotal,
			float64(hitTotal)/float64(max(currentTotal, 1))*100.0,
			p.MediaSSRC, targetSSRC)
	}
}
