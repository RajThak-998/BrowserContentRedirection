const Observers = (() => {
    /**
   * Watch a video element for size changes.
   * Fires callback when element dimensions change.
   *
   * @param {HTMLVideoElement} videoEl
   * @param {Function} callback
   * @returns {ResizeObserver} - Call .disconnect() to stop
   */

    function watchResize(videoE1, callback) {
        const observer = new ResizeObserver((entries)=> {
            for(const entry of entries) {
                if(entry.target === videoE1) {
                    callback({reason: "resize", entry});
                }
            }
        });

        observer.observe(videoE1);
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

    function watchVisibility(videoE1, callback) {
        const thresholds = [0, 0.1, 0.25, 0.5, 0.75, 1.0];
        const observer = new IntersectionObserver((entries)=> {
            for(const entry of entries) {
                if(entry.target === videoE1) {
                    callback({
                        reason: "visibility",
                        intersectionRatio: entry.intersectionRatio,
                        inViewport: entry.isIntersecting,
                    });
                }
            }
        }, {threshold: thresholds});

        observer.observe(videoE1);
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
        const handler = throttle(()=>{
            callback({reason: "scroll"});
        }, 33);

        window.addEventListener("scroll", handler, {passive:true});

        return ()=>window.removeEventListener("scroll", handler);
    }

    /**
   * Watch for fullscreen state changes.
   *
   * Listens on document level — not on video element directly.
   * Reason: YouTube and other sites fullscreen a wrapper div,
   * not the <video> element itself. Document-level catches both cases.
   *
   * Callback receives which element entered fullscreen (if any).
   *
   * @param {Function} callback
   * @returns {Function} cleanup - Call to remove listener
   */

    function watchFullscreen(callback) {
        const handler = () => {
            callback({
                reason: "fullscreen",
                fullscreenElement: document.fullscreenElement??null,
                isFullscreen: !!document.fullscreenElement,
            });
        };

        document.addEventListener("fullscreenchange", handler);

        document.addEventListener("webkitfullscreenchange", handler);

        return () => {
            document.removeEventListener("fullscreenchange", handler);
            document.removeEventListener("webkitfullscreenchange", handler);
        };
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

    function watchPlayback(videoE1, callback) {
        const events = ["play", "pause", "seeking", "ratechange", "ended"];

        const handler = (event)=>{
            callback({
                reason: "playback",
                eventType: event.type,
                currentTime: videoE1.currentTime,
                playbackRate: videoE1.playbackRate,
                paused: videoE1.paused,
                ended: videoE1.ended,
            });
        };

        events.forEach((evt)=> videoE1.addEventListener(evt, handler));

        return ()=>{
            events.forEach((evt)=> videoE1.removeEventListener(evt, handler));
        };
    }

    return {
        watchResize,
        watchVisibility,
        watchScroll,
        watchFullscreen,
        watchPlayback,
    };
})();