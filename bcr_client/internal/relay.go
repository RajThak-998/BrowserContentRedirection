package engine

// relay.go — Bridges raw decrypted RTP from the shadow transport to the WebM muxer.

import (
	"strings"
	"sync/atomic"

	"github.com/pion/rtp"
)

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

	// Look up codec for this packet's payload type.
	codec, ok := ptCodecMap[pkt.Header.PayloadType]
	if ok {
		// Sampled diagnostic logging — log every 100th packet per track type.
		if strings.HasPrefix(strings.ToLower(codec.MimeType), "audio/") {
			n := atomic.AddUint64(&rtpAudioCount, 1)
			if n == 1 || n%100 == 0 {
				e.logf("[raw][%s] RTP audio packet #%d SSRC=%d PT=%d codec=%s seq=%d ts=%d len=%d",
					bridgeID, n, pkt.SSRC, pkt.PayloadType, codec.MimeType, pkt.SequenceNumber, pkt.Timestamp, len(pkt.Payload))
			}
		} else if strings.HasPrefix(strings.ToLower(codec.MimeType), "video/") {
			n := atomic.AddUint64(&rtpVideoCount, 1)
			if n == 1 || n%100 == 0 {
				e.logf("[raw][%s] RTP video packet #%d SSRC=%d PT=%d codec=%s seq=%d ts=%d len=%d",
					bridgeID, n, pkt.SSRC, pkt.PayloadType, codec.MimeType, pkt.SequenceNumber, pkt.Timestamp, len(pkt.Payload))
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

	// Get or create loopback session.
	e.relayMu.Lock()
	session, ok := e.loopbackSessions[bridgeID]
	if !ok {
		session = newLoopbackSession(bridgeID, e.logf, e.cb.OnLoopbackOffer, ptCodecMap)
		e.loopbackSessions[bridgeID] = session
	}
	e.relayMu.Unlock()

	// Forward the decrypted RTP packet to the loopback session
	session.WriteRTP(pkt, ptCodecMap)
}
