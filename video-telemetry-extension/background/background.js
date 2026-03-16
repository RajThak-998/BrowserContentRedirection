/**
 * Background Service Worker
 * Transport + Message Routing combined in one file.
 * MV3 classic service workers cannot use importScripts reliably.
 */

// ─── Feature Flags ──────────────────────────────────────────────────────────

const ENABLE_MEDIA_CHUNK_ROUTE = true;
const MEDIA_LOG_EVERY_N = 100;
const MAX_MEDIA_CHUNK_BYTES = 4 * 1024 * 1024; // 4MB safety cap per chunk

// ─── Media Counters ─────────────────────────────────────────────────────────

let _mediaSeen = 0;

// ─── Transport ─────────────────────────────────────────────────────────────

class Transport {
    constructor() {
        this._WS_URL = "ws://localhost:8765/ws?role=extension";
        this._MAX_BACKOFF_MS = 5000;
        this._INITIAL_BACKOFF_MS = 1000;
        this._MAX_QUEUE = 100;
        this._MAX_MEDIA_QUEUE = 50;

        this._socket = null;
        this._retryCount = 0;
        this._retryTimer = null;
        this._intentionalClose = false;

        this._pendingQueue = [];
        this._pendingMediaQueue = [];

        this._encoder = new TextEncoder();
    }

    static getInstance() {
        if (!Transport._instance) {
            Transport._instance = new Transport();
        }
        return Transport._instance;
    }

    connect() {
        if (this._socket && this._socket.readyState === WebSocket.OPEN) {
            console.log("[Transport] Already connected.");
            return;
        }

        if (this._retryTimer) {
            return;
        }

        this._intentionalClose = false;
        this._createSocket();
    }

    disconnect() {
        this._intentionalClose = true;

        if (this._retryTimer) {
            clearTimeout(this._retryTimer);
            this._retryTimer = null;
        }

        if (this._socket) {
            this._socket.close();
            this._socket = null;
        }

        console.log("[Transport] Disconnected intentionally.");
    }

    // Existing JSON telemetry path (unchanged).
    send(event) {
        if (this._socket && this._socket.readyState === WebSocket.OPEN) {
            try {
                this._socket.send(JSON.stringify(event));
            } catch (err) {
                console.warn("[Transport] Send failed:", err);
                this._enqueue(event);
            }
        } else {
            this._enqueue(event);

            if (!this._intentionalClose) {
                this.connect();
            }
        }
    }

    // New binary media path.
    sendMediaChunk(event) {
        if (this._socket && this._socket.readyState === WebSocket.OPEN) {
            try {
                const frame = this._buildMediaFrame(event);
                this._socket.send(frame);
            } catch (err) {
                console.warn("[Transport] Binary media send failed:", err);
                this._enqueueMedia(event);
            }
        } else {
            this._enqueueMedia(event);

            if (!this._intentionalClose) {
                this.connect();
            }
        }
    }

    getStatus() {
        if (!this._socket) return "NO_SOCKET";
        return ["CONNECTING", "OPEN", "CLOSING", "CLOSED"][this._socket.readyState];
    }

    _enqueue(event) {
        if (this._pendingQueue.length >= this._MAX_QUEUE) {
            this._pendingQueue.shift();
        }
        this._pendingQueue.push(event);
    }

    _enqueueMedia(event) {
        if (this._pendingMediaQueue.length >= this._MAX_MEDIA_QUEUE) {
            this._pendingMediaQueue.shift();
        }
        this._pendingMediaQueue.push(event);
    }

    _buildMediaFrame(event) {
        const payload = event.payload;
        const chunkBytes = Uint8Array.from(payload.chunkBytes);

        const header = {
            type: "MEDIA_CHUNK",
            payload: {
                seq: payload.seq,
                size: payload.size,
                ts: payload.ts,
                trackType: payload.trackType,
                mimeType: payload.mimeType,
                codec: payload.codec,
                sourceBufferId: payload.sourceBufferId,
            },
            meta: event.meta ?? {},
        };

        const headerBytes = this._encoder.encode(JSON.stringify(header));

        // Frame layout: [u32 headerLen LE][headerJSON][rawChunkBytes]
        const frame = new Uint8Array(4 + headerBytes.length + chunkBytes.length);
        const dv = new DataView(frame.buffer);
        dv.setUint32(0, headerBytes.length, true);

        frame.set(headerBytes, 4);
        frame.set(chunkBytes, 4 + headerBytes.length);

        return frame.buffer;
    }

    _createSocket() {
        if (this._socket) {
            const state = this._socket.readyState;
            if (state === WebSocket.CONNECTING || state === WebSocket.OPEN) {
                return;
            }
        }

        console.log(`[Transport] Connecting to ${this._WS_URL}...`);

        try {
            this._socket = new WebSocket(this._WS_URL);
        } catch (err) {
            console.error("[Transport] WebSocket construction failed:", err);
            this._scheduleReconnect();
            return;
        }

        this._socket.onopen = () => this._onOpen();
        this._socket.onclose = (event) => this._onClose(event);
        this._socket.onerror = (error) => this._onError(error);
        this._socket.onmessage = (event) => this._onMessage(event);
    }

