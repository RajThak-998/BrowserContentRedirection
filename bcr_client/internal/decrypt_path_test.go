package engine

import (
	"bytes"
	"sync"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// srtpPair builds matched inbound/outbound contexts so a test can encrypt a
// packet the way the SFU would and feed it through the receive path.
func srtpPair(t *testing.T) (inbound, outbound *srtp.Context) {
	t.Helper()

	key := make([]byte, 16)
	salt := make([]byte, 14)
	for i := range key {
		key[i] = byte(i + 7)
	}
	for i := range salt {
		salt[i] = byte(i + 21)
	}

	var err error
	inbound, err = srtp.CreateContext(key, salt, srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		t.Fatalf("inbound CreateContext: %v", err)
	}
	outbound, err = srtp.CreateContext(key, salt, srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		t.Fatalf("outbound CreateContext: %v", err)
	}
	return inbound, outbound
}

// receivingSession wires a session so decryptAndDispatch can be driven directly
// and the packets it emits collected.
func receivingSession(t *testing.T) (*rawShadowSession, *srtp.Context, *[]*rtp.Packet, *sync.Mutex) {
	t.Helper()

	inbound, outbound := srtpPair(t)
	s := newRawShadowSession("test", func(string, ...any) {})
	s.inboundCtx = inbound

	s.mu.Lock()
	s.ptCodecMap[96] = CodecInfo{MimeType: "video/H264", ClockRate: 90000, PayloadType: 96}
	s.ptCodecMap[111] = CodecInfo{MimeType: "audio/opus", ClockRate: 48000, Channels: 2, PayloadType: 111}
	s.mu.Unlock()
	s.publishCodecSnapshot()

	var mu sync.Mutex
	got := make([]*rtp.Packet, 0, 16)
	s.onRTPPacket = func(p *rtp.Packet) {
		mu.Lock()
		// Copy the payload: the caller's backing array is a pooled buffer in
		// production and must not be retained.
		cp := &rtp.Packet{Header: p.Header, Payload: append([]byte(nil), p.Payload...)}
		got = append(got, cp)
		mu.Unlock()
	}
	return s, outbound, &got, &mu
}

func encryptRTP(t *testing.T, ctx *srtp.Context, p *rtp.Packet) []byte {
	t.Helper()
	raw, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	enc, err := ctx.EncryptRTP(nil, raw, nil)
	if err != nil {
		t.Fatalf("EncryptRTP: %v", err)
	}
	return enc
}

// The receive path now parses the RTP header once and slices the payload at the
// offset that parse reported, instead of re-running a full Unmarshal over the
// decrypted bytes. The packet handed to the relay must be identical either way
// — if the offset is ever wrong the payload is silently shifted, which decodes
// as corruption rather than failing.
func TestDecryptedPacketMatchesTheOriginal(t *testing.T) {
	s, outbound, got, mu := receivingSession(t)

	payload := []byte{0x65, 0x88, 0x84, 0x00, 0x21, 0xFF, 0xAB, 0xCD} // H264 IDR-ish
	original := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         true,
			PayloadType:    96,
			SequenceNumber: 4242,
			Timestamp:      900000,
			SSRC:           0xDEADBEEF,
		},
		Payload: payload,
	}

	s.decryptAndDispatch(encryptRTP(t, outbound, original))

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("want 1 packet delivered to the relay, got %d", len(*got))
	}
	out := (*got)[0]

	if !bytes.Equal(out.Payload, payload) {
		t.Fatalf("payload mismatch:\n got %v\nwant %v", out.Payload, payload)
	}
	if out.SSRC != original.SSRC || out.SequenceNumber != original.SequenceNumber ||
		out.Timestamp != original.Timestamp || out.PayloadType != original.PayloadType ||
		out.Marker != original.Marker {
		t.Fatalf("header mismatch:\n got %+v\nwant %+v", out.Header, original.Header)
	}
}

