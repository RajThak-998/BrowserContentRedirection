# BCR — YouTube Video Redirection

Browser Content Redirection (BCR) offloads YouTube playback out of a VDI/RDP session. Instead of decoding video inside the remote browser and streaming pixels back over RDP (expensive, laggy), BCR intercepts the raw MSE media segments in the remote Chrome, ships them down a dynamic virtual channel to the physical endpoint, and plays them in a **native, hardware-accelerated overlay window** positioned exactly on top of the real (now-suppressed) YouTube player.

The remote browser still runs YouTube's page, JS, and controls — it just never decodes or renders the video. All decode/compositing happens locally.

> This document covers **YouTube redirection only**. The WebRTC/Teams call-offload path shares the same extension and transport but is documented separately.

---

## Architecture

```
              VDI / RDP SESSION (remote)                    │   PHYSICAL ENDPOINT (local)
                                                            │
  ┌──────────────────────┐    WebSocket        ┌──────────┐│ DVC "BCR_VC"  ┌───────────────┐   TCP     ┌──────────────┐
  │  Chrome + Extension   │  ws://localhost:8765 │ bcr_host ││   over RDP    │ bcr_dvc_plugin │ 127.0.0.1 │  bcr_client   │
  │  (MSE interceptor)    │ ───────────────────▶ │  (relay) ││ ────────────▶ │    .dll        │ :8081     │ (Wails player)│
  └──────────────────────┘   ?role=extension    └──────────┘│  (mstsc addin)└───────────────┘ ─────────▶ └──────────────┘
        intercepts MSE                              opens DVC │   bridges DVC ⇆ local TCP          native overlay + MSE
        segments in-page                            server side│                                    decode/audio/render
```

**Data flow:** YouTube fetches an MSE segment → the extension's page-world interceptor grabs it (and blocks the native decode) → `postMessage` to the isolated content script → `background.js` frames it as binary → WebSocket to **bcr_host** → **bcr_host** writes it to the **BCR_VC** dynamic virtual channel → the RDP client loads **bcr_dvc_plugin.dll**, which bridges the DVC to a local TCP socket → **bcr_client** receives the segment on `:8081`, and the Wails frontend appends it into a `MediaSource` and plays it in the overlay.

**Local-dev fallback (no RDP):** when the DVC is unavailable, bcr_host dials `127.0.0.1:8081` directly, so bcr_host and bcr_client can run on the same machine with **no plugin needed**.

### Components

| Path | Runs on | Role |
|------|---------|------|
| `video-telemetry-extension/` | VDI Chrome | MV3 extension: intercepts/suppresses the primary video, forwards MSE chunks, tracks overlay geometry |
| `bcr_host/` | VDI session | Go relay: WebSocket server (`:8765`) ⇆ RDP dynamic virtual channel (`BCR_VC`) |
| `bcr_client/dvc_plugin/` | Local endpoint | Native RDP DVC plugin DLL: bridges `BCR_VC` ⇆ local TCP `:8081` |
| `bcr_client/` | Local endpoint | Go engine + Wails overlay: receives chunks on `:8081`, plays via MSE |

### Ports & channels

| Endpoint | Where | Purpose |
|----------|-------|---------|
| `ws://localhost:8765/ws?role=extension` | VDI | Extension → bcr_host telemetry + media |
| TCP `127.0.0.1:8081` | Local | bcr_client media/control listener (DVC plugin or bcr_host connects in) |
| DVC `BCR_VC` | RDP link | bcr_host (server side) ⇆ plugin DLL (client side) |

---

## Setup & Run

