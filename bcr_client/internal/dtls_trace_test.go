package engine

import (
	"encoding/binary"
	"testing"
)

// buildRecord assembles one DTLS record carrying a single complete handshake
// message, mirroring the wire layout the Teams relay sends.
func buildRecord(contentType uint8, epoch uint16, seq uint64, hsType uint8, msgSeq uint16, body []byte) []byte {
	hs := make([]byte, dtlsHandshakeHeaderLen)
	hs[0] = hsType
	putUint24(hs[1:4], uint32(len(body)))
	binary.BigEndian.PutUint16(hs[4:6], msgSeq)
	putUint24(hs[6:9], 0)                  // fragment_offset
	putUint24(hs[9:12], uint32(len(body))) // fragment_length == length ⇒ unfragmented
	hs = append(hs, body...)

	rec := make([]byte, dtlsRecordHeaderLen)
	rec[0] = contentType
	rec[1], rec[2] = 0xfe, 0xfd // DTLS 1.2
	binary.BigEndian.PutUint16(rec[3:5], epoch)
	putUint48(rec[5:11], seq)
	binary.BigEndian.PutUint16(rec[11:13], uint16(len(hs)))
	return append(rec, hs...)
}

func putUint24(b []byte, v uint32) {
	b[0], b[1], b[2] = byte(v>>16), byte(v>>8), byte(v)
}

func putUint48(b []byte, v uint64) {
	b[0], b[1], b[2] = byte(v>>40), byte(v>>32), byte(v>>24)
	b[3], b[4], b[5] = byte(v>>16), byte(v>>8), byte(v)
}

// buildCertificateBody wraps a DER blob in the Certificate message structure.
func buildCertificateBody(der []byte) []byte {
	entry := make([]byte, 3)
	putUint24(entry, uint32(len(der)))
	entry = append(entry, der...)

	body := make([]byte, 3)
	putUint24(body, uint32(len(entry)))
	return append(body, entry...)
}

// A server flight arrives as several records packed into one datagram — that
// packing is exactly what made the original 774-byte failure hard to read.
func TestParseDTLSRecords_MultipleRecordsInOneDatagram(t *testing.T) {
	der := []byte{0x30, 0x82, 0x01, 0x0a, 0xde, 0xad, 0xbe, 0xef}

	datagram := buildRecord(dtlsContentTypeHandshake, 0, 0, dtlsHandshakeTypeServerHello, 0, []byte{0x01, 0x02, 0x03})
	datagram = append(datagram, buildRecord(dtlsContentTypeHandshake, 0, 1, dtlsHandshakeTypeCertificate, 1, buildCertificateBody(der))...)
	datagram = append(datagram, buildRecord(dtlsContentTypeHandshake, 0, 2, dtlsHandshakeTypeServerHelloDone, 2, nil)...)

	recs := parseDTLSRecords(datagram)
	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recs))
	}

	want := []struct {
		hsType uint8
		msgSeq uint16
		seq    uint64
	}{
		{dtlsHandshakeTypeServerHello, 0, 0},
		{dtlsHandshakeTypeCertificate, 1, 1},
		{dtlsHandshakeTypeServerHelloDone, 2, 2},
	}
	for i, w := range want {
		r := recs[i]
		if !r.HasHandshake {
			t.Errorf("record %d: HasHandshake = false, want true", i)
			continue
		}
		if r.HandshakeType != w.hsType {
			t.Errorf("record %d: HandshakeType = %d, want %d", i, r.HandshakeType, w.hsType)
		}
		if r.MessageSeq != w.msgSeq {
			t.Errorf("record %d: MessageSeq = %d, want %d", i, r.MessageSeq, w.msgSeq)
		}
		if r.Sequence != w.seq {
			t.Errorf("record %d: Sequence = %d, want %d", i, r.Sequence, w.seq)
		}
		if r.Epoch != 0 {
			t.Errorf("record %d: Epoch = %d, want 0", i, r.Epoch)
		}
		if !r.Complete() {
			t.Errorf("record %d: Complete() = false, want true", i)
		}
	}

	got, err := peerCertFromCertificateMessage(recs[1].Body)
	if err != nil {
		t.Fatalf("peerCertFromCertificateMessage: %v", err)
	}
	if string(got) != string(der) {
		t.Errorf("extracted DER = %x, want %x", got, der)
	}
}

