package engine

import (
	"sync"
	"testing"
	"time"
)

// recorder captures what an engine asked for, so tests can assert on behaviour
// rather than on internal state.
type recorder struct {
	mu        sync.Mutex
	requested map[uint16]int // seq → how many times it was requested
	gaveUp    int
	keyframes int
}

func newRecorder() *recorder {
	return &recorder{requested: make(map[uint16]int)}
}

func (r *recorder) attach(n *nackEngine) {
	n.sendNACK = func(_ uint32, seqs []uint16) {
		r.mu.Lock()
		for _, s := range seqs {
			r.requested[s]++
		}
		r.mu.Unlock()
	}
	n.onGaveUp = func(_ uint32, count int) {
		r.mu.Lock()
		r.gaveUp += count
		r.mu.Unlock()
	}
	n.requestKeyframe = func(uint32) {
		r.mu.Lock()
		r.keyframes++
		r.mu.Unlock()
	}
}

func (r *recorder) counts() (distinct, total, gaveUp, keyframes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.requested {
		distinct++
		total += c
	}
	return distinct, total, r.gaveUp, r.keyframes
}

func (r *recorder) timesRequested(seq uint16) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requested[seq]
}

func testEngine(t *testing.T) (*nackEngine, *recorder) {
	t.Helper()
	n := newNACKEngine(func(string, ...any) {})
	r := newRecorder()
	r.attach(n)
	return n, r
}

// A packet that is merely reordered must cost nothing: no NACK, no loss.
// Sending one immediately was the old behaviour and produced a large share of
// the repair traffic on an otherwise healthy link.
func TestReorderWithinGraceIsNotRequested(t *testing.T) {
	n, r := testEngine(t)

	n.recordGap(1, 100, 103) // 100, 101, 102 missing

	// The reordered packets turn up before the grace period elapses.
	for _, seq := range []uint16{100, 101, 102} {
		cleared, wasRequested := n.observe(1, seq)
		if !cleared {
			t.Fatalf("seq %d should have been pending", seq)
		}
		if wasRequested {
			t.Fatalf("seq %d was requested despite arriving inside the reorder grace", seq)
		}
	}

	n.tick()

	if distinct, _, gaveUp, _ := r.counts(); distinct != 0 || gaveUp != 0 {
		t.Fatalf("reordering produced repair traffic: requested=%d gaveUp=%d", distinct, gaveUp)
	}
	if n.pendingCount() != 0 {
		t.Fatalf("pending set not drained: %d", n.pendingCount())
	}
}

// Genuine loss must be requested once the grace has elapsed.
func TestLossIsRequestedAfterGrace(t *testing.T) {
	n, r := testEngine(t)

	n.recordGap(1, 100, 103)

	n.tick() // too early — still inside the grace window
	if distinct, _, _, _ := r.counts(); distinct != 0 {
		t.Fatalf("requested %d seq(s) before the reorder grace elapsed", distinct)
	}

	time.Sleep(nackReorderGrace + 5*time.Millisecond)
	n.tick()

	distinct, total, _, _ := r.counts()
	if distinct != 3 || total != 3 {
		t.Fatalf("want 3 sequence numbers requested once each, got distinct=%d total=%d", distinct, total)
	}
}

// The single-shot behaviour was the biggest hole in repair: one lost NACK or
// one lost RTX reply made the packet unrecoverable. Requests must repeat.
func TestRequestsAreRetried(t *testing.T) {
	n, r := testEngine(t)

	n.recordGap(1, 500, 501)
	time.Sleep(nackReorderGrace + 5*time.Millisecond)

	n.tick() // first request
	if got := r.timesRequested(500); got != 1 {
		t.Fatalf("after first tick want 1 request, got %d", got)
	}

	// An immediate re-tick must not re-ask; the reply has not had time to land.
	n.tick()
	if got := r.timesRequested(500); got != 1 {
		t.Fatalf("re-requested before the retry interval elapsed: %d requests", got)
	}

	time.Sleep(nackRetryInterval + 5*time.Millisecond)
	n.tick()
	if got := r.timesRequested(500); got != 2 {
		t.Fatalf("after the retry interval want 2 requests, got %d", got)
	}
}

