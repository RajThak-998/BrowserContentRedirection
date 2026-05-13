package engine

// diag.go — Deep observability for the WebRTC/SRTP media pipeline.
//
// GOAL: produce enough structured log lines to answer, from logs alone:
//   1. What codec/PT topology was negotiated?
//   2. Is PT=99 RTX / FEC / simulcast?
//   3. Which SSRC belongs to which media role?
//   4. Are packets being lost?
//   5. Are NACKs generated?
//   6. Are retransmissions arriving?
//   7. Are keyframes actually arriving?
//   8. Is RTP continuous but decoder state broken?
//   9. Are renegotiations changing mappings?
//  10. Is Teams dynamically changing stream topology?
//
// DO NOT change media behaviour. Observability only.

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ─── Per-SSRC Stream State ────────────────────────────────────────────────────

// streamRole classifies an SSRC by its media role.
type streamRole string

const (
	roleAudio      streamRole = "audio"
	rolePrimVideo  streamRole = "primary-video"
	roleRTXVideo   streamRole = "rtx-video"
	roleUnknown    streamRole = "unknown"
)

// ssrcStats holds per-SSRC diagnostic counters updated on every inbound packet.
type ssrcStats struct {
	mu sync.Mutex

	ssrc       uint32
	pt         uint8
	codec      string
	role       streamRole
	firstSeen  time.Time
	lastSeen   time.Time
	pktCount   int64
	byteCount  int64 // payload bytes (post-decrypt)

	// Sequence tracking for gap/reorder detection.
	highestSeq   uint16
	seqInitiated bool
	totalLost    int64
	totalReorder int64
	totalDup     int64

	// Keyframe counters (video only).
	keyframeCount int64
	lastKeyframe  time.Time

	// NACK counters (if NACK generation is implemented).
	nackSent      int64
	nackRecovered int64
}

// ssrcRegistry is a thread-safe map from SSRC → ssrcStats.
type ssrcRegistry struct {
	mu    sync.Mutex
	stats map[uint32]*ssrcStats
}

func newSSRCRegistry() *ssrcRegistry {
	return &ssrcRegistry{stats: make(map[uint32]*ssrcStats)}
}

// getOrCreate returns the stats object for ssrc, creating it on first call.
// Returns (stats, isNew).
func (r *ssrcRegistry) getOrCreate(ssrc uint32) (*ssrcStats, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stats[ssrc]
	if !ok {
		s = &ssrcStats{ssrc: ssrc, firstSeen: time.Now()}
		r.stats[ssrc] = s
	}
	return s, !ok
}

// snapshot returns a copy of all stats (for the summary ticker).
func (r *ssrcRegistry) snapshot() []*ssrcStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*ssrcStats, 0, len(r.stats))
	for _, s := range r.stats {
		s.mu.Lock()
		cp := *s
		s.mu.Unlock()
		cp2 := cp
		out = append(out, &cp2)
	}
	return out
}

// ─── RTX Mapping ─────────────────────────────────────────────────────────────

// rtxMap stores PT → apt mappings discovered from fmtp:apt=<N> lines in SDP.
// PT=99 → apt=107 means PT 99 is RTX retransmitting PT 107.
type rtxMap struct {
	mu      sync.Mutex
	apt     map[uint8]uint8 // rtxPT → mediaPT
	reverse map[uint8]uint8 // mediaPT → rtxPT
}

func newRTXMap() *rtxMap {
	return &rtxMap{apt: make(map[uint8]uint8), reverse: make(map[uint8]uint8)}
}

// set records rtxPT retransmits mediaPT.
func (m *rtxMap) set(rtxPT, mediaPT uint8) {
	m.mu.Lock()
	m.apt[rtxPT] = mediaPT
	m.reverse[mediaPT] = rtxPT
	m.mu.Unlock()
}

// isRTX returns (mediaPT, true) if pt is an RTX payload type.
func (m *rtxMap) isRTX(pt uint8) (uint8, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	apt, ok := m.apt[pt]
	return apt, ok
}

// ─── NACK State ──────────────────────────────────────────────────────────────

// nackState tracks missing sequence numbers per SSRC for NACK generation readiness.
type nackState struct {
	mu      sync.Mutex
	missing map[uint32]map[uint16]bool // ssrc → set of missing seqs
}

