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
    // YouTube MSE sends chunks in bursts with 5-6s natural gaps between them.
    // Use a longer timeout to avoid false idle detection mid-stream.
    const IDLE_HIDE_MS = 30000; // 30 seconds — safe for bursty chunk delivery
    window.__lastChunkReceivedTime = window.__lastChunkReceivedTime || Date.now();
    let idleHidden = false;
    let lastActivityLogTime = 0;
    let isWebRTCLive = false;

    function markChunkReceived() {
        const now = Date.now();
        window.__lastChunkReceivedTime = now;

        // Sampled activity log to avoid console flood during high-throughput chunks.
        if (now - lastActivityLogTime >= 1000) {
            logTerminal("[Stream] activity detected");
            lastActivityLogTime = now;
        }

        // Re-show window if it was idle-hidden
        if (idleHidden) {
            logTerminal("[Stream] resuming from idle — showing window");
            window.go.main.App.ShowWindow();
        }
        idleHidden = false;
    }

    if (window.__streamIdleIntervalId) {
        clearInterval(window.__streamIdleIntervalId);
    }

    window.__streamIdleIntervalId = setInterval(() => {
        const idleMs = Date.now() - window.__lastChunkReceivedTime;
        if (!idleHidden && idleMs >= IDLE_HIDE_MS) {
            logTerminal("[Stream] idle detected");
            logTerminal(`Stream idle for ${IDLE_HIDE_MS / 1000}s. Hiding window.`);
            window.go.main.App.HideWindow();
            idleHidden = true;
            // DO NOT reset MSE state on idle — preserve the pipeline so playback
            // resumes seamlessly when chunks arrive again. Only reset when the
            // stream format actually changes or on explicit user action.
        }
    }, 1000);

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
        isWebRTCLive = true;
        logTerminal("Received WebRTC SDP Offer. Showing Window...");
        window.go.main.App.ShowWindow();

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
                if (isWebRTCLive && (pc.connectionState === 'disconnected' || pc.connectionState === 'failed')) {
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
    let currentVideoType = null;
    let currentAudioType = null;
    let lastVideoSeq = 0;
    let lastAudioSeq = 0;

    let waitingVideoInitLogged = false;
    let waitingAudioInitLogged = false;

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

    function readBufferedRanges(sb) {
        if (!sb || !sb.buffered || sb.buffered.length === 0) {
            return "none";
        }

        const ranges = [];
        for (let i = 0; i < sb.buffered.length; i++) {
            ranges.push(`${sb.buffered.start(i).toFixed(2)}-${sb.buffered.end(i).toFixed(2)}`);
        }
        return ranges.join(",");
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
                            seq: Number.isFinite(payload.seq) ? payload.seq : null,
                            ts: Number.isFinite(payload.ts) ? payload.ts : null,
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
            seq: null,
            ts: null,
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
        videoElement.autoplay = true;
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
                currentVideoType = typeString;
                logTerminal(`[MSE] video buffer created: ${typeString}`);

                videoSourceBuffer.addEventListener("updateend", () => {
                    videoAppending = false;
                    logTerminal("[MSE] append success track=video");
                    logTerminal(`[MSE] buffered track=video ranges=${readBufferedRanges(videoSourceBuffer)}`);
                    logTerminal(`[MSE] queue drain track=video remaining=${videoQueue.length}`);
                    processTrackQueue("video");

                    if (videoElement.paused && videoElement.readyState >= 2) {
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
                currentAudioType = typeString;
                logTerminal(`[MSE] audio buffer created: ${typeString}`);

                audioSourceBuffer.addEventListener("updateend", () => {
                    audioAppending = false;
                    logTerminal("[MSE] append success track=audio");
                    logTerminal(`[MSE] buffered track=audio ranges=${readBufferedRanges(audioSourceBuffer)}`);
                    logTerminal(`[MSE] queue drain track=audio remaining=${audioQueue.length}`);
                    processTrackQueue("audio");

                    if (videoElement.paused && videoElement.readyState >= 2) {
                        videoElement.play().catch(e => logErrorTerm("Play error: " + e));
                    }
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

        if (!sb) return;
        if (sb.updating) {
            logTerminal(`[MSE] append skipped updating=true track=${trackType}`);
            return;
        }
        if (appending || queue.length === 0) return;

        const next = queue[0];

        // Do not append media before init for this track
        if (!hasInit && !next.isInitSegment) {
            if (isVideo && !waitingVideoInitLogged) {
                logTerminal("[MSE] waiting for init (video)");
                waitingVideoInitLogged = true;
            }
            if (!isVideo && !waitingAudioInitLogged) {
                logTerminal("[MSE] waiting for init (audio)");
                waitingAudioInitLogged = true;
            }
            return;
        }

        queue.shift();

        if (isVideo) videoAppending = true;
        else audioAppending = true;

        try {
            logTerminal("[MSE] append scheduled");
            sb.appendBuffer(next.chunk);
            if (isVideo) {
                logTerminal(`[MSE] append video size=${next.chunk.byteLength}`);
            } else {
                logTerminal(`[MSE] append audio size=${next.chunk.byteLength}`);
            }
        } catch (e) {
            if (e.name === "QuotaExceededError") {
                logErrorTerm(`[MSE] QuotaExceeded track=${trackType} — evicting old buffered data`);
                try {
                    if (sb.buffered.length > 0) {
                        const evictStart = sb.buffered.start(0);
                        const evictEnd = Math.min(sb.buffered.start(0) + 10, sb.buffered.end(0));
                        sb.remove(evictStart, evictEnd);
                    }
                    // Re-queue the chunk so it retries after eviction completes
                    queue.unshift({ chunk: next.chunk, isInitSegment: next.isInitSegment });
                } catch (removeErr) {
                    logErrorTerm(`[MSE] buffer eviction failed: ${removeErr}`);
                }
                if (isVideo) videoAppending = false;
                else audioAppending = false;
                return;
            }
            if (isVideo) videoAppending = false;
            else audioAppending = false;
            logErrorTerm(`Error appending ${trackType} buffer: ${e}`);
        }
    }

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
        currentVideoType = null;
        currentAudioType = null;
        waitingVideoInitLogged = false;
        waitingAudioInitLogged = false;
        lastVideoSeq = 0;
        lastAudioSeq = 0;
    }

    window.runtime.EventsOn("onVideoChunk", (base64Chunk) => {
        isWebRTCLive = false;
        // Idle detection is based on incoming data, not append/playback state.
        markChunkReceived();
        window.go.main.App.ShowWindow();

        if (!mediaSource) {
            initMSE();
        }

        const parsed = parseIncomingChunk(base64Chunk);
        const { trackType, mimeType, codec, seq, isInitSegment, chunk } = parsed;
        const incomingType = buildTypeString(mimeType, codec);

        // Enhanced diagnostic logging
        if (isInitSegment) {
            logTerminal(`[MSE-DIAG] INIT RECEIVED track=${trackType} mime=${mimeType} codec=${codec} seq=${seq} size=${chunk.byteLength}`);
        }
        if (Number.isFinite(seq) && seq % 50 === 0) {
            logTerminal(`[MSE-DIAG] STATUS track=${trackType} seq=${seq} vBuf=${readBufferedRanges(videoSourceBuffer)} aBuf=${readBufferedRanges(audioSourceBuffer)} vInit=${hasVideoInit} aInit=${hasAudioInit} vQ=${videoQueue.length} aQ=${audioQueue.length}`);
        }

        if (trackType === "video" && Number.isFinite(seq)) {
            if (lastVideoSeq !== 0) {
                if (seq === lastVideoSeq) {
                    logErrorTerm(`[MSE] seq duplicate track=video seq=${seq}`);
                } else if (seq < lastVideoSeq) {
                    logErrorTerm(`[MSE] seq out_of_order track=video prev=${lastVideoSeq} current=${seq}`);
                } else if (seq > lastVideoSeq + 1) {
                    logErrorTerm(`[MSE] seq gap track=video prev=${lastVideoSeq} current=${seq}`);
                }
            }
            if (seq > lastVideoSeq) lastVideoSeq = seq;
        }

        if (trackType === "audio" && Number.isFinite(seq)) {
            if (lastAudioSeq !== 0) {
                if (seq === lastAudioSeq) {
                    logErrorTerm(`[MSE] seq duplicate track=audio seq=${seq}`);
                } else if (seq < lastAudioSeq) {
                    logErrorTerm(`[MSE] seq out_of_order track=audio prev=${lastAudioSeq} current=${seq}`);
                } else if (seq > lastAudioSeq + 1) {
                    logErrorTerm(`[MSE] seq gap track=audio prev=${lastAudioSeq} current=${seq}`);
                }
            }
            if (seq > lastAudioSeq) lastAudioSeq = seq;
        }

        const videoFormatChanged =
            trackType === "video" &&
            currentVideoType &&
            incomingType !== "unknown" &&
            incomingType !== currentVideoType;

        const audioFormatChanged =
            trackType === "audio" &&
            currentAudioType &&
            incomingType !== "unknown" &&
            incomingType !== currentAudioType;

        if (videoFormatChanged || audioFormatChanged) {
            logTerminal("[MSE] FORMAT CHANGE detected -> resetting pipeline");
            resetMSEState();

            if (trackType === "video") {
                pendingVideoType = { mimeType, codec };
            } else if (trackType === "audio") {
                pendingAudioType = { mimeType, codec };
            }

            initMSE();
        }

        // Track discovered types for buffer creation
        if (
            trackType === "video" &&
            (!pendingVideoType || pendingVideoType.mimeType !== mimeType || pendingVideoType.codec !== codec)
        ) {
            pendingVideoType = { mimeType, codec };
            if (mediaSource && mediaSource.readyState === "open") {
                maybeCreateBufferForTrack("video", mimeType, codec);
            }
        }
        if (
            trackType === "audio" &&
            (!pendingAudioType || pendingAudioType.mimeType !== mimeType || pendingAudioType.codec !== codec)
        ) {
            pendingAudioType = { mimeType, codec };
            if (mediaSource && mediaSource.readyState === "open") {
                maybeCreateBufferForTrack("audio", mimeType, codec);
            }
        }

        if (mediaSource && mediaSource.readyState === "open") {
            maybeCreateBuffers();
        }

        if (trackType === "video") {
            if (isInitSegment) {
                hasVideoInit = true;
                waitingVideoInitLogged = false;
                logTerminal(`[MSE] init segment received (video) size=${chunk.byteLength}`);
            }
            videoQueue.push({ chunk, isInitSegment });
            logTerminal(`[MSE] queue push track=video size=${chunk.byteLength}`);
            logTerminal(`[MSE] queue size video=${videoQueue.length} audio=${audioQueue.length}`);
            processTrackQueue("video");
            return;
        }

        if (trackType === "audio") {
            if (isInitSegment) {
                hasAudioInit = true;
                waitingAudioInitLogged = false;
                logTerminal(`[MSE] init segment received (audio) size=${chunk.byteLength}`);
            }
            audioQueue.push({ chunk, isInitSegment });
            logTerminal(`[MSE] queue push track=audio size=${chunk.byteLength}`);
            logTerminal(`[MSE] queue size video=${videoQueue.length} audio=${audioQueue.length}`);
            processTrackQueue("audio");
            return;
        }

        // Unknown track fallback: keep old behavior but warn
        logErrorTerm("[MSE] Unknown trackType; dropping chunk");
    });

    logTerminal("WebRTC/MSE UI ready. Waiting for payloads...");
};
