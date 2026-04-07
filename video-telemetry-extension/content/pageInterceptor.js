/**
 * BCR Page Interceptor — page world (world: MAIN), runs at document_start.
 *
 * Responsibilities:
 *   1. Suppress native decode of the primary video element (largest visible).
 *   2. Forward all MSE media chunks to the isolated-world content script via
 *      window.postMessage (BCR_MEDIA_CHUNK) for relay to the BCR pipeline.
 *
 * Strategy:
 *   - Prototype-level patches on HTMLMediaElement and SourceBuffer so every
 *     instance — present and future — is covered without per-element looping.
 *   - A page-world WeakSet (suppressedVideos) gates every intercepted API.
 *   - Suppressed SourceBuffers drop the native appendBuffer call but fire a
 *     synthetic 'updateend' so the page's streaming loop keeps running and we
 *     keep receiving chunks through our forwarding path.
 *   - Primary video = highest-area visible <video>.  Re-elected on each new
 *     video node.  Only one video is suppressed at a time.
 */
(() => {
    if (window.__BCR_PAGE_INTERCEPTOR_INSTALLED__) return;
    window.__BCR_PAGE_INTERCEPTOR_INSTALLED__ = true;

    // ─── Shared State ─────────────────────────────────────────────────────────
    // Exposed on window so isolated-world scripts can read flags if needed.
    // All WeakSets: no memory leak risk, GC-friendly.
    const state = {
        suppressedVideos:        new WeakSet(),
        suppressedMediaSources:  new WeakSet(),
        suppressedSourceBuffers: new WeakSet(),
        primaryVideo:            null,
    };
    window.__BCR_STATE__ = state;

    // ─── Internal Bypass Flag ─────────────────────────────────────────────────
    // Set to true when OUR code calls patched APIs to prevent re-entrant triggering.
    let _bypass = false;

    // ─── Save Originals Before Any Patch ──────────────────────────────────────
    const origPlay            = HTMLMediaElement.prototype.play;
    const origLoad            = HTMLMediaElement.prototype.load;
    const origAppendBuffer    = SourceBuffer.prototype.appendBuffer;
    const origChangeType      = SourceBuffer.prototype.changeType; // may be undefined
    const origAddSourceBuffer = MediaSource.prototype?.addSourceBuffer;

    // Save RTCPeerConnection method originals for a transparent canary hook.
    const rtcProto = window.RTCPeerConnection?.prototype;
    const origSetLocalDescription = rtcProto?.setLocalDescription;
    const origSetRemoteDescription = rtcProto?.setRemoteDescription;

    // Property descriptors (needed to bypass our own patches internally)
    const srcObjectDesc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'srcObject');
    const srcDesc       = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'src');

    // ─── Blob URL → MediaSource Registry ─────────────────────────────────────
    // YouTube uses: ms = new MediaSource() → video.src = URL.createObjectURL(ms)
    // We need this map to suppress the MS when video.src is set to a blob URL.
    const blobUrlToMediaSource = new Map();

    const origCreateObjectURL = URL.createObjectURL.bind(URL);
    URL.createObjectURL = function patchedCreateObjectURL(obj) {
        const url = origCreateObjectURL(obj);
        if (obj instanceof MediaSource) {
            blobUrlToMediaSource.set(url, obj);
        }
        return url;
    };

    // ─── MediaSource Suppression ───────────────────────────────────────────────
    /**
     * Mark a MediaSource (and all its current SourceBuffers) as suppressed.
     * Does NOT call endOfStream() — we want page JS to keep fetching chunks
     * so our forwarding pipeline continues to receive data.
     */
    function suppressMediaSource(ms) {
        if (!ms || state.suppressedMediaSources.has(ms)) return;
        state.suppressedMediaSources.add(ms);
        try {
            const active = ms.activeSourceBuffers;
            for (let i = 0; i < active.length; i++) {
                state.suppressedSourceBuffers.add(active[i]);
            }
        } catch (_) {}
    }

    // ─── Video Suppression ────────────────────────────────────────────────────
    /**
     * Suppress a video element:
     *   - Add to suppressedVideos so all prototype patches gate on it.
     *   - Pause immediately.
     *   - Suppress any already-attached MediaSource.
     *   - Call origLoad() to flush the GPU decode pipeline (also detaches the
     *     current MediaSource; page JS will re-assign, which our srcObject patch
     *     will catch and mark suppressed).
     */
    function suppressVideo(video) {
        if (state.suppressedVideos.has(video)) return;
        state.suppressedVideos.add(video);
        state.primaryVideo = video;

        video.pause();

        // Suppress any already-attached MediaSource (srcObject path)
        try {
            const existingMs = srcObjectDesc ? srcObjectDesc.get.call(video) : null;
            if (existingMs instanceof MediaSource) {
                suppressMediaSource(existingMs);
            }
        } catch (_) {}

        // Suppress any already-attached MediaSource (blob URL src= path)
        try {
            const currentSrc = srcDesc ? srcDesc.get.call(video) : '';
            if (currentSrc && blobUrlToMediaSource.has(currentSrc)) {
                suppressMediaSource(blobUrlToMediaSource.get(currentSrc));
            }
        } catch (_) {}

        // Flush GPU decode pipeline.  Also causes the browser to detach the
        // MediaSource — page will re-assign, hitting our patched setter.
        _bypass = true;
        try {
            origLoad.call(video);
        } finally {
            _bypass = false;
        }

        // Prevent autoplay and preloading
        video.muted   = true;
        video.preload = 'none';

    }

    // ─── Primary Video Election ────────────────────────────────────────────────
    /**
     * Score a video by visible area.  Off-screen videos get a heavy penalty
     * so they are never elected over a visible one.
     */
    function scoreVideo(video) {
        try {
            const rect = video.getBoundingClientRect();
            const area = rect.width * rect.height;
            if (area < 100) return 0; // ignore tiny / hidden videos
            const inViewport =
                rect.top    < window.innerHeight &&
                rect.bottom > 0 &&
                rect.left   < window.innerWidth  &&
                rect.right  > 0;
            return inViewport ? area : area * 0.05;
        } catch (_) {
            return 0;
        }
    }

    /**
     * Select and suppress the highest-scoring video.
     * Only fires suppressVideo() when the winner changes, so it is safe to
     * call repeatedly (e.g., on each MutationObserver callback).
     */
    function electPrimaryVideo() {
        const videos = document.querySelectorAll('video');
        let best      = null;
        let bestScore = 0;

        for (const v of videos) {
            const score = scoreVideo(v);
            if (score > bestScore) {
                bestScore = score;
                best      = v;
            }
        }

        if (best && best !== state.primaryVideo) {
            suppressVideo(best);
        }
    }

    // ─── Prototype Patches ────────────────────────────────────────────────────

    // RTCPeerConnection canary hook (transparent): log SDP shape, preserve
    // original method behavior and Promise chains exactly.
    if (
        rtcProto &&
        typeof origSetLocalDescription === 'function' &&
        typeof origSetRemoteDescription === 'function' &&
        rtcProto.__BCR_RTC_CANARY_PATCHED__ !== true
    ) {
        Object.defineProperty(rtcProto, '__BCR_RTC_CANARY_PATCHED__', {
            value: true,
            writable: false,
            configurable: true,
            enumerable: false,
        });

        rtcProto.setLocalDescription = function patchedSetLocalDescription(...args) {
            const description = args[0];
            if (
                description &&
                typeof description.type === 'string' &&
                typeof description.sdp === 'string'
            ) {
                console.log('[BCR RTC Canary] localDescription', description.type, description.sdp.length);
            }
            return origSetLocalDescription.apply(this, args);
        };

        rtcProto.setRemoteDescription = function patchedSetRemoteDescription(...args) {
            const description = args[0];
            if (
                description &&
                typeof description.type === 'string' &&
                typeof description.sdp === 'string'
            ) {
                console.log('[BCR RTC Canary] remoteDescription', description.type, description.sdp.length);
            }
            return origSetRemoteDescription.apply(this, args);
        };
    }

    // play() → return resolved Promise for suppressed videos (never reject —
    // many sites chain .then()/.catch() on the return value).
    Object.defineProperty(HTMLMediaElement.prototype, 'play', {
        value: function patchedPlay() {
            if (!_bypass && state.suppressedVideos.has(this)) {
                return Promise.resolve();
            }
            return origPlay.call(this);
        },
        writable:     true,
        configurable: true,
        enumerable:   false,
    });

    // load() → no-op for suppressed unless WE are the ones calling it.
    HTMLMediaElement.prototype.load = function patchedLoad() {
        if (!_bypass && state.suppressedVideos.has(this)) return;
        return origLoad.call(this);
    };

    // srcObject setter → when a suppressed video gets a new MediaSource,
    // mark it suppressed immediately.  We allow the assignment through so
    // sourceopen fires and page JS can create SourceBuffers — which we then
    // also suppress in addSourceBuffer below.
    if (srcObjectDesc) {
        Object.defineProperty(HTMLMediaElement.prototype, 'srcObject', {
            get() {
                return srcObjectDesc.get.call(this);
            },
            set(value) {
                if (!_bypass && state.suppressedVideos.has(this)) {
                    if (value instanceof MediaSource) {
                        // Pre-mark so addSourceBuffer sees it immediately
                        state.suppressedMediaSources.add(value);
                    }
                    // Let assignment through — we gate at the SourceBuffer level
                }
                srcObjectDesc.set.call(this, value);
            },
            configurable: true,
            enumerable:   true,
        });
    }

    // src setter → resolve blob URL to MediaSource and pre-mark it suppressed.
    if (srcDesc) {
        Object.defineProperty(HTMLMediaElement.prototype, 'src', {
            get() {
                return srcDesc.get.call(this);
            },
            set(value) {
                if (!_bypass && state.suppressedVideos.has(this)) {
                    if (value && blobUrlToMediaSource.has(value)) {
                        state.suppressedMediaSources.add(blobUrlToMediaSource.get(value));
                    }
                    // Let assignment through
                }
                srcDesc.set.call(this, value);
            },
            configurable: true,
            enumerable:   true,
        });
    }

    // ─── MSE Telemetry Helpers (shared with suppression gate) ─────────────────
    const sourceBufferMeta  = new WeakMap();
    const sourceBufferIds   = new WeakMap();
    const loggedFormats     = new Set();
    let   sourceBufferSeq   = 0;

    const SCAN_WINDOW_BYTES    = 8192;
    const MOOV_NEAR_START_BYTES = 2048;

    function toByteView(data) {
        if (data instanceof ArrayBuffer)   return new Uint8Array(data);
        if (ArrayBuffer.isView(data))      return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
        return null;
    }

    function getOrCreateSourceBufferId(sb) {
        let id = sourceBufferIds.get(sb);
        if (!id) {
            id = `sb-${++sourceBufferSeq}`;
            sourceBufferIds.set(sb, id);
        }
        return id;
    }

    function extractCodec(mimeType) {
        if (typeof mimeType !== 'string') return 'unknown';
        const m = mimeType.match(/codecs\s*=\s*"?([^";]+)"?/i);
        if (!m || !m[1]) return 'unknown';
        return m[1].split(',')[0]?.trim() || 'unknown';
    }

    function resolveTrackType(mimeType, codec) {
        const mt = (mimeType || '').toLowerCase();
        const c  = (codec    || '').toLowerCase();
        if (mt.startsWith('audio/'))                           return 'audio';
        if (mt.startsWith('video/'))                           return 'video';
        if (mt.includes('text') || mt.includes('vtt'))        return 'text';
        if (/(mp4a|opus|vorbis|flac|aac|ac-3|ec-3)/i.test(c)) return 'audio';
        if (/(avc1|av01|vp8|vp9|hvc1|hev1|theora)/i.test(c))  return 'video';
        return 'unknown';
    }

    function setSourceBufferMeta(sb, mimeType) {
        const mime      = typeof mimeType === 'string' ? mimeType : 'unknown';
        const codec     = extractCodec(mime);
        const trackType = resolveTrackType(mime, codec);
        sourceBufferMeta.set(sb, {
            trackType,
            mimeType:       mime,
            codec,
            sourceBufferId: getOrCreateSourceBufferId(sb),
        });
    }

    // ─── MP4 / WebM Init Segment Detection ────────────────────────────────────

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

        // MP4: ftyp + moov, or moov near start
        const ftypPos = findAscii(u8, 'ftyp', scanLimit);
        const moovPos = findAscii(u8, 'moov', scanLimit);
        if (
            (ftypPos !== -1 && moovPos !== -1) ||
            (moovPos !== -1 && moovPos <= MOOV_NEAR_START_BYTES)
        ) return true;

        // WebM: EBML header + Segment marker
        const ebml    = [0x1A, 0x45, 0xDF, 0xA3];
        const segment = [0x18, 0x53, 0x80, 0x67];
        const hasEbml =
            u8.length >= 4 &&
            u8[0] === ebml[0] && u8[1] === ebml[1] &&
            u8[2] === ebml[2] && u8[3] === ebml[3];

        return hasEbml && findBytes(u8, segment, scanLimit) !== -1;
    }

    // ─── addSourceBuffer Patch ────────────────────────────────────────────────
    // If the owning MediaSource is suppressed, the new SourceBuffer is too.
    function patchAddSourceBuffer(ctorName) {
        const Ctor = window[ctorName];
        if (!Ctor?.prototype?.addSourceBuffer) return;

        const origAdd = Ctor.prototype.addSourceBuffer;
        Ctor.prototype.addSourceBuffer = function patchedAddSourceBuffer(mimeType) {
            const sb = origAdd.call(this, mimeType);
            try {
                setSourceBufferMeta(sb, mimeType);
                if (state.suppressedMediaSources.has(this)) {
                    state.suppressedSourceBuffers.add(sb);
                }
            } catch (_) {}
            return sb;
        };
    }

    patchAddSourceBuffer('MediaSource');
    patchAddSourceBuffer('ManagedMediaSource');

    // ─── changeType Patch (preserve existing behaviour) ──────────────────────
    if (typeof origChangeType === 'function') {
        SourceBuffer.prototype.changeType = function patchedChangeType(mimeType) {
            const result = origChangeType.call(this, mimeType);
            try { setSourceBufferMeta(this, mimeType); } catch (_) {}
            return result;
        };
    }

    // ─── appendBuffer Patch — Core Gate + Telemetry Forwarding ───────────────
    /**
     * For suppressed SourceBuffers:
     *   - Forward the chunk to isolated-world via postMessage (BCR pipeline).
     *   - DO NOT call the native appendBuffer → decoder never sees the data.
     *   - Dispatch synthetic 'updateend' so the page's streaming loop unblocks
     *     and keeps fetching.  Without this the stream would stall after the
     *     first chunk because the page waits for 'updateend' before enqueuing
     *     the next append.
     *
     * For non-suppressed SourceBuffers:
     *   - Forward telemetry as before.
     *   - Call original appendBuffer normally.
     */
    SourceBuffer.prototype.appendBuffer = function patchedAppendBuffer(data) {
        const isSuppressed = state.suppressedSourceBuffers.has(this);

        try {
            const view = toByteView(data);
            if (view) {
                const copied = view.slice(); // fresh copy — original 'data' stays intact

                const meta = sourceBufferMeta.get(this) ?? {
                    trackType:      'unknown',
                    mimeType:       'unknown',
                    codec:          'unknown',
                    sourceBufferId: getOrCreateSourceBufferId(this),
                };

                // Log format once per unique (trackType, mimeType, codec) triplet
                const formatKey = `${meta.trackType}|${meta.mimeType}|${meta.codec}`;
                if (!loggedFormats.has(formatKey)) {
                    loggedFormats.add(formatKey);
                }

                const isInitSegment = detectInitSegment(copied);

                // Always forward telemetry — BCR WebRTC pipeline needs every chunk.
                // copied.buffer is transferred (zero-copy) to the content script.
                window.postMessage(
                    {
                        type:           'BCR_MEDIA_CHUNK',
                        size:           copied.byteLength,
                        ts:             performance.now(),
                        trackType:      meta.trackType,
                        mimeType:       meta.mimeType,
                        codec:          meta.codec,
                        sourceBufferId: meta.sourceBufferId,
                        isInitSegment,
                        chunkBuffer:    copied.buffer,
                    },
                    '*',
                    [copied.buffer]  // transfer ownership to content script
                );
            }
        } catch (_) {}

        if (isSuppressed) {
            // Gate the native call — decoder never receives this data.
            // Fire synthetic updateend so page.js streaming loop continues.
            const sb = this;
            queueMicrotask(() => {
                try {
                    sb.dispatchEvent(new Event('update'));
                    sb.dispatchEvent(new Event('updateend'));
                } catch (_) {}
            });
            return; // intentional: no origAppendBuffer call
        }

        return origAppendBuffer.call(this, data);
    };

    // ─── MutationObserver — Dynamic <video> Detection ─────────────────────────
    const domObserver = new MutationObserver((mutations) => {
        let hasNewVideo = false;
        for (const mutation of mutations) {
            for (const node of mutation.addedNodes) {
                if (node.nodeType !== Node.ELEMENT_NODE) continue;
                if (node.tagName === 'VIDEO' || node.querySelector?.('video')) {
                    hasNewVideo = true;
                    break;
                }
            }
            if (hasNewVideo) break;
        }
        if (hasNewVideo) electPrimaryVideo();
    });

    function startObserving() {
        // Observe the highest available root so we catch even early insertions.
        const target = document.body || document.documentElement;
        domObserver.observe(target, { childList: true, subtree: true });
        electPrimaryVideo();
    }

    // document_start: body may not exist yet.  Start immediately if possible,
    // and re-run after DOMContentLoaded to catch videos added by early scripts.
    if (document.documentElement) {
        domObserver.observe(document.documentElement, { childList: true, subtree: true });
    }

    if (document.readyState === 'complete' || document.readyState === 'interactive') {
        startObserving();
    } else {
        document.addEventListener('DOMContentLoaded', startObserving, { once: true });
    }

})();