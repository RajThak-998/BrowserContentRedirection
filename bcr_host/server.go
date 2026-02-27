package main

import (
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// upgrader configures the WebSocket upgrade. CheckOrigin is permissive
// for the MVP — the Chrome extension origin will not match localhost.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Server holds shared state needed to handle incoming connections.
type Server struct {
	registry *Registry
}

// NewServer constructs a Server with a given Registry.
func NewServer(registry *Registry) *Server {
	return &Server{registry: registry}
}

// HandleWS is the HTTP handler for the /ws endpoint.
// It upgrades the connection, extracts the role query param,
// registers the connection, and launches the read loop.
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	if role != "extension" && role != "client" {
		log.Printf("[server] rejected connection — invalid role %q from %s", role, r.RemoteAddr)
		http.Error(w, "invalid role — use ?role=extension or ?role=client", http.StatusBadRequest)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] websocket upgrade failed for %s: %v", r.RemoteAddr, err)
		return
	}

	conn := &Connection{
		ID:   uuid.NewString(),
		Role: role,
		WS:   ws,
	}

	log.Printf("[server] new connection accepted (id=%s, role=%s, addr=%s)", conn.ID, conn.Role, r.RemoteAddr)

	s.registry.Register(conn)

	// Read loop runs in the same goroutine as the handler.
	// The HTTP handler goroutine is already separate per connection.
	ReadLoop(conn, s.registry)
}
