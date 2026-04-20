package engine

// relay.go — Bridges raw decrypted RTP from the shadow transport to the WebM muxer.

import (
	"github.com/pion/rtp"
)

// onRawRTPPacket is the entry point for each inbound decrypted RTP packet from
// the rawShadowSession's SRTP read loop. It routes the packet to the muxer layer:
//
//  1. Looks up the codec for the packet's payload type in ptCodecMap.
//  2. Creates the webmMuxer on first call for a bridge.
//  3. Writes every subsequent packet to the muxer.
func (e *Engine) onRawRTPPacket(bridgeID string, pkt *rtp.Packet, ptCodecMap map[uint8]CodecInfo) {
	if !e.shouldProcessBridgeTrack(bridgeID) {
		return
	}

	// Look up codec for this packet's payload type.
	_, ok := ptCodecMap[pkt.Header.PayloadType]
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

	// Get or create muxer session.
	e.relayMu.Lock()
	muxer, ok := e.webmMuxers[bridgeID]
	if !ok {
		muxer = newWebMMuxer(bridgeID, e.logf, e.cb.OnVideoChunk, ptCodecMap)
		e.webmMuxers[bridgeID] = muxer
	}
	e.relayMu.Unlock()

	// Forward the decrypted RTP packet to the muxer
	muxer.WriteRTP(pkt, ptCodecMap)
}
