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
	"net"
	"time"

	"github.com/pion/ice/v4"
)

const (
	muxSRTPBufSize = 2048 // media is high-frequency; larger buffer prevents drops
)

// iceConnSplitter wraps an ice.Conn to implement net.PacketConn for pion/dtls,
// while inline-diverting SRTP packets to a separate channel.
type iceConnSplitter struct {
	conn     *ice.Conn
	srtpCh   chan []byte
	logf     func(string, ...any)
	bridgeID string
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
			s.logf("[raw][%s] [VDI-DEBUG] splitter ReadFrom: read %d bytes, first_byte=0x%02x, remote=%v", s.bridgeID, n, first, s.conn.RemoteAddr())
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
		s.logf("[raw][%s] [VDI-DEBUG] splitter WriteTo: writing %d bytes, first_byte=0x%02x", s.bridgeID, len(p), p[0])
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

// Set*Deadline delegate directly to the underlying ice.Conn for native kernel timeout support.
func (s *iceConnSplitter) SetDeadline(t time.Time) error {
	return s.conn.SetDeadline(t)
}

func (s *iceConnSplitter) SetReadDeadline(t time.Time) error {
	return s.conn.SetReadDeadline(t)
}

func (s *iceConnSplitter) SetWriteDeadline(t time.Time) error {
	return s.conn.SetWriteDeadline(t)
}

// SRTPChan returns the read-only channel carrying SRTP/SRTCP datagrams.
func (s *iceConnSplitter) SRTPChan() <-chan []byte {
	return s.srtpCh
}
