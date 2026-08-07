package engine

import "testing"

// The DTLS role must be derived from BOTH a=setup: values, per RFC 5763 §5.
//
// Reading only the remote SDP works for outgoing calls and deadlocks incoming
// ones: an SFU's answer carries a concrete "passive", but an SFU's offer
// carries "actpass", and the browser's answer — which is forwarded to the SFU
// untouched — says "active". Treating "actpass" as "server" left both ends
// waiting for a ClientHello neither would send.
func TestResolveDTLSRole(t *testing.T) {
	const (
		remoteActpass = "v=0\r\na=setup:actpass\r\na=ice-ufrag:x\r\n"
		remotePassive = "v=0\r\na=setup:passive\r\na=ice-ufrag:x\r\n"
		remoteActive  = "v=0\r\na=setup:active\r\na=ice-ufrag:x\r\n"
		remoteNoSetup = "v=0\r\na=ice-ufrag:x\r\n"
	)

	tests := []struct {
		name       string
		localSetup string
		remoteSDP  string
		want       string
	}{
		{
			// Incoming call: SFU offers actpass, the browser answers active, so
			// we send the ClientHello. This is the case that was broken.
			name:       "incoming call — local active beats remote actpass",
			localSetup: "active",
			remoteSDP:  remoteActpass,
			want:       "client",
		},
		{
			name:       "incoming call — local passive means we wait",
			localSetup: "passive",
			remoteSDP:  remoteActpass,
			want:       "server",
		},
		{
			// Outgoing call: we offered actpass, the SFU answered passive.
			// This path worked before and must keep working.
			name:       "outgoing call — local actpass defers to remote passive",
			localSetup: "actpass",
			remoteSDP:  remotePassive,
			want:       "client",
		},
		{
			name:       "outgoing call — local actpass defers to remote active",
			localSetup: "actpass",
			remoteSDP:  remoteActive,
			want:       "server",
		},
		{
			name:       "no local setup recorded falls back to the remote answer",
			localSetup: "",
			remoteSDP:  remotePassive,
			want:       "client",
		},
		{
			name:       "neither side commits — offerer-waits convention",
			localSetup: "actpass",
			remoteSDP:  remoteNoSetup,
			want:       "server",
		},
		{
			name:       "case and whitespace are not significant",
			localSetup: "  ACTIVE ",
			remoteSDP:  remoteActpass,
			want:       "client",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDTLSRole(tc.localSetup, tc.remoteSDP); got != tc.want {
				t.Errorf("resolveDTLSRole(%q, remote) = %q, want %q", tc.localSetup, got, tc.want)
			}
		})
	}
}

// firstSDPAttr underpins the role decision, so its handling of BUNDLE's
// repeated attributes and of CRLF line endings is worth pinning down.
func TestFirstSDPAttr(t *testing.T) {
	sdp := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=setup:actpass\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 103\r\na=setup:actpass\r\n"

	if got := firstSDPAttr(sdp, "a=setup:"); got != "actpass" {
		t.Errorf("firstSDPAttr(a=setup:) = %q, want %q", got, "actpass")
	}
	if got := firstSDPAttr(sdp, "a=fingerprint:"); got != "" {
		t.Errorf("firstSDPAttr of an absent attribute = %q, want empty", got)
	}
}
