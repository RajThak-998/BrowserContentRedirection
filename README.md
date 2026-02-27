# BCR — Browser Content Relay

> Real-time video telemetry pipeline from browser to native overlay.

BCR detects video elements in any webpage, tracks their position and state in real time, and mirrors them as a native floating overlay window on your desktop — with no media decoding, no screen capture, and no browser plugins beyond a Chrome extension.

---

## What It Does

1. Chrome extension detects `<video>` elements on any page
2. Tracks their position, size, visibility, and playback state in real time
3. Sends telemetry over WebSocket to a local Go relay server
4. A native Go client receives the telemetry and renders a borderless floating window that mirrors the video's exact screen position and size
5. The overlay hides when the video scrolls out of view and reappears when it scrolls back

---

## Project Structure

```
BCR/
├── bcr_host/                        # Go WebSocket relay server
│   ├── main.go                      # Entry point, HTTP server
│   ├── server.go                    # WebSocket upgrade handler
│   ├── registry.go                  # Connection registry (extension + clients)
│   ├── router.go                    # Per-connection read loop + message routing
│   └── protocol.go                  # Shared packet type definitions
│
├── bcr_client/                      # Go native client + overlay renderer
│   ├── main.go                      # Entry point, threading setup, lifecycle
│   ├── websocket_client.go          # WebSocket client with reconnect logic
│   ├── packet_handler.go            # Routes packets to overlay manager
│   ├── overlay_manager.go           # Tracks active video, sends geometry updates
│   ├── window.go                    # GLFW window + OpenGL render loop
│   ├── protocol.go                  # Packet + payload type definitions
│   └── logger.go                    # Structured log formatters
│
├── video-telemetry-extension/       # Chrome Extension (Manifest V3)
│   ├── manifest.json                # Extension manifest
│   ├── background/
│   │   └── background.js            # Service worker: WebSocket transport
│   ├── content/
│   │   ├── bootstrap.js             # Content script entry point
│   │   ├── videoRegistry.js         # DOM video element discovery + tracking
│   │   ├── videoTracker.js          # Per-video observer pipeline
│   │   ├── stateManager.js          # Delta computation + change filtering
│   │   ├── emitter.js               # chrome.runtime.sendMessage wrapper
│   │   ├── observers.js             # ResizeObserver, IntersectionObserver, scroll, fullscreen, playback
│   │   └── overlayRenderer.js       # In-browser debug overlay (visual highlight)
│   └── utils/
│       └── throttle.js              # Leading-edge RAF throttle
│
└── server/                          # Legacy Node.js prototype server (reference only)
    ├── index.js
    └── package.json
```

---

## Prerequisites

### System (Fedora / RPM-based)

```bash
sudo dnf install -y \
    libX11-devel \
    libXrandr-devel \
    libXinerama-devel \
    libXcursor-devel \
    libXi-devel \
    libXxf86vm-devel \
    mesa-libGL-devel \
    mesa-libGLU-devel \
    gcc \
    pkg-config
```

### Go

```bash
go version  # requires Go 1.22+
```

### Chrome

Any Chromium-based browser with extension developer mode enabled.

---

## Installation

### 1. Clone

```bash
git clone <repo-url>
cd BCR
```

### 2. Install Go dependencies — bcr_host

```bash
cd bcr_host
go mod tidy
```

### 3. Install Go dependencies — bcr_client

```bash
cd ../bcr_client
go get github.com/go-gl/gl/v2.1/gl
go get github.com/go-gl/glfw/v3.3/glfw
go mod tidy
```

### 4. Load Chrome Extension

1. Open `chrome://extensions`
2. Enable **Developer mode** (top right toggle)
3. Click **Load unpacked**
4. Select the `video-telemetry-extension/` directory

---

## Running

Open **three terminals**:

**Terminal 1 — Relay server**
```bash
cd BCR/bcr_host
go run .
```

**Terminal 2 — Native client + overlay**
```bash
cd BCR/bcr_client
go run .
```

**Terminal 3 — (optional) verify logs**
```bash
# bcr_client logs all packet events to stdout
# You should see VIDEO_ADDED / VIDEO_UPDATE / VIDEO_REMOVED
```

Then open any YouTube video in Chrome.

The overlay window appears at the video's exact screen position. Scroll down — it hides. Scroll back — it reappears.

---

## How It Works (Brief)

```
[Chrome Tab]
  <video> element detected by content script
        ↓
  Position + visibility computed (getBoundingClientRect + screenX/Y)
        ↓
  Delta-filtered — only meaningful changes emitted
        ↓
[Service Worker]
  chrome.runtime.sendMessage → WebSocket → bcr_host
        ↓
[bcr_host — Go]
  Registered extension connection broadcasts to all client connections
        ↓
[bcr_client — Go]
  Packet decoded → OverlayManager → geometry channel
        ↓  (main OS thread)
  GLFW window: SetPos + SetSize + Show/Hide
  OpenGL: glClear (phase 1 — solid colour rectangle)
```

---

## Configuration

| Location | Constant | Default | Description |
|---|---|---|---|
| `bcr_host/main.go` | `listenAddr` | `:8765` | Host WebSocket port |
| `bcr_client/websocket_client.go` | `hostURL` | `ws://localhost:8765/ws?role=client` | Client connect URL |
| `bcr_client/websocket_client.go` | `retryInterval` | `2s` | Reconnect delay |
| `bcr_client/overlay_manager.go` | `visibilityThreshold` | `0.0` | Hide overlay below this IntersectionRatio |
| `background.js` | `_INITIAL_BACKOFF_MS` | `1000ms` | Extension WebSocket initial reconnect backoff |
| `background.js` | `_MAX_BACKOFF_MS` | `5000ms` | Extension WebSocket max reconnect backoff |

---

## Threading Model

| Component | Thread | Reason |
|---|---|---|
| GLFW init + window ops | Main OS thread (locked) | GLFW hard requirement |
| OpenGL calls | Main OS thread (locked) | Context is thread-local |
| WebSocket read loop | Goroutine | Blocking I/O |
| Packet handler | Goroutine (same as WS) | Non-blocking — channel send only |
| Signal watcher | Goroutine | Blocking select |

---

## Current Limitations (Phase 1)

- Tracks only the **most recent** video (single overlay window)
- Overlay is a **solid colour rectangle** — no media content
- Coordinates are **CSS pixels** — no HiDPI/fractional scaling correction yet
- **X11 only** — Wayland not tested (XWayland may work)
- No input forwarding to the browser video

---

## Roadmap

- [ ] HiDPI / device pixel ratio correction
- [ ] Multi-video tracking (multiple overlay windows)
- [ ] Wayland native support
- [ ] Video frame capture and texture upload (Phase 2)
- [ ] Playback controls forwarded to browser

---

## License

MIT