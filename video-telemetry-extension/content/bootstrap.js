(() => {
    // pageInterceptor.js runs in the page's JS world (world: MAIN) via the
    // manifest — no script-tag injection needed here.
    // This isolated-world bootstrap only needs to:
    //   1. Listen for BCR_MEDIA_CHUNK postMessages from page world.
    //   2. Forward them to the Emitter (background service worker).

    const ENABLE_MEDIA_FORWARD = true;

    const MEDIA_WINDOW_MS = 1000;
    const MAX_MEDIA_EVENTS_PER_WINDOW = 2500;

    let _mediaListenerAttached = false;
    let _onWindowMessage = null;
    let _runtimeListenerAttached = false;
    let _onRuntimeMessage = null;

    let _videoSeq = 0;
    let _audioSeq = 0;
    let _otherSeq = 0;
    let _windowStartTs = performance.now();
    let _windowCount = 0;
    let _droppedInWindow = 0;


    function _rotateMediaWindowIfNeeded(now) {
        if (now - _windowStartTs < MEDIA_WINDOW_MS) return;

        _windowStartTs = now;
        _windowCount = 0;
        _droppedInWindow = 0;
    }

    function _attachMediaChunkListener() {
        if (_mediaListenerAttached) return;

        _onWindowMessage = (event) => {
            if (event.source !== window) return;

            const data = event.data;
            if (!data || typeof data.type !== "string") return;

            if (data.type === "BCR_RTC_SHADOW_REMOTE") {
                Emitter.getInstance().emitRtcShadowRemote(data.payload ?? {});
                return;
            }

            if (data.type === "BCR_RTC_SHADOW_LOCAL") {
                Emitter.getInstance().emitRtcShadowLocal(data.payload ?? {});
                return;
            }

            if (data.type === "BCR_RTC_SHADOW_ICE_CANDIDATE") {
                Emitter.getInstance().emitRtcShadowIceCandidate(data.payload ?? {});
                return;
            }

            if (data.type === "BCR_RTC_SHADOW_CLOSE") {
                Emitter.getInstance().emitRtcShadowClose(data.payload ?? {});
                return;
            }

            if (data.type !== "BCR_MEDIA_CHUNK") return;

            const now = performance.now();
            _rotateMediaWindowIfNeeded(now);

            const chunkBuffer = data.chunkBuffer;
            if (!(chunkBuffer instanceof ArrayBuffer)) return;

            const size = chunkBuffer.byteLength;
            const ts = typeof data.ts === "number" ? data.ts : now;
            const trackType = data.trackType ?? "unknown";
            const mimeType = data.mimeType ?? "unknown";
            const codec = data.codec ?? "unknown";
            const sourceBufferId = data.sourceBufferId ?? "unknown";
            const isInitSegment = data.isInitSegment === true;

            if (!ENABLE_MEDIA_FORWARD) return;

            if (_windowCount >= MAX_MEDIA_EVENTS_PER_WINDOW) {
                _droppedInWindow++;
                return;
            }

            _windowCount++;

            // Per-track sequence counters to avoid interleaved gaps.
            let seqNum;
            if (trackType === "video") {
                seqNum = ++_videoSeq;
            } else if (trackType === "audio") {
                seqNum = ++_audioSeq;
            } else {
                seqNum = ++_otherSeq;
            }

            Emitter.getInstance().emitMediaChunk({
                seq: seqNum,
                size,
                ts,
                trackType,
                mimeType,
                codec,
                sourceBufferId,
                isInitSegment,
                chunk: chunkBuffer,
            });
        };

        window.addEventListener("message", _onWindowMessage);
        _mediaListenerAttached = true;
    }

    function _attachRuntimeListener() {
        if (_runtimeListenerAttached) return;

        _onRuntimeMessage = (message) => {
            if (!message || typeof message.type !== "string") return;

            if (message.type === "RTC_SHADOW_READY") {
                window.postMessage({
                    type: "BCR_RTC_SHADOW_READY",
                    payload: message.payload ?? {},
                }, "*");
                return;
            }

            if (message.type === "RTC_SHADOW_ERROR") {
                window.postMessage({
                    type: "BCR_RTC_SHADOW_ERROR",
                    payload: message.payload ?? {},
                }, "*");
            }
        };

        chrome.runtime.onMessage.addListener(_onRuntimeMessage);
        _runtimeListenerAttached = true;
    }

    function _detachMediaChunkListener() {
        if (!_mediaListenerAttached || !_onWindowMessage) return;
        window.removeEventListener("message", _onWindowMessage);
        _onWindowMessage = null;
        _mediaListenerAttached = false;
    }

    function _detachRuntimeListener() {
        if (!_runtimeListenerAttached || !_onRuntimeMessage) return;
        chrome.runtime.onMessage.removeListener(_onRuntimeMessage);
        _onRuntimeMessage = null;
        _runtimeListenerAttached = false;
    }

    function start() {
        VideoRegistry.getInstance().init();
    }

    function stop() {
        _detachMediaChunkListener();
        _detachRuntimeListener();
        OverlayRenderer.getInstance().destroyAll();
        VideoRegistry.getInstance().destroy();
    }

    // pageInterceptor.js (world: MAIN) is already active via the manifest.
    // We only need to attach the postMessage listener on the isolated-world side.
    _attachMediaChunkListener();
    _attachRuntimeListener();

    if (document.readyState === "complete" || document.readyState === "interactive") {
        start();
    } else {
        document.addEventListener("DOMContentLoaded", start, {once: true});
    }

    window.addEventListener("pagehide", stop, {once: true});
})();