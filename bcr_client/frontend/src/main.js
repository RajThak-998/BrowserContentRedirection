// Wait for Wails to inject runtime bridging
window.onload = function () {
    const videoElement = document.getElementById("videoPlayer");

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

    // ==========================================
    // MSE Player (YouTube video redirection)
    // ==========================================

    let mediaSource = null;
    let sourceBuffers = {};        // keyed by sourceBufferId
    let chunkQueues = {};          // queued chunks waiting while SourceBuffer is updating
    let mseReady = false;
    let mseInitCount = 0;          // how many init segments received — used for first-chunk logging

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
    function teardownMSE() {
        if (!mediaSource) return;
        logTerminal("[MSE] Tearing down MSE pipeline");

        try {
            Object.keys(sourceBuffers).forEach((id) => {
                try {
                    const sb = sourceBuffers[id];
                    if (!sb.updating) {
                        mediaSource.removeSourceBuffer(sb);
                    }
                } catch (e) { /* ignore */ }
            });
            if (mediaSource.readyState === 'open') {
                mediaSource.endOfStream();
            }
        } catch (e) { /* ignore */ }

        sourceBuffers = {};
        chunkQueues = {};
        mediaSource = null;
        mseReady = false;
        mseInitCount = 0;

        // Revoke old object URL and clear srcObject so the video element resets
        if (videoElement.src && videoElement.src.startsWith('blob:')) {
            URL.revokeObjectURL(videoElement.src);
        }
        videoElement.src = '';
        videoElement.removeAttribute('src');
        videoElement.srcObject = null;

        setMode('idle');
        if (window.go && window.go.main && window.go.main.App) {
            window.go.main.App.NotifyMSEActive(false).catch(() => {});
        }
        logTerminal("[MSE] Pipeline torn down");
    }

    /**
     * Create and attach a fresh MediaSource to the video element. Called on
     * the first MEDIA_CHUNK that arrives while in idle or MSE mode.
     */
    function setupMSE() {
        if (mediaSource) return; // already set up

        // Detach any existing WebRTC stream
        if (videoElement.srcObject) {
            logTerminal("[MSE] Detaching WebRTC srcObject to switch to MSE mode");
            videoElement.srcObject = null;
        }

        mediaSource = new MediaSource();
        videoElement.src = URL.createObjectURL(mediaSource);

        mediaSource.addEventListener('sourceopen', () => {
            logTerminal("[MSE] MediaSource sourceopen — pipeline ready");
            mseReady = true;
            // Flush any queued chunks that arrived before sourceopen
            Object.keys(chunkQueues).forEach((id) => {
                drainQueue(id);
            });
        });

        mediaSource.addEventListener('sourceclose', () => {
            logTerminal("[MSE] MediaSource sourceclose");
            mseReady = false;
        });

        mediaSource.addEventListener('sourceended', () => {
            logTerminal("[MSE] MediaSource sourceended");
        });

        setMode('mse');
        if (window.go && window.go.main && window.go.main.App) {
            window.go.main.App.NotifyMSEActive(true).catch(() => {});
        }
        logTerminal("[MSE] Pipeline set up — waiting for sourceopen");
    }

    /**
     * Drain the pending chunk queue for a given SourceBuffer ID.
     * Respects the SourceBuffer.updating flag to avoid InvalidStateError.
     */
    function drainQueue(sbID) {
        const sb = sourceBuffers[sbID];
        const queue = chunkQueues[sbID];

        if (!sb || !queue || queue.length === 0) return;
        if (sb.updating) return; // will be called again from updateend

        const chunk = queue.shift();
        try {
            sb.appendBuffer(chunk);
        } catch (e) {
            logErrorTerm(`[MSE][${sbID}] appendBuffer error: ${e}`);
            // On quota exceeded, clear old buffered range
            if (e.name === 'QuotaExceededError' && sb.buffered.length > 0) {
                const removeEnd = sb.buffered.end(0) - 30; // keep last 30s
                if (removeEnd > sb.buffered.start(0)) {
                    try { sb.remove(sb.buffered.start(0), removeEnd); } catch (_) {}
                }
                // Re-add chunk after removal
                queue.unshift(chunk);
            }
        }
    }

    /**
     * Handle a single incoming media chunk (called from Wails event handler).
     */
    function handleMediaChunk(seq, trackType, mimeType, codec, sourceBufferID, isInitSegment, chunkB64) {
        // Lazily set up MSE on first chunk
        if (!mediaSource) {
            setupMSE();
        }

        const data = base64ToUint8Array(chunkB64);

        if (isInitSegment) {
            mseInitCount++;
            logTerminal(`[MSE][${sourceBufferID}] Init segment #${mseInitCount} mimeType=${mimeType} codec=${codec} size=${data.length}`);

            // Create SourceBuffer if this is the first init for this track
            if (!sourceBuffers[sourceBufferID] && mseReady) {
                const fullMime = codec ? `${mimeType}; codecs="${codec}"` : mimeType;
                try {
                    const sb = mediaSource.addSourceBuffer(fullMime);
                    sb.mode = 'segments';
                    sourceBuffers[sourceBufferID] = sb;
                    chunkQueues[sourceBufferID] = [];

                    sb.addEventListener('updateend', () => {
                        drainQueue(sourceBufferID);
                    });
                    sb.addEventListener('error', (e) => {
                        logErrorTerm(`[MSE][${sourceBufferID}] SourceBuffer error: ${e}`);
                    });

                    logTerminal(`[MSE][${sourceBufferID}] SourceBuffer created for ${fullMime}`);
                } catch (e) {
                    logErrorTerm(`[MSE][${sourceBufferID}] Failed to add SourceBuffer for ${fullMime}: ${e}`);
                    return;
                }
            } else if (!mseReady) {
                // sourceopen not yet fired — queue it
                if (!chunkQueues[sourceBufferID]) {
                    chunkQueues[sourceBufferID] = [];
                }
                chunkQueues[sourceBufferID].unshift(data); // prepend init
                return;
            }
        }

        // Queue the chunk
        if (!chunkQueues[sourceBufferID]) {
            chunkQueues[sourceBufferID] = [];
        }
        chunkQueues[sourceBufferID].push(data);

        // Start draining if MSE is ready and SourceBuffer exists
        if (mseReady && sourceBuffers[sourceBufferID]) {
            drainQueue(sourceBufferID);
        }

        // Attempt autoplay on first media chunk
        if (seq === 0 || (isInitSegment && mseInitCount === 1)) {
            videoElement.play().catch((e) => {
                logTerminal(`[MSE] play() blocked (autoplay policy?): ${e}`);
            });
        }
    }

    // ── Wails event: onMediaChunk ─────────────────────────────────────────
    // Arguments: seq, trackType, mimeType, codec, sourceBufferId, isInitSegment, chunkB64
    window.runtime.EventsOn("onMediaChunk", (seq, trackType, mimeType, codec, sourceBufferID, isInitSegment, chunkB64) => {
        // Don't process MSE chunks while in WebRTC mode (e.g. Teams call active)
        if (playbackMode === 'webrtc') {
            return;
        }
        try {
            handleMediaChunk(seq, trackType, mimeType, codec, sourceBufferID, isInitSegment, chunkB64);
        } catch (e) {
            logErrorTerm(`[MSE] onMediaChunk handler error: ${e}`);
        }
    });

    // ── Wails event: onVideoLifecycle ────────────────────────────────────
    window.runtime.EventsOn("onVideoLifecycle", (evtType, videoID) => {
        logTerminal(`[Video] lifecycle evtType=${evtType} videoId=${videoID}`);
        if (evtType === "VIDEO_REMOVED" && playbackMode === 'mse') {
            teardownMSE();
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

    logTerminal("BCR Player UI ready (MSE + WebRTC loopback, codec-pinned: H264+Opus). Waiting for media...");
};
