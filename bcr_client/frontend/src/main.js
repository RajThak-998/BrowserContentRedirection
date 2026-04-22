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
        isWebRTCLive = true;
        logTerminal(`[Loopback] Received SDP Offer for bridgeId=${bridgeID}. Showing Window...`);
        window.go.main.App.ShowWindow();

        if (!localPCs[bridgeID]) {
            logTerminal(`[Loopback] Creating new RTCPeerConnection for bridgeId=${bridgeID}`);
            const pc = new RTCPeerConnection({
                iceServers: [] // Local loopback, no ICE servers needed
            });

            pc.onsignalingstatechange = () => {
                logTerminal(`[Loopback] Signaling state changed to: ${pc.signalingState}`);
            };

            pc.oniceconnectionstatechange = () => {
                logTerminal(`[Loopback] ICE connection state changed to: ${pc.iceConnectionState}`);
            };

            pc.ontrack = (event) => {
                logTerminal(`[Loopback] Stream track received! track.kind=${event.track.kind} codec-pinned=H264+Opus`);
                if (videoElement.srcObject !== event.streams[0]) {
                    videoElement.srcObject = event.streams[0];
                    // Ensure playback starts immediately with the pinned codec stream
                    videoElement.play().catch(e => {
                        logTerminal(`[Loopback] Autoplay blocked, will retry: ${e}`);
                    });
                }
            };

            pc.onconnectionstatechange = () => {
                logTerminal(`[Loopback] Connection state changed to: ${pc.connectionState}`);
                if (isWebRTCLive && (pc.connectionState === 'disconnected' || pc.connectionState === 'failed')) {
                    cleanupStatsForBridge(bridgeID);
                    window.go.main.App.HideWindow();
                    delete localPCs[bridgeID];
                }

                if (pc.connectionState === 'closed') {
                    cleanupStatsForBridge(bridgeID);
                }
            };

            localPCs[bridgeID] = pc;
        }

        const pc = localPCs[bridgeID];

        try {
            await pc.setRemoteDescription(new RTCSessionDescription({
                type: 'offer',
                sdp: offerSdp
            }));

            startStatsPolling(bridgeID, pc);
            collectAndLogReceiverStats(bridgeID, pc);
            
            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            
            // Send answer back to Go backend
            window.go.main.App.SetLoopbackAnswer(bridgeID, answer.sdp);
            logTerminal(`[Loopback] SDP Answer generated and sent back to Go backend.`);
        } catch (error) {
            logErrorTerm(`[Loopback] WebRTC Negotiation Failed: ${error}`);
        }
    });

    logTerminal("WebRTC Loopback UI ready (codec-pinned: H264+Opus). Waiting for payloads...");
};
