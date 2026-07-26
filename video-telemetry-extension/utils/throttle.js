/**
 * Leading + trailing edge throttle, aligned to animation frames.
 *
 * The trailing edge matters here: these throttles gate overlay geometry updates,
 * and a leading-edge-only throttle discards the FINAL event of a burst. For a
 * scroll or a resize that means the last (settled) rect never gets sent, so the
 * overlay ends up parked on a stale intermediate position until some unrelated
 * event happens to nudge it. Always emitting the last call fixes that.
 *
 * @param {Function} fn
 * @param {number} limitMs
 * @returns {Function}
 */
function throttle(fn, limitMs = 33) {
    let lastCall = 0;
    let rafHandle = null;
    let trailingTimer = null;
    let pendingArgs = null;
    let pendingThis = null;

    function invoke(context, args) {
        lastCall = performance.now();
        if (rafHandle) cancelAnimationFrame(rafHandle);
        rafHandle = requestAnimationFrame(() => {
            rafHandle = null;
            fn.apply(context, args);
        });
    }

    return function throttled(...args) {
        const now = performance.now();
        const remaining = limitMs - (now - lastCall);

        if (remaining <= 0) {
            // Leading edge — a queued trailing call is now redundant.
            if (trailingTimer) {
                clearTimeout(trailingTimer);
                trailingTimer = null;
                pendingArgs = null;
                pendingThis = null;
            }
            invoke(this, args);
            return;
        }

        // Inside the window: remember the newest call and fire it when the
        // window expires. setTimeout (not rAF) so this still fires in a
        // background tab, where rAF is suspended.
        pendingArgs = args;
        pendingThis = this;
        if (!trailingTimer) {
            trailingTimer = setTimeout(() => {
                trailingTimer = null;
                const a = pendingArgs;
                const c = pendingThis;
                pendingArgs = null;
                pendingThis = null;
                if (a) invoke(c, a);
            }, remaining);
        }
    };
}
