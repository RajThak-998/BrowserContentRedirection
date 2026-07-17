class VideoRegistry {
    constructor() {
        this._registry = new Map();
        this._mutationObserver = null;
    }

    // ─── Singleton ────────────────────────────────────────────────────────────

    static getInstance() {
        if (!VideoRegistry._instance) {
            VideoRegistry._instance = new VideoRegistry();
        }
        return VideoRegistry._instance;
    }

    // ─── Public API ───────────────────────────────────────────────────────────

    init() {
        const existingVideos = document.querySelectorAll("video");
        existingVideos.forEach((videoEl) => this._registerVideo(videoEl));

        this._startMutationObserver();
    }

    destroy() {
        if (this._mutationObserver) {
            this._mutationObserver.disconnect();
            this._mutationObserver = null;
        }

        this._registry.forEach(({tracker}) => tracker.destroy());
        this._registry.clear();
    }

    /**
     * Returns count of currently tracked videos.
     * Useful for debugging.
     *
     * @returns {number}
     */
    count() {
        return this._registry.size;
    }

    // ─── Internal ─────────────────────────────────────────────────────────────

    /**
     * Register a new video element.
     * Skips if already registered.
     *
     * @param {HTMLVideoElement} videoEl
     */
    _registerVideo(videoEl) {
        if (this._registry.has(videoEl)) return;
        const id = this._generateId();
        const tracker = new VideoTracker(id, videoEl);

        this._registry.set(videoEl, {id, tracker});
        try {
            videoEl.setAttribute('data-bcr-video-id', id);
        } catch (_) {}
        Emitter.getInstance().emitAdded(id);
    }

    /**
     * Unregister a video element.
     * Destroys its tracker and clears state.
     *
     * @param {HTMLVideoElement} videoEl
     */
    _unregisterVideo(videoEl) {
        const entry = this._registry.get(videoEl);
        if (!entry) return;

        entry.tracker.destroy();
        this._registry.delete(videoEl);
        try {
            videoEl.removeAttribute('data-bcr-video-id');
        } catch (_) {}
        Emitter.getInstance().emitRemoved(entry.id);
    }

    _startMutationObserver() {
        this._mutationObserver = new MutationObserver((mutations) => {
            for (const mutation of mutations) {
                mutation.addedNodes.forEach((node) => {
                    if (node.nodeType !== Node.ELEMENT_NODE) return;

                    if (node.tagName === "VIDEO") {
                        this._registerVideo(node);
                    }

                    node.querySelectorAll?.("video").forEach((videoEl) => this._registerVideo(videoEl));
                });

                mutation.removedNodes.forEach((node) => {
                    if (node.nodeType !== Node.ELEMENT_NODE) return;

                    if (node.tagName === "VIDEO") {
                        this._unregisterVideo(node);
                    }

                    node.querySelectorAll?.("video").forEach((videoEl) => this._unregisterVideo(videoEl));
                });
            }
        });

        this._mutationObserver.observe(document.body, {
            childList: true,
            subtree: true,
        });
    }

    /**
     * Generate a unique ID for a video element.
     * Uses crypto.randomUUID where available, falls back to timestamp+random.
     *
     * @returns {string}
     */
    _generateId() {
        if (typeof crypto !== "undefined" && crypto.randomUUID) {
            return crypto.randomUUID();
        }
        return `video-${Date.now()}-${Math.random().toString(36).slice(2, 9)}`;
    }
}

// Singleton static property
VideoRegistry._instance = null;