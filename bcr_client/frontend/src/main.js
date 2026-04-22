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

    // ── Video Element Optimization ──────────────────────────────────────────
    // Pre-configure the video element for low-latency WebRTC playback.
    // With codec pinning (VP8 + Opus only), we know exactly what media will
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

            pc.ontrack = (event) => {
                logTerminal(`[Loopback] Stream track received! track.kind=${event.track.kind} codec-pinned=VP8+Opus`);
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
                    window.go.main.App.HideWindow();
                    delete localPCs[bridgeID];
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
            
            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            
            // Send answer back to Go backend
            window.go.main.App.SetLoopbackAnswer(bridgeID, answer.sdp);
            logTerminal(`[Loopback] SDP Answer generated and sent back to Go backend.`);
        } catch (error) {
            logErrorTerm(`[Loopback] WebRTC Negotiation Failed: ${error}`);
        }
    });

    logTerminal("WebRTC Loopback UI ready (codec-pinned: VP8+Opus). Waiting for payloads...");
};
