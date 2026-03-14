(() => {
    // Prevent double patching if script is injected more than once.
    if (window.__BCR_PAGE_INTERCEPTOR_INSTALLED__) return;
    window.__BCR_PAGE_INTERCEPTOR_INSTALLED__ = true;

    // SourceBuffer may not exist on some pages.
    if (typeof SourceBuffer === "undefined" || !SourceBuffer.prototype?.appendBuffer) {
        console.log("[BCR] pageInterceptor: SourceBuffer unavailable on this page.");
        return;
    }

    const originalAppendBuffer = SourceBuffer.prototype.appendBuffer;

    SourceBuffer.prototype.appendBuffer = function patchedAppendBuffer(data) {
        try {
            const size =
                data?.byteLength ??
                data?.length ??
                0;

            window.postMessage(
                {
                    type: "BCR_MEDIA_CHUNK",
                    size,
                    ts: performance.now(),
                    trackType: "unknown",
                },
                "*"
            );
        } catch (err) {
            // Never block playback path due to telemetry.
            console.warn("[BCR] pageInterceptor error:", err);
        }

        return originalAppendBuffer.call(this, data);
    };

    console.log("[BCR] pageInterceptor installed.");
})();