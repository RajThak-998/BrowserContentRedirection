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

**Overlay positioning (native Win32, not Wails)** — Wails' `WindowSetPosition`/`WindowSetSize` turned out to be unusable for this: `SetPos` treats the coordinates as relative to the **work area of whatever monitor the window is currently on**, so on a second monitor every update re-added that monitor's origin and the window walked off the desktop. `bcr_client/winplace_windows.go` + `placer.go` bypass Wails entirely — the overlay's HWND is found once via `EnumWindows` filtered by PID + a named window class (`WindowClassName: "BCROverlayWindow"` in `main.go`), and geometry is applied with a single atomic `SetWindowPos(..., HWND_TOPMOST, ..., SWP_NOACTIVATE|SWP_ASYNCWINDOWPOS)` in physical pixels. `placer.go` coalesces updates (always applies the *last* rect of a burst, never a stale intermediate one) and re-asserts `HWND_TOPMOST` on a heartbeat so the overlay can't be raised-over by clicking the RDP window. Requires per-monitor DPI awareness, which is set both by the app manifest and defensively at process `init()`.

**Overlay visibility** — shown only when **both** hold: `mseActive` (the frontend is actually decoding frames) and `onScreen` (the extension reports the video is actually visible in the VDI browser — not minimized, not backgrounded, not scrolled out of the viewport). `onScreen` comes from `document.visibilityState` plus intersecting the video's rect with the browser's content area in `videoTracker.js`; when the video is *partially* scrolled off-screen the overlay window is clipped to the visible portion and a `clip` transform (scale + offset) is sent to the frontend so the `<video>` element inside still lines up pixel-for-pixel. MSE-inactive hides are debounced (`overlayHideGrace`, 2s) to ride out YouTube's teardown/rebuild churn at ad boundaries; `onScreen` changes are **not** debounced, since they reflect a deliberate state (tab hidden, window minimized) with no churn to wait out.

**Multi-monitor / RDP viewport mapping** — the extension measures the video in the **VDI's own desktop coordinate space** (origin `0,0`), which only coincides with the local desktop when the RDP session is fullscreen on the local primary monitor. `winplace_windows.go`'s `findRDPViewport()` locates the local RDP client window (matched by **process name** — `mstsc.exe`, `msrdc.exe`, `msrdcw.exe`, `rdclient.exe` — since window classes are undocumented and differ between clients) and translates remote coordinates by that window's client-area origin, re-polled every 400ms so dragging the RDP window between monitors is tracked. **Does not yet handle scale** (smart-sizing / a client area sized differently than the remote resolution) — see Future scope.

**bcr_client reachability & fallback** — if the local player isn't running (or dies mid-playback), there is nothing to redirect to, so the extension must leave YouTube alone. `bcr_host` tracks its own bridge connect/disconnect (not traffic staleness, which can't distinguish "client dead" from "nothing playing yet") and pushes `BCR_STATUS` to the extension. A content script queries this at page load before deciding whether to suppress the video at all; if the client disappears mid-session, `pageInterceptor.js`'s `restoreNativePlayback()` un-suppresses the element and hands it back to native VDI playback (no prototype patches to unwind in the default pulse-decode mode — media was already reaching the real decoder, only pause/mute/occlusion needs undoing). No auto re-enable if the client comes back — resuming redirection mid-playback would silently jump the picture and cut the page's audio, so a reload is required.

**Media source lifecycle** — closing the YouTube tab (or navigating away from it) is detected in `background.js` via `chrome.tabs.onRemoved`/`onUpdated`, since a dying page frequently can't deliver its own `pagehide` message. This sends a definitive `VIDEO_SOURCE_GONE` that bypasses both the extension's own removal debounce and `bcr_client`'s `overlayHideGrace`/`TEARDOWN_GRACE_MS` (700ms), tearing down immediately instead of playing out the remaining buffer and stalling. As a backstop for the cases with no clean signal at all (VDI browser crash, extension unloaded, bridge severed), the frontend's 1Hz reporting loop also runs a starvation watchdog: buffer exhausted **and** no chunk received for `MEDIA_STARVATION_MS` (8s) tears the pipeline down. The 8s threshold is deliberately longer than a normal pulse-decode gap (which can leave 30–60s of *buffered*, not exhausted, video) so it can't false-fire during ordinary playback.

