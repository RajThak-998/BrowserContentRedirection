package main

import (
	"context"
	"encoding/base64"
	"log"
	"math"
	"net"
	"sync"
	"time"

	"bcr_client/internal"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App struct
type App struct {
	ctx        context.Context
	signalConn net.Conn

	windowMu        sync.Mutex
	lastWindowApply time.Time

	// mseActive is set to true once MSE (YouTube) media chunks start arriving,
	// and false when the video is removed. It gates window show/hide so that
	// the WebRTC overlay and the YouTube overlay do not interfere with each other.
	mseActiveMu sync.Mutex
	mseActive   bool
	hideTimer   *time.Timer // debounces overlay HIDE so brief teardown churn doesn't flash the window

	mediaEngine *engine.Engine
}

const telemetryWindowThrottle = 33 * time.Millisecond

// overlayHideGrace defers hiding the overlay after MSE goes inactive, so brief
// teardown→rebuild churn (ad boundaries, hover previews, videoID swaps) doesn't
// flash the window. A show within this window cancels the pending hide.
const overlayHideGrace = 2 * time.Second

// minOverlayVisiblePx is the minimum on-screen extent (per axis) a telemetry
// position must leave visible to be applied. Transient layout jumps report the
// tracked <video> rect far off-screen (e.g. y=-510); those are ignored so the
// overlay keeps its last valid position instead of sliding out of view.
const minOverlayVisiblePx = 60

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	

	a.mediaEngine = engine.New(
		engine.Config{ListenAddr: ":8081"},
		engine.Callbacks{
			OnLoopbackOffer: func(bridgeID string, sdp string) {
				log.Printf("[bcr_client][loopback] emitting onLocalLoopbackOffer to frontend bridgeId=%s sdpLen=%d", bridgeID, len(sdp))
				runtime.EventsEmit(a.ctx, "onLocalLoopbackOffer", bridgeID, sdp)
			},
			OnVideoUpdate: func(update engine.VideoUpdate) {
				b := update.Payload.ScreenBounds
				a.applyWindowFromTelemetry(b.X, b.Y, b.Width, b.Height)
				// Window visibility is managed exclusively by NotifyMSEActive() called
				// from the frontend — not by VDI playback state, which flaps rapidly
				// during quality switches and causes window oscillation.
			},
			OnVideoLifecycle: func(evtType string, videoID string) {
				log.Printf("[bcr_client][video] lifecycle evtType=%s videoId=%s", evtType, videoID)
				runtime.EventsEmit(a.ctx, "onVideoLifecycle", evtType, videoID)
				// NOTE: window visibility is intentionally NOT hidden here.
				// A VIDEO_REMOVED fires for hover-preview thumbnails and for the
				// brief element swap YouTube does at ad boundaries — hiding on it
				// made the overlay flicker/vanish while the frontend was still
				// playing. Visibility is now owned exclusively by the frontend via
				// NotifyMSEActive(false), which it calls only after its debounced
				// teardown actually runs (see scheduleTeardown in main.js).
			},
			OnMediaChunk: func(header engine.MediaChunkHeader, chunkData []byte) {
				// Encode chunk bytes as base64 for JSON-safe Wails event transport.
				chunkB64 := base64.StdEncoding.EncodeToString(chunkData)
				runtime.EventsEmit(a.ctx, "onMediaChunk",
					header.Seq,
					header.TrackType,
					header.MimeType,
					header.Codec,
					header.SourceBufferID,
					header.IsInitSegment,
					chunkB64,
					header.VideoID,
				)
			},
			OnLog: func(message string) {
				log.Println(message)
			},
			OnYTDiag: func(message string) {
				log.Printf("[YT] %s", message)
			},
			// Verbose diagnostics (full SDP dumps) go to the log file only.
			OnVerboseLog: verboseLog,
		},
	)

	// Start the TCP server for WebRTC SDP in the background
	go a.startTCPServer()

	// Start core engine (WebSocket signaling + media bridge) in background.
	go func() {
		if err := a.mediaEngine.Run(ctx); err != nil {
			log.Printf("Engine stopped with error: %v", err)
		}
	}()
}

