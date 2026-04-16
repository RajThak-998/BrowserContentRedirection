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
	return &dtlsPacketConnAdapter{mux: m}
}

// dtlsPacketConnAdapter implements net.PacketConn over the DTLS channel of a
// dtlsSRTPMux. It satisfies the interface required by dtls.Client / dtls.Server.
//
// ReadFrom blocks until a DTLS packet is available on the channel (or the
// channel is closed). WriteTo delegates to the underlying ice.Conn.Write so
// DTLS records are transmitted on the established UDP 5-tuple.
type dtlsPacketConnAdapter struct {
	mux *dtlsSRTPMux
}

func (a *dtlsPacketConnAdapter) ReadFrom(p []byte) (int, net.Addr, error) {
	pkt, ok := <-a.mux.dtlsCh
	if !ok {
		return 0, nil, io.EOF
	}
	n := copy(p, pkt)
	return n, a.mux.iceConn.RemoteAddr(), nil
}

func (a *dtlsPacketConnAdapter) WriteTo(p []byte, _ net.Addr) (int, error) {
	// addr is ignored — the ICE transport already knows the remote address.
	return a.mux.iceConn.Write(p)
}

// Close is a deliberate no-op. The rawShadowSession owns the lifecycle of both
// the ice.Conn and the mux; closing this adapter must not prematurely shut them down.
func (a *dtlsPacketConnAdapter) Close() error { return nil }

func (a *dtlsPacketConnAdapter) LocalAddr() net.Addr {
	return a.mux.iceConn.LocalAddr()
}

func (a *dtlsPacketConnAdapter) SetDeadline(t time.Time) error {
	return a.mux.iceConn.SetDeadline(t)
}

func (a *dtlsPacketConnAdapter) SetReadDeadline(t time.Time) error {
	return a.mux.iceConn.SetReadDeadline(t)
}

func (a *dtlsPacketConnAdapter) SetWriteDeadline(t time.Time) error {
	return a.mux.iceConn.SetWriteDeadline(t)
}
