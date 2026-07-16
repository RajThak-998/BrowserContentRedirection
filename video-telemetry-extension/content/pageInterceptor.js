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
 *
 * Changelog:
 * 2026-06-26: Implementing fixes for unreliable calls and connection resets:
 *   - Fix 1: Restructured shadow-wait to run in background, unblocking createOffer/createAnswer.
 *   - Fix 2: Added HEAD-request TCP keepalive to SFU signaling endpoint during Go shadow-ready wait.
 *   - Fix 3: Added ICE candidate starvation detection, IPv6 filtering fallback, and detailed candidate logging.
 *   - Fix 4: Enabled structured fallback logging for setRemoteDescription failures to aid diagnostics.
 *   - Fix 5: Cleared stale session state and credentials on close to prevent leaking between successive calls.
 */


(() => {
    if (window.__BCR_PAGE_INTERCEPTOR_INSTALLED__) return;
    window.__BCR_PAGE_INTERCEPTOR_INSTALLED__ = true;

    function BCR_LOG(...args) {
        console.log('[BCR-INTERNAL]', ...args);
    }

    try {
        window.BCR_LOG = BCR_LOG;
    } catch (_) {
        // Ignore failures in locked-down contexts.
    }

    // ─── Silent Audio Keepalive (Anti-Throttle) ────────────────────────────────
    // Chrome aggressively throttles background tabs (setTimeout → 60s, CPU starved).
    // Playing a silent audio tone prevents Chrome from applying "intensive wake up
    // throttling" to this tab, keeping our WebRTC signaling responsive even when
    // Teams is in a background tab or Windows Efficiency Mode is active.
    let _silentAudioCtx = null;
    let _silentAudioStarted = false;

    function startSilentAudioKeepalive() {
        if (_silentAudioStarted) return;
        try {
            const AudioCtx = window.AudioContext || window.webkitAudioContext;
            if (!AudioCtx) return;
            _silentAudioCtx = new AudioCtx();
            const gain = _silentAudioCtx.createGain();
            gain.gain.value = 0; // completely silent
            const osc = _silentAudioCtx.createOscillator();
            osc.frequency.value = 440;
            osc.connect(gain);
            gain.connect(_silentAudioCtx.destination);
            osc.start();
            _silentAudioStarted = true;
            BCR_LOG('[BCR] Silent audio keepalive started — tab will resist background throttling');
        } catch (e) {
            BCR_LOG('[BCR] Silent audio keepalive failed:', e);
        }
    }

    function stopSilentAudioKeepalive() {
        if (_silentAudioCtx) {
            try { _silentAudioCtx.close(); } catch (_) {}
            _silentAudioCtx = null;
            _silentAudioStarted = false;
        }
    }

    // ─── Shared State ─────────────────────────────────────────────────────────
    // Exposed on window so isolated-world scripts can read flags if needed.
    // All WeakSets: no memory leak risk, GC-friendly.
    const state = {
        suppressedVideos: new WeakSet(),
        suppressedMediaSources: new WeakSet(),
        suppressedSourceBuffers: new WeakSet(),
        primaryVideo: null,
    };
    window.__BCR_STATE__ = state;

    // ─── Internal Bypass Flag ─────────────────────────────────────────────────
    // Set to true when OUR code calls patched APIs to prevent re-entrant triggering.
    let _bypass = false;

    // ─── Save Originals Before Any Patch ──────────────────────────────────────
    const origPlay = HTMLMediaElement.prototype.play;
    const origLoad = HTMLMediaElement.prototype.load;
    const origAppendBuffer = SourceBuffer.prototype.appendBuffer;
    const origChangeType = SourceBuffer.prototype.changeType; // may be undefined
    const origAddSourceBuffer = MediaSource.prototype?.addSourceBuffer;
    const origCanPlayType = HTMLMediaElement.prototype.canPlayType;
    const origIsTypeSupported = typeof MediaSource !== 'undefined' ? MediaSource.isTypeSupported : null;

    // Block AV1 codec support to force fallback to VP9/H.264
    function shouldBlockCodec(type) {
        if (typeof type !== 'string') return false;
        const lower = type.toLowerCase();
        return lower.includes('av01') || lower.includes('av1');
    }

    if (origIsTypeSupported) {
        MediaSource.isTypeSupported = function patchedIsTypeSupported(type) {
            if (shouldBlockCodec(type)) {
                BCR_LOG('[CodecBlocker] Blocking AV1 type check in isTypeSupported:', type);
                return false;
            }
            return origIsTypeSupported.call(MediaSource, type);
        };
    }

    HTMLMediaElement.prototype.canPlayType = function patchedCanPlayType(type) {
        if (shouldBlockCodec(type)) {
            BCR_LOG('[CodecBlocker] Blocking AV1 type check in canPlayType:', type);
            return "";
        }
        return origCanPlayType.call(this, type);
    };

    // Save RTCPeerConnection method originals for shadow signaling hooks.
    const rtcProto = window.RTCPeerConnection?.prototype;
    const origSetLocalDescription = rtcProto?.setLocalDescription;
    const origSetRemoteDescription = rtcProto?.setRemoteDescription;
    const origCreateOffer = rtcProto?.createOffer;
    const origCreateAnswer = rtcProto?.createAnswer;
    const origSetConfiguration = rtcProto?.setConfiguration;
    const origRTCPeerConnection = window.RTCPeerConnection;

    // Property descriptors (needed to bypass our own patches internally)
    const srcObjectDesc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'srcObject');
    const srcDesc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'src');

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
        } catch (_) { }
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
        } catch (_) { }

        // Suppress any already-attached MediaSource (blob URL src= path)
        try {
            const currentSrc = srcDesc ? srcDesc.get.call(video) : '';
            if (currentSrc && blobUrlToMediaSource.has(currentSrc)) {
                suppressMediaSource(blobUrlToMediaSource.get(currentSrc));
            }
        } catch (_) { }

        // Flush GPU decode pipeline.  Also causes the browser to detach the
        // MediaSource — page will re-assign, hitting our patched setter.
        _bypass = true;
        try {
            origLoad.call(video);
        } finally {
            _bypass = false;
        }

        // Prevent autoplay and preloading
        video.muted = true;
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
                rect.top < window.innerHeight &&
                rect.bottom > 0 &&
                rect.left < window.innerWidth &&
                rect.right > 0;
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
        let best = null;
        let bestScore = 0;

        for (const v of videos) {
            const score = scoreVideo(v);
            if (score > bestScore) {
                bestScore = score;
                best = v;
            }
        }

        if (best && best !== state.primaryVideo) {
            suppressVideo(best);
        }
    }

    // ─── Prototype Patches ────────────────────────────────────────────────────

    // ── SHADOW_READY await timeout ─────────────────────────────────────────────
    // How long (ms) each intercepted RTC call waits for the Go shadow PC to reply
    // with RTC_SHADOW_READY before failing open and letting the native browser
    // connection take over (split-brain fallback).
    //
    // TEMPORARY DEBUG VALUE: raised from 5 000 ms → 60 000 ms while investigating
    // the Go-side 29-second hang inside createAlignedShadowPC / ICE gathering.
    // This prevents the browser from racing ahead and sending its native offer to
    // Azure before Go has finished, which caused the DTLS fingerprint mismatch.
    //
    // Per-PC state stored in a WeakMap (GC-safe) and a bridgeId→PC lookup Map.
    const rtcStateByPeer = new WeakMap();
    const rtcPcByBridgeId = new Map(); // bridgeId → PC (for late candidate trickle)

    function generateBridgeId() {
        if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
            return crypto.randomUUID();
        }
        return `bcr-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
    }

    function emitShadowEvent(type, payload) {
        if (type === 'BCR_RTC_SHADOW_REMOTE' || type === 'BCR_RTC_SHADOW_LOCAL' || type === 'BCR_RTC_SHADOW_CLOSE') {
            BCR_LOG('[BCR] Dispatching to Host via bridgeId:', payload?.bridgeId, 'event=', type);
        }
        window.postMessage({ type, payload }, '*');
    }

    function captureIceServersFromConnection(pc) {
        const entry = ensureRtcState(pc);
        try {
            if (typeof pc.getConfiguration === 'function') {
                const config = pc.getConfiguration();
                if (config && Array.isArray(config.iceServers) && config.iceServers.length > 0) {
                    entry.iceServers = config.iceServers.map(s => ({
                        urls: Array.isArray(s.urls) ? s.urls : (s.url ? [s.url] : []),
                        username: s.username ?? '',
                        credential: s.credential ?? '',
                    }));
                    BCR_LOG('[BCR] Captured configuration ICE server(s) dynamically via getConfiguration() for bridgeId=', entry.bridgeId, 'count=', entry.iceServers.length);
                }
            }
        } catch (e) {
            BCR_LOG('[BCR] Failed to get RTCPeerConnection configuration:', e);
        }
    }

    function ensureRtcState(pc) {
        let entry = rtcStateByPeer.get(pc);
        if (entry) return entry;

        const bridgeId = generateBridgeId();
        entry = {
            bridgeId,
            closedEmitted: false,
            lastSeen: performance.now(),
        };

        rtcStateByPeer.set(pc, entry);

        try {
            Object.defineProperty(pc, '_bcr_id', {
                value: bridgeId,
                writable: false,
                configurable: true,
                enumerable: false,
            });
        } catch (_) {
            // Best-effort debug hint only.
            pc._bcr_id = bridgeId;
        }

        rtcPcByBridgeId.set(bridgeId, pc);

        return entry;
    }

    // ─── Async Credential Application ────────────────────────────────────────
    // Called when SHADOW_READY arrives (asynchronously after setLocalDescription
    // has already returned). Applies trickle ICE candidates to the PC so the
    // SFU can connect to the shadow transport.
    function _applyCredentialsAndTrickle(pc, entry) {
        if (!entry || !entry.shadowCredentials) return;
        // Trickle candidates (deduplicated by generationId)
        if (Array.isArray(entry.shadowCredentials.candidates)) {
            const genId = entry.shadowCredentials.generatedAt || entry.shadowCredentials.iceUfrag;
            if (entry.lastDispatchedTrickleId !== genId) {
                entry.lastDispatchedTrickleId = genId;
                dispatchShadowTrickleCandidates(pc, entry.shadowCredentials.candidates);
            }
        }
    }

    function buildDescriptionLike(originalDescription, nextSdp) {
        const next = {
            type: originalDescription?.type,
            sdp: nextSdp,
        };

        if (typeof window.RTCSessionDescription === 'function') {
            try {
                return new window.RTCSessionDescription(next);
            } catch (_) {
                return next;
            }
        }

        return next;
    }

    function extractOrderedMids(sdp) {
        if (typeof sdp !== 'string' || sdp.length === 0) {
            return [];
        }

        const mids = [];
        for (const line of sdp.split(/\r?\n/)) {
            if (!line.startsWith('a=mid:')) continue;
            const mid = line.slice('a=mid:'.length).trim();
            if (mid.length > 0) {
                mids.push(mid);
            }
        }
        return mids;
    }

    function rewriteAnswerMidsForOfferInPlace(answerSdp, offerSdp) {
        if (typeof answerSdp !== 'string' || typeof offerSdp !== 'string') {
            return answerSdp;
        }

        const answerMids = extractOrderedMids(answerSdp);
        const offerMids = extractOrderedMids(offerSdp);
        if (answerMids.length === 0 || offerMids.length === 0) {
            return answerSdp;
        }

        const remapCount = Math.min(answerMids.length, offerMids.length);
        if (remapCount === 0) {
            return answerSdp;
        }

        const remap = new Map();
        let changed = false;
        for (let i = 0; i < remapCount; i++) {
            remap.set(answerMids[i], offerMids[i]);
            if (answerMids[i] !== offerMids[i]) {
                changed = true;
            }
        }

        const nl = answerSdp.includes('\r\n') ? '\r\n' : '\n';
        const lines = answerSdp.split(/\r?\n/);
        const out = [];

        for (const line of lines) {
            if (line.startsWith('a=mid:')) {
                const oldMid = line.slice('a=mid:'.length).trim();
                const mapped = remap.get(oldMid);
                if (mapped) {
                    out.push(`a=mid:${mapped}`);
                    if (mapped !== oldMid) {
                        changed = true;
                    }
                    continue;
                }
            }

            if (line.startsWith('a=group:BUNDLE')) {
                // Keep BUNDLE mids constrained to the offered sequence and in order.
                const safeBundleMids = offerMids.slice(0, remapCount);
                const nextLine = `a=group:BUNDLE ${safeBundleMids.join(' ')}`;
                if (nextLine !== line) {
                    changed = true;
                }
                out.push(nextLine);
                continue;
            }

            out.push(line);
        }

        if (!changed) {
            return answerSdp;
        }

        return out.join(nl);
    }

    function isIPv6Candidate(line) {
        const parts = line.trim().split(/\s+/);
        // candidate format: candidate:<foundation> <component> <protocol>
        //                   <priority> <address> <port> typ <type> ...
        // address is at index 4
        if (parts.length < 6) return false;
        const addr = parts[4];
        // IPv6 addresses contain colons; IPv4 do not
        return addr.includes(':');
    }

    function dispatchShadowTrickleCandidates(pc, candidates) {
        let candidateLines = [];
        if (Array.isArray(candidates)) {
            candidateLines = candidates.filter(line => {
                if (typeof line !== 'string') return false;
                const trimmed = line.trim();
                if (trimmed.length === 0) return false;

                // Filter out IPv6 candidates to prevent Teams proprietary parser crashes.
                // Mobile hotspots generate IPv6 candidates, which severely break Teams'
                // JS simulcast logic when trickled.
                if (isIPv6Candidate(trimmed)) {
                    BCR_LOG('[BCR] Dropping IPv6 candidate to avoid Teams crash:', trimmed);
                    return false;
                }
                return true;
            });
        }

        if (candidateLines.length === 0 && Array.isArray(candidates) && candidates.length > 0) {
            BCR_LOG('[BCR] WARNING: ALL', candidates.length,
                'shadow candidates were dropped by the IPv6 filter.',
                'Teams will have no ICE candidates. Check shadow PC network config.',
                'Raw dropped candidates:', candidates);
            // Emit a diagnostic event so the content script / Go side can log it
            emitShadowEvent('BCR_SHADOW_DIAGNOSTIC', {
                type: 'ALL_CANDIDATES_FILTERED',
                droppedCount: candidates.length,
                droppedSample: candidates.slice(0, 3),
                timestamp: Date.now(),
            });
            return; // nothing to trickle
        }

        let mid = '0';
        BCR_LOG('[BCR] Synthetic ICE Trickle queued',
            candidateLines.length, 'IPv4 candidates (dropped',
            (candidates.length - candidateLines.length), 'IPv6)');

        // Dispatch synthetic candidates using queueMicrotask to avoid background tab setTimeout throttling (Efficiency Mode)
        candidateLines.forEach((line) => {
            queueMicrotask(() => {
                try {
                    let candidateStr = line.trim();
                    if (candidateStr.startsWith('a=')) {
                        candidateStr = candidateStr.substring(2); // strictly remove "a="
                    }
                    if (candidateStr.startsWith('candidate:')) {
                        candidateStr = candidateStr.substring(10); // strictly remove "candidate:" if present natively
                    }
                    candidateStr = candidateStr.trim();

                    const candidateDict = {
                        candidate: candidateStr,
                        sdpMLineIndex: 0,
                        sdpMid: mid
                    };
                    const rtcCandidate = (typeof window.RTCIceCandidate === 'function')
                        ? new window.RTCIceCandidate(candidateDict)
                        : candidateDict;

                    let event;
                    if (typeof window.RTCPeerConnectionIceEvent === 'function') {
                        event = new window.RTCPeerConnectionIceEvent('icecandidate', { candidate: rtcCandidate });
                    } else {
                        event = new Event('icecandidate');
                        event.candidate = rtcCandidate;
                    }
                    pc.dispatchEvent(event);
                } catch (e) {
                    BCR_LOG('[BCR] Failed to synthetically dispatch ice candidate:', e);
                }
            });
        });

        // Finally dispatch end-of-candidates
        queueMicrotask(() => {
            try {
                let nullEvent;
                if (typeof window.RTCPeerConnectionIceEvent === 'function') {
                    nullEvent = new window.RTCPeerConnectionIceEvent('icecandidate', { candidate: null });
                } else {
                    nullEvent = new Event('icecandidate');
                    nullEvent.candidate = null;
                }
                pc.dispatchEvent(nullEvent);
                BCR_LOG('[BCR] Synthetic ICE Trickle complete, dispatched', candidateLines.length, 'candidates');
            } catch (e) {
                BCR_LOG('[BCR] Synthetic ICE Trickle end dispatch failed:', e);
            }
        });
    }

    // ── SDP Codec Pinning ──────────────────────────────────────────────────────
    // Preferred codecs for the BCR pipeline. By constraining the SDP to a single
    // video codec (H264) and audio codec (Opus), we prevent the SFU from switching
    // codecs mid-session, which would trigger renegotiation cascades that crash
    // the external player's decoder pipeline.
    const BCR_PREFERRED_VIDEO_CODEC = 'H264';
    const BCR_PREFERRED_AUDIO_CODEC = 'opus';

    /**
     * filterSdpToPreferredCodecs — Strip all codecs from the SDP except the
     * preferred video and audio codecs.
     *
     * For each m=video / m=audio section:
     *   1. Parse all a=rtpmap: lines to find the PT(s) for the preferred codec.
     *   2. Find associated RTX PTs via a=fmtp:<rtx_pt> apt=<base_pt>.
     *   3. Keep only the a=rtpmap:, a=fmtp:, and a=rtcp-fb: lines for kept PTs.
     *   4. Rewrite the m= line's format list to only include kept PTs.
     *
     * @param {string} sdp — The SDP to filter.
     * @param {string} preferVideo — Preferred video codec name (case-insensitive), e.g. 'H264'.
     * @param {string} preferAudio — Preferred audio codec name (case-insensitive), e.g. 'opus'.
     * @returns {string} — The filtered SDP.
     */
    function filterSdpToPreferredCodecs(sdp, preferVideo = BCR_PREFERRED_VIDEO_CODEC, preferAudio = BCR_PREFERRED_AUDIO_CODEC) {
        if (typeof sdp !== 'string' || sdp.length === 0) return sdp;

        const nl = sdp.includes('\r\n') ? '\r\n' : '\n';
        const lines = sdp.split(/\r?\n/);
        const output = [];

        let inMediaSection = false;
        let currentMediaType = ''; // 'audio', 'video', or ''
        let sectionLines = [];
        let mLineIndex = -1;

        function flushSection() {
            if (!inMediaSection || sectionLines.length === 0) return;

            if (currentMediaType !== 'audio' && currentMediaType !== 'video') {
                // Non-audio/video section (e.g. m=application) — pass through unchanged
                output.push(...sectionLines);
                sectionLines = [];
                return;
            }

            const preferCodec = currentMediaType === 'video'
                ? preferVideo.toLowerCase()
                : preferAudio.toLowerCase();

            // Step 1: Parse all rtpmap lines to build PT → codec name map
            const ptToCodec = new Map(); // PT string → codec name (lowercase)
            const rtpmapRegex = /^a=rtpmap:(\d+)\s+([^\s/]+)/;
            for (const line of sectionLines) {
                const m = line.match(rtpmapRegex);
                if (m) {
                    ptToCodec.set(m[1], m[2].toLowerCase());
                }
            }

            // Step 2: Find the base PT(s) for the preferred codec
            const keepPTs = new Set();
            for (const [pt, codec] of ptToCodec) {
                if (codec === preferCodec) {
                    keepPTs.add(pt);
                }
            }

            // If preferred codec not found in this section, pass through unchanged
            // (safety: don't break the SDP if the SFU doesn't offer our preferred codec)
            if (keepPTs.size === 0) {
                BCR_LOG(`[BCR] Codec pin: preferred ${preferCodec} not found in m=${currentMediaType}, passing through unchanged`);
                output.push(...sectionLines);
                sectionLines = [];
                return;
            }

            // Step 3: Find RTX PTs associated with kept base PTs
            // RTX lines look like: a=fmtp:<rtx_pt> apt=<base_pt>
            const aptRegex = /^a=fmtp:(\d+)\s+apt=(\d+)/;
            for (const line of sectionLines) {
                const m = line.match(aptRegex);
                if (m && keepPTs.has(m[2])) {
                    keepPTs.add(m[1]); // Add the RTX PT
                }
            }

            // Step 4: Rewrite the m= line to only include kept PTs
            // m= line format: m=<type> <port> <protocol> <pt1> <pt2> ...
            const filteredLines = [];
            for (let i = 0; i < sectionLines.length; i++) {
                const line = sectionLines[i];

                if (line.startsWith('m=')) {
                    // Rewrite m= line with only the kept PTs
                    const parts = line.split(/\s+/);
                    // parts[0] = m=video/m=audio, parts[1] = port, parts[2] = protocol, rest = PTs
                    if (parts.length >= 4) {
                        const keptFormatList = parts.slice(3).filter(pt => keepPTs.has(pt));
                        if (keptFormatList.length > 0) {
                            filteredLines.push(`${parts[0]} ${parts[1]} ${parts[2]} ${keptFormatList.join(' ')}`);
                        } else {
                            // Fallback: keep original if filtering would remove all PTs
                            filteredLines.push(line);
                        }
                    } else {
                        filteredLines.push(line);
                    }
                    continue;
                }

                // Step 5: Filter codec-specific attribute lines
                const ptMatch = line.match(/^a=(?:rtpmap|fmtp|rtcp-fb):(\d+)\s/);
                if (ptMatch) {
                    if (keepPTs.has(ptMatch[1])) {
                        filteredLines.push(line);
                    }
                    // Drop lines for non-kept PTs
                    continue;
                }

                // Keep all non-codec lines (ice, dtls, mid, direction, ssrc, etc.)
                filteredLines.push(line);
            }

            const keptCodecNames = [...keepPTs].map(pt => ptToCodec.get(pt) ?? 'rtx').join(', ');
            BCR_LOG(`[BCR] Codec pin: m=${currentMediaType} kept PTs=[${[...keepPTs].join(',')}] codecs=[${keptCodecNames}]`);

            output.push(...filteredLines);
            sectionLines = [];
        }

        for (const line of lines) {
            if (line.startsWith('m=')) {
                // Flush previous section before starting a new one
                flushSection();

                inMediaSection = true;
                if (line.startsWith('m=audio')) {
                    currentMediaType = 'audio';
                } else if (line.startsWith('m=video')) {
                    currentMediaType = 'video';
                } else {
                    currentMediaType = '';
                }
                sectionLines.push(line);
                continue;
            }

            if (inMediaSection) {
                sectionLines.push(line);
            } else {
                output.push(line);
            }
        }

        // Flush the last section
        flushSection();

        return output.join(nl);
    }

    function mungeSdpTransport(sdp, shadow) {
        if (typeof sdp !== 'string' || !shadow) return sdp;

        let munged = sdp;

        // ── ICE credentials ────────────────────────────────────────────────────
        if (typeof shadow.iceUfrag === 'string' && shadow.iceUfrag.length > 0) {
            munged = munged.replace(/^a=ice-ufrag:.*$/gm, `a=ice-ufrag:${shadow.iceUfrag}`);
        }
        if (typeof shadow.icePwd === 'string' && shadow.icePwd.length > 0) {
            munged = munged.replace(/^a=ice-pwd:.*$/gm, `a=ice-pwd:${shadow.icePwd}`);
        }

        // ── DTLS fingerprint ───────────────────────────────────────────────────
        if (typeof shadow.dtlsFingerprint === 'string' && shadow.dtlsFingerprint.length > 0) {
            const fullFingerprint = shadow.dtlsFingerprint.trim();
            if (/^[A-Za-z0-9-]+\s+[0-9A-Fa-f:]+$/.test(fullFingerprint)) {
                munged = munged.replace(/^a=fingerprint:.*$/gm, `a=fingerprint:${fullFingerprint}`);
            } else {
                munged = munged.replace(
                    /^a=fingerprint:([A-Za-z0-9-]+)\s+.*$/gm,
                    (_, alg) => `a=fingerprint:${alg} ${fullFingerprint}`
                );
            }
        }

        // ── Connection address ─────────────────────────────────────────────────
        if (typeof shadow.localIp === 'string' && shadow.localIp.length > 0) {
            munged = munged.replace(/^c=IN IP4\s+.*$/gm, `c=IN IP4 ${shadow.localIp}`);
        }

        // ── ICE candidates ─────────────────────────────────────────────────────
        // Replace the browser's own a=candidate lines with the shadow PC's
        // gathered candidates. This points the remote peer at the shadow's
        // actual transport address (bcr_client's local IP + UDP port).
        // a=end-of-candidates is also removed; we re-insert it after injection.
        if (Array.isArray(shadow.candidates) && shadow.candidates.length > 0) {
            // Detect line ending used in this SDP.
            const nl = munged.includes('\r\n') ? '\r\n' : '\n';

            // Strip existing candidate and end-of-candidates lines.
            munged = munged.replace(/^a=candidate:.*$/gm, '');
            munged = munged.replace(/^a=end-of-candidates.*$/gm, '');
            // Clean up any blank lines left behind.
            munged = munged.replace(/(\r?\n){3,}/g, `${nl}${nl}`);

            // Inject shadow candidates after the first a=ice-pwd: line
            // (which marks the start of the media transport block in BUNDLE).
            // We inject once only — in BUNDLE mode a single ICE session serves all m-lines.
            let injected = false;
            const candidateBlock = shadow.candidates.join(nl) + nl + 'a=end-of-candidates';
            munged = munged.replace(/(a=ice-pwd:[^\r\n]+)(\r?\n)/g, (match, pwd, newline) => {
                if (injected) return match;
                injected = true;
                return pwd + newline + candidateBlock + newline;
            });
        }

        return munged;
    }

    function maybeEmitShadowClose(pc, reason) {
        const entry = ensureRtcState(pc);
        if (entry.closedEmitted) return;

        entry.closedEmitted = true;

        entry.shadowCredentials = null;
        entry.lastOriginalLocalSdp = null;
        entry.lastCreateActionType = null;
        entry.lastDispatchedTrickleId = null;
        entry.lastRemoteOfferSdp = null;
        entry.shadowLocalEmitted = false;
        entry._cachedPinnedSdp = null;

        stopSilentAudioKeepalive();

        emitShadowEvent('BCR_RTC_SHADOW_CLOSE', {
            bridgeId: entry.bridgeId,
            reason,
            timestamp: Date.now(),
        });
    }

    function attachPeerLifecycleHooks(pc) {
        ensureRtcState(pc);

        const onStateChange = () => {
            if (pc.connectionState === 'closed' || pc.connectionState === 'failed') {
                maybeEmitShadowClose(pc, `connectionState:${pc.connectionState}`);
                return;
            }

            if (pc.iceConnectionState === 'closed' || pc.iceConnectionState === 'failed') {
                maybeEmitShadowClose(pc, `iceConnectionState:${pc.iceConnectionState}`);
            }
        };

        pc.addEventListener('connectionstatechange', onStateChange);
        pc.addEventListener('iceconnectionstatechange', onStateChange);
    }

    function installPeerConstructorHook() {
        if (typeof origRTCPeerConnection !== 'function') return;
        if (window.RTCPeerConnection?.__BCR_RTC_CTOR_PATCHED__ === true) return;

        const PatchedRTCPeerConnection = function BCRPatchedRTCPeerConnection(...args) {
            const pc = new origRTCPeerConnection(...args);
            attachPeerLifecycleHooks(pc);

            // Capture the ICE server configuration from the VDI app so the shadow PC
            // can use the same STUN/TURN servers for connectivity.
            const config = args[0];
            if (config && Array.isArray(config.iceServers) && config.iceServers.length > 0) {
                const entry = ensureRtcState(pc);
                entry.iceServers = config.iceServers.map(s => ({
                    urls: Array.isArray(s.urls) ? s.urls : (s.url ? [s.url] : []),
                    username: s.username ?? '',
                    credential: s.credential ?? '',
                }));
                BCR_LOG('[BCR] Captured', entry.iceServers.length, 'ICE server(s) for bridgeId=', entry.bridgeId);
            }

            return pc;
        };

        PatchedRTCPeerConnection.prototype = origRTCPeerConnection.prototype;
        Object.setPrototypeOf(PatchedRTCPeerConnection, origRTCPeerConnection);

        Object.defineProperty(PatchedRTCPeerConnection, '__BCR_RTC_CTOR_PATCHED__', {
            value: true,
            writable: false,
            configurable: false,
            enumerable: false,
        });

        window.RTCPeerConnection = PatchedRTCPeerConnection;
    }

    window.addEventListener('message', (event) => {
        if (event.source !== window) return;

        const data = event.data;
        if (!data || typeof data.type !== 'string') return;

        if (data.type === 'BCR_RTC_SHADOW_READY') {
            const payload = data.payload;
            const bridgeId = payload?.bridgeId;
            if (typeof bridgeId === 'string' && bridgeId.length > 0) {
                const pc = rtcPcByBridgeId.get(bridgeId);
                if (!pc) {
                    BCR_LOG('[BCR] SHADOW_READY arrived for unknown/closed bridgeId=', bridgeId, '— ignored');
                    return;
                }
                const existingEntry = rtcStateByPeer.get(pc);
                if (!existingEntry || existingEntry.closedEmitted) {
                    BCR_LOG('[BCR] SHADOW_READY arrived for closed PC bridgeId=', bridgeId, '— ignored');
                    return;
                }
                BCR_LOG('[BCR] Received SHADOW_READY bridgeId=', bridgeId);
                // Eagerly store credentials and apply trickle ICE candidates.
                // setLocalDescription is non-blocking — credentials are applied async.
                existingEntry.shadowCredentials = payload;
                _applyCredentialsAndTrickle(pc, existingEntry);
            }
        }

        if (data.type === 'BCR_RTC_SHADOW_ERROR') {
            const payload = data.payload;
            const bridgeId = payload?.bridgeId;
            if (typeof bridgeId === 'string' && bridgeId.length > 0) {
                BCR_LOG('[BCR] Received SHADOW_ERROR bridgeId=', bridgeId, 'stage=', payload?.stage, 'reason=', payload?.reason);
                // No action needed — setLocalDescription is non-blocking,
                // and the localDescription getter will return original SDP as fallback.
            }
        }

        if (data.type === 'BCR_RTC_SHADOW_ICE_CANDIDATE') {
            const payload = data.payload;
            const bridgeId = payload?.bridgeId;
            const candidateStr = payload?.candidate;
            if (typeof bridgeId === 'string' && typeof candidateStr === 'string' && candidateStr.length > 0) {
                const pc = rtcPcByBridgeId.get(bridgeId);
                if (pc) {
                    BCR_LOG('[BCR] Late ICE candidate received, trickle bridgeId=', bridgeId);
                    dispatchShadowTrickleCandidates(pc, [candidateStr]);
                } else {
                    BCR_LOG('[BCR] Late ICE candidate dropped — no PC for bridgeId=', bridgeId);
                }
            }
        }
    });

    if (
        rtcProto &&
        typeof origSetLocalDescription === 'function' &&
        typeof origSetRemoteDescription === 'function' &&
        typeof origCreateOffer === 'function' &&
        typeof origCreateAnswer === 'function' &&
        rtcProto.__BCR_RTC_SHADOW_PATCHED__ !== true
    ) {
        installPeerConstructorHook();
        BCR_LOG('[BCR] Hooking setRemoteDescription...');
        BCR_LOG('[BCR] Hooking setLocalDescription...');

        Object.defineProperty(rtcProto, '__BCR_RTC_SHADOW_PATCHED__', {
            value: true,
            writable: false,
            configurable: true,
            enumerable: false,
        });

        // ── ICE / Connection State Mocker ────────────────────────────────────
        // Mask native Chrome's 'failed' and 'disconnected' states for managed connections
        // to prevent Teams from terminating the call when the VDI network fails natively.
        const origIceStateGetter = Object.getOwnPropertyDescriptor(rtcProto, 'iceConnectionState');
        const origConnStateGetter = Object.getOwnPropertyDescriptor(rtcProto, 'connectionState');

        function getMockedState(origGetter, pc) {
            if (!origGetter) return undefined;
            const realState = origGetter.call(pc);
            const entry = rtcStateByPeer.get(pc);
            // Only mask if we are actively managing via shadow proxy.
            if (entry && entry.shadowCredentials && (realState === 'failed' || realState === 'disconnected')) {
                return 'connected';
            }
            return realState;
        }

        if (origIceStateGetter && origIceStateGetter.get) {
            Object.defineProperty(rtcProto, 'iceConnectionState', {
                get() { return getMockedState(origIceStateGetter.get, this); },
                configurable: true,
                enumerable: true,
            });
        }

        if (origConnStateGetter && origConnStateGetter.get) {
            Object.defineProperty(rtcProto, 'connectionState', {
                get() { return getMockedState(origConnStateGetter.get, this); },
                configurable: true,
                enumerable: true,
            });
        }

        // ── localDescription / currentLocalDescription getter patches ────────
        // The VDI app reads pc.localDescription to obtain the SDP it sends over
        // signaling. We intercept the getter to return a munged copy containing
        // the shadow PC's credentials and candidates so the remote peer connects
        // to the shadow transport instead of the browser's real ICE agent.
        const origLocalDescGetter = Object.getOwnPropertyDescriptor(rtcProto, 'localDescription');
        const origCurrentLocalDescGetter = Object.getOwnPropertyDescriptor(rtcProto, 'currentLocalDescription');

        function getMungedLocalDesc(origGetter, pc) {
            const desc = origGetter.call(pc);
            if (!desc || !desc.sdp) return desc;
            const entry = rtcStateByPeer.get(pc);
            if (!entry || !entry.shadowCredentials) return desc;
            // Use cached pinned SDP if available to avoid redundant filtering.
            const pinnedSdp = entry._cachedPinnedSdp || filterSdpToPreferredCodecs(desc.sdp);
            const mungedSdp = mungeSdpTransport(pinnedSdp, entry.shadowCredentials);
            return { type: desc.type, sdp: mungedSdp };
        }

        if (origLocalDescGetter && origLocalDescGetter.get) {
            Object.defineProperty(rtcProto, 'localDescription', {
                get() { return getMungedLocalDesc(origLocalDescGetter.get, this); },
                configurable: true,
                enumerable: true,
            });
        }
        if (origCurrentLocalDescGetter && origCurrentLocalDescGetter.get) {
            Object.defineProperty(rtcProto, 'currentLocalDescription', {
                get() { return getMungedLocalDesc(origCurrentLocalDescGetter.get, this); },
                configurable: true,
                enumerable: true,
            });
        }

        if (typeof origSetConfiguration === 'function') {
            BCR_LOG('[BCR] Hooking setConfiguration...');
            rtcProto.setConfiguration = function patchedSetConfiguration(config, ...args) {
                if (config && Array.isArray(config.iceServers) && config.iceServers.length > 0) {
                    const entry = ensureRtcState(this);
                    entry.iceServers = config.iceServers.map(s => ({
                        urls: Array.isArray(s.urls) ? s.urls : (s.url ? [s.url] : []),
                        username: s.username ?? '',
                        credential: s.credential ?? '',
                    }));
                    BCR_LOG('[BCR] Captured dynamic setConfiguration', entry.iceServers.length, 'ICE server(s) for bridgeId=', entry.bridgeId);
                    
                    emitShadowEvent('BCR_RTC_SHADOW_ICE_SERVERS', {
                        bridgeId: entry.bridgeId,
                        iceServers: entry.iceServers,
                        timestamp: Date.now(),
                    });
                }
                return origSetConfiguration.apply(this, [config, ...args]);
            };
        }

        rtcProto.setLocalDescription = async function patchedSetLocalDescription(...args) {
            const description = args[0];
            if (!description || typeof description.sdp !== 'string') {
                return origSetLocalDescription.apply(this, args);
            }

            const entry = ensureRtcState(this);
            entry.lastSeen = performance.now();

            const sdpType = (description.type ?? '').toLowerCase();
            const origSdpString = entry.lastOriginalLocalSdp || description.sdp;

            // Fire-and-forget: emit SHADOW_LOCAL if not already emitted for this sdpType.
            // Unlike the old blocking design, we do NOT await SHADOW_READY here.
            // The localDescription getter handles munging when credentials arrive.
            if (!entry.shadowLocalEmitted || sdpType !== (entry.lastCreateActionType ?? '').toLowerCase()) {
                entry.lastCreateActionType = sdpType;
                entry.lastOriginalLocalSdp = description.sdp;
                entry.shadowLocalEmitted = true;
                const pinnedSdp = filterSdpToPreferredCodecs(description.sdp);
                entry._cachedPinnedSdp = pinnedSdp;

                captureIceServersFromConnection(this);

                emitShadowEvent('BCR_RTC_SHADOW_LOCAL', {
                    bridgeId: entry.bridgeId,
                    sdpType: description.type ?? 'unknown',
                    sdp: pinnedSdp,
                    iceServers: entry.iceServers ?? [],
                    timestamp: Date.now(),
                });

                BCR_LOG('[BCR] Emitted SHADOW_LOCAL (non-blocking) type:', description.type ?? 'unknown', 'bridgeId=', entry.bridgeId);

                // Start silent audio to prevent tab throttling during the call
                startSilentAudioKeepalive();
            }

            // CRITICAL: Pass the ORIGINAL SDP to Chrome's native method.
            // Chrome's ICE agent validates that ice-ufrag/pwd match its internal state.
            const chromeSafeDescription = {
                type: description.type,
                sdp: origSdpString
            };

            const result = await origSetLocalDescription.apply(this, [chromeSafeDescription, ...args.slice(1)]);

            // If credentials already arrived (fast Go path), apply immediately.
            // Otherwise, the localDescription getter will munge on-read.
            if (entry.shadowCredentials) {
                const pinnedForMutation = entry._cachedPinnedSdp || filterSdpToPreferredCodecs(origSdpString);
                const targetSdp = mungeSdpTransport(pinnedForMutation, entry.shadowCredentials);
                try {
                    description.sdp = targetSdp;
                    BCR_LOG('[BCR] Description object mutated with shadow SDP bridgeId=', entry.bridgeId);
                } catch (_) {}
                // Trickle ICE candidates
                _applyCredentialsAndTrickle(this, entry);
            }

            return result;
        };

        rtcProto.setRemoteDescription = function patchedSetRemoteDescription(...args) {
            const description = args[0];
            let browserDescription = description;
            if (
                description &&
                typeof description.type === 'string' &&
                typeof description.sdp === 'string'
            ) {
                const entry = ensureRtcState(this);
                entry.lastSeen = performance.now();

                const descType = description.type.toLowerCase();
                let effectiveRemoteSdp = description.sdp;

                // Aggressive renegotiation pinning: scrub remote offers before any
                // downstream processing so H264/VP9 never leak into Go state.
                if (descType === 'offer') {
                    const scrubbedOfferSdp = filterSdpToPreferredCodecs(description.sdp);
                    if (scrubbedOfferSdp !== description.sdp) {
                        BCR_LOG('[BCR] Scrubbed remote offer to preferred codecs bridgeId=', entry.bridgeId);
                    }
                    effectiveRemoteSdp = scrubbedOfferSdp;
                    try {
                        description.sdp = scrubbedOfferSdp;
                    } catch (_) {
                        // Description may be immutable in some runtimes.
                    }
                    browserDescription = buildDescriptionLike(description, scrubbedOfferSdp);
                }

                BCR_LOG('[BCR] Captured SDP type:', description.type, 'direction=remote bridgeId=', entry.bridgeId);

                if (descType === 'offer') {
                    entry.lastRemoteOfferSdp = effectiveRemoteSdp;
                }

                if (descType === 'answer' && typeof entry.lastRemoteOfferSdp === 'string') {
                    const translatedSdp = rewriteAnswerMidsForOfferInPlace(description.sdp, entry.lastRemoteOfferSdp);
                    if (translatedSdp !== description.sdp) {
                        BCR_LOG('[BCR] Rewrote remote answer mids in-place to match offer bridgeId=', entry.bridgeId);
                        browserDescription = buildDescriptionLike(description, translatedSdp);
                    }
                }

                captureIceServersFromConnection(this);

                emitShadowEvent('BCR_RTC_SHADOW_REMOTE', {
                    bridgeId: entry.bridgeId,
                    sdpType: description.type,
                    sdp: effectiveRemoteSdp,
                    iceServers: entry.iceServers ?? [],
                    timestamp: Date.now(),
                });
            }
           
            const srdPromise = origSetRemoteDescription.apply(this, [browserDescription, ...args.slice(1)]);
            const entry2 = rtcStateByPeer.get(this);
            if (entry2 && entry2.shadowCredentials) {
                return srdPromise.catch((err) => {
                    const errMsg = err?.message || String(err);
                    const errName = err?.name || 'UnknownError';
                    BCR_LOG('[BCR] setRemoteDescription FAILED bridgeId=', entry2.bridgeId,
                        'name=', errName, 'message=', errMsg);

                    // Emit diagnostic so Go side / content script can correlate failures
                    emitShadowEvent('BCR_SHADOW_DIAGNOSTIC', {
                        type: 'SET_REMOTE_DESC_FAILED',
                        bridgeId: entry2.bridgeId,
                        errorName: errName,
                        errorMessage: errMsg,
                        sdpType: entry2?.lastCreateActionType ?? 'unknown',
                        timestamp: Date.now(),
                    });

                    // Still swallow to prevent unhandled rejection crashing Teams UI,
                    // but NOW we have a record of it.
                });
            }
            return srdPromise;
        };

        async function patchedCreateAction(origMethod, actionType, ...args) {
            const origDescription = await origMethod.apply(this, args);
            // In case creation failed or returned nothing
            if (!origDescription || typeof origDescription.sdp !== 'string') {
                return origDescription;
            }

            const entry = ensureRtcState(this);
            entry.lastSeen = performance.now();
            entry.lastCreateActionType = actionType;
            entry.lastOriginalLocalSdp = origDescription.sdp;

            // ── SDP Codec Pinning ─────────────────────────────────────────────────────
            const pinnedSdp = filterSdpToPreferredCodecs(origDescription.sdp);
            entry._cachedPinnedSdp = pinnedSdp;

            captureIceServersFromConnection(this);

            // SHADOW_LOCAL emission is deferred to setLocalDescription to avoid
            // sending duplicate events when Teams calls createOffer then setLocalDescription.

            BCR_LOG('[BCR] Captured SDP type:', origDescription.type ?? actionType, 'direction=local build from', actionType, 'bridgeId=', entry.bridgeId);

            // Must return an object that circumvents strict instanceof RTCSessionDescription checks inside RxJS / React data pipelines
            return buildDescriptionLike(origDescription, pinnedSdp);
        }

        rtcProto.createOffer = function patchedCreateOffer(...args) {
            return patchedCreateAction.call(this, origCreateOffer, 'offer', ...args);
        };

        rtcProto.createAnswer = function patchedCreateAnswer(...args) {
            return patchedCreateAction.call(this, origCreateAnswer, 'answer', ...args);
        };

        // ── addIceCandidate interception ───────────────────────────────────────
        // When the VDI signaling delivers remote peer's ICE candidates to the
        // browser's PC via addIceCandidate, forward them to the shadow PC in
        // bcr_client so it can respond to the remote peer's connectivity checks.
        const origAddIceCandidate = rtcProto.addIceCandidate;
        rtcProto.addIceCandidate = function patchedAddIceCandidate(candidate, ...rest) {
            const entry = rtcStateByPeer.get(this);
            if (candidate && candidate.candidate) {
                if (entry && entry.bridgeId) {
                    BCR_LOG('[BCR] Forwarding remote ICE candidate to shadow bridgeId=', entry.bridgeId);
                    emitShadowEvent('BCR_RTC_SHADOW_ICE_CANDIDATE', {
                        bridgeId: entry.bridgeId,
                        candidate: candidate.candidate,
                        sdpMid: candidate.sdpMid ?? '0',
                        timestamp: Date.now(),
                    });
                }
            }
            
            // Call through so the browser's PC can process it if it's healthy natively.
            const aicPromise = origAddIceCandidate.apply(this, [candidate, ...rest]);
            
            // If we are managing this connection in the shadow PC, Teams' native PC
            // often falls into a bad state (e.g. failed setRemoteDescription).
            // We must swallow addIceCandidate rejections to avoid crashing Teams' UI
            // with Unhandled Promise Rejections.
            if (entry && entry.shadowCredentials && aicPromise && typeof aicPromise.catch === 'function') {
                return aicPromise.catch((err) => {
                     BCR_LOG('[BCR] addIceCandidate failed natively (swallowed for shadow PC):', err?.message || err);
                });
            }
            return aicPromise;
        };

        // ── Trickle ICE overrides (addEventListener & onicecandidate) ──────────
        // Intercept bound listeners to silently drop native Chrome candidates
        // while allowing our synthetic `bcr_client` STUN payloads to trickle freely natively.
        const origAddEventListener = rtcProto.addEventListener;
        rtcProto.addEventListener = function patchedAddEventListener(type, listener, options) {
            if (type === 'icecandidate' && typeof listener === 'function') {
                if (!listener.__bcrWrapper) {
                    listener.__bcrWrapper = function (event) {
                        if (event.isTrusted) return; // Drop native Chrome trickle!
                        return listener.apply(this, arguments);
                    };
                }
                return origAddEventListener.call(this, type, listener.__bcrWrapper, options);
            }
            return origAddEventListener.call(this, type, listener, options);
        };

        const origRemoveEventListener = rtcProto.removeEventListener;
        rtcProto.removeEventListener = function patchedRemoveEventListener(type, listener, options) {
            if (type === 'icecandidate' && listener && listener.__bcrWrapper) {
                return origRemoveEventListener.call(this, type, listener.__bcrWrapper, options);
            }
            return origRemoveEventListener.call(this, type, listener, options);
        };

        const onIceDesc = Object.getOwnPropertyDescriptor(rtcProto, 'onicecandidate');
        if (onIceDesc) {
            Object.defineProperty(rtcProto, 'onicecandidate', {
                get() {
                    return this.__bcrOnIceCandidate || onIceDesc.get.call(this);
                },
                set(listener) {
                    this.__bcrOnIceCandidate = listener;
                    if (typeof listener === 'function') {
                        const wrapper = function (event) {
                            if (event.isTrusted) return; // Drop native Chrome trickle!
                            return listener.apply(this, arguments);
                        };
                        onIceDesc.set.call(this, wrapper);
                    } else {
                        onIceDesc.set.call(this, listener);
                    }
                },
                enumerable: true,
                configurable: true
            });
        }
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
        writable: true,
        configurable: true,
        enumerable: false,
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
            enumerable: true,
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
            enumerable: true,
        });
    }

    // ─── MSE Telemetry Helpers (shared with suppression gate) ─────────────────
    const sourceBufferMeta = new WeakMap();
    const sourceBufferIds = new WeakMap();
    const loggedFormats = new Set();
    let sourceBufferSeq = 0;

    const SCAN_WINDOW_BYTES = 8192;
    const MOOV_NEAR_START_BYTES = 2048;

    function toByteView(data) {
        if (data instanceof ArrayBuffer) return new Uint8Array(data);
        if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
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
        const c = (codec || '').toLowerCase();
        if (mt.startsWith('audio/')) return 'audio';
        if (mt.startsWith('video/')) return 'video';
        if (mt.includes('text') || mt.includes('vtt')) return 'text';
        if (/(mp4a|opus|vorbis|flac|aac|ac-3|ec-3)/i.test(c)) return 'audio';
        if (/(avc1|av01|vp8|vp9|hvc1|hev1|theora)/i.test(c)) return 'video';
        return 'unknown';
    }

    function setSourceBufferMeta(sb, mimeType) {
        const mime = typeof mimeType === 'string' ? mimeType : 'unknown';
        const codec = extractCodec(mime);
        const trackType = resolveTrackType(mime, codec);
        sourceBufferMeta.set(sb, {
            trackType,
            mimeType: mime,
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
        const ebml = [0x1A, 0x45, 0xDF, 0xA3];
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
            } catch (_) { }
            return sb;
        };
    }

    // ─── changeType Patch (preserve existing behaviour) ──────────────────────
    if (typeof origChangeType === 'function') {
        SourceBuffer.prototype.changeType = function patchedChangeType(mimeType) {
            const result = origChangeType.call(this, mimeType);
            try { setSourceBufferMeta(this, mimeType); } catch (_) { }
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
                    trackType: 'unknown',
                    mimeType: 'unknown',
                    codec: 'unknown',
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
                        type: 'BCR_MEDIA_CHUNK',
                        size: copied.byteLength,
                        ts: performance.now(),
                        trackType: meta.trackType,
                        mimeType: meta.mimeType,
                        codec: meta.codec,
                        sourceBufferId: meta.sourceBufferId,
                        isInitSegment,
                        chunkBuffer: copied.buffer,
                    },
                    '*',
                    [copied.buffer]  // transfer ownership to content script
                );
            }
        } catch (_) { }

        if (isSuppressed) {
            // Gate the native call — decoder never receives this data.
            // Fire synthetic updateend so page.js streaming loop continues.
            const sb = this;
            queueMicrotask(() => {
                try {
                    sb.dispatchEvent(new Event('update'));
                    sb.dispatchEvent(new Event('updateend'));
                } catch (_) { }
            });
            return; // intentional: no origAppendBuffer call
        }

        return origAppendBuffer.call(this, data);
    };

    patchAddSourceBuffer('MediaSource');
    patchAddSourceBuffer('ManagedMediaSource');

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