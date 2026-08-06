package engine

// raw_mux.go — DTLS/SRTP inline packet splitter and net.PacketConn adapter.
//
// The Teams SFU sends both DTLS handshake records and SRTP media datagrams to
// the same UDP 5-tuple that the ICE agent established.
//
// To prevent race conditions with pion/dtls's internal 100ms flight timer,
// we cannot use an intermediate channel or separate goroutine. Instead, we
// give pion/dtls direct synchronous access to iceConn.Read() via iceConnSplitter.
//
// iceConnSplitter implements net.PacketConn. When pion/dtls calls ReadFrom(),
// the call blocks directly in iceConn.Read(). If the incoming packet is SRTP,
// it is diverted to srtpCh, and the loop continues to fetch the next packet
// without returning to pion/dtls.
//
// Set*Deadline calls are passed directly to the underlying ice.Conn, allowing
// pion/dtls's context cancellation and timeout mechanisms to use native
// kernel-level deadlines.

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
)

const (
	muxSRTPBufSize = 2048 // media is high-frequency; larger buffer prevents drops
)

// iceConnSplitter wraps an ice.Conn to implement net.PacketConn for pion/dtls,
// while inline-diverting SRTP packets to a separate channel.
type iceConnSplitter struct {
	conn            *ice.Conn
	srtpCh          chan []byte
	logf            func(string, ...any)
	bridgeID        string
	ignoreDeadlines bool

	// expectedPeerFP is the SFU's a=fingerprint from the remote SDP, normalized
	// to bare uppercase colon-hex. Every inbound Certificate message is hashed
	// and compared against it, which is what distinguishes our actual peer's
	// handshake from a stale or foreign one arriving on the same 5-tuple.
	expectedPeerFP string

	// clientHelloSent flips once we write our own ClientHello. Any inbound
	// handshake record seen before that cannot be a response to us — it is
	// left over from another association. Read and written from different
	// goroutines (pion/dtls reads and sends on separate ones), hence atomic.
	clientHelloSent atomic.Bool

	// handshakeDone flips once DTLS completes. The stale filter only guards the
	// handshake; afterwards records are encrypted (epoch > 0) and unreadable,
	// and pion must see everything.
	handshakeDone atomic.Bool

	// staleFilterEnabled gates dropping (as opposed to merely reporting)
	// handshake records that cannot belong to this association.
	staleFilterEnabled bool
	staleDropped       atomic.Uint64
}

func newIceConnSplitter(conn *ice.Conn, logf func(string, ...any), bridgeID string) *iceConnSplitter {
	return &iceConnSplitter{
		conn:               conn,
		srtpCh:             make(chan []byte, muxSRTPBufSize),
		logf:               logf,
		bridgeID:           bridgeID,
		staleFilterEnabled: staleDTLSFilterEnabled(),
	}
}

// staleDTLSFilterEnabled reports whether to drop handshake records that cannot
// belong to this association. Set BCR_DTLS_STALE_FILTER=off to report only.
func staleDTLSFilterEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("BCR_DTLS_STALE_FILTER")), "off")
}

// SetHandshakeComplete stops the stale filter once DTLS is established.
func (s *iceConnSplitter) SetHandshakeComplete() {
	s.handshakeDone.Store(true)
	if n := s.staleDropped.Load(); n > 0 {
		s.logf("[raw][%s] [DTLS-FILTER] handshake complete — %d stale/foreign datagram(s) were dropped along the way", s.bridgeID, n)
	}
}

