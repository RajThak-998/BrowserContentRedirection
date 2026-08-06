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

    function showToast(message) {
        const id = 'bcr-toast-notification';
        let container = document.getElementById(id);
        if (!container) {
            container = document.createElement('div');
            container.id = id;
            container.style.cssText = `
                position: fixed;
                top: 20px;
                right: 20px;
                background: rgba(24, 24, 27, 0.85);
                backdrop-filter: blur(8px);
                -webkit-backdrop-filter: blur(8px);
                border: 1px solid rgba(255, 255, 255, 0.1);
                color: #ffffff;
                padding: 12px 20px;
                border-radius: 8px;
                font-family: 'Segoe UI', Roboto, Helvetica, Arial, sans-serif;
                font-size: 14px;
                font-weight: 500;
                box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
                z-index: 999999;
                transition: opacity 0.3s ease, transform 0.3s ease;
                transform: translateY(-20px);
                opacity: 0;
                display: flex;
                align-items: center;
                gap: 10px;
            `;
            const target = document.body || document.documentElement;
            if (target) {
                target.appendChild(container);
            }
        }

        container.textContent = '';
        const dot = document.createElement('span');
        dot.style.cssText = 'display:inline-block; width:8px; height:8px; background-color:#10B981; border-radius:50%; box-shadow: 0 0 8px #10B981;';
        const txt = document.createElement('span');
        txt.textContent = message;
        container.appendChild(dot);
        container.appendChild(txt);

        // Trigger reflow
        container.offsetHeight;

        container.style.transform = 'translateY(0)';
        container.style.opacity = '1';

        setTimeout(() => {
            container.style.transform = 'translateY(-20px)';
            container.style.opacity = '0';
        }, 3000);
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

    // Descriptors for currentTime / paused / duration / playbackRate — captured
    // before any patch so the virtual playback clock (below) can read the native
    // values and override the getters without recursing into itself.
    const currentTimeDesc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'currentTime');
    const pausedDesc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'paused');
    const durationDesc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'duration');
    const playbackRateDesc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'playbackRate');
    const readyStateDesc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'readyState');
    const HAVE_ENOUGH_DATA = 4;

    // requestVideoFrameCallback originals (HTMLVideoElement only). Used by the rVFC
    // lever below to feed YouTube's downloader a synthetic "frame rendered" signal —
    // the one thing a paused, empty-buffer element never emits, and the suspected
    // real driver of its segment fetching (which ignores our currentTime spoof).
    const origRVFC = (typeof HTMLVideoElement !== 'undefined' && HTMLVideoElement.prototype.requestVideoFrameCallback)
        ? HTMLVideoElement.prototype.requestVideoFrameCallback : null;
    const origCVFC = (typeof HTMLVideoElement !== 'undefined' && HTMLVideoElement.prototype.cancelVideoFrameCallback)
        ? HTMLVideoElement.prototype.cancelVideoFrameCallback : null;

    // ── DIAGNOSTIC TOGGLE: disable append-gating ──────────────────────────────
    // When true, suppressed videos are NOT gated: the native appendBuffer runs so the
    // real VDI <video> actually decodes and plays (currentTime advances for real), and
    // the virtual clock stays OFF. Chunks are still forwarded to bcr_client. This is a
    // CONTROL TEST — if YouTube keeps fetching in this mode, its downloader requires
    // real playback progression (decode), which no JS spoof can replace. There is a
    // GPU-decode cost on the VDI in this mode, so leave it false for normal offload.
    const BCR_DISABLE_APPEND_GATING = false;

    // ─── Virtual Playback Clock ──────────────────────────────────────────────
    // PROBLEM: suppressVideo() pauses the primary <video> so it never decodes on
    // the VDI. But YouTube's streaming engine only fetches media while it believes
    // playback is progressing: it buffers ~120s ahead of the element's currentTime,
    // and once the element is paused with currentTime frozen at 0 it stops requesting
    // new segments. The external bcr_client player keeps playing, drains that initial
    // ~120s, and then hard-stalls (~2 min in) with nothing left to play.
    //
    // FIX: simulate playback on the suppressed element WITHOUT decoding. We report it
    // as playing (paused=false + play/playing events), advance a synthetic currentTime
    // at real-time rate, and fire periodic 'timeupdate' events. YouTube then keeps
    // topping up its forward buffer (~120s ahead of the advancing clock), fetching
    // segments progressively and in order — exactly as during real playback. The real
    // element stays paused and muted underneath, so no GPU decode ever happens.
    const PUMP_INTERVAL_MS = 250;

    // ── Seek-pulse config ─────────────────────────────────────────────────────
    // Property spoofs (currentTime/paused/readyState/rVFC) drive YouTube's UI but NOT
    // its downloader. A SEEK (assigning video.currentTime), however, moves the native
    // media clock the downloader reads — CONFIRMED by manual scrubber jumps forcing
    // fetches. The trick for CONTINUITY: seek to just BEHIND YouTube's own buffered
    // frontier (getProgressState().loaded), so it fetches the NEXT contiguous region
    // (bcr_client's stream extends smoothly, no jump), instead of jumping past the
    // buffer into unbuffered territory (which gaps). Paced to keep the frontier
    // ~SEEK_LEAD_S ahead of real playback (wall-clock proxy), so we don't front-load
    // the whole video. Requires a seekable element → the INIT segment is un-gated below.
    const BCR_SEEK_PULSE_ENABLED = true;
    const SEEK_LEAD_S     = 60;    // keep YouTube's frontier at least this far ahead of playback
    const SEEK_BEHIND_S   = 10;    // seek this far BEHIND the frontier (stays inside buffer → no gap)
    const SEEK_MIN_GAP_MS = 2000;  // minimum wall-clock between seeks

    // ── Pulse-decode config ───────────────────────────────────────────────────
    // NEW default mode (supersedes the paused seek-only pulse). Media is un-gated so the
    // VDI element REALLY decodes; YouTube then front-loads a full ~120s buffer in-order.
    // To keep it cheap the element only decodes in brief PULSES: when bcr_client's buffer
    // (reverse channel) runs below SEEK_LEAD_S we seek the element to just behind its OWN
    // native buffered frontier and play; when the buffer is topped up to HIGH_WATER (or
    // MAX_PULSE_MS elapses) we pause again. The video region is occluded (BCR_OCCLUDE) so
    // the decode costs no RDP bandwidth. Set BCR_PULSE_DECODE=false to fall back to the
    // old seek-only pulse (BCR_SEEK_PULSE_ENABLED) or the control-test passthrough.
    const BCR_PULSE_DECODE   = true;
    const BCR_OCCLUDE        = true;   // cover the VDI video with an opaque layer (RDP saving)
    const HIGH_WATER_S       = 90;     // end a pulse once buffer is this far ahead of playback
    const MAX_PULSE_MS       = 12000;  // safety cap on a single pulse's decode time
    const PULSE_MIN_GAP_MS   = 1500;   // minimum idle (paused) time between pulses
    const pulseState = { playing: false, startWall: 0, lastEndWall: 0 };

    // YouTube ads play through the SAME element + MSE pipeline as the content, so every
    // ad break costs a re-init cycle and splices ad media into bcr_client's timeline.
    // Skipping them as early as possible is a pipeline fix, not just a UX one.
    const BCR_AD_AUTOSKIP    = true;   // click "Skip Ad" as soon as YouTube renders it
    const BCR_AD_UNCOVER     = true;   // lift the occluder while an ad shows (keeps it visible)
    const BCR_AD_FASTFORWARD = true;   // seek unskippable ads to their end (no button exists)
    const AD_MAX_DURATION_S  = 180;    // sanity bound — never fast-forward longer media
    let adActive = false;              // true while an ad break is in progress
    let contentDuration = NaN;         // element duration observed OUTSIDE an ad

    // The user watches on bcr_client, so a scrub THERE must move the VDI element too:
    // YouTube only fetches around its own playhead, so without this the external player
    // seeks into a region the downloader will never be asked to request.
    const BCR_SEEK_FOLLOW    = true;
    const SEEK_FOLLOW_JUMP_S = 3;      // playhead delta (beyond normal drift) that means "seek"

    const virtualClock = {
        video: null,
        active: false,        // armed (clock owns this element) — set on suppression
        started: false,       // advancing — set on the FIRST real media chunk
        baseTime: 0,          // media-time (s) anchored at anchorWall
        anchorWall: 0,        // performance.now() when baseTime was set
        rate: 1,
        timer: null,
        reads: 0,             // diag: times YouTube has read our synthetic currentTime
        lastLog: 0,           // diag: throttle for the periodic state log
        seekTarget: 0,        // seek-pulse: last native-clock seek target
        lastSeekWall: 0,      // seek-pulse: performance.now() of the last seek (0 = none yet)
        playbackStartWall: 0, // wall-clock when playback began — the real-playback pacing proxy
    };

    // Read YouTube's OWN buffered frontier (seconds) via its player API. This is the
    // model buffered_end — the exact edge past which a seek creates a fetch deficit.
    // Returns NaN if the player API isn't available.
    function getYouTubeLoaded() {
        try {
            const mp = document.getElementById('movie_player');
            if (mp && typeof mp.getProgressState === 'function') {
                const st = mp.getProgressState();
                if (st && typeof st.loaded === 'number' && isFinite(st.loaded)) {
                    return st.loaded;
                }
            }
        } catch (_) {}
        return NaN;
    }

    // YouTube's OWN video id for the page. This is the identity bcr_client keys its
    // session on, and it must be STABLE: the per-element `data-bcr-video-id` UUID is
    // regenerated on every src/srcObject assignment, so YouTube's startup element churn
    // produced a fresh id (or 'unknown') several times per load — while a real SPA
    // navigation to a different video sometimes produced none at all, so the client never
    // tore the old session down and kept playing the previous video's buffer.
    //
    // The URL is checked FIRST on purpose: getVideoData().video_id switches to the AD's
    // id during an ad break, which would read as a video change and tear down the session
    // at every ad. location.href does not change for ads.
    let _ytIdHref = '';
    let _ytIdCached = null;
    function currentYouTubeVideoId() {
        const href = location.href; // appendBuffer is hot — only recompute on navigation
        if (href === _ytIdHref) return _ytIdCached;
        _ytIdHref = href;
        _ytIdCached = null;
        try {
            const u = new URL(href);
            const v = u.searchParams.get('v');
            if (v) {
                _ytIdCached = v;
            } else {
                const m = u.pathname.match(/^\/(?:shorts|embed|live)\/([^/?#]+)/);
                if (m) _ytIdCached = m[1];
            }
        } catch (_) {}
        if (!_ytIdCached) {
            try {
                const mp = document.getElementById('movie_player');
                if (mp && typeof mp.getVideoData === 'function') {
                    const vd = mp.getVideoData();
                    if (vd && typeof vd.video_id === 'string' && vd.video_id) _ytIdCached = vd.video_id;
                }
            } catch (_) {}
        }
        return _ytIdCached;
    }

    // diag: how many suppressed media chunks we've forwarded, and when the last one
    // flowed. If `chunks` keeps climbing past the stall point, YouTube is still
    // fetching and the loss is downstream (transport); if it plateaus, YouTube
    // stopped feeding and the fetch lever needs more work.
    let fwdVideoChunks = 0;
    let fwdAudioChunks = 0;
    let lastChunkWall = 0;

    // Playback state reported BACK by bcr_client (its real MSE buffer is the ground
    // truth for the frontier — the VDI element's buffer is empty because we gate it).
    // Fed by BCR_YT_PLAYBACK messages; drives the seek pulse's target + pacing.
    let bcrBufferedEnd = NaN;
    let bcrPlayhead = NaN;
    let bcrReportWall = 0;

    // Seek-follow: reports arrive ~1×/sec, so during normal playback the playhead
    // advances by roughly the elapsed wall time. A delta that departs from that by more
    // than SEEK_FOLLOW_JUMP_S is a user scrub on bcr_client, which the VDI element must
    // mirror. Latched here and consumed by pulseTick (the pump owns all element writes).
    let lastBcrPlayhead = NaN;
    let lastBcrPlayheadWall = 0;
    let pendingFollowSeek = NaN;

    function noteBcrPlayhead(ph) {
        if (!BCR_SEEK_FOLLOW || typeof ph !== 'number' || !Number.isFinite(ph)) return;
        const now = performance.now();
        if (Number.isFinite(lastBcrPlayhead) && lastBcrPlayheadWall > 0) {
            const elapsed = (now - lastBcrPlayheadWall) / 1000;
            const delta = ph - lastBcrPlayhead;
            // Both tests matter: the first catches a jump, the second stops a long
            // report gap on a PAUSED client (delta 0, elapsed 10s) from looking like one.
            if (Math.abs(delta - elapsed) > SEEK_FOLLOW_JUMP_S &&
                Math.abs(delta) > SEEK_FOLLOW_JUMP_S) {
                pendingFollowSeek = ph;
                BCR_LOG('[BCR] seek-follow armed → ' + ph.toFixed(1) + 's (was ' +
                    lastBcrPlayhead.toFixed(1) + 's, ' + elapsed.toFixed(1) + 's ago)');
            }
        }
        lastBcrPlayhead = ph;
        lastBcrPlayheadWall = now;
    }

    // Seek the hidden VDI element to `target`. Prefer YouTube's own seekTo (exactly
    // what the working manual scrubber drag uses); fall back to a raw currentTime set.
    // Either way, re-anchor the virtual clock so getter/UI stay consistent.
    function seekVDI(v, target) {
        let done = false;
        try {
            const mp = document.getElementById('movie_player');
            if (mp && typeof mp.seekTo === 'function') {
                mp.seekTo(target, true);
                done = true;
            }
        } catch (_) {}
        if (!done) {
            try { v.currentTime = target; } catch (_) {}
        }
        anchorVirtualTime(target);
    }

    // ── Pulse-decode driver ───────────────────────────────────────────────────
    // The VDI element's OWN native buffered frontier (media un-gated, so this is
    // YouTube's real buffer). `buffered` is not overridden, so this reads native.
    function nativeBufferedEnd(v) {
        try {
            const b = v.buffered;
            if (b && b.length > 0) return b.end(b.length - 1);
        } catch (_) {}
        return NaN;
    }

    function startPulse(v, opts) {
        const nb = nativeBufferedEnd(v);
        const ct = nativeCurrentTime(v);
        // Only re-sync forward when the element has DRIFTED well behind its own frontier.
        // Seeking at startup / when already near the frontier disrupts YouTube's initial
        // load (it churned video elements and left bcr_client on an init-less session).
        // noSeek: a follow-seek just positioned the element — don't drag it back.
        if (!(opts && opts.noSeek) &&
            Number.isFinite(nb) && nb > SEEK_BEHIND_S && ct < nb - SEEK_LEAD_S) {
            seekVDI(v, nb - SEEK_BEHIND_S);
        }
        try { v.muted = true; v.volume = 0; } catch (_) {}
        try { const p = origPlay.call(v); if (p && typeof p.catch === 'function') p.catch(() => {}); } catch (_) {}
        pulseState.playing = true;
        pulseState.startWall = performance.now();
        BCR_LOG('[BCR] pulse START nativeBufEnd=' + (Number.isFinite(nb) ? nb.toFixed(1) : '?') + 's ct=' + ct.toFixed(1) + 's');
    }

    function endPulse(v, reason) {
        try { v.pause(); } catch (_) {}
        pulseState.playing = false;
        pulseState.lastEndWall = performance.now();
        BCR_LOG('[BCR] pulse END reason=' + reason);
    }

    function pulseTick(v) {
        updateOccluder(v);
        // Keep the VDI element silent (audio plays only on bcr_client). YouTube re-unmutes
        // during load / on play, so enforce it every tick.
        try { if (!v.muted || v.volume !== 0) { v.muted = true; v.volume = 0; } } catch (_) {}

        const now = performance.now();

        // ── Ad break ──────────────────────────────────────────────────────────
        // Ads run on a different timeline, so the content buffer math below is
        // meaningless here and a frontier seek would fight the ad. Suspend the pulse
        // and let the ad handler drive the element until the break is over.
        if (isAdShowing()) {
            if (!adActive) {
                adActive = true;
                BCR_LOG('[BCR] ad break started — pulse suspended, element forced to play');
            }
            handleAdBreak(v);
            return;
        }
        if (adActive) {
            adActive = false;
            // The ad handler left the element playing; hand it back paused so the
            // normal low-buffer logic restarts a pulse from a known state.
            endPulse(v, 'ad-end');
            BCR_LOG('[BCR] ad break ended — pulse resumed');
            return;
        }

        // Mirror an external-player scrub before any buffer math: the element must be
        // sitting at the new position for YouTube to start fetching around it, and the
        // stale bcrBufferedEnd from the old position would otherwise skew `bufferAhead`.
        if (Number.isFinite(pendingFollowSeek)) {
            const target = Math.max(0, pendingFollowSeek);
            pendingFollowSeek = NaN;
            seekVDI(v, target);
            BCR_LOG('[BCR] seek-follow → VDI element seeked to ' + target.toFixed(1) + 's');
            if (pulseState.playing) {
                // Already decoding — give the pulse a full budget to fetch at the new spot.
                pulseState.startWall = now;
            } else {
                // Fetch from the new position immediately; ignore PULSE_MIN_GAP_MS.
                startPulse(v, { noSeek: true });
            }
            return;
        }
        const nb = nativeBufferedEnd(v);
        const dur = nativeDuration(v);
        // Remember the content duration while we're NOT in a break — tryFastForwardAd
        // refuses to seek anything that still matches it.
        if (Number.isFinite(dur) && dur > 0) contentDuration = dur;
        const reportFresh = bcrReportWall > 0 && (now - bcrReportWall) < 8000;
        const bufferAhead = (reportFresh && Number.isFinite(bcrBufferedEnd) && Number.isFinite(bcrPlayhead))
            ? (bcrBufferedEnd - bcrPlayhead) : NaN;
        const atEnd = Number.isFinite(dur) && dur > 0 && Number.isFinite(bcrBufferedEnd) && bcrBufferedEnd >= dur - 1;

        if (!pulseState.playing) {
            // Idle (paused). Start a pulse when the buffer is low — or when we have no
            // reports yet (bootstrap) — but only once the element actually holds data
            // (never play an empty element) and not at end of video.
            const low = !Number.isFinite(bufferAhead) || bufferAhead < SEEK_LEAD_S;
            const dueByTime = now - pulseState.lastEndWall >= PULSE_MIN_GAP_MS;
            const haveData = Number.isFinite(nb);
            if (low && !atEnd && dueByTime && haveData) startPulse(v);
        } else {
            // Pulsing (playing). End it when topped up, safety-capped, faulted, or ended.
            const topped = Number.isFinite(bufferAhead) && bufferAhead >= HIGH_WATER_S;
            const timedOut = now - pulseState.startWall >= MAX_PULSE_MS;
            const faulted = !!v.error;
            if (topped || timedOut || faulted || atEnd) {
                endPulse(v, faulted ? 'fault' : (topped ? 'topped' : (atEnd ? 'end' : 'timeout')));
            }
        }

        // Periodic diagnostic (VDI console + bcr_client.log via BCR_YT_DIAG).
        if (now - virtualClock.lastLog >= 2000) {
            virtualClock.lastLog = now;
            const sinceChunk = lastChunkWall ? Math.round(now - lastChunkWall) : -1;
            const line = 'Pulse: ' + (pulseState.playing ? 'PLAY' : 'idle') +
                ' bcrBufEnd=' + (Number.isFinite(bcrBufferedEnd) ? bcrBufferedEnd.toFixed(1) : '?') +
                's bcrPlay=' + (Number.isFinite(bcrPlayhead) ? bcrPlayhead.toFixed(1) : '?') +
                's ahead=' + (Number.isFinite(bufferAhead) ? bufferAhead.toFixed(1) : '?') +
                's nativeBufEnd=' + (Number.isFinite(nb) ? nb.toFixed(1) : '?') +
                's fwdChunks(v/a)=' + fwdVideoChunks + '/' + fwdAudioChunks +
                ' sinceLastChunk=' + sinceChunk + 'ms';
            BCR_LOG('[BCR] ' + line);
            try { window.postMessage({ type: 'BCR_YT_DIAG', payload: { message: line } }, '*'); } catch (_) {}
        }
    }

    // ── Ad handling ───────────────────────────────────────────────────────────
    // There is no player-API method to skip an ad — YouTube never shipped one — so the
    // button click IS the mechanism. `ad-showing` / `ad-interrupting` on #movie_player is
    // YouTube's own ad-state flag and is what its stylesheets key off, so it is a far
    // more stable signal than the button selectors themselves.
    function isAdShowing() {
        try {
            const mp = document.getElementById('movie_player');
            if (!mp) return false;
            return mp.classList.contains('ad-showing') || mp.classList.contains('ad-interrupting');
        } catch (_) { return false; }
    }

    // Ordered most-current first. These are YouTube-internal class names and DO get
    // renamed; a miss is harmless (the occluder lifts, so the button stays clickable).
    const AD_SKIP_SELECTORS = [
        '.ytp-skip-ad-button',
        '.ytp-ad-skip-button-modern',
        '.ytp-ad-skip-button',
    ];
    let lastAdSkipWall = 0;

    function trySkipAd() {
        if (!BCR_AD_AUTOSKIP || !isAdShowing()) return;
        const now = performance.now();
        // The button only appears ~5s in; re-checking every tick is fine but clicking
        // repeatedly is not (YouTube treats a click storm as a mis-click).
        if (now - lastAdSkipWall < 600) return;
        lastAdSkipWall = now;
        for (const sel of AD_SKIP_SELECTORS) {
            let btn = null;
            try { btn = document.querySelector(sel); } catch (_) { continue; }
            if (!btn) continue;
            // offsetParent is unreliable for the fixed/absolutely-positioned ad chrome,
            // so measure instead — a rendered button always has a non-zero box.
            let visible = false;
            try {
                const r = btn.getBoundingClientRect();
                visible = r.width > 0 && r.height > 0;
            } catch (_) {}
            if (!visible) continue;
            try {
                btn.click();
                BCR_LOG('[BCR] ad: clicked skip button (' + sel + ')');
            } catch (_) {}
            return;
        }
    }

    // Unskippable ads (bumpers, non-skippable pre-rolls) render NO skip button at all,
    // so clicking can never work — seek the ad to its end instead. Guarded twice against
    // ever doing this to the real video: the duration must be ad-length AND must differ
    // from the content duration we recorded outside the break.
    let lastAdFfWall = 0;
    function tryFastForwardAd(v) {
        if (!BCR_AD_FASTFORWARD || !v) return;
        const now = performance.now();
        if (now - lastAdFfWall < 800) return;
        const d = nativeDuration(v);
        if (!Number.isFinite(d) || d <= 0 || d > AD_MAX_DURATION_S) return;
        if (Number.isFinite(contentDuration) && Math.abs(d - contentDuration) < 1) return;
        const ct = nativeCurrentTime(v);
        if (ct >= d - 0.5) return; // already at the end
        lastAdFfWall = now;
        // Raw currentTime, not seekVDI: movie_player.seekTo is scoped to the content
        // timeline and YouTube blocks scrubbing during an ad.
        try { v.currentTime = Math.max(0, d - 0.1); } catch (_) {}
        BCR_LOG('[BCR] ad: fast-forwarded ' + ct.toFixed(1) + 's → ' + d.toFixed(1) + 's (no skip button)');
    }

    function handleAdBreak(v) {
        // THE important part: the ad plays on the SAME element the pulse machinery
        // pauses. If the break starts while we're idle between pulses, the ad never
        // advances — so YouTube never renders the skip button (it appears ~5s IN) and
        // the break never ends. Keep the element running for the whole ad.
        try {
            if (v.paused) {
                const p = origPlay.call(v);
                if (p && typeof p.catch === 'function') p.catch(() => {});
            }
        } catch (_) {}
        trySkipAd();
        tryFastForwardAd(v);
    }

    // ── Occlusion layer ───────────────────────────────────────────────────────
    // An opaque element pinned over the primary video so the VDI framebuffer shows a
    // static region (cheap over RDP) while the video decodes behind it. pointer-events
    // are disabled so clicks still reach YouTube's controls underneath.
    let _occluder = null;
    function ensureOccluder() {
        if (_occluder && _occluder.isConnected) return _occluder;
        const d = document.createElement('div');
        d.setAttribute('data-bcr-occluder', '1');
        d.style.cssText = 'position:fixed; left:0; top:0; width:0; height:0; background:#000; ' +
            'pointer-events:none; z-index:2147483646; display:none;';
        (document.body || document.documentElement).appendChild(d);
        _occluder = d;
        return d;
    }
    function updateOccluder(video) {
        if (!BCR_OCCLUDE || !bcrRedirectEnabled) return;
        const d = ensureOccluder();
        try {
            // Uncover during an ad: auto-skip needs ~5s before the button exists, and the
            // selectors above can go stale, so the ad must stay visible/clickable.
            if (BCR_AD_UNCOVER && isAdShowing()) { d.style.display = 'none'; return; }
            if (!video || video.isConnected === false) { d.style.display = 'none'; return; }
            const r = video.getBoundingClientRect();
            if (r.width < 2 || r.height < 2 || r.bottom < 0 || r.right < 0) { d.style.display = 'none'; return; }
            d.style.left = r.left + 'px';
            d.style.top = r.top + 'px';
            d.style.width = r.width + 'px';
            d.style.height = r.height + 'px';
            d.style.display = 'block';
        } catch (_) { d.style.display = 'none'; }
    }

    function nativeCurrentTime(el) {
        try { return currentTimeDesc && currentTimeDesc.get ? currentTimeDesc.get.call(el) : 0; }
        catch (_) { return 0; }
    }
    function nativeDuration(el) {
        try { return durationDesc && durationDesc.get ? durationDesc.get.call(el) : NaN; }
        catch (_) { return NaN; }
    }
    function nativePlaybackRate(el) {
        try {
            const r = playbackRateDesc && playbackRateDesc.get ? playbackRateDesc.get.call(el) : 1;
            return (typeof r === 'number' && r > 0) ? r : 1;
        } catch (_) { return 1; }
    }

    // Current virtual media-time for the active clock (null if inactive). While the
    // clock is armed but not yet started (no media flowing), it stays FROZEN at
    // baseTime so currentTime can never run ahead of what YouTube has actually
    // fetched — running ahead makes YouTube think the playhead overtook its buffer
    // and it stops filling normally.
    function getVirtualTime() {
        if (!virtualClock.active || !virtualClock.video) return null;
        if (!virtualClock.started) return virtualClock.baseTime;
        const elapsed = (performance.now() - virtualClock.anchorWall) / 1000;
        let t = virtualClock.baseTime + elapsed * virtualClock.rate;
        if (t < 0) t = 0;
        const dur = nativeDuration(virtualClock.video);
        if (Number.isFinite(dur) && dur > 0 && t > dur) t = dur;
        return t;
    }

    // Re-anchor the clock to a specific media-time (used on seeks / rate changes).
    function anchorVirtualTime(t) {
        virtualClock.baseTime = (typeof t === 'number' && t >= 0) ? t : 0;
        virtualClock.anchorWall = performance.now();
    }

    function stopVirtualClock() {
        if (virtualClock.timer) {
            clearInterval(virtualClock.timer);
            virtualClock.timer = null;
        }
        virtualClock.active = false;
        virtualClock.started = false;
        virtualClock.video = null;
        pulseState.playing = false;
        pulseState.startWall = 0;
        pulseState.lastEndWall = 0;
        // Drop the seek baseline too — the next session's first report must not be
        // diffed against the previous video's playhead (that reads as a huge scrub).
        lastBcrPlayhead = NaN;
        lastBcrPlayheadWall = 0;
        pendingFollowSeek = NaN;
        adActive = false;
        contentDuration = NaN;
        if (_occluder) _occluder.style.display = 'none';
        stopSyntheticRVFC();
    }

    // ─── bcr_client availability / fallback ───────────────────────────────────
    //
    // Redirection only makes sense when there is a local player to redirect TO.
    // With bcr_client gone the old behaviour left the user staring at the opaque
    // occluder with no audio, because nothing ever reconsidered the decision to
    // suppress. bcr_host now reports the real bridge state (BCR_STATUS), and
    // this flag gates the whole redirect path on it.
    //
    // Starts true deliberately. If the status has not arrived by the time
    // YouTube's first appendBuffer fires we redirect anyway: guessing
    // "disconnected" wrongly disables BCR silently and completely, while
    // guessing "connected" wrongly costs a brief occluder that
    // restoreNativePlayback() then clears. Fail toward the recoverable error.
    let bcrRedirectEnabled = true;

    // Element state captured before suppressVideo() mutated it, so it can be put
    // back exactly as it was.
    const _preSuppressState = new WeakMap();

    /**
     * Hand the video back to the VDI: stop redirecting, uncover the element,
     * restore audio, and resume playing at wherever the external player had got
     * to. Safe to call when nothing was suppressed yet.
     *
     * This is cheap because BCR_PULSE_DECODE mode never fakes anything
     * structural — media is passed to the real decoder (see patchedAppendBuffer)
     * and markPlaybackStarted() early-returns, so virtualClock.started stays
     * false and every property spoof (currentTime/paused/readyState/rVFC) is
     * already inert. There are no prototype patches to unwind, only element
     * state to put back.
     */
    function restoreNativePlayback(reason) {
        if (!bcrRedirectEnabled) return;
        bcrRedirectEnabled = false;

        BCR_LOG('[BCR] falling back to VDI playback — ' + reason);

        const v = state.primaryVideo;

        // Stops the pump interval, clears pulse state, hides the occluder, and
        // (for non-pulse modes) disarms every property spoof.
        stopVirtualClock();

        if (_occluder) {
            try { _occluder.remove(); } catch (_) {}
            _occluder = null;
        }

        stopSilentAudioKeepalive();

        if (!v) return; // nothing was ever suppressed — gate alone is enough

        try { state.suppressedVideos.delete(v); } catch (_) {}

        const prev = _preSuppressState.get(v);
        try {
            v.muted = prev ? prev.muted : false;
            v.volume = prev ? prev.volume : 1;
            v.preload = prev ? prev.preload : 'auto';
        } catch (_) {}

        // YouTube re-applies its own stored volume through the player API, which
        // is more reliable than poking the element when the two disagree.
        try {
            const mp = document.getElementById('movie_player');
            if (mp && typeof mp.unMute === 'function') mp.unMute();
        } catch (_) {}

        // The element has been pulsed around its own buffer, so it is not where
        // the user was watching. bcrPlayhead is the external player's last
        // reported position — the only record of what the user actually saw.
        if (Number.isFinite(bcrPlayhead) && bcrPlayhead > 0) {
            try { seekVDI(v, bcrPlayhead); } catch (_) {}
        }

        try {
            const p = origPlay.call(v);
            if (p && typeof p.catch === 'function') p.catch(() => {});
        } catch (_) {}

        try { showToast('BCR player unavailable — playing in session'); } catch (_) {}
    }

    // ── requestVideoFrameCallback (rVFC) lever ────────────────────────────────
    // YouTube's segment downloader appears to track playback progress via rendered
    // video frames, NOT via video.currentTime (which we already spoof — see ytReads).
    // A suppressed element never decodes a frame, so rVFC never fires and the
    // downloader's playhead stays frozen at 0. We synthesize rVFC callbacks driven by
    // requestAnimationFrame, each carrying an advancing mediaTime (= the virtual clock)
    // and an incrementing presentedFrames — mimicking real frame presentation without
    // decoding. Synthetic handles are offset by RVFC_BASE so they never collide with
    // native handles (previews / non-clock videos keep 100% native rVFC behavior).
    const RVFC_BASE = 0x40000000;
    let rvfcSeq = 0;
    const rvfcCallbacks = new Map(); // synthetic handle -> callback
    let rvfcRafId = null;
    let rvfcPresentedFrames = 0;

    function fireSyntheticRVFC() {
        rvfcRafId = null;
        if (!virtualClock.started || !virtualClock.video || rvfcCallbacks.size === 0) return;

        const v = virtualClock.video;
        const now = performance.now();
        const vt = getVirtualTime();
        rvfcPresentedFrames++;

        const metadata = {
            presentationTime: now,
            expectedDisplayTime: now + 16,
            width: v.videoWidth || 1280,
            height: v.videoHeight || 720,
            mediaTime: vt != null ? vt : 0,
            presentedFrames: rvfcPresentedFrames,
            processingDuration: 0,
        };

        // rVFC is one-shot: fire every currently-registered callback once, then clear.
        // Callbacks that re-register (the normal per-frame pattern) refill the map and
        // keep the rAF loop alive via requestVideoFrameCallback below.
        const pending = Array.from(rvfcCallbacks.values());
        rvfcCallbacks.clear();
        for (const cb of pending) {
            try { cb(now, metadata); } catch (_) {}
        }

        if (rvfcCallbacks.size > 0 && rvfcRafId === null) {
            rvfcRafId = requestAnimationFrame(fireSyntheticRVFC);
        }
    }

    function ensureSyntheticRVFCLoop() {
        if (rvfcRafId === null && rvfcCallbacks.size > 0) {
            rvfcRafId = requestAnimationFrame(fireSyntheticRVFC);
        }
    }

    function stopSyntheticRVFC() {
        if (rvfcRafId !== null) {
            try { cancelAnimationFrame(rvfcRafId); } catch (_) {}
            rvfcRafId = null;
        }
        rvfcCallbacks.clear();
        rvfcPresentedFrames = 0;
    }

    // Begin advancing. Called from patchedAppendBuffer on the FIRST real (non-init)
    // media chunk for the primary — i.e. once YouTube is genuinely streaming — so the
    // synthetic currentTime starts from 0 in lock-step with real playback, never ahead.
    function markPlaybackStarted(video) {
        if (BCR_PULSE_DECODE) return; // real playback drives the clock; no synthetic spoof
        if (!virtualClock.active || virtualClock.video !== video || virtualClock.started) return;
        virtualClock.started = true;
        virtualClock.rate = nativePlaybackRate(video);
        virtualClock.playbackStartWall = performance.now(); // pacing anchor for the seek pulse
        anchorVirtualTime(nativeCurrentTime(video));

        // suppressVideo() fired a real 'pause'; flip YouTube back to a playing state so
        // its streaming engine leaves "paused" and keeps requesting segments.
        try { video.dispatchEvent(new Event('play')); } catch (_) {}
        try { video.dispatchEvent(new Event('playing')); } catch (_) {}
        BCR_LOG('[BCR] Virtual clock: first media chunk seen — playback simulation ON, advancing currentTime from ' + virtualClock.baseTime.toFixed(2) + 's');
    }

    function pumpTick() {
        const v = virtualClock.video;
        if (!virtualClock.active || !v) return;

        // Primary video left the DOM — nothing to drive anymore.
        if (v.isConnected === false) {
            BCR_LOG('[BCR] Virtual clock: primary video detached — stopping');
            if (_occluder) _occluder.style.display = 'none';
            stopVirtualClock();
            return;
        }

        // Pulse-decode mode: real decode in brief pulses drives YouTube's fetching. Runs
        // regardless of virtualClock.started (kept false so the paused-element spoofs
        // stay inert — the element plays for real, native currentTime is authoritative).
        if (BCR_PULSE_DECODE) {
            pulseTick(v);
            return;
        }

        if (!virtualClock.started) return; // armed but YouTube hasn't started streaming yet

        // Track live playback-rate changes so fast-forward / slow-mo stay honest.
        const r = nativePlaybackRate(v);
        if (r !== virtualClock.rate) {
            anchorVirtualTime(getVirtualTime() ?? virtualClock.baseTime);
            virtualClock.rate = r;
        }

        const vt = getVirtualTime();
        const dur = nativeDuration(v);
        if (Number.isFinite(dur) && dur > 0 && vt !== null && vt >= dur) {
            return; // reached end of a finite video — idle
        }

        // Drive YouTube's streaming loop: a 'timeupdate' with an advanced currentTime
        // makes it recompute buffer-ahead and fetch the next segments in order.
        try { v.dispatchEvent(new Event('timeupdate')); } catch (_) {}

        const now = performance.now();

        // ── Seek pulse ────────────────────────────────────────────────────────
        // Move the native media clock to just BEHIND bcr_client's REAL buffered frontier
        // (reported via BCR_YT_PLAYBACK — the VDI element's own buffer is empty because we
        // gate it, so the client's MSE buffer is the only ground truth). This creates a
        // fetch deficit that extends the buffer CONTIGUOUSLY — bcr_client's stream never
        // jumps. Paced against the client's real playhead so it stays ~SEEK_LEAD_S ahead
        // without front-loading. Reports are fresh only if received within ~5s.
        const reportFresh = bcrReportWall > 0 && (now - bcrReportWall) < 5000;
        if (BCR_SEEK_PULSE_ENABLED && reportFresh && Number.isFinite(bcrBufferedEnd) && Number.isFinite(bcrPlayhead)) {
            const bufferAhead = bcrBufferedEnd - bcrPlayhead;
            const dueByTime = now - virtualClock.lastSeekWall >= SEEK_MIN_GAP_MS;
            const notAtEnd = !(Number.isFinite(dur) && dur > 0 && bcrBufferedEnd >= dur - 1);
            if (bufferAhead < SEEK_LEAD_S && dueByTime && notAtEnd) {
                const target = Math.max(0, bcrBufferedEnd - SEEK_BEHIND_S);
                virtualClock.seekTarget = target;
                virtualClock.lastSeekWall = now;
                seekVDI(v, target);
                BCR_LOG('[BCR] Seek pulse → ' + target.toFixed(1) + 's (bcrBufEnd=' + bcrBufferedEnd.toFixed(1) +
                    ' playhead=' + bcrPlayhead.toFixed(1) + ' ahead=' + bufferAhead.toFixed(1) + ')');
            }
        }

        // Periodic diagnostic (VDI Chrome console + bcr_client.log). fwdChunks climbing
        // past the old ~120s cap after seeks start = the seek pulse is working.
        if (now - virtualClock.lastLog >= 2000) {
            virtualClock.lastLog = now;
            const sinceChunk = lastChunkWall ? Math.round(now - lastChunkWall) : -1;
            const line = 'Virtual clock: t=' + (vt != null ? vt.toFixed(2) : '?') +
                's bcrBufEnd=' + (Number.isFinite(bcrBufferedEnd) ? bcrBufferedEnd.toFixed(1) : '?') +
                's bcrPlay=' + (Number.isFinite(bcrPlayhead) ? bcrPlayhead.toFixed(1) : '?') +
                's seekTarget=' + virtualClock.seekTarget.toFixed(1) +
                's ytReads=' + virtualClock.reads +
                ' fwdChunks(v/a)=' + fwdVideoChunks + '/' + fwdAudioChunks +
                ' sinceLastChunk=' + sinceChunk + 'ms';
            BCR_LOG('[BCR] ' + line);
            // Mirror the YT diagnostic into bcr_client.log via the telemetry path
            // (BCR_YT_DIAG → bootstrap → emitter → background → bcr_host → bcr_client).
            try { window.postMessage({ type: 'BCR_YT_DIAG', payload: { message: line } }, '*'); } catch (_) {}
        }
    }

    // Arm the clock for a newly-suppressed primary. It does NOT advance yet — it waits
    // for the first real media chunk (markPlaybackStarted) before simulating playback.
    function startVirtualClock(video) {
        if (!video) return;
        stopVirtualClock();
        virtualClock.video = video;
        virtualClock.active = true;
        virtualClock.started = false;
        virtualClock.rate = nativePlaybackRate(video);
        virtualClock.baseTime = nativeCurrentTime(video); // honors a start-at offset (?t=)
        virtualClock.anchorWall = performance.now();
        virtualClock.reads = 0;
        virtualClock.lastLog = performance.now();
        virtualClock.seekTarget = 0;
        virtualClock.lastSeekWall = 0;
        virtualClock.playbackStartWall = 0;
        virtualClock.timer = setInterval(pumpTick, PUMP_INTERVAL_MS);
        BCR_LOG('[BCR] Virtual playback clock armed for primary video — waiting for first media chunk');
    }

    // ─── Blob URL → MediaSource Registry ─────────────────────────────────────
    // YouTube uses: ms = new MediaSource() → video.src = URL.createObjectURL(ms)
    // We need this map to suppress the MS when video.src is set to a blob URL.
    const blobUrlToMediaSource = new Map();
    // Reverse lookup: MediaSource → the <video> element it was assigned to.
    // Populated by the src/srcObject setters below. Used for O(1) eager suppression
    // in patchedAppendBuffer without iterating the DOM.
    const mediaSourceToVideo = new WeakMap();
    // Reverse lookup: SourceBuffer → its owning MediaSource.
    // Populated by patchedAddSourceBuffer. Part of the same O(1) eager-suppression chain.
    const sourceBufferToMediaSource = new WeakMap();

    const origCreateObjectURL = URL.createObjectURL.bind(URL);
    URL.createObjectURL = function patchedCreateObjectURL(obj) {
        const url = origCreateObjectURL(obj);
        if (obj instanceof MediaSource) {
            blobUrlToMediaSource.set(url, obj);
        }
        return url;
    };

    // Stamp the element with the SAME identity the media chunks carry. VideoRegistry
    // reads this attribute for VIDEO_ADDED/VIDEO_REMOVED, and bcr_client compares those
    // ids against the id on its media chunks — so if the two disagree, lifecycle events
    // silently stop matching the active session. On YouTube both are the video id;
    // elsewhere both fall back to the same generated UUID.
    function assignVideoIdentity(video) {
        try {
            const id = currentYouTubeVideoId() ||
                video.getAttribute('data-bcr-video-id') ||
                ((typeof crypto !== 'undefined' && crypto.randomUUID)
                    ? crypto.randomUUID()
                    : `video-${Date.now()}-${Math.random().toString(36).slice(2, 9)}`);
            video.setAttribute('data-bcr-video-id', id);
        } catch (_) {}
    }

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
            // Suppress ALL existing SourceBuffers (ms.sourceBuffers = all,
            // ms.activeSourceBuffers = only the active subset — we need both).
            const all = ms.sourceBuffers;
            for (let i = 0; i < all.length; i++) {
                state.suppressedSourceBuffers.add(all[i]);
            }
        } catch (_) { }
    }

    // ─── Video Suppression ────────────────────────────────────────────────────
    /**
     * Suppress a video element:
     *   - Pause and mute immediately (no audio from VDI from this point).
     *   - Add to suppressedVideos so all prototype patches gate on it.
     *   - Suppress any already-attached MediaSource + its existing SourceBuffers.
     *   - All future appendBuffer calls on suppressed SBs are intercepted:
     *     forwarded to BCR pipeline, NOT passed to the native decoder.
     *   - We deliberately do NOT call origLoad() to avoid detaching the MediaSource,
     *     which would flood the pipeline with a new round of init segments.
     */
    function suppressVideo(video) {
        if (state.suppressedVideos.has(video)) return;

        // No local player to redirect to — leave the element completely alone so
        // the VDI renders the video normally.
        if (!bcrRedirectEnabled) return;

        // Remember what we're about to change, so restoreNativePlayback() can put
        // it back exactly rather than guessing.
        if (!_preSuppressState.has(video)) {
            try {
                _preSuppressState.set(video, {
                    muted: video.muted,
                    volume: video.volume,
                    preload: video.preload,
                });
            } catch (_) {}
        }

        if (state.primaryVideo && state.primaryVideo !== video) {
            try {
                state.primaryVideo.removeAttribute('data-bcr-primary');
            } catch (_) {}
        }

        state.suppressedVideos.add(video);
        state.primaryVideo = video;

        assignVideoIdentity(video);
        try { video.setAttribute('data-bcr-primary', '1'); } catch (_) {}

        // Immediately pause and silence — no GPU decode from this point.
        // In the append-gating control test we deliberately do NOT pause, so the real
        // element decodes and plays (its native currentTime then advances on its own).
        if (!BCR_DISABLE_APPEND_GATING) {
            video.pause();
        }
        video.muted = true;
        video.preload = 'none';

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

        // Start silent audio keepalive to prevent background tab CPU/occlusion throttling
        startSilentAudioKeepalive();

        // NOTE: We deliberately do NOT call origLoad() here.
        // Calling load() detaches the MediaSource, causing YouTube to immediately
        // reattach it and flood the pipeline with fresh init segments — which was
        // crashing the thin-client MSE session repeatedly.
        if (BCR_DISABLE_APPEND_GATING) {
            BCR_LOG('[BCR] suppressVideo: APPEND-GATING DISABLED (control test) — real decode/playback ON, virtual clock OFF');
        } else {
            BCR_LOG('[BCR] suppressVideo: video suppressed, GPU decode gated');
            // Keep YouTube's streaming engine requesting new segments by simulating
            // playback progress on this (paused, non-decoding) element. Without this it
            // buffers only ~120s ahead of a frozen currentTime and then stops fetching,
            // starving the external bcr_client player ~2 minutes in.
            startVirtualClock(video);
        }
    }

    function isMainVideoCandidate(video) {
        try {
            if (window.location.hostname.includes('youtube.com')) {
                // Reject if the video is part of any hover preview or thumbnail component
                if (
                    video.closest('ytd-rich-item-renderer') ||
                    video.closest('ytd-video-renderer') ||
                    video.closest('ytd-grid-video-renderer') ||
                    video.closest('yt-lockup-view-model') ||
                    video.closest('ytd-compact-video-renderer') ||
                    video.closest('ytd-thumbnail') ||
                    video.closest('.ytSpecTouchFeedbackShapeHost') ||
                    video.closest('.ytSpecTouchFeedbackShapeTouchResponse') ||
                    video.closest('.ytSpecTouchFeedbackShapeThumbnailSizeLarge') ||
                    video.closest('.ytSpecTouchFeedbackShapeTriggerEvents')
                ) {
                    return false;
                }

                // Accept only if it belongs to the main player or the official miniplayer
                return !!video.closest('#movie_player') || 
                       !!video.closest('.ytp-miniplayer-scrim') ||
                       !!video.closest('.ytp-miniplayer-ui') ||
                       !!video.closest('.ytp-miniplayer-view');
            }
            // Generic page fallback: check if video is reasonably sized
            const rect = video.getBoundingClientRect();
            return rect.width >= 450 && rect.height >= 250;
        } catch (_) {
            return false;
        }
    }

    // ─── Primary Video Election ────────────────────────────────────────────────
    /**
     * Score a video by visible area.  Off-screen videos get a heavy penalty
     * so they are never elected over a visible one.
     */
    function scoreVideo(video) {
        if (!isMainVideoCandidate(video)) return 0;
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
    // Go builds the ICE agent on demand and replies as soon as the first
    // server-reflexive candidate is gathered, so this normally resolves quickly.
    // The timeout is a safety net for a network where STUN never answers.
    const RTC_WAIT_TIMEOUT_MS = 60_000;

    // Per-PC state stored in a WeakMap (GC-safe) and a bridgeId→PC lookup Map.
    const rtcStateByPeer = new WeakMap();
    const rtcPcByBridgeId = new Map(); // bridgeId → PC (for late candidate trickle)

    // ── Promise-based SHADOW_READY await infrastructure ───────────────────────
    // rtcReadyByBridgeId: cached resolved SHADOW_READY payloads
    // rtcWaitersByBridgeId: pending {resolve, timer} waiters for each bridgeId
    // rtcLastErrorByBridgeId: error reasons for failed shadow sessions
    const rtcReadyByBridgeId = new Map();
    const rtcWaitersByBridgeId = new Map();
    const rtcLastErrorByBridgeId = new Map();

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
            waiter.resolve(null); // null = failed/timed-out
        }
    }

    function awaitShadowReady(bridgeId, timeoutMs) {
        if (!bridgeId) return Promise.resolve(null);

        // If SHADOW_READY already arrived for this bridge, return it immediately.
        const cached = rtcReadyByBridgeId.get(bridgeId);
        if (cached) return Promise.resolve(cached);

        return new Promise((resolve) => {
            const timer = setTimeout(() => {
                const list = rtcWaitersByBridgeId.get(bridgeId) ?? [];
                rtcWaitersByBridgeId.set(
                    bridgeId,
                    list.filter((item) => item.resolve !== resolve)
                );
                resolve(null); // timeout — fail-open
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

    // (Async credential application was removed — munging is now guaranteed
    //  synchronously inside createOffer/createAnswer via awaitShadowReady.)

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

    // ══ BISECT TOGGLES ═════════════════════════════════════════════════════════
    // Diagnostic switches to isolate WHAT in the munged offer trips the VDI's
    // reset of POST api.flightproxy.skype.com/api/v2/cpconv. Relay-only candidates
    // (below) did NOT fix it, so the trigger is in the OFFER BODY or is structural.
    // Flip ONE at a time, reload the extension, place one call, and check whether
    // cpconv still resets. All default to the normal (BCR-on) behavior.
    //
    //   BCR_SDP_PASSTHROUGH — return Teams' ORIGINAL offer untouched (no codec pin,
    //     no shadow munge, no candidate interception). This is effectively "extension
    //     loaded but SDP not modified". If cpconv SUCCEEDS → the reset is caused by our
    //     SDP modifications (bisect further with the two flags below). If cpconv STILL
    //     RESETS → BCR's SDP is NOT the trigger; the cause is structural (e.g. a
    //     conflict with the VDI's own Teams media-optimization) and we pivot.
    //     NOTE: with this on, media does NOT offload — it's a diagnostic only.
    //
    //   BCR_DISABLE_CODEC_PIN — keep shadow munge, but send Teams' full codec list
    //     instead of pinning to H264+opus. Tests whether the reduced codec set is the
    //     trigger (it's the biggest content difference vs a working non-BCR offer).
    //     Now also stops remote offers being scrubbed; before, it only affected the
    //     offers we create, so the pin was never fully off and the recorded result
    //     below was measured with remote-offer scrubbing still active.
    //     Safe to switch on for media reasons too — bcr_client no longer requires
    //     H264/Opus. See the BCR_PREFERRED_*_CODEC notes further down.
    //
    //   BCR_DISABLE_SHADOW_MUNGE — keep codec pin, but do NOT replace ice-ufrag/pwd/
    //     fingerprint/connection-address. Tests whether the credential munge is the
    //     trigger. NOTE: with this on, media does NOT offload — diagnostic only.
    const BCR_SDP_PASSTHROUGH = false;
    const BCR_DISABLE_CODEC_PIN = false;  // Test 2 done: codec pin is NOT the cpconv trigger
    const BCR_DISABLE_SHADOW_MUNGE = false;

    // Scrubs `raddr <private-ip> rport <port>` from any candidate line.
    // srflx candidates carry the STUN-derived public address as the main IP (safe),
    // but per RFC 5245/8445 they ALSO carry the private local base address in raddr
    // (the address the STUN mapping was discovered from) — same for relay candidates,
    // where raddr holds the local reflexive address behind the TURN allocation.
    // No-op if the line has no raddr token.
    function scrubCandidateRaddr(line) {
        // Replace: raddr <any-ip> rport <port>  →  raddr 0.0.0.0 rport <port>
        return line.replace(/(\braddr\s+)[^\s]+(\s+rport\b)/, '$10.0.0.0$2');
    }

    // Dispatches the synthetic end-of-candidates (null) event that tells Teams
    // ICE gathering has finished. Kept separate because it must be sendable on
    // its own — bcr_client signals it independently of any candidate, once
    // gathering completes or its deadline elapses.
    function dispatchEndOfCandidates(pc) {
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
                BCR_LOG('[BCR] Dispatched end-of-candidates (gathering complete)');
            } catch (e) {
                BCR_LOG('[BCR] end-of-candidates dispatch failed:', e);
            }
        });
    }

    // endOfCandidates must only be true when bcr_client has confirmed gathering
    // is finished. It used to be signalled after EVERY batch, which was harmless
    // while all candidates were sent as one batch — but now the srflx is sent as
    // soon as it is gathered and the rest trickle behind it, so signalling early
    // would tell Teams gathering was over while candidates were still coming,
    // and anything trickled afterwards could legitimately be ignored.
    function dispatchShadowTrickleCandidates(pc, candidates, endOfCandidates = false) {
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

                // Nothing else is filtered, and both of the candidate-type filters
                // that used to live here have been removed because their experiments
                // concluded:
                //
                //   srflx must NOT be dropped. It is bcr_client's only TURN-free
                //   routable candidate — its host candidates are a LAN the SFU
                //   cannot reach — so relay-only mode left the SFU with no path
                //   and ICE never connected.
                //
                //   host candidates are harmless to send. The "private IPs trip the
                //   VDI DPI" theory was disproven: a working non-BCR cpconv carried
                //   private host IPs (172.25.0.219) and connected fine. Normal Teams
                //   sends them, so mirroring that is the safe default.
                return true;
            });

            // Scrub raddr from every surviving candidate — srflx and relay both
            // carry a related-address that can leak a private LAN IP.
            candidateLines = candidateLines.map(scrubCandidateRaddr);
        }

        if (candidateLines.length === 0 && Array.isArray(candidates) && candidates.length > 0) {
            BCR_LOG('[BCR] WARNING: ALL', candidates.length,
                'shadow candidates were dropped by candidate filters.',
                'Teams will have no ICE candidates. Waiting for srflx/relay...',
                'Raw dropped candidates:', candidates);
            // Still close gathering if this was the final batch, otherwise Teams
            // would wait on an end-of-candidates event that never arrives.
            if (endOfCandidates) {
                dispatchEndOfCandidates(pc);
            }
            return; // nothing to trickle yet — srflx/relay may arrive later
        }

        let mid = '0';
        BCR_LOG('[BCR] Synthetic ICE Trickle queued',
            candidateLines.length, 'candidates (dropped',
            (candidates.length - candidateLines.length), 'host/srflx/IPv6)');
        // Print the EXACT strings handed to Teams — these end up verbatim in the
        // cpconv POST body, so this is what the VDI DPI actually inspects.
        // Verify here that no private IP survives in any addr or raddr field.
        candidateLines.forEach((line) => BCR_LOG('[BCR]   → dispatching:', line.trim()));

        // Dispatch synthetic candidates using queueMicrotask to avoid background tab setTimeout throttling (Efficiency Mode)
        candidateLines.forEach((line) => {
            queueMicrotask(() => {
                try {
                    let candidateStr = line.trim();
                    if (candidateStr.startsWith('a=')) {
                        candidateStr = candidateStr.substring(2); // strictly remove "a="
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

        // Close gathering only when bcr_client says there is nothing left to come.
        if (endOfCandidates) {
            dispatchEndOfCandidates(pc);
        } else {
            BCR_LOG('[BCR] Synthetic ICE Trickle dispatched', candidateLines.length,
                'candidates; gathering still open, awaiting more');
        }
    }

    // ── SDP Codec Pinning ──────────────────────────────────────────────────────
    // Constrains the SDP to one video codec (H264) and one audio codec (Opus),
    // so the SFU cannot switch codec mid-session.
    //
    // This is now a POLICY choice, not a correctness requirement. bcr_client used
    // to need it: its loopback built one track per payload type and dropped
    // anything outside an H264/Opus allowlist, so an unexpected codec meant
    // silence. It no longer does — the loopback carries one video and one audio
    // track, maps VP8/VP9/AV1/H265/Opus/G722/PCMU/PCMA onto them, and rebuilds a
    // track around whatever actually arrives if its initial guess was wrong (see
    // canonicalCodec and switchTrackCodecLocked in bcr_client/internal/loopback.go).
    //
    // What the pin still buys is a stable decoder in the WebView and no mid-call
    // codec change to renegotiate around. What it costs is that Teams may not use
    // VP9/AV1 even where they would be more efficient than H264 on this path.
    //
    // Note it does NOT pin to a single payload type: Teams offers H264 under
    // seven, differing in packetization-mode and profile-level-id, and all seven
    // survive here. That is harmless — bcr_client treats them as one codec — but
    // it does mean "pinned" means "one codec", not "one format".
    //
    // To turn the pin off, set BCR_DISABLE_CODEC_PIN below; it now covers both
    // the offers we create and the remote offers we answer.
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
        // Strip Teams' own candidates (they point at the VDI browser, not bcr_client)…
        const nl = munged.includes('\r\n') ? '\r\n' : '\n';
        munged = munged.replace(/^a=candidate:.*$/gm, '');
        munged = munged.replace(/^a=end-of-candidates.*$/gm, '');
        munged = munged.replace(/(\r?\n){3,}/g, `${nl}${nl}`);

        // …and INJECT bcr_client's own gathered candidates inline instead.
        //
        // ROOT CAUSE (proven by diffing the flightproxy /cpconv request body of a
        // failing BCR call vs a working non-BCR call): Teams builds its cpconv
        // call-setup POST from localDescription. If that SDP carries NO candidates,
        // Teams substitutes the placeholder `c=IN IP4 10.10.10.10` / `m=… 1234` /
        // `a=candidate:… 10.10.10.10 1234 typ host`, and the service RSTs the POST
        // (ERR_CONNECTION_RESET → call never places). The working non-BCR cpconv
        // carried inline candidates — including PRIVATE host IPs (172.25.0.219),
        // IPv6, and srflx with a private raddr — and connected fine. So private IPs
        // are NOT what the network resets on; an EMPTY/placeholder candidate set is.
        // Mirror normal Teams: put at least one real candidate inline. bcr_client
        // bundles its host candidates plus the first srflx into SHADOW_READY; any
        // remaining candidates trickle in afterwards.
        if (Array.isArray(shadow.candidates) && shadow.candidates.length > 0) {
            const candBlock = shadow.candidates
                .map((c) => (typeof c === 'string' ? c.trim() : ''))
                .filter((c) => c.length > 0)
                .map((c) => (c.startsWith('a=') ? c : 'a=' + c))
                .join(nl);
            if (candBlock) {
                // Inject into the FIRST BUNDLE m-section only (after its a=ice-pwd);
                // rtcp-mux + BUNDLE share one transport, exactly as normal Teams does.
                let injectedCands = false;
                munged = munged.replace(/(^a=ice-pwd:[^\r\n]+)(\r?\n)/m, (m0, pwd, newline) => {
                    if (injectedCands) return m0;
                    injectedCands = true;
                    return pwd + newline + candBlock + nl;
                });
            }
        }

        return munged;
    }

    function maybeEmitShadowClose(pc, reason) {
        const entry = ensureRtcState(pc);
        if (entry.closedEmitted) return;

        entry.closedEmitted = true;

        // Clear all cached state for this session.
        entry.shadowCredentials = null;
        entry.lastOriginalLocalSdp = null;
        entry.lastCreateActionType = null;
        entry.lastDispatchedTrickleId = null;
        entry.lastRemoteOfferSdp = null;
        entry._cachedPinnedSdp = null;

        // Unblock any pending awaitShadowReady() calls for this PC.
        rtcReadyByBridgeId.delete(entry.bridgeId);
        rejectShadowReady(entry.bridgeId, `pc_closed:${reason}`);

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
            const entry = ensureRtcState(pc);
            BCR_LOG('[BCR] RTCPeerConnection constructed bridgeId=', entry.bridgeId, 'iceServersCount=', args[0]?.iceServers?.length ?? 0);
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

        if (data.type === 'BCR_STATUS') {
            const connected = data.payload?.clientConnected;
            if (connected === false) {
                // Covers both cases: the client was never there (answer to the
                // startup query), and the client died mid-playback (pushed on
                // the bridge transition).
                restoreNativePlayback('bcr_client not reachable');
            } else if (connected === true) {
                BCR_LOG('[BCR] bcr_client reachable — media redirection active');
            }
            // Deliberately no auto re-enable: if the client comes back, resuming
            // redirection mid-playback would silence the page and jump the
            // picture to the overlay with no warning. A reload picks it up.
            return;
        }

        if (data.type === 'BCR_YT_PLAYBACK') {
            const p = data.payload || {};
            if (typeof p.bufferedEnd === 'number') bcrBufferedEnd = p.bufferedEnd;
            if (typeof p.playhead === 'number') {
                bcrPlayhead = p.playhead;
                noteBcrPlayhead(p.playhead); // detects a user scrub on bcr_client
            }
            bcrReportWall = performance.now();
            return;
        }

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
                // Unblock the awaitShadowReady() Promise inside createOffer/createAnswer.
                // Credentials will be stored on entry.shadowCredentials by patchedCreateAction
                // after awaitShadowReady() returns — do NOT store them here to avoid races.
                resolveShadowReady(bridgeId, payload);
            }
        }

        if (data.type === 'BCR_RTC_SHADOW_ERROR') {
            const payload = data.payload;
            const bridgeId = payload?.bridgeId;
            if (typeof bridgeId === 'string' && bridgeId.length > 0) {
                BCR_LOG('[BCR] Received SHADOW_ERROR bridgeId=', bridgeId, 'stage=', payload?.stage, 'reason=', payload?.reason);
                // Unblock the awaitShadowReady() Promise with a null (fail-open).
                rejectShadowReady(bridgeId, payload?.reason ?? 'shadow_error');
            }
        }

        if (data.type === 'BCR_RTC_SHADOW_ICE_CANDIDATE') {
            const payload = data.payload;
            const bridgeId = payload?.bridgeId;
            const candidateStr = payload?.candidate;
            const isEndOfCandidates = payload?.endOfCandidates === true;

            if (typeof bridgeId !== 'string' || bridgeId.length === 0) {
                return;
            }

            // End-of-candidates arrives as its own message with no candidate.
            if (isEndOfCandidates) {
                const pc = rtcPcByBridgeId.get(bridgeId);
                if (pc) {
                    BCR_LOG('[BCR] End-of-candidates received bridgeId=', bridgeId);
                    dispatchEndOfCandidates(pc);
                } else {
                    BCR_LOG('[BCR] End-of-candidates dropped — no PC for bridgeId=', bridgeId);
                }
                return;
            }

            if (typeof candidateStr === 'string' && candidateStr.length > 0) {
                const pc = rtcPcByBridgeId.get(bridgeId);
                if (pc) {
                    BCR_LOG('[BCR] Late ICE candidate received, trickle bridgeId=', bridgeId);
                    dispatchShadowTrickleCandidates(pc, [candidateStr], false);
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

        // ── Local Description Getters Interception ───────────────────────────
        // Teams and other WebRTC libraries read pc.localDescription / currentLocalDescription
        // to retrieve the SDP to send over signaling to the SFU. Since we pass
        // Chrome the original unmunged SDP in setLocalDescription (to keep Chrome's
        // ICE agent happy), we MUST intercept the getters to return the munged SDP.
        const origLocalDescGetter = Object.getOwnPropertyDescriptor(rtcProto, 'localDescription');
        const origCurrentLocalDescGetter = Object.getOwnPropertyDescriptor(rtcProto, 'currentLocalDescription');
        const origPendingLocalDescGetter = Object.getOwnPropertyDescriptor(rtcProto, 'pendingLocalDescription');

        function getMungedLocalDescription(origGetter, pc) {
            if (!origGetter) return undefined;
            const realDesc = origGetter.call(pc);
            if (!realDesc || typeof realDesc.sdp !== 'string') return realDesc;

            // Honor the bisect toggles: when munging is disabled, Teams' signaling
            // read of localDescription must also see the un-munged SDP, otherwise
            // the getter would silently re-munge what createOffer returned raw.
            if (BCR_SDP_PASSTHROUGH || BCR_DISABLE_SHADOW_MUNGE) return realDesc;

            const entry = rtcStateByPeer.get(pc);
            if (entry && entry.shadowCredentials && entry._cachedPinnedSdp) {
                const munged = mungeSdpTransport(entry._cachedPinnedSdp, entry.shadowCredentials);
                return buildDescriptionLike(realDesc, munged);
            }
            return realDesc;
        }

        if (origLocalDescGetter && origLocalDescGetter.get) {
            Object.defineProperty(rtcProto, 'localDescription', {
                get() { return getMungedLocalDescription(origLocalDescGetter.get, this); },
                configurable: true,
                enumerable: true,
            });
        }
        if (origCurrentLocalDescGetter && origCurrentLocalDescGetter.get) {
            Object.defineProperty(rtcProto, 'currentLocalDescription', {
                get() { return getMungedLocalDescription(origCurrentLocalDescGetter.get, this); },
                configurable: true,
                enumerable: true,
            });
        }
        if (origPendingLocalDescGetter && origPendingLocalDescGetter.get) {
            Object.defineProperty(rtcProto, 'pendingLocalDescription', {
                get() { return getMungedLocalDescription(origPendingLocalDescGetter.get, this); },
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

            // CRITICAL: Pass the ORIGINAL (un-munged) SDP to Chrome's native setLocalDescription.
            // Chrome's ICE agent validates that ice-ufrag/pwd match its internal state.
            // Munging is handled upstream in createOffer/createAnswer — by the time
            // Teams calls setLocalDescription, the description it passes has already
            // been munged for the SFU. We must give Chrome the ORIGINAL credentials.
            const origSdpString = entry.lastOriginalLocalSdp || description.sdp;
            const chromeSafeDescription = {
                type: description.type,
                sdp: origSdpString,
            };

            return origSetLocalDescription.apply(this, [chromeSafeDescription, ...args.slice(1)]);
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

                // Diagnostic: this fires the moment Teams applies the SFU's remote SDP.
                // For an offerer, a 'answer' here means the SFU answer DID arrive at the
                // browser (so if bcr_client never logs RTC_SHADOW_REMOTE, the loss is
                // between here and Go). If this never fires with type='answer', the SFU
                // answer never reached the browser at all — the offer/answer exchange failed.
                BCR_LOG('[BCR] setRemoteDescription received type=', description.type,
                    'sdpLen=', description.sdp.length, 'bridgeId=', entry.bridgeId);

                const descType = description.type.toLowerCase();
                let effectiveRemoteSdp = description.sdp;

                // Renegotiation pinning: constrain a remote offer to the preferred
                // codecs before the browser answers it, so the answer — and
                // therefore what the SFU goes on to send — stays on one codec.
                //
                // This MUST honour the same toggles as the createOffer path below.
                // It did not, which meant BCR_DISABLE_CODEC_PIN and
                // BCR_SDP_PASSTHROUGH only ever disabled half the pinning: our own
                // offers went out with the full codec list while remote offers were
                // still being scrubbed to H264. Any earlier bisect result recorded
                // against those flags was measured in that half-on state.
                if (descType === 'offer' && !BCR_SDP_PASSTHROUGH && !BCR_DISABLE_CODEC_PIN) {
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

                    // Swallowed to keep an unhandled rejection from breaking Teams'
                    // UI. The BCR_LOG above is the record — a BCR_SHADOW_DIAGNOSTIC
                    // event used to be emitted here too, but bootstrap.js forwards
                    // only the fixed BCR_RTC_SHADOW_* set, so nothing ever saw it.
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

            // ── BISECT: full passthrough ──────────────────────────────────────────────
            // Return Teams' original offer with zero BCR modification. Diagnostic only.
            if (BCR_SDP_PASSTHROUGH) {
                BCR_LOG('[BCR] BISECT: BCR_SDP_PASSTHROUGH on —', actionType,
                        'returned UNMODIFIED (no codec pin, no munge, no trickle). Media will NOT offload.');
                return origDescription;
            }

            // ── SDP Codec Pinning ─────────────────────────────────────────────────────
            const pinnedSdp = BCR_DISABLE_CODEC_PIN
                ? origDescription.sdp
                : filterSdpToPreferredCodecs(origDescription.sdp);
            if (BCR_DISABLE_CODEC_PIN) {
                BCR_LOG('[BCR] BISECT: BCR_DISABLE_CODEC_PIN on — sending full codec list for', actionType);
            }
            entry._cachedPinnedSdp = pinnedSdp;

            captureIceServersFromConnection(this);

            // ── Emit SHADOW_LOCAL to Go ───────────────────────────────────────────────
            // Emit here (not in setLocalDescription) so the await below can block
            // before Teams gets the SDP — guaranteeing the munge always applies.
            if (actionType === 'offer') {
                // Invalidate any stale SHADOW_READY so we wait for fresh credentials.
                rtcReadyByBridgeId.delete(entry.bridgeId);
            }

            emitShadowEvent('BCR_RTC_SHADOW_LOCAL', {
                bridgeId: entry.bridgeId,
                sdpType: origDescription.type ?? actionType,
                sdp: pinnedSdp,
                iceServers: entry.iceServers ?? [],
                timestamp: Date.now(),
            });

            BCR_LOG('[BCR] Emitted SHADOW_LOCAL from', actionType, 'bridgeId=', entry.bridgeId,
                    '— blocking for SHADOW_READY');

            // ── BLOCKING WAIT ─────────────────────────────────────────────────────────
            // Awaits the Go SHADOW_READY signal. Go builds the ICE agent on demand
            // here and returns as soon as the first server-reflexive candidate is in
            // hand (normally tens of ms; capped by BCR_GATHER_WAIT_MS). Everything
            // gathered after that arrives as trickled RTC_SHADOW_ICE_CANDIDATE
            // messages, so this wait is short. RTC_WAIT_TIMEOUT_MS bounds it so Teams
            // can never hang indefinitely.
            const shadowReady = await awaitShadowReady(entry.bridgeId, RTC_WAIT_TIMEOUT_MS);

            // Verify the PC hasn't been torn down during the await.
            const currentEntry = ensureRtcState(this);
            if (currentEntry.bridgeId !== entry.bridgeId) {
                BCR_LOG('[BCR] BridgeId changed during', actionType, '; fail-open old=',
                        entry.bridgeId, 'new=', currentEntry.bridgeId);
                return buildDescriptionLike(origDescription, pinnedSdp);
            }

            if (!shadowReady) {
                const errReason = consumeShadowErrorReason(entry.bridgeId);
                BCR_LOG('[BCR] ⚠️', actionType, ': SHADOW_READY timeout/error, fail-open bridgeId=',
                        entry.bridgeId, 'reason=', errReason ?? 'timeout');
                // Fail-open: return the codec-pinned (but unmunged) SDP so Teams
                // can attempt native calling rather than getting stuck.
                return buildDescriptionLike(origDescription, pinnedSdp);
            }

            // Store credentials for downstream use (addIceCandidate swallow, state mocker, etc.)
            entry.shadowCredentials = shadowReady;

            // Start silent audio keepalive to prevent background-tab throttling.
            startSilentAudioKeepalive();

            // ── MUNGE ─────────────────────────────────────────────────────────────────
            // Replace ice-ufrag, ice-pwd, fingerprint, connection address (0.0.0.0),
            // and strip all a=candidate lines (trickled asynchronously below).
            if (BCR_DISABLE_SHADOW_MUNGE) {
                BCR_LOG('[BCR] BISECT: BCR_DISABLE_SHADOW_MUNGE on — returning codec-pinned but UN-munged',
                        actionType, '(original ICE/DTLS creds). Media will NOT offload.');
                return buildDescriptionLike(origDescription, pinnedSdp);
            }
            const mungedSdp = mungeSdpTransport(pinnedSdp, shadowReady);

            if (mungedSdp && mungedSdp !== pinnedSdp) {
                BCR_LOG('[BCR] ✅', actionType, ': SDP munged with shadow credentials bridgeId=',
                        entry.bridgeId, 'ufrag=', shadowReady.iceUfrag?.substring(0, 8) + '...');
                // Verifies THIS (browser) munge output, not Go's diagnostic twin.
                // endOfCandidates MUST be false — declaring it on a candidate-less
                // offer makes the SFU give up and never answer.
                BCR_LOG('[BCR] MUNGE-CHECK endOfCandidates=', /^a=end-of-candidates/m.test(mungedSdp),
                        'candidateLines=', (mungedSdp.match(/^a=candidate:/gm) || []).length,
                        'trickleOption=', /a=ice-options:[^\r\n]*trickle/.test(mungedSdp),
                        'sdpLen=', mungedSdp.length);
            } else {
                BCR_LOG('[BCR] ⚠️', actionType, ': mungeSdpTransport returned unchanged SDP! bridgeId=',
                        entry.bridgeId,
                        'ufrag=', shadowReady.iceUfrag ?? 'null',
                        'fingerprint=', shadowReady.dtlsFingerprint ? 'present' : 'null',
                        'candidates=', shadowReady.candidates?.length ?? 0);
            }

            // ── Trickle candidates asynchronously (with 1500ms delay) ──────────────────
            // The initial SDP has zero candidates (DPI firewall bypass).
            // Trickle Go's candidates 1.5 seconds after returning the munged SDP to Teams.
            // This delay ensures the initial signaling POST (flightproxy cpconv)
            // completes successfully without candidate leakage, avoiding DPI resets.
            if (Array.isArray(shadowReady.candidates) && shadowReady.candidates.length > 0) {
                const pc = this;
                setTimeout(() => {
                    // Check if the PeerConnection was closed during the timeout
                    const currentEntry = rtcStateByPeer.get(pc);
                    if (!currentEntry || currentEntry.closedEmitted) return;

                    const genId = shadowReady.generatedAt || shadowReady.iceUfrag;
                    if (currentEntry.lastDispatchedTrickleId !== genId) {
                        currentEntry.lastDispatchedTrickleId = genId;
                        // Only close gathering here if bcr_client had already
                        // finished it; otherwise it sends an explicit
                        // end-of-candidates once it does.
                        dispatchShadowTrickleCandidates(
                            pc,
                            shadowReady.candidates,
                            shadowReady.gatheringComplete === true,
                        );
                    }
                }, 1500);
            }

            // Must return an object that circumvents strict instanceof RTCSessionDescription
            // checks inside RxJS / React data pipelines.
            return buildDescriptionLike(origDescription, mungedSdp ?? pinnedSdp);
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
            if (!_bypass && !BCR_DISABLE_APPEND_GATING && state.suppressedVideos.has(this)) {
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

    // ─── Virtual-clock property overrides ────────────────────────────────────
    // While the virtual clock drives the primary suppressed video, currentTime must
    // read as the advancing synthetic time and paused must read false — so YouTube's
    // streaming engine treats the element as actively playing and keeps fetching.
    // Gated on `virtualClock.video === this`, so every other media element (previews,
    // the WebRTC path, etc.) keeps 100% native behavior.
    if (currentTimeDesc && currentTimeDesc.get && currentTimeDesc.set) {
        Object.defineProperty(HTMLMediaElement.prototype, 'currentTime', {
            get() {
                if (!_bypass && virtualClock.started && virtualClock.video === this) {
                    const vt = getVirtualTime();
                    if (vt !== null) {
                        virtualClock.reads++;
                        return vt;
                    }
                }
                return currentTimeDesc.get.call(this);
            },
            set(value) {
                // A seek (user scrub or YouTube start-offset) re-anchors the clock…
                if (!_bypass && virtualClock.active && virtualClock.video === this) {
                    anchorVirtualTime(value);
                }
                // …and still applies natively so YouTube's own seek bookkeeping runs.
                return currentTimeDesc.set.call(this, value);
            },
            configurable: true,
            enumerable: true,
        });
    }

    if (pausedDesc && pausedDesc.get) {
        Object.defineProperty(HTMLMediaElement.prototype, 'paused', {
            get() {
                if (!_bypass && virtualClock.started && virtualClock.video === this) {
                    return false;
                }
                return pausedDesc.get.call(this);
            },
            configurable: true,
            enumerable: true,
        });
    }

    // readyState → HAVE_ENOUGH_DATA while the clock runs. The real SourceBuffers are
    // empty (we gate the native appendBuffer), so the element's native readyState stays
    // low — which tells YouTube the element can't actually be playing, so it drops back
    // to its small "buffer-ahead while paused" target and stops fetching. Reporting
    // HAVE_ENOUGH_DATA keeps YouTube in its full playing-state streaming loop.
    if (readyStateDesc && readyStateDesc.get) {
        Object.defineProperty(HTMLMediaElement.prototype, 'readyState', {
            get() {
                if (!_bypass && virtualClock.started && virtualClock.video === this) {
                    return HAVE_ENOUGH_DATA;
                }
                return readyStateDesc.get.call(this);
            },
            configurable: true,
            enumerable: true,
        });
    }

    // requestVideoFrameCallback / cancelVideoFrameCallback — synthesize frame callbacks
    // for the clock-driven primary; pass straight through for every other element.
    if (origRVFC) {
        HTMLVideoElement.prototype.requestVideoFrameCallback = function patchedRVFC(cb) {
            if (!_bypass && virtualClock.started && virtualClock.video === this && typeof cb === 'function') {
                const handle = RVFC_BASE + (++rvfcSeq);
                rvfcCallbacks.set(handle, cb);
                ensureSyntheticRVFCLoop();
                return handle;
            }
            return origRVFC.call(this, cb);
        };
        if (origCVFC) {
            HTMLVideoElement.prototype.cancelVideoFrameCallback = function patchedCVFC(handle) {
                if (typeof handle === 'number' && handle >= RVFC_BASE) {
                    rvfcCallbacks.delete(handle);
                    return;
                }
                return origCVFC.call(this, handle);
            };
        }
    }

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
                // Track MS → video for eager suppression lookup.
                if (value instanceof MediaSource) {
                    mediaSourceToVideo.set(value, this);
                }
                if (!_bypass && state.suppressedVideos.has(this)) {
                    if (value) {
                        // Re-stamp rather than mint a fresh UUID: YouTube reassigns
                        // srcObject several times during startup, and a new id each time
                        // read downstream as a new video.
                        assignVideoIdentity(this);
                        try { this.setAttribute('data-bcr-primary', '1'); } catch (_) {}
                    }
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
                // Track blob-URL MS → video for eager suppression lookup.
                if (value && blobUrlToMediaSource.has(value)) {
                    mediaSourceToVideo.set(blobUrlToMediaSource.get(value), this);
                }
                if (!_bypass && state.suppressedVideos.has(this)) {
                    if (value) {
                        assignVideoIdentity(this);
                        try { this.setAttribute('data-bcr-primary', '1'); } catch (_) {}
                    }
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
                sourceBufferToMediaSource.set(sb, this); // for eager-suppression reverse lookup
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
        // Eager suppression: when data first flows through an unsuppressed SourceBuffer,
        // immediately try to elect + suppress the primary video. This eliminates the
        // MutationObserver timing race where SourceBuffers are created before the video
        // element grows large enough to be scored by the DOM observer.
        if (!_bypass && !state.suppressedSourceBuffers.has(this)) {
            electPrimaryVideo();
        }

        // After a fallback the SourceBuffers stay in the suppressed set (WeakSet
        // has no clear()), so gate on the master switch as well. This makes the
        // append a plain pass-through: no chunks forwarded, no BCR bookkeeping.
        const isSuppressed = bcrRedirectEnabled && state.suppressedSourceBuffers.has(this);

        if (isSuppressed) {
            let isInitSegment = false;
            try {
                const view = toByteView(data);
                if (view) {
                    const copied = view.slice(); // fresh copy — original 'data' stays intact

                    const ms = sourceBufferToMediaSource.get(this);
                    const video = ms ? mediaSourceToVideo.get(ms) : null;
                    // Prefer YouTube's stable video id; the per-element UUID is only a
                    // fallback for non-YouTube pages (see currentYouTubeVideoId).
                    const videoId = currentYouTubeVideoId() ||
                        (video ? video.getAttribute('data-bcr-video-id') : null);

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

                    isInitSegment = detectInitSegment(copied);

                    // Forward only suppressed/redirected media chunks to the BCR pipeline.
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
                            videoId: videoId || 'unknown',
                            isInitSegment,
                            chunkBuffer: copied.buffer,
                        },
                        '*',
                        [copied.buffer]  // transfer ownership to content script
                    );

                    // diag: track chunk throughput so we can tell whether YouTube keeps
                    // feeding past the stall point (see fwdVideoChunks/fwdAudioChunks log).
                    if (meta.trackType === 'audio') fwdAudioChunks++;
                    else fwdVideoChunks++;
                    lastChunkWall = performance.now();

                    // First real (non-init) media chunk for the primary → begin
                    // advancing the virtual clock, in lock-step with actual streaming.
                    // A suppressed SourceBuffer always belongs to the primary, so fall
                    // back to the armed primary when the ms→video lookup misses (that
                    // lookup is why some chunks log videoId=unknown).
                    if (!isInitSegment) {
                        markPlaybackStarted(video || virtualClock.video);
                    }
                }
            } catch (_) { }

            // Pulse-decode (and the control test) let the real decoder receive the data so
            // the element actually plays and YouTube front-loads a full buffer. The native
            // append fires the real 'updateend', so we must NOT fire the synthetic one.
            if (BCR_PULSE_DECODE || BCR_DISABLE_APPEND_GATING) {
                return origAppendBuffer.call(this, data);
            }

            // Seek-pulse support: let the REAL SourceBuffer receive the (tiny) INIT
            // segment so the element gains metadata → duration → becomes SEEKABLE (the
            // seek pulse needs a seekable element to move the native clock). MEDIA
            // segments stay gated below, so no frames ever decode. The native append
            // fires its own 'updateend', so we do NOT add the synthetic one here.
            if (BCR_SEEK_PULSE_ENABLED && isInitSegment) {
                try {
                    origAppendBuffer.call(this, data);
                    return;
                } catch (_) {
                    // Real append rejected (e.g. SB busy) — fall through to the gated path.
                }
            }

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