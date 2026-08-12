package engine

// nack.go — Loss detection and repair for the SFU→bcr_client (ingress) leg.
//
// This replaces the previous arrangement, in which decryptAndDispatch noticed a
// sequence gap, immediately recorded every missing number in a map, and fired
// `go sendNACK(...)` on the spot. That had five separate problems, all of which
// this file exists to fix:
//
//   1. No reorder tolerance. A packet arriving one position late was declared
//      lost and NACKed instantly. Networks reorder routinely, so a large share
//      of the repair traffic was spent asking for packets already in flight.
//
//   2. Reordering was never forgiven. Only an RTX arrival cleared an entry; a
//      packet that simply turned up late left its entry in the map forever. So
//      the missing set grew without bound for the life of the call and the
//      reported loss figure counted every reorder as a permanent loss, which
//      made the loss numbers unusable for judging anything else.
//
//   3. Single-shot requests. A sequence number was NACKed exactly once, ever,
//      because the recording step returned only newly-missing numbers. NACKs
//      and RTX replies both travel over UDP, so losing either one made the
//      packet permanently unrecoverable. This was the largest hole in repair.
//
//   4. A goroutine per loss event, on the media hot path, all contending for
//      the outbound SRTCP context.
//
//   5. Unbounded enumeration. An SSRC restart produced a gap of up to 32767,
//      and every one of those numbers was allocated into the missing set even
//      though only the first 100 could ever fit in a NACK packet.
//
// The model here is the conventional one: a gap opens a *pending* entry, the
// entry is held briefly in case the packet is merely reordered, then requested,
// then re-requested on a timer until it either arrives or ages out. A single
// goroutine on a short tick does all the sending, coalescing everything pending
// for one SSRC into one packet.

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	// nackReorderGrace is how long a missing sequence number is tolerated
	// before it is treated as lost and requested. Anything arriving inside this
	// window costs nothing at all — no NACK is sent, no loss is recorded.
	//
	// Sized against ordinary network reordering rather than against the jitter
	// buffer: reordering is a few milliseconds, and waiting longer than that
	// just delays a repair that is genuinely needed.
	nackReorderGrace = 15 * time.Millisecond

	// nackRetryInterval is the gap between successive requests for the same
	// sequence number. It stands in for a round trip to the SFU; asking again
	// sooner than the reply could arrive only wastes uplink.
	nackRetryInterval = 60 * time.Millisecond

	// nackMaxRequests bounds how many times one sequence number is asked for
	// before it is written off. Four attempts across nackRetryInterval covers
	// the realistic case of a NACK or an RTX reply being dropped once or twice.
	nackMaxRequests = 4

	// nackMaxAge is the ceiling on how long a sequence number stays pending
	// regardless of attempts. Past this the receiver's jitter buffer has moved
	// on and the packet would be discarded even if it arrived, so continuing to
	// ask for it spends uplink on something that cannot help.
	nackMaxAge = 500 * time.Millisecond

	// nackTickInterval is how often the sender goroutine drains pending
	// requests. It bounds the delay added to a repair on top of the reorder
	// grace, and is short enough that grace and retry timings stay meaningful.
	nackTickInterval = 20 * time.Millisecond

	// nackMaxGap is the largest run of missing sequence numbers worth
	// enumerating. Above this the stream has not dropped a few packets, it has
	// restarted or been cut — a keyframe is the only useful repair, and
	// enumerating tens of thousands of numbers to request the first 100 of them
	// is pure waste.
	nackMaxGap = 256

	// nackMaxPendingPerSSRC caps the pending set for one stream. Reached only
	// under loss severe enough that repair is hopeless; it exists so a
	// pathological stream cannot grow the map without limit.
	nackMaxPendingPerSSRC = 1024

	// nackMaxSeqsPerPacket bounds one NACK packet. Each NackPair is 4 bytes, so
	// 100 sequence numbers is at most 400 bytes plus header — comfortably below
	// the MTU even in the worst case where no numbers share a bitmask.
	nackMaxSeqsPerPacket = 100
)

// nackEntry is one sequence number believed missing.
type nackEntry struct {
	firstSeen time.Time // when the gap was noticed — starts the reorder grace
	lastSent  time.Time // when it was last requested; zero if never
	sends     int       // how many times it has been requested
}

// nackStream is the pending set for one inbound SSRC.
type nackStream struct {
	pending map[uint16]*nackEntry
	isVideo bool

	// keyframeRequested suppresses repeated keyframe escalations while a stream
	// is continuously failing. Cleared once the stream has nothing pending, so
	// a later independent failure can escalate again.
	keyframeRequested bool
}

