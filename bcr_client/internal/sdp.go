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
	cnameRe	 		= regexp.MustCompile(`^a=ssrc:\d+\s+cname:([^\s]+)`)
	transportCCRe   = regexp.MustCompile(`^a=extmap:(\d+).*(transport-wide-cc|transport-cc)`)
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

// ssrcLineRe matches "a=ssrc:<decimal> ..." lines.
var ssrcLineRe = regexp.MustCompile(`^a=ssrc:(\d+)\s`)

// ExtractAllSSRCs returns deduplicated SSRC values declared in any m=audio or
// m=video section of the SDP via a=ssrc: lines. Unlike ExtractVideoSSRCs, this
// includes audio SSRCs — critical for setting localSenderSSRC from audio-only
// initial offers where no m=video section exists yet.
func ExtractAllSSRCs(sdp string) []uint32 {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	inMediaSection := false
	seen := make(map[uint32]bool)
	var out []uint32

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		if strings.HasPrefix(bare, "m=audio") || strings.HasPrefix(bare, "m=video") {
			inMediaSection = true
			continue
		}
		if strings.HasPrefix(bare, "m=") {
			inMediaSection = false
			continue
		}
		if !inMediaSection {
			continue
		}

		m := ssrcLineRe.FindStringSubmatch(bare)
		if m == nil {
			continue
		}
		ssrc64, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		ssrc := uint32(ssrc64)
		if !seen[ssrc] {
			seen[ssrc] = true
			out = append(out, ssrc)
		}
	}

	return out
}

// ExtractVideoSSRCs returns deduplicated SSRC values declared in m=video
// sections of the SDP via a=ssrc: lines.
//
// This is critical for breaking the PLI chicken-and-egg deadlock: the SFU
// won't send video without receiving a PLI, but we can only send PLI if we
// know the target SSRC. By extracting SSRCs from the SDP we can proactively
// send PLI before any video packets arrive.
func ExtractVideoSSRCs(sdp string) []uint32 {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	inVideoSection := false
	seen := make(map[uint32]bool)
	var out []uint32

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		if strings.HasPrefix(bare, "m=video") {
			inVideoSection = true
			continue
		}
		if strings.HasPrefix(bare, "m=") {
			inVideoSection = false
			continue
		}
		if !inVideoSection {
			continue
		}

		m := ssrcLineRe.FindStringSubmatch(bare)
		if m == nil {
			continue
		}
		ssrc64, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		ssrc := uint32(ssrc64)
		if !seen[ssrc] {
			seen[ssrc] = true
			out = append(out, ssrc)
		}
	}

	return out
}

// ExtractMediaSections parses an SDP blob and returns a list of MediaSections,
// which contain the type, port, protocol, and formats for each m= line.
// This is used for diagnostic logging during renegotiation.
func ExtractMediaSections(sdp string) []MediaSection {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)

	var sections []MediaSection
	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")
		if strings.HasPrefix(bare, "m=") {
			parts := strings.Split(bare[2:], " ")
			if len(parts) >= 4 {
				sections = append(sections, MediaSection{
					Type:     parts[0],
					Port:     parts[1],
					Protocol: parts[2],
					Formats:  parts[3:],
				})
			}
		}
	}
	return sections
}

// ExtractCNAME extracts the first CNAME value from a=ssrc: lines in any
// m=audio or m=video section of the SDP. This is the browser's CNAME that
// must be echoed in our outbound SDES packets.
func ExtractCNAME(sdp string) string {
	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")

	inMediaSection := false
	for _, line := range lines {
		if strings.HasPrefix(line, "m=audio") || strings.HasPrefix(line, "m=video") {
			inMediaSection = true
			continue
		}
		if strings.HasPrefix(line, "m=") {
			inMediaSection = false
			continue
		}
		if !inMediaSection {
			continue
		}

		m := cnameRe.FindStringSubmatch(line)
		if m != nil {
			return m[1] // No TrimSpace needed because [^\s]+ guarantees no spaces
		}
	}
	return ""
}

// ExtractTransportCCExtID extracts the RTP header extension ID used for
// transport-wide congestion control from the SDP. Returns 0 if not found.
//
// Example SDP line:
//
//	a=extmap:3 http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01
func ExtractTransportCCExtID(sdp string) uint8 {
	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")

	for _, line := range lines {
		m := transportCCRe.FindStringSubmatch(line)
		if m != nil {
			id, err := strconv.ParseUint(m[1], 10, 8)
			if err == nil {
				return uint8(id)
			}
		}
	}
	return 0
}

