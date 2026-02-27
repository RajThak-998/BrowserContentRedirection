package main

import (
	"log"

	"github.com/gorilla/websocket"
)

// ReadLoop blocks on reading messages from a single connection.
// It routes messages based on the connection's role:
//   - extension: broadcast raw bytes to all clients
//   - client:    log and ignore (future: handle control messages)
//
// When the connection closes or errors, it cleans up from the registry
// and returns — letting the goroutine started in server.go exit cleanly.
func ReadLoop(conn *Connection, registry *Registry) {
	defer func() {
		registry.Remove(conn)
		conn.WS.Close()
		log.Printf("[router] read loop exited (id=%s, role=%s)", conn.ID, conn.Role)
	}()

	for {
		msgType, data, err := conn.WS.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
				websocket.CloseNoStatusReceived,
			) {
				log.Printf("[router] unexpected close error (id=%s, role=%s): %v", conn.ID, conn.Role, err)
			}
			// Normal/expected close — no extra log needed.
			// The deferred log above covers it.
			return
		}

		switch conn.Role {
		case "extension":
			// Silently broadcast — no per-message log.
			// The bcr_client logger handles detailed output.
			registry.Broadcast(msgType, data)

		case "client":
			log.Printf("[router] received %d bytes from client (id=%s) — ignored", len(data), conn.ID)

		default:
			log.Printf("[router] received message from unknown role %q (id=%s) — dropped", conn.Role, conn.ID)
		}
	}
}
