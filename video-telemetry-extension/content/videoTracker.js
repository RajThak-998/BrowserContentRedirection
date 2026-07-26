// How long to track the video rect frame-by-frame after a layout-mode change.
// Comfortably covers YouTube's theater/miniplayer transition (~200-300ms).
const LAYOUT_BURST_MS = 450;

// Below this many CSS px of visible video (either axis) the overlay is hidden
// rather than shown as a sliver.
const MIN_VISIBLE_CSS_PX = 16;

class VideoTracker {
    /**
     * @param {string} videoId - Unique ID assigned by VideoRegistry
     * @param {HTMLVideoElement} videoEl - The video DOM element to track
     */
    constructor(videoId, videoEl) {
        this.videoId = videoId;
        this.videoEl = videoEl;

        // Each tracker owns its own StateManager instance
        this.stateManager = new StateManager();

        this._cleanups = [];

        this._lastVisibility = {
            intersectionRatio: 0,
            inViewport: false,
        };

        this._isFullscreen = false;

        // Layout-transition burst state — see _startLayoutBurst().
        this._burstRaf = null;
        this._burstDeadline = 0;

        this._init();
    }

    _init() {
        // Create visual overlay for this video
        OverlayRenderer.getInstance().create(this.videoId, this.videoEl);

        const resizeObserver = Observers.watchResize(this.videoEl, (data) => {
            this._onObserverFired(data);
        });
        this._cleanups.push(() => resizeObserver.disconnect());

        const intersectionObserver = Observers.watchVisibility(
            this.videoEl, (data) => {
                this._lastVisibility = {
                    intersectionRatio: data.intersectionRatio,
                    inViewport: data.inViewport,
                };
                this._onObserverFired(data);
            }
        );
        this._cleanups.push(() => intersectionObserver.disconnect());

        const cleanupScroll = Observers.watchScroll((data) => {
            this._onObserverFired(data);
        });
        this._cleanups.push(cleanupScroll);

        const cleanupFullscreen = Observers.watchFullscreen(
            this.videoEl,
            (data) => {
                // Trust the observer's computed result directly
                // _checkFullscreen already ran all strategies
                const changed = this._isFullscreen !== data.isFullscreen;
                this._isFullscreen = data.isFullscreen;
                this._onObserverFired(data);
                // Entering/leaving fullscreen animates too — track it per-frame
                // rather than waiting for the next poll. (This observer also
                // fires on a 500ms heartbeat, hence the changed check.)
                if (changed) this._startLayoutBurst("fullscreen");
            }
        );
        this._cleanups.push(cleanupFullscreen);

        const cleanupPlayback = Observers.watchPlayback(this.videoEl, (data) => {
            this._onObserverFired(data);
        });
        this._cleanups.push(cleanupPlayback);

        const cleanupLayout = Observers.watchLayoutMode(this.videoEl, (data) => {
            this._startLayoutBurst(data.mode);
        });
        this._cleanups.push(cleanupLayout);

        const cleanupPageVisibility = Observers.watchPageVisibility((data) => {
            // A burst here would be pointless (rAF is suspended while hidden) —
            // one emit is all that's needed to tell the client to hide.
            this._onObserverFired(data);
        });
        this._cleanups.push(cleanupPageVisibility);
        this._cleanups.push(() => this._stopLayoutBurst());
    }

    /**
     * Track the video rect every frame for a short window after a layout-mode
     * change.
     *
     * YouTube *animates* theater and miniplayer transitions over a few hundred
     * milliseconds. A single measurement taken when the mode flips describes the
     * rect before the animation, so the overlay would sit on stale geometry for
     * the whole transition and then snap — which reads as the video "cutting
     * off". Sampling per-frame follows the animation instead.
     *
     * The 1px delta gate in StateManager still applies, so frames where nothing
     * actually moved cost nothing.
     */
    _startLayoutBurst(reason) {
        const deadline = performance.now() + LAYOUT_BURST_MS;

        // Already bursting — just extend it (mode changes often arrive in pairs).
        if (this._burstRaf !== null) {
            this._burstDeadline = Math.max(this._burstDeadline, deadline);
            return;
        }

        this._burstDeadline = deadline;

        const step = () => {
            this._onObserverFired({reason: "layout-burst", mode: reason});
            if (performance.now() < this._burstDeadline) {
                this._burstRaf = requestAnimationFrame(step);
            } else {
                this._burstRaf = null;
            }
        };

        this._burstRaf = requestAnimationFrame(step);
    }

    _stopLayoutBurst() {
        if (this._burstRaf !== null) {
            cancelAnimationFrame(this._burstRaf);
            this._burstRaf = null;
        }
    }