func newNACKState() *nackState {
	return &nackState{missing: make(map[uint32]map[uint16]bool)}
}

// recordGap marks sequences [expected, got) as missing for ssrc.
func (n *nackState) recordGap(ssrc uint32, expected, got uint16) []uint16 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.missing[ssrc] == nil {
		n.missing[ssrc] = make(map[uint16]bool)
	}
	var seqs []uint16
	for seq := expected; seq != got; seq++ {
		if !n.missing[ssrc][seq] {
			n.missing[ssrc][seq] = true
			seqs = append(seqs, seq)
		}
	}
	return seqs
}

// recover marks a sequence as recovered (arrived via RTX or late delivery).
func (n *nackState) recover(ssrc uint32, seq uint16) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	m := n.missing[ssrc]
	if m == nil {
		return false
	}
	if m[seq] {
		delete(m, seq)
		return true
	}
	return false
}

// ─── SDP Dump Helpers (called after every SDP parse) ─────────────────────────

// DumpPTMap logs the full PT → codec topology using [PTMAP] prefix.
// Called after ParsePTCodecMap, so it receives the raw map + the full SDP
// to extract RTX apt mappings and extmaps inline.
func DumpPTMap(sdp, context string, ptMap map[uint8]CodecInfo, logf func(string, ...any)) {
	logf("[PTMAP] ── %s ──────────────────────────────────", context)
	for pt, ci := range ptMap {
		logf("[PTMAP] PT=%d mime=%s clock=%d", pt, ci.MimeType, ci.ClockRate)
	}

	// Extract RTX / FEC mappings from fmtp:apt= lines.
	rtxMappings := extractFmtpAPT(sdp)
	for rtxPT, mediaPT := range rtxMappings {
		role := "RTX"
		logf("[PTMAP] PT=%d mime=video/rtx fmtp=apt=%d  ← [RTX] PT=%d retransmits PT=%d",
			rtxPT, mediaPT, rtxPT, mediaPT)
		_ = role
	}
	if len(rtxMappings) == 0 {
		logf("[PTMAP] (no RTX apt mappings found)")
	}

	// Extmap dump.
	extmaps := extractExtmaps(sdp)
	for id, uri := range extmaps {
		logf("[EXTMAP] id=%d uri=%s", id, uri)
		if strings.Contains(uri, "transport-wide-cc") || strings.Contains(uri, "transport-cc") {
			logf("[TWCC] extmap id=%d present in %s", id, context)
		}
	}
	logf("[PTMAP] ── end %s ──────────────────────────────", context)
}

// extractFmtpAPT parses "a=fmtp:<PT> apt=<N>" lines and returns rtxPT→mediaPT.
func extractFmtpAPT(sdp string) map[uint8]uint8 {
	out := make(map[uint8]uint8)
	for _, line := range strings.Split(sdp, "\n") {
		bare := strings.TrimRight(strings.TrimSpace(line), "\r")
		if !strings.HasPrefix(bare, "a=fmtp:") {
			continue
		}
		rest := bare[len("a=fmtp:"):]
		spaceIdx := strings.Index(rest, " ")
		if spaceIdx < 0 {
			continue
		}
		ptStr := rest[:spaceIdx]
		params := rest[spaceIdx+1:]
		var pt uint8
		if _, err := fmt.Sscanf(ptStr, "%d", &pt); err != nil {
			continue
		}
		for _, kv := range strings.Split(params, ";") {
			kv = strings.TrimSpace(kv)
			if strings.HasPrefix(kv, "apt=") {
				var apt uint8
				if _, err := fmt.Sscanf(kv[4:], "%d", &apt); err == nil {
					out[pt] = apt
				}
			}
		}
	}
	return out
}

// extractExtmaps parses "a=extmap:<id>[ direction] <uri>" lines.
func extractExtmaps(sdp string) map[uint8]string {
	out := make(map[uint8]string)
	for _, line := range strings.Split(sdp, "\n") {
		bare := strings.TrimRight(strings.TrimSpace(line), "\r")
		if !strings.HasPrefix(bare, "a=extmap:") {
			continue
		}
		rest := bare[len("a=extmap:"):]
		parts := strings.Fields(rest)
		if len(parts) < 2 {
			continue
		}
		// parts[0] may be "3" or "3/sendrecv"
		idStr := strings.SplitN(parts[0], "/", 2)[0]
		var id uint8
		if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
			continue
		}
		out[id] = parts[len(parts)-1]
	}
	return out
}

