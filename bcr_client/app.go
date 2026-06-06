package main

import (
	"context"
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

	mediaEngine *engine.Engine
}

const telemetryWindowThrottle = 33 * time.Millisecond

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
			},
			OnLog: func(message string) {
				log.Println(message)
			},
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
