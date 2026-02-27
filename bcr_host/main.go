package main

import (
	"log"
	"net/http"
)

const listenAddr = ":8765"

func main() {
	log.Println("┌─────────────────────────────────────────┐")
	log.Println("│           bcr_host starting             │")
	log.Println("│  WebSocket endpoint: ws://localhost:8765/ws  │")
	log.Println("│  Roles: ?role=extension | ?role=client  │")
	log.Println("└─────────────────────────────────────────┘")

	registry := NewRegistry()
	server := NewServer(registry)

	http.HandleFunc("/ws", server.HandleWS)

	log.Printf("[main] listening on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("[main] server error: %v", err)
	}
}