// shouldDropInbound reports whether a DTLS datagram cannot belong to the
// handshake we are running, and why.
//
// Two rules, both derived from the bad_certificate(42) failure where pion
// consumed a server flight that predated our ClientHello:
//
//  1. A handshake record seen before we sent our ClientHello is by definition
//     not a response to us. Consuming it makes pion verify a ServerKeyExchange
//     signed over somebody else's client_random, which always fails.
//  2. A Certificate that does not hash to the a=fingerprint in the SFU's SDP is
//     not from our peer at all — whether it comes from another endpoint sharing
//     the ICE buffer or from a stale association on a reused NAT port.
//
// Datagrams are dropped whole: a server flight arrives as one datagram holding
// several records, and accepting part of a foreign flight is not useful.
func (s *iceConnSplitter) shouldDropInbound(p []byte) (bool, string) {
	if !s.staleFilterEnabled || s.handshakeDone.Load() {
		return false, ""
	}

	for _, r := range parseDTLSRecords(p) {
		if !r.HasHandshake {
			continue
		}

		if !s.clientHelloSent.Load() {
			return true, fmt.Sprintf("inbound %s arrived before our ClientHello was sent",
				dtlsHandshakeTypeName(r.HandshakeType))
		}

		if r.HandshakeType != dtlsHandshakeTypeCertificate || s.expectedPeerFP == "" {
			continue
		}
		if !r.Complete() {
			// Cannot hash a partial message; rule 1 still guards this datagram.
			continue
		}
		der, err := peerCertFromCertificateMessage(r.Body)
		if err != nil {
			continue
		}
		if got := sha256ColonHex(der); got != s.expectedPeerFP {
			return true, fmt.Sprintf("peer certificate sha-256 %s does not match the SFU's a=fingerprint sha-256 %s",
				got, s.expectedPeerFP)
		}
	}

	return false, ""
}

// SetExpectedPeerFingerprint arms the inbound certificate check with the
// a=fingerprint value taken from the SFU's SDP. Call before the handshake
// starts; passing an empty value leaves the check disabled.
func (s *iceConnSplitter) SetExpectedPeerFingerprint(fp string) {
	s.expectedPeerFP = normalizeFingerprint(fp)
	if s.expectedPeerFP == "" {
		s.logf("[raw][%s] [DTLS-TRACE] no remote a=fingerprint available — inbound certificate check disabled", s.bridgeID)
		return
	}
	s.logf("[raw][%s] [DTLS-TRACE] expecting peer certificate fingerprint sha-256 %s", s.bridgeID, s.expectedPeerFP)
}

// traceDTLS decodes and reports every DTLS record in a datagram. dir is "IN" or
// "OUT". It never modifies or drops anything — Phase 3 adds enforcement on top
// of the same decoding.
func (s *iceConnSplitter) traceDTLS(dir string, p []byte) {
	for _, r := range parseDTLSRecords(p) {
		s.logf("[raw][%s] [DTLS-TRACE] %s %s", s.bridgeID, dir, r.String())

		if !r.HasHandshake {
			continue
		}

		if dir == "OUT" && r.HandshakeType == dtlsHandshakeTypeClientHello {
			s.clientHelloSent.Store(true)
			continue
		}

		if dir != "IN" {
			continue
		}

		// The decisive timing signal: a server flight that predates our
		// ClientHello was generated for somebody else's client_random, so its
		// ServerKeyExchange signature can never verify against ours.
		if !s.clientHelloSent.Load() {
			s.logf("[raw][%s] [DTLS-TRACE] STALE SUSPECT: inbound %s (msg_seq=%d) arrived BEFORE our ClientHello was sent",
				s.bridgeID, dtlsHandshakeTypeName(r.HandshakeType), r.MessageSeq)
		}

		if r.HandshakeType == dtlsHandshakeTypeCertificate {
			s.tracePeerCertificate(r)
		}
	}
}

