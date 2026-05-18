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

    // ── Video Element Optimization ──────────────────────────────────────────
    // Pre-configure the video element for low-latency WebRTC playback.
    // With codec pinning (H264 + Opus only), we know exactly what media will
    // arrive and can optimize accordingly.
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
    // Local WebRTC Loopback Mode
    // ==========================================
    window.runtime.EventsOn("onLocalLoopbackOffer", async (bridgeID, offerSdp) => {
        // Top-level try/catch — any unhandled error in this handler is caught
        // and logged via LogInfo (visible as INF | in the terminal). LogError
        // output is silently filtered by some Wails/WebKit configurations.
        try {
            isWebRTCLive = true;
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
                    }
                    if (pc.connectionState === 'closed') {
                        cleanupStatsForBridge(bridgeID);
                        delete localPCs[bridgeID];
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

    logTerminal("WebRTC Loopback UI ready (codec-pinned: H264+Opus). Waiting for payloads...");
};