    _readState() {
        const rect = this.videoEl.getBoundingClientRect();

        // ── Viewport-relative coords (CSS px) ────────────────────────────────
        // Used by OverlayRenderer for position:fixed DOM elements.
        // These are correct as-is from getBoundingClientRect().
        const bounds = {
            x:      rect.left,
            y:      rect.top,
            width:  rect.width,
            height: rect.height,
        };

        // ── Screen coords for the native overlay window (Chrome on Windows) ──
        // window.screenX/screenY, getBoundingClientRect() and outerHeight/
        // innerHeight are all reported in CSS pixels. The overlay is placed with
        // a single raw SetWindowPos (bcr_client/winplace_windows.go), which takes
        // absolute PHYSICAL pixels in virtual-screen coordinates — so everything
        // is scaled by dpr here.
        //
        // devicePixelRatio folds in both OS display scaling and page zoom, so this
        // stays correct if the user zooms the page. It assumes a uniform scale
        // factor across monitors, which holds for the setups this targets.
        //
        // x/y are deliberately NOT clamped to >= 0: a monitor placed left of or
        // above the primary one has legitimately negative screen coordinates.
        const dpr = window.devicePixelRatio || 1;
        const chromeUIHeight = Math.max(0, window.outerHeight - window.innerHeight);

        // The <video> rect in screen space, unclipped.
        const fullLeft = rect.left + window.screenX;
        const fullTop  = rect.top + window.screenY + chromeUIHeight;

        // ── Clip to the browser viewport ────────────────────────────────────
        // getBoundingClientRect() keeps reporting the element's box after it has
        // scrolled out of view, so placing the overlay on the raw rect let it
        // spill over Chrome's toolbar (and stay visible after the video was
        // scrolled away entirely). The overlay must only ever cover the pixels
        // the video actually occupies, so intersect with the content area.
        const vpLeft   = window.screenX;
        const vpTop    = window.screenY + chromeUIHeight;
        const vpRight  = vpLeft + window.innerWidth;
        const vpBottom = vpTop + window.innerHeight;

        const clipLeft   = Math.max(fullLeft, vpLeft);
        const clipTop    = Math.max(fullTop, vpTop);
        const clipRight  = Math.min(fullLeft + rect.width, vpRight);
        const clipBottom = Math.min(fullTop + rect.height, vpBottom);

        const clipW = clipRight - clipLeft;
        const clipH = clipBottom - clipTop;

        const screenBounds = {
            x:      Math.round(clipLeft * dpr),
            y:      Math.round(clipTop * dpr),
            width:  Math.round(Math.max(0, clipW) * dpr),
            height: Math.round(Math.max(0, clipH) * dpr),
        };

        // How the <video> inside the (now possibly clipped) overlay window must
        // be scaled and offset so the visible pixels still line up with the real
        // player. Expressed as ratios of the clipped box, so the client can apply
        // them as CSS percentages without caring about its own pixel density.
        const clip = (clipW > 0 && clipH > 0)
            ? {
                scaleX:  rect.width  / clipW,
                scaleY:  rect.height / clipH,
                offsetX: (clipLeft - fullLeft) / clipW,
                offsetY: (clipTop  - fullTop)  / clipH,
            }
            : {scaleX: 1, scaleY: 1, offsetX: 0, offsetY: 0};

        // onScreen drives overlay show/hide on the client. It is false when the
        // tab/window is not being displayed at all (covers minimize, background
        // tab, and Chrome's native occlusion detection) or when too little of the
        // video remains inside the viewport to be worth showing.
        const pageVisible = document.visibilityState === "visible";
        const onScreen = pageVisible &&
            clipW >= MIN_VISIBLE_CSS_PX &&
            clipH >= MIN_VISIBLE_CSS_PX;

        return {
            bounds,
            screenBounds,
            clip,
            onScreen,
            visibility: {
                intersectionRatio: this._lastVisibility.intersectionRatio,
                inViewport:        this._lastVisibility.inViewport,
            },
            playback: {
                state:       this.videoEl.paused ? "paused" : "playing",
                currentTime: this.videoEl.currentTime,
                rate:        this.videoEl.playbackRate,
            },
            fullscreen: this._isFullscreen || Observers._checkFullscreen(this.videoEl),
        };
    }

    /**
     * Called by every observer when anything changes.
     * Reads fresh state, runs delta check, emits if needed.
     *
     * This is the single funnel point — all observer events
     * collapse into one pipeline here.
     *
     * @param {object} _data - Raw observer event data (unused directly,
     *                          state is always re-read fresh from DOM)
     */
    _onObserverFired(_data) {
        const currentState = this._readState();

        // Instance-based delta — no videoId needed
        const delta = this.stateManager.computeDelta(currentState);

        if (delta === null) return;

        // Update visual overlay with new state
        OverlayRenderer.getInstance().update(this.videoId, delta);

        Emitter.getInstance().emitUpdate(this.videoId, delta);
    }

    destroy() {
        // 1. ResizeObserver.disconnect()
        // 2. IntersectionObserver.disconnect()
        // 3. Scroll listener removed
        // 4. Fullscreen cleanup called (interval + mutation + native listeners)
        // 5. Playback listeners removed
        // All 5 above are stored in this._cleanups and called here:
        this._cleanups.forEach((cleanup) => {
            try {
                cleanup();
            } catch (_) {
            }
        });

        this._cleanups = [];

        // 6. StateManager cleared
        this.stateManager.clear();

        // 7. Overlay destroyed
        OverlayRenderer.getInstance().destroy(this.videoId);

    }
}