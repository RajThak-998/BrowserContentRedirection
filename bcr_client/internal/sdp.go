package engine

// sdp.go — Minimal SDP parsing utilities for the raw transport layer.
//
// This file intentionally contains NO SDP generation, codec alignment, or
// PeerConnection-level SDP manipulation. The Go daemon treats all SDP as opaque
// signaling text; only the small set of values needed to establish a raw
// ICE/DTLS/SRTP transport are extracted here.

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	iceUfragRe      = regexp.MustCompile(`(?m)^a=ice-ufrag:(.+)$`)
	icePwdRe        = regexp.MustCompile(`(?m)^a=ice-pwd:(.+)$`)
	fingerprintRe   = regexp.MustCompile(`(?m)^a=fingerprint:(.+)$`)
	candidateLineRe = regexp.MustCompile(`(?m)^(a=candidate:[^\r\n]+)`)
	rtpMapLineRe    = regexp.MustCompile(`^a=rtpmap:(\d+)\s+([^/\s]+)/(\d+)(?:/(\d+))?`)
)

// sdpInternalCodecNames are codec encoding names that must NOT be exposed to
// the relay layer. They are transport-level helpers (RTX, FEC) that do not
// carry independent media streams and whose PTs differ from the base codec PT.
var sdpInternalCodecNames = map[string]bool{
	"rtx": true, "ulpfec": true, "flexfec-03": true,
	"cn": true, "telephone-event": true,
}

// ExtractShadowCredentials parses ICE ufrag, ICE pwd, and the DTLS fingerprint
// (with its algorithm prefix, e.g. "sha-256 AB:CD:...") from any SDP blob.
// Trailing \r is stripped to handle CRLF line endings.
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

// ExtractShadowCandidates returns a deduplicated slice of "a=candidate:..."
// lines from an SDP. In BUNDLE mode the same candidates repeat in every m=
// section; duplicates are collapsed so each candidate is injected once.
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

// ParsePTCodecMap extracts a payload-type → CodecInfo map from an SDP blob.
// It reads a=rtpmap: lines within m=audio / m=video sections and skips
// transport-helper codecs (RTX, ULPFEC, CN, telephone-event).
//
// This is the only codec parsing we do in the raw transport layer. It is used to:
//   - Populate rawShadowSession.ptCodecMap so the relay can create
//     TrackLocalStaticRTP instances with the correct MIME type and clock rate.
//   - Seed the relay PeerConnection MediaEngine with Teams' original PT numbers
//     so the Wails frontend receives correctly typed RTP packets.
func ParsePTCodecMap(sdp string) map[uint8]CodecInfo {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	out := make(map[uint8]CodecInfo)
	currentIsVideo := false
	inMediaSection := false

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		if strings.HasPrefix(bare, "m=audio") {
			currentIsVideo = false
			inMediaSection = true
			continue
		}
		if strings.HasPrefix(bare, "m=video") {
			currentIsVideo = true
			inMediaSection = true
			continue
		}
		if strings.HasPrefix(bare, "m=") {
			// m=application or other — skip codec parsing for this section.
			inMediaSection = false
			continue
		}
		if !inMediaSection {
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
		// m[2] = encoding name (original case, e.g. "VP8", "opus", "H264")
		encodingNameLower := strings.ToLower(m[2])
		if sdpInternalCodecNames[encodingNameLower] {
			continue // skip RTX/FEC/CN
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
		if currentIsVideo {
			mimePrefix = "video/"
		}

		pt := uint8(pt64)
		out[pt] = CodecInfo{
			MimeType:    mimePrefix + m[2], // preserve original case ("video/VP8")
			ClockRate:   uint32(clockRate),
			Channels:    channels,
			PayloadType: pt,
		}
	}

	return out
}
