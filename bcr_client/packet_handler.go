package main

import (
    "encoding/json"
    "log"
)

// HandlePacket routes a decoded Packet to the appropriate logger.
// It performs no state transformation and contains no rendering logic.
// Future overlay integration hooks belong here as additional calls
// after the log statements.
func HandlePacket(clientID string, pkt Packet) {
    // Decode meta once — shared across all packet types.
    var meta Meta
    if pkt.Meta != nil {
        if err := json.Unmarshal(pkt.Meta, &meta); err != nil {
            log.Printf("[handler] failed to decode meta: %v", err)
        }
    }

    switch pkt.Type {
    case "VIDEO_ADDED":
        var payload AddedPayload
        if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
            log.Printf("[handler] failed to decode VIDEO_ADDED payload: %v", err)
            return
        }
        LogAdded(clientID, payload, meta)

    case "VIDEO_REMOVED":
        var payload RemovedPayload
        if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
            log.Printf("[handler] failed to decode VIDEO_REMOVED payload: %v", err)
            return
        }
        LogRemoved(clientID, payload, meta)

    case "VIDEO_UPDATE":
        var payload UpdatePayload
        if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
            log.Printf("[handler] failed to decode VIDEO_UPDATE payload: %v", err)
            return
        }
        LogUpdate(clientID, payload, meta)

    default:
        log.Printf("[handler] unknown packet type %q — dropped", pkt.Type)
    }
}