// tracePeerCertificate hashes the leaf certificate out of an inbound Certificate
// message and reports whether it matches the fingerprint the SFU advertised.
func (s *iceConnSplitter) tracePeerCertificate(r dtlsRecordInfo) {
	if !r.Complete() {
		s.logf("[raw][%s] [DTLS-TRACE] inbound certificate is fragmented (offset=%d frag_len=%d msg_len=%d) — cannot fingerprint this record",
			s.bridgeID, r.FragmentOffset, r.FragmentLength, r.MessageLength)
		return
	}

	der, err := peerCertFromCertificateMessage(r.Body)
	if err != nil {
		s.logf("[raw][%s] [DTLS-TRACE] could not extract peer certificate: %v", s.bridgeID, err)
		return
	}

	got := sha256ColonHex(der)
	if s.expectedPeerFP == "" {
		s.logf("[raw][%s] [DTLS-TRACE] inbound peer certificate sha-256 %s (no expected value to compare)", s.bridgeID, got)
		return
	}

	if got == s.expectedPeerFP {
		s.logf("[raw][%s] [DTLS-TRACE] inbound peer certificate MATCHES the SFU's a=fingerprint (sha-256 %s)", s.bridgeID, got)
		return
	}

	s.logf("[raw][%s] [DTLS-TRACE] FOREIGN CERTIFICATE: inbound peer certificate sha-256 %s does NOT match the SFU's a=fingerprint sha-256 %s — this flight is not from our peer",
		s.bridgeID, got, s.expectedPeerFP)
}

// ReadFrom blocks on the underlying ice.Conn. If the packet is SRTP, it is
// buffered to srtpCh and the read loops again. Only DTLS packets are returned.
func (s *iceConnSplitter) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, err := s.conn.Read(p)
		if err != nil {
			return 0, nil, err
		}
		if n < 1 {
			continue
		}

		first := p[0]
		if first < 128 {
			// Alerts always matter — they are how a failed handshake explains
			// itself, and they can arrive long after one succeeded.
			//
			// NOTE: ice.Conn does not expose the datagram's real source address —
			// it funnels non-STUN traffic from every valid remote candidate into
			// one buffer. selectedPair below is the pair ICE nominated, NOT proof
			// of who sent this packet. Use the record trace and the certificate
			// fingerprint to identify the sender.
			if first == dtlsContentTypeAlert {
				s.logf("[raw][%s] DTLS ALERT in: %s (selectedPair.remote=%v)",
					s.bridgeID, explainAlert(p[:n]), s.conn.RemoteAddr())
			}
			// Record tracing is only informative during the handshake. After it
			// every record is encrypted application_data whose only readable
			// fields are epoch and length — one line every couple of seconds,
			// for the life of the call, saying nothing.
			if !s.handshakeDone.Load() {
				s.traceDTLS("IN", p[:n])
			}
		}
		switch {
		case first >= 20 && first <= 63: // DTLS record
			// Withhold datagrams that cannot belong to this handshake. pion/dtls
			// caches whatever it is handed and matches records to the current
			// flight by message_seq alone, so one stale flight is enough to fail
			// the handshake with bad_certificate.
			if drop, reason := s.shouldDropInbound(p[:n]); drop {
				s.staleDropped.Add(1)
				s.logf("[raw][%s] [DTLS-FILTER] dropped %d-byte datagram: %s", s.bridgeID, n, reason)
				continue
			}
			// Return to pion/dtls
			return n, s.conn.RemoteAddr(), nil

		case first >= 128 && first <= 191: // SRTP or SRTCP
			// Copy buffer because 'p' belongs to pion/dtls
			pkt := make([]byte, n)
			copy(pkt, p[:n])
			select {
			case s.srtpCh <- pkt:
			default:
				// RTP loss is acceptable — the codec recovers via PLI/FEC.
			}
			// Loop to read next packet without returning to pion/dtls
			continue

		default:
			s.logf("[raw][%s] splitter: unknown packet first_byte=0x%02x len=%d — dropped", s.bridgeID, first, n)
			continue
		}
	}
}

