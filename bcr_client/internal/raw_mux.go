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
	"time"

	"github.com/pion/ice/v4"
)

const (
	muxSRTPBufSize = 2048 // media is high-frequency; larger buffer prevents drops

	dtlsContentTypeAlert = 21 // DTLS record content type for alerts
)

// iceConnSplitter wraps an ice.Conn to implement net.PacketConn for pion/dtls,
// while inline-diverting SRTP packets to a separate channel.
type iceConnSplitter struct {
	conn            *ice.Conn
	srtpCh          chan []byte
	logf            func(string, ...any)
	bridgeID        string
	ignoreDeadlines bool
}

func newIceConnSplitter(conn *ice.Conn, logf func(string, ...any), bridgeID string) *iceConnSplitter {
	return &iceConnSplitter{
		conn:     conn,
		srtpCh:   make(chan []byte, muxSRTPBufSize),
		logf:     logf,
		bridgeID: bridgeID,
	}
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
			// NOTE: ice.Conn does not expose the datagram's real source address —
			// it funnels non-STUN traffic from every valid remote candidate into
			// one buffer. selectedPair below is the pair ICE nominated, NOT proof
			// of who sent this packet. Use the record trace and the certificate
			// fingerprint to identify the sender.
			if first == dtlsContentTypeAlert {
				s.logf("[raw][%s] [VDI-DEBUG] splitter ReadFrom: read DTLS ALERT: %s, selectedPair.remote=%v",
					s.bridgeID, explainAlert(p[:n]), s.conn.RemoteAddr())
			} else {
				s.logf("[raw][%s] [VDI-DEBUG] splitter ReadFrom: read %d bytes, first_byte=0x%02x, selectedPair.remote=%v",
					s.bridgeID, n, first, s.conn.RemoteAddr())
			}
		}
		switch {
		case first >= 20 && first <= 63: // DTLS record
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
			s.logf("[raw][%s] [VDI-DEBUG] splitter WriteTo: writing DTLS ALERT: %s", s.bridgeID, explainAlert(p))
		} else {
			s.logf("[raw][%s] [VDI-DEBUG] splitter WriteTo: writing %d bytes, first_byte=0x%02x", s.bridgeID, len(p), p[0])
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
		s.logf("[raw][%s] [VDI-DEBUG] splitter SetIgnoreDeadlines: active. Clearing all connection deadlines.", s.bridgeID)
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
