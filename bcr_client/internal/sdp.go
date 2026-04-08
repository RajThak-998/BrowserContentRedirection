package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pion/webrtc/v4"
)

var (
	iceUfragRe    = regexp.MustCompile(`(?m)^a=ice-ufrag:(.+)$`)
	icePwdRe      = regexp.MustCompile(`(?m)^a=ice-pwd:(.+)$`)
	fingerprintRe = regexp.MustCompile(`(?m)^a=fingerprint:(.+)$`)
	rtpMapLineRe  = regexp.MustCompile(`^a=rtpmap:(\d+)\s+([^/\s]+)/(\d+)(?:/(\d+))?`)
	fmtpLineRe    = regexp.MustCompile(`^a=fmtp:(\d+)\s+(.+)`)
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
	"rtx": true, "red": true, "ulpfec": true, "flexfec-03": true, "cn": true,
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
		if sdpInternalCodecs[strings.ToLower(codecName)] {
			continue
		}
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

