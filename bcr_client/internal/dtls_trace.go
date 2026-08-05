package engine

// dtls_trace.go — DTLS record/handshake header decoding for diagnostics.
//
// Why this exists: a bad_certificate(42) alert was traced to pion/dtls
// consuming a server flight that was NOT a response to our ClientHello — the
// flight was already sitting in the socket when the handshake started, so its
// ServerKeyExchange is signed over a different client_random and
// verifyKeySignature fails (pion/dtls flight5handler.go). The previous logging
// only printed the datagram length and first byte, which cannot distinguish a
// legitimate flight from a stale or foreign one.
//
// Decoding the record and handshake headers gives the three facts that do:
//   - epoch + sequence_number      → which DTLS association a record belongs to
//   - handshake message_seq        → whether a flight is a retransmit or new
//   - the peer certificate         → whether the sender is the peer named in
//                                    the SFU's a=fingerprint at all
//
// This file is decode-and-report only. Nothing here drops packets.

import (
	"encoding/binary"
	"fmt"
	"strings"
)

const (
	dtlsRecordHeaderLen    = 13
	dtlsHandshakeHeaderLen = 12
)

// DTLS ContentType values (RFC 6347 §4.1).
const (
	dtlsContentTypeChangeCipherSpec uint8 = 20
	dtlsContentTypeAlert            uint8 = 21
	dtlsContentTypeHandshake        uint8 = 22
	dtlsContentTypeApplicationData  uint8 = 23
)

// DTLS HandshakeType values (RFC 5246 §7.4, RFC 6347 §4.3.2).
const (
	dtlsHandshakeTypeHelloRequest       uint8 = 0
	dtlsHandshakeTypeClientHello        uint8 = 1
	dtlsHandshakeTypeServerHello        uint8 = 2
	dtlsHandshakeTypeHelloVerifyRequest uint8 = 3
	dtlsHandshakeTypeCertificate        uint8 = 11
	dtlsHandshakeTypeServerKeyExchange  uint8 = 12
	dtlsHandshakeTypeCertificateRequest uint8 = 13
	dtlsHandshakeTypeServerHelloDone    uint8 = 14
	dtlsHandshakeTypeCertificateVerify  uint8 = 15
	dtlsHandshakeTypeClientKeyExchange  uint8 = 16
	dtlsHandshakeTypeFinished           uint8 = 20
)

// dtlsRecordInfo is a decoded DTLS record header, plus the handshake header
// when the record carries an unencrypted handshake message.
type dtlsRecordInfo struct {
	ContentType uint8
	Epoch       uint16
	Sequence    uint64 // uint48 on the wire
	Length      uint16

	// Truncated is set when the record claims more bytes than the datagram
	// actually holds — a malformed or clipped packet.
	Truncated bool

	// Handshake fields, valid only when HasHandshake is true. Records at
	// epoch > 0 are encrypted, so their handshake headers are not readable.
	HasHandshake   bool
	HandshakeType  uint8
	MessageLength  uint32 // uint24: full message length across all fragments
	MessageSeq     uint16
	FragmentOffset uint32 // uint24
	FragmentLength uint32 // uint24
	Body           []byte // this fragment's handshake body (not a copy)
}

// Complete reports whether this record carries a whole handshake message rather
// than one fragment of a larger one. Body is only meaningful to parse when true.
func (r dtlsRecordInfo) Complete() bool {
	return r.HasHandshake && r.FragmentOffset == 0 && r.FragmentLength == r.MessageLength
}

func (r dtlsRecordInfo) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ct=%s(%d) epoch=%d seq=%d len=%d",
		dtlsContentTypeName(r.ContentType), r.ContentType, r.Epoch, r.Sequence, r.Length)
	if r.Truncated {
		b.WriteString(" TRUNCATED")
	}
	if r.HasHandshake {
		fmt.Fprintf(&b, " hs=%s(%d) msg_seq=%d frag=%d/%d msg_len=%d",
			dtlsHandshakeTypeName(r.HandshakeType), r.HandshakeType,
			r.MessageSeq, r.FragmentOffset, r.FragmentLength, r.MessageLength)
		if !r.Complete() {
			b.WriteString(" FRAGMENTED")
		}
	}
	return b.String()
}

