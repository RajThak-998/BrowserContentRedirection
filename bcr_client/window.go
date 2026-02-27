package main

import (
	"log"
	"runtime"

	"github.com/go-gl/gl/v2.1/gl"
	"github.com/go-gl/glfw/v3.3/glfw"
)

// init locks the goroutine that imports this package to its OS thread.
// This is the standard Go-OpenGL pattern: the blank import of this file
// by main.go ensures the lock happens before main() runs.
// We also call runtime.LockOSThread() explicitly in main() for clarity.
func init() {
	runtime.LockOSThread()
}

// defaultW / defaultH are the initial window dimensions used when
// VIDEO_ADDED fires before any VIDEO_UPDATE with real bounds.
const (
	defaultW = 640
	defaultH = 360
)

// RunRenderLoop initialises GLFW, creates the overlay window, and runs
// the render loop on the calling goroutine (which must be the main OS thread).
//
// geomCh — receives GeometryUpdate messages from OverlayManager.
// doneCh — closed by the caller (context cancellation) to signal shutdown.
//
// This function blocks until doneCh is closed or the window is closed.
func RunRenderLoop(geomCh <-chan GeometryUpdate, doneCh <-chan struct{}) {
	// ── GLFW init ────────────────────────────────────────────────────────────

	if err := glfw.Init(); err != nil {
		log.Fatalf("[window] glfw.Init failed: %v", err)
	}
	defer glfw.Terminate()

	// ── Window hints ─────────────────────────────────────────────────────────

	glfw.DefaultWindowHints()
	glfw.WindowHint(glfw.Decorated, glfw.False) // borderless
	glfw.WindowHint(glfw.Resizable, glfw.False) // we resize programmatically
	glfw.WindowHint(glfw.Floating, glfw.True)   // always-on-top
	glfw.WindowHint(glfw.Visible, glfw.False)   // start hidden; show on VIDEO_ADDED
	glfw.WindowHint(glfw.ContextVersionMajor, 2)
	glfw.WindowHint(glfw.ContextVersionMinor, 1)

	// ── Window creation ──────────────────────────────────────────────────────

	win, err := glfw.CreateWindow(defaultW, defaultH, "BCR Overlay", nil, nil)
	if err != nil {
		log.Fatalf("[window] CreateWindow failed: %v", err)
	}
	defer win.Destroy()

	win.MakeContextCurrent()
	glfw.SwapInterval(1) // vsync — no need to spin at thousands of FPS

	// ── OpenGL init ──────────────────────────────────────────────────────────

	if err := gl.Init(); err != nil {
		log.Fatalf("[window] gl.Init failed: %v", err)
	}

	log.Printf("[window] render loop started (OpenGL %s)", gl.GoStr(gl.GetString(gl.VERSION)))

	// ── Render loop ──────────────────────────────────────────────────────────

	for {
		// Check for shutdown signal.
		select {
		case <-doneCh:
			log.Println("[window] shutdown signal received — exiting render loop")
			return
		default:
		}

		// Check for OS close button (rare — window is borderless, but defensive).
		if win.ShouldClose() {
			log.Println("[window] window close requested — exiting render loop")
			return
		}

		// Drain all pending geometry updates before drawing.
		// Using a loop so that if several updates arrived between frames
		// we apply the latest one and discard stale intermediates.
		applyUpdates(win, geomCh)

		// Draw — only gl.Clear in Phase 1.
		drawFrame(win)

		// Process GLFW events (mouse, keyboard, resize callbacks, etc.).
		glfw.PollEvents()
	}
}

// applyUpdates drains the geometry channel and applies the latest update.
// Older updates in the same batch are discarded — only the last one matters.
func applyUpdates(win *glfw.Window, geomCh <-chan GeometryUpdate) {
	var latest *GeometryUpdate

	// Drain all pending without blocking.
	for {
		select {
		case u, ok := <-geomCh:
			if !ok {
				return
			}
			// Copy onto heap so latest pointer stays valid next iteration.
			copy := u
			latest = &copy
		default:
			goto apply
		}
	}

apply:
	if latest == nil {
		return // nothing to apply this frame
	}

	if !latest.Visible {
		win.Hide()
		log.Println("[window] hidden (no active video)")
		return
	}

	// Apply geometry first, then show — avoids a visible frame at wrong position.
	win.SetSize(latest.W, latest.H)
	win.SetPos(latest.X, latest.Y)

	// Sync the OpenGL viewport to the new framebuffer size.
	fbW, fbH := win.GetFramebufferSize()
	gl.Viewport(0, 0, int32(fbW), int32(fbH))

	win.Show()

	log.Printf("[window] geometry applied — pos=(%d,%d) size=(%d×%d)",
		latest.X, latest.Y, latest.W, latest.H)
}

// drawFrame clears the framebuffer with a solid blue and swaps buffers.
// Phase 1: no textures, no shaders, no geometry — just a colour fill.
//
// Future video rendering:
//   - Upload decoded YUV/RGB frames as a GL_TEXTURE_2D here.
//   - Bind a simple shader (vert: fullscreen quad, frag: sample texture).
//   - Replace gl.Clear with gl.DrawArrays.
func drawFrame(win *glfw.Window) {
	// BCR blue — visible, not distracting.
	gl.ClearColor(0.10, 0.18, 0.36, 1.0)
	gl.Clear(gl.COLOR_BUFFER_BIT)

	win.SwapBuffers()
}
