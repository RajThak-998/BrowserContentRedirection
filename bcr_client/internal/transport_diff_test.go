package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pion/ice/v4"
)

// sdpWith builds a minimal SDP carrying only the transport attributes
// transportDiff reads, so each test varies exactly one thing.
func sdpWith(ufrag, pwd, fingerprint, setup string, candidates ...string) string {
	var b strings.Builder
	b.WriteString("v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n")
	b.WriteString("a=ice-ufrag:" + ufrag + "\r\n")
	b.WriteString("a=ice-pwd:" + pwd + "\r\n")
	b.WriteString("a=fingerprint:sha-256 " + fingerprint + "\r\n")
	b.WriteString("a=setup:" + setup + "\r\n")
	for _, c := range candidates {
		b.WriteString("a=candidate:" + c + "\r\n")
	}
	return b.String()
}

const (
	fpA = "84:B4:CC:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD"
	fpB = "EC:E2:EA:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD"

	candHost  = "1684144148 1 udp 2130706431 192.168.1.10 53959 typ host"
	candSrflx = "1025034457 1 udp 1694498815 103.200.213.226 53961 typ srflx raddr 0.0.0.0 rport 53961"
)

// A renegotiation that keeps the same transport must not be reported as a
// change, and one that rotates any of ufrag / pwd / fingerprint must be.
//
// This is the fact that decides whether an incoming call can be recovered by
// retrying the existing ICE agent: if the SFU moved its transport at pickup,
// the live agent is authenticating against credentials the peer has abandoned
// and no retry of it can ever succeed.
func TestTransportDiff(t *testing.T) {
	base := sdpWith("AplP", "pwd-one", fpA, "actpass", candHost, candSrflx)

	tests := []struct {
		name        string
		prev        string
		next        string
		wantChanged bool
	}{
		{
			name:        "identical transport",
			prev:        base,
			next:        base,
			wantChanged: false,
		},
		{
			// The post-pickup renegotiation in the captured incoming call added an
			// m=application section and more candidates. Neither moves the peer.
			name:        "more candidates only",
			prev:        sdpWith("AplP", "pwd-one", fpA, "actpass", candHost),
			next:        sdpWith("AplP", "pwd-one", fpA, "actpass", candHost, candSrflx),
			wantChanged: false,
		},
		{
			name:        "setup role flipped but transport identical",
			prev:        sdpWith("AplP", "pwd-one", fpA, "actpass", candHost),
			next:        sdpWith("AplP", "pwd-one", fpA, "passive", candHost),
			wantChanged: false,
		},
		{
			name:        "ufrag rotated — ICE restart",
			prev:        base,
			next:        sdpWith("Zq7X", "pwd-one", fpA, "actpass", candHost, candSrflx),
			wantChanged: true,
		},
		{
			name:        "password rotated",
			prev:        base,
			next:        sdpWith("AplP", "pwd-two", fpA, "actpass", candHost, candSrflx),
			wantChanged: true,
		},
		{
			name:        "fingerprint rotated — peer identity changed",
			prev:        base,
			next:        sdpWith("AplP", "pwd-one", fpB, "actpass", candHost, candSrflx),
			wantChanged: true,
		},
		{
			// Before Connect() runs there is no baseline; that must read as "not
			// changed" rather than as a spurious restart signal.
			name:        "no prior transport recorded",
			prev:        "",
			next:        base,
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, changed := transportDiff(tt.prev, tt.next)
			if changed != tt.wantChanged {
				t.Errorf("transportDiff() changed = %v, want %v\nsummary: %s", changed, tt.wantChanged, summary)
			}
			if summary == "" {
				t.Error("transportDiff() returned an empty summary")
			}
		})
	}
}

// The remote ICE password is a credential and must never reach the log, in any
// field, however the summary is formatted.
func TestTransportDiffNeverLogsPassword(t *testing.T) {
	const secret = "c7lMYOKlScsIQ3hWFj7w5d"

	prev := sdpWith("AplP", secret, fpA, "actpass", candHost)
	next := sdpWith("AplP", "a-different-secret", fpA, "actpass", candHost)

	for _, summary := range []string{
		mustSummary(t, prev, next),
		mustSummary(t, "", next),
		mustSummary(t, prev, prev),
	} {
		if strings.Contains(summary, secret) {
			t.Errorf("transportDiff() leaked the ICE password into its summary: %s", summary)
		}
		if strings.Contains(summary, "a-different-secret") {
			t.Errorf("transportDiff() leaked the ICE password into its summary: %s", summary)
		}
	}
}

func mustSummary(t *testing.T, prev, next string) string {
	t.Helper()
	summary, _ := transportDiff(prev, next)
	return summary
}