**Prerequisites:** Go 1.21+, Node 18+, [Wails v2 CLI](https://wails.io) (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`), and for the DLL: MSYS2/MinGW-w64 `g++`. A working RDP/VDI connection for production; a single machine is fine for local dev.

### 1. bcr_client — native overlay (local endpoint)

```bash
cd bcr_client
go mod tidy
cd frontend && npm install && cd ..
wails dev          # dev overlay with hot reload
# or: wails build  → produces webrtc-player.exe
```
Starts the frameless, always-on-top overlay and the `:8081` listener. It stays hidden until media arrives.

### 2. Extension (VDI Chrome)

1. In the VDI browser open `chrome://extensions`.
2. Enable **Developer mode**.
3. **Load unpacked** → select `video-telemetry-extension/`.

After editing extension files, hit **Reload** on the card. `background.js` auto-connects to `ws://localhost:8765`.

### 3. bcr_host — relay (VDI session)

```bash
cd bcr_host
go mod tidy
go run .            # listens on :8765, opens BCR_VC (or falls back to TCP :8081)
```

> ⚠️ Run bcr_host **inside the interactive RDP session** as a **standard (non-elevated) user**. The DVC uses `WTS_CURRENT_SESSION`; from Session 0 or an elevated process that doesn't match `mstsc.exe`, the channel won't reach the client. Startup diagnostics print the session ID and elevation state.

### 4. DVC plugin DLL (local endpoint, production RDP only)

The DLL is loaded by `mstsc.exe` and bridges `BCR_VC` to the local `:8081` socket.

**Compile** (MSYS2/MinGW shell):
```bash
cd bcr_client/dvc_plugin
g++ -shared -static -o bcr_dvc_plugin.dll dvc_plugin.cpp -lwtsapi32 -lws2_32 -lole32 -luuid
```

**Register** for the RDP client (on the physical machine, per-user):
1. `regedit` → `HKEY_CURRENT_USER\Software\Microsoft\Terminal Server Client\Default\AddIns`
2. New key: `bcr_dvc_plugin`
3. Inside it, new **String value (REG_SZ)**:
   - Name: `Name`
   - Data: absolute path to the DLL, e.g. `C:\Development\BrowserContentRedirection\bcr_client\dvc_plugin\bcr_dvc_plugin.dll`

Reconnect with `mstsc.exe`; the client loads the DLL and stands up the `BCR_VC` bridge automatically. (Verify with [DebugView](https://learn.microsoft.com/sysinternals/downloads/debugview) — the DLL emits `BCR DLL:` traces via `OutputDebugString`.)

### 5. Play

Open any YouTube video in the VDI Chrome. The overlay appears over the player, audio plays locally, and video renders from the redirected VP9 stream.

---

## Implementation notes

**Primary-video election & suppression** — `pageInterceptor.js` runs in the **MAIN world at `document_start`** and patches `HTMLMediaElement`/`SourceBuffer`/`MediaSource` prototypes, so every present and future element is covered. It scores visible `<video>` elements by area and elects the largest as *primary* (hover-preview/thumbnail renderers are excluded). The primary is paused + muted, and its `appendBuffer` calls are **gated**: the data is copied out to the pipeline and the native decode is skipped, while a **synthetic `updateend`** is dispatched so YouTube's streaming loop keeps running. AV1 is blocked (`isTypeSupported`/`canPlayType`) to force YouTube onto **VP9 video + Opus audio** (WebM), which the local player decodes reliably.

**Chunk transport & framing** — Each intercepted segment carries a small JSON header (`trackType`, `mimeType`, `codec`, `sourceBufferId`, `videoId`, `isInitSegment`, `seq`) plus the raw bytes. `background.js` packs it as a binary frame `[u32 headerLen LE][headerJSON][rawChunk]` and sends it over the WebSocket. bcr_host writes **one frame per DVC message**; over TCP (and inside the DVC payload) frames are length-prefixed `[4-byte BE length][payload]`. On the client, the DVC read strips the 8-byte `CHANNEL_PDU_HEADER` that some Windows builds prepend, reassembles frames from the byte stream, and hands each segment to the engine, which base64-encodes it for the Wails `onMediaChunk` event.

**Dual MediaSource playback** — WebView2 restricts a single `MediaSource` to one WebM `SourceBuffer`, so the frontend uses **separate `MediaSource` instances** for a `<video>` and a hidden `<audio>` element, keeping play/pause/seek/rate/volume and time-drift (>150 ms) in sync. Chunks are appended one-per-`updateend` via a per-track queue; on `QuotaExceededError` the buffer behind the playhead is trimmed.

**Teardown debounce** — YouTube briefly removes/re-adds the `<video>` at ad boundaries and during DOM reshuffles. Rather than tearing down instantly on `VIDEO_REMOVED` (which forces a full rebuild + rebuffer), teardown is deferred ~700 ms and cancelled if media keeps flowing or the video is re-added. A `videoID` change tears down the stale session explicitly.

**Stall recovery** — the player watches for `waiting` events and runs a periodic soft-stall watchdog; if the playhead is stuck at a gap between buffered ranges it jumps past it, otherwise it nudges forward slightly to unstick the decoder.

**Overlay positioning** — the extension reports the player's on-screen rect (CSS px) and the Go host applies it via Wails. Note the Wails asymmetry: `WindowSetSize` expects **logical** pixels (DPI-scaled internally) while `WindowSetPosition` expects **physical** pixels, so position is multiplied by `devicePixelRatio` (matters on non-100% displays).

---

## Known limitations

- **Media queues drop on overflow.** bcr_host's ingress/bridge channels are bounded and silently drop MEDIA_CHUNK frames when full; a dropped segment leaves a timeline gap the player can't refill (no retransmit). Under a slow DVC this can cause mid-video stalls.
- **No VDI↔local pause/seek sync.** The remote page and the local overlay track playback independently.
- **Windows/RDP only** for the production DVC path; other platforms use the local TCP fallback.

---

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Overlay never appears | Extension loaded? bcr_host reachable on `:8765`? bcr_client running on `:8081`? |
| DVC never connects | bcr_host in the interactive session (not Session 0) and non-elevated? DLL registered under the correct HKCU AddIns key? DebugView showing `BCR DLL:` lines? |
| `WTSVirtualChannelOpenEx failed` | Run bcr_host from the RDP desktop session; check the printed session ID (`BCR_SESSION_ID` can override). |
| Video plays but no audio | Opus `<audio>` element — confirm autoplay (`WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--autoplay-policy=no-user-gesture-required` is set by bcr_client). |
| Overlay misaligned on scaled display | `devicePixelRatio` handling — see overlay positioning note above. |