// nackEngine owns loss detection and repair for all inbound SSRCs of one
// session. Safe for concurrent use: the SRTP read loop reports arrivals and
// gaps, and a single internal goroutine does all the sending.
type nackEngine struct {
	mu      sync.Mutex
	streams map[uint32]*nackStream

	// totalPending mirrors the total size of every stream's pending set. It is
	// read on the media hot path by observe() to skip the mutex entirely while
	// nothing is outstanding, which is the overwhelmingly common case.
	totalPending atomic.Int64

	// sendNACK transmits one coalesced request. Called only from the engine's
	// own goroutine, so the caller need not be reentrant.
	sendNACK func(ssrc uint32, seqs []uint16)

	// requestKeyframe escalates a stream that cannot be repaired by
	// retransmission.
	requestKeyframe func(ssrc uint32)

	// onGaveUp reports sequence numbers written off as unrecoverable, so the
	// caller can account them as real loss. This is the ONLY thing that should
	// increment a loss counter — a gap that later resolves was never loss.
	onGaveUp func(ssrc uint32, count int)

	// onRequested reports how many sequence numbers were asked for, including
	// retries of numbers already requested.
	onRequested func(ssrc uint32, count int)

	logf func(string, ...any)
}

func newNACKEngine(logf func(string, ...any)) *nackEngine {
	return &nackEngine{
		streams: make(map[uint32]*nackStream),
		logf:    logf,
	}
}

// setVideo marks an SSRC as carrying video, which makes it eligible for
// keyframe escalation when retransmission fails.
func (n *nackEngine) setVideo(ssrc uint32, isVideo bool) {
	n.mu.Lock()
	n.streamLocked(ssrc).isVideo = isVideo
	n.mu.Unlock()
}

// streamLocked returns the stream state for ssrc, creating it if absent.
// MUST be called with n.mu held.
func (n *nackEngine) streamLocked(ssrc uint32) *nackStream {
	st, ok := n.streams[ssrc]
	if !ok {
		st = &nackStream{pending: make(map[uint16]*nackEntry)}
		n.streams[ssrc] = st
	}
	return st
}

// recordGap registers sequence numbers [from, to) as possibly missing on ssrc.
//
// "Possibly" is the important word: nothing is requested here and nothing is
// counted as lost. The entries begin a reorder grace period, and only survive
// it if the packets really are absent.
//
// Returns true when the gap was too large to enumerate, in which case the
// caller should treat the stream as having restarted rather than as having lost
// a run of packets.
func (n *nackEngine) recordGap(ssrc uint32, from, to uint16) (tooLarge bool) {
	span := int(to - from) // uint16 arithmetic: correct across wrap-around
	if span <= 0 {
		return false
	}
	if span > nackMaxGap {
		n.mu.Lock()
		st := n.streamLocked(ssrc)
		dropped := len(st.pending)
		st.pending = make(map[uint16]*nackEntry)
		isVideo := st.isVideo
		st.keyframeRequested = isVideo
		n.mu.Unlock()

		if dropped > 0 {
			n.totalPending.Add(int64(-dropped))
			if n.onGaveUp != nil {
				n.onGaveUp(ssrc, dropped)
			}
		}
		n.logf("[NACK] SSRC=%d sequence jumped %d→%d (%d packets) — too large to repair by retransmission; %s",
			ssrc, from, to, span,
			map[bool]string{true: "requesting a keyframe", false: "resynchronising"}[isVideo])
		if isVideo && n.requestKeyframe != nil {
			n.requestKeyframe(ssrc)
		}
		return true
	}

	now := time.Now()
	added := 0

	n.mu.Lock()
	st := n.streamLocked(ssrc)
	for seq := from; seq != to; seq++ {
		if _, exists := st.pending[seq]; exists {
			continue
		}
		if len(st.pending) >= nackMaxPendingPerSSRC {
			break
		}
		st.pending[seq] = &nackEntry{firstSeen: now}
		added++
	}
	n.mu.Unlock()

	if added > 0 {
		n.totalPending.Add(int64(added))
	}
	return false
}