// A packet carrying header extensions has a larger header, so the payload
// offset differs. This is where an off-by-N in the single-parse rewrite would
// show up — Teams negotiates mid, abs-send-time and transport-cc.
func TestDecryptedPacketWithHeaderExtensions(t *testing.T) {
	s, outbound, got, mu := receivingSession(t)
	s.setTransportCCExtID(3)

	payload := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE}
	original := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 100,
			Timestamp:      3000,
			SSRC:           0x11223344,
		},
		Payload: payload,
	}
	if err := original.Header.SetExtension(3, []byte{0x00, 0x2A}); err != nil {
		t.Fatalf("SetExtension: %v", err)
	}

	s.decryptAndDispatch(encryptRTP(t, outbound, original))

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("want 1 packet, got %d", len(*got))
	}
	if !bytes.Equal((*got)[0].Payload, payload) {
		t.Fatalf("payload mismatch with extensions present:\n got %v\nwant %v", (*got)[0].Payload, payload)
	}

	// The transport-wide sequence number should have been harvested for TWCC.
	pending, _ := s.twcc.drain()
	if len(pending) != 1 || pending[0].seq != 0x2A {
		t.Fatalf("transport-cc sequence number not extracted: %+v", pending)
	}
}

// Video SSRCs must be registered from the wire so PLI and REMB can name them,
// and the tracker's clock rate must come from the codec map.
func TestReceivePathRegistersStreamState(t *testing.T) {
	s, outbound, _, _ := receivingSession(t)

	for i := 0; i < 3; i++ {
		s.decryptAndDispatch(encryptRTP(t, outbound, &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96,
				SequenceNumber: uint16(1000 + i), Timestamp: uint32(90000 * i), SSRC: 555,
			},
			Payload: []byte{0x01, 0x02},
		}))
	}

	found := false
	for _, ssrc := range s.getVideoSSRCs() {
		if ssrc == 555 {
			found = true
		}
	}
	if !found {
		t.Fatal("video SSRC was not registered from the wire")
	}

	s.incomingSeqsMu.Lock()
	tr := s.incomingSeqs[555]
	s.incomingSeqsMu.Unlock()
	if tr == nil {
		t.Fatal("no sequence tracker created")
	}
	if tr.clockRate != 90000 {
		t.Fatalf("tracker clock rate is %d, want 90000 — jitter cannot be computed without it", tr.clockRate)
	}
	if tr.received != 3 {
		t.Fatalf("tracker counted %d packets, want 3", tr.received)
	}
}

// A gap on a media stream must open pending repair entries — but must not, on
// its own, be recorded as loss. Loss is only what the engine gives up on.
func TestGapOpensRepairWithoutRecordingLoss(t *testing.T) {
	s, outbound, _, _ := receivingSession(t)

	send := func(seq uint16) {
		s.decryptAndDispatch(encryptRTP(t, outbound, &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96,
				SequenceNumber: seq, Timestamp: 90000, SSRC: 777,
			},
			Payload: []byte{0x01},
		}))
	}

	send(10)
	send(14) // 11, 12, 13 missing

	if got := s.nackEng.pendingCount(); got != 3 {
		t.Fatalf("want 3 sequence numbers pending repair, got %d", got)
	}

	st, _ := s.ssrcReg.getOrCreate(777)
	st.mu.Lock()
	lost := st.totalLost
	st.mu.Unlock()
	if lost != 0 {
		t.Fatalf("a fresh gap was recorded as %d lost packet(s); loss should only be counted when repair gives up", lost)
	}

	// The reordered packets arrive. They must clear the pending set and be
	// credited as reordering, not as loss and not as repair.
	send(11)
	send(12)
	send(13)

	if got := s.nackEng.pendingCount(); got != 0 {
		t.Fatalf("late arrivals left %d entries pending — reordering would be counted as permanent loss", got)
	}
	st.mu.Lock()
	lost, reordered := st.totalLost, st.reordered
	st.mu.Unlock()
	if lost != 0 {
		t.Fatalf("reordering was recorded as %d lost packet(s)", lost)
	}
	if reordered != 3 {
		t.Fatalf("want 3 packets credited as reordered, got %d", reordered)
	}
}

