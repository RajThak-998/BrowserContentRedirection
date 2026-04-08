package engine

import "regexp"

var (
	iceUfragRe    = regexp.MustCompile(`(?m)^a=ice-ufrag:(.+)$`)
	icePwdRe      = regexp.MustCompile(`(?m)^a=ice-pwd:(.+)$`)
	fingerprintRe = regexp.MustCompile(`(?m)^a=fingerprint:(.+)$`)
)

func ExtractShadowCredentials(sdp string) (ufrag, pwd, fingerprint string) {
	if m := iceUfragRe.FindStringSubmatch(sdp); len(m) > 1 {
		ufrag = m[1]
	}
	if m := icePwdRe.FindStringSubmatch(sdp); len(m) > 1 {
		pwd = m[1]
	}
	if m := fingerprintRe.FindStringSubmatch(sdp); len(m) > 1 {
		fingerprint = m[1]
	}
	return
}