func (a *App) startTCPServer() {
	listener, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatalf("Failed to bind TCP port 8080: %v", err)
	}
	defer listener.Close()

	log.Println("TCP Server listening on :8080 for WebRTC SDP Offers...")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept failed: %v", err)
			continue
		}

		a.signalConn = conn
		go a.handleConnection(conn)
	}
}

func (a *App) handleConnection(conn net.Conn) {
	// We do NOT defer conn.Close() here because WebRTC might need
	// the connection kept open to exchange ICE candidates immediately after the SDP.
	// For this basic SDP exchange, we read the incoming offer buffer.

	buf := make([]byte, 8192) // 8KB buffer for SDP
	n, err := conn.Read(buf)
	if err != nil {
		log.Printf("Failed to read SDP from TCP: %v", err)
		return
	}

	sdpOffer := string(buf[:n])
	log.Printf("Received SDP Offer (%d bytes)", n)

	// Emit the SDP to the frontend JavaScript to instantiate the WebRTC player
	runtime.EventsEmit(a.ctx, "onSdpOffer", sdpOffer)
}

// SendSdpAnswer takes the SDP answer from Javascript and sends it back over TCP
func (a *App) SendSdpAnswer(sdp string) {
	if a.signalConn != nil {
		_, err := a.signalConn.Write([]byte(sdp + "\n"))
		if err != nil {
			log.Println("Error sending SDP Answer over TCP:", err)
		} else {
			log.Println("Successfully sent SDP Answer to TCP client.")
		}
	} else {
		log.Println("Cannot send SDP Answer: No active TCP connection.")
	}
}

// SetLoopbackAnswer takes the SDP answer from Javascript and passes it to the loopback session
func (a *App) SetLoopbackAnswer(bridgeID string, sdp string) {
	if a.mediaEngine != nil {
		a.mediaEngine.SetLoopbackAnswer(bridgeID, sdp)
	}
}

// RequestLoopbackOffer is called by the Wails frontend immediately after it
// registers its EventsOn("onLocalLoopbackOffer") listener. This recovers from
// the cold-start race where the Go backend fired the offer before the JS
// listener was alive — the engine re-emits any cached offer SDPs for active
// loopback sessions.
func (a *App) RequestLoopbackOffer() {
	log.Println("[bcr_client] RequestLoopbackOffer called by frontend — re-emitting any cached loopback offers")
	if a.mediaEngine != nil {
		a.mediaEngine.ReEmitAllLoopbackOffers()
	}
}

// ReportPlayback relays the frontend MSE buffer state (bufferedEnd + playhead, in
// seconds) back to the extension interceptor so it can drive the YouTube seek pulse.
func (a *App) ReportPlayback(bufferedEnd float64, playhead float64) {
	if a.mediaEngine != nil {
		a.mediaEngine.SendYTPlayback(bufferedEnd, playhead)
	}
}

// -------------------------------------------------------------
// UI Window Controls (Exposed to Javascript or called from Go)
// -------------------------------------------------------------

// ShowWindow makes the application visible
func (a *App) ShowWindow() {
	runtime.WindowShow(a.ctx)
}

// HideWindow hides the application
func (a *App) HideWindow() {
	runtime.WindowHide(a.ctx)
}

// SetWindowPosition moves the frameless window on the desktop
func (a *App) SetWindowPosition(x int, y int) {
	runtime.WindowSetPosition(a.ctx, x, y)
}

// SetWindowSize changes the size of the frameless window
func (a *App) SetWindowSize(width int, height int) {
	runtime.WindowSetSize(a.ctx, width, height)
}


