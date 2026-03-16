package main

import (
	"encoding/binary"
	"encoding/json"
	"log"
)

// overlayManager is the single OverlayManager instance.
// Initialised in main() before the WebSocket client starts.
var overlayManager *OverlayManager

// HandlePacket routes a decoded Packet to the appropriate logger
// and forwards geometry events to the OverlayManager.
//
// Called from the WebSocket goroutine — must NOT touch GLFW/OpenGL directly.
// All window manipulation goes through overlayManager → geomCh → render loop.
func HandlePacket(p Packet) {
	switch p.Type {
	case "VIDEO_ADDED":
		var payload AddedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			log.Printf("[handler] VIDEO_ADDED — failed to decode payload: %v", err)
			return
		}
		log.Printf("[handler] VIDEO_ADDED   id=%s  ts=%d", payload.ID, payload.Timestamp)
		overlayManager.Create(payload.ID)

	case "VIDEO_UPDATE":
		var payload UpdatePayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			log.Printf("[handler] VIDEO_UPDATE — failed to decode payload: %v", err)
			return
		}
		log.Printf("[handler] VIDEO_UPDATE  id=%s  screen=(%.0f,%.0f)  size=(%.0f×%.0f)  state=%s  ratio=%.2f",
			payload.ID,
			payload.ScreenBounds.X, payload.ScreenBounds.Y,
			payload.ScreenBounds.Width, payload.ScreenBounds.Height,
			payload.Playback.State,
			payload.Visibility.IntersectionRatio,
		)
		// Use ScreenBounds for GLFW — screen-absolute pixels.
		overlayManager.Update(payload.ID, payload.ScreenBounds, payload.Visibility)

	case "VIDEO_REMOVED":
		var payload RemovedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			log.Printf("[handler] VIDEO_REMOVED — failed to decode payload: %v", err)
			return
		}
		log.Printf("[handler] VIDEO_REMOVED id=%s  ts=%d", payload.ID, payload.Timestamp)
		overlayManager.Destroy(payload.ID)

	case "MEDIA_CHUNK_LOG":
		var payload MediaChunkLogPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			log.Printf("[handler] MEDIA_CHUNK_LOG — failed to decode payload: %v", err)
			return
		}

		// Meta is optional; decode best-effort.
		var meta Meta
		if len(p.Meta) > 0 {
			if err := json.Unmarshal(p.Meta, &meta); err != nil {
				log.Printf("[handler] MEDIA_CHUNK_LOG — failed to decode meta: %v", err)
			}
		}

		if payload.Seq%25 == 0 || payload.IsInitSegment {
			log.Printf("[handler] MEDIA_CHUNK_LOG seq=%d size=%d track=%s init=%v sb=%s",
				payload.Seq, payload.Size, payload.TrackType, payload.IsInitSegment, payload.SourceBufferID)
		}

		log.Printf("[handler] MEDIA_CHUNK_LOG seq=%d size=%d track=%s",
			payload.Seq, payload.Size, payload.TrackType)
		LogMediaChunkLog("bcr_client_main", payload, meta)

	default:
		log.Printf("[handler] unknown packet type: %q", p.Type)
	}
}

// HandleMediaBinaryFrame parses [u32 headerLen LE][headerJSON][rawChunk]
// and logs validated media chunk info. No decode/buffering yet.
func HandleMediaBinaryFrame(data []byte) {
	if len(data) < 4 {
		log.Printf("[handler] MEDIA_BINARY frame too small (%d bytes)", len(data))
		return
	}

	headerLen := int(binary.LittleEndian.Uint32(data[:4]))
	if headerLen <= 0 || 4+headerLen > len(data) {
		log.Printf("[handler] MEDIA_BINARY invalid header length=%d frame=%d", headerLen, len(data))
		return
	}

	headerBytes := data[4 : 4+headerLen]
	chunkBytes := data[4+headerLen:]

	var hdr MediaChunkFrameHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		log.Printf("[handler] MEDIA_BINARY header decode failed: %v", err)
		return
	}

	if hdr.Type != "MEDIA_CHUNK" {
		log.Printf("[handler] MEDIA_BINARY unexpected type=%q", hdr.Type)
		return
	}

	if hdr.Payload.Size != len(chunkBytes) {
		log.Printf("[handler] MEDIA_BINARY size mismatch header=%d actual=%d",
			hdr.Payload.Size, len(chunkBytes))
	}

	trackKey := BuildTrackKey(hdr.Payload.TrackType, hdr.Payload.SourceBufferID, hdr.Payload.Codec)

	snapshot, shouldLog := mediaBufferManager.StoreChunk(
		trackKey,
		hdr.Payload.Seq,
		hdr.Payload.IsInitSegment,
		chunkBytes,
	)

	// Keep logs lightweight: only init events + sampled snapshots.
	if hdr.Payload.IsInitSegment {
		log.Printf("[handler] MEDIA_BINARY INIT key=%s seq=%d size=%d",
			trackKey, hdr.Payload.Seq, len(chunkBytes))
	}

	if shouldLog {
		LogMediaBufferSnapshot(snapshot)
	}
}
