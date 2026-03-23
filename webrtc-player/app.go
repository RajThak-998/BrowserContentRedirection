package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App struct
type App struct {
	ctx        context.Context
	signalConn net.Conn
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// Start the TCP server for WebRTC SDP in the background
	go a.startTCPServer()

	// Start the WebSocket server for MSE byte chunks in the background
	go a.startWebSocketServer()
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

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow from any origin for desktop app
	},
}

// startWebSocketServer listens for incoming byte chunks
func (a *App) startWebSocketServer() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket Upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		log.Println("WebSocket Client Connected for MSE Byte Streaming.")

		for {
			mt, message, err := conn.ReadMessage()
			if err != nil || mt == websocket.CloseMessage {
				log.Printf("WebSocket disconnected. err=%v, type=%d", err, mt)
				break
			}

			log.Printf("WebSocket received message of type: %d, size: %d bytes", mt, len(message))

			// Process Text messages as JSON Window commands
			if mt == websocket.TextMessage {
				var cmd struct {
					X      *int  `json:"x"`
					Y      *int  `json:"y"`
					Width  *int  `json:"width"`
					Height *int  `json:"height"`
					Hidden *bool `json:"hidden"`
				}
				if err := json.Unmarshal(message, &cmd); err == nil {
					log.Printf("Parsed Text command: %+v", cmd)
					if cmd.Hidden != nil {
						if *cmd.Hidden {
							a.HideWindow()
						} else {
							a.ShowWindow()
						}
					}
					if cmd.X != nil && cmd.Y != nil {
						a.SetWindowPosition(*cmd.X, *cmd.Y)
					}
					if cmd.Width != nil && cmd.Height != nil {
						a.SetWindowSize(*cmd.Width, *cmd.Height)
					}
				} else {
					log.Printf("Error parsing text command %s: %v", string(message), err)
				}
			}

			// Only process Binary messages for video streaming
			if mt == websocket.BinaryMessage {
				base64Str := base64.StdEncoding.EncodeToString(message)
				runtime.EventsEmit(a.ctx, "onVideoChunk", base64Str)
			}
		}
	})

	log.Println("WebSocket Server listening on :8081 for MSE Byte Chunks...")
	err := http.ListenAndServe(":8081", nil)
	if err != nil {
		log.Fatalf("Failed to start WebSocket server on :8081: %v", err)
	}
}
