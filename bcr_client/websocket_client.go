package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

const (
	hostURL       = "ws://localhost:8765/ws?role=client"
	retryInterval = 2 * time.Second
)

// Client holds the identity and active WebSocket connection.
type Client struct {
	ID string
}

// Run starts the connection loop. It blocks until ctx is cancelled.
// On each disconnect it waits retryInterval then reconnects.
// Only state transitions (connected/disconnected/reconnecting) are logged.
func (c *Client) Run(ctx context.Context) {
	for {
		connected := c.connect(ctx)

		// If context was cancelled during connect or read, exit cleanly.
		select {
		case <-ctx.Done():
			log.Println("[client] context cancelled — shutting down")
			return
		default:
		}

		if connected {
			// Was connected, now lost — log the state change.
			log.Printf("[client] disconnected from host — retrying in %s", retryInterval)
		} else {
			// Never connected — host not reachable.
			log.Printf("[client] could not reach host — retrying in %s", retryInterval)
		}

		select {
		case <-ctx.Done():
			log.Println("[client] context cancelled — shutting down")
			return
		case <-time.After(retryInterval):
			log.Println("[client] reconnecting...")
		}
	}
}

// connect dials bcr_host and reads messages until the connection drops
// or ctx is cancelled. Returns true if a connection was established.
func (c *Client) connect(ctx context.Context) (wasConnected bool) {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, hostURL, nil)
	if err != nil {
		// Connection failed — not a state change worth logging every 2s.
		// The outer Run() loop logs "could not reach host" instead.
		return false
	}

	log.Printf("[client] connected to host (id=%s, url=%s)", c.ID, hostURL)
	wasConnected = true

	// Ensure connection is closed when this function exits,
	// whether due to read error, ctx cancel, or normal close.
	defer func() {
		conn.WriteMessage( //nolint
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
		conn.Close()
	}()

	// Spin up a goroutine that closes the connection when ctx is cancelled.
	// This unblocks ReadMessage() so Run() can exit cleanly on SIGINT.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled — not an error, just shutdown.
				return wasConnected
			}
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
				websocket.CloseNoStatusReceived,
			) {
				log.Printf("[client] unexpected close: %v", err)
			}
			return wasConnected
		}

		if msgType == websocket.BinaryMessage {
			HandleMediaBinaryFrame(data)
			continue
		}

		if msgType != websocket.TextMessage {
			continue
		}

		var pkt Packet
		if err := json.Unmarshal(data, &pkt); err != nil {
			log.Printf("[client] failed to parse packet: %v", err)
			continue
		}

		HandlePacket(pkt)
	}
}
