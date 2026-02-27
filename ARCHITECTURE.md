# BCR — High-Level Architecture

> This document describes the full system architecture of BCR (Browser Content Relay).
> It is intended as input for generating a formal HLD diagram.

---

## 1. System Overview

BCR is a local real-time pipeline that extracts video element telemetry from a Chrome browser tab and uses it to drive a native desktop overlay window. No screen capture or media decoding occurs. The browser voluntarily reports video element geometry via a Chrome Extension. A native Go application consumes this telemetry and renders a matching borderless window using GLFW and OpenGL.

---

## 2. Component Inventory

| ID | Component | Language / Runtime | Role |
|---|---|---|---|
| C1 | Chrome Extension — Content Script | JavaScript (MV3) | DOM observation, telemetry production |
| C2 | Chrome Extension — Service Worker | JavaScript (MV3) | WebSocket transport to bcr_host |
| C3 | bcr_host | Go | WebSocket relay server |
| C4 | bcr_client | Go | WebSocket client + native overlay renderer |
| C5 | GLFW Window | C (via cgo) | Native OS window management |
| C6 | OpenGL Context | C (via cgo) | GPU framebuffer rendering |

---

## 3. Data Flow

```
┌──────────────────────────────────────────────────────────────────────┐
│  Chrome Browser Process                                              │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │  Tab (Renderer Process)                                      │    │
│  │                                                              │    │
│  │  DOM                                                         │    │
│  │   └─ <video> element                                         │    │
│  │         │                                                    │    │
│  │         ▼                                                    │    │
│  │  VideoRegistry — MutationObserver (DOM scan)                 │    │
│  │         │                                                    │    │
│  │         ▼                                                    │    │
│  │  VideoTracker (per video)                                    │    │
│  │   ├─ ResizeObserver       → size changes                     │    │
│  │   ├─ IntersectionObserver → viewport visibility              │    │
│  │   ├─ scroll listener      → position changes                 │    │
│  │   ├─ fullscreen listener  → fullscreen state                 │    │
│  │   └─ playback listeners   → play/pause/seek/rate             │    │
│  │         │                                                    │    │
│  │         ▼                                                    │    │
│  │  _readState()                                                │    │
│  │   getBoundingClientRect() + window.screenX/Y                 │    │
│  │   + (outerHeight - innerHeight) = screen-absolute coords     │    │
│  │         │                                                    │    │
│  │         ▼                                                    │    │
│  │  StateManager.computeDelta()                                 │    │
│  │   Position delta threshold: 1px                              │    │
│  │   Suppresses no-change events                                │    │
│  │         │                                                    │    │
│  │         ▼                                                    │    │
│  │  Emitter.emitUpdate/Added/Removed()                          │    │
│  │   chrome.runtime.sendMessage → Service Worker                │    │
│  └──────────────────────────────────────────────────────────────┘   │
│                    │                                                  │
│                    ▼                                                  │
│  ┌──────────────────────────────┐                                    │
│  │  Service Worker              │                                    │
│  │  (background.js)             │                                    │
│  │                              │                                    │
│  │  Transport (WebSocket)       │                                    │
│  │   └─ ws://localhost:8765/ws  │                                    │
│  │      ?role=extension         │                                    │
│  │                              │                                    │
│  │  Enriches packet with:       │                                    │
│  │   tabId, tabUrl, frameId     │                                    │
│  └──────────────────────────────┘                                    │
└──────────────────────────────────────────────────────────────────────┘
                     │
                     │  WebSocket (JSON text frames)
                     │  ws://localhost:8765/ws?role=extension
                     ▼
┌──────────────────────────────────────────────────────────────────────┐
│  bcr_host  (Go)                                                      │
│                                                                      │
│  HTTP /ws endpoint                                                   │
│   └─ WebSocket upgrade (gorilla/websocket)                           │
│                                                                      │
│  Registry                                                            │
│   ├─ extension slot (single, last-one-wins)                          │
│   └─ clients map  (multiple, keyed by UUID)                          │
│                                                                      │
│  ReadLoop (per connection, goroutine)                                │
│   └─ extension message → Registry.Broadcast() → all clients         │
│                                                                      │
│  Connection safety:                                                  │
│   └─ per-connection write mutex (concurrent send safety)             │
│   └─ CloseWithMessage (WriteControl + WS.Close)                      │
└──────────────────────────────────────────────────────────────────────┘
                     │
                     │  WebSocket (JSON text frames)
                     │  ws://localhost:8765/ws?role=client
                     ▼
┌──────────────────────────────────────────────────────────────────────┐
│  bcr_client  (Go)                                                    │
│                                                                      │
│  ┌──────────────────────┐      ┌──────────────────────────────────┐  │
│  │  WebSocket goroutine │      │  Main OS Thread (locked)         │  │
│  │                      │      │                                  │  │
│  │  Client.Run(ctx)     │      │  RunRenderLoop()                 │  │
│  │   └─ connect()       │      │   ├─ glfw.Init()                 │  │
│  │       └─ ReadMessage │      │   ├─ glfw.CreateWindow()         │  │
│  │           │          │      │   │   Decorated=False            │  │
│  │           ▼          │      │   │   Floating=True              │  │
│  │  HandlePacket(pkt)   │      │   │   Visible=False (start)      │  │
│  │           │          │      │   └─ render loop:                │  │
│  │           ▼          │      │       ├─ applyUpdates()          │  │
│  │  OverlayManager      │      │       │   drain geomCh           │  │
│  │   Create/Update/     │─────▶│       │   SetPos/SetSize         │  │
│  │   Destroy            │ chan │       │   Show/Hide              │  │
│  │           │          │      │       ├─ drawFrame()             │  │
│  │     geomCh<-update   │      │       │   glClear (blue)         │  │
│  │  (non-blocking send) │      │       │   SwapBuffers            │  │
│  └──────────────────────┘      │       └─ glfw.PollEvents()       │  │
│                                └──────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────┘
                     │
                     ▼
          Native OS Window (X11 / GLFW)
          Borderless, floating, always-on-top
          Position + size mirrors browser video element
          in real time
```

