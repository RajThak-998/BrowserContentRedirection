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
        this._send({
            type: "VIDEO_UPDATE",
            payload: {
                id:           videoId,
                timestamp:    Date.now(),
                bounds:       stateWithDelta.bounds,
                screenBounds: stateWithDelta.screenBounds,
                clip:         stateWithDelta.clip,
                onScreen:     stateWithDelta.onScreen,
                visibility:   stateWithDelta.visibility,
                playback:     stateWithDelta.playback,
                fullscreen:   stateWithDelta.fullscreen,
                delta:        stateWithDelta.delta,
            },
        });
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

    emitMediaChunk(payload) {
        const chunkView = new Uint8Array(payload.chunk);

        this._send({
            type: "MEDIA_CHUNK",
            payload: {
                seq:            payload.seq,
                size:           payload.size,
                ts:             payload.ts,
                trackType:      payload.trackType ?? "unknown",
                mimeType:       payload.mimeType ?? "unknown",
                codec:          payload.codec ?? "unknown",
                sourceBufferId: payload.sourceBufferId ?? "unknown",
                videoId:        payload.videoId ?? "unknown",
                isInitSegment:  payload.isInitSegment === true,
                chunkBytes:     Array.from(chunkView),
            },
        });
    }

    emitRtcShadowRemote(payload) {
        this._sendWithRetry(
            {
                type: "RTC_SHADOW_REMOTE",
                payload: {
                    bridgeId: payload.bridgeId,
                    sdpType: payload.sdpType ?? "unknown",
                    sdp: payload.sdp ?? "",
                    // Forward authenticated TURN credentials alongside the SDP so
                    // the Go shadow PC can use them before its first ICE gather.
                    iceServers: payload.iceServers ?? [],
                    timestamp: payload.timestamp ?? Date.now(),
                },
            },
            this._MAX_RETRIES
        );
    }

    emitRtcShadowLocal(payload) {
        this._sendWithRetry(
            {
                type: "RTC_SHADOW_LOCAL",
                payload: {
                    bridgeId: payload.bridgeId,
                    sdpType: payload.sdpType ?? "unknown",
                    sdp: payload.sdp ?? "",
                    iceServers: payload.iceServers ?? [],
                    timestamp: payload.timestamp ?? Date.now(),
                },
            },
            this._MAX_RETRIES
        );
    }

    emitRtcShadowIceServers(payload) {
        this._sendWithRetry(
            {
                type: "RTC_SHADOW_ICE_SERVERS",
                payload: {
                    bridgeId: payload.bridgeId,
                    iceServers: payload.iceServers ?? [],
                    timestamp: payload.timestamp ?? Date.now(),
                },
            },
            this._MAX_RETRIES
        );
    }

    emitRtcShadowClose(payload) {
        this._sendWithRetry(
            {
                type: "RTC_SHADOW_CLOSE",
                payload: {
                    bridgeId: payload.bridgeId,
                    reason: payload.reason ?? "unknown",
                    timestamp: payload.timestamp ?? Date.now(),
                },
            },
            this._MAX_RETRIES
        );
    }

    emitRtcShadowIceCandidate(payload) {
        this._sendWithRetry(
            {
                type: "RTC_SHADOW_ICE_CANDIDATE",
                payload: {
                    bridgeId: payload.bridgeId,
                    candidate: payload.candidate ?? "",
                    sdpMid: payload.sdpMid ?? "0",
                    timestamp: payload.timestamp ?? Date.now(),
                },
            },
            this._MAX_RETRIES
        );
    }

    // YouTube diagnostic line (virtual-clock / chunk-throughput state) — routed as
    // plain telemetry so it lands in bcr_client.log via the host bridge.
    emitYTDiag(payload) {
        this._send({
            type: "YT_DIAG",
            payload: {
                message: payload.message ?? "",
                timestamp: Date.now(),
            },
        });
    }

    // ─── Internal ─────────────────────────────────────────────────────────────

    _sendWithRetry(message, retriesLeft) {
        if (!this._isContextAlive()) {
            return;
        }

        try {
            chrome.runtime.sendMessage(message, (response) => {
                if (!this._isContextAlive()) return;

                if (chrome.runtime.lastError) {
                    const errMsg = chrome.runtime.lastError.message ?? "";

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
                        console.error("[Emitter] Max retries reached. Dropping:", message.type);
                    }
                }
            });
        } catch (err) {
            if (!err.message?.includes("Extension context invalidated")) {
                console.warn("[Emitter] Unexpected sendMessage exception:", err);
            }
        }
    }

    _send(message) {
        // Bug fix: no undefined retriesLeft usage in _send().
        if (!this._isContextAlive()) {
            return;
        }

        try {
            chrome.runtime.sendMessage(message, (response) => {
                if (!this._isContextAlive()) return;

                if (chrome.runtime.lastError) {
                    const errMsg = chrome.runtime.lastError.message ?? "";

                    if (errMsg.includes("Extension context invalidated")) return;
                    if (errMsg.includes("message port closed")) return;

                    console.warn("[Emitter] sendMessage error:", errMsg);
                    return;
                }

                if (response?.status !== "ok") {
                    console.warn("[Emitter] Background did not acknowledge:", response);
                }
            });
        } catch (err) {
            if (!err.message?.includes("Extension context invalidated")) {
                console.warn("[Emitter] Unexpected sendMessage exception:", err);
            }
        }
    }
}

// Singleton static property
Emitter._instance = null;