// parseDTLSRecords decodes every record in a single datagram. DTLS permits
// several records to be packed into one datagram, which is exactly what the
// Teams relay does — its whole server flight (ServerHello, Certificate,
// ServerKeyExchange, ServerHelloDone) arrived as one 774-byte datagram.
//
// Malformed input yields whatever was decodable; it never panics.
func parseDTLSRecords(p []byte) []dtlsRecordInfo {
	var out []dtlsRecordInfo

	for off := 0; off+dtlsRecordHeaderLen <= len(p); {
		var r dtlsRecordInfo
		r.ContentType = p[off]
		r.Epoch = binary.BigEndian.Uint16(p[off+3 : off+5])
		r.Sequence = uint48(p[off+5 : off+11])
		r.Length = binary.BigEndian.Uint16(p[off+11 : off+13])

		body := p[off+dtlsRecordHeaderLen:]
		if int(r.Length) > len(body) {
			r.Truncated = true
		} else {
			body = body[:r.Length]
		}

		// Only epoch 0 records are in the clear; later epochs are encrypted and
		// their first byte is ciphertext, not a handshake type.
		if r.ContentType == dtlsContentTypeHandshake && r.Epoch == 0 && len(body) >= dtlsHandshakeHeaderLen {
			r.HasHandshake = true
			r.HandshakeType = body[0]
			r.MessageLength = uint24(body[1:4])
			r.MessageSeq = binary.BigEndian.Uint16(body[4:6])
			r.FragmentOffset = uint24(body[6:9])
			r.FragmentLength = uint24(body[9:12])

			frag := body[dtlsHandshakeHeaderLen:]
			if int(r.FragmentLength) <= len(frag) {
				frag = frag[:r.FragmentLength]
			}
			r.Body = frag
		}

		out = append(out, r)

		if r.Truncated {
			break
		}
		off += dtlsRecordHeaderLen + int(r.Length)
	}

	return out
}

// peerCertFromCertificateMessage extracts the leaf certificate DER from the body
// of a Certificate handshake message.
//
//	opaque ASN.1Cert<1..2^24-1>;
//	struct { ASN.1Cert certificate_list<0..2^24-1>; } Certificate;
//
// The caller must only pass a complete (unfragmented) message body.
func peerCertFromCertificateMessage(body []byte) ([]byte, error) {
	if len(body) < 3 {
		return nil, fmt.Errorf("certificate message too short (%d bytes)", len(body))
	}

	listLen := uint24(body[0:3])
	rest := body[3:]
	if int(listLen) > len(rest) {
		return nil, fmt.Errorf("certificate_list length %d exceeds available %d bytes", listLen, len(rest))
	}
	if listLen == 0 {
		return nil, fmt.Errorf("empty certificate_list")
	}
	if len(rest) < 3 {
		return nil, fmt.Errorf("truncated certificate entry header")
	}

	certLen := uint24(rest[0:3])
	if int(certLen)+3 > len(rest) {
		return nil, fmt.Errorf("certificate length %d exceeds available %d bytes", certLen, len(rest)-3)
	}
	if certLen == 0 {
		return nil, fmt.Errorf("zero-length leaf certificate")
	}

	return rest[3 : 3+certLen], nil
}

// normalizeFingerprint reduces an SDP a=fingerprint value to bare uppercase
// colon-hex so values from different sources compare directly. It accepts
// "sha-256 AB:CD:..." as well as a bare "ab:cd:...".
func normalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	if i := strings.LastIndex(fp, " "); i >= 0 {
		fp = fp[i+1:]
	}
	return strings.ToUpper(strings.TrimSpace(fp))
}

func dtlsContentTypeName(t uint8) string {
	switch t {
	case dtlsContentTypeChangeCipherSpec:
		return "change_cipher_spec"
	case dtlsContentTypeAlert:
		return "alert"
	case dtlsContentTypeHandshake:
		return "handshake"
	case dtlsContentTypeApplicationData:
		return "application_data"
	default:
		return "unknown"
	}
}

func dtlsHandshakeTypeName(t uint8) string {
	switch t {
	case dtlsHandshakeTypeHelloRequest:
		return "hello_request"
	case dtlsHandshakeTypeClientHello:
		return "client_hello"
	case dtlsHandshakeTypeServerHello:
		return "server_hello"
	case dtlsHandshakeTypeHelloVerifyRequest:
		return "hello_verify_request"
	case dtlsHandshakeTypeCertificate:
		return "certificate"
	case dtlsHandshakeTypeServerKeyExchange:
		return "server_key_exchange"
	case dtlsHandshakeTypeCertificateRequest:
		return "certificate_request"
	case dtlsHandshakeTypeServerHelloDone:
		return "server_hello_done"
	case dtlsHandshakeTypeCertificateVerify:
		return "certificate_verify"
	case dtlsHandshakeTypeClientKeyExchange:
		return "client_key_exchange"
	case dtlsHandshakeTypeFinished:
		return "finished"
	default:
		return "unknown"
	}
}

func uint24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func uint48(b []byte) uint64 {
	return uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(b[2])<<24 |
		uint64(b[3])<<16 | uint64(b[4])<<8 | uint64(b[5])
}
