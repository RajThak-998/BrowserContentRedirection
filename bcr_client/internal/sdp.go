package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pion/webrtc/v4"
)

var (
	iceUfragRe       = regexp.MustCompile(`(?m)^a=ice-ufrag:(.+)$`)
	icePwdRe         = regexp.MustCompile(`(?m)^a=ice-pwd:(.+)$`)
	fingerprintRe    = regexp.MustCompile(`(?m)^a=fingerprint:(.+)$`)
	candidateLineRe  = regexp.MustCompile(`(?m)^(a=candidate:[^\r\n]+)`)
	rtpMapLineRe     = regexp.MustCompile(`^a=rtpmap:(\d+)\s+([^/\s]+)/(\d+)(?:/(\d+))?`)
	fmtpLineRe       = regexp.MustCompile(`^a=fmtp:(\d+)\s+(.+)`)
)

// ExtractShadowCredentials parses ICE and DTLS credentials from a Pion-generated
// local description SDP. Trailing \r is stripped to handle CRLF line endings
// from browser-originated SDPs that may propagate through the pipeline.
func ExtractShadowCredentials(sdp string) (ufrag, pwd, fingerprint string) {
	if m := iceUfragRe.FindStringSubmatch(sdp); len(m) > 1 {
		ufrag = strings.TrimRight(m[1], "\r")
	}
	if m := icePwdRe.FindStringSubmatch(sdp); len(m) > 1 {
		pwd = strings.TrimRight(m[1], "\r")
	}
	if m := fingerprintRe.FindStringSubmatch(sdp); len(m) > 1 {
		fingerprint = strings.TrimRight(m[1], "\r")
	}
	return
}

// normalizeSDP makes a browser SDP compatible with Pion's strict RFC 8829 parser.
// Currently handles:
//   - Missing a=mid on m= sections. Legacy VDI browsers omit it. Pion requires it.
//     Sequential integer values (0, 1, 2…) are injected immediately after each m= line.
func normalizeSDP(sdp string) string {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	// Pass 1: locate each m= section and whether it already has a=mid.
	type secInfo struct {
		mIdx   int
		hasMid bool
	}
	var sections []secInfo
	for i, line := range lines {
		bare := strings.TrimRight(line, "\r")
		if strings.HasPrefix(bare, "m=") {
			sections = append(sections, secInfo{mIdx: i})
		} else if len(sections) > 0 && strings.HasPrefix(bare, "a=mid:") {
			sections[len(sections)-1].hasMid = true
		}
	}

	// Build set of line indices that need a=mid injected after them.
	inject := make(map[int]int, len(sections)) // lineIdx → midValue
	for i, s := range sections {
		if !s.hasMid {
			inject[s.mIdx] = i
		}
	}
	if len(inject) == 0 {
		return sdp // fast path
	}

	// Pass 2: rebuild, inserting a=mid:{n} right after each affected m= line.
	out := make([]string, 0, len(lines)+len(inject))
	for i, line := range lines {
		out = append(out, line)
		if midVal, ok := inject[i]; ok {
			out = append(out, fmt.Sprintf("a=mid:%d", midVal))
		}
	}
	return strings.Join(out, sep)
}

// sdpCodecEntry pairs Pion codec parameters with their media type for registration.
type sdpCodecEntry struct {
	params    webrtc.RTPCodecParameters
	codecType webrtc.RTPCodecType
}

// sdpInternalCodecs are handled by Pion's interceptor pipeline; manual
// registration causes PT conflicts and must be skipped.
var sdpInternalCodecs = map[string]bool{
	"ulpfec": true, "flexfec-03": true, "cn": true,
}

// parseSDPCodecs extracts all RTP codec definitions from a browser offer SDP.
// The returned entries are suitable for mediaEngine.RegisterCodec so that the
// shadow PC's MediaEngine exactly mirrors what the browser has advertised,
// including H.264 profile variants and VP9 profiles absent from Pion's defaults.
func parseSDPCodecs(sdp string) []sdpCodecEntry {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	// Build a global PT→fmtp map (PTs are unique within a valid SDP).
	fmtpMap := map[uint8]string{}
	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")
		if m := fmtpLineRe.FindStringSubmatch(bare); m != nil {
			if pt, err := strconv.ParseUint(m[1], 10, 8); err == nil {
				fmtpMap[uint8(pt)] = strings.TrimRight(m[2], "\r")
			}
		}
	}

	var entries []sdpCodecEntry
	currentType := webrtc.RTPCodecTypeAudio

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		if strings.HasPrefix(bare, "m=audio") {
			currentType = webrtc.RTPCodecTypeAudio
			continue
		}
		if strings.HasPrefix(bare, "m=video") {
			currentType = webrtc.RTPCodecTypeVideo
			continue
		}

		m := rtpMapLineRe.FindStringSubmatch(bare)
		if m == nil {
			continue
		}

		pt64, err := strconv.ParseUint(m[1], 10, 8)
		if err != nil {
			continue
		}
		codecName := m[2]
		clockRate, err := strconv.ParseUint(m[3], 10, 32)
		if err != nil {
			continue
		}
		var channels uint16
		if m[4] != "" {
			if ch, err2 := strconv.ParseUint(m[4], 10, 16); err2 == nil {
				channels = uint16(ch)
			}
		}

		mimePrefix := "audio/"
		if currentType == webrtc.RTPCodecTypeVideo {
			mimePrefix = "video/"
		}

		entries = append(entries, sdpCodecEntry{
			params: webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:    mimePrefix + codecName,
					ClockRate:   uint32(clockRate),
					Channels:    channels,
					SDPFmtpLine: fmtpMap[uint8(pt64)],
				},
				PayloadType: webrtc.PayloadType(pt64),
			},
			codecType: currentType,
		})
	}
	return entries
}