// DumpRTCPComposition logs the human-readable packet list for a compound RTCP.
func DumpRTCPComposition(direction, label string, pktDescs []string, logf func(string, ...any)) {
	logf("[RTCP-%s] %s packets=[%s]", direction, label, strings.Join(pktDescs, ", "))
}

// ─── H264 NAL Type Detection ──────────────────────────────────────────────────

// H264NALType classifies an RTP payload as H264 NAL type.
type H264NALType string

const (
	H264Single H264NALType = "single"
	H264STAPA  H264NALType = "STAP-A"
	H264FUA    H264NALType = "FU-A"
	H264IDR    H264NALType = "IDR"
	H264SPS    H264NALType = "SPS"
	H264PPS    H264NALType = "PPS"
	H264Other  H264NALType = "other"
)

// InspectH264NAL classifies RTP payload bytes for H264 media type awareness.
// Returns (nalType, isKeyframe).
// payload is the raw RTP payload (after header).
func InspectH264NAL(payload []byte) (H264NALType, bool) {
	if len(payload) < 1 {
		return H264Other, false
	}
	nalByte := payload[0] & 0x1F
	switch {
	case nalByte == 24: // STAP-A
		return H264STAPA, false
	case nalByte == 28: // FU-A
		if len(payload) < 2 {
			return H264FUA, false
		}
		startBit := (payload[1] >> 7) & 1
		fuNAL := payload[1] & 0x1F
		if startBit == 1 && fuNAL == 5 {
			return H264IDR, true // IDR inside FU-A
		}
		if startBit == 1 && fuNAL == 7 {
			return H264SPS, false
		}
		if startBit == 1 && fuNAL == 8 {
			return H264PPS, false
		}
		return H264FUA, false
	case nalByte == 5: // IDR
		return H264IDR, true
	case nalByte == 7: // SPS
		return H264SPS, false
	case nalByte == 8: // PPS
		return H264PPS, false
	default:
		return H264Other, false
	}
}

// ─── Media Summary ────────────────────────────────────────────────────────────

// mediaSummaryStats aggregates per-role counters for the 5-second summary tick.
type mediaSummaryStats struct {
	packets   int64
	bytes     int64
	keyframes int64
	lost      int64
	nack      int64
	recovered int64
	dropped   int64
}

// EmitMediaSummary logs a compact [MEDIA-SUMMARY] block from the ssrcRegistry snapshot.
// rtxDropped is passed explicitly because it lives on the session, not the registry.
func EmitMediaSummary(
	bridgeID string,
	reg *ssrcRegistry,
	ptMap map[uint8]CodecInfo,
	rtxMap *rtxMap,
	rtcpRR, rtcpREMB, rtcpPLI, rtcpNACK int64,
	intervalSec float64,
	logf func(string, ...any),
) {
	snaps := reg.snapshot()

	byRole := make(map[streamRole]*mediaSummaryStats)
	for _, s := range snaps {
		r := s.role
		if _, ok := byRole[r]; !ok {
			byRole[r] = &mediaSummaryStats{}
		}
		st := byRole[r]
		st.packets += s.pktCount
		st.bytes += s.byteCount
		st.keyframes += s.keyframeCount
		st.lost += s.totalLost
		st.nack += s.nackSent
		st.recovered += s.nackRecovered
	}

	logf("[MEDIA-SUMMARY] bridgeId=%s interval=%.0fs ──────────────────", bridgeID, intervalSec)
	for _, role := range []streamRole{roleAudio, rolePrimVideo, roleRTXVideo, roleUnknown} {
		st, ok := byRole[role]
		if !ok {
			continue
		}
		kbps := float64(st.bytes*8) / (intervalSec * 1000)
		logf("[MEDIA-SUMMARY] %-16s pkts=%-6d bitrate=%.0fkbps keyframes=%d lost=%d nack=%d recovered=%d",
			role, st.packets, kbps, st.keyframes, st.lost, st.nack, st.recovered)
	}
	logf("[MEDIA-SUMMARY] rtcp: rr=%d remb=%d pli=%d nack=%d",
		rtcpRR, rtcpREMB, rtcpPLI, rtcpNACK)
	logf("[MEDIA-SUMMARY] ──────────────────────────────────────────────")
}
