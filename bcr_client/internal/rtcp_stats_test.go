package engine

import (
	"math"
	"sync"
	"testing"
	"time"
)

// The previous implementation fed time.Now().UnixMicro() into updateJitter and
// multiplied it by the clock rate. At 90kHz that is ~1.6e20, which overflows
// int64 and wraps to an arbitrary value — so the Jitter field of every Receiver
// Report the client ever sent was noise. This asserts the arithmetic stays in
// range and produces the value RFC 3550 §A.8 defines.
func TestJitterDoesNotOverflowAtVideoClockRate(t *testing.T) {
	const clockRate = 90000
	tr := &seqTracker{clockRate: clockRate}

	// A perfectly paced 30fps stream: 33333µs and 3000 ticks between packets.
	// With no timing variation at all, jitter must converge on zero.
	var arrivalUs int64
	var rtpTS uint32
	for i := 0; i < 200; i++ {
		tr.updateJitter(arrivalUs, rtpTS)
		arrivalUs += 33333
		rtpTS += 3000
	}

	if math.IsNaN(tr.jitter) || math.IsInf(tr.jitter, 0) {
		t.Fatalf("jitter is not a finite number: %v", tr.jitter)
	}
	if tr.jitter < 0 {
		t.Fatalf("jitter went negative (%v) — the arithmetic wrapped", tr.jitter)
	}
	// One clock tick of slack for the µs→ticks rounding; the old overflowing
	// version produced values in the billions here.
	if tr.jitter > 2 {
		t.Fatalf("evenly paced stream reported jitter %v; expected ~0", tr.jitter)
	}
}

// Same check for the audio clock rate, which also overflowed.
func TestJitterDoesNotOverflowAtAudioClockRate(t *testing.T) {
	const clockRate = 48000
	tr := &seqTracker{clockRate: clockRate}

	var arrivalUs int64
	var rtpTS uint32
	for i := 0; i < 500; i++ {
		tr.updateJitter(arrivalUs, rtpTS)
		arrivalUs += 20000 // 20ms Opus frames
		rtpTS += 960
	}

	if tr.jitter < 0 || tr.jitter > 2 {
		t.Fatalf("evenly paced audio reported jitter %v; expected ~0", tr.jitter)
	}
}

// Real timing variation must actually register, or the metric is useless in the
// other direction.
func TestJitterRisesWithTimingVariation(t *testing.T) {
	const clockRate = 90000
	tr := &seqTracker{clockRate: clockRate}

	var arrivalUs int64
	var rtpTS uint32
	for i := 0; i < 200; i++ {
		tr.updateJitter(arrivalUs, rtpTS)
		step := int64(33333)
		if i%2 == 0 {
			step += 15000 // alternating early/late delivery
		} else {
			step -= 15000
		}
		arrivalUs += step
		rtpTS += 3000
	}

	if tr.jitter <= 1 {
		t.Fatalf("a stream with ±15ms of variation reported jitter %v; expected clearly positive", tr.jitter)
	}
}

// A Receiver Report's RC field is 5 bits, so more than 31 report blocks cannot
// be expressed and rtcp.Marshal rejects the whole compound packet. With Teams
// renegotiating repeatedly, the tracker map can pass 31 entries.
func TestReceptionReportsCappedAtProtocolLimit(t *testing.T) {
	s := newRawShadowSession("test", func(string, ...any) {})
	s.incomingSeqs = make(map[uint32]*seqTracker)

	// 50 live SSRCs, with ascending packet counts so the busiest are known.
	for i := uint32(1); i <= 50; i++ {
		s.incomingSeqs[i] = &seqTracker{
			clockRate:    90000,
			received:     i, // higher SSRC == busier
			lastPacketAt: time.Now(),
		}
	}

	reports := s.buildReceptionReports()

	if len(reports) != maxReceptionReports {
		t.Fatalf("want %d report blocks (the 5-bit RC ceiling), got %d", maxReceptionReports, len(reports))
	}
	// The busiest streams are the ones worth reporting on.
	for _, r := range reports {
		if r.SSRC <= 50-uint32(maxReceptionReports) {
			t.Fatalf("report includes idle SSRC %d while busier ones were dropped", r.SSRC)
		}
	}
}

// Trackers for streams that have gone silent must be discarded, or a call full
// of renegotiations accumulates dead SSRCs for its whole duration.
func TestStaleTrackersArePruned(t *testing.T) {
	s := newRawShadowSession("test", func(string, ...any) {})
	s.incomingSeqs = map[uint32]*seqTracker{
		1: {clockRate: 90000, lastPacketAt: time.Now()},
		2: {clockRate: 90000, lastPacketAt: time.Now().Add(-staleSSRCTimeout - time.Second)},
		3: {clockRate: 48000, lastPacketAt: time.Now()},
	}

	reports := s.buildReceptionReports()

	if len(reports) != 2 {
		t.Fatalf("want 2 reports after pruning the silent stream, got %d", len(reports))
	}
	s.incomingSeqsMu.Lock()
	_, stillThere := s.incomingSeqs[2]
	s.incomingSeqsMu.Unlock()
	if stillThere {
		t.Fatal("the silent tracker was reported on but not removed")
	}
}

