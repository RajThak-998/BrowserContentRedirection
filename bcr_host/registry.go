package main

import (
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Connection wraps a gorilla WebSocket connection with its assigned
// role and a unique ID. The mutex guards concurrent writes — gorilla
// websocket connections are NOT safe for concurrent writes.
type Connection struct {
	ID   string
	Role string
	WS   *websocket.Conn
	mu   sync.Mutex
}

// Send writes a raw message to this connection.
// It is safe to call from multiple goroutines.
func (c *Connection) Send(msgType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.WS.WriteMessage(msgType, data)
}

// CloseWithMessage sends a close frame then closes the underlying
// connection. Safe to call concurrently with ReadLoop.
func (c *Connection) CloseWithMessage(code int, text string) {
	c.mu.Lock()
	// Best-effort write of close frame — ignore errors.
	_ = c.WS.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, text),
		time.Now().Add(2*time.Second),
	)
	c.mu.Unlock()

	// Force-close the underlying TCP connection so ReadLoop unblocks.
	// This is safe to call concurrently with ReadMessage.
	c.WS.Close()
}

// Registry tracks the single active extension connection and all
// active client connections. All fields are protected by RWMutex.
type Registry struct {
	mu        sync.RWMutex
	extension *Connection
	clients   map[string]*Connection
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		clients: make(map[string]*Connection),
	}
}

// Register adds a connection to the registry under its role.
// For the extension role, last-one-wins: the existing extension
// connection is closed before being replaced.
func (r *Registry) Register(conn *Connection) {
	r.mu.Lock()

	var oldConn *Connection

	switch conn.Role {
	case "extension":
		if r.extension != nil && r.extension.ID != conn.ID {
			log.Printf("[registry] extension reconnected — replacing previous (old=%s, new=%s)", r.extension.ID, conn.ID)
			oldConn = r.extension
		}
		r.extension = conn
		log.Printf("[registry] extension registered (id=%s)", conn.ID)

	case "client":
		r.clients[conn.ID] = conn
		log.Printf("[registry] client registered (id=%s, total_clients=%d)", conn.ID, len(r.clients))

	default:
		log.Printf("[registry] unknown role %q for connection (id=%s) — rejected", conn.Role, conn.ID)
		r.mu.Unlock()
		conn.WS.Close()
		return
	}

	r.mu.Unlock()

	// Close old connection OUTSIDE the registry lock to avoid deadlock.
	// We send CloseGoingAway (1001) so background.js knows this was
	// a server-initiated replacement, not a crash.
	if oldConn != nil {
		oldConn.CloseWithMessage(websocket.CloseGoingAway, "replaced by new connection")
	}
}

// Remove unregisters a connection from the registry.
// It matches by connection ID to avoid removing a replacement
// that may have already taken the slot (extension reconnect race).
func (r *Registry) Remove(conn *Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch conn.Role {
	case "extension":
		// Only remove if this is still the current extension.
		// A reconnect may have already replaced it.
		if r.extension != nil && r.extension.ID == conn.ID {
			r.extension = nil
			log.Printf("[registry] extension unregistered (id=%s)", conn.ID)
		}

	case "client":
		delete(r.clients, conn.ID)
		log.Printf("[registry] client unregistered (id=%s, remaining_clients=%d)", conn.ID, len(r.clients))
	}
}

// Broadcast sends a raw message to every connected client.
// Failed sends are logged but do not abort the broadcast loop.
func (r *Registry) Broadcast(msgType int, data []byte) {
	r.mu.RLock()
	snapshot := make([]*Connection, 0, len(r.clients))
	for _, c := range r.clients {
		snapshot = append(snapshot, c)
	}
	r.mu.RUnlock()

	if len(snapshot) == 0 {
		// Downgrade to debug-level — this fires on every extension message
		// when no client is connected. Not worth logging.
		return
	}

	for _, client := range snapshot {
		if err := client.Send(msgType, data); err != nil {
			log.Printf("[registry] failed to send to client (id=%s): %v", client.ID, err)
		}
	}
}

// SendToExtension sends a raw message to the currently active extension
// connection. Returns false when no extension is connected.
func (r *Registry) SendToExtension(msgType int, data []byte) bool {
	r.mu.RLock()
	ext := r.extension
	r.mu.RUnlock()

	if ext == nil {
		return false
	}

	if err := ext.Send(msgType, data); err != nil {
		log.Printf("[registry] failed to send to extension (id=%s): %v", ext.ID, err)
		return false
	}

	return true
}
