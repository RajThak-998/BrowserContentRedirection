package main

import (
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
		log.Printf("[handler] VIDEO_UPDATE  id=%s  pos=(%.0f,%.0f)  size=(%.0f×%.0f)  state=%s  ratio=%.2f",
			payload.ID,
			payload.Bounds.X, payload.Bounds.Y,
			payload.Bounds.Width, payload.Bounds.Height,
			payload.Playback.State,
			payload.Visibility.IntersectionRatio,
		)
		// Pass both Bounds and Visibility — manager decides show/hide.
		overlayManager.Update(payload.ID, payload.Bounds, payload.Visibility)

	case "VIDEO_REMOVED":
		var payload RemovedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			log.Printf("[handler] VIDEO_REMOVED — failed to decode payload: %v", err)
			return
		}
		log.Printf("[handler] VIDEO_REMOVED id=%s  ts=%d", payload.ID, payload.Timestamp)
		overlayManager.Destroy(payload.ID)

	default:
		log.Printf("[handler] unknown packet type: %q", p.Type)
	}
}