---

## 4. Packet Protocol

All messages are JSON text frames over WebSocket.

### Packet Envelope

```json
{
  "type": "VIDEO_ADDED | VIDEO_UPDATE | VIDEO_REMOVED",
  "payload": { ... },
  "meta": {
    "tabId": 123,
    "tabUrl": "https://youtube.com/watch?v=...",
    "frameId": 0
  }
}
```

### VIDEO_ADDED payload

```json
{
  "id": "uuid-v4",
  "timestamp": 1709123456789
}
```

### VIDEO_UPDATE payload

```json
{
  "id": "uuid-v4",
  "timestamp": 1709123456789,
  "bounds": {
    "x": 412.0,
    "y": 187.0,
    "width": 854.0,
    "height": 480.0
  },
  "visibility": {
    "inViewport": true,
    "intersectionRatio": 0.87
  },
  "playback": {
    "state": "playing",
    "currentTime": 42.5,
    "rate": 1.0
  },
  "fullscreen": false,
  "delta": {
    "dx": 0.0,
    "dy": -3.0,
    "dw": 0.0,
    "dh": 0.0
  }
}
```

### VIDEO_REMOVED payload

```json
{
  "id": "uuid-v4",
  "timestamp": 1709123456789
}
```

---

## 5. Coordinate System

```
Screen origin (0,0)
┌──────────────────────────────────────────────────────┐
│  Browser window                                      │
│  ┌────────────────────────────────────────────────┐  │
│  │  Chrome UI (tabs + address bar)                │  │
│  │  height = window.outerHeight - window.innerHeight  │
│  ├────────────────────────────────────────────────┤  │
│  │  Viewport (window.innerHeight)                 │  │
│  │                                                │  │
│  │   ┌──────────────────────┐                     │  │
│  │   │  <video> element     │                     │  │
│  │   │  getBoundingClient   │                     │  │
│  │   │  Rect() → x,y        │                     │  │
│  │   │  (viewport-relative) │                     │  │
│  │   └──────────────────────┘                     │  │
│  └────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────┘

Screen-absolute x = rect.left + window.screenX
Screen-absolute y = rect.top  + window.screenY + (outerHeight - innerHeight)
```

