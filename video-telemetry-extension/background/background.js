/**
 * Background Service Worker
 * Transport + Message Routing combined in one file.
 * MV3 classic service workers cannot use importScripts reliably.
 */

// ─── Transport ─────────────────────────────────────────────────────────────

class Transport {
    constructor() {
        this._WS_URL = "ws://localhost:8765";
        this._MAX_BACKOFF_MS = 5000;
        this._INITIAL_BACKOFF_MS = 100;
        this._MAX_QUEUE = 100;

        this._socket = null;
        this._retryCount = 0;
        this._retryTimer = null;
        this._intentionalClose = false;
        this._pendingQueue = [];
    }

    // ─── Singleton ────────────────────────────────────────────────────────────

    static getInstance() {
        if (!Transport._instance) {
            Transport._instance = new Transport();
        }
        return Transport._instance;
    }

    // ─── Public API ───────────────────────────────────────────────────────────

    connect() {
        if (this._socket && this._socket.readyState === WebSocket.OPEN) {
            console.log("[Transport] Already connected.");
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
                // Attempt immediate reconnect in addition to scheduled backoff.
                // Guards against MV3 service worker timer suspension killing
                // the scheduled retry.
                this.connect();
            }
        }
    }

    getStatus() {
        if (!this._socket) return "NO_SOCKET";
        return ["CONNECTING", "OPEN", "CLOSING", "CLOSED"][this._socket.readyState];
    }

    // ─── Internal ─────────────────────────────────────────────────────────────

    /**
     * Enqueue a message with MAX_QUEUE cap.
     * Oldest message is dropped when cap is reached.
     *
     * @param {object} event
     */
    _enqueue(event) {
        if (this._pendingQueue.length >= this._MAX_QUEUE) {
            // Drop oldest message to make room
            this._pendingQueue.shift();
        }
        this._pendingQueue.push(event);
    }

    _createSocket() {
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
            console.log(`[Transport] Flushing ${this._pendingQueue.length} queued messages.`);
            const queue = [...this._pendingQueue];
            this._pendingQueue = [];
            queue.forEach((event) => this.send(event));
        }
    }

    _onClose(event) {
        console.warn(`[Transport] Connection closed. Code: ${event.code}`);
        if (!this._intentionalClose) {
            this._scheduleReconnect();
        }
    }

    _onError(error) {
        console.error("[Transport] WebSocket error:", error);
    }

    _onMessage(event) {
        console.log("[Transport] Received from endpoint:", event.data);
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

// Singleton static property
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

        default:
            console.warn("[Background] Unknown message type:", message.type);
            sendResponse({status: "error", reason: "unknown type"});
    }

    return true;
});

// ─── Handlers ─────────────────────────────────────────────────────────────

function _handleTelemetry(message, sender, sendResponse) {
    const enrichedMessage = {
        ...message,
        meta: {
            tabId: sender.tab.id,
            tabUrl: sender.tab.url,
            frameId: sender.frameId ?? 0,
        },
    };

    try {
        Transport.getInstance().send(enrichedMessage);
        sendResponse({status: "ok"});
    } catch (err) {
        console.error("[Background] Failed to send via transport:", err);
        sendResponse({status: "error", reason: err.message});
    }
}

// ─── Lifecycle ─────────────────────────────────────────────────────────────

chrome.runtime.onStartup.addListener(() => {
    console.log("[Background] onStartup fired. Reconnecting transport...");
    Transport.getInstance().connect();
});