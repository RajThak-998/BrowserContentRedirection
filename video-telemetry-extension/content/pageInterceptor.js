(() => {
    if (window.__BCR_PAGE_INTERCEPTOR_INSTALLED__) return;
    window.__BCR_PAGE_INTERCEPTOR_INSTALLED__ = true;

    if (typeof SourceBuffer === "undefined" || !SourceBuffer.prototype?.appendBuffer) {
        return;
    }

    const sourceBufferMeta = new WeakMap();
    const sourceBufferIds = new WeakMap();
    const initSeenBySourceBuffer = new WeakMap();
    const loggedFormats = new Set();
    let sourceBufferSeq = 0;

    const SCAN_WINDOW_BYTES = 8192;
    const MOOV_NEAR_START_BYTES = 2048;

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

        // Representation/type changes may introduce a fresh init segment.
        initSeenBySourceBuffer.set(sb, false);
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

    function findAscii(u8, ascii, limit) {
        const max = Math.min(limit, u8.length);
        if (ascii.length === 0 || max < ascii.length) return -1;

        outer:
        for (let i = 0; i <= max - ascii.length; i++) {
            for (let j = 0; j < ascii.length; j++) {
                if (u8[i + j] !== ascii.charCodeAt(j)) continue outer;
            }
            return i;
        }
        return -1;
    }

    function findBytes(u8, needle, limit) {
        const max = Math.min(limit, u8.length);
        if (needle.length === 0 || max < needle.length) return -1;

        outer:
        for (let i = 0; i <= max - needle.length; i++) {
            for (let j = 0; j < needle.length; j++) {
                if (u8[i + j] !== needle[j]) continue outer;
            }
            return i;
        }
        return -1;
    }

    function detectInitSegment(u8) {
        const scanLimit = Math.min(SCAN_WINDOW_BYTES, u8.length);

        // MP4: init likely contains ftyp + moov; fallback to moov near start.
        const ftypPos = findAscii(u8, "ftyp", scanLimit);
        const moovPos = findAscii(u8, "moov", scanLimit);
        const isMp4Init =
            (ftypPos !== -1 && moovPos !== -1) ||
            (moovPos !== -1 && moovPos <= MOOV_NEAR_START_BYTES);

        if (isMp4Init) return true;

        // WebM: EBML header at start + Segment marker in scan window.
        const ebml = [0x1A, 0x45, 0xDF, 0xA3];
        const segment = [0x18, 0x53, 0x80, 0x67];

        const hasEbmlAtStart =
            u8.length >= 4 &&
            u8[0] === ebml[0] &&
            u8[1] === ebml[1] &&
            u8[2] === ebml[2] &&
            u8[3] === ebml[3];

        const segmentPos = findBytes(u8, segment, scanLimit);
        if (hasEbmlAtStart && segmentPos !== -1) return true;

        return false;
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

                const formatKey = `${meta.trackType}|${meta.mimeType}|${meta.codec}`;
                if (!loggedFormats.has(formatKey)) {
                    loggedFormats.add(formatKey);
                    console.log(`[Format] track=${meta.trackType} mime=${meta.mimeType} codec=${meta.codec}`);
                }

                let isInitSegment = false;
                const initAlreadySeen = initSeenBySourceBuffer.get(this) === true;

                if (!initAlreadySeen) {
                    isInitSegment = detectInitSegment(copied);
                    if (isInitSegment) {
                        initSeenBySourceBuffer.set(this, true);
                    }
                }

                window.postMessage(
                    {
                        type: "BCR_MEDIA_CHUNK",
                        size: copied.byteLength,
                        ts: performance.now(),
                        trackType: meta.trackType,
                        mimeType: meta.mimeType,
                        codec: meta.codec,
                        sourceBufferId: meta.sourceBufferId,
                        isInitSegment,
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