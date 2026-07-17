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
        if (!videoEl.hasAttribute('data-bcr-primary')) return;

        const id = videoEl.getAttribute('data-bcr-video-id');
        if (!id) return;

        const tracker = new VideoTracker(id, videoEl);
        this._registry.set(videoEl, {id, tracker});
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
                if (mutation.type === 'attributes' && (mutation.attributeName === 'data-bcr-primary' || mutation.attributeName === 'data-bcr-video-id')) {
                    const target = mutation.target;
                    if (target.tagName === 'VIDEO') {
                        if (target.hasAttribute('data-bcr-primary') && target.hasAttribute('data-bcr-video-id')) {
                            const currentId = target.getAttribute('data-bcr-video-id');
                            const registered = this._registry.get(target);
                            if (registered && registered.id !== currentId) {
                                // Reused player got a new video session ID. Clean up old session first.
                                this._unregisterVideo(target);
                            }
                            this._registerVideo(target);
                        } else {
                            this._unregisterVideo(target);
                        }
                    }
                    continue;
                }

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
            attributes: true,
            attributeFilter: ['data-bcr-primary', 'data-bcr-video-id'],
        });
    }

}

// Singleton static property
VideoRegistry._instance = null;