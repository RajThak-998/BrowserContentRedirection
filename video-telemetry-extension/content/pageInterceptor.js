(() => {
    if (window.__BCR_PAGE_INTERCEPTOR_INSTALLED__) return;
    window.__BCR_PAGE_INTERCEPTOR_INSTALLED__ = true;

    if (typeof SourceBuffer === "undefined" || !SourceBuffer.prototype?.appendBuffer) {
        return;
    }

    const sourceBufferMeta = new WeakMap();
    const sourceBufferIds = new WeakMap();
    let sourceBufferSeq = 0;

    const originalAppendBuffer = SourceBuffer.prototype.appendBuffer;
    const originalChangeType = SourceBuffer.prototype.changeType;

    function toByteView(data) {
        if (data instanceof ArrayBuffer) return new Uint8Array(data);
        if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
        return null;
    }

    function getOrCreateSourceBufferId(sb) {
        let id = sourceBufferIds.get(sb);
        if (!id) {
            sourceBufferSeq += 1;
            id = `sb-${sourceBufferSeq}`;
            sourceBufferIds.set(sb, id);
        }
        return id;
    }

    function extractCodec(mimeType) {
        if (typeof mimeType !== "string") return "unknown";
        const m = mimeType.match(/codecs\s*=\s*"?([^";]+)"?/i);
        if (!m || !m[1]) return "unknown";
        const first = m[1].split(",")[0]?.trim();
        return first || "unknown";
    }

    function resolveTrackType(mimeType, codec) {
        const mt = (mimeType || "").toLowerCase();
        const c = (codec || "").toLowerCase();

        if (mt.startsWith("audio/")) return "audio";
        if (mt.startsWith("video/")) return "video";
        if (mt.includes("text") || mt.includes("vtt")) return "text";

        if (/(mp4a|opus|vorbis|flac|aac|ac-3|ec-3)/i.test(c)) return "audio";
        if (/(avc1|av01|vp8|vp9|hvc1|hev1|theora)/i.test(c)) return "video";

        return "unknown";
    }

    function setSourceBufferMeta(sb, mimeType) {
        const normalizedMime = typeof mimeType === "string" ? mimeType : "unknown";
        const codec = extractCodec(normalizedMime);
        const trackType = resolveTrackType(normalizedMime, codec);

        sourceBufferMeta.set(sb, {
            trackType,
            mimeType: normalizedMime,
            codec,
            sourceBufferId: getOrCreateSourceBufferId(sb),
        });
    }

    function patchAddSourceBuffer(ctorName) {
        const Ctor = window[ctorName];
        if (!Ctor?.prototype?.addSourceBuffer) return;

        const origAdd = Ctor.prototype.addSourceBuffer;
        Ctor.prototype.addSourceBuffer = function patchedAddSourceBuffer(mimeType) {
            const sb = origAdd.call(this, mimeType);
            try {
                setSourceBufferMeta(sb, mimeType);
            } catch (_) {}
            return sb;
        };
    }

    patchAddSourceBuffer("MediaSource");
    patchAddSourceBuffer("ManagedMediaSource");

    if (typeof originalChangeType === "function") {
        SourceBuffer.prototype.changeType = function patchedChangeType(mimeType) {
            const result = originalChangeType.call(this, mimeType);
            try {
                setSourceBufferMeta(this, mimeType);
            } catch (_) {}
            return result;
        };
    }

    SourceBuffer.prototype.appendBuffer = function patchedAppendBuffer(data) {
        try {
            const view = toByteView(data);
            if (view) {
                const copied = view.slice();

                const meta = sourceBufferMeta.get(this) ?? {
                    trackType: "unknown",
                    mimeType: "unknown",
                    codec: "unknown",
                    sourceBufferId: getOrCreateSourceBufferId(this),
                };

                window.postMessage(
                    {
                        type: "BCR_MEDIA_CHUNK",
                        size: copied.byteLength,
                        ts: performance.now(),
                        trackType: meta.trackType,
                        mimeType: meta.mimeType,
                        codec: meta.codec,
                        sourceBufferId: meta.sourceBufferId,
                        chunkBuffer: copied.buffer,
                    },
                    "*",
                    [copied.buffer]
                );
            }
        } catch (_) {}

        return originalAppendBuffer.call(this, data);
    };
})();