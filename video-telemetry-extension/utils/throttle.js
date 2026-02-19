/**
 * @param {Function} fn
 * @param {number} limitMs
 * @returns {Function}
 */
function throttle(fn, limitMs = 33) {
    let lastCall = 0;
    let rafHandle = null;

    return function throttled(...args) {
        const now = performance.now();
        const remaining = limitMs - (now - lastCall);

        if(remaining <= 0) {
            if(rafHandle) {
                cancelAnimationFrame(rafHandle);
                rafHandle = null;
            }
            lastCall = now;
            rafHandle = requestAnimationFrame(() => {
                fn.apply(this, args);
                rafHandle = null;
            });
        }
    };
}