// Records at epoch > 0 are encrypted; treating ciphertext as a handshake header
// would invent handshake types that were never sent.
func TestParseDTLSRecords_EncryptedEpochNotParsedAsHandshake(t *testing.T) {
	rec := buildRecord(dtlsContentTypeHandshake, 1, 7, dtlsHandshakeTypeFinished, 5, []byte{0xaa, 0xbb, 0xcc})

	recs := parseDTLSRecords(rec)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0].Epoch != 1 {
		t.Errorf("Epoch = %d, want 1", recs[0].Epoch)
	}
	if recs[0].HasHandshake {
		t.Error("HasHandshake = true for an epoch-1 record; encrypted records must not be decoded")
	}
}

// A clipped datagram must report what it has rather than panicking or looping.
func TestParseDTLSRecords_TruncatedRecord(t *testing.T) {
	full := buildRecord(dtlsContentTypeHandshake, 0, 0, dtlsHandshakeTypeServerHello, 0, []byte{0x01, 0x02, 0x03, 0x04, 0x05})
	recs := parseDTLSRecords(full[:len(full)-3])

	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if !recs[0].Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestParseDTLSRecords_ShortInputIsSafe(t *testing.T) {
	for _, in := range [][]byte{nil, {}, {22}, {22, 0xfe, 0xfd, 0x00}} {
		if recs := parseDTLSRecords(in); len(recs) != 0 {
			t.Errorf("parseDTLSRecords(%x) = %d records, want 0", in, len(recs))
		}
	}
}

// A fragmented Certificate must be reported as such, not silently fingerprinted
// from a partial body — that would produce a bogus FOREIGN CERTIFICATE verdict.
func TestFragmentedHandshakeIsNotComplete(t *testing.T) {
	body := []byte{0x01, 0x02, 0x03, 0x04}

	hs := make([]byte, dtlsHandshakeHeaderLen)
	hs[0] = dtlsHandshakeTypeCertificate
	putUint24(hs[1:4], 100) // full message is 100 bytes
	binary.BigEndian.PutUint16(hs[4:6], 3)
	putUint24(hs[6:9], 0)
	putUint24(hs[9:12], uint32(len(body))) // this fragment carries only 4
	hs = append(hs, body...)

	rec := make([]byte, dtlsRecordHeaderLen)
	rec[0] = dtlsContentTypeHandshake
	rec[1], rec[2] = 0xfe, 0xfd
	binary.BigEndian.PutUint16(rec[11:13], uint16(len(hs)))
	rec = append(rec, hs...)

	recs := parseDTLSRecords(rec)
	if len(recs) != 1 || !recs[0].HasHandshake {
		t.Fatalf("expected 1 handshake record, got %+v", recs)
	}
	if recs[0].Complete() {
		t.Error("Complete() = true for a fragmented message, want false")
	}
}

func TestPeerCertFromCertificateMessage_Malformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":                nil,
		"list length overruns": {0x00, 0xff, 0xff, 0x01},
		"empty list":           {0x00, 0x00, 0x00},
		"cert length overruns": {0x00, 0x00, 0x08, 0x00, 0xff, 0xff, 0x01},
	}
	for name, body := range cases {
		if _, err := peerCertFromCertificateMessage(body); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	want := "3A:24:74:64"
	for _, in := range []string{
		"sha-256 3A:24:74:64",
		"  sha-256   3a:24:74:64  ",
		"3a:24:74:64",
	} {
		if got := normalizeFingerprint(in); got != want {
			t.Errorf("normalizeFingerprint(%q) = %q, want %q", in, got, want)
		}
	}
}
