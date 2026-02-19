/**
 * BCR Telemetry - Local WebSocket Endpoint
 *
 * Phase 1 prototype server.
 * Receives telemetry JSON from the Chrome extension
 * and logs it in a readable format for validation.
 *
 * Run with: node index.js
 * Listens on: ws://localhost:8765
 */

const { WebSocketServer } = require("ws");

const PORT = 8765;

// ─── Server Setup ──────────────────────────────────────────────────────────

const wss = new WebSocketServer({ port: PORT });

console.log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━");
console.log("  BCR Telemetry Server");
console.log(`  Listening on ws://localhost:${PORT}`);
console.log("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━");

// Track connected clients
let clientCount = 0;

wss.on("connection", (socket, request) => {
  clientCount++;
  const clientId = `client-${clientCount}`;
  const origin = request.headers.origin ?? "unknown";

  console.log(`\n[+] Client connected: ${clientId}`);
  console.log(`    Origin: ${origin}`);
  console.log(`    Total clients: ${wss.clients.size}`);

  // ─── Message Handler ──────────────────────────────────────────────────

  socket.on("message", (raw) => {
    try {
      const event = JSON.parse(raw.toString());
      _handleEvent(clientId, event);
    } catch (err) {
      console.error(`[!] Failed to parse message from ${clientId}:`, err.message);
      console.error(`    Raw data: ${raw.toString().slice(0, 200)}`);
    }
  });

  // ─── Connection Lifecycle ─────────────────────────────────────────────

  socket.on("close", (code, reason) => {
    console.log(`\n[-] Client disconnected: ${clientId}`);
    console.log(`    Code: ${code} | Reason: ${reason.toString() || "none"}`);
    console.log(`    Remaining clients: ${wss.clients.size}`);
  });

  socket.on("error", (err) => {
    console.error(`\n[!] Socket error from ${clientId}:`, err.message);
  });
});

wss.on("error", (err) => {
  console.error("\n[!] Server error:", err.message);

  if (err.code === "EADDRINUSE") {
    console.error(`    Port ${PORT} is already in use.`);
    console.error(`    Kill the process using it and restart.`);
    process.exit(1);
  }
});

// ─── Event Handlers ────────────────────────────────────────────────────────

/**
 * Route incoming events by type and log them clearly.
 *
 * @param {string} clientId
 * @param {object} event
 */
function _handleEvent(clientId, event) {
  const { type, payload, meta } = event;

  switch (type) {
    case "VIDEO_ADDED":
      _logAdded(clientId, payload, meta);
      break;

    case "VIDEO_REMOVED":
      _logRemoved(clientId, payload, meta);
      break;

    case "VIDEO_UPDATE":
      _logUpdate(clientId, payload, meta);
      break;

    default:
      console.warn(`[?] Unknown event type from ${clientId}: ${type}`);
  }
}

// ─── Loggers ───────────────────────────────────────────────────────────────

function _logAdded(clientId, payload, meta) {
  console.log("\n┌─ VIDEO_ADDED ─────────────────────────────");
  console.log(`│  Client   : ${clientId}`);
  console.log(`│  Video ID : ${payload.id}`);
  console.log(`│  Tab      : ${meta?.tabUrl ?? "unknown"}`);
  console.log(`│  Time     : ${_formatTime(payload.timestamp)}`);
  console.log("└────────────────────────────────────────────");
}

function _logRemoved(clientId, payload, meta) {
  console.log("\n┌─ VIDEO_REMOVED ────────────────────────────");
  console.log(`│  Client   : ${clientId}`);
  console.log(`│  Video ID : ${payload.id}`);
  console.log(`│  Tab      : ${meta?.tabUrl ?? "unknown"}`);
  console.log(`│  Time     : ${_formatTime(payload.timestamp)}`);
  console.log("└────────────────────────────────────────────");
}

function _logUpdate(clientId, payload, meta) {
  const { id, bounds, visibility, playback, fullscreen, delta, timestamp } = payload;

  console.log("\n┌─ VIDEO_UPDATE ─────────────────────────────");
  console.log(`│  Client     : ${clientId}`);
  console.log(`│  Video ID   : ${id}`);
  console.log(`│  Time       : ${_formatTime(timestamp)}`);
  console.log(`│  Tab        : ${meta?.tabUrl ?? "unknown"}`);
  console.log("│  ── Position ──────────────────────────────");
  console.log(`│  Bounds     : x=${_n(bounds.x)} y=${_n(bounds.y)} w=${_n(bounds.width)} h=${_n(bounds.height)}`);
  console.log(`│  Delta      : dx=${_n(delta.dx)} dy=${_n(delta.dy)} dw=${_n(delta.dw)} dh=${_n(delta.dh)}`);
  console.log("│  ── Visibility ────────────────────────────");
  console.log(`│  In Viewport: ${visibility.inViewport}`);
  console.log(`│  Ratio      : ${(visibility.intersectionRatio * 100).toFixed(1)}%`);
  console.log("│  ── Playback ──────────────────────────────");
  console.log(`│  State      : ${playback.state}`);
  console.log(`│  Time       : ${playback.currentTime.toFixed(2)}s`);
  console.log(`│  Rate       : ${playback.rate}x`);
  console.log("│  ── Fullscreen ────────────────────────────");
  console.log(`│  Active     : ${fullscreen}`);
  console.log("└────────────────────────────────────────────");
}

// ─── Utilities ─────────────────────────────────────────────────────────────

/**
 * Format a unix timestamp to readable time string.
 * @param {number} ts
 * @returns {string}
 */
function _formatTime(ts) {
  return new Date(ts).toISOString().replace("T", " ").replace("Z", "");
}

/**
 * Format a number to 2 decimal places for clean log output.
 * @param {number} n
 * @returns {string}
 */
function _n(n) {
  return typeof n === "number" ? n.toFixed(2) : n;
}

// ─── Graceful Shutdown ─────────────────────────────────────────────────────

process.on("SIGINT", () => {
  console.log("\n\n[~] Shutting down server...");
  wss.close(() => {
    console.log("[~] Server closed. Goodbye.");
    process.exit(0);
  });
});