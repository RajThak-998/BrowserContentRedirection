package engine

// raw_mux.go — DTLS/SRTP packet demultiplexer and PacketConn adapter.
//
// The Teams SFU sends both DTLS handshake records and SRTP media datagrams to
// the same UDP 5-tuple that the ICE agent established. This file splits that
// stream into two separate channels per RFC 5764 §5.1.2:
//
//   first_byte ∈ [20, 63]   → DTLS record layer
//   first_byte ∈ [128, 191] → SRTP or SRTCP
//   first_byte ∈ [0, 3]     → STUN (handled by ice.Agent before we see the payload)
//
// dtlsPacketConnAdapter wraps the DTLS channel into a net.PacketConn so that
// dtls.Client() / dtls.Server() can consume it without knowing about the mux.
//
// ── Why Set*Deadline are NO-OPs ──────────────────────────────────────────────
// pion/dtls v3 wraps our adapter in netctx.NewPacketConn (from pion/transport/v3).
// netctx.ReadFromContext implements context cancellation by calling
//   p.nextConn.SetReadDeadline(veryOld)
// on the underlying conn when the context fires. If we forward that call to
// iceConn, it kills the mux readLoop (which reads from the same iceConn on a
// separate goroutine), draining dtlsCh and making ALL subsequent ReadFrom calls
// return EOF instantly — so pion/dtls gets "context.Canceled" rather than real
// server data, the DTLS handshake silently fails, and dtls.Client() returns nil
// with an uninitialized cipher suite (srtpProfile=0, ConnectionState ok=false).
//
// Our ReadFrom blocks on a Go channel, NOT on iceConn, so SetReadDeadline on
// iceConn has zero effect on unblocking it anyway. Cancellation is handled
// exclusively via the adapter's own `closed` channel, closed by Close().

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/ice/v4"
)

const (
	muxDTLSBufSize = 256  // DTLS retransmits; a small buffer is fine
	muxSRTPBufSize = 2048 // media is high-frequency; larger buffer prevents drops
	muxMaxPktSize  = 4096 // largest expected UDP payload (MTU headroom)
)

// dtlsSRTPMux reads from an ice.Conn and routes datagrams to one of two
// buffered channels based on the first byte of the payload.
type dtlsSRTPMux struct {
	iceConn  *ice.Conn
	dtlsCh   chan []byte
	srtpCh   chan []byte
	stopOnce sync.Once
	logf     func(string, ...any)
	bridgeID string
}

func newDTLSSRTPMux(iceConn *ice.Conn, logf func(string, ...any), bridgeID string) *dtlsSRTPMux {
	m := &dtlsSRTPMux{
		iceConn:  iceConn,
		dtlsCh:   make(chan []byte, muxDTLSBufSize),
		srtpCh:   make(chan []byte, muxSRTPBufSize),
		logf:     logf,
		bridgeID: bridgeID,
	}
	go m.readLoop()
	return m
}

func (m *dtlsSRTPMux) readLoop() {
	// Closing both channels on exit lets downstream consumers (DTLS adapter and
	// SRTP loop) exit cleanly via range/ok-check idioms.
	defer func() {
		close(m.dtlsCh)
		close(m.srtpCh)
	}()

	buf := make([]byte, muxMaxPktSize)
	for {
		n, err := m.iceConn.Read(buf)
		if err != nil {
			if err != io.EOF {
				m.logf("[raw][%s] mux: iceConn read error: %v", m.bridgeID, err)
			}
			return
		}
		if n < 1 {
			continue
		}

		// Copy before placing on channel — buf is reused in the next iteration.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		first := pkt[0]
		switch {
		case first >= 20 && first <= 63: // DTLS record
			m.logf("[raw][%s] mux: DTLS pkt first=0x%02x len=%d → dtlsCh", m.bridgeID, first, n)
			select {
			case m.dtlsCh <- pkt:
			default:
				// DTLS has its own retransmission; dropping here is safe.
				m.logf("[raw][%s] mux: DTLS channel full, dropping packet (DTLS will retransmit)", m.bridgeID)
			}

		case first >= 128 && first <= 191: // SRTP or SRTCP
			select {
			case m.srtpCh <- pkt:
			default:
				// RTP loss is acceptable — the codec recovers via PLI/FEC.
			}

		default:
			m.logf("[raw][%s] mux: unknown packet first_byte=0x%02x len=%d — dropped",
				m.bridgeID, first, n)
		}
	}
}

// SRTPChan returns the read-only channel carrying SRTP/SRTCP datagrams.
func (m *dtlsSRTPMux) SRTPChan() <-chan []byte { return m.srtpCh }

// DTLSPipe returns a net.PacketConn that presents only the DTLS side of the mux
// to dtls.Client() / dtls.Server(). Writes go directly to the ice.Conn so DTLS
// handshake records reach the remote peer.
func (m *dtlsSRTPMux) DTLSPipe() net.PacketConn {
	return &dtlsPacketConnAdapter{
		mux:    m,
		closed: make(chan struct{}),
	}
}

// dtlsPacketConnAdapter implements net.PacketConn over the DTLS channel of a
// dtlsSRTPMux. It satisfies the interface required by dtls.Client / dtls.Server.
//
// ReadFrom blocks until a DTLS packet is available on the channel, the channel
// is closed, or Close() is called. WriteTo delegates to the underlying ice.Conn.
// Set*Deadline are deliberate NO-OPs — see package-level comment for rationale.
type dtlsPacketConnAdapter struct {
	mux       *dtlsSRTPMux
	closed    chan struct{}
	closeOnce sync.Once
}

func (a *dtlsPacketConnAdapter) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case pkt, ok := <-a.mux.dtlsCh:
		if !ok {
			return 0, nil, io.EOF
		}
		n := copy(p, pkt)
		return n, a.mux.iceConn.RemoteAddr(), nil
	case <-a.closed:
		return 0, nil, net.ErrClosed
	}
}

func (a *dtlsPacketConnAdapter) WriteTo(p []byte, _ net.Addr) (int, error) {
	// addr is ignored — the ICE transport already knows the remote address.
	return a.mux.iceConn.Write(p)
}

// Close signals ReadFrom to unblock and return net.ErrClosed. It does NOT touch
// the underlying iceConn or the mux readLoop — those are owned by rawShadowSession.
func (a *dtlsPacketConnAdapter) Close() error {
	a.closeOnce.Do(func() { close(a.closed) })
	return nil
}

func (a *dtlsPacketConnAdapter) LocalAddr() net.Addr {
	return a.mux.iceConn.LocalAddr()
}

// Set*Deadline intentionally do nothing. See package-level comment.
func (a *dtlsPacketConnAdapter) SetDeadline(t time.Time) error      { return nil }
func (a *dtlsPacketConnAdapter) SetReadDeadline(t time.Time) error  { return nil }
func (a *dtlsPacketConnAdapter) SetWriteDeadline(t time.Time) error { return nil }