// ExtractShadowCandidates returns a deduplicated slice of full "a=candidate:..."
// lines from a Pion-generated local description SDP. In BUNDLE mode Pion repeats
// the same candidates in every m= section; duplicates are collapsed here so the
// JS munge injects each candidate exactly once.
func ExtractShadowCandidates(sdp string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range candidateLineRe.FindAllStringSubmatch(sdp, -1) {
		cand := strings.TrimRight(m[1], "\r")
		if cand != "" && !seen[cand] {
			seen[cand] = true
			out = append(out, cand)
		}
	}
	return out
}

// ─── SDP Alignment Helpers ──────────────────────────────────────────────────

var (
	mLineRe     = regexp.MustCompile(`^m=(\w+)\s+`)
	directionRe = regexp.MustCompile(`^a=(sendrecv|recvonly|sendonly|inactive)\s*$`)
	rtcpFbRe    = regexp.MustCompile(`^a=rtcp-fb:(\d+)\s+(.+)`)
	bundleRe    = regexp.MustCompile(`(?m)^a=group:BUNDLE\s+(.+)$`)
)

// ParseOfferMediaSections extracts each m= section from an SDP, returning
// the kind (audio/video/application), direction, and mid for each.
// This is used to create matching transceivers on the shadow PC.
func ParseOfferMediaSections(sdp string) []MediaSection {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	var sections []MediaSection
	currentIdx := -1

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		if m := mLineRe.FindStringSubmatch(bare); m != nil {
			sections = append(sections, MediaSection{
				Kind:      m[1],
				Direction: "sendrecv", // default per RFC 4566
			})
			currentIdx = len(sections) - 1
			continue
		}

		if currentIdx < 0 {
			continue
		}

		if strings.HasPrefix(bare, "a=mid:") {
			sections[currentIdx].Mid = strings.TrimRight(strings.TrimPrefix(bare, "a=mid:"), "\r")
		}

		if m := directionRe.FindStringSubmatch(bare); m != nil {
			sections[currentIdx].Direction = m[1]
		}
	}

	return sections
}

// ParseSDPCodecsStrict extracts codecs from a browser offer SDP with exact
// payload type preservation AND rtcp-fb parameters. Unlike parseSDPCodecs,
// this does NOT rely on RegisterDefaultCodecs — the returned entries carry
// the exact PT assignments from the browser's offer so the shadow PC produces
// a structurally identical offer.
func ParseSDPCodecsStrict(sdp string) []sdpCodecEntry {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	// Pass 1: Build PT → fmtp map.
	fmtpMap := map[uint8]string{}
	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")
		if m := fmtpLineRe.FindStringSubmatch(bare); m != nil {
			if pt, err := strconv.ParseUint(m[1], 10, 8); err == nil {
				fmtpMap[uint8(pt)] = strings.TrimRight(m[2], "\r")
			}
		}
	}

	// Pass 2: Build PT → []RTCPFeedback map.
	fbMap := map[uint8][]webrtc.RTCPFeedback{}
	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")
		if m := rtcpFbRe.FindStringSubmatch(bare); m != nil {
			if pt, err := strconv.ParseUint(m[1], 10, 8); err == nil {
				fbVal := strings.TrimRight(m[2], "\r")
				parts := strings.SplitN(fbVal, " ", 2)
				fb := webrtc.RTCPFeedback{Type: parts[0]}
				if len(parts) > 1 {
					fb.Parameter = parts[1]
				}
				fbMap[uint8(pt)] = append(fbMap[uint8(pt)], fb)
			}
		}
	}

	// Pass 3: Extract codec entries.
	var entries []sdpCodecEntry
	currentType := webrtc.RTPCodecTypeAudio

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		if strings.HasPrefix(bare, "m=audio") {
			currentType = webrtc.RTPCodecTypeAudio
			continue
		}
		if strings.HasPrefix(bare, "m=video") {
			currentType = webrtc.RTPCodecTypeVideo
			continue
		}

		m := rtpMapLineRe.FindStringSubmatch(bare)
		if m == nil {
			continue
		}

		pt64, err := strconv.ParseUint(m[1], 10, 8)
		if err != nil {
			continue
		}
		codecName := m[2]
		clockRate, err := strconv.ParseUint(m[3], 10, 32)
		if err != nil {
			continue
		}
		var channels uint16
		if m[4] != "" {
			if ch, err2 := strconv.ParseUint(m[4], 10, 16); err2 == nil {
				channels = uint16(ch)
			}
		}

		mimePrefix := "audio/"
		if currentType == webrtc.RTPCodecTypeVideo {
			mimePrefix = "video/"
		}

		pt := uint8(pt64)
		entries = append(entries, sdpCodecEntry{
			params: webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:     mimePrefix + codecName,
					ClockRate:    uint32(clockRate),
					Channels:     channels,
					SDPFmtpLine:  fmtpMap[pt],
					RTCPFeedback: fbMap[pt],
				},
				PayloadType: webrtc.PayloadType(pt),
			},
			codecType: currentType,
		})
	}
	return entries
}

