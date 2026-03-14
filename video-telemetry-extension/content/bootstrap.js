(() => {
    const ENABLE_MEDIA_INTERCEPT = true;
    let _mediaListenerAttached = false;

    function _injectPageInterceptor() {
        // Idempotent guard if start() is ever called more than once.
        if (document.getElementById("bcr-page-interceptor")) return;

        const script = document.createElement("script");
        script.id = "bcr-page-interceptor";
        script.src = chrome.runtime.getURL("content/pageInterceptor.js");
        script.onload = () => script.remove();
        (document.head || document.documentElement).appendChild(script);
    }

    function _attachMediaChunkListener() {
        if (_mediaListenerAttached) return;
        _mediaListenerAttached = true;

        window.addEventListener("message", (event) => {
            if (event.source !== window) return;

            const data = event.data;
            if (!data || data.type !== "BCR_MEDIA_CHUNK") return;

            console.log("[BCR] media chunk received:", data.size);
        });
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