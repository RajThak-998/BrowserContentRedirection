package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

func main() {
	// ── Main OS thread lock ───────────────────────────────────────────────────
	// GLFW requires all calls to happen on the thread that called glfw.Init().
	// runtime.LockOSThread pins this goroutine to its OS thread permanently.
	// window.go's init() also calls this for safety — calling twice is harmless.
	runtime.LockOSThread()

	// ── Geometry channel ─────────────────────────────────────────────────────
	// Buffered so that the WebSocket goroutine never blocks waiting for the
	// render loop to drain. Capacity of 16 covers bursts of rapid VIDEO_UPDATE
	// packets (e.g. scrolling) without dropping.
	geomCh := make(chan GeometryUpdate, 16)

	// ── Overlay manager ──────────────────────────────────────────────────────
	// Initialised before the WebSocket client starts so HandlePacket
	// can safely call overlayManager.* from the very first packet.
	overlayManager = NewOverlayManager(geomCh)

	// ── Shutdown plumbing ─────────────────────────────────────────────────────
	// Root context — cancelled on SIGINT or SIGTERM.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan struct{})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("[main] received signal %s — shutting down", sig)
		case <-ctx.Done():
		}
		close(doneCh)
		cancel()
	}()

	// ── WebSocket client ──────────────────────────────────────────────────────
	// Runs in a goroutine — must NOT block main thread.
	// Must NOT call GLFW or OpenGL directly.
	go func() {
		log.Println("[main] starting WebSocket client")
		c := &Client{ID: "bcr_client_main"}
		c.Run(ctx)
		log.Println("[main] WebSocket client exited")
		// If the WS client exits (e.g. host went away), trigger shutdown.
		cancel()
		close(doneCh)
	}()

	// ── Render loop — blocks on main OS thread ────────────────────────────────
	log.Println("[main] starting render loop on main OS thread")
	RunRenderLoop(geomCh, doneCh)

	log.Println("[main] bcr_client exited cleanly")
}
