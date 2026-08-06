package engine

// relay.go — Bridges raw decrypted RTP from the shadow transport to the
// loopback PeerConnection which feeds the Wails WebView <video> element.

import (
	"strings"
	"sync/atomic"

	"github.com/pion/rtp"
)

// loopbackWriteCount tracks total packets written to loopback for sampled logging.
var loopbackWriteCount uint64

// Sampled RTP packet counters for diagnostic logging.
var (
	rtpAudioCount uint64
	rtpVideoCount uint64
)

// onRawRTPPacket is the entry point for each inbound decrypted RTP packet from
// the rawShadowSession's SRTP read loop. It routes the packet to the loopback session:
//
//  1. Looks up the codec for the packet's payload type in ptCodecMap.
//  2. Creates the loopbackSession on first call for a bridge.
//  3. Writes every subsequent packet to the loopback session.
func (e *Engine) onRawRTPPacket(bridgeID string, pkt *rtp.Packet, ptCodecMap map[uint8]CodecInfo) {
	if !e.shouldProcessBridgeTrack(bridgeID) {
		return
	}

	// There is deliberately no codec allowlist here any more.
	//
	// This used to drop any payload type outside a preferred set (H264/Opus).
	// It never narrowed anything — Teams offers H264 under seven payload types
	// and all seven passed the name check — while being fail-closed, so an SFU
	// choosing anything unexpected produced silence rather than a picture. The
	// loopback negotiates its own codec with the receiver regardless of which
	// payload type the SFU used, so the only question worth asking is whether
	// we can map this codec at all, and loopbackSession.WriteRTP asks it.
	codec, ok := ptCodecMap[pkt.Header.PayloadType]
	if ok {
		// First packet per track type only — enough to confirm the stream
		// started and on which SSRC/PT. Rate and loss thereafter are reported
		// by [MEDIA-SUMMARY] every 5s; sampling here as well just interleaved
		// several lines a second into the same log.
		if strings.HasPrefix(strings.ToLower(codec.MimeType), "audio/") {
			if atomic.AddUint64(&rtpAudioCount, 1) == 1 {
				e.logf("[raw][%s] first RTP audio: SSRC=%d PT=%d codec=%s seq=%d",
					bridgeID, pkt.SSRC, pkt.PayloadType, codec.MimeType, pkt.SequenceNumber)
			}
		} else if strings.HasPrefix(strings.ToLower(codec.MimeType), "video/") {
			if atomic.AddUint64(&rtpVideoCount, 1) == 1 {
				e.logf("[raw][%s] first RTP video: SSRC=%d PT=%d codec=%s seq=%d",
					bridgeID, pkt.SSRC, pkt.PayloadType, codec.MimeType, pkt.SequenceNumber)
			}
		}
	}

	if !ok {
		// Log the first occurrence of each unknown PT.
		e.unknownPTMu.Lock()
		if _, seen := e.unknownPTs[pkt.Header.PayloadType]; !seen {
			if e.unknownPTs == nil {
				e.unknownPTs = make(map[uint8]bool)
			}
			e.unknownPTs[pkt.Header.PayloadType] = true
			e.logf("[bcr_client][webm] unknown PT=%d SSRC=%d (not in ptCodecMap, %d codecs registered) bridgeId=%s",
				pkt.Header.PayloadType, pkt.SSRC, len(ptCodecMap), bridgeID)
		}
		e.unknownPTMu.Unlock()
		return
	}

	// Get loopback session — created eagerly at SRTP-ready time by
	// ensureLoopbackSession (called from promoteActiveBridge). If it is
	// missing here, something went wrong in the startup sequence; drop
	// the packet and emit a one-time warning so the log is not flooded.
	e.relayMu.Lock()
	session := e.loopbackSessions[bridgeID]
	e.relayMu.Unlock()

	if session == nil {
		e.unknownPTMu.Lock()
		if e.unknownPTs == nil {
			e.unknownPTs = make(map[uint8]bool)
		}
		if !e.unknownPTs[255] { // sentinel key for "no-session" warning
			e.unknownPTs[255] = true
			e.logf("[raw][%s] [WARN] no loopback session found for bridgeId=%s — RTP dropped PT=%d — eager creation may have failed",
				bridgeID, bridgeID, pkt.Header.PayloadType)
		}
		e.unknownPTMu.Unlock()
		return
	}

	// Forward the decrypted RTP packet to the loopback PeerConnection, together
	// with the codec it was negotiated as — that is what decides whether it is
	// video or audio, and so which loopback track it belongs on.
	session.WriteRTP(pkt, codec)

	// One line confirming the relay reached the loopback at all. Throughput
	// after that is [MEDIA-SUMMARY]'s job.
	if atomic.AddUint64(&loopbackWriteCount, 1) == 1 {
		e.logf("[loopback][relay] first packet written to loopback bridgeId=%s PT=%d SSRC=%d",
			bridgeID, pkt.Header.PayloadType, pkt.SSRC)
	}
}
