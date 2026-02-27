// instance based - not a singleton class

const POSITION_DELTA_THRESHOLD = 1;     // 1px for jitter tolerance

class StateManager {
    constructor() {
        // Holds the last known state for this video
        this.prevState = null;
    }

    /**
     * Check if the new state is meaningfully different from the last known state.
     * Returns delta object if emission is warranted, null otherwise.
     *
     * @param {object} newState
     * @returns {{ delta: object } | null}
     */
    computeDelta(newState) {
        if (!newState || !newState.bounds) return null;

        const prev = this.prevState;

        if (!prev) {
            this.prevState = newState;
            return this._buildDelta(null, newState);
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

        this.prevState = newState;
        return this._buildDelta({dx, dy, dw, dh}, newState);
    }

    /**
     * Build a delta object to attach to the emitted event.
     *
     * @param {{ dx, dy, dw, dh } | null} delta
     * @param {object} newState
     * @returns {object}
     */
    _buildDelta(delta, newState) {
        return {
            ...newState,
            delta: delta ?? {dx: 0, dy: 0, dw: 0, dh: 0},
        };
    }

    /**
     * Reset internal state — called on tracker destroy.
     */
    clear() {
        this.prevState = null;
    }
}