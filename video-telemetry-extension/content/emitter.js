/**
 * Responsible for sending structured telemetry messages to the background
 * script via chrome.runtime.sendMessage.
 */
class Emitter {
    constructor() {
        this._MAX_RETRIES = 2;
    }

    // ─── Singleton ────────────────────────────────────────────────────────────

    static getInstance() {
        if (!Emitter._instance) {
            Emitter._instance = new Emitter();
        }
        return Emitter._instance;
    }

    // ─── Context Guard ────────────────────────────────────────────────────────

    /**
     * Check if the extension context is still valid.
     * chrome.runtime.id becomes undefined when context is invalidated.
     * This happens during page navigation or extension reload.
     *
     * @returns {boolean}
     */
    _isContextAlive() {
        try {
            return !!chrome.runtime?.id;
        } catch {
            return false;
        }
    }

    // ─── Public API ───────────────────────────────────────────────────────────

    /**
     * Send a VIDEO_UPDATE event to the background script.
     *
     * @param {string} videoId
     * @param {object} stateWithDelta - Output from StateManager.computeDelta
     */
    emitUpdate(videoId, stateWithDelta) {
        const message = {
            type: "VIDEO_UPDATE",
            payload: {
                id: videoId,
                timestamp: Date.now(),
                bounds: stateWithDelta.bounds,
                visibility: stateWithDelta.visibility,
                playback: stateWithDelta.playback,
                fullscreen: stateWithDelta.fullscreen,
                delta: stateWithDelta.delta,
            },
        };

        this._sendWithRetry(message, this._MAX_RETRIES);
    }

    /**
     * Send a VIDEO_ADDED event when a new video is detected.
     *
     * @param {string} videoId
     */
    emitAdded(videoId) {
        this._sendWithRetry(
            {type: "VIDEO_ADDED", payload: {id: videoId, timestamp: Date.now()}},
            this._MAX_RETRIES
        );
    }

    /**
     * Send a VIDEO_REMOVED event when a video leaves the DOM.
     *
     * @param {string} videoId
     */
    emitRemoved(videoId) {
        this._sendWithRetry(
            {type: "VIDEO_REMOVED", payload: {id: videoId, timestamp: Date.now()}},
            this._MAX_RETRIES
        );
    }

    // ─── Internal ─────────────────────────────────────────────────────────────

    /**
     * Internal: Send message to background with simple retry on failure.
     * Silently drops message if extension context is no longer valid.
     *
     * @param {object} message
     * @param {number} retriesLeft
     */
    _sendWithRetry(message, retriesLeft) {
        // ── Guard: stop immediately if context is dead ──
        if (!this._isContextAlive()) {
            // Silent return — this is expected during page navigation
            // No console.warn here — would spam on every navigation
            return;
        }

        try {
            chrome.runtime.sendMessage(message, (response) => {
                // Re-check context inside async callback too
                // Chrome can invalidate between send and callback
                if (!this._isContextAlive()) return;

                if (chrome.runtime.lastError) {
                    const errMsg = chrome.runtime.lastError.message ?? "";

                    // Context invalidated inside callback — silent drop
                    if (errMsg.includes("Extension context invalidated")) return;
                    if (errMsg.includes("message port closed")) return;

                    console.warn("[Emitter] sendMessage error:", errMsg);
                    return;
                }

                if (response?.status !== "ok") {
                    console.warn("[Emitter] Background did not acknowledge:", response);

                    if (retriesLeft > 0) {
                        console.log(`[Emitter] Retrying... (${retriesLeft} left)`);
                        setTimeout(() => this._sendWithRetry(message, retriesLeft - 1), 100);
                    } else {
                        console.error(
                            "[Emitter] Max retries reached. Dropping:",
                            message.type
                        );
                    }
                }
            });
        } catch (err) {
            // Only log if it's an unexpected error
            // "Extension context invalidated" here is expected on navigation
            if (!err.message?.includes("Extension context invalidated")) {
                console.warn("[Emitter] Unexpected sendMessage exception:", err);
            }
        }
    }
}

// Singleton static property
Emitter._instance = null;