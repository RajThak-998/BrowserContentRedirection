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
// Every field here is read by EmitMediaSummary or by getPTForSSRC. Fields that
// were only ever written have been removed: they were updated once per packet
// on the SRTP hot path (lastSeen took a clock reading for every packet at
// ~1000/s) and never appeared in any output.
type ssrcStats struct {
	mu sync.Mutex

	pt        uint8
	role      streamRole
	pktCount  int64
	byteCount int64 // payload bytes (post-decrypt)

	// Sequence tracking for gap detection.
	highestSeq   uint16
	seqInitiated bool

	// totalLost counts sequence numbers the NACK engine gave up on — packets
	// that are genuinely gone. It is deliberately NOT incremented when a gap is
	// first noticed: most gaps close again, either because the packet was
	// merely reordered or because retransmission recovered it, and counting
	// them at detection time made the reported loss figure large enough to be
	// useless for judging anything.
	totalLost int64

	// Keyframe counter (video only).
	keyframeCount int64

	// NACK counters. nackSent counts requests actually put on the wire,
	// retries included. reordered counts gaps that closed on their own inside
	// the reorder grace, without a request ever being sent — worth seeing
	// separately, because a high reordered count with low loss means the link
	// is fine and only the ordering is untidy.
	nackSent      int64
	nackRecovered int64
	reordered     int64
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
		s = &ssrcStats{}
		r.stats[ssrc] = s
	}
	return s, !ok
}

// ssrcSnapshot is a lock-free copy of one SSRC's counters, taken for the
// summary ticker. It is a distinct type from ssrcStats because `*s` would copy
// the embedded mutex along with the fields (go vet: "assignment copies lock
// value"), handing the caller a struct carrying a mutex in whatever state it
// happened to be in.
type ssrcSnapshot struct {
	role          streamRole
	pktCount      int64
	byteCount     int64
	totalLost     int64
	keyframeCount int64
	nackSent      int64
	nackRecovered int64
	reordered     int64
}

// snapshot returns a copy of all stats (for the summary ticker).
func (r *ssrcRegistry) snapshot() []ssrcSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ssrcSnapshot, 0, len(r.stats))
	for _, s := range r.stats {
		s.mu.Lock()
		out = append(out, ssrcSnapshot{
			role:          s.role,
			pktCount:      s.pktCount,
			byteCount:     s.byteCount,
			totalLost:     s.totalLost,
			keyframeCount: s.keyframeCount,
			nackSent:      s.nackSent,
			nackRecovered: s.nackRecovered,
			reordered:     s.reordered,
		})
		s.mu.Unlock()
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

// NACK state lives in nack.go. It used to be here, as a bare per-SSRC set of
// missing sequence numbers with no expiry, no retry and no reorder tolerance —
// see the header comment on nack.go for what was wrong with that and why the
// replacement is shaped the way it is.

// ─── SDP Dump Helpers (called after every SDP parse) ─────────────────────────

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

// ─── H264 NAL Type Detection ──────────────────────────────────────────────────

// IsH264Keyframe reports whether an RTP payload starts an IDR (keyframe),
// handling both a bare NAL unit and the first fragment of a FU-A.
//
// This used to also return an H264NALType naming the packetisation form, but
// its one caller discarded that value, so the type and its seven constants
// existed only to be thrown away.
func IsH264Keyframe(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	switch nalType := payload[0] & 0x1F; nalType {
	case 5: // IDR
		return true
	case 28: // FU-A — the IDR marker is in the fragmentation header
		if len(payload) < 2 {
			return false
		}
		startBit := (payload[1] >> 7) & 1
		return startBit == 1 && payload[1]&0x1F == 5
	default:
		return false
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
	reordered int64
}

// summaryBaseline holds the previous tick's cumulative totals so each block can
// report what happened during the interval rather than since the session began.
type summaryBaseline struct {
	byRole   map[streamRole]mediaSummaryStats
	rr       int64
	remb     int64
	pli      int64
	nack     int64
}

func newSummaryBaseline() *summaryBaseline {
	return &summaryBaseline{byRole: make(map[streamRole]mediaSummaryStats)}
}

// EmitMediaSummary logs a compact [MEDIA-SUMMARY] block from the ssrcRegistry snapshot.
//
// The per-SSRC counters in the registry are cumulative for the life of the
// session. The block is labelled with an interval, so it subtracts the previous
// tick's totals and reports the delta — otherwise every number only ever grows
// and the "bitrate" is lifetime-bytes÷interval, which climbs steadily no matter
// what the stream is actually doing. Cumulative totals are still shown, in
// parentheses, because absolute loss over a call is worth seeing too.
func EmitMediaSummary(
	bridgeID string,
	reg *ssrcRegistry,
	ptMap map[uint8]CodecInfo,
	rtxMap *rtxMap,
	rtcpRR, rtcpREMB, rtcpPLI, rtcpNACK int64,
	intervalSec float64,
	base *summaryBaseline,
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
		st.reordered += s.reordered
	}

	logf("[MEDIA-SUMMARY] bridgeId=%s last %.0fs (totals in parens) ─────", bridgeID, intervalSec)
	for _, role := range []streamRole{roleAudio, rolePrimVideo, roleRTXVideo, roleUnknown} {
		st, ok := byRole[role]
		if !ok {
			continue
		}
		prev := base.byRole[role]
		base.byRole[role] = *st

		dPackets := st.packets - prev.packets
		dBytes := st.bytes - prev.bytes
		dKeyframes := st.keyframes - prev.keyframes
		dLost := st.lost - prev.lost
		dNack := st.nack - prev.nack
		dRecovered := st.recovered - prev.recovered
		dReordered := st.reordered - prev.reordered

		kbps := float64(dBytes*8) / (intervalSec * 1000)
		// lost= is now unrecoverable loss only (the NACK engine gave up).
		// reordered= is gaps that closed on their own without costing a
		// request. Reading them together says whether the link is lossy or
		// merely out of order — the old single lost= counter conflated both.
		logf("[MEDIA-SUMMARY] %-16s pkts=%-5d bitrate=%-6.0fkbps keyframes=%d lost=%d(%d) nack=%d(%d) recovered=%d(%d) reordered=%d(%d)",
			role, dPackets, kbps, dKeyframes,
			dLost, st.lost, dNack, st.nack, dRecovered, st.recovered, dReordered, st.reordered)
	}
	logf("[MEDIA-SUMMARY] rtcp tx: rr=%d remb=%d pli=%d nack=%d",
		rtcpRR-base.rr, rtcpREMB-base.remb, rtcpPLI-base.pli, rtcpNACK-base.nack)
	base.rr, base.remb, base.pli, base.nack = rtcpRR, rtcpREMB, rtcpPLI, rtcpNACK
	logf("[MEDIA-SUMMARY] ──────────────────────────────────────────────")
}