GLFW `SetPos(x, y)` takes screen-absolute pixel coordinates. The extension computes and sends these directly — `bcr_client` uses them as-is with no transformation.

---

## 6. Threading Architecture

```
Process: bcr_client

OS Thread 1 ─── runtime.LockOSThread() ────────────────────────────────
│
│  main()
│   ├─ geomCh := make(chan GeometryUpdate, 16)
│   ├─ overlayManager = NewOverlayManager(geomCh)
│   ├─ go signalWatcher()
│   ├─ go Client.Run(ctx)          ← spawns OS Thread N
│   └─ RunRenderLoop(geomCh, done) ← blocks here forever
│       ├─ glfw.Init()
│       ├─ glfw.CreateWindow()
│       └─ for loop:
│           ├─ select doneCh
│           ├─ applyUpdates(win, geomCh)  ← reads channel
│           ├─ drawFrame(win)
│           └─ glfw.PollEvents()

OS Thread N ─── goroutine ─────────────────────────────────────────────
│
│  Client.Run(ctx)
│   └─ connect(ctx)
│       └─ ReadMessage()  ← blocks on network I/O
│           └─ HandlePacket(pkt)
│               └─ overlayManager.Update(id, bounds, visibility)
│                   └─ select { case geomCh <- u: | default: drop }
│                      ↑ non-blocking — never stalls network read
```

**Key invariant:** No GLFW or OpenGL function is ever called from OS Thread N.
All window state changes flow through `geomCh` exclusively.

---

## 7. Connection Lifecycle

### Extension → bcr_host

```
Extension starts
  → Transport._createSocket()
  → WS connect: ws://localhost:8765/ws?role=extension
  → Registry.Register(conn) [last-one-wins slot]

Extension reconnects (service worker restart)
  → New conn arrives
  → Registry sends CloseGoingAway (1001) to old conn
  → Old conn closes
  → Extension receives 1001 → does NOT reconnect
    (replacement already registered)

Extension crashes / network drop
  → abnormal close code received by extension
  → _scheduleReconnect() with exponential backoff
    (initial: 1000ms, max: 5000ms)
```

### bcr_client → bcr_host

```
bcr_client starts
  → Client.Run(ctx) goroutine
  → DialContext ws://localhost:8765/ws?role=client
  → Registry.Register(conn) [added to clients map]

bcr_host not reachable
  → connect() returns false
  → Run() waits retryInterval (2s)
  → reconnects

bcr_client shuts down (SIGINT)
  → ctx cancelled
  → conn.Close() unblocks ReadMessage
  → defer sends CloseNormalClosure frame
  → Run() exits
  → main() resumes after RunRenderLoop exits
```

---

## 8. Content Script Observer Pipeline

```
VideoRegistry (singleton)
  │
  ├─ MutationObserver on document.body
  │   ├─ addedNodes   → _registerVideo(el)
  │   └─ removedNodes → _unregisterVideo(el)
  │
  └─ per video: VideoTracker
      │
      ├─ ResizeObserver        → fires on element resize
      ├─ IntersectionObserver  → fires at thresholds [0, 0.1, 0.25, 0.5, 0.75, 1.0]
      ├─ scroll (throttled 33ms RAF) → fires on page scroll
      ├─ fullscreen (3 strategies)   → native API + class mutation + size heuristic
      └─ playback events             → play, pause, seeking, ratechange, ended
           │
           ▼  (all funnel to same handler)
      _onObserverFired()
           │
           ▼
      _readState()  → fresh getBoundingClientRect() + screen coords
           │
           ▼
      StateManager.computeDelta()
           │  threshold: 1px positional change
           │  or: playback state change
           │  or: fullscreen change
           │  or: visibility change
           ▼
      Emitter.emitUpdate()  → chrome.runtime.sendMessage
```

---

## 9. State Change Filtering (StateManager)

The StateManager prevents telemetry flooding by suppressing no-op updates.

An update is emitted **only if** at least one of:

| Condition | Threshold |
|---|---|
| Position changed (`dx` or `dy`) | `> 1px` |
| Size changed (`dw` or `dh`) | `> 1px` |
| Playback state changed | any |
| Fullscreen changed | any |
| Visibility changed (with ratio agreement) | any |

