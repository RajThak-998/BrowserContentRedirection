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

    function toByteView(data) {
        if (data instanceof ArrayBuffer) {
            return new Uint8Array(data);
        }

        if (ArrayBuffer.isView(data)) {
            return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
        }

        return null;
    }

    SourceBuffer.prototype.appendBuffer = function patchedAppendBuffer(data) {
        try {
            const view = toByteView(data);

            if (view) {
                // Safe copy so we never risk mutating or detaching the player's original data.
                const copied = view.slice();

                window.postMessage(
                    {
                        type: "BCR_MEDIA_CHUNK",
                        size: copied.byteLength,
                        ts: performance.now(),
                        trackType: "unknown",
                        chunkBuffer: copied.buffer,
                    },
                    "*",
                    [copied.buffer] // transferable for lower overhead page -> content
                );
            }
        } catch (err) {
            // Never block playback path due to telemetry/interception.
            console.warn("[BCR] pageInterceptor error:", err);
        }

        return originalAppendBuffer.call(this, data);
    };

    console.log("[BCR] pageInterceptor installed.");
})();