func (a *App) applyWindowFromTelemetry(x, y, w, h float64) {
	ix := int(math.Round(x))
	iy := int(math.Round(y))
	iw := int(math.Round(w))
	ih := int(math.Round(h))

	if iw < 1 || ih < 1 {
		return
	}

	// Ignore near-/fully-off-screen positions (keep the last valid one). During
	// scroll/layout transitions the tracked <video> rect briefly reports far above
	// or left of the viewport (seen as y=-510); applying it slides the overlay out
	// of view and it looks deleted. A real on-screen update follows shortly after.
	if iy+ih < minOverlayVisiblePx || ix+iw < minOverlayVisiblePx {
		return
	}

	now := time.Now()

	a.windowMu.Lock()
	if !a.lastWindowApply.IsZero() && now.Sub(a.lastWindowApply) < telemetryWindowThrottle {
		a.windowMu.Unlock()
		return
	}

	a.lastWindowApply = now
	a.windowMu.Unlock()

	a.SetWindowPosition(ix, iy)
	a.SetWindowSize(iw, ih)

	log.Printf("[Telemetry] VIDEO_UPDATE applied pos=(%d, %d) size=(%d, %d)", ix, iy, iw, ih)
}

// showIfNeeded shows the window and marks MSE as active. Safe to call on every
// VIDEO_UPDATE with state=playing — it is idempotent.
func (a *App) showIfNeeded() {
	a.mseActiveMu.Lock()
	if !a.mseActive {
		a.mseActive = true
		a.mseActiveMu.Unlock()
		log.Println("[bcr_client][video] MSE active — showing overlay window")
		runtime.WindowShow(a.ctx)
		return
	}
	a.mseActiveMu.Unlock()
}

// hideIfMSEMode hides the window only when MSE (YouTube) is the active mode.
// It does NOT hide during WebRTC sessions, preventing cross-feature interference.
func (a *App) hideIfMSEMode() {
	a.mseActiveMu.Lock()
	defer a.mseActiveMu.Unlock()
	if a.mseActive {
		a.mseActive = false
		log.Println("[bcr_client][video] MSE inactive — hiding overlay window")
		runtime.WindowHide(a.ctx)
	}
}

// NotifyMSEActive is called by the frontend when the MSE pipeline becomes live
// (canplay event fired) or is torn down. It drives the overlay window visibility
// so the window only shows when the thin client is actually rendering video.
//
// Shows are immediate; hides are debounced by overlayHideGrace so the brief
// teardown→rebuild churn YouTube produces (ad boundaries, hover previews, videoID
// swaps) doesn't flash the overlay in and out. A show during the grace cancels the
// pending hide.
func (a *App) NotifyMSEActive(active bool) {
	var doShow bool

	a.mseActiveMu.Lock()
	if active {
		if a.hideTimer != nil {
			a.hideTimer.Stop()
			a.hideTimer = nil
		}
		if !a.mseActive {
			a.mseActive = true
			doShow = true
		}
	} else if a.mseActive && a.hideTimer == nil {
		// Defer the hide; a rebuild within the grace window cancels it above.
		a.hideTimer = time.AfterFunc(overlayHideGrace, a.hideOverlayAfterGrace)
	}
	a.mseActiveMu.Unlock()

	if doShow {
		log.Println("[bcr_client][video] MSE active — showing overlay window")
		runtime.WindowShow(a.ctx)
	}
}

// hideOverlayAfterGrace fires when the overlay has been inactive for overlayHideGrace
// with no rebuild — the video really stopped, so hide the window.
func (a *App) hideOverlayAfterGrace() {
	var doHide bool

	a.mseActiveMu.Lock()
	a.hideTimer = nil
	if a.mseActive {
		a.mseActive = false
		doHide = true
	}
	a.mseActiveMu.Unlock()

	if doHide {
		log.Println("[bcr_client][video] MSE inactive (grace elapsed) — hiding overlay window")
		runtime.WindowHide(a.ctx)
	}
}
