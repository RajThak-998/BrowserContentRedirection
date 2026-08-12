package engine

import (
	"sync"
	"testing"

	"github.com/pion/rtcp"
	"github.com/pion/srtp/v3"
)

// countingWriter stands in for the ICE connection.
type countingWriter struct {
	mu     sync.Mutex
	writes int
	bytes  int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	w.bytes += len(p)
	w.mu.Unlock()
	return len(p), nil
}

func (w *countingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// sessionWithSRTP builds a session wired to a real srtp.Context and a fake
// transport, so the outbound RTCP path can be driven end to end.
func sessionWithSRTP(t *testing.T) (*rawShadowSession, *countingWriter) {
	t.Helper()

	// AES_CM_128_HMAC_SHA1_80: 16-byte key, 14-byte salt.
	key := make([]byte, 16)
	salt := make([]byte, 14)
	for i := range key {
		key[i] = byte(i + 1)
	}
	for i := range salt {
		salt[i] = byte(i + 100)
	}

	ctx, err := srtp.CreateContext(key, salt, srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		t.Fatalf("CreateContext: %v", err)
	}

	s := newRawShadowSession("test", func(string, ...any) {})
	w := &countingWriter{}
	s.outboundCtx = ctx
	s.mediaWriter = w
	return s, w
}

// THE regression test for the crash.
//
// pion's srtp.Context keeps its per-SSRC state in plain maps with no internal
// locking, so two goroutines encrypting at once is a concurrent map access —
// which the Go runtime turns into an unrecoverable fatal error, not a catchable
// panic. Four RTCP producers existed and only three took the write mutex; the
// one that skipped it, the TWCC loop, was also the most frequent at 10Hz.
//
// This drives all four producers concurrently at far above their real rates.
// Under -race it fails on any unsynchronised access; without -race a
// regression would show up as the test binary dying outright.
func TestConcurrentRTCPSendersAreSerialised(t *testing.T) {
	s, w := sessionWithSRTP(t)

	const perProducer = 300
	var wg sync.WaitGroup

	send := func(build func(i int) []rtcp.Packet, label string) {
		defer wg.Done()
		for i := 0; i < perProducer; i++ {
			s.sendRTCP(build(i), label)
		}
	}

	wg.Add(4)

	// Heartbeat: compound RR + SDES.
	go send(func(int) []rtcp.Packet {
		return []rtcp.Packet{
			&rtcp.ReceiverReport{SSRC: 1000},
			rtcp.NewCNAMESourceDescription(1000, "bcr-test"),
		}
	}, "heartbeat")

	// TWCC — the producer that used to bypass the lock entirely.
	go send(func(i int) []rtcp.Packet {
		return []rtcp.Packet{&rtcp.TransportLayerCC{
			SenderSSRC:         1000,
			MediaSSRC:          2000,
			BaseSequenceNumber: uint16(i),
			PacketStatusCount:  1,
			FbPktCount:         uint8(i),
			PacketChunks: []rtcp.PacketStatusChunk{&rtcp.StatusVectorChunk{
				SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
				SymbolList: []uint16{rtcp.TypeTCCPacketReceivedSmallDelta},
			}},
			RecvDeltas: []*rtcp.RecvDelta{{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: 250}},
		}}
	}, "twcc")

	// PLI.
	go send(func(int) []rtcp.Packet {
		return []rtcp.Packet{&rtcp.PictureLossIndication{SenderSSRC: 1000, MediaSSRC: 2000}}
	}, "pli")

	// NACK.
	go send(func(i int) []rtcp.Packet {
		return []rtcp.Packet{&rtcp.TransportLayerNack{
			SenderSSRC: 1000,
			MediaSSRC:  2000,
			Nacks:      []rtcp.NackPair{{PacketID: uint16(i), LostPackets: 0}},
		}}
	}, "nack")

	wg.Wait()

	if got := w.count(); got != 4*perProducer {
		t.Fatalf("want %d datagrams written, got %d", 4*perProducer, got)
	}
}

// SRTCP carries a monotonically increasing index that the peer uses for replay
// protection. If two encryptions interleave, indices collide and the peer drops
// the feedback silently. Encrypting the same packet repeatedly must produce
// distinct ciphertexts, which is only true if the index really advanced.
func TestSRTCPIndexAdvancesUnderConcurrency(t *testing.T) {
	s, _ := sessionWithSRTP(t)

	var mu sync.Mutex
	seen := make(map[string]bool)
	collisions := 0

	s.mediaWriter = writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		k := string(p)
		if seen[k] {
			collisions++
		}
		seen[k] = true
		mu.Unlock()
		return len(p), nil
	})

	const n = 200
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				// Byte-identical plaintext every time, so any duplicate
				// ciphertext means the SRTCP index did not advance.
				s.sendRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{SenderSSRC: 1000, MediaSSRC: 2000},
				}, "pli")
			}
		}()
	}
	wg.Wait()

	if collisions != 0 {
		t.Fatalf("%d duplicate SRTCP datagram(s) — the index counter was raced and the peer would drop them", collisions)
	}
	if len(seen) != 4*n {
		t.Fatalf("want %d distinct datagrams, got %d", 4*n, len(seen))
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// sendRTCP is called from loops that start before the transport is up and keep
// running as it goes away. It must be inert, not panic, when either half of the
// outbound path is missing.
func TestSendRTCPBeforeTransportIsReady(t *testing.T) {
	s := newRawShadowSession("test", func(string, ...any) {})

	// No context, no writer.
	s.sendRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{}}, "pli")

	// Context but no writer.
	withCtx, _ := sessionWithSRTP(t)
	withCtx.mediaWriter = nil
	withCtx.sendRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{}}, "pli")

	// Empty packet list.
	ready, w := sessionWithSRTP(t)
	ready.sendRTCP(nil, "empty")
	if w.count() != 0 {
		t.Fatal("an empty packet list produced a write")
	}
}
