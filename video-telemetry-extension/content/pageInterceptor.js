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

    function BCR_LOG(...args) {
        console.log('[BCR-INTERNAL]', ...args);
    }

    try {
        window.BCR_LOG = BCR_LOG;
    } catch (_) {
        // Ignore failures in locked-down contexts.
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

    // Save RTCPeerConnection method originals for shadow signaling hooks.
    const rtcProto = window.RTCPeerConnection?.prototype;
    const origSetLocalDescription = rtcProto?.setLocalDescription;
    const origSetRemoteDescription = rtcProto?.setRemoteDescription;
    const origCreateOffer = rtcProto?.createOffer;
    const origCreateAnswer = rtcProto?.createAnswer;
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

    const RTC_WAIT_TIMEOUT_MS = 5000;
    const rtcStateByPeer = new WeakMap();
    const rtcReadyByBridgeId = new Map();
    const rtcWaitersByBridgeId = new Map();
    const rtcLastErrorByBridgeId = new Map();

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

        return entry;
    }

    function resolveShadowReady(bridgeId, payload) {
        if (!bridgeId) return;
        rtcReadyByBridgeId.set(bridgeId, payload);
        rtcLastErrorByBridgeId.delete(bridgeId);

        const waiters = rtcWaitersByBridgeId.get(bridgeId);
        if (!waiters || waiters.length === 0) return;

        rtcWaitersByBridgeId.delete(bridgeId);
        for (const waiter of waiters) {
            clearTimeout(waiter.timer);
            waiter.resolve(payload);
        }
    }

    function rejectShadowReady(bridgeId, reason = 'shadow_error') {
        if (!bridgeId) return;

        rtcLastErrorByBridgeId.set(bridgeId, reason);

        const waiters = rtcWaitersByBridgeId.get(bridgeId);
        if (!waiters || waiters.length === 0) return;

        rtcWaitersByBridgeId.delete(bridgeId);
        for (const waiter of waiters) {
            clearTimeout(waiter.timer);
            waiter.resolve(null);
        }
    }

    function awaitShadowReady(bridgeId, timeoutMs) {
        if (!bridgeId) return Promise.resolve(null);

        const cached = rtcReadyByBridgeId.get(bridgeId);
        if (cached) return Promise.resolve(cached);

        return new Promise((resolve) => {
            const timer = setTimeout(() => {
                const list = rtcWaitersByBridgeId.get(bridgeId) ?? [];
                rtcWaitersByBridgeId.set(
                    bridgeId,
                    list.filter((item) => item.resolve !== resolve)
                );
                resolve(null);
            }, timeoutMs);

            const waiters = rtcWaitersByBridgeId.get(bridgeId) ?? [];
            waiters.push({ resolve, timer });
            rtcWaitersByBridgeId.set(bridgeId, waiters);
        });
    }

    function consumeShadowErrorReason(bridgeId) {
        if (!bridgeId) return null;
        const reason = rtcLastErrorByBridgeId.get(bridgeId) ?? null;
        rtcLastErrorByBridgeId.delete(bridgeId);
        return reason;
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
                const parts = trimmed.split(' ');
                if (parts.length > 4 && parts[4].includes(':')) {
                    BCR_LOG('[BCR] Dropping IPv6 candidate to avoid Teams crash:', parts[4]);
                    return false;
                }
                return true;
            });
        }

        let mid = '0';
        BCR_LOG('[BCR] Preparing to synthetically trickle', candidateLines.length, 'candidates...');

        // Stagger dispatching synthetic candidates slightly avoiding queue flood
        candidateLines.forEach((line, index) => {
            setTimeout(() => {
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
            }, index * 20 + 50); // slight offset start + stagger
        });

        // Finally dispatch end-of-candidates
        setTimeout(() => {
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
        }, candidateLines.length * 20 + 80);
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
        rtcReadyByBridgeId.delete(entry.bridgeId);
        rejectShadowReady(entry.bridgeId);

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
                BCR_LOG('[BCR] Received SHADOW_READY bridgeId=', bridgeId);
                resolveShadowReady(bridgeId, payload);
            }
        }

        if (data.type === 'BCR_RTC_SHADOW_ERROR') {
            const payload = data.payload;
            const bridgeId = payload?.bridgeId;
            if (typeof bridgeId === 'string' && bridgeId.length > 0) {
                BCR_LOG('[BCR] Received SHADOW_ERROR bridgeId=', bridgeId, 'stage=', payload?.stage, 'reason=', payload?.reason);
                rejectShadowReady(bridgeId, payload?.reason ?? 'shadow_error');
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
            const mungedSdp = mungeSdpTransport(desc.sdp, entry.shadowCredentials);
            // Return a plain object that looks like RTCSessionDescription
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

        rtcProto.setLocalDescription = async function patchedSetLocalDescription(...args) {
            const description = args[0];
            if (!description || typeof description.sdp !== 'string') {
                return origSetLocalDescription.apply(this, args);
            }

            const entry = ensureRtcState(this);
            entry.lastSeen = performance.now();

            let origSdpString = description.sdp;
            let isRedundant = false;

            if (entry.lastOriginalLocalSdp && entry.shadowCredentials) {
                // Determine if this setLocalDescription is just the subsequent application
                // of the offer/answer we just generated upstream in createOffer/createAnswer.
                const typeMatches = (description.type ?? '').toLowerCase() === (entry.lastCreateActionType ?? '').toLowerCase();

                if (typeMatches) {
                    isRedundant = true;
                    // Because the VDI app could have mutated the SDP structurally by stripping codecs
                    // before giving it back to us, we just safely pass the last raw original local SDP
                    // back into Chrome to prevent DOMExceptions out of ICE mismatches.
                    origSdpString = entry.lastOriginalLocalSdp;
                }
            }

            if (!isRedundant) {
                emitShadowEvent('BCR_RTC_SHADOW_LOCAL', {
                    bridgeId: entry.bridgeId,
                    sdpType: description.type ?? 'unknown',
                    sdp: description.sdp,
                    iceServers: entry.iceServers ?? [],
                    timestamp: Date.now(),
                });

                BCR_LOG('[BCR] Captured SDP type:', description.type ?? 'unknown', 'direction=local bridgeId=', entry.bridgeId);

                if ((description.type ?? '').toLowerCase() === 'offer') {
                    rtcReadyByBridgeId.delete(entry.bridgeId);
                }

                const shadowReady = await awaitShadowReady(entry.bridgeId, RTC_WAIT_TIMEOUT_MS);

                const currentEntry = ensureRtcState(this);
                if (currentEntry.bridgeId !== entry.bridgeId) {
                    BCR_LOG('[BCR] BridgeId changed during setLocalDescription; fail-open with original SDP');
                    return origSetLocalDescription.apply(this, args);
                }

                if (!shadowReady) {
                    const errReason = consumeShadowErrorReason(entry.bridgeId);
                    BCR_LOG('[BCR] SHADOW_READY timeout or error, fail-open bridgeId=', entry.bridgeId, 'reason=', errReason ?? 'timeout');
                    entry.shadowCredentials = null;
                } else {
                    entry.shadowCredentials = shadowReady;
                    BCR_LOG('[BCR] Shadow credentials stored bridgeId=', entry.bridgeId);
                }
            } else {
                BCR_LOG('[BCR] Skipping redundant SHADOW_LOCAL for pre-munged SDP bridgeId=', entry.bridgeId);
            }

            // CRITICAL: Pass the ORIGINAL SDP to Chrome's native method.
            // Chrome's ICE agent validates that ice-ufrag/pwd match its internal state.
            const chromeSafeDescription = {
                type: description.type,
                sdp: origSdpString
            };

            const result = await origSetLocalDescription.apply(this, [chromeSafeDescription, ...args.slice(1)]);

            // After Chrome accepts the original SDP, mutate the argument if the framework reads it later.
            if (entry.shadowCredentials) {
                const targetSdp = mungeSdpTransport(origSdpString, entry.shadowCredentials);
                try {
                    description.sdp = targetSdp;
                    BCR_LOG('[BCR] Description object mutated with shadow SDP bridgeId=', entry.bridgeId);
                } catch (_) {
                    BCR_LOG('[BCR] Description object mutation failed (frozen) bridgeId=', entry.bridgeId);
                }
            }

            // After setLocalDescription succeeds, start trickling the ICE candidates 
            // cached securely inside the shadow setup properties out into the VDI framework.
            if (entry.shadowCredentials && Array.isArray(entry.shadowCredentials.candidates)) {
                // Deduplicate by tracking the signature of the generation
                const generationId = entry.shadowCredentials.generatedAt || entry.shadowCredentials.iceUfrag;
                if (entry.lastDispatchedTrickleId !== generationId) {
                    entry.lastDispatchedTrickleId = generationId;
                    dispatchShadowTrickleCandidates(this, entry.shadowCredentials.candidates);
                }
            }

            return result;
        };

        rtcProto.setRemoteDescription = function patchedSetRemoteDescription(...args) {
            const description = args[0];
            if (
                description &&
                typeof description.type === 'string' &&
                typeof description.sdp === 'string'
            ) {
                const entry = ensureRtcState(this);
                entry.lastSeen = performance.now();

                BCR_LOG('[BCR] Captured SDP type:', description.type, 'direction=remote bridgeId=', entry.bridgeId);

                // When a new remote offer arrives, the shadow PC will be rebuilt with
                // fresh credentials. Invalidate any stale SHADOW_READY so the upcoming
                // setLocalDescription(answer) waits for the fresh response.
                if (description.type.toLowerCase() === 'offer') {
                    rtcReadyByBridgeId.delete(entry.bridgeId);
                }

                emitShadowEvent('BCR_RTC_SHADOW_REMOTE', {
                    bridgeId: entry.bridgeId,
                    sdpType: description.type,
                    sdp: description.sdp,
                    timestamp: Date.now(),
                });
            }
            // The browser's own PeerConnection will often fail ICE because its native
            // candidates are suppressed and replaced with shadow candidates. For managed
            // connections, catch and suppress the rejection to prevent Teams from
            // detecting the failure and triggering a renegotiation storm.
            const srdPromise = origSetRemoteDescription.apply(this, args);
            const entry2 = rtcStateByPeer.get(this);
            if (entry2 && entry2.shadowCredentials) {
                return srdPromise.catch((err) => {
                    BCR_LOG('[BCR] setRemoteDescription failed (expected for managed connection) bridgeId=',
                        entry2.bridgeId, 'err=', err?.message || err);
                    // Swallow — the shadow PC handles the real connection to Teams.
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

            emitShadowEvent('BCR_RTC_SHADOW_LOCAL', {
                bridgeId: entry.bridgeId,
                sdpType: origDescription.type ?? actionType,
                sdp: origDescription.sdp,
                iceServers: entry.iceServers ?? [],
                timestamp: Date.now(),
            });

            BCR_LOG('[BCR] Captured SDP type:', origDescription.type ?? actionType, 'direction=local build from', actionType, 'bridgeId=', entry.bridgeId);

            // Wait for the freshly built shadow PC's credentials
            const shadowReady = await awaitShadowReady(entry.bridgeId, RTC_WAIT_TIMEOUT_MS);

            const currentEntry = ensureRtcState(this);
            if (currentEntry.bridgeId !== entry.bridgeId) {
                BCR_LOG('[BCR] BridgeId changed during', actionType, 'fail-open with original SDP');
                return origDescription;
            }

            if (shadowReady) {
                entry.shadowCredentials = shadowReady;
                // Store the original SDP so we can retrieve it in setLocalDescription later.
                entry.lastOriginalLocalSdp = origDescription.sdp;

                const mungedSdp = mungeSdpTransport(origDescription.sdp, entry.shadowCredentials);
                BCR_LOG('[BCR]', actionType, 'resolved and munged seamlessly bridgeId=', entry.bridgeId);

                // Must return an object that circumvents strict instanceof RTCSessionDescription checks inside RxJS / React data pipelines
                return buildDescriptionLike(origDescription, mungedSdp);
            }

            BCR_LOG('[BCR] SHADOW_READY timeout or error during', actionType, 'fail-open via original result bridgeId=', entry.bridgeId);
            return origDescription;
        }

        rtcProto.createOffer = function patchedCreateOffer(...args) {
            rtcReadyByBridgeId.delete(ensureRtcState(this).bridgeId);
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

    patchAddSourceBuffer('MediaSource');
    patchAddSourceBuffer('ManagedMediaSource');

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