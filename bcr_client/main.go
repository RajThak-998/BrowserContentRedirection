package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Println("  BCR Client")
	log.Println("  Connecting to ws://localhost:8765/ws?role=client")
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Root context — cancelled on SIGINT or SIGTERM.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trap shutdown signals and cancel the context.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("[main] received signal %s — shutting down", sig)
		cancel()
	}()

	client := &Client{ID: "bcr-client-1"}
	client.Run(ctx) // blocks until ctx is cancelled

	log.Println("[main] goodbye")
}
