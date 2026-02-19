const VideoRegistry = (()=>{
    const _registry = new Map();
    let _mutationObserver = null;

    function init() {
        console.log("[VideoRegistry] Initializing...");
        const existingVideos = document.querySelectorAll("video");
        existingVideos.forEach(_registerVideo);

        console.log(`[VideoRegistry] Found ${existingVideos.length} existing video(s)`);

        _startMutationObserver();
    }

    function destroy() {
        if(_mutationObserver) {
            _mutationObserver.disconnect();
            _mutationObserver = null;
        }

        _registry.forEach(({tracker})=> tracker.destroy());
        _registry.clear();
        console.log("[VideoRegistry] Destroyed all trackers.");
    }

    /**
   * Returns count of currently tracked videos.
   * Useful for debugging.
   *
   * @returns {number}
   */

    function count() {
        return _registry.size;
    }

    /**
   * Register a new video element.
   * Skips if already registered.
   *
   * @param {HTMLVideoElement} videoEl
   */

    function _registerVideo(videoE1) {
        if (_registry.has(videoE1)) return;
        const id = _generateId();
        const tracker = new VideoTracker(id, videoE1);

        _registry.set(videoE1, {id, tracker});
        Emitter.emitAdded(id);
        console.log(`[VideoRegistry] Registered video: ${id}`);
    }

    /**
   * Unregister a video element.
   * Destroys its tracker and clears state.
   *
   * @param {HTMLVideoElement} videoEl
   */

    function _unregisterVideo(videoE1) {
        const entry = _registry.get(videoE1);
        if(!entry) return;

        entry.tracker.destroy();
        _registry.delete(videoE1);
        Emitter.emitRemoved(entry.id);
        console.log(`[VideoRegistry] Unregistered video: ${entry.id}`);
    }

    function _startMutationObserver() {
        _mutationObserver = new MutationObserver((mutations) => {
            for (const mutation of mutations) {
                mutation.addedNodes.forEach((node)=>{
                    if(node.nodeType !== Node.ELEMENT_NODE) return;

                    if(node.tagName === "VIDEO") {
                        _registerVideo(node);
                    }

                    node.querySelectorAll?.("video").forEach(_registerVideo);
                });

                mutation.removedNodes.forEach((node)=> {
                    if(node.nodeType !== Node.ELEMENT_NODE) return;
                    if(node.tagName === "VIDEO") {
                        _unregisterVideo(node);
                    }

                    node.querySelectorAll?.("video").forEach(_unregisterVideo);
                });
            }
        });

        _mutationObserver.observe(document.body, {
            childList: true,
            subtree: true,
        });

        console.log("[VideoRegistry] MutationObserver started.");
    }

    /**
   * Generate a unique ID for a video element.
   * Uses crypto.randomUUID where available, falls back to timestamp+random.
   *
   * @returns {string}
   */

    function _generateId() {
        if(typeof crypto !== "undefined" && crypto.randomUUID){
            return crypto.randomUUID();
        }
        return `video-${Date.now()}-${Math.random().toString(36).slice(2, 9)}`;
    }

    return {init, destroy, count};


})();