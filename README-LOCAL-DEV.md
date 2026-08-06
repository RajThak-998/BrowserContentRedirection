# Local development mode (and how to get back to VDI)

This branch (`local-dev`) runs all three BCR components on a single machine, with the
RDP virtual channel out of the picture.

**No VDI code has been deleted.** The virtual-channel transport is selected by an
environment variable. Going back to the VDI is a configuration change, not a code
restoration — there is nothing to revert or cherry-pick.

---

## The two topologies

### VDI (production)

```
  ── virtual desktop ─────────────────────┐   ┌── thin client ──────────────────
                                          │   │
   Teams tab                              │   │
     │  window.postMessage                │   │
   extension (content + background)       │   │
     │  ws://localhost:8765/ws            │   │
   bcr_host  ──── DVC "BCR_VC" ───────────┼───┼──▶ bcr_dvc_plugin.dll
                                          │   │        │  TCP 127.0.0.1:8081
                                          │   │     bcr_client  (plays media locally)
  ───────────────────────────────────────┘   └─────────────────────────────────
```

`bcr_dvc_plugin.dll` is loaded by `mstsc.exe` on the thin client and is a pure
DVC ⇆ `127.0.0.1:8081` byte pump. It is C++ and is **not** built by `go build`.

### Local (this branch)

```
  ── one machine ──────────────────────────────────────────────────────────────
   Teams tab
     │  window.postMessage
   extension (content + background)
     │  ws://localhost:8765/ws?role=extension
   bcr_host
     │  TCP 127.0.0.1:8081        ← no virtual channel, no plugin, no mstsc
   bcr_client  (Wails GUI: overlay player + MSE pipeline)
  ─────────────────────────────────────────────────────────────────────────────
```

The wire format is byte-identical on both paths: `[4-byte big-endian length][payload]`.
Only the pipe differs.

---

## Ports

| Port | Owner | Purpose |
|------|-------|---------|
| `8765` | bcr_host | WebSocket for the extension (`/ws?role=extension`) |
| `8081` | bcr_client | Signalling listener — bcr_host (or the DVC plugin) dials **in** |
| `5173` | Wails dev server | bcr_client frontend, `wails dev` only |

> A `?role=client` WebSocket peer on 8765 is a **debug/telemetry viewer**, not `bcr_client`.
> The two meanings of "client" are unrelated.

---

## Running locally

`local` is the default transport, so no environment variable is needed on this branch.

Start order matters — **bcr_host dials bcr_client**, so the client must be listening first.

```powershell
# terminal 1 — the Wails GUI build; required for MSE/YouTube and the player UI
cd bcr_client;  wails dev

# terminal 2 — once :8081 is listening
cd bcr_host;    go run .
```

Then load `video-telemetry-extension\` as an unpacked extension in Chrome and open Teams.

There is also a headless client (`bcr_client/cmd`) that runs signalling only, with no
player UI. Use it for bridge/signalling work; it cannot play media.

### Logs

Both binaries write to stdout **and** a file, truncated on every start, with microsecond
timestamps so the two timelines can be lined up against each other:

| File | Written by | Override |
|---|---|---|
| `bcr_client.log` | bcr_client | `-log <path>` |
| `bcr_host.log` | bcr_host | `-log <path>` |

By default each lands next to its executable.

### Confirming it worked

- `bcr_host.log`: `[DIAG] Transport: local — TCP 127.0.0.1:8081`, then
  `✅ transport active: local — TCP 127.0.0.1:8081` — and **no** virtual-channel lines
- `bcr_client.log`: `TCP signaling server listening on :8081`
- Extension console: `BCR_STATUS { clientConnected: true }`

---

## Environment variables

### Transport

| Variable | Values | Default | Effect |
|---|---|---|---|
| `BCR_TRANSPORT` | `local` \| `vdi` | `local` | `local` dials bcr_client over TCP on this machine. `vdi` opens the RDP Dynamic Virtual Channel `BCR_VC`. |
| `BCR_CLIENT_ADDR` | `host:port` | `127.0.0.1:8081` | Where bcr_host dials bcr_client in local mode. Also allows a two-machine LAN setup with no RDP. |

There is **no probe-and-fallback mode**, deliberately. An implicit fallback made a real DVC
failure in the VDI look like a signalling timeout: bcr_host would quietly dial
`127.0.0.1:8081` *inside the virtual desktop*, where bcr_client does not exist. Each mode now
fails as itself, and the active transport is logged at startup and on every bridge attempt.

Logging follows the mode: in `local` the RDP session and elevation checks are skipped (the
elevation check shells out to `net session`, which is slow and warns about `mstsc.exe`
privileges that mean nothing on one machine), and no virtual-channel lines are emitted.

### ICE / DTLS (bcr_client)

| Variable | Default | Effect |
|---|---|---|
| `BCR_STUN_URLS` | `stun:stun.cloudflare.com:3478` | Comma-separated STUN list. Kept to one responsive server — unreachable STUN servers cost gather latency and contribute nothing. |
| `BCR_GATHER_WAIT_MS` | `1200` | Ceiling on how long `Init()` blocks waiting for the first server-reflexive candidate. |
| `BCR_GATHER_MODE` | *(unset)* | `complete` restores the old "wait for gathering to finish" behaviour, for A/B testing. |
| `BCR_DTLS_STALE_FILTER` | *(on)* | `off` makes the stale/foreign DTLS flight filter report-only instead of dropping. |
| `BCR_TURN_URL`, `BCR_TURN_USERNAME`, `BCR_TURN_CREDENTIAL` | *(unset)* | Optional TURN relay. |

### Diagnostics (bcr_client)

All off by default. A healthy session logs a few dozen lines; turn these on only
for the layer you are actually debugging, one at a time.

| Variable | Turns on |
|---|---|
| `BCR_ICE_DEBUG=1` | pion/ice at Debug — every candidate pair, every connectivity check, every TURN allocation step. Use when ICE never reaches `Connected`. |
| `BCR_DTLS_DEBUG=1` | pion/dtls at Trace — per-flight handshake messages and the reason a certificate was rejected. Use when the handshake fails or you see a `bad_certificate` alert. |
| `BCR_SDP_DUMP=1` | The full munged offer/answer written to `bcr_client.log` (~200 lines per negotiation), for pasting into `chrome://webrtc-internals` or diffing against the cpconv body. |