// The RTX repair stream is never itself repaired — asking the SFU to retransmit
// a retransmission is not something it can do.
func TestRTXStreamIsNotNACKed(t *testing.T) {
	s, outbound, _, _ := receivingSession(t)

	s.mu.Lock()
	s.ptCodecMap[97] = CodecInfo{MimeType: "video/rtx", ClockRate: 90000, PayloadType: 97}
	s.mu.Unlock()
	s.publishCodecSnapshot()
	s.rtxMapping.set(97, 96)
	s.rtxSSRCMu.Lock()
	s.rtxSSRCtoMedia[888] = 555
	s.rtxSSRCMu.Unlock()

	send := func(seq uint16) {
		s.decryptAndDispatch(encryptRTP(t, outbound, &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 97,
				SequenceNumber: seq, Timestamp: 90000, SSRC: 888,
			},
			Payload: []byte{0x00, 0x64, 0xFF}, // OSN 100 + one payload byte
		}))
	}

	send(1)
	send(9) // a large gap on the RTX stream itself

	if got := s.nackEng.pendingCount(); got != 0 {
		t.Fatalf("the RTX repair stream opened %d repair request(s) for itself", got)
	}
}

// An RTX packet must be decapsulated back to the media SSRC and payload type,
// and the recovery credited against the media stream rather than the RTX one.
func TestRTXRecoveryIsCreditedToTheMediaStream(t *testing.T) {
	s, outbound, got, mu := receivingSession(t)

	s.mu.Lock()
	s.ptCodecMap[97] = CodecInfo{MimeType: "video/rtx", ClockRate: 90000, PayloadType: 97}
	s.mu.Unlock()
	s.publishCodecSnapshot()
	s.rtxMapping.set(97, 96)
	s.rtxSSRCMu.Lock()
	s.rtxSSRCtoMedia[888] = 555
	s.rtxSSRCMu.Unlock()

	// Open a gap on the media stream and let it be requested.
	s.nackEng.setVideo(555, true)
	s.nackEng.recordGap(555, 100, 101)

	// The SFU retransmits seq 100 inside an RTX packet.
	s.decryptAndDispatch(encryptRTP(t, outbound, &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 97,
			SequenceNumber: 5000, Timestamp: 90000, SSRC: 888,
		},
		Payload: []byte{0x00, 0x64, 0xDE, 0xAD}, // OSN=100, then payload
	}))

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("want the reconstructed packet delivered, got %d packets", len(*got))
	}
	out := (*got)[0]
	if out.SSRC != 555 {
		t.Fatalf("reconstructed packet has SSRC %d, want the media SSRC 555", out.SSRC)
	}
	if out.PayloadType != 96 {
		t.Fatalf("reconstructed packet has PT %d, want the media PT 96", out.PayloadType)
	}
	if out.SequenceNumber != 100 {
		t.Fatalf("reconstructed packet has seq %d, want the original 100", out.SequenceNumber)
	}
	if !bytes.Equal(out.Payload, []byte{0xDE, 0xAD}) {
		t.Fatalf("OSN was not stripped: payload %v", out.Payload)
	}

	if s.nackEng.pendingCount() != 0 {
		t.Fatal("the recovered sequence number is still pending")
	}
	// Credit belongs to the media stream; crediting the RTX SSRC left
	// primary-video showing loss it had in fact repaired.
	media, _ := s.ssrcReg.getOrCreate(555)
	media.mu.Lock()
	recovered := media.nackRecovered + media.reordered
	media.mu.Unlock()
	if recovered != 1 {
		t.Fatalf("recovery was not credited to the media SSRC (got %d)", recovered)
	}
}