// observe reports that a sequence number arrived on ssrc, whether via the
// normal stream (late) or via RTX. If it was pending it is cleared and true is
// returned.
//
// This is the fix for treating reordering as permanent loss: the previous code
// cleared entries only on RTX arrival, so a packet that merely turned up late
// stayed "missing" forever. It is called on the media hot path, so it does
// nothing at all — not even a lock acquisition — while the session has no
// outstanding requests.
// cleared reports whether the sequence number was outstanding; wasRequested
// distinguishes a genuine repair (we asked, the SFU resent) from plain
// reordering that resolved inside the grace window and cost nothing.
func (n *nackEngine) observe(ssrc uint32, seq uint16) (cleared, wasRequested bool) {
	if n.totalPending.Load() == 0 {
		return false, false
	}

	n.mu.Lock()
	st, ok := n.streams[ssrc]
	if !ok {
		n.mu.Unlock()
		return false, false
	}
	e, pending := st.pending[seq]
	if pending {
		wasRequested = e.sends > 0
		delete(st.pending, seq)
		if len(st.pending) == 0 {
			st.keyframeRequested = false
		}
	}
	n.mu.Unlock()

	if pending {
		n.totalPending.Add(-1)
	}
	return pending, wasRequested
}

// forget drops all state for an SSRC — used when a stream is retired.
func (n *nackEngine) forget(ssrc uint32) {
	n.mu.Lock()
	st, ok := n.streams[ssrc]
	if ok {
		n.totalPending.Add(int64(-len(st.pending)))
		delete(n.streams, ssrc)
	}
	n.mu.Unlock()
}

// run drives the engine until ctx-equivalent done channel closes. All NACK
// transmission happens here, on one goroutine, so requests for a given SSRC go
// out in order and the outbound SRTCP context is touched from a single place.
func (n *nackEngine) run(done <-chan struct{}) {
	ticker := time.NewTicker(nackTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			n.tick()
		}
	}
}

// nackBatch is one SSRC's worth of work produced by a tick.
type nackBatch struct {
	ssrc         uint32
	seqs         []uint16
	gaveUp       int
	wantKeyframe bool
}

// tick advances every pending entry: requests those past the reorder grace,
// re-requests those whose last attempt has gone unanswered, and writes off
// those that have run out of attempts or time.
func (n *nackEngine) tick() {
	now := time.Now()
	var batches []nackBatch

	n.mu.Lock()
	for ssrc, st := range n.streams {
		if len(st.pending) == 0 {
			continue
		}

		batch := nackBatch{ssrc: ssrc}
		for seq, e := range st.pending {
			age := now.Sub(e.firstSeen)

			// Written off: out of attempts, or too old for the receiver's
			// jitter buffer to still want it.
			if e.sends >= nackMaxRequests || age > nackMaxAge {
				delete(st.pending, seq)
				batch.gaveUp++
				continue
			}

			// Still inside the reorder grace — the packet may yet arrive on its
			// own, and asking now would be premature.
			if age < nackReorderGrace {
				continue
			}

			// Already asked recently; give the reply time to arrive.
			if e.sends > 0 && now.Sub(e.lastSent) < nackRetryInterval {
				continue
			}

			if len(batch.seqs) < nackMaxSeqsPerPacket {
				batch.seqs = append(batch.seqs, seq)
				e.sends++
				e.lastSent = now
			}
		}

		if batch.gaveUp > 0 && st.isVideo && !st.keyframeRequested {
			// Retransmission has failed for this stream. A keyframe is the only
			// remaining way to resynchronise the decoder, and is far cheaper
			// than leaving it to decode from a broken reference chain.
			st.keyframeRequested = true
			batch.wantKeyframe = true
		}
		if len(st.pending) == 0 {
			st.keyframeRequested = false
		}

		if batch.gaveUp > 0 {
			n.totalPending.Add(int64(-batch.gaveUp))
		}
		if len(batch.seqs) > 0 || batch.gaveUp > 0 || batch.wantKeyframe {
			batches = append(batches, batch)
		}
	}
	n.mu.Unlock()

	// Everything below is I/O and callbacks — deliberately outside the lock so
	// the SRTP read loop is never blocked behind an encrypt-and-write.
	for _, b := range batches {
		if len(b.seqs) > 0 {
			if n.onRequested != nil {
				n.onRequested(b.ssrc, len(b.seqs))
			}
			if n.sendNACK != nil {
				n.sendNACK(b.ssrc, b.seqs)
			}
		}
		if b.gaveUp > 0 && n.onGaveUp != nil {
			n.onGaveUp(b.ssrc, b.gaveUp)
		}
		if b.wantKeyframe && n.requestKeyframe != nil {
			n.requestKeyframe(b.ssrc)
		}
	}
}

// pendingCount reports how many sequence numbers are currently outstanding
// across all streams. Diagnostics only.
func (n *nackEngine) pendingCount() int64 {
	return n.totalPending.Load()
}
