package engine

import (
	"bytes"
	"testing"
)

func pkt(seq uint16, marker byte) []byte {
	b := make([]byte, 32)
	b[0] = marker
	b[1] = byte(seq)
	b[2] = byte(seq >> 8)
	return b
}

func TestRingBufferStoresAndRetrieves(t *testing.T) {
	rb := newRTPRingBuffer()

	for seq := uint16(1); seq <= 10; seq++ {
		rb.store(seq, pkt(seq, 0xAA))
	}
	for seq := uint16(1); seq <= 10; seq++ {
		got, ok := rb.retrieve(seq)
		if !ok {
			t.Fatalf("seq %d missing from the cache", seq)
		}
		if !bytes.Equal(got, pkt(seq, 0xAA)) {
			t.Fatalf("seq %d came back with the wrong bytes", seq)
		}
	}
}

// Slots are indexed by seq modulo the buffer size, so a late packet carrying an
// old sequence number lands on the slot of a packet rtpCacheSize ahead of it.
// Storing it would evict a current packet in favour of one already delivered —
// and the NACK most likely to arrive next is for the current one.
func TestLateStoreDoesNotEvictCurrentPacket(t *testing.T) {
	rb := newRTPRingBuffer()

	current := uint16(rtpCacheSize + 5) // aliases slot 5
	rb.store(current, pkt(current, 0xCC))

	// An RTX-recovered packet from before the window, aliasing the same slot.
	stale := uint16(5)
	rb.store(stale, pkt(stale, 0x55))

	got, ok := rb.retrieve(current)
	if !ok {
		t.Fatal("the current packet was evicted by a late out-of-order store")
	}
	if got[0] != 0xCC {
		t.Fatalf("slot holds the stale packet (marker 0x%02X) instead of the current one", got[0])
	}
	if _, ok := rb.retrieve(stale); ok {
		t.Fatal("the stale packet was cached, shadowing the current one")
	}
}

// Wrap-around must be handled as forward progress, not as a huge backwards jump.
func TestRingBufferAcceptsSequenceWrap(t *testing.T) {
	rb := newRTPRingBuffer()

	rb.store(65534, pkt(65534, 0x11))
	rb.store(65535, pkt(65535, 0x22))
	rb.store(0, pkt(0, 0x33))
	rb.store(1, pkt(1, 0x44))

	for _, seq := range []uint16{65534, 65535, 0, 1} {
		if _, ok := rb.retrieve(seq); !ok {
			t.Fatalf("seq %d rejected across the wrap point", seq)
		}
	}
}

// Retrieving a sequence number the slot no longer holds must miss cleanly
// rather than returning whichever packet aliases it.
func TestRingBufferMissesOnAliasedSlot(t *testing.T) {
	rb := newRTPRingBuffer()

	rb.store(7, pkt(7, 0x11))
	rb.store(7+rtpCacheSize, pkt(7+rtpCacheSize, 0x22)) // same slot, newer

	if _, ok := rb.retrieve(7); ok {
		t.Fatal("retrieve returned the evicted packet for a stale sequence number")
	}
	if got, ok := rb.retrieve(7 + rtpCacheSize); !ok || got[0] != 0x22 {
		t.Fatal("the newer packet should occupy the slot")
	}
}

// Slot buffers are reused across stores. A reused slot must not leak bytes of
// the packet it previously held when the new one is shorter.
func TestRingBufferReuseDoesNotLeakOldBytes(t *testing.T) {
	rb := newRTPRingBuffer()

	long := make([]byte, 64)
	for i := range long {
		long[i] = 0xFF
	}
	rb.store(1, long)

	short := []byte{0x01, 0x02, 0x03}
	rb.store(1+rtpCacheSize, short) // same slot

	got, ok := rb.retrieve(1 + rtpCacheSize)
	if !ok {
		t.Fatal("packet missing after slot reuse")
	}
	if len(got) != len(short) {
		t.Fatalf("reused slot reported length %d, want %d — old bytes leaked", len(got), len(short))
	}
	if !bytes.Equal(got, short) {
		t.Fatalf("reused slot returned %v, want %v", got, short)
	}
}

// The pool hands out buffers holding a copy of the source. It must never alias
// the caller's slice — the whole point is that pion/dtls reuses its read buffer.
func TestSRTPBufferPoolCopies(t *testing.T) {
	src := []byte{1, 2, 3, 4, 5}
	bp := getSRTPBuffer(src)

	if !bytes.Equal(*bp, src) {
		t.Fatalf("pooled buffer holds %v, want %v", *bp, src)
	}

	src[0] = 0xFF
	if (*bp)[0] == 0xFF {
		t.Fatal("pooled buffer aliases the source slice")
	}

	putSRTPBuffer(bp)

	// A recycled buffer must come back sized to the new content, not to the old.
	next := []byte{9, 9}
	bp2 := getSRTPBuffer(next)
	if len(*bp2) != 2 || !bytes.Equal(*bp2, next) {
		t.Fatalf("recycled buffer returned %v (len %d), want %v", *bp2, len(*bp2), next)
	}
	putSRTPBuffer(bp2)
}

// A datagram larger than the pooled capacity must still be served correctly.
func TestSRTPBufferPoolHandlesOversizedDatagram(t *testing.T) {
	big := make([]byte, muxDatagramCap*2)
	for i := range big {
		big[i] = byte(i)
	}

	bp := getSRTPBuffer(big)
	if !bytes.Equal(*bp, big) {
		t.Fatal("oversized datagram was truncated or corrupted")
	}
	// Returning it must not poison the pool with an oversized buffer.
	putSRTPBuffer(bp)

	small := []byte{1}
	bp2 := getSRTPBuffer(small)
	if cap(*bp2) > muxDatagramCap*2 {
		t.Fatal("an oversized buffer was retained in the pool")
	}
	putSRTPBuffer(bp2)
}