// The receive path slices the payload itself rather than running a second full
// rtp.Packet.Unmarshal. That shortcut is only safe if it agrees with Unmarshal
// on every input — including padded packets, where the trailing padding belongs
// to neither the payload nor a re-marshal. RTX bandwidth probes are padded, so
// this is not a hypothetical case.
func TestPayloadSlicingMatchesUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		pkt  *rtp.Packet
	}{
		{"plain", &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, SSRC: 1},
			Payload: []byte{1, 2, 3, 4, 5, 6, 7},
		}},
		{"padded", &rtp.Packet{
			Header:      rtp.Header{Version: 2, Padding: true, PayloadType: 96, SequenceNumber: 2, SSRC: 1},
			Payload:     []byte{1, 2, 3},
			PaddingSize: 4,
		}},
		{"padding only", &rtp.Packet{
			Header:      rtp.Header{Version: 2, Padding: true, PayloadType: 96, SequenceNumber: 3, SSRC: 1},
			Payload:     []byte{},
			PaddingSize: 8,
		}},
		{"csrc list", &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96, SequenceNumber: 4, SSRC: 1,
				CSRC: []uint32{0xAAAA, 0xBBBB, 0xCCCC},
			},
			Payload: []byte{9, 8, 7},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.pkt.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var viaUnmarshal rtp.Packet
			if err := viaUnmarshal.Unmarshal(raw); err != nil {
				t.Fatalf("reference Unmarshal: %v", err)
			}

			var viaSlice rtp.Packet
			headerLen, err := viaSlice.Header.Unmarshal(raw)
			if err != nil {
				t.Fatalf("Header.Unmarshal: %v", err)
			}
			if !setPayloadWithPadding(&viaSlice, raw, headerLen) {
				t.Fatal("setPayloadWithPadding rejected a packet Unmarshal accepted")
			}

			if !bytes.Equal(viaSlice.Payload, viaUnmarshal.Payload) {
				t.Fatalf("payload differs from Unmarshal:\n got %v\nwant %v", viaSlice.Payload, viaUnmarshal.Payload)
			}
			if viaSlice.PaddingSize != viaUnmarshal.PaddingSize {
				t.Fatalf("PaddingSize %d, want %d — a re-marshal would be malformed",
					viaSlice.PaddingSize, viaUnmarshal.PaddingSize)
			}

			// The packet must survive a round trip back onto the wire, which is
			// what the loopback relay does with it.
			reMarshalled, err := viaSlice.Marshal()
			if err != nil {
				t.Fatalf("re-Marshal: %v", err)
			}
			if !bytes.Equal(reMarshalled, raw) {
				t.Fatalf("round trip changed the packet:\n got %v\nwant %v", reMarshalled, raw)
			}
		})
	}
}

// A padded packet must reach the relay with the padding already removed.
func TestPaddedPacketReachesRelayWithoutPadding(t *testing.T) {
	s, outbound, got, mu := receivingSession(t)

	original := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, Padding: true, PayloadType: 96,
			SequenceNumber: 77, Timestamp: 90000, SSRC: 0xABCD,
		},
		Payload:     []byte{0x41, 0x42, 0x43},
		PaddingSize: 5,
	}

	s.decryptAndDispatch(encryptRTP(t, outbound, original))

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("want 1 packet, got %d", len(*got))
	}
	if !bytes.Equal((*got)[0].Payload, []byte{0x41, 0x42, 0x43}) {
		t.Fatalf("padding leaked into the payload: %v", (*got)[0].Payload)
	}
}

// A truncated or non-RTP datagram must be dropped without panicking — this runs
// on every inbound packet and there is no sanitising layer above it.
func TestReceivePathRejectsMalformedInput(t *testing.T) {
	s, _, got, mu := receivingSession(t)

	s.decryptAndDispatch(nil)
	s.decryptAndDispatch([]byte{0x80})
	s.decryptAndDispatch(make([]byte, 11)) // shorter than an RTP header
	s.decryptAndDispatch(make([]byte, 12)) // header-sized but not valid SRTP
	s.decryptAndDispatch(bytes.Repeat([]byte{0xFF}, 40))

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Fatalf("malformed input produced %d packet(s)", len(*got))
	}
}
