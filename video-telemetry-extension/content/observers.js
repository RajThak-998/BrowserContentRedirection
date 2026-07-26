// Observers — utility module with plain functions.
// Not a class. Not a singleton.
// Kept as a plain object for the same public API surface.
const Observers = (() => {
    /**
     * Watch a video element for size changes.
     * Fires callback when element dimensions change.
     *
     * @param {HTMLVideoElement} videoEl
     * @param {Function} callback
     * @returns {ResizeObserver} - Call .disconnect() to stop
     */
    function watchResize(videoEl, callback) {
        const observer = new ResizeObserver((entries) => {
            for (const entry of entries) {
                if (entry.target === videoEl) {
                    callback({reason: "resize", entry});
                }
            }
        });

        observer.observe(videoEl);
        return observer;
    }

    /**
     * Watch a video element for viewport visibility changes.
     * Fires callback with intersection data when visibility ratio changes.
     *
     * threshold array gives us granular visibility reporting,
     * not just in/out binary.
     *
     * @param {HTMLVideoElement} videoEl
     * @param {Function} callback
     * @returns {IntersectionObserver} - Call .disconnect() to stop
     */
    function watchVisibility(videoEl, callback) {
        const thresholds = [0, 0.1, 0.25, 0.5, 0.75, 1.0];
        const observer = new IntersectionObserver((entries) => {
            for (const entry of entries) {
                if (entry.target === videoEl) {
                    callback({
                        reason: "visibility",
                        intersectionRatio: entry.intersectionRatio,
                        inViewport: entry.isIntersecting,
                    });
                }
            }
        }, {threshold: thresholds});

        observer.observe(videoEl);
        return observer;
    }

    /**
     * Watch scroll events to recompute bounding box position.
     * Uses throttle to cap update frequency at 30fps.
     *
     * Scroll does NOT change element size — only its position
     * relative to viewport. getBoundingClientRect() handles this.
     *
     * @param {Function} callback
     * @returns {Function} cleanup - Call to remove listener
     */
    function watchScroll(callback) {
        const handler = throttle(() => {
            callback({reason: "scroll"});
        }, 33);

        window.addEventListener("scroll", handler, {passive: true});

        return () => window.removeEventListener("scroll", handler);
    }

    /**
     * Watch whether the page is being displayed at all.
     *
     * visibilitychange covers the browser window being minimized, the tab being
     * switched away from, and Chrome's native window-occlusion detection. None of
     * this was observed before, so the overlay stayed on screen (showing a stale
     * or black frame) over a YouTube window the user had already put away.
     *
     * @param {Function} callback
     * @returns {Function} cleanup
     */
    function watchPageVisibility(callback) {
        const handler = () => {
            callback({
                reason: "pagevisibility",
                visible: document.visibilityState === "visible",
            });
        };

        document.addEventListener("visibilitychange", handler);
        // pagehide fires on navigation away / bfcache entry, where
        // visibilitychange is not guaranteed to arrive first.
        window.addEventListener("pagehide", handler);

        return () => {
            document.removeEventListener("visibilitychange", handler);
            window.removeEventListener("pagehide", handler);
        };
    }

    // ─── Fullscreen Listener ──────────────────────────────────────────────────

    /**
     * Watch for fullscreen state changes using multiple strategies.
     *
     * Strategy 1: Native fullscreenchange event on document
     * Strategy 2: YouTube-specific class mutation on <html> or <body>
     * Strategy 3: Size heuristic — video fills window within 10px tolerance
     *
     * All strategies funnel into one callback.
     *
     * @param {HTMLVideoElement} videoEl
     * @param {Function} callback
     * @returns {Function} cleanup - Call to remove all listeners
     */
    function watchFullscreen(videoEl, callback) {
        const cleanups = [];

        // ── Strategy 1: Native fullscreenchange (all browsers) ──
        const nativeHandler = () => {
            const isFullscreen = _checkFullscreen(videoEl);
            callback({reason: "fullscreen", isFullscreen});
        };

        document.addEventListener("fullscreenchange", nativeHandler);
        document.addEventListener("webkitfullscreenchange", nativeHandler);
        cleanups.push(() => {
            document.removeEventListener("fullscreenchange", nativeHandler);
            document.removeEventListener("webkitfullscreenchange", nativeHandler);
        });

        // ── Strategy 2: MutationObserver on <html> and <body> ──
        // YouTube adds/removes classes like "no-scroll", "scrolling",
        // "player-full-bleed-container" to signal fullscreen state.
        const classMutationHandler = throttle(() => {
            const isFullscreen = _checkFullscreen(videoEl);
            callback({reason: "fullscreen", isFullscreen});
        }, 100);

        const classObserver = new MutationObserver(classMutationHandler);

        classObserver.observe(document.documentElement, {
            attributes: true,
            attributeFilter: ["class", "style"],
        });

        classObserver.observe(document.body, {
            attributes: true,
            attributeFilter: ["class", "style"],
        });

        cleanups.push(() => classObserver.disconnect());

        // ── Strategy 3: Periodic size heuristic check ──
        // Last resort fallback — checks every 500ms if video
        // fills the window. Catches cases where no events fire.
        const heuristicInterval = setInterval(() => {
            const isFullscreen = _checkFullscreen(videoEl);
            callback({reason: "fullscreen", isFullscreen});
        }, 500);

        cleanups.push(() => clearInterval(heuristicInterval));

        return () => cleanups.forEach((fn) => fn());
    }

    /**
     * Watch playback state events on the video element.
     *
     * Events tracked:
     *  - play        → state becomes "playing"
     *  - pause       → state becomes "paused"
     *  - seeking     → mid-seek position jump
     *  - ratechange  → playback speed changed
     *  - ended       → video finished
     *
     * timeupdate intentionally excluded here —
     * position is read directly in tracker on each cycle,
     * not event-driven (would be too noisy).
     *
     * @param {HTMLVideoElement} videoEl
     * @param {Function} callback
     * @returns {Function} cleanup - Call to remove all listeners
     */
    function watchPlayback(videoEl, callback) {
        const events = ["play", "pause", "seeking", "ratechange", "ended"];

        const handler = (event) => {
            callback({
                reason: "playback",
                eventType: event.type,
                currentTime: videoEl.currentTime,
                playbackRate: videoEl.playbackRate,
                paused: videoEl.paused,
                ended: videoEl.ended,
            });
        };

        events.forEach((evt) => videoEl.addEventListener(evt, handler));

        return () => {
            events.forEach((evt) => videoEl.removeEventListener(evt, handler));
        };
    }

    /**
     * Watch for YouTube player layout-mode changes: theater, miniplayer,
     * fullscreen, and SPA navigation.
     *
     * Without this, a theater or miniplayer toggle was only noticed indirectly —
     * by the ResizeObserver on the <video>, or by the 500ms fullscreen poll — so
     * the overlay lagged behind the transition and appeared to cut off. These are
     * the signals YouTube actually emits when the mode changes.
     *
     * @param {HTMLVideoElement} videoEl
     * @param {Function} callback - receives {reason:"layout", mode}
     * @returns {Function} cleanup
     */
    function watchLayoutMode(videoEl, callback) {
        const cleanups = [];

        // The observed attributes are noisy — #movie_player's class flips for
        // things like ytp-autohide every time the mouse idles over the player.
        // So we don't report every mutation; we derive a mode signature and only
        // report when THAT changes.
        let lastMode = _layoutModeSignature(videoEl);

        const check = () => {
            const mode = _layoutModeSignature(videoEl);
            if (mode === lastMode) return;
            lastMode = mode;
            callback({reason: "layout", mode});
        };

        // ── ytd-watch-flexy carries the theater/fullscreen flags as attributes ──
        // These live on the page container, not on the <video>, so nothing else
        // in the pipeline was observing them — theater toggles were previously
        // only noticed via the ResizeObserver or the 500ms fullscreen poll.
        const flexy = document.querySelector("ytd-watch-flexy");
        if (flexy) {
            const flexyObserver = new MutationObserver(check);
            flexyObserver.observe(flexy, {
                attributes: true,
                attributeFilter: [
                    "theater",
                    "fullscreen",
                    "is-two-columns_",
                    "player-fullscreen",
                    "hidden",
                ],
            });
            cleanups.push(() => flexyObserver.disconnect());
        }

        // ── #movie_player class flips on miniplayer / fullscreen enter+exit ──
        const player = videoEl.closest("#movie_player") ||
            document.querySelector("#movie_player");
        if (player) {
            const playerObserver = new MutationObserver(check);
            playerObserver.observe(player, {
                attributes: true,
                attributeFilter: ["class"],
            });
            cleanups.push(() => playerObserver.disconnect());
        }

        // ── SPA navigation ──
        // Forced (not signature-gated): the whole page layout is rebuilt, so the
        // rect moves even when the mode name is unchanged.
        const navHandler = () => {
            lastMode = _layoutModeSignature(videoEl);
            callback({reason: "layout", mode: "navigate"});
        };
        document.addEventListener("yt-navigate-finish", navHandler);
        cleanups.push(() => {
            document.removeEventListener("yt-navigate-finish", navHandler);
        });

        return () => cleanups.forEach((fn) => fn());
    }

    /**
     * A compact string describing the player's current layout mode. Used to tell
     * a real mode change from incidental class churn on the same elements.
     *
     * @param {HTMLVideoElement} videoEl
     * @returns {string}
     */
    function _layoutModeSignature(videoEl) {
        const flexy = document.querySelector("ytd-watch-flexy");
        const theater = flexy ? flexy.hasAttribute("theater") : false;
        const flexyFs = flexy ? flexy.hasAttribute("fullscreen") : false;

        const mini = !!videoEl.closest(
            ".ytp-miniplayer-ui, .ytp-miniplayer-view, .ytp-miniplayer-scrim"
        );

        const player = videoEl.closest("#movie_player") ||
            document.querySelector("#movie_player");
        const playerFs = player ? player.classList.contains("ytp-fullscreen") : false;

        return `t${+theater}|f${+(flexyFs || playerFs)}|m${+mini}`;
    }

    // ─── Fullscreen Detection Logic ───────────────────────────────────────────

    /**
     * Multi-strategy fullscreen check for a video element.
     * Returns true if ANY strategy confirms fullscreen.
     *
     * @param {HTMLVideoElement} videoEl
     * @returns {boolean}
     */
    function _checkFullscreen(videoEl) {
        // Strategy 1: Native API — direct video element
        if (document.fullscreenElement === videoEl) return true;

        // Strategy 2: Native API — any ancestor contains video
        if (
            document.fullscreenElement &&
            document.fullscreenElement.contains(videoEl)
        ) return true;

        // Strategy 3: Webkit fallback
        if (
            document.webkitFullscreenElement &&
            (document.webkitFullscreenElement === videoEl ||
                document.webkitFullscreenElement.contains(videoEl))
        ) return true;

        // Strategy 4: YouTube-specific class check on known containers
        const ytContainers = [
            document.querySelector("ytd-app"),
            document.querySelector(".html5-video-player"),
            document.querySelector("#movie_player"),
            document.querySelector(".html5-video-container"),
            videoEl.closest(".html5-video-player"),
            videoEl.closest("#movie_player"),
        ];

        for (const el of ytContainers) {
            if (!el) continue;
            const classList = el.classList;
            if (
                classList.contains("ytp-fullscreen") ||
                classList.contains("full-bleed") ||
                classList.contains("player-full-bleed-container")
            ) return true;
        }

        // Strategy 5: Size heuristic — video bounds fill window within 10px
        const rect = videoEl.getBoundingClientRect();
        const fillsWidth = Math.abs(rect.width - window.innerWidth) < 10;
        const fillsHeight = Math.abs(rect.height - window.innerHeight) < 10;
        const atOrigin = Math.abs(rect.top) < 10 && Math.abs(rect.left) < 10;

        if (fillsWidth && fillsHeight && atOrigin) return true;

        return false;
    }

    return {
        watchResize,
        watchVisibility,
        watchScroll,
        watchFullscreen,
        watchPlayback,
        watchLayoutMode,
        watchPageVisibility,
        _checkFullscreen,
    };
})();