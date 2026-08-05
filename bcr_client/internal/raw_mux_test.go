package engine

import "testing"

func newTestSplitter(expectedFP string, filterOn bool) *iceConnSplitter {
	return &iceConnSplitter{
		logf:               func(string, ...any) {},
		bridgeID:           "test",
		expectedPeerFP:     normalizeFingerprint(expectedFP),
		staleFilterEnabled: filterOn,
	}
}

// serverFlight builds the datagram shape the Teams relay sends: ServerHello,
// Certificate and ServerHelloDone packed into one datagram.
func serverFlight(der []byte) []byte {
	d := buildRecord(dtlsContentTypeHandshake, 0, 0, dtlsHandshakeTypeServerHello, 0, []byte{0x01})
	d = append(d, buildRecord(dtlsContentTypeHandshake, 0, 1, dtlsHandshakeTypeCertificate, 1, buildCertificateBody(der))...)
	d = append(d, buildRecord(dtlsContentTypeHandshake, 0, 2, dtlsHandshakeTypeServerHelloDone, 2, nil)...)
	return d
}

// The exact failure from the VDI log: a server flight already sitting in the
// socket when the handshake starts. pion consumed it and aborted the handshake
// with bad_certificate, because its ServerKeyExchange was signed over a
// different client_random.
func TestShouldDropInbound_FlightBeforeOurClientHello(t *testing.T) {
	der := []byte{0x30, 0x82, 0xaa, 0xbb}
	s := newTestSplitter(sha256ColonHex(der), true) // even a MATCHING cert must be dropped

	drop, reason := s.shouldDropInbound(serverFlight(der))
	if !drop {
		t.Fatal("expected the flight to be dropped; it predates our ClientHello")
	}
	if reason == "" {
		t.Error("expected a reason explaining the drop")
	}
}

func TestShouldDropInbound_LegitimateFlightIsKept(t *testing.T) {
	der := []byte{0x30, 0x82, 0xaa, 0xbb}
	s := newTestSplitter(sha256ColonHex(der), true)
	s.clientHelloSent.Store(true)

	if drop, reason := s.shouldDropInbound(serverFlight(der)); drop {
		t.Fatalf("legitimate flight was dropped: %s", reason)
	}
}

func TestShouldDropInbound_ForeignCertificate(t *testing.T) {
	s := newTestSplitter(sha256ColonHex([]byte{0x30, 0x82, 0x11, 0x22}), true)
	s.clientHelloSent.Store(true)

	drop, reason := s.shouldDropInbound(serverFlight([]byte{0x30, 0x82, 0x99, 0x88}))
	if !drop {
		t.Fatal("expected a flight with a non-matching certificate to be dropped")
	}
	if reason == "" {
		t.Error("expected a reason explaining the drop")
	}
}

// With no a=fingerprint to compare against, the certificate rule must stay off
// rather than dropping everything.
func TestShouldDropInbound_NoExpectedFingerprintKeepsFlight(t *testing.T) {
	s := newTestSplitter("", true)
	s.clientHelloSent.Store(true)

	if drop, reason := s.shouldDropInbound(serverFlight([]byte{0x30, 0x82, 0x99, 0x88})); drop {
		t.Fatalf("flight dropped with no expected fingerprint configured: %s", reason)
	}
}

func TestShouldDropInbound_DisabledFilterNeverDrops(t *testing.T) {
	der := []byte{0x30, 0x82, 0xaa, 0xbb}
	s := newTestSplitter(sha256ColonHex([]byte{0x11, 0x22}), false) // mismatching FP

	if drop, _ := s.shouldDropInbound(serverFlight(der)); drop {
		t.Fatal("filter is disabled; nothing may be dropped")
	}
}

// After the handshake, records are encrypted and unreadable — filtering them
// would tear down a healthy session.
func TestShouldDropInbound_StopsAfterHandshake(t *testing.T) {
	der := []byte{0x30, 0x82, 0xaa, 0xbb}
	s := newTestSplitter(sha256ColonHex([]byte{0x11, 0x22}), true)
	s.handshakeDone.Store(true)

	if drop, _ := s.shouldDropInbound(serverFlight(der)); drop {
		t.Fatal("handshake is complete; the filter must be inert")
	}
}

// Alerts and application data carry no handshake header and must pass through
// so pion can react to them.
func TestShouldDropInbound_NonHandshakeRecordsPass(t *testing.T) {
	s := newTestSplitter(sha256ColonHex([]byte{0x11, 0x22}), true)

	alert := make([]byte, dtlsRecordHeaderLen+2)
	alert[0] = dtlsContentTypeAlert
	alert[1], alert[2] = 0xfe, 0xfd
	alert[12] = 2 // record length
	alert[13], alert[14] = 2, 42

	if drop, reason := s.shouldDropInbound(alert); drop {
		t.Fatalf("alert record was dropped: %s", reason)
	}
}

// A fragmented certificate cannot be hashed; it must not be judged foreign on
// the strength of a partial body.
func TestShouldDropInbound_FragmentedCertificateNotJudged(t *testing.T) {
	s := newTestSplitter(sha256ColonHex([]byte{0x11, 0x22}), true)
	s.clientHelloSent.Store(true)

	body := buildCertificateBody([]byte{0x30, 0x82, 0x99, 0x88})
	hs := make([]byte, dtlsHandshakeHeaderLen)
	hs[0] = dtlsHandshakeTypeCertificate
	putUint24(hs[1:4], 500) // full message far larger than this fragment
	putUint24(hs[9:12], uint32(len(body)))
	hs = append(hs, body...)

	rec := make([]byte, dtlsRecordHeaderLen)
	rec[0] = dtlsContentTypeHandshake
	rec[1], rec[2] = 0xfe, 0xfd
	rec[11] = byte(len(hs) >> 8)
	rec[12] = byte(len(hs))
	rec = append(rec, hs...)

	if drop, reason := s.shouldDropInbound(rec); drop {
		t.Fatalf("fragmented certificate was judged foreign: %s", reason)
	}
}
