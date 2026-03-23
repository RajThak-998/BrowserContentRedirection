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

    // Stream state tracking for dynamic window visibility
    let streamTimeout = null;
    const IDLE_HIDE_MS = 5000; // Hide the window if no data is played for 5 seconds

    function resetMSEState() {
        if (mediaSource && mediaSource.readyState === "open") {
            try {
                mediaSource.endOfStream();
            } catch (_) {
                // no-op
            }
        }

        if (videoElement.src) {
            try {
                URL.revokeObjectURL(videoElement.src);
            } catch (_) {
                // no-op
            }
        }

        videoElement.src = "";
        videoElement.srcObject = null;

        mediaSource = null;
        videoSourceBuffer = null;
        audioSourceBuffer = null;
        videoQueue = [];
        audioQueue = [];
        videoAppending = false;
        audioAppending = false;
        hasVideoInit = false;
        hasAudioInit = false;
        pendingVideoType = null;
        pendingAudioType = null;
    }

    function resetIdleTimer() {
        if (streamTimeout) clearTimeout(streamTimeout);
        streamTimeout = setTimeout(() => {
            logTerminal("Stream idle for 5 seconds. Hiding window.");
            window.go.main.App.HideWindow();

            // Clean up players fully on hide so they can restart cleanly.
            resetMSEState();
        }, IDLE_HIDE_MS);
    }

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
    // 1. WebRTC Mode (Default/Live Streams)
    // ==========================================
    window.runtime.EventsOn("onSdpOffer", async (offerSdp) => {
        logTerminal("Received WebRTC SDP Offer. Showing Window...");
        window.go.main.App.ShowWindow();
        resetIdleTimer();

        // Clear any MSE or existing streams
        if (videoElement.src) {
            URL.revokeObjectURL(videoElement.src);
            videoElement.removeAttribute('src');
        }

        const pc = new RTCPeerConnection({
            iceServers: [] // Add STUN/TURN servers here if not on a local network
        });

        pc.ontrack = (event) => {
            logTerminal("WebRTC Stream track received!");
            if (videoElement.srcObject !== event.streams[0]) {
                videoElement.srcObject = event.streams[0];
            }
            // Keep timer alive on connection state change
            pc.onconnectionstatechange = () => {
                if (pc.connectionState === 'connected') {
                    resetIdleTimer();
                    // In a real RTC setup, you might want to ping over data channel or check packets to keep alive
                } else if (pc.connectionState === 'disconnected' || pc.connectionState === 'failed') {
                    if (streamTimeout) clearTimeout(streamTimeout);
                    window.go.main.App.HideWindow();
                }
            };
        };

        try {
            await pc.setRemoteDescription(new RTCSessionDescription({
                type: 'offer',
                sdp: offerSdp
            }));
            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            window.go.main.App.SendSdpAnswer(answer.sdp);
            console.log("WebRTC Answer sent.");
        } catch (error) {
            console.error("WebRTC Negotiation Failed:", error);
        }
    });

    // ==========================================
    // 2. MSE Mode (Byte Chunks via WebSocket)
    // ==========================================
    let mediaSource = null;

    let videoSourceBuffer = null;
    let audioSourceBuffer = null;

    let videoQueue = [];
    let audioQueue = [];

    let videoAppending = false;
    let audioAppending = false;

    let hasVideoInit = false;
    let hasAudioInit = false;

    let pendingVideoType = null;
    let pendingAudioType = null;

    // Helper to decode Base64 strings sent from Go WebSocket into Uint8Arrays for MSE
    function buildTypeString(mimeType, codec) {
        const mime = (mimeType || "unknown").trim();
        const c = (codec || "unknown").trim();

        if (mime.includes("codecs=")) return mime;
        if (mime === "unknown" || c === "unknown") return mime;

        return `${mime}; codecs="${c}"`;
    }

    function base64ToUint8Array(base64) {
        const binaryString = window.atob(base64);
        const len = binaryString.length;
        const bytes = new Uint8Array(len);
        for (let i = 0; i < len; i++) bytes[i] = binaryString.charCodeAt(i);
        return bytes;
    }

    // Parses BCR framed chunk: [u32 LE headerLen][headerJSON][rawChunk]
    // Falls back to legacy raw chunk format.
    function parseIncomingChunk(base64Chunk) {
        const allBytes = base64ToUint8Array(base64Chunk);

        if (allBytes.length >= 4) {
            const dv = new DataView(allBytes.buffer, allBytes.byteOffset, allBytes.byteLength);
            const headerLen = dv.getUint32(0, true);

            if (headerLen > 0 && 4 + headerLen <= allBytes.length) {
                try {
                    const headerBytes = allBytes.slice(4, 4 + headerLen);
                    const headerJson = new TextDecoder().decode(headerBytes);
                    const hdr = JSON.parse(headerJson);

                    if (hdr?.type === "MEDIA_CHUNK" && hdr?.payload) {
                        const payload = hdr.payload;
                        const chunk = allBytes.slice(4 + headerLen);

                        return {
                            trackType: payload.trackType || "unknown",
                            mimeType: payload.mimeType || "unknown",
                            codec: payload.codec || "unknown",
                            isInitSegment: payload.isInitSegment === true,
                            chunk,
                        };
                    }
                } catch (_) {
                    // fall through to legacy raw mode
                }
            }
        }

        // Legacy sender mode: no metadata available
        return {
            trackType: "unknown",
            mimeType: "unknown",
            codec: "unknown",
            isInitSegment: false,
            chunk: allBytes,
        };
    }

    // Initialize the MediaSource when the first chunk arrives
    function initMSE() {
        if (mediaSource) return;

        logTerminal("Initializing Media Source Extensions (MSE) for Byte Chunks...");

        if (videoElement.srcObject) {
            videoElement.srcObject = null;
        }

        mediaSource = new MediaSource();
        videoElement.src = URL.createObjectURL(mediaSource);

        mediaSource.addEventListener("sourceopen", () => {
            logTerminal("MediaSource opened.");
            maybeCreateBuffers();
            processTrackQueue("video");
            processTrackQueue("audio");
        });

        mediaSource.addEventListener("error", (e) => {
            logErrorTerm("MediaSource Error: " + e);
        });
    }

    function maybeCreateBufferForTrack(trackType, mimeType, codec) {
        const typeString = buildTypeString(mimeType, codec);

        if (trackType === "video") {
            if (videoSourceBuffer) return;
            if (typeString === "unknown") return;

            if (!MediaSource.isTypeSupported(typeString)) {
                logErrorTerm(`[MSE] Unsupported type: ${typeString}`);
                return;
            }

            try {
                videoSourceBuffer = mediaSource.addSourceBuffer(typeString);
                logTerminal(`[MSE] video buffer created: ${typeString}`);

                videoSourceBuffer.addEventListener("updateend", () => {
                    videoAppending = false;
                    resetIdleTimer();
                    processTrackQueue("video");

                    if (videoElement.paused && videoElement.readyState >= 3) {
                        videoElement.play().catch(e => logErrorTerm("Play error: " + e));
                    }
                });

                videoSourceBuffer.addEventListener("error", () => {
                    logErrorTerm("Video SourceBuffer error.");
                });
            } catch (e) {
                logErrorTerm(`[MSE] Failed to create video buffer: ${e}`);
            }
            return;
        }

        if (trackType === "audio") {
            if (audioSourceBuffer) return;
            if (typeString === "unknown") return;

            if (!MediaSource.isTypeSupported(typeString)) {
                logErrorTerm(`[MSE] Unsupported type: ${typeString}`);
                return;
            }

            try {
                audioSourceBuffer = mediaSource.addSourceBuffer(typeString);
                logTerminal(`[MSE] audio buffer created: ${typeString}`);

                audioSourceBuffer.addEventListener("updateend", () => {
                    audioAppending = false;
                    resetIdleTimer();
                    processTrackQueue("audio");
                });

                audioSourceBuffer.addEventListener("error", () => {
                    logErrorTerm("Audio SourceBuffer error.");
                });
            } catch (e) {
                logErrorTerm(`[MSE] Failed to create audio buffer: ${e}`);
            }
        }
    }

    function maybeCreateBuffers() {
        if (pendingVideoType) {
            maybeCreateBufferForTrack("video", pendingVideoType.mimeType, pendingVideoType.codec);
        }
        if (pendingAudioType) {
            maybeCreateBufferForTrack("audio", pendingAudioType.mimeType, pendingAudioType.codec);
        }
    }

    function processTrackQueue(trackType) {
        const isVideo = trackType === "video";
        const sb = isVideo ? videoSourceBuffer : audioSourceBuffer;
        const queue = isVideo ? videoQueue : audioQueue;
        const appending = isVideo ? videoAppending : audioAppending;
        const hasInit = isVideo ? hasVideoInit : hasAudioInit;

        if (!sb || sb.updating || appending || queue.length === 0) return;

        const next = queue[0];

        // Do not append media before init for this track
        if (!hasInit && !next.isInitSegment) return;

        queue.shift();

        if (isVideo) videoAppending = true;
        else audioAppending = true;

        try {
            sb.appendBuffer(next.chunk);
            if (isVideo) {
                logTerminal(`[MSE] append video size=${next.chunk.byteLength}`);
            } else {
                logTerminal(`[MSE] append audio size=${next.chunk.byteLength}`);
            }
        } catch (e) {
            if (isVideo) videoAppending = false;
            else audioAppending = false;
            logErrorTerm(`Error appending ${trackType} buffer: ${e}`);
        }
    }

    window.runtime.EventsOn("onVideoChunk", (base64Chunk) => {
        window.go.main.App.ShowWindow();
        resetIdleTimer();

        if (!mediaSource) {
            initMSE();
        }

        const parsed = parseIncomingChunk(base64Chunk);
        const { trackType, mimeType, codec, isInitSegment, chunk } = parsed;

        // Track discovered types for buffer creation when sourceopen fires
        if (trackType === "video" && !pendingVideoType) {
            pendingVideoType = { mimeType, codec };
        }
        if (trackType === "audio" && !pendingAudioType) {
            pendingAudioType = { mimeType, codec };
        }

        if (mediaSource && mediaSource.readyState === "open") {
            maybeCreateBuffers();
        }

        if (trackType === "video") {
            if (isInitSegment && !hasVideoInit) {
                hasVideoInit = true;
                logTerminal("[MSE] init segment received (video)");
            }
            videoQueue.push({ chunk, isInitSegment });
            processTrackQueue("video");
            return;
        }

        if (trackType === "audio") {
            if (isInitSegment && !hasAudioInit) {
                hasAudioInit = true;
                logTerminal("[MSE] init segment received (audio)");
            }
            audioQueue.push({ chunk, isInitSegment });
            processTrackQueue("audio");
            return;
        }

        // Unknown track fallback: keep old behavior but warn
        logErrorTerm("[MSE] Unknown trackType; dropping chunk");
    });

    logTerminal("WebRTC/MSE UI ready. Waiting for payloads...");
};
