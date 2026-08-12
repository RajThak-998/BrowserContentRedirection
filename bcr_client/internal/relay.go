package engine

// relay.go — Bridges raw decrypted RTP from the shadow transport to the
// loopback PeerConnection which feeds the Wails WebView <video> element.

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pion/rtp"
)

// firstPacketSeen records which "first packet of this kind" lines have already
// been emitted, per bridge.
//
// These used to be three package-level counters incremented with atomic.AddUint64
// and compared against 1. Being process-global rather than per-bridge, every one
// of them fired exactly once for the lifetime of bcr_client — so the "first RTP
// audio", "first RTP video" and "first packet written to loopback" lines
// appeared for the first call of a session and never again. Those lines are the
// primary evidence that media reached the relay at all, and they were missing
// from precisely the situation where they are most needed: the second call,
// after something had already gone wrong once.
var (
	firstPacketMu   sync.Mutex
	firstPacketSeen = map[string]bool{}
)

// noteFirst reports whether this is the first time (bridgeID, event) has been
// seen, and records it.
func noteFirst(bridgeID, event string) bool {
	key := bridgeID + "/" + event
	firstPacketMu.Lock()
	defer firstPacketMu.Unlock()
	if firstPacketSeen[key] {
		return false
	}
	firstPacketSeen[key] = true
	return true
}

// forgetFirsts clears a bridge's first-packet records so a later session on the
// same bridge reports its own firsts. Called from closeShadowSession.
func forgetFirsts(bridgeID string) {
	prefix := bridgeID + "/"
	firstPacketMu.Lock()
	for k := range firstPacketSeen {
		if strings.HasPrefix(k, prefix) {
			delete(firstPacketSeen, k)
		}
	}
	firstPacketMu.Unlock()
}

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
			if noteFirst(bridgeID, "rtp-audio") {
				e.logf("[raw][%s] first RTP audio: SSRC=%d PT=%d codec=%s seq=%d",
					bridgeID, pkt.SSRC, pkt.PayloadType, codec.MimeType, pkt.SequenceNumber)
			}
		} else if strings.HasPrefix(strings.ToLower(codec.MimeType), "video/") {
			if noteFirst(bridgeID, "rtp-video") {
				e.logf("[raw][%s] first RTP video: SSRC=%d PT=%d codec=%s seq=%d",
					bridgeID, pkt.SSRC, pkt.PayloadType, codec.MimeType, pkt.SequenceNumber)
			}
		}
	}

	if !ok {
		// First occurrence of each unknown PT, per bridge. Keying on the
		// payload type alone meant a second bridge hitting the same unknown PT
		// stayed silent about it.
		if noteFirst(bridgeID, fmt.Sprintf("unknown-pt-%d", pkt.Header.PayloadType)) {
			e.logf("[bcr_client][webm] unknown PT=%d SSRC=%d (not in ptCodecMap, %d codecs registered) bridgeId=%s",
				pkt.Header.PayloadType, pkt.SSRC, len(ptCodecMap), bridgeID)
		}
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
		// Keyed per bridge rather than on a shared sentinel entry in the
		// unknown-PT map, which meant a second bridge hitting this could never
		// report it.
		if noteFirst(bridgeID, "no-loopback-session") {
			e.logf("[raw][%s] [WARN] no loopback session found for bridgeId=%s — RTP dropped PT=%d — eager creation may have failed",
				bridgeID, bridgeID, pkt.Header.PayloadType)
		}
		return
	}

	// Forward the decrypted RTP packet to the loopback PeerConnection, together
	// with the codec it was negotiated as — that is what decides whether it is
	// video or audio, and so which loopback track it belongs on.
	session.WriteRTP(pkt, codec)

	// One line confirming the relay reached the loopback at all. Throughput
	// after that is [MEDIA-SUMMARY]'s job.
	if noteFirst(bridgeID, "loopback-write") {
		e.logf("[loopback][relay] first packet written to loopback bridgeId=%s PT=%d SSRC=%d",
			bridgeID, pkt.Header.PayloadType, pkt.SSRC)
	}
}