// A sequence number that never comes back must eventually be written off, and
// counted as loss exactly once.
func TestGivesUpAndCountsLossOnce(t *testing.T) {
	n, r := testEngine(t)
	n.setVideo(1, true)

	n.recordGap(1, 900, 901)

	deadline := time.Now().Add(2 * time.Second)
	for n.pendingCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(nackTickInterval)
		n.tick()
	}

	if n.pendingCount() != 0 {
		t.Fatal("entry never aged out of the pending set")
	}
	_, total, gaveUp, keyframes := r.counts()
	if gaveUp != 1 {
		t.Fatalf("want exactly 1 sequence number written off, got %d", gaveUp)
	}
	if total > nackMaxRequests {
		t.Fatalf("requested %d times, more than the %d-attempt cap", total, nackMaxRequests)
	}
	if keyframes != 1 {
		t.Fatalf("a video stream that lost a packet unrecoverably should escalate to exactly one keyframe, got %d", keyframes)
	}
}

// Recovery via retransmission must be distinguishable from reordering.
func TestObserveDistinguishesRepairFromReorder(t *testing.T) {
	n, _ := testEngine(t)

	n.recordGap(1, 10, 11)
	time.Sleep(nackReorderGrace + 5*time.Millisecond)
	n.tick() // now requested

	cleared, wasRequested := n.observe(1, 10)
	if !cleared || !wasRequested {
		t.Fatalf("a packet recovered after being requested should report both: cleared=%v wasRequested=%v", cleared, wasRequested)
	}
}

// An SSRC restart is not a run of lost packets. Enumerating tens of thousands
// of sequence numbers to request the first hundred is pure waste, and the old
// code allocated every one of them.
func TestOversizedGapDoesNotEnumerate(t *testing.T) {
	n, r := testEngine(t)
	n.setVideo(7, true)

	if tooLarge := n.recordGap(7, 0, 30000); !tooLarge {
		t.Fatal("a 30000-packet jump should be reported as too large to repair")
	}
	if n.pendingCount() != 0 {
		t.Fatalf("oversized gap enumerated %d entries", n.pendingCount())
	}
	if _, _, _, keyframes := r.counts(); keyframes != 1 {
		t.Fatalf("an oversized gap on video should request a keyframe, got %d", keyframes)
	}
}

// The pending set must not grow without bound on a pathological stream.
func TestPendingSetIsBounded(t *testing.T) {
	n, _ := testEngine(t)

	// Repeated in-range gaps, more than the per-SSRC cap in total.
	for base := 0; base < 40; base++ {
		from := uint16(base * nackMaxGap)
		n.recordGap(3, from, from+nackMaxGap)
	}

	if got := n.pendingCount(); got > nackMaxPendingPerSSRC {
		t.Fatalf("pending set grew to %d, above the %d cap", got, nackMaxPendingPerSSRC)
	}
}

// Sequence numbers wrap; a gap spanning 65535→0 must enumerate correctly rather
// than being read as a 65000-packet backwards jump.
func TestGapAcrossSequenceWrap(t *testing.T) {
	n, _ := testEngine(t)

	if tooLarge := n.recordGap(1, 65534, 2); tooLarge {
		t.Fatal("a 4-packet gap across the wrap point was misread as oversized")
	}
	if got := n.pendingCount(); got != 4 {
		t.Fatalf("want 4 pending (65534, 65535, 0, 1), got %d", got)
	}
	for _, seq := range []uint16{65534, 65535, 0, 1} {
		if cleared, _ := n.observe(1, seq); !cleared {
			t.Fatalf("seq %d should have been pending across the wrap", seq)
		}
	}
}

// observe is called for a large share of inbound packets, so it must cost
// nothing at all when there is no outstanding repair.
func TestObserveIsFreeWhenNothingPending(t *testing.T) {
	n, _ := testEngine(t)

	if cleared, _ := n.observe(1, 42); cleared {
		t.Fatal("observe reported a clear with an empty pending set")
	}
	if n.pendingCount() != 0 {
		t.Fatal("observe mutated state when nothing was pending")
	}
}

// forget must release the pending accounting, not just the map entry —
// otherwise totalPending drifts up and observe stops taking its fast path.
func TestForgetReleasesPendingCount(t *testing.T) {
	n, _ := testEngine(t)

	n.recordGap(9, 1, 6)
	if n.pendingCount() != 5 {
		t.Fatalf("want 5 pending, got %d", n.pendingCount())
	}

	n.forget(9)
	if n.pendingCount() != 0 {
		t.Fatalf("forget left %d phantom pending entries", n.pendingCount())
	}
}