// A tracker created before its SDP arrived has clockRate 0 and can never report
// jitter. The first packet that resolves the codec must repair it.
func TestZeroValueTrackerIsNotPrunedAsStale(t *testing.T) {
	s := newRawShadowSession("test", func(string, ...any) {})
	// lastPacketAt is the zero time — a tracker that exists but has not been
	// stamped yet must not be mistaken for one that has been silent since 1970.
	s.incomingSeqs = map[uint32]*seqTracker{1: {clockRate: 90000}}

	if got := len(s.buildReceptionReports()); got != 1 {
		t.Fatalf("a freshly created tracker was pruned as stale: %d reports", got)
	}
}

// mergeCodecsAndSSRCs used to assign the remote SDP's extracted extension ID
// unconditionally. A renegotiation answer that simply omitted the extmap then
// reset this to 0 mid-call, silently disabling TWCC and downgrading congestion
// control to REMB for the rest of the session.
func TestTransportCCExtIDSurvivesAnSDPWithoutTheExtmap(t *testing.T) {
	s := newRawShadowSession("test", func(string, ...any) {})

	s.setTransportCCExtID(3)
	if got := s.transportCCExtID.Load(); got != 3 {
		t.Fatalf("want extension ID 3, got %d", got)
	}

	// A later SDP with no transport-cc extmap parses to 0.
	s.setTransportCCExtID(0)
	if got := s.transportCCExtID.Load(); got != 3 {
		t.Fatalf("a renegotiation without the extmap cleared the ID to %d", got)
	}

	// A genuine change is still honoured.
	s.setTransportCCExtID(5)
	if got := s.transportCCExtID.Load(); got != 5 {
		t.Fatalf("a real ID change was ignored: got %d", got)
	}
}

// The media path reads codecs through an immutable snapshot. Publishing must
// actually make new entries visible, and the returned map must be a copy — a
// reader mutating it would corrupt every other reader.
func TestCodecSnapshotIsPublishedAndIsolated(t *testing.T) {
	s := newRawShadowSession("test", func(string, ...any) {})

	if len(s.codecs()) != 0 {
		t.Fatal("a fresh session should publish an empty codec map, not a nil one")
	}

	s.mu.Lock()
	s.ptCodecMap[96] = CodecInfo{MimeType: "video/H264", ClockRate: 90000}
	s.mu.Unlock()
	s.publishCodecSnapshot()

	snap := s.codecs()
	if got, ok := snap[96]; !ok || got.ClockRate != 90000 {
		t.Fatalf("published snapshot missing PT 96: %+v", snap)
	}

	// A later mutation must not be visible until it is republished, which is
	// what makes the snapshot safe to read without a lock.
	s.mu.Lock()
	s.ptCodecMap[97] = CodecInfo{MimeType: "audio/opus", ClockRate: 48000}
	s.mu.Unlock()
	if _, leaked := snap[97]; leaked {
		t.Fatal("the snapshot aliases ptCodecMap — readers would race with the signalling goroutine")
	}

	s.publishCodecSnapshot()
	if _, ok := s.codecs()[97]; !ok {
		t.Fatal("republishing did not make the new codec visible")
	}
}

// The concurrency the snapshot exists to make safe: the signalling goroutine
// republishing while the media path reads. Run under -race this is the
// regression test for the original concurrent-map panic.
func TestCodecSnapshotUnderConcurrentPublish(t *testing.T) {
	s := newRawShadowSession("test", func(string, ...any) {})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for pt := uint8(96); ; pt++ {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			s.ptCodecMap[pt] = CodecInfo{MimeType: "video/H264", ClockRate: 90000}
			s.mu.Unlock()
			s.publishCodecSnapshot()
			if pt > 125 {
				pt = 95
			}
		}
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for pt, info := range s.codecs() {
					_, _ = pt, info
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// senderSSRCFor used to always prefer the video SSRC once video was negotiated,
// so every audio NACK identified us as the video m-section's participant.
func TestSenderSSRCMatchesTheStreamsMediaKind(t *testing.T) {
	s := newRawShadowSession("test", func(string, ...any) {})
	s.mu.Lock()
	s.localSenderSSRC = 1111 // audio
	s.localVideoSSRC = 2222
	s.mu.Unlock()

	videoStream, _ := s.ssrcReg.getOrCreate(10)
	videoStream.role = rolePrimVideo
	audioStream, _ := s.ssrcReg.getOrCreate(20)
	audioStream.role = roleAudio

	if got := s.senderSSRCFor(10); got != 2222 {
		t.Fatalf("video NACK should be sent from the video SSRC 2222, got %d", got)
	}
	if got := s.senderSSRCFor(20); got != 1111 {
		t.Fatalf("audio NACK should be sent from the audio SSRC 1111, got %d", got)
	}
	// An SSRC we know nothing about falls back to audio rather than guessing video.
	if got := s.senderSSRCFor(999); got != 1111 {
		t.Fatalf("unknown SSRC should fall back to 1111, got %d", got)
	}
}
