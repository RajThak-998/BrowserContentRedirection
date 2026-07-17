# BCR — Browser Content Relay

A real-time media redirection pipeline that intercepts in-browser streams and redirects them to a hardware-accelerated native desktop overlay — bypassing the VDI browser renderer entirely.

---

## 🎥 1. YouTube Video Redirection (MSE)

YouTube redirection intercepts adaptive video and audio chunks (MSE) in the VDI Chrome browser and streams them down to a native Wails player overlay.

### Developer / Local Setup

> [!IMPORTANT]
> **Prerequisite:** For production RDP/VDI environments, compile and register the RDP Dynamic Virtual Channel plugin. Follow the registry registration and compilation steps in the [RDP Dynamic Virtual Channel (DVC) DLL Plugin](#3-rdp-dynamic-virtual-channel-dvc-dll-plugin) section first.

#### Step 1 — Start the Native Client Overlay (on the Local Client Machine)
The Wails native player renders the redirected video stream hardware-accelerated on the user's physical machine.
```bash
cd bcr_client
go mod tidy
cd frontend && npm install && cd ..
wails dev
# Launches the Wails Native Player UI overlay window
```

#### Step 2 — Load the Chrome Extension (on the VDI Host)
1. Open Google Chrome/Chromium inside the VDI session and navigate to `chrome://extensions`.
2. Toggle **Developer mode** in the top right corner.
3. Click **Load unpacked** and select the `video-telemetry-extension/` directory.

#### Step 3 — Start the Relay Host (on the VDI Host)
Start the relay server with the browser extension already preloaded in Chrome.
```bash
cd bcr_host
go mod tidy
go run .
# Listens on :8765 (VDI browser extension websocket) and :8081 (client control channel)
```

#### Step 4 — Play the Video
Once running, play any YouTube video in Chrome on the VDI browser. The native overlay window will automatically cover the video element, play the synchronized audio locally on the client, and render the VP9/H.264 video.

---

## 📞 2. WebRTC Live Call Offloading

WebRTC redirection pipeline intercepts browser RTP streams at the JavaScript prototype layer, synthesizes shadow DTLS/ICE configurations, and offloads decryption and rendering to the local client.

### Key Features
* **Zero-copy SDP hijacking** — Intercepts WebRTC negotiation at the JS prototype level.
* **Shadow credential synthesis** — Generates instant shadow credentials to allow Teams to proceed immediately, trickling actual ICE candidates in the background (Trickle ICE).
* **Direct SFU transport** — Client connects directly to the remote server, receiving media packets natively.

### Developer / Local Setup
Follow the same startup steps as the YouTube Redirection. Once a WebRTC call is joined (e.g. Teams on the web), the extension automatically redirects the live stream to the native player overlay.

---

## 🔌 3. RDP Dynamic Virtual Channel (DVC) DLL Plugin

For seamless VDI redirection over RDP, the client and the VDI host communicate through a dynamic virtual channel instead of raw WebSocket connections. This is done using a native Windows RDP plugin DLL.

### Step 1 — Compile the DLL Plugin (on Client Machine)
To compile the plugin DLL using `g++` inside MSYS2 / MinGW:
```bash
cd bcr_client/dvc_plugin
g++ -shared -static -o bcr_dvc_plugin.dll dvc_plugin.cpp -lwtsapi32 -lws2_32 -lole32 -luuid
```

### Step 2 — Register the DLL in the Windows Registry
To let the Windows Remote Desktop Client (`mstsc.exe`) know about the plugin, register it on the **physical local client machine**:

1. Open **Registry Editor** (`regedit.exe`).
2. Navigate to the following path:
   `HKEY_CURRENT_USER\Software\Microsoft\Terminal Server Client\Default\AddIns`
3. Create a new Registry Key (Folder) named:
   `bcr_dvc_plugin`
4. Inside the `bcr_dvc_plugin` key, create a new **String Value (REG_SZ)**:
   * **Value Name**: `Name`
   * **Value Data**: The absolute file path to the compiled DLL (e.g., `C:\Development\BrowserContentRedirection\bcr_client\dvc_plugin\bcr_dvc_plugin.dll`).

When you launch Remote Desktop Connection (`mstsc.exe`) and connect to your VDI host, the RDP client will automatically load the DLL and establish the `BCR_VC` virtual channel bridge.

---

## Technical Deep Dive

### 1. Media Starvation via Missing RTCP Feedback
* **Problem:** Completing the DTLS handshake with the remote SFU resulted in zero media packets because modern SFUs gate RTP streams.
* **Solution:** Added a proactive RTCP feedback loop in the Go engine to begin dispatching periodic RTCP PLI requests to the SFU on handshake completion.

### 2. WebKit Decoder Deadlocks (Lazy Track Loading)
* **Problem:** Allocating all transceivers simultaneously caused WebKit rendering crash loops.
* **Solution:** Implemented lazy track loading in `loopback.go`. Tracks are dynamically added to the local connection only upon arrival of the first decrypted RTP packet for that SSRC.

### 3. Dual MediaSource Audio Fix (MSE)
* **Problem:** Edge WebView2 on the native client restricts `MediaSource` to exactly one active `SourceBuffer` for WebM streams, throwing `QuotaExceededError` if both video and audio buffers are added to the same instance.
* **Solution:** Configured separate `MediaSource` objects for the `<video>` player and a dynamically generated `<audio>` player, syncing playback state, volume, and time drift in real-time.

---

## Repository Structure

```
BCR/
├── bcr_host/                  # Go WebSocket & DVC host relay server
├── bcr_client/                # Go WebRTC engine + Wails overlay
│   ├── dvc_plugin/            # Windows RDP DLL connector
│   ├── raw_session.go         # Direct handshakes with remote SFU
│   ├── loopback.go            # Local WebRTC loopback stream
│   └── frontend/              # Wails player UI
└── video-telemetry-extension/ # Manifest V3 browser extension
```