Our own `[DTLS-TRACE]` record decoding always runs during the handshake and stops
once it completes — after that every record is encrypted and shows nothing but an
epoch and a length. DTLS **alerts** are always logged, in both directions.

### VDI only

| Variable | Effect |
|---|---|
| `BCR_SESSION_ID` | Overrides the WTS session used to open the virtual channel. Ignored unless the DVC path runs. |

---

## Going back to VDI

1. **Set the transport.** `BCR_TRANSPORT=vdi` in the virtual desktop. This is required — the
   default is `local`, which will dial `127.0.0.1:8081` inside the virtual desktop and fail.
2. **Deploy the plugin on the thin client.** It is not rebuilt by `go build`:
   ```powershell
   cd bcr_client\dvc_plugin
   g++ -shared -static -o bcr_dvc_plugin.dll dvc_plugin.cpp -lwtsapi32 -lws2_32 -lole32 -luuid
   ```
   Register it under `HKCU\...\Terminal Server Client\Default\AddIns\bcr_dvc_plugin` pointing at
   the DLL, then restart `mstsc.exe`. See `README.md` for the exact key.
3. **Start bcr_client on the thin client** — it still just listens on `8081`; the plugin dials in.
4. **Verify:** `bcr_host.log` shows `✅ transport active: vdi — RDP Dynamic Virtual Channel
   BCR_VC`. If it logs a DVC open failure instead, the usual causes are `mstsc.exe` privilege
   level or the AddIns registration.

Nothing else changes. No files are restored, no commits are reverted.

---

## What local mode cannot tell you

Local passes are necessary but **not sufficient**. Several problems only exist in the VDI:

- **ICE/NAT behaviour.** On one machine ICE resolves over the local interfaces. The thin
  client's NAT, its blocked STUN servers, and foreign traffic sharing the ICE buffer — the
  conditions behind the DTLS `bad_certificate` failure and the 4-second gather stall — do not
  exist here.
- **DVC framing.** The virtual-channel reader reassembles a byte stream, skips zero-length
  padding, discards all-zero keep-alives, and strips an 8-byte `CHANNEL_PDU_HEADER`. TCP
  delivers clean framed messages, so fragmentation and partial-frame bugs stay invisible until
  you go back.
- **Latency and bandwidth.** Loopback hides timing bugs that a real RDP link exposes.
- **The cpconv reset.** Teams' call-setup endpoint being reset during the TLS handshake is a
  VDI-environment fault (rejections arrive in 1–7 ms from a server ~140 ms away, so they are
  injected locally). It cannot be reproduced or fixed here — it is with the environment team.

## Gotchas

- **Do not "simplify" the loopback PeerConnection to `127.0.0.1`.** `internal/loopback.go`
  deliberately uses real NIC host candidates: WebKit silently filters loopback candidates and
  `setRemoteDescription` then hangs forever. Running everything on one machine makes this look
  like an obvious cleanup. It is not.
- **The DTLS certificate is process-global and intentionally cached.** It is generated once in
  `raw_session.go` and reused for every session so the fingerprint munged into the SDP stays
  stable across reconnects and renegotiations. Per-session certificates would change the
  advertised fingerprint mid-call and break peer verification.
- **ICE is bundled-then-trickled, not pure trickle.** `Init()` returns as soon as the first
  server-reflexive candidate is in hand; that candidate ships inline in the offer, and the rest
  arrive as `RTC_SHADOW_ICE_CANDIDATE` messages. End-of-candidates is signalled explicitly by
  Go — the extension must not synthesise it early, or Teams may ignore everything trickled after.
- **Pre-warm has been removed.** The ICE agent is created at call time. Pre-warm never measurably
  helped, leaked an agent and its sockets per call, and its credentials could go stale between
  page load and the call.
