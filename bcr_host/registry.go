package main

import (
	"log"
	"sync"

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
	defer r.mu.Unlock()

	switch conn.Role {
	case "extension":
		if r.extension != nil {
			log.Printf("[registry] extension reconnected — closing previous connection (id=%s)", r.extension.ID)
			r.extension.WS.Close()
		}
		r.extension = conn
		log.Printf("[registry] extension registered (id=%s)", conn.ID)

	case "client":
		r.clients[conn.ID] = conn
		log.Printf("[registry] client registered (id=%s, total_clients=%d)", conn.ID, len(r.clients))

	default:
		log.Printf("[registry] unknown role %q for connection (id=%s) — rejected", conn.Role, conn.ID)
		conn.WS.Close()
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
		log.Printf("[registry] broadcast called but no clients connected — dropping message")
		return
	}

	for _, client := range snapshot {
		if err := client.Send(msgType, data); err != nil {
			log.Printf("[registry] failed to send to client (id=%s): %v", client.ID, err)
			// Removal is handled by the client's own ReadLoop when it exits.
			// We do not force-remove here to avoid a deadlock (we'd need a write
			// lock while the read lock is still logically in scope).
		}
	}
}