// WriteTo delegates directly to the underlying ice.Conn.
func (s *iceConnSplitter) WriteTo(p []byte, _ net.Addr) (int, error) {
	if len(p) > 0 {
		if p[0] == dtlsContentTypeAlert {
			s.logf("[raw][%s] DTLS ALERT out: %s", s.bridgeID, explainAlert(p))
		}
		// traceDTLS("OUT") must keep running until the handshake completes even
		// when quiet: it is what sets clientHelloSent, which the stale-flight
		// filter depends on.
		if !s.handshakeDone.Load() {
			s.traceDTLS("OUT", p)
		}
	}
	// addr is ignored — the ICE transport already knows the remote address.
	return s.conn.Write(p)
}

// Close is a NO-OP. The underlying ice.Conn lifecycle is owned by rawShadowSession.Close().
func (s *iceConnSplitter) Close() error {
	return nil
}

func (s *iceConnSplitter) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

// SetIgnoreDeadlines enables or disables deadline ignoring.
// When enabled, it clears any active deadlines on the connection immediately.
func (s *iceConnSplitter) SetIgnoreDeadlines(ignore bool) {
	s.ignoreDeadlines = ignore
	if ignore {
		_ = s.conn.SetDeadline(time.Time{})
		_ = s.conn.SetReadDeadline(time.Time{})
		_ = s.conn.SetWriteDeadline(time.Time{})
	}
}

// Set*Deadline delegate directly to the underlying ice.Conn for native kernel timeout support.
func (s *iceConnSplitter) SetDeadline(t time.Time) error {
	if s.ignoreDeadlines {
		return nil
	}
	return s.conn.SetDeadline(t)
}

func (s *iceConnSplitter) SetReadDeadline(t time.Time) error {
	if s.ignoreDeadlines {
		return nil
	}
	return s.conn.SetReadDeadline(t)
}

func (s *iceConnSplitter) SetWriteDeadline(t time.Time) error {
	if s.ignoreDeadlines {
		return nil
	}
	return s.conn.SetWriteDeadline(t)
}

// SRTPChan returns the read-only channel carrying SRTP/SRTCP datagrams.
func (s *iceConnSplitter) SRTPChan() <-chan []byte {
	return s.srtpCh
}

func explainAlert(p []byte) string {
	if len(p) < 15 {
		return fmt.Sprintf("malformed alert (len=%d)", len(p))
	}
	level := p[13]
	desc := p[14]

	levelStr := "unknown"
	switch level {
	case 1:
		levelStr = "warning"
	case 2:
		levelStr = "fatal"
	}

	descStr := "unknown"
	switch desc {
	case 0:
		descStr = "close_notify"
	case 10:
		descStr = "unexpected_message"
	case 20:
		descStr = "bad_record_mac"
	case 21:
		descStr = "decryption_failed"
	case 22:
		descStr = "record_overflow"
	case 30:
		descStr = "decompression_failure"
	case 40:
		descStr = "handshake_failure"
	case 42:
		descStr = "bad_certificate"
	case 43:
		descStr = "unsupported_certificate"
	case 44:
		descStr = "certificate_revoked"
	case 45:
		descStr = "certificate_expired"
	case 46:
		descStr = "certificate_unknown"
	case 47:
		descStr = "illegal_parameter"
	case 48:
		descStr = "unknown_ca"
	case 49:
		descStr = "access_denied"
	case 50:
		descStr = "decode_error"
	case 51:
		descStr = "decrypt_error"
	case 70:
		descStr = "protocol_version"
	case 71:
		descStr = "insufficient_security"
	case 80:
		descStr = "internal_error"
	case 90:
		descStr = "user_canceled"
	case 100:
		descStr = "no_renegotiation"
	case 110:
		descStr = "unsupported_extension"
	case 115:
		descStr = "certificate_unobtainable"
	case 116:
		descStr = "unrecognized_name"
	case 117:
		descStr = "bad_certificate_status_response"
	case 118:
		descStr = "bad_certificate_hash_value"
	case 120:
		descStr = "unknown_psk_identity"
	}

	return fmt.Sprintf("level=%d(%s) description=%d(%s)", level, levelStr, desc, descStr)
}