    _onOpen() {
        console.log("[Transport] Connected.");
        this._retryCount = 0;

        if (this._pendingQueue.length > 0) {
            console.log(`[Transport] Flushing ${this._pendingQueue.length} queued telemetry messages.`);
            const queue = [...this._pendingQueue];
            this._pendingQueue = [];
            queue.forEach((event) => this.send(event));
        }

        if (this._pendingMediaQueue.length > 0) {
            console.log(`[Transport] Flushing ${this._pendingMediaQueue.length} queued media messages.`);
            const mediaQueue = [...this._pendingMediaQueue];
            this._pendingMediaQueue = [];
            mediaQueue.forEach((event) => this.sendMediaChunk(event));
        }
    }

    _onClose(event) {
        const code = event.code;
        console.warn(`[Transport] Connection closed. Code: ${code}`);
        this._socket = null;

        if (this._intentionalClose) {
            return;
        }

        if (code === 1001) {
            console.log("[Transport] Server replaced connection (1001). Not reconnecting — replacement already active.");
            return;
        }

        this._scheduleReconnect();
    }

    _onError(error) {
        console.error("[Transport] WebSocket error:", error);
    }

    _onMessage(event) {
        // Keep logging compact. Incoming side may be text/binary.
        if (typeof event.data === "string") {
            console.log("[Transport] Received text from endpoint:", event.data);
        }
    }

    _scheduleReconnect() {
        if (this._retryTimer) return;

        const backoff = Math.min(
            this._INITIAL_BACKOFF_MS * Math.pow(2, this._retryCount),
            this._MAX_BACKOFF_MS
        );

        console.log(`[Transport] Reconnecting in ${backoff}ms (attempt ${this._retryCount + 1})...`);

        this._retryTimer = setTimeout(() => {
            this._retryTimer = null;
            this._retryCount++;
            this._createSocket();
        }, backoff);
    }
}

Transport._instance = null;

// ─── Startup ───────────────────────────────────────────────────────────────

Transport.getInstance().connect();
console.log("[Background] Service worker started. Transport connecting...");

// ─── Message Listener ──────────────────────────────────────────────────────

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
    if (!sender.tab) {
        console.warn("[Background] Ignoring message from non-tab sender.");
        sendResponse({status: "rejected", reason: "non-tab sender"});
        return false;
    }

    if (!message?.type || !message?.payload) {
        console.warn("[Background] Malformed message received:", message);
        sendResponse({status: "error", reason: "malformed message"});
        return false;
    }

    switch (message.type) {
        case "VIDEO_UPDATE":
        case "VIDEO_ADDED":
        case "VIDEO_REMOVED":
            _handleTelemetry(message, sender, sendResponse);
            break;

        case "MEDIA_CHUNK":
            _handleMediaChunk(message, sender, sendResponse);
            break;

        default:
            console.warn("[Background] Unknown message type:", message.type);
            sendResponse({status: "error", reason: "unknown type"});
    }

    return true;
});

// ─── Handlers ─────────────────────────────────────────────────────────────

function _enrichWithSenderMeta(message, sender) {
    return {
        ...message,
        meta: {
            tabId: sender.tab.id,
            tabUrl: sender.tab.url,
            frameId: sender.frameId ?? 0,
        },
    };
}

function _handleTelemetry(message, sender, sendResponse) {
    const enrichedMessage = _enrichWithSenderMeta(message, sender);

    try {
        Transport.getInstance().send(enrichedMessage);
        sendResponse({status: "ok"});
    } catch (err) {
        console.error("[Background] Failed to send via transport:", err);
        sendResponse({status: "error", reason: err.message});
    }
}

function _isValidTrackType(trackType) {
    return trackType === "audio" ||
        trackType === "video" ||
        trackType === "text" ||
        trackType === "unknown";
}

function _isValidMediaPayload(payload) {
    if (!payload) return false;
    if (!Number.isFinite(payload.seq) || payload.seq <= 0) return false;
    if (!Number.isFinite(payload.size) || payload.size < 0) return false;
    if (!Number.isFinite(payload.ts) || payload.ts < 0) return false;
    if (!_isValidTrackType(payload.trackType)) return false;

    if (typeof payload.mimeType !== "string") return false;
    if (typeof payload.codec !== "string") return false;
    if (typeof payload.sourceBufferId !== "string") return false;

    if (!Array.isArray(payload.chunkBytes)) return false;
    if (payload.chunkBytes.length !== payload.size) return false;
    if (payload.size > MAX_MEDIA_CHUNK_BYTES) return false;

    return true;
}

function _handleMediaChunk(message, sender, sendResponse) {
    if (!ENABLE_MEDIA_CHUNK_ROUTE) {
        sendResponse({status: "ok", skipped: true});
        return;
    }

    if (!_isValidMediaPayload(message.payload)) {
        sendResponse({status: "error", reason: "invalid MEDIA_CHUNK payload"});
        return;
    }

    const enrichedMessage = _enrichWithSenderMeta(message, sender);

    try {
        Transport.getInstance().sendMediaChunk(enrichedMessage);

        _mediaSeen++;
        if (_mediaSeen % MEDIA_LOG_EVERY_N === 0) {
            console.log(
                `[Background] MEDIA_CHUNK seq=${message.payload.seq} size=${message.payload.size} tabId=${sender.tab.id}`
            );
        }

        sendResponse({status: "ok"});
    } catch (err) {
        console.error("[Background] Failed to route MEDIA_CHUNK:", err);
        sendResponse({status: "error", reason: err.message});
    }
}

// ─── Lifecycle ─────────────────────────────────────────────────────────────

chrome.runtime.onStartup.addListener(() => {
    console.log("[Background] onStartup fired. Reconnecting transport...");
    Transport.getInstance().connect();
});