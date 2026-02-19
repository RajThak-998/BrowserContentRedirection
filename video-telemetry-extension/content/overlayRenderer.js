/**
 * OverlayRenderer
 *
 * Renders a visual highlight rectangle over each tracked video element.
 * Used for presentation and debugging — makes tracked videos visually obvious.
 *
 * Each overlay:
 *  - Blue border rectangle over the video
 *  - Small label showing video ID
 *  - Warns visually when video is out of viewport
 *  - Shows partial visibility state
 *
 * Does NOT affect playback or page layout.
 * Uses fixed positioning to sit above the video.
 */

const OverlayRenderer = (() => {

  // Map<videoId, HTMLElement>
  const _overlays = new Map();

  const STYLES = {
    VISIBLE: {
      border: "2px solid #3ea6ff",
      background: "rgba(62, 166, 255, 0.08)",
      labelBg: "#3ea6ff",
      labelColor: "#000",
    },
    PARTIAL: {
      border: "2px solid #f9a825",
      background: "rgba(249, 168, 37, 0.08)",
      labelBg: "#f9a825",
      labelColor: "#000",
    },
    OUT_OF_VIEW: {
      border: "2px solid #e53935",
      background: "rgba(229, 57, 53, 0.08)",
      labelBg: "#e53935",
      labelColor: "#fff",
    },
  };

  // ─── Public API ──────────────────────────────────────────────────────────

  /**
   * Create an overlay for a newly detected video.
   *
   * @param {string} videoId
   * @param {HTMLVideoElement} videoEl
   */
  function create(videoId, videoEl) {
    if (_overlays.has(videoId)) return;

    // ── Wrapper ──
    const wrapper = document.createElement("div");
    wrapper.setAttribute("data-bcr-overlay", videoId);
    wrapper.style.cssText = `
      position: fixed;
      pointer-events: none;
      z-index: 2147483647;
      box-sizing: border-box;
      transition: border-color 0.2s ease, background 0.2s ease;
    `;

    // ── Label ──
    const label = document.createElement("div");
    label.setAttribute("data-bcr-label", "");
    label.style.cssText = `
      position: absolute;
      top: 0;
      left: 0;
      padding: 2px 6px;
      font-size: 10px;
      font-family: monospace;
      font-weight: bold;
      border-radius: 0 0 4px 0;
      white-space: nowrap;
      transition: background 0.2s ease, color 0.2s ease;
    `;
    label.textContent = `BCR: ${videoId.slice(0, 8)}`;

    // ── Warning Banner ──
    const warning = document.createElement("div");
    warning.setAttribute("data-bcr-warning", "");
    warning.style.cssText = `
      position: absolute;
      bottom: 0;
      left: 0;
      right: 0;
      padding: 4px 8px;
      font-size: 11px;
      font-family: monospace;
      font-weight: bold;
      text-align: center;
      display: none;
      transition: background 0.2s ease;
    `;

    wrapper.appendChild(label);
    wrapper.appendChild(warning);
    document.documentElement.appendChild(wrapper);

    _overlays.set(videoId, { wrapper, label, warning, videoEl });

    // Initial position sync
    _syncPosition(videoId);

    console.log(`[OverlayRenderer] Created overlay for: ${videoId.slice(0, 8)}`);
  }

  /**
   * Update overlay position and state from telemetry data.
   * Called every time VideoTracker fires an update.
   *
   * @param {string} videoId
   * @param {object} state - Full state from StateManager
   */
  function update(videoId, state) {
    const entry = _overlays.get(videoId);
    if (!entry) return;

    const { wrapper, label, warning } = entry;
    const { bounds, visibility } = state;

    // ── Position & Size ──
    wrapper.style.left   = `${bounds.x}px`;
    wrapper.style.top    = `${bounds.y}px`;
    wrapper.style.width  = `${bounds.width}px`;
    wrapper.style.height = `${bounds.height}px`;

    // ── Visual State ──
    if (!visibility.inViewport) {
      // Fully out of viewport
      _applyStyle(wrapper, label, warning, STYLES.OUT_OF_VIEW);
      warning.style.display = "block";
      warning.style.background = "#e53935";
      warning.style.color = "#fff";
      warning.textContent = "⚠ VIDEO OUT OF VIEW";

    } else if (visibility.intersectionRatio < 1.0 && visibility.intersectionRatio > 0) {
      // Partially visible
      _applyStyle(wrapper, label, warning, STYLES.PARTIAL);
      warning.style.display = "block";
      warning.style.background = "#f9a825";
      warning.style.color = "#000";
      warning.textContent = `◑ PARTIAL — ${(visibility.intersectionRatio * 100).toFixed(0)}% visible`;

    } else {
      // Fully visible
      _applyStyle(wrapper, label, warning, STYLES.VISIBLE);
      warning.style.display = "none";
      warning.textContent = "";
    }
  }

  /**
   * Remove and destroy overlay for a video.
   * Called when video is removed from DOM.
   *
   * @param {string} videoId
   */
  function destroy(videoId) {
    const entry = _overlays.get(videoId);
    if (!entry) return;

    entry.wrapper.remove();
    _overlays.delete(videoId);

    console.log(`[OverlayRenderer] Destroyed overlay for: ${videoId.slice(0, 8)}`);
  }

  /**
   * Destroy all overlays.
   * Called on page unload.
   */
  function destroyAll() {
    _overlays.forEach((_, videoId) => destroy(videoId));
  }

  // ─── Internal ────────────────────────────────────────────────────────────

  /**
   * Sync overlay position directly from video element bounds.
   * Used on initial creation before first telemetry update arrives.
   *
   * @param {string} videoId
   */
  function _syncPosition(videoId) {
    const entry = _overlays.get(videoId);
    if (!entry) return;

    const rect = entry.videoEl.getBoundingClientRect();
    entry.wrapper.style.left   = `${rect.left}px`;
    entry.wrapper.style.top    = `${rect.top}px`;
    entry.wrapper.style.width  = `${rect.width}px`;
    entry.wrapper.style.height = `${rect.height}px`;
  }

  /**
   * Apply a color style set to overlay elements.
   *
   * @param {HTMLElement} wrapper
   * @param {HTMLElement} label
   * @param {HTMLElement} warning
   * @param {object} style
   */
  function _applyStyle(wrapper, label, warning, style) {
    wrapper.style.border     = style.border;
    wrapper.style.background = style.background;
    label.style.background   = style.labelBg;
    label.style.color        = style.labelColor;
  }

  return { create, update, destroy, destroyAll };
})();