**Geometry gets a dedicated transport lane** — `VIDEO_UPDATE` (overlay position) is state, not an event: only the newest value matters. `bcr_host/router.go` gives it its own single-slot, replace-on-full channel on both ingress and egress, so it's never queued behind media chunks or dropped by the control-channel backpressure watermark — this is what keeps the overlay tracking smoothly even while a 4K stream is saturating the link.

---

## Known limitations

- **Media queues drop on overflow.** bcr_host's ingress/bridge channels for MEDIA_CHUNK are bounded and silently drop frames when full; a dropped segment leaves a timeline gap the player can't refill (no retransmit). Under a slow DVC this can cause mid-video stalls. (Overlay geometry no longer has this problem — see the dedicated transport lane note above.)
- **RDP viewport scale not handled.** If the RDP session uses smart-sizing (client window a different size than the remote resolution), the overlay's *position* still follows the RDP window but its *size* will be off by the scale ratio. Only a 1:1 client-to-remote size is fully correct today.
- **Minimizing the local RDP client window doesn't hide the overlay.** Overlay visibility is driven by state reported from *inside* the VDI (is the tab/window visible in the remote session), which doesn't change just because the user minimized the window they're viewing that session through — the remote session keeps running and reporting itself as visible. `findRDPViewport()` already detects when the local RDP window is minimized (`IsIconic`) for placement purposes; wiring that same signal into the overlay's visibility gate in `app.go` would close this, but hasn't been done yet.
- **No VDI↔local pause/seek sync.** The remote page and the local overlay track playback independently.
- **Windows/RDP only** for the production DVC path; other platforms use the local TCP fallback.

---

## Future scope: multi-monitor configuration

Current behavior is heuristic and covers the common case (fullscreen RDP session, 1:1 scale) well, but a few things are still open:

1. **Scale mapping.** Extend `findRDPViewport()`'s result with the remote session's native resolution (bcr_host can query this with `GetSystemMetrics(SM_CX/CYVIRTUALSCREEN)` inside the VDI session and report it once on connect) and apply `scale = clientSize / remoteResolution` to both the position and size of the overlay rect, not just the origin translation that exists today.
2. **Local-window-minimize → overlay hide**, per the known limitation above — a small addition since the minimized-state detection already exists in `winplace_windows.go`, it just isn't consulted by the visibility gate yet.
3. **Explicit multi-monitor override.** The RDP client is currently found by process name and picked by largest visible window — reasonable as a default, but there's no way to override it if a user runs multiple RDP sessions at once or a non-standard client. A config value (env var or a small JSON config file bcr_client reads at startup) naming a specific window title/monitor to bind to would remove the ambiguity for that case.
4. **True per-monitor DPI mapping**, if the VDI and local displays ever run at different scale factors — today the code assumes a uniform scale and doesn't attempt to reconcile mismatched DPI between the remote desktop and the local monitor showing it.

None of these are required for the common single-session, 1:1-scale, fullscreen-or-normal-window setups this has been validated against — they matter for less standard deployments.

---

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Overlay never appears | Extension loaded? bcr_host reachable on `:8765`? bcr_client running on `:8081`? Check `bcr_client.log` for `[winplace] process DPI awareness:` (should be `PER_MONITOR_AWARE`) and `[placer] overlay window resolved hwnd=...`. |
| Overlay never appears, but video plays fine in-VDI | Likely the fallback: bcr_client wasn't reachable when the page loaded, so the extension left YouTube alone (this is expected behavior, not a bug — see "bcr_client reachability & fallback" above). Confirm bcr_client is running and reload the tab. |
| Overlay on the wrong monitor | Check `bcr_client.log` for `[placer] RDP session viewport: ...` — if it's missing, `findRDPViewport()` didn't recognize the RDP client process (see the process name list in the multi-monitor note above). If present but still wrong, you may be hitting the unhandled smart-sizing/scale case — see Known limitations. |
| DVC never connects | bcr_host in the interactive session (not Session 0) and non-elevated? DLL registered under the correct HKCU AddIns key? DebugView showing `BCR DLL:` lines? |
| `WTSVirtualChannelOpenEx failed` | Run bcr_host from the RDP desktop session; check the printed session ID (`BCR_SESSION_ID` can override). |
| Video plays but no audio | Opus `<audio>` element — confirm autoplay (`WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--autoplay-policy=no-user-gesture-required` is set by bcr_client). |
| Overlay stuck on screen after closing the YouTube tab | Should now be fixed by `VIDEO_SOURCE_GONE` detection (see "Media source lifecycle" above) — if it still happens, check whether `chrome.tabs.onRemoved` fired in the background service worker console. |