// candidateKey must ignore the parts of a candidate line the extension
// rewrites, so a candidate that comes back to us is still recognised as ours.
// scrubCandidateRaddr replaces raddr with 0.0.0.0 before dispatch, which is
// exactly why the key cannot be the raw line.
func TestCandidateKeyIdentifiesSameTransportAddress(t *testing.T) {
	same := candidateKey("20.202.56.96", 54201, 1)
	if got := candidateKey("20.202.56.96", 54201, 1); got != same {
		t.Errorf("candidateKey is not stable: %s != %s", got, same)
	}

	tests := []struct {
		name    string
		address string
		port    int
		comp    uint16
	}{
		{"different port", "20.202.56.96", 54202, 1},
		{"different address", "20.202.56.97", 54201, 1},
		{"different component", "20.202.56.96", 54201, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := candidateKey(tt.address, tt.port, tt.comp); got == same {
				t.Errorf("candidateKey collided with a different candidate: %s", got)
			}
		})
	}
}

// newTestSession builds a session with a real (but never gathering) ICE agent,
// so AddRemoteIceCandidate can be exercised end to end, and returns a closure
// giving the accumulated log.
func newTestSession(t *testing.T) (*rawShadowSession, func() string) {
	t.Helper()

	var mu sync.Mutex
	var logged []string
	s := newRawShadowSession("test-bridge", func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	})

	agent, err := ice.NewAgent(&ice.AgentConfig{})
	if err != nil {
		t.Fatalf("ice.NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	s.iceAgent = agent

	return s, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(logged, "\n")
	}
}

// A candidate of our own handed back to us must be dropped before it reaches
// the ICE agent. Adding it would make pion pair a local candidate against
// itself — hairpin pairs that can never carry SFU media but that are retried
// for the whole checking phase.
//
// This is not hypothetical: bootstrap.js relayed every downstream candidate
// straight back upstream, so bcr_client received each of its own candidates
// 2-3ms after trickling it.
func TestAddRemoteIceCandidateRejectsOwn(t *testing.T) {
	s, log := newTestSession(t)

	// Register it exactly as the OnCandidate handler does.
	s.localCandidateKeys[candidateKey("20.202.56.96", 54201, 1)] = true

	// Note raddr 0.0.0.0: the extension's scrubCandidateRaddr rewrites it before
	// dispatch, so this is not byte-identical to the line we gathered. Matching
	// on the transport address is what makes it recognisable anyway.
	s.AddRemoteIceCandidate("a=candidate:1225087830 1 udp 16777215 20.202.56.96 54201 typ relay raddr 0.0.0.0 rport 6355")

	if !strings.Contains(log(), "IGNORED echoed local candidate") {
		t.Errorf("our own candidate was not dropped; log:\n%s", log())
	}

	// A genuine SFU candidate must still get through — the guard must key on the
	// transport address, not reject everything that arrives late.
	s.AddRemoteIceCandidate("a=candidate:1 1 udp 2130706431 20.202.56.99 3480 typ host")

	if !strings.Contains(log(), "remote candidate: 1 1 udp 2130706431 20.202.56.99") {
		t.Errorf("a foreign candidate was dropped; the guard is too broad. log:\n%s", log())
	}
}

// An empty candidate is the peer's end-of-candidates marker, not a malformed
// one, and must be ignored quietly. The extension's emitter drops the
// endOfCandidates flag (emitter.js rebuilds the payload without it), so this is
// exactly how that marker arrives.
func TestAddRemoteIceCandidateIgnoresEndOfCandidates(t *testing.T) {
	s, log := newTestSession(t)

	s.AddRemoteIceCandidate("")
	s.AddRemoteIceCandidate("a=candidate:")

	if strings.Contains(log(), "UnmarshalCandidate failed") {
		t.Errorf("end-of-candidates was treated as a malformed candidate; log:\n%s", log())
	}
	if strings.Contains(log(), "remote candidate:") {
		t.Errorf("end-of-candidates was added as a real candidate; log:\n%s", log())
	}
}

// ShortenSDP's section list is m-line type+port, not a=mid: values. The label
// said "mids=" for a long time and made every log line that used it read as the
// opposite of what it was.
func TestShortenSDPLabelsMediaSections(t *testing.T) {
	got := ShortenSDP(sdpWith("AplP", "pwd", fpA, "actpass", candHost))
	if !strings.Contains(got, "msections=[audio:9]") {
		t.Errorf("ShortenSDP() = %q, want it to contain msections=[audio:9]", got)
	}
	if strings.Contains(got, "mids=") {
		t.Errorf("ShortenSDP() still uses the misleading mids= label: %q", got)
	}
}