// fidGroupRe matches "a=ssrc-group:FID <mediaSSRC> <rtxSSRC>"
var fidGroupRe = regexp.MustCompile(`^a=ssrc-group:FID\s+(\d+)\s+(\d+)`)

// ExtractFIDGroups returns a map from RTX SSRC → media SSRC, parsed from
// ExtractFIDGroups returns a map from RTX SSRC → media SSRC, parsed from
// "a=ssrc-group:FID <mediaSSRC> <rtxSSRC>" lines in the SDP.
// This is used to reconstruct the original RTP SSRC during RTX decapsulation.
func ExtractFIDGroups(sdp string) map[uint32]uint32 {
	out := make(map[uint32]uint32)
	for _, line := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		bare := strings.TrimRight(strings.TrimSpace(line), "\r")
		m := fidGroupRe.FindStringSubmatch(bare)
		if m == nil {
			continue
		}
		mediaSSRC, err1 := strconv.ParseUint(m[1], 10, 32)
		rtxSSRC, err2 := strconv.ParseUint(m[2], 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		out[uint32(rtxSSRC)] = uint32(mediaSSRC)
	}
	return out
}

// ShortenSDP returns a compact, human-readable summary of the SDP's key parameters.
func ShortenSDP(sdp string) string {
	ufrag, pwd, fingerprint := ExtractShadowCredentials(sdp)

	// Extract connection lines
	var cLines []string
	for _, line := range strings.Split(sdp, "\n") {
		bare := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(bare, "c=") {
			cLines = append(cLines, bare)
		}
	}

	// These are media SECTIONS (type + m-line port), NOT a=mid: values. The label
	// used to read "mids=", which invited exactly the wrong reading of every log
	// line it produced — "mids=[audio:9, video:9]" is two m-sections on port 9,
	// not two mids named "audio:9" and "video:9".
	sections := ExtractMediaSections(sdp)
	var sectionStr []string
	for _, m := range sections {
		sectionStr = append(sectionStr, m.Type+":"+m.Port)
	}

	return "ufrag=" + ufrag + " pwd=" + pwd + " fingerprint=" + fingerprint + " c=[" + strings.Join(cLines, ", ") + "] msections=[" + strings.Join(sectionStr, ", ") + "]"
}

// MungeSDPTransport Go implementation of the SDP munger.
// Replaces ufrag, pwd, fingerprint, connection address (to 0.0.0.0), and removes candidates.
func MungeSDPTransport(sdp string, ufrag string, pwd string, fingerprint string) string {
	sep := "\r\n"
	if !strings.Contains(sdp, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(sdp, sep)
	var out []string

	for _, line := range lines {
		bare := strings.TrimRight(line, "\r")

		// Skip candidates
		if strings.HasPrefix(bare, "a=candidate:") || strings.HasPrefix(bare, "a=end-of-candidates") {
			continue
		}

		// Replace ufrag
		if strings.HasPrefix(bare, "a=ice-ufrag:") {
			out = append(out, "a=ice-ufrag:"+ufrag)
			continue
		}

		// Replace pwd.
		// NOTE: no "a=end-of-candidates" is injected here — see the matching comment
		// in the JS munger (pageInterceptor.js mungeSdpTransport). Declaring
		// end-of-candidates on a candidate-less offer makes the SFU give up before
		// trickle delivers anything.
		if strings.HasPrefix(bare, "a=ice-pwd:") {
			out = append(out, "a=ice-pwd:"+pwd)
			continue
		}

		// Replace fingerprint
		if strings.HasPrefix(bare, "a=fingerprint:") {
			if strings.HasPrefix(fingerprint, "sha-256 ") {
				out = append(out, "a=fingerprint:"+fingerprint)
			} else {
				out = append(out, "a=fingerprint:sha-256 "+fingerprint)
			}
			continue
		}

		// Replace connection address
		if strings.HasPrefix(bare, "c=") {
			out = append(out, "c=IN IP4 0.0.0.0")
			continue
		}

		out = append(out, line)
	}

	return strings.Join(out, sep)
}

