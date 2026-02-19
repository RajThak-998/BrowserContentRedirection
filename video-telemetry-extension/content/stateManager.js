const POSITION_DELTA_THRESHOLD = 1;
const StateManager = (() => {
    const _states = new Map();

    /**
   * Check if the new state is meaningfully different from the last known state.
   * Returns delta object if emission is warranted, null otherwise.
   *
   * @param {string} videoId
   * @param {object} newState
   * @returns {{ delta: object } | null}
   */

    function computeDelta(videoId, newState) {
        const prev = _states.get(videoId);

        if (!prev) {
            _states.set(videoId, newState);
            return buildDelta(null, newState);
        }

        const dx = newState.bounds.x - prev.bounds.x;
        const dy = newState.bounds.y - prev.bounds.y;
        const dw = newState.bounds.width - prev.bounds.width;
        const dh = newState.bounds.height - prev.bounds.height;

        const positionChanged =
            Math.abs(dx) > POSITION_DELTA_THRESHOLD ||
            Math.abs(dy) > POSITION_DELTA_THRESHOLD ||
            Math.abs(dw) > POSITION_DELTA_THRESHOLD ||
            Math.abs(dh) > POSITION_DELTA_THRESHOLD;

        const playbackChanged = prev.playback.state !== newState.playback.state;
        const fullscreenChanged = prev.fullscreen !== newState.fullscreen;

        // Fix: only treat as visibility change when ratio also agrees
        const visibilityChanged =
            prev.visibility.inViewport !== newState.visibility.inViewport &&
            (newState.visibility.intersectionRatio > 0 || !newState.visibility.inViewport);

        const shouldEmit = positionChanged || playbackChanged || fullscreenChanged || visibilityChanged;

        if (!shouldEmit) return null;

        _states.set(videoId, newState);
        return buildDelta({dx, dy, dw, dh}, newState);
    }
    /**
   * Build a delta object to attach to the emitted event.
   *
   * @param {{ dx, dy, dw, dh } | null} delta
   * @param {object} newState
   * @returns {object}
   */

    function buildDelta(delta, newState) {
        return {
            ...newState,
            delta: delta ?? {dx: 0, dy: 0, dw: 0, dh: 0},
        };
    }

    /**
   * Remove state for a video (called on removal).
   *
   * @param {string} videoId
   */

    function clearState(videoId) {
        _states.delete(videoId);
    }

    /**
   * Check if a video has any tracked state.
   *
   * @param {string} videoId
   * @returns {boolean}
   */

    function hasState(videoId) {
        return _states.has(videoId);
    }

    return { computeDelta, clearState, hasState };
})();