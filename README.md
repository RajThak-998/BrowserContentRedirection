# BCR — Browser Content Relay

> Real-time video telemetry + WebRTC media redirection pipeline from browser to native overlay.

BCR intercepts WebRTC media streams (e.g., Teams calls) inside a Chrome tab and redirects them to a native Wails desktop application — with no screen capture, no repacking, and no noticeable latency.

It also tracks `<video>` element geometry and mirrors the video's exact screen position as a floating overlay window, so the native player follows the browser video in real time.

---

## What It Does

1. **Chrome extension** (`video-telemetry-extension`) detects `<video>` elements and any `RTCPeerConnection` calls on the page.
2. Extension **intercepts the browser's WebRTC SDP signaling** via prototype-level patches on `RTCPeerConnection` methods (`setLocalDescription`, `setRemoteDescription`, `createOffer`, `createAnswer`).
3. Intercepted SDP and ICE server credentials are forwarded (as `RTC_SHADOW_*` packets over WebSocket) to the **relay server** (`bcr_host`).
4. `bcr_host` bridges the packets to the **native client** (`bcr_client`) over a dual-channel WebSocket bridge.
5. `bcr_client` runs a **shadow `RTCPeerConnection`** (using [Pion WebRTC](https://github.com/pion/webrtc)) that mirrors the browser's negotation — same codecs, same media sections, same ICE servers — connecting directly to the remote Teams SFU.
6. When the shadow PC receives media, a **local relay `RTCPeerConnection`** forwards the RTP tracks to the **Wails WebView** (the native overlay's built-in browser) via a localhost WebRTC session.
7. The WebView renders the video in a frameless, always-on-top, transparent-background overlay window. Video element geometry (position, size, visibility) is tracked independently and used to keep the overlay window perfectly aligned with the browser video.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  Chrome Tab (VDI App, e.g. Teams)                                          │
│                                                                             │
│  RTCPeerConnection                                                          │
│   setLocalDescription() ─── pageInterceptor.js patches ──► BCR_RTC_SHADOW_LOCAL  │
│   setRemoteDescription() ─── patches ──────────────────► BCR_RTC_SHADOW_REMOTE │
│   createOffer/createAnswer() ── patches ────────────────► (same flow)       │
│                                                                             │
│  <video> element ─── content scripts ──────────────────► VIDEO_UPDATE       │
│                                                                             │
│  MSE SourceBuffer.appendBuffer() ── patched ───────────► MEDIA_CHUNK (binary)│
└──────────────────────────────────────┬──────────────────────────────────────┘
                                       │  chrome.runtime.sendMessage
                                       ▼
                        ┌──────────────────────────┐
                        │  Service Worker           │
                        │  (background.js)          │
                        │  WebSocket client         │
                        └──────────┬───────────────┘
                                   │  ws://localhost:8765
                                   ▼
                        ┌──────────────────────────┐
                        │  bcr_host (Go)            │
                        │  WebSocket relay server   │
                        │  ── prioritized queues ── │
                        │  high: RTC_SHADOW_*        │
                        │  low:  VIDEO_UPDATE, chunks│
                        │  ── bridgeForwarder ────── │
                        │  control channel (:8081)   │
                        │  data channel    (:8081)   │
                        └──────────┬───────────────┘
                                   │  ws://localhost:8081
                                   ▼
                        ┌──────────────────────────────────────────────────┐
                        │  bcr_client (Go + Wails)                        │
                        │                                                  │
                        │  Engine (internal/engine.go)                    │
                        │   ├─ readControlLoop: handles RTC_SHADOW_*      │
                        │   │     + VIDEO_UPDATE packets                  │
                        │   └─ readDataLoop: handles binary MEDIA_CHUNKs  │
                        │                                                  │
                        │  Shadow PeerConnection (Pion)                   │
                        │   ├─ createAlignedShadowPC()                    │
                        │   │     mirrors browser offer (codecs, mids,    │
                        │   │     transceivers) for SFU compatibility     │
                        │   ├─ ICE gathering with TURN credentials        │
                        │   ├─ DTLS handshake with Teams SFU              │
                        │   └─ OnTrack ──► relay session                  │
                        │                                                  │
                        │  Relay PeerConnection (Pion → Wails WebView)    │
                        │   ├─ TrackLocalStaticRTP per received track     │
                        │   ├─ forwardRTP goroutine (shadow → relay)      │
                        │   ├─ OnRelayOffer ──► Wails runtime.EventsEmit │
                        │   └─ HandleRelayAnswer (from JS WebRTC answer)  │
                        │                                                  │
                        │  Wails App (app.go)                             │
                        │   ├─ applyWindowFromTelemetry (VIDEO_UPDATE)    │
                        │   ├─ SendRelayAnswer (JS → Go binding)          │
                        │   └─ Frameless, AlwaysOnTop window overlay      │
                        └──────────────────────────────────────────────────┘
```

---

## WebRTC Routing — How Shadow Signaling Works

This is the core of BCR's media redirection. When a VDI app (e.g., Microsoft Teams) opens a call, the extension intercepts the SDP negotiation and silently mirrors it through a **shadow PeerConnection** in the Go client:

### 1. Offer Intercept (Browser → SFU)

When the browser calls `createOffer()` or `setLocalDescription(offer)`:
- `pageInterceptor.js` captures the offer SDP and emits a `BCR_RTC_SHADOW_LOCAL` event.
- The event is picked up by the isolated-world content script and forwarded via `chrome.runtime.sendMessage` to the **service worker**.
- The service worker sends a `RTC_SHADOW_LOCAL` WebSocket packet to `bcr_host`.

### 2. Shadow PC Creation (`bcr_client/internal/engine.go`)

On receiving `RTC_SHADOW_LOCAL`, the engine calls `createAlignedShadowPC()`:
- Parses the browser's offer SDP using `ParseSDPCodecsStrict()` and `ParseOfferMediaSections()`.
- Builds a Pion `MediaEngine` seeded with the **exact same codec payload types and rtcp-fb parameters** as the browser — no `RegisterDefaultCodecs()` — to guarantee PT alignment.
- Adds one transceiver per `m=` section (audio/video as `recvonly`; application as a dummy DataChannel).
- Calls `CreateOffer()` + `SetLocalDescription()` to produce the shadow's own offer.
- Waits for ICE gathering (with TURN credentials from the browser) and then fires `RTC_SHADOW_READY`.

### 3. SHADOW_READY → SDP Munge (Browser-side)

The `RTC_SHADOW_READY` packet carries the shadow PC's ICE credentials, DTLS fingerprint, and gathered candidates. Back in the browser:
- `mungeSdpTransport()` in `pageInterceptor.js` rewrites the browser's local SDP (already set on the native PC) to embed the **shadow's ICE ufrag/pwd, DTLS fingerprint, and candidates**.
- The VDI app reads `pc.localDescription` (which returns the munged copy via a patched getter) and sends that over its signaling channel to the Teams SFU.
- The SFU now believes it is talking to the shadow transport, not the browser's real ICE agent.

### 4. Answer Intercept (SFU → Browser)

When the SFU sends its answer and the browser calls `setRemoteDescription(answer)`:
- `pageInterceptor.js` captures the answer SDP and emits `BCR_RTC_SHADOW_REMOTE`.
- The Go engine receives `RTC_SHADOW_REMOTE`, optionally runs `ScrubGhostPayloadTypes()` and `TranslateAnswerMids()` to align the answer with the shadow's offer, and calls `shadow.pc.SetRemoteDescription()`.

### 5. ICE + DTLS (Shadow PC ↔ SFU)

The shadow PC in `bcr_client` completes ICE connectivity and performs the DTLS handshake directly with the Teams SFU. No media flows through the browser at all after this point.

### 6. RTP Track Relay (Shadow PC → Wails WebView)

When the shadow PC fires `OnTrack`:
- `relay.go` creates a `TrackLocalStaticRTP` for each incoming track.
- A `relaySession` holds a second Pion `PeerConnection` (the **relay PC**) that connects only to localhost.
- A 200 ms settle window batches audio + video tracks before sending the relay offer.
- The relay offer SDP is emitted via `OnRelayOffer` → `runtime.EventsEmit("onRelayOffer", ...)` to the Wails WebView.
- The WebView creates a native `RTCPeerConnection`, calls `setRemoteDescription(relayOffer)`, and returns its answer via `window.go.main.App.SendRelayAnswer()`.
- `HandleRelayAnswer()` applies the answer to the relay PC, ICE+DTLS negotiates on localhost, and media begins flowing to `video.srcObject` in the Wails WebView.

### Packet Types

| Packet | Direction | Description |
|---|---|---|
| `RTC_SHADOW_LOCAL` | Extension → bcr_host → bcr_client | Browser's local SDP (offer or answer) |
| `RTC_SHADOW_REMOTE` | Extension → bcr_host → bcr_client | Browser's remote SDP (SFU answer or re-offer) |
| `RTC_SHADOW_ICE_CANDIDATE` | Bidirectional | Single trickle-ICE candidate |
| `RTC_SHADOW_CLOSE` | Extension → bcr_host → bcr_client | PeerConnection closed/failed |
| `RTC_SHADOW_READY` | bcr_client → bcr_host → Extension | Shadow PC credentials + candidates |
| `RTC_SHADOW_ERROR` | bcr_client → bcr_host → Extension | Shadow PC failure notification |

### SDP Utility Functions (`bcr_client/internal/sdp.go`)

| Function | Purpose |
|---|---|
| `ExtractShadowCredentials` | Pull ICE ufrag/pwd + DTLS fingerprint from Pion's local description |
| `ExtractShadowCandidates` | Deduplicated `a=candidate:` lines from BUNDLE SDP |
| `ParseSDPCodecsStrict` | Exact PT + rtcp-fb extraction for aligned MediaEngine |
| `ParseOfferMediaSections` | Per-`m=` section metadata for transceiver mirroring |
| `normalizeSDP` | Inject missing `a=mid:` lines for strict Pion parser |
| `TranslateAnswerMids` | Remap answer mids to shadow's offer mids positionally |
| `ScrubGhostPayloadTypes` | Strip PTs from answers that weren't in the offer |

---

## Project Structure

```
BCR/
├── bcr_host/                        # Go WebSocket relay server
│   ├── main.go                      # Entry point, HTTP server
│   ├── server.go                    # WebSocket upgrade handler
│   ├── registry.go                  # Connection registry (extension + clients)
│   ├── router.go                    # Per-connection read loop, message routing,
│   │                                #   prioritized queues (high: RTC_SHADOW_*,
│   │                                #   low: VIDEO_UPDATE + MEDIA_CHUNK),
│   │                                #   bridgeForwarder (dual-channel to bcr_client)
│   └── protocol.go                  # Shared packet type definitions + constants
│
├── bcr_client/                      # Go native client — Wails desktop app
│   ├── main.go                      # Wails entry point, frameless overlay window
│   ├── app.go                       # App struct: startup, relay answer handling,
│   │                                #   window position/size application,
│   │                                #   SendRelayAnswer (JS→Go binding)
│   ├── go.mod                       # Dependencies: Pion WebRTC v4, Wails v2, Gorilla WS
│   ├── internal/
│   │   ├── engine.go                # Core engine: dual-channel WS server,
│   │   │                            #   shadow PC lifecycle, aligned PC creation,
│   │   │                            #   ICE/DTLS diagnostics, no-track watchdog
│   │   ├── relay.go                 # Local relay PC: OnTrack → TrackLocalStaticRTP
│   │   │                            #   → relay offer → Wails WebView → RTP forwarding
│   │   ├── sdp.go                   # SDP parsing, munge utilities, mid translation,
│   │   │                            #   ghost PT scrubbing
│   │   └── types.go                 # Packet structs: RTC_SHADOW_*, MediaSection, Config
│   └── frontend/                    # Wails WebView frontend (Vite + vanilla JS)
│       ├── index.html
│       └── src/
│
└── video-telemetry-extension/       # Chrome Extension (Manifest V3)
    ├── manifest.json                # Extension manifest
    ├── background/
    │   └── background.js            # Service worker: WebSocket transport to bcr_host,
    │                                #   RTC_SHADOW_* packet forwarding, reconnect backoff
    ├── content/
    │   ├── bootstrap.js             # Content script entry point
    │   ├── pageInterceptor.js       # Page-world (MAIN) script injected at document_start:
    │   │                            #   patches RTCPeerConnection prototype (setLocalDesc,
    │   │                            #   setRemoteDesc, createOffer, createAnswer),
    │   │                            #   munges localDescription with shadow credentials,
    │   │                            #   dispatches synthetic trickle-ICE candidates,
    │   │                            #   suppresses primary <video> decode for MSE relay
    │   ├── emitter.js               # chrome.runtime.sendMessage wrapper with retry logic
    │   ├── videoRegistry.js         # DOM video element discovery + tracking
    │   ├── videoTracker.js          # Per-video observer pipeline
    │   ├── stateManager.js          # Delta computation + change filtering
    │   ├── observers.js             # ResizeObserver, IntersectionObserver, scroll,
    │   │                            #   fullscreen, playback listeners
    │   └── overlayRenderer.js       # In-browser debug overlay (visual highlight)
    └── utils/
        └── throttle.js              # Leading-edge RAF throttle
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
    webkit2gtk4.0-devel \
    gcc \
    pkg-config
```

> [!NOTE]
> `webkit2gtk4.0-devel` is required for the Wails WebView on Linux.

### Go

```bash
go version  # requires Go 1.22+
```

### Wails CLI

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@latest
wails doctor   # verify system dependencies
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
go mod tidy
cd frontend && npm install && cd ..
```

### 4. Load Chrome Extension

1. Open `chrome://extensions`
2. Enable **Developer mode** (top-right toggle)
3. Click **Load unpacked**
4. Select the `video-telemetry-extension/` directory

---

## Running

Open **two terminals**:

**Terminal 1 — Relay server**
```bash
cd BCR/bcr_host
go run .
# Listens on :8765 (extension WebSocket) and bridges to bcr_client on :8081
```

**Terminal 2 — Native client + overlay**
```bash
cd BCR/bcr_client
wails dev
# Or for production: wails build && ./build/bin/bcr_client
```

Then open a Teams call (or any WebRTC app) in Chrome.

The extension intercepts the WebRTC negotiation, the shadow PC connects to the SFU, and the media is relayed to the Wails overlay window. Video geometry telemetry keeps the overlay positioned over the browser video element.

---

## How It Works (Full Flow)

```
[Chrome Tab — Teams call]
  RTCPeerConnection.createOffer()
        ↓ patched by pageInterceptor.js
  BCR_RTC_SHADOW_LOCAL → window.postMessage
        ↓ content script → chrome.runtime.sendMessage
[Service Worker — background.js]
  RTC_SHADOW_LOCAL → WebSocket → bcr_host (:8765)
        ↓
[bcr_host — Go]
  High-priority queue → bridgeForwarder → control channel
        ↓ ws://localhost:8081?channel=control
[bcr_client — Engine]
  createAlignedShadowPC() builds matching Pion PC
  CreateOffer() + SetLocalDescription()
  ICE Gathering (STUN/TURN from browser config)
        ↓
  sendShadowReady() → RTC_SHADOW_READY
        ↓ control channel → bcr_host → extension
[pageInterceptor.js]
  mungeSdpTransport() rewrites browser localDescription
  SFU receives shadow ICE + DTLS credentials
        ↓ Teams SFU answers
  setRemoteDescription(sfuAnswer) intercepted
  BCR_RTC_SHADOW_REMOTE → bcr_client
        ↓
[Shadow PC — Pion]
  ICE + DTLS handshake with SFU (direct, off-browser)
  OnTrack fires for audio + video
        ↓
[relay.go]
  TrackLocalStaticRTP per track
  Relay PeerConnection (Pion → Wails WebView)
  200 ms settle → CreateOffer → ICE gather (localhost)
  OnRelayOffer → runtime.EventsEmit("onRelayOffer")
        ↓
[Wails WebView — frontend]
  setRemoteDescription(relayOffer)
  createAnswer() → SendRelayAnswer(bridgeID, sdp)
        ↓ Go binding → HandleRelayAnswer()
[Relay PC — Pion]
  SetRemoteDescription(answer)
  ICE + DTLS on localhost
  RTP flows → video.srcObject
        ↓
  [<video> in Wails overlay window]

Parallel: VIDEO_UPDATE telemetry → applyWindowFromTelemetry()
  → Wails WindowSetPosition + WindowSetSize
```

---

## Configuration

| Location | Constant / Field | Default | Description |
|---|---|---|---|
| `bcr_host/main.go` | `listenAddr` | `:8765` | Extension-facing WebSocket port |
| `bcr_host/router.go` | `bridgeControlURL` | `ws://localhost:8081?channel=control` | Host → client control bridge |
| `bcr_host/router.go` | `bridgeDataURL` | `ws://localhost:8081?channel=data` | Host → client binary data bridge |
| `bcr_host/router.go` | `bridgeFilterMode` | `"all"` | Media forward mode: `all`, `video-only`, `audio-only` |
| `bcr_client/internal/engine.go` | `Config.ListenAddr` | `:8081` | Client WebSocket server port |
| `bcr_client/app.go` | (TCP server) | `:8080` | Legacy TCP SDP signaling port |
| `background.js` | `_INITIAL_BACKOFF_MS` | `1000ms` | Extension WebSocket reconnect backoff (initial) |
| `background.js` | `_MAX_BACKOFF_MS` | `5000ms` | Extension WebSocket reconnect backoff (max) |
| `pageInterceptor.js` | `RTC_WAIT_TIMEOUT_MS` | `60000ms` | Timeout waiting for `RTC_SHADOW_READY` before failing open |

---

## Threading Model (`bcr_client`)

| Component | Thread | Reason |
|---|---|---|
| Wails app + WebView | Main thread (Wails runtime) | WebView requirement |
| WebSocket control read loop | Goroutine | Blocking I/O |
| WebSocket data read loop | Goroutine | Blocking I/O |
| Shadow PC operations | Goroutines (one per bridge) | Non-blocking signaling |
| ICE gathering | Internal Pion goroutines | Async gather |
| RTP forwarding | One goroutine per track | Per-packet read/write |
| Relay settle timer | `time.AfterFunc` goroutine | 200 ms batch window |
| DTLS transport watcher | Goroutine per shadow PC | 50 ms polling |
| No-track watchdog | Goroutine per ICE connect | 3 s diagnostic guard |

---

## Current Limitations

- Tracks only the **most recent** video element for geometry telemetry (single overlay window).
- WebRTC codecs with **IPv6 ICE candidates** are filtered out to avoid Teams parser crashes.
- `RTC_WAIT_TIMEOUT_MS` is temporarily set to 60 s while investigating ICE gather latency — should be reduced to ~5–8 s for production.
- `parseRTPDirection()` forces all transceivers to `recvonly` regardless of the browser's negotiated direction.
- **X11 only** for the native overlay — Wayland not tested (XWayland may work via compatibility layer).

---

## Roadmap

- [ ] Reduce `RTC_WAIT_TIMEOUT_MS` once ICE gathering latency is resolved
- [ ] Multi-video tracking (multiple overlay windows)
- [ ] HiDPI / device pixel ratio correction
- [ ] Wayland native support
- [ ] Bidirectional media (microphone from Wails WebView → SFU)
- [ ] Playback controls forwarded to browser
- [ ] Reduce relay PC latency (direct SRTP forwarding without re-negotiation)

---

## License

MIT