// ExtractAnswerMids returns the ordered list of a=mid: values from an SDP.
func ExtractAnswerMids(sdp string) []string {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	var mids []string
	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")
		if strings.HasPrefix(bare, "a=mid:") {
			mids = append(mids, strings.TrimRight(strings.TrimPrefix(bare, "a=mid:"), "\r"))
		}
	}
	return mids
}

// TranslateAnswerMids rewrites an answer SDP so that its mid references match
// the shadow's offer mids instead of the browser's. This is needed when the
// browser assigns mids like "audio", "video" but Pion assigns "0", "1".
// The remapping is positional: answer's first mid → shadow's first mid, etc.
func TranslateAnswerMids(answerSDP, shadowOfferSDP string) string {
	answerMids := ExtractAnswerMids(answerSDP)
	shadowMids := ExtractAnswerMids(shadowOfferSDP)

	if len(answerMids) == 0 || len(shadowMids) == 0 {
		return answerSDP // nothing to translate
	}

	// Build positional remap: answerMid[i] → shadowMid[i].
	remap := make(map[string]string)
	needsTranslation := false
	for i := 0; i < len(answerMids) && i < len(shadowMids); i++ {
		if answerMids[i] != shadowMids[i] {
			needsTranslation = true
		}
		remap[answerMids[i]] = shadowMids[i]
	}

	if !needsTranslation {
		return answerSDP // mids already match
	}

	sep := "\r\n"
	if !strings.Contains(answerSDP, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(answerSDP, sep)
	out := make([]string, 0, len(lines))

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		// Remap a=mid: lines.
		if strings.HasPrefix(bare, "a=mid:") {
			oldMid := strings.TrimRight(strings.TrimPrefix(bare, "a=mid:"), "\r")
			if newMid, ok := remap[oldMid]; ok {
				out = append(out, "a=mid:"+newMid)
				continue
			}
		}

		// Remap a=group:BUNDLE line.
		if strings.HasPrefix(bare, "a=group:BUNDLE") {
			if m := bundleRe.FindStringSubmatch(bare); m != nil {
				oldMids := strings.Fields(strings.TrimRight(m[1], "\r"))
				newMids := make([]string, len(oldMids))
				for i, om := range oldMids {
					if nm, ok := remap[om]; ok {
						newMids[i] = nm
					} else {
						newMids[i] = om
					}
				}
				out = append(out, "a=group:BUNDLE "+strings.Join(newMids, " "))
				continue
			}
		}

		out = append(out, line)
	}

	return strings.Join(out, sep)
}

// ScrubGhostPayloadTypes removes any RTP payload types from the answer SDP that
// were not present in the original offer SDP. This prevents Pion's MediaEngine
// from panicking with "payload type not found" when remote servers dynamically
// inject unregistered codecs like RTX or ULPFEC during renegotiation.
func ScrubGhostPayloadTypes(answerSDP, offerSDP string) string {
	offerValidPTs := make(map[string]bool)
	for _, m := range rtpMapLineRe.FindAllStringSubmatch(offerSDP, -1) {
		offerValidPTs[m[1]] = true
	}

	sep := "\r\n"
	if !strings.Contains(answerSDP, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(answerSDP, sep)
	var out []string

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		// 1. Scrub m= lines (e.g., "m=video 9 UDP/TLS/RTP/SAVPF 102 120")
		if strings.HasPrefix(bare, "m=") {
			parts := strings.Split(bare, " ")
			if len(parts) >= 4 { // m=video port proto pt1 pt2...
				validParts := parts[:3]
				for _, pt := range parts[3:] {
					if offerValidPTs[pt] {
						validParts = append(validParts, pt)
					}
				}
				// Edge case: if we removed ALL payload types, leave a dummy to prevent SDP syntax error
				if len(validParts) == 3 && len(parts) > 3 {
					validParts = append(validParts, parts[3]) 
				}
				out = append(out, strings.Join(validParts, " "))
				continue
			}
		}

		// 2. Scrub a=rtpmap, a=fmtp, a=rtcp-fb
		if m := rtpMapLineRe.FindStringSubmatch(bare); m != nil {
			if !offerValidPTs[m[1]] {
				continue // strip
			}
		} else if m := fmtpLineRe.FindStringSubmatch(bare); m != nil {
			if !offerValidPTs[m[1]] {
				continue // strip
			}
		} else if m := rtcpFbRe.FindStringSubmatch(bare); m != nil {
			if !offerValidPTs[m[1]] {
				continue // strip
			}
		}

		out = append(out, line)
	}

	return strings.Join(out, sep)
}
