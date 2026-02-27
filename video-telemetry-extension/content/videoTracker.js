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
                this._isFullscreen = data.isFullscreen;
                this._onObserverFired(data);
            }
        );
        this._cleanups.push(cleanupFullscreen);

        const cleanupPlayback = Observers.watchPlayback(this.videoEl, (data) => {
            this._onObserverFired(data);
        });
        this._cleanups.push(cleanupPlayback);

        console.log(`[VideoTracker] Initialized tracker for video: ${this.videoId}`);
    }

    /**
     * Read current DOM state of the video element.
     * Called every time any observer fires.
     *
     * getBoundingClientRect() is used for position — it accounts for:
     *  - Scroll position
     *  - CSS transforms
     *  - Element visibility in viewport
     *
     * @returns {object} Current state snapshot
     */
    _readState() {
        const rect = this.videoEl.getBoundingClientRect();

        // ── Screen-absolute conversion ────────────────────────────────────────
        // getBoundingClientRect() returns viewport-relative CSS pixels.
        // GLFW SetPos needs screen-absolute pixels.
        //
        // window.screenX/Y — browser window's top-left corner on the screen.
        // chromeUIHeight   — space consumed by tabs + address bar + bookmarks bar.
        //                    outerHeight includes all chrome UI.
        //                    innerHeight is the pure viewport.
        const chromeUIHeight = window.outerHeight - window.innerHeight;
        const screenX = window.screenX;
        const screenY = window.screenY;

        return {
            bounds: {
                x: rect.left  + screenX,
                y: rect.top   + screenY + chromeUIHeight,
                width:  rect.width,
                height: rect.height,
            },
            visibility: {
                intersectionRatio: this._lastVisibility.intersectionRatio,
                inViewport: this._lastVisibility.inViewport,
            },
            playback: {
                state: this.videoEl.paused ? "paused" : "playing",
                currentTime: this.videoEl.currentTime,
                rate: this.videoEl.playbackRate,
            },
            // Re-verify at read time — catches cases where event
            // fired but _isFullscreen cache was stale
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
            } catch (err) {
                console.warn(`[VideoTracker] Cleanup error for ${this.videoId}:`, err);
            }
        });

        this._cleanups = [];

        // 6. StateManager cleared
        this.stateManager.clear();

        // 7. Overlay destroyed
        OverlayRenderer.getInstance().destroy(this.videoId);

        console.log(`[VideoTracker] Destroyed tracker for video: ${this.videoId}`);
    }
}