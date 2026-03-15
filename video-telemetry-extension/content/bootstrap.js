(() => {
    const ENABLE_MEDIA_INTERCEPT = true;
    const ENABLE_MEDIA_FORWARD = true;

    // Rate protection for MEDIA_CHUNK forwarding (content -> background).
    const MEDIA_WINDOW_MS = 1000;
    const MAX_MEDIA_EVENTS_PER_WINDOW = 250;

    let _mediaListenerAttached = false;
    let _onWindowMessage = null;

    let _mediaSeq = 0;
    let _windowStartTs = performance.now();
    let _windowCount = 0;
    let _droppedInWindow = 0;

    function _injectPageInterceptor() {
        // Idempotent guard if start() is ever called more than once.
        if (document.getElementById("bcr-page-interceptor")) return;

        const script = document.createElement("script");
        script.id = "bcr-page-interceptor";
        script.src = chrome.runtime.getURL("content/pageInterceptor.js");
        script.onload = () => script.remove();
        (document.head || document.documentElement).appendChild(script);
    }

    function _rotateMediaWindowIfNeeded(now) {
        if (now - _windowStartTs < MEDIA_WINDOW_MS) return;

        if (_droppedInWindow > 0) {
            console.warn(`[BCR] media chunk drops in last window: ${_droppedInWindow}`);
        }

        _windowStartTs = now;
        _windowCount = 0;
        _droppedInWindow = 0;
    }

    function _attachMediaChunkListener() {
        if (_mediaListenerAttached) return;

        _onWindowMessage = (event) => {
            if (event.source !== window) return;

            const data = event.data;
            if (!data || data.type !== "BCR_MEDIA_CHUNK") return;

            const now = performance.now();
            _rotateMediaWindowIfNeeded(now);

            const chunkBuffer = data.chunkBuffer;
            if (!(chunkBuffer instanceof ArrayBuffer)) return;

            const size = chunkBuffer.byteLength;
            const ts = typeof data.ts === "number" ? data.ts : now;
            const trackType = data.trackType ?? "unknown";

            // Keep minimal visibility without flooding.
            if (_windowCount % 50 === 0) {
                console.log("[BCR] media chunk received:", size);
            }

            if (!ENABLE_MEDIA_FORWARD) return;

            if (_windowCount >= MAX_MEDIA_EVENTS_PER_WINDOW) {
                _droppedInWindow++;
                return;
            }

            _windowCount++;

            Emitter.getInstance().emitMediaChunk({
                seq: ++_mediaSeq, // sequence generated in content layer
                size,
                ts,
                trackType,
                chunk: chunkBuffer,
            });
        };

        window.addEventListener("message", _onWindowMessage);
        _mediaListenerAttached = true;
    }

    function _detachMediaChunkListener() {
        if (!_mediaListenerAttached || !_onWindowMessage) return;

        window.removeEventListener("message", _onWindowMessage);
        _onWindowMessage = null;
        _mediaListenerAttached = false;
    }

    function start() {
        console.log("[Bootstrap] BCR Video Telemetry starting...");

        if (ENABLE_MEDIA_INTERCEPT) {
            _attachMediaChunkListener();
            _injectPageInterceptor();
        }

        VideoRegistry.getInstance().init();
        console.log("[Bootstrap] Registry initialized. Tracking active.");
    }

    function stop() {
        console.log("[Bootstrap] Page unloading. Tearing down...");
        _detachMediaChunkListener();
        OverlayRenderer.getInstance().destroyAll();
        VideoRegistry.getInstance().destroy();
    }

    if (
        document.readyState === "complete" ||
        document.readyState === "interactive"
    ) {
        start();
    } else {
        document.addEventListener("DOMContentLoaded", start, {once: true});
    }

    window.addEventListener("pagehide", stop, {once: true});
})();