// Wait for Wails to inject runtime bridging
window.onload = function () {
    const videoElement = document.getElementById("videoPlayer");
    const audioElement = document.createElement("audio");
    audioElement.id = "audioPlayer";
    audioElement.autoplay = true;
    audioElement.playsInline = true;
    audioElement.muted = false;
    audioElement.style.display = "none";
    document.body.appendChild(audioElement);

    // Synchronize audio playback with video element
    videoElement.addEventListener('play', () => {
        if (playbackMode === 'mse') {
            audioElement.play().catch((err) => logTerminal(`[Sync] Audio play blocked: ${err}`));
        }
    });

    videoElement.addEventListener('pause', () => {
        if (playbackMode === 'mse') {
            audioElement.pause();
        }
    });

    videoElement.addEventListener('seeking', () => {
        if (playbackMode === 'mse') {
            audioElement.currentTime = videoElement.currentTime;
        }
    });

    videoElement.addEventListener('ratechange', () => {
        if (playbackMode === 'mse') {
            audioElement.playbackRate = videoElement.playbackRate;
        }
    });

    videoElement.addEventListener('volumechange', () => {
        if (playbackMode === 'mse') {
            audioElement.volume = videoElement.volume;
            audioElement.muted = videoElement.muted;
        }
    });

    // Correct drift if video and audio drift apart by more than 150ms
    videoElement.addEventListener('timeupdate', () => {
        if (playbackMode === 'mse' && !videoElement.paused) {
            const drift = Math.abs(videoElement.currentTime - audioElement.currentTime);
            if (drift > 0.15) {
                audioElement.currentTime = videoElement.currentTime;
            }
        }
    });

    function logTerminal(message) {
        console.log(message);
        if (window.runtime && typeof window.runtime.LogInfo === "function") {
            window.runtime.LogInfo(message);
        }
    }

    function logErrorTerm(message) {
        console.error(message);
        if (window.runtime && typeof window.runtime.LogError === "function") {
            window.runtime.LogError(message);
        }
    }

    // ── Playback Mode Tracking ──────────────────────────────────────────────
    // 'idle' | 'mse' | 'webrtc'
    let playbackMode = 'idle';

    function setMode(newMode) {
        if (playbackMode === newMode) return;
        logTerminal(`[Mode] switching ${playbackMode} → ${newMode}`);
        playbackMode = newMode;
    }

    // ==========================================
    // WebRTC Loopback State (Teams / calls)
    // ==========================================
    let isWebRTCLive = false;
    let localPCs = {};
    let statsIntervals = {};
    let statsSnapshots = {};

    function cleanupStatsForBridge(bridgeID) {
        if (statsIntervals[bridgeID]) {
            clearInterval(statsIntervals[bridgeID]);
            delete statsIntervals[bridgeID];
        }

        Object.keys(statsSnapshots).forEach((k) => {
            if (k.startsWith(`${bridgeID}:`)) {
                delete statsSnapshots[k];
            }
        });
    }

    async function collectAndLogReceiverStats(bridgeID, pc) {
        if (!pc || pc.connectionState === "closed") {
            return;
        }

        try {
            const report = await pc.getStats();
            const codecById = new Map();
            let inboundCount = 0;

            report.forEach((entry) => {
                if (entry.type === "codec") {
                    codecById.set(entry.id, entry);
                }
            });

            report.forEach((entry) => {
                if (entry.type !== "inbound-rtp" || entry.isRemote) {
                    return;
                }

                inboundCount++;

                const kind = entry.kind || entry.mediaType || "unknown";
                const codec = codecById.get(entry.codecId);
                const codecMime = (codec && codec.mimeType) ? codec.mimeType : "unknown";
                const key = `${bridgeID}:${entry.id}`;
                const prev = statsSnapshots[key] || {};

                const bytesReceived = entry.bytesReceived || 0;
                const packetsReceived = entry.packetsReceived || 0;
                const packetsLost = entry.packetsLost || 0;

                const bytesDelta = (prev.bytesReceived !== undefined) ? (bytesReceived - prev.bytesReceived) : 0;
                const packetsDelta = (prev.packetsReceived !== undefined) ? (packetsReceived - prev.packetsReceived) : 0;

                if (kind === "video") {
                    const framesDecoded = entry.framesDecoded || 0;
                    const keyFramesDecoded = entry.keyFramesDecoded || 0;
                    const framesDelta = (prev.framesDecoded !== undefined) ? (framesDecoded - prev.framesDecoded) : 0;

                    logTerminal(
                        `[Loopback][Stats][Video] bridgeId=${bridgeID} codec=${codecMime} bytes=${bytesReceived}(+${bytesDelta}) ` +
                        `pkts=${packetsReceived}(+${packetsDelta}) lost=${packetsLost} framesDecoded=${framesDecoded}(+${framesDelta}) keyFrames=${keyFramesDecoded}`
                    );

                    statsSnapshots[key] = {
                        bytesReceived,
                        packetsReceived,
                        framesDecoded,
                    };
                    return;
                }

                if (kind === "audio") {
                    logTerminal(
                        `[Loopback][Stats][Audio] bridgeId=${bridgeID} codec=${codecMime} bytes=${bytesReceived}(+${bytesDelta}) ` +
                        `pkts=${packetsReceived}(+${packetsDelta}) lost=${packetsLost}`
                    );

                    statsSnapshots[key] = {
                        bytesReceived,
                        packetsReceived,
                    };
                }
            });

            if (inboundCount === 0) {
                logTerminal(`[Loopback][Stats] bridgeId=${bridgeID} no inbound-rtp streams yet`);
            }
        } catch (err) {
            logErrorTerm(`[Loopback][Stats] getStats failed bridgeId=${bridgeID}: ${err}`);
        }
    }

    function startStatsPolling(bridgeID, pc) {
        cleanupStatsForBridge(bridgeID);
        statsIntervals[bridgeID] = setInterval(() => {
            collectAndLogReceiverStats(bridgeID, pc);
        }, 2000);
    }

    // ── Video Element base configuration ───────────────────────────────────
    videoElement.autoplay = true;
    videoElement.playsInline = true;
    videoElement.muted = false;

    // Sync HTML5 Video Fullscreen with Native Window Fullscreen
    videoElement.addEventListener('fullscreenchange', () => {
        if (document.fullscreenElement) {
            logTerminal("Video entered fullscreen. Maximizing native window.");
            window.runtime.WindowFullscreen();
        } else {
            logTerminal("Video exited fullscreen. Restoring native window.");
            window.runtime.WindowUnfullscreen();
        }
    });

    // ── Clip transform ─────────────────────────────────────────────────────
    // When the YouTube player is partially scrolled out of the browser viewport
    // in the VDI, the overlay window is clipped to the part that is actually
    // visible (otherwise it spills over Chrome's toolbar). The <video> then has
    // to be scaled up and shifted by the clipped-away amount so the picture
    // still lines up with the real player underneath.
    //
    // Values arrive as ratios of the clipped box, so they apply as CSS
    // percentages and are independent of this window's own pixel density.
    window.runtime.EventsOn("onVideoClip", (scaleX, scaleY, offsetX, offsetY) => {
        const s = videoElement.style;
        s.position = 'absolute';
        s.width  = `${scaleX * 100}%`;
        s.height = `${scaleY * 100}%`;
        s.left   = `${-offsetX * 100}%`;
        s.top    = `${-offsetY * 100}%`;
    });

    // ==========================================
    // MSE Player (YouTube video redirection)
    // ==========================================

    let mediaSources = { video: null, audio: null };
    let sourceBuffers = { video: null, audio: null };
    let chunkQueues = { video: [], audio: [] };
    let configuredMimeTypes = { video: null, audio: null };
    let pendingSourceBuffers = { video: null, audio: null };
    let mseReady = { video: false, audio: false };
    let mseInitCount = 0;          // how many init segments received — used for first-chunk logging
    let activeVideoID = null;      // the unique ID of the video currently playing/redirecting
    // Cache of the most recent init segment per track ({data, fullMime}). YouTube sends
    // an init segment only once per quality, so after a mid-stream decode error we must
    // re-inject the cached one to rebuild the SourceBuffer — otherwise the track stays
    // dead (e.g. audio keeps playing while video is frozen).
    let lastInitSegment = { video: null, audio: null };
    // Guards against a reset→error→reset loop: after too many resets we fall back to a
    // full teardown. Cleared once a track appends media cleanly again.
    let trackResetCount = { video: 0, audio: 0 };

    /**
     * Base64 → Uint8Array (no extra dependencies)
     */
    function base64ToUint8Array(b64) {
        const binary = atob(b64);
        const bytes = new Uint8Array(binary.length);
        for (let i = 0; i < binary.length; i++) {
            bytes[i] = binary.charCodeAt(i);
        }
        return bytes;
    }

    /**
     * Tear down the MSE pipeline. Called when switching to WebRTC mode or on
     * VIDEO_REMOVED.
     */
    // ── Teardown debounce ──────────────────────────────────────────────────
    // YouTube momentarily removes and re-adds the <video> element during ad
    // transitions and DOM reshuffles. Tearing the MSE pipeline down the instant
    // a VIDEO_REMOVED arrives destroys the SourceBuffer and forces a full
    // rebuild once media resumes — the visible "player torn down → buffering"
    // glitch. Instead we defer teardown by a short grace period and cancel it if
    // the stream proves still alive (a media chunk arrives, or the video is
    // re-added within the window).
    const TEARDOWN_GRACE_MS = 700;
    let pendingTeardownTimer = null;

    function scheduleTeardown(reason) {
        if (pendingTeardownTimer) return; // already pending
        logTerminal(`[MSE] teardown scheduled (${reason}) — ${TEARDOWN_GRACE_MS}ms grace`);
        pendingTeardownTimer = setTimeout(() => {
            pendingTeardownTimer = null;
            logTerminal("[MSE] grace period elapsed — tearing down");
            teardownMSE();
        }, TEARDOWN_GRACE_MS);
    }

    function cancelScheduledTeardown(reason) {
        if (!pendingTeardownTimer) return;
        clearTimeout(pendingTeardownTimer);
        pendingTeardownTimer = null;
        logTerminal(`[MSE] scheduled teardown cancelled (${reason})`);
    }

    function teardownMSE() {
        // Any explicit teardown supersedes a debounced one.
        if (pendingTeardownTimer) {
            clearTimeout(pendingTeardownTimer);
            pendingTeardownTimer = null;
        }
        if (!mediaSources.video && !mediaSources.audio) return;
        logTerminal("[MSE] Tearing down MSE pipeline");

        ['video', 'audio'].forEach((type) => {
            const ms = mediaSources[type];
            if (!ms) return;
            try {
                const sb = sourceBuffers[type];
                if (sb) {
                    try { sb.abort(); } catch (_) {}
                    ms.removeSourceBuffer(sb);
                }
                if (ms.readyState === 'open') {
                    ms.endOfStream();
                }
            } catch (e) { /* ignore */ }
        });

        sourceBuffers = { video: null, audio: null };
        chunkQueues = { video: [], audio: [] };
        configuredMimeTypes = { video: null, audio: null };
        pendingSourceBuffers = { video: null, audio: null };
        mediaSources = { video: null, audio: null };
        mseReady = { video: false, audio: false };
        mseInitCount = 0;
        activeVideoID = null;
        // The cached init belongs to the video we are tearing down. Leaving it would let
        // resetTrack() rebuild the NEXT video's track with the previous video's init.
        lastInitSegment = { video: null, audio: null };
        trackResetCount = { video: 0, audio: 0 };

        // Stop stall watchdog so it doesn't fire against a destroyed pipeline.
        if (stallWatchdogTimer) {
            clearInterval(stallWatchdogTimer);
            stallWatchdogTimer = null;
        }
        if (onWaitingHandler && videoElement) {
            videoElement.removeEventListener('waiting', onWaitingHandler);
            onWaitingHandler = null;
        }
        lastPlayheadTime = -1;
        lastPlayheadCheck = 0;

        // Revoke old object URL and clear srcObject so the video element resets
        if (videoElement.src && videoElement.src.startsWith('blob:')) {
            URL.revokeObjectURL(videoElement.src);
        }
        videoElement.src = '';
        videoElement.removeAttribute('src');
        videoElement.srcObject = null;
        // Clearing src normally rewinds the element, but if the old media is still
        // resolving the scrubber can keep the previous video's position. Force it.
        try { videoElement.currentTime = 0; } catch (_) {}

        if (audioElement.src && audioElement.src.startsWith('blob:')) {
            URL.revokeObjectURL(audioElement.src);
        }
        audioElement.src = '';
        audioElement.removeAttribute('src');
        audioElement.srcObject = null;
        try { audioElement.currentTime = 0; } catch (_) {}

        setMode('idle');
        if (window.go && window.go.main && window.go.main.App) {
            window.go.main.App.NotifyMSEActive(false).catch(() => {});
        }
        logTerminal("[MSE] Pipeline torn down");
    }

    /**
     * Reset a SINGLE track (video or audio) in place — used to recover from a decode
     * error WITHOUT tearing down the whole pipeline (which hides the overlay window).
     * A bad Opus segment poisons only the <audio> element; resetting just that track
     * lets video (and the overlay) keep playing, and the next chunk rebuilds the track.
     * Deliberately does NOT touch playbackMode, activeVideoID, the other track, or
     * overlay visibility.
     */
    function resetTrack(trackType) {
        trackResetCount[trackType]++;
        if (trackResetCount[trackType] > 3) {
            logErrorTerm(`[MSE][${trackType}] too many resets (${trackResetCount[trackType]}) — full teardown`);
            trackResetCount[trackType] = 0;
            teardownMSE();
            return;
        }
        logTerminal(`[MSE][${trackType}] resetting track in place (error recovery) — overlay/other track untouched`);
        const targetEl = (trackType === 'video') ? videoElement : audioElement;
        const ms = mediaSources[trackType];
        try {
            const sb = sourceBuffers[trackType];
            if (sb && ms) {
                try { sb.abort(); } catch (_) {}
                try { ms.removeSourceBuffer(sb); } catch (_) {}
            }
            if (ms && ms.readyState === 'open') {
                try { ms.endOfStream(); } catch (_) {}
            }
        } catch (_) {}

        if (targetEl.src && targetEl.src.startsWith('blob:')) {
            try { URL.revokeObjectURL(targetEl.src); } catch (_) {}
        }
        targetEl.src = '';
        targetEl.removeAttribute('src');

        // Clear only this track's state.
        mediaSources[trackType] = null;
        sourceBuffers[trackType] = null;
        chunkQueues[trackType] = [];
        configuredMimeTypes[trackType] = null;
        pendingSourceBuffers[trackType] = null;
        mseReady[trackType] = false;

        // Rebuild immediately by re-injecting the cached init segment. YouTube won't
        // resend an init after a mid-stream error, so without this the SourceBuffer is
        // never recreated and the track stays dead. setupTrackMSE re-attaches a fresh
        // MediaSource (which also clears the element's error state); on 'sourceopen' it
        // creates the SourceBuffer from pendingSourceBuffers and drains the queued init,
        // after which normal media segments append and the track recovers.
        const cachedInit = lastInitSegment[trackType];
        if (cachedInit) {
            setupTrackMSE(trackType);
            pendingSourceBuffers[trackType] = cachedInit.fullMime;
            chunkQueues[trackType] = [cachedInit.data.slice()];
            logTerminal(`[MSE][${trackType}] re-injecting cached init segment to rebuild track`);
        } else {
            logTerminal(`[MSE][${trackType}] no cached init segment — track will rebuild on next init`);
        }
    }

    /**
     * Create and attach a fresh MediaSource to the target element. Called on
     * the first MEDIA_CHUNK that arrives while in idle or MSE mode.
     */
    function setupTrackMSE(trackType) {
        if (mediaSources[trackType]) return; // already set up

        logTerminal(`[MSE][${trackType}] Setting up MediaSource`);

        // Detach any existing WebRTC stream (only for video element)
        if (trackType === 'video' && videoElement.srcObject) {
            logTerminal("[MSE] Detaching WebRTC srcObject to switch to MSE mode");
            videoElement.srcObject = null;
        }

        const ms = new MediaSource();
        mediaSources[trackType] = ms;

        const targetEl = (trackType === 'video') ? videoElement : audioElement;
        targetEl.src = URL.createObjectURL(ms);

        ms.addEventListener('sourceopen', (e) => {
            // Guard against stale events from a torn-down MediaSource. `e.target`
            // is always `ms` here (the listener is bound directly to it), so that
            // comparison is a no-op — the real check is whether `ms` is still the
            // session's CURRENT MediaSource. Under rapid VIDEO_ADDED/REMOVED churn
            // (e.g. ad transitions) a session can be torn down and a new one set up
            // before this old ms's queued 'sourceopen' event is dispatched; without
            // this check it would still fire and set mseReady[trackType] = true for
            // a NEWER MediaSource that hasn't actually opened yet, causing
            // addSourceBuffer() to throw "readyState is not 'open'" and silently
            // drop that init segment.
            if (mediaSources[trackType] !== ms) return;
            logTerminal(`[MSE][${trackType}] MediaSource sourceopen — pipeline ready`);
            mseReady[trackType] = true;

            // Create the SourceBuffer if we received the init segment before sourceopen
            const fullMime = pendingSourceBuffers[trackType];
            if (fullMime && !sourceBuffers[trackType]) {
                try {
                    const newSb = ms.addSourceBuffer(fullMime);
                    newSb.mode = 'segments';
                    sourceBuffers[trackType] = newSb;
                    if (!chunkQueues[trackType]) chunkQueues[trackType] = [];
                    configuredMimeTypes[trackType] = fullMime;

                    newSb.addEventListener('updateend', () => {
                        drainQueue(trackType);
                    });
                    newSb.addEventListener('error', (err) => {
                        logErrorTerm(`[MSE][${trackType}] SourceBuffer error: ${err}`);
                    });

                    logTerminal(`[MSE][${trackType}] SourceBuffer created (deferred) for ${fullMime}`);
                } catch (err) {
                    logErrorTerm(`[MSE][${trackType}] Failed to create deferred SourceBuffer for ${fullMime}: ${err}`);
                }
                pendingSourceBuffers[trackType] = null;
            }

            // Flush any queued chunks
            drainQueue(trackType);
        });

        ms.addEventListener('sourceclose', (e) => {
            // Same stale-instance guard as 'sourceopen' above — only the CURRENT
            // MediaSource for this trackType is allowed to mutate shared state.
            if (mediaSources[trackType] !== ms) return;
            logTerminal(`[MSE][${trackType}] MediaSource sourceclose — resetting for fresh session`);
            mseReady[trackType] = false;
            mediaSources[trackType] = null;
            sourceBuffers[trackType] = null;
            chunkQueues[trackType] = [];
            configuredMimeTypes[trackType] = null;
            pendingSourceBuffers[trackType] = null;
        });

        ms.addEventListener('sourceended', (e) => {
            if (mediaSources[trackType] !== ms) return;
            logTerminal(`[MSE][${trackType}] MediaSource sourceended`);
        });

        if (trackType === 'video') {
            // Show the overlay and start playback only when the browser signals it
            // has enough buffered data — not before, to avoid flashing a blank window.
            videoElement.addEventListener('canplay', () => {
                if (playbackMode !== 'mse') return; // torn down before canplay fired
                videoElement.play().catch((e) => {
                    logTerminal(`[MSE] canplay auto-play blocked: ${e}`);
                });
                if (window.go && window.go.main && window.go.main.App) {
                    window.go.main.App.NotifyMSEActive(true).catch(() => {});
                }
            }, { once: true });

            // Stall watchdog: detect when playhead is stuck at a gap and jump past it.
            setupStallRecovery(videoElement, 'video');

            // Recover from native video decode errors (bad segments, GPU reset, etc.)
            // Reset only the video track — keep the overlay visible so it doesn't vanish.
            videoElement.addEventListener('error', () => {
                if (videoElement.error && playbackMode === 'mse') {
                    logTerminal(`[MSE] Video element error (code=${videoElement.error.code}) — resetting video track (overlay kept)`);
                    resetTrack('video');
                }
            }, { once: true });
        } else {
            // Ensure audio element plays when it's ready
            audioElement.addEventListener('canplay', () => {
                if (playbackMode !== 'mse') return;
                audioElement.play().catch((e) => {
                    logTerminal(`[MSE] audio canplay auto-play blocked: ${e}`);
                });
            }, { once: true });

            // Recover from native audio decode errors, mirroring the video error
            // handler above. Without this, one bad Opus segment sets
            // HTMLMediaElement.error permanently — every subsequent appendBuffer()
            // call then throws InvalidStateError forever, and audio silently stays
            // dead for the rest of the session while video keeps playing.
            audioElement.addEventListener('error', () => {
                if (audioElement.error && playbackMode === 'mse') {
                    logTerminal(`[MSE] Audio element error (code=${audioElement.error.code}) — resetting audio track only (video/overlay keep playing)`);
                    resetTrack('audio');
                }
            }, { once: true });
        }

        if (playbackMode !== 'mse') {
            setMode('mse');
        }
        logTerminal(`[MSE][${trackType}] Pipeline set up — waiting for sourceopen`);
    }

    /**
     * Drain the pending chunk queue for a given trackType.
     * Respects the SourceBuffer.updating flag to avoid InvalidStateError.
     */
    function drainQueue(trackType) {
        const sb = sourceBuffers[trackType];
        const queue = chunkQueues[trackType];

        if (!sb || !queue || queue.length === 0) return;
        if (sb.updating) return; // will be called again from updateend

        // Once the media element has faulted, appendBuffer() throws
        // InvalidStateError unconditionally — trying again per incoming chunk
        // just spams identical errors until the element's 'error' handler resets
        // the pipeline. Bail out now and let that handler do the reset.
        const targetEl = (trackType === 'video') ? videoElement : audioElement;
        if (targetEl.error) return;

        const chunk = queue.shift();
        try {
            sb.appendBuffer(chunk);
            trackResetCount[trackType] = 0; // healthy append — clear the reset-loop guard
            // Appending may have created/extended a future range. Check for a gap to
            // jump. NOT gated on !paused: after an error reset the element is paused at
            // currentTime 0 with data only far ahead, and it can only resume once we
            // seek it into the buffer (which then lets 'canplay' fire and play()).
            if (targetEl && !targetEl.seeking) {
                tryJumpGap(targetEl, trackType);
            }
        } catch (e) {
            logErrorTerm(`[MSE][${trackType}] appendBuffer error: ${e}`);
            // On quota exceeded, trim buffered data behind the playhead.
            // Keep a 30-second window behind currentTime so seek-back still works.
            if (e.name === 'QuotaExceededError' && sb.buffered.length > 0) {
                const removeEnd = targetEl.currentTime - 30;
                if (removeEnd > sb.buffered.start(0)) {
                    try { sb.remove(sb.buffered.start(0), removeEnd); } catch (_) {}
                }
                // Re-add chunk after removal
                queue.unshift(chunk);
            }
        }
    }

    // ── Stall Watchdog + Gap Jump ─────────────────────────────────────────────
    // Detects when the playhead is stuck at a gap in the SourceBuffer (caused by
    // a dropped segment) and jumps to the next buffered range. Without this, MSE
    // hard-stalls indefinitely because Chrome only auto-jumps sub-second gaps.

    let stallWatchdogTimer = null;
    let onWaitingHandler = null;
    let lastPlayheadTime = -1;

    // Wall-clock of the last media chunk received, for the starvation watchdog in
    // the 1Hz reporting loop below. 0 = nothing received yet.
    let lastChunkAt = 0;

    // How long the pipeline may sit with an exhausted buffer and no incoming
    // chunks before we conclude the source is dead.
    //
    // This must tolerate normal pulse-decode gaps: the interceptor tops the
    // buffer up to HIGH_WATER_S (90s) and does not pulse again until it drops
    // below SEEK_LEAD_S (60s), so ~30s of playback with zero chunks is expected.
    // That is why the check also requires the buffer to be EXHAUSTED — during a
    // normal gap there are tens of seconds buffered, so it cannot false-fire.
    const MEDIA_STARVATION_MS = 8000;
    let lastPlayheadCheck = 0;

    /**
     * Scan the video SourceBuffer for a gap at the current playhead position
     * and jump past it. Returns true if a gap was found and jumped.
     */
    function tryJumpGap(el, trackType) {
        const sb = sourceBuffers[trackType];
        if (!sb || !sb.buffered || sb.buffered.length < 1) return false;

        const current = el.currentTime;

        // Playhead stranded BEFORE the first buffered range. Happens after an error
        // reset rebuilds the track: currentTime snaps back to 0 but the re-fed segments
        // begin at the current media time (~140s+), so there is no data at 0 and the
        // element can't even fire 'canplay'. Seek forward into the buffer so playback
        // (and the canplay→play path) can resume. The audio drift-sync then follows.
        const firstStart = sb.buffered.start(0);
        if (current < firstStart - 0.5) {
            logTerminal(`[MSE][${trackType}] playhead ${current.toFixed(2)}s before buffered start ${firstStart.toFixed(2)}s — seeking into buffer`);
            el.currentTime = firstStart + 0.01;
            return true;
        }

        if (sb.buffered.length < 2) return false;
        for (let i = 0; i < sb.buffered.length - 1; i++) {
            const rangeEnd = sb.buffered.end(i);
            const nextRangeStart = sb.buffered.start(i + 1);
            // Playhead is in the gap between range i and range i+1 (or near rangeEnd)
            if (current >= rangeEnd - 0.5 && current < nextRangeStart) {
                const gapSize = nextRangeStart - rangeEnd;
                logTerminal(`[MSE][${trackType}] Gap detected: ${rangeEnd.toFixed(2)}s → ${nextRangeStart.toFixed(2)}s (${gapSize.toFixed(2)}s) — jumping playhead`);
                el.currentTime = nextRangeStart + 0.01;
                return true;
            }
        }
        return false;
    }

    /**
     * Set up stall recovery for a media element:
     *   1. 'waiting' event handler — fires when the browser stalls at a gap.
     *   2. 3-second periodic watchdog — catches "soft stalls" where timeupdate
     *      stops firing but no waiting event is emitted.
     */
    function setupStallRecovery(el, trackType) {
        // Clean up any previous watchdog and listener (e.g. from a prior MSE session)
        if (stallWatchdogTimer) {
            clearInterval(stallWatchdogTimer);
            stallWatchdogTimer = null;
        }
        if (onWaitingHandler && el) {
            el.removeEventListener('waiting', onWaitingHandler);
            onWaitingHandler = null;
        }
        lastPlayheadTime = -1;
        lastPlayheadCheck = 0;

        onWaitingHandler = () => {
            if (playbackMode !== 'mse') return;
            logTerminal(`[MSE][${trackType}] 'waiting' event fired at ${el.currentTime.toFixed(2)}s — checking for gap`);
            tryJumpGap(el, trackType);
        };
        el.addEventListener('waiting', onWaitingHandler);

        // Periodic soft-stall watchdog: if the playhead hasn't moved for 3s
        // while the element thinks it's playing, attempt a gap jump.
        stallWatchdogTimer = setInterval(() => {
            if (playbackMode !== 'mse') return;
            if (el.paused || el.ended || el.seeking) return;

            const now = performance.now();
            const current = el.currentTime;

            if (lastPlayheadTime >= 0 && Math.abs(current - lastPlayheadTime) < 0.05) {
                // Playhead hasn't moved in ~3s while playing — stalled.
                if (now - lastPlayheadCheck >= 2900) {
                    logTerminal(`[MSE][${trackType}] Soft stall detected at ${current.toFixed(2)}s — attempting gap jump`);
                    if (!tryJumpGap(el, trackType)) {
                        // No gap found — try nudging forward slightly to unstick the decoder.
                        logTerminal(`[MSE][${trackType}] No gap found — nudging playhead +0.1s`);
                        el.currentTime = current + 0.1;
                    }
                }
            } else {
                lastPlayheadTime = current;
                lastPlayheadCheck = now;
            }
        }, 3000);
    }

    /**
     * Handle a single incoming media chunk (called from Wails event handler).
     */
    function handleMediaChunk(seq, trackType, mimeType, codec, sourceBufferID, isInitSegment, chunkB64, videoID) {
        // Media is still arriving, so any teardown scheduled by a just-fired
        // VIDEO_REMOVED was a false alarm (ad boundary / DOM reshuffle). Cancel
        // it. If this chunk belongs to a genuinely different video, the videoID
        // change check below tears the stale session down explicitly instead.
        cancelScheduledTeardown("media still flowing");
        lastChunkAt = performance.now();

        // If the video ID changes, tear down the stale session to prevent QuotaExceededError
        if ((mediaSources.video || mediaSources.audio) && videoID && videoID !== 'unknown' && activeVideoID && activeVideoID !== 'unknown' && videoID !== activeVideoID) {
            logTerminal(`[MSE] videoID changed from ${activeVideoID} to ${videoID} — tearing down stale session`);
            teardownMSE();
        }

        // Lazily set up MSE on first chunk for this track
        if (!mediaSources[trackType]) {
            setupTrackMSE(trackType);
            if (!activeVideoID || activeVideoID === 'unknown') {
                activeVideoID = videoID;
                logTerminal(`[MSE] Set activeVideoID to ${activeVideoID}`);
            }
        } else if (videoID && videoID !== 'unknown' && activeVideoID !== videoID) {
            // Update activeVideoID if it was previously unset or unknown
            activeVideoID = videoID;
            logTerminal(`[MSE] Sync activeVideoID to ${activeVideoID}`);
        }

        const data = base64ToUint8Array(chunkB64);
        const fullMime = mimeType.includes('codecs') ? mimeType : (codec ? `${mimeType}; codecs="${codec}"` : mimeType);

        if (isInitSegment) {
            mseInitCount++;
            logTerminal(`[MSE][${trackType}][${sourceBufferID}] Init segment #${mseInitCount} mimeType=${mimeType} codec=${codec} size=${data.length} videoId=${videoID}`);

            // Cache this init so resetTrack() can rebuild the track after a decode error
            // even though YouTube won't resend an init segment mid-stream.
            lastInitSegment[trackType] = { data: data.slice(), fullMime };

            // Flush prior queue for this track type
            chunkQueues[trackType] = [];

            if (mseReady[trackType]) {
                const sb = sourceBuffers[trackType];
                if (!sb) {
                    try {
                        const newSb = mediaSources[trackType].addSourceBuffer(fullMime);
                        newSb.mode = 'segments';
                        sourceBuffers[trackType] = newSb;
                        configuredMimeTypes[trackType] = fullMime;

                        newSb.addEventListener('updateend', () => {
                            drainQueue(trackType);
                        });
                        newSb.addEventListener('error', (e) => {
                            logErrorTerm(`[MSE][${trackType}] SourceBuffer error: ${e}`);
                        });

                        logTerminal(`[MSE][${trackType}] SourceBuffer created for ${fullMime}`);
                    } catch (e) {
                        logErrorTerm(`[MSE][${trackType}] Failed to add SourceBuffer for ${fullMime}: ${e}`);
                        return;
                    }
                } else if (configuredMimeTypes[trackType] !== fullMime) {
                    logTerminal(`[MSE][${trackType}] Codec change: ${configuredMimeTypes[trackType]} → ${fullMime}`);
                    try {
                        if (typeof sb.changeType === 'function') {
                            sb.changeType(fullMime);
                            configuredMimeTypes[trackType] = fullMime;
                        } else {
                            logErrorTerm(`[MSE][${trackType}] changeType not supported — keeping existing config`);
                        }
                    } catch (e) {
                        logErrorTerm(`[MSE][${trackType}] changeType failed: ${e}`);
                    }
                }
            } else {
                pendingSourceBuffers[trackType] = fullMime;
                chunkQueues[trackType].push(data);
                return;
            }
        }

        // Queue this chunk (init or media segment).
        if (!chunkQueues[trackType]) {
            chunkQueues[trackType] = [];
        }
        chunkQueues[trackType].push(data);

        // Drain as soon as the SourceBuffer is ready.
        if (mseReady[trackType] && sourceBuffers[trackType]) {
            drainQueue(trackType);
        }
    }

    // ── Wails event: onMediaChunk ─────────────────────────────────────────
    // Arguments: seq, trackType, mimeType, codec, sourceBufferId, isInitSegment, chunkB64, videoId
    window.runtime.EventsOn("onMediaChunk", (seq, trackType, mimeType, codec, sourceBufferID, isInitSegment, chunkB64, videoID) => {
        // Don't process MSE chunks while in WebRTC mode (e.g. Teams call active)
        if (playbackMode === 'webrtc') {
            return;
        }
        try {
            handleMediaChunk(seq, trackType, mimeType, codec, sourceBufferID, isInitSegment, chunkB64, videoID);
        } catch (e) {
            logErrorTerm(`[MSE] onMediaChunk handler error: ${e}`);
        }
    });

    // ── Wails event: onVideoLifecycle ────────────────────────────────────
    window.runtime.EventsOn("onVideoLifecycle", (evtType, videoID) => {
        logTerminal(`[Video] lifecycle evtType=${evtType} videoId=${videoID}`);
        if (evtType === "VIDEO_ADDED") {
            // A video (re)appeared — the stream is alive again, so abort any
            // teardown that a preceding VIDEO_REMOVED scheduled (ad boundary).
            cancelScheduledTeardown("VIDEO_ADDED");
            return;
        }
        if (evtType === "VIDEO_SOURCE_GONE") {
            // The tab is closed or has navigated away — nothing will ever feed
            // this pipeline again. Unlike VIDEO_REMOVED (which fires spuriously
            // at ad boundaries and DOM reshuffles) this is definitive, so tear
            // down immediately instead of through the debounce.
            logTerminal(`[Video] source gone (${videoID}) — tearing down immediately`);
            teardownMSE();
            return;
        }

        if (evtType === "VIDEO_REMOVED" && (!activeVideoID || videoID === activeVideoID)) {
            // Teardown ONLY if the active main video is removed, or if no active video was set yet.
            // This prevents hover/preview videos from tearing down the active main session.
            // Deferred through scheduleTeardown so a quick re-add (ad transition)
            // doesn't cause a full pipeline rebuild + rebuffer.
            scheduleTeardown(`VIDEO_REMOVED ${videoID}`);
        }
    });

    // ==========================================
    // Local WebRTC Loopback Mode (Teams / calls)
    // ==========================================
    window.runtime.EventsOn("onLocalLoopbackOffer", async (bridgeID, offerSdp) => {
        // Top-level try/catch — any unhandled error in this handler is caught
        // and logged via LogInfo (visible as INF | in the terminal). LogError
        // output is silently filtered by some Wails/WebKit configurations.
        try {
            // If MSE is active (YouTube playing), suspend it in favour of WebRTC
            if (playbackMode === 'mse') {
                logTerminal(`[Loopback][${bridgeID}] WebRTC offer received during MSE — suspending MSE`);
                teardownMSE();
            }

            isWebRTCLive = true;
            setMode('webrtc');

            logTerminal(`[Loopback] Received SDP Offer for bridgeId=${bridgeID} sdpLen=${offerSdp ? offerSdp.length : 'null'}`);

            if (!offerSdp || offerSdp.length === 0) {
                logTerminal(`[Loopback][${bridgeID}] ERROR: received empty/null offer SDP — aborting`);
                return;
            }

            if (!localPCs[bridgeID]) {
                logTerminal(`[Loopback][${bridgeID}] Creating new RTCPeerConnection`);
                const pc = new RTCPeerConnection({
                    iceServers: [] // Local loopback — both peers on same machine
                });

                pc.onsignalingstatechange = () => {
                    logTerminal(`[Loopback][${bridgeID}] Signaling state: ${pc.signalingState}`);
                };

                pc.oniceconnectionstatechange = () => {
                    logTerminal(`[Loopback][${bridgeID}] ICE connection state: ${pc.iceConnectionState}`);
                };

                pc.onicecandidate = (event) => {
                    if (event.candidate) {
                        logTerminal(`[Loopback][${bridgeID}] Local ICE candidate: ${event.candidate.candidate.substring(0, 80)}`);
                    } else {
                        logTerminal(`[Loopback][${bridgeID}] ICE gathering complete`);
                    }
                };

                pc.ontrack = (event) => {
                    logTerminal(`[Loopback][${bridgeID}] Track received: kind=${event.track.kind} id=${event.track.id} streams=${event.streams.length}`);
                    if (event.streams && event.streams[0]) {
                        if (videoElement.srcObject !== event.streams[0]) {
                            logTerminal(`[Loopback][${bridgeID}] Binding stream to video element`);
                            videoElement.srcObject = event.streams[0];
                        }
                        videoElement.play().catch(e => {
                            logTerminal(`[Loopback][${bridgeID}] play() blocked: ${e}`);
                        });
                    } else {
                        logTerminal(`[Loopback][${bridgeID}] ERROR: ontrack fired but event.streams[0] is missing`);
                    }
                };

                pc.onconnectionstatechange = () => {
                    logTerminal(`[Loopback][${bridgeID}] Connection state: ${pc.connectionState}`);
                    if (isWebRTCLive && (pc.connectionState === 'disconnected' || pc.connectionState === 'failed')) {
                        logTerminal(`[Loopback][${bridgeID}] Connection lost — hiding window`);
                        cleanupStatsForBridge(bridgeID);
                        window.go.main.App.HideWindow();
                        delete localPCs[bridgeID];
                        // Return to idle if no other WebRTC sessions
                        if (Object.keys(localPCs).length === 0) {
                            isWebRTCLive = false;
                            setMode('idle');
                        }
                    }
                    if (pc.connectionState === 'closed') {
                        cleanupStatsForBridge(bridgeID);
                        delete localPCs[bridgeID];
                        if (Object.keys(localPCs).length === 0) {
                            isWebRTCLive = false;
                            setMode('idle');
                        }
                    }
                };

                localPCs[bridgeID] = pc;
                logTerminal(`[Loopback][${bridgeID}] RTCPeerConnection created successfully`);
            }

            const pc = localPCs[bridgeID];

            // Guard: skip if this exact SDP was already applied
            if (pc.signalingState === 'stable' && pc.remoteDescription && pc.remoteDescription.sdp === offerSdp) {
                logTerminal(`[Loopback][${bridgeID}] Duplicate offer in stable state — ignoring`);
                return;
            }

            logTerminal(`[Loopback][${bridgeID}] Applying remote offer (signalingState=${pc.signalingState})...`);
            await pc.setRemoteDescription(new RTCSessionDescription({
                type: 'offer',
                sdp: offerSdp
            }));
            logTerminal(`[Loopback][${bridgeID}] Remote offer applied successfully`);

            window.go.main.App.ShowWindow();
            startStatsPolling(bridgeID, pc);

            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);

            logTerminal(`[Loopback][${bridgeID}] SDP Answer generated (len=${answer.sdp.length}), sending to Go backend...`);
            window.go.main.App.SetLoopbackAnswer(bridgeID, answer.sdp);
            logTerminal(`[Loopback][${bridgeID}] SDP Answer sent. Waiting for media...`);

        } catch (error) {
            // Use LogInfo so errors are VISIBLE in the terminal (LogError may be filtered)
            logTerminal(`[Loopback][${bridgeID}] FATAL ERROR in handler: ${error}`);
            if (error && error.stack) {
                logTerminal(`[Loopback][${bridgeID}] Stack: ${error.stack}`);
            }
        }
    });

    // ── Cold-start recovery (Bug-2 fix) ────────────────────────────────────
    // The Go backend may have fired the loopback offer before this EventsOn
    // listener was registered (race between Wails startup and Go SRTP readiness).
    // Calling RequestLoopbackOffer() tells Go to re-emit any cached offer SDPs
    // for sessions that are already live. The call is deferred by one tick to
    // ensure the EventsOn handler above is fully registered first.
    setTimeout(() => {
        if (window.go && window.go.main && window.go.main.App) {
            logTerminal("[Loopback] Requesting any pending loopback offers from Go backend...");
            window.go.main.App.RequestLoopbackOffer().catch(e => {
                logErrorTerm(`[Loopback] RequestLoopbackOffer failed: ${e}`);
            });
        }
    }, 0);

    // ── Playback reporter (drives the VDI-side YouTube seek pulse) ───────────
    // The VDI page can't see how much media is really buffered (its <video> is gated
    // and empty). OUR MSE buffer is the ground truth, so report bufferedEnd + playhead
    // back to the interceptor ~1×/sec; it seeks YouTube just behind bufferedEnd to keep
    // fetching the next contiguous region. Only meaningful while MSE (YouTube) is active.
    setInterval(() => {
        if (playbackMode !== 'mse') return;
        try {
            const b = videoElement.buffered;
            if (!b || b.length === 0) return;
            const playhead = videoElement.currentTime || 0;

            // Report the end of the CONTIGUOUS buffered region at/ahead of the playhead —
            // NOT b.end(last). With a fragmented buffer (e.g. after a seek that leaves a
            // gap), the last range's end overstates the real playable frontier, so the
            // seek pulse thinks there's plenty buffered and stops fetching — starving
            // playback at the gap. Walk from the first range ending after the playhead and
            // extend across near-adjacent ranges (sub-1s gaps the watchdog auto-jumps).
            let contiguousEnd = playhead;
            for (let i = 0; i < b.length; i++) {
                if (b.end(i) <= playhead) continue; // range entirely behind the playhead
                contiguousEnd = b.end(i);
                let j = i;
                while (j + 1 < b.length && b.start(j + 1) - b.end(j) < 1.0) {
                    contiguousEnd = b.end(j + 1);
                    j++;
                }
                break;
            }

            if (window.go && window.go.main && window.go.main.App && window.go.main.App.ReportPlayback) {
                window.go.main.App.ReportPlayback(contiguousEnd, playhead).catch(() => {});
            }

            // ── Starvation watchdog ────────────────────────────────────────
            // Backstop for a source that dies without any clean signal: the VDI
            // browser crashing, the extension being unloaded, or the bridge
            // dropping. In those cases we play out whatever is buffered and then
            // sit frozen forever, holding a stale overlay over the session.
            //
            // Requires BOTH an exhausted buffer and no recent chunks, so normal
            // pulse-decode gaps (which leave tens of seconds buffered) cannot
            // trigger it. Also covers reaching the natural end of the video.
            if (lastChunkAt > 0 &&
                (contiguousEnd - playhead) < 0.5 &&
                (performance.now() - lastChunkAt) > MEDIA_STARVATION_MS) {
                logTerminal(
                    `[MSE] media starved — buffer exhausted at ${playhead.toFixed(2)}s and ` +
                    `no chunks for ${Math.round((performance.now() - lastChunkAt) / 1000)}s; tearing down`
                );
                lastChunkAt = 0; // don't re-fire while teardown runs
                teardownMSE();
                if (window.go && window.go.main && window.go.main.App && window.go.main.App.NotifyMediaStarved) {
                    window.go.main.App.NotifyMediaStarved().catch(() => {});
                }
            }
        } catch (_) {}
    }, 1000);

    logTerminal("BCR Player UI ready (MSE + WebRTC loopback, codec-pinned: H264+Opus). Waiting for media...");
};