This means rapid scroll events that don't actually move the video more than 1px are suppressed — preventing unnecessary packets and render loop churn.

---

## 10. Overlay Visibility Logic

```
VIDEO_ADDED
  → overlayManager.Create(id)
  → geomCh <- GeometryUpdate{default 640×360, Visible: true}
  → render loop: win.Show()

VIDEO_UPDATE
  → overlayManager.Update(id, bounds, visibility)
  → visible = visibility.IntersectionRatio > 0.0
  → geomCh <- GeometryUpdate{X,Y,W,H, Visible: visible}
  → render loop:
      if Visible:
          win.SetSize(W, H)
          win.SetPos(X, Y)
          win.Show()
      else:
          win.Hide()

VIDEO_REMOVED
  → overlayManager.Destroy(id)
  → geomCh <- GeometryUpdate{Visible: false}
  → render loop: win.Hide()
```

---

## 11. Key Design Decisions

| Decision | Rationale |
|---|---|
| Single relay server (bcr_host) instead of direct extension→client | Chrome extensions cannot open a TCP server; a local Go server bridges the gap |
| JSON text frames over binary | Simplicity for Phase 1; binary can replace it in Phase 2 for frame data |
| Channel (not mutex) for GLFW updates | GLFW must run on main OS thread; channel is the only safe cross-goroutine primitive |
| Non-blocking channel send (select/default) | WebSocket goroutine must never stall waiting for the render loop |
| Last-one-wins for extension slot | Service workers restart frequently in MV3; a reconnecting extension should always win |
| CloseGoingAway (1001) for extension replacement | Signals intentional server-side replacement; extension's Transport treats 1001 as "do not reconnect" |
| Reuse window on VIDEO_ADDED (not destroy/recreate) | GLFW window creation is expensive and causes visible flicker; SetPos/SetSize is instant |
| Hide on VIDEO_REMOVED (not destroy) | Avoids re-initializing GLFW context on next VIDEO_ADDED |
| Screen coords computed in extension | Extension has direct access to window.screenX/Y; computing in bcr_client would require platform-specific X11 calls |
| OpenGL v2.1 (not core profile) | glClear requires zero shader code in compatibility profile; no unnecessary complexity for Phase 1 |

---

## 12. Interfaces Between Components

```
[Content Script] → [Service Worker]
  Protocol: chrome.runtime.sendMessage (JSON)
  Direction: one-way (content → background)
  Message shape: { type, payload }

[Service Worker] → [bcr_host]
  Protocol: WebSocket, role=extension
  Direction: one-way (extension → host)
  Frame type: text (JSON)
  Message shape: { type, payload, meta }

[bcr_host] → [bcr_client]
  Protocol: WebSocket, role=client
  Direction: one-way (host → client)
  Frame type: text (JSON)
  Message shape: { type, payload, meta } (identical passthrough)

[WebSocket goroutine] → [Render loop]
  Protocol: Go buffered channel (cap=16)
  Direction: one-way (goroutine → main thread)
  Message shape: GeometryUpdate{X, Y, W, H, Visible}
```

---

## 13. Future Extension Points

| Seam | File | How to extend |
|---|---|---|
| Video frame rendering | `window.go: drawFrame()` | Upload decoded RGB to GL_TEXTURE_2D, draw fullscreen quad with shader |
| Multiple overlays | `overlay_manager.go` | Replace single `currentVideoID` with a map; manage a pool of GLFW windows |
| HiDPI support | `window.go: applyUpdates()` | Multiply X/Y/W/H by `glfw.GetMonitor().GetContentScale()` |
| Wayland support | `window.go` | Add `GLFW_PLATFORM_WAYLAND` hint; handle fractional scaling differently |
| Browser window offset calibration | `videoTracker.js: _readState()` | Add `window.screenX/Y` correction factor configurable via extension options page |
| Playback control forwarding | `packet_handler.go` | Send control messages back through bcr_host to extension |
| WebRTC frame capture | `videoTracker.js` | Capture frames via `canvas.drawImage(video)` → send as binary WS frames |