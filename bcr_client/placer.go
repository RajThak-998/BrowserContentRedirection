package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// placer owns the overlay window's geometry.
//
// Telemetry arrives faster than a window can usefully be moved, so it has to be
// rate-limited — but the previous approach (drop any update arriving within 33ms
// of the last one, app.go:248) threw away the *final* rect of every burst, which
// left the overlay parked on a stale intermediate geometry until some unrelated
// event happened to nudge it. That is the "doesn't resize smoothly" symptom.
//
// This is a coalescing applier instead of a throttle: Update() only records the
// latest desired rect, and a ticker applies it. The last value of a burst is
// therefore always applied, at most one tick late.
//
// The loop also reconciles. Wails' own WM_DPICHANGED handler
// (internal/frontend/desktop/windows/window.go:240) re-applies the OS-suggested
// rect over ours whenever the window crosses a DPI boundary — i.e. exactly when
// the overlay moves between monitors of different scale. A one-shot applier
// would lose that race, so when idle we periodically compare the window's actual
// rect against the desired one and re-apply if they have diverged.
type placer struct {
	mu      sync.Mutex
	desired winRect
	have    bool
	dirty   bool
	visible bool

	hwnd       windowHandle
	lastApply  time.Time
	lastLookup time.Time
	warned     bool

	// viewport is the RDP session's client area on the local desktop; remote
	// coordinates are translated by its origin. Zero origin (or viewportKnown
	// false) means no translation — the pre-existing behaviour, which is correct
	// when the session is fullscreen on the primary monitor.
	viewport      winRect
	viewportKnown bool
	lastViewport  time.Time
}

// localRect translates a remote-space rect onto the local desktop.
func (p *placer) localRect(r winRect) winRect {
	p.mu.Lock()
	vp, known := p.viewport, p.viewportKnown
	p.mu.Unlock()

	if !known {
		return r
	}
	return winRect{
		Left:   r.Left + vp.Left,
		Top:    r.Top + vp.Top,
		Right:  r.Right + vp.Left,
		Bottom: r.Bottom + vp.Top,
	}
}

// refreshViewport re-locates the RDP session window. Returns true if the origin
// changed, meaning the overlay must be re-placed even though the video hasn't
// moved within the remote desktop.
func (p *placer) refreshViewport(now time.Time) bool {
	p.mu.Lock()
	if !p.lastViewport.IsZero() && now.Sub(p.lastViewport) < viewportPollInterval {
		p.mu.Unlock()
		return false
	}
	p.lastViewport = now
	prev, prevKnown := p.viewport, p.viewportKnown
	p.mu.Unlock()

	vp, ok := findRDPViewport()

	if !ok {
		if prevKnown {
			log.Printf("[placer] RDP session window no longer visible — placing without translation")
		}
		p.mu.Lock()
		p.viewportKnown = false
		p.mu.Unlock()
		return prevKnown
	}

	if prevKnown && prev == vp {
		return false
	}

	p.mu.Lock()
	p.viewport = vp
	p.viewportKnown = true
	p.mu.Unlock()

	log.Printf("[placer] RDP session viewport: %s — remote coordinates translated by (%d,%d)",
		vp, vp.Left, vp.Top)
	return true
}

const (
	// ~60Hz ceiling on actual SetWindowPos calls.
	placeInterval = 16 * time.Millisecond

	// How often to check for external interference while idle.
	reconcileEvery = 250 * time.Millisecond

	// SWP_ASYNCWINDOWPOS means an apply may not be reflected by GetWindowRect
	// immediately; don't treat a read this soon after an apply as divergence.
	reconcileSettle = 4 * placeInterval

	// How often to re-locate the RDP session window (it can be moved or resized
	// by the user at any time).
	viewportPollInterval = 400 * time.Millisecond

	// How often to retry finding the overlay window while it is unresolved.
	// Each attempt is a full EnumWindows sweep.
	hwndLookupInterval = 500 * time.Millisecond

	// Minimum on-screen extent (per axis) a rect must keep to be accepted.
	// Transient layout jumps report the tracked <video> rect far off-screen
	// (y=-510 was observed); those are ignored so the overlay holds its last
	// good position instead of sliding out of view.
	minOverlayVisiblePx = 60
)

func newPlacer() *placer {
	return &placer{}
}

// Update records the newest desired rect, in REMOTE (VDI desktop) coordinates.
// Translation to local screen coordinates happens at apply time, because the RDP
// window can move independently of the video. Called from the telemetry
// callback, so it must never block and never touch Win32.
func (p *placer) Update(r winRect) {
	if r.empty() {
		return
	}

	p.mu.Lock()
	if !p.have || r != p.desired {
		p.desired = r
		p.have = true
		p.dirty = true
	}
	p.mu.Unlock()
}

// SetVisible gates applies. While hidden there is no point moving the window,
// and doing so would fight the show/hide logic in app.go.
func (p *placer) SetVisible(v bool) {
	p.mu.Lock()
	p.visible = v
	if v {
		// Re-apply on the next tick so the window is already in the right place
		// when it becomes visible, rather than appearing at a stale position.
		p.dirty = p.have
	}
	p.mu.Unlock()

	if v {
		if hwnd, ok := p.resolveHWND(); ok {
			setTopmost(hwnd)
		}
	}
}

// Pending reports the rect that would be applied next, if any. Used by app.go to
// place the window before showing it.
func (p *placer) Pending() (winRect, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.desired, p.have
}

func (p *placer) resolveHWND() (windowHandle, bool) {
	p.mu.Lock()
	hwnd := p.hwnd
	lastLookup := p.lastLookup
	p.mu.Unlock()

	if hwnd != 0 && isWindowAlive(hwnd) {
		return hwnd, true
	}

	// A lookup is a full EnumWindows sweep, so don't repeat it on every tick
	// while the window doesn't exist yet.
	now := time.Now()
	if !lastLookup.IsZero() && now.Sub(lastLookup) < hwndLookupInterval {
		return 0, false
	}

	p.mu.Lock()
	p.lastLookup = now
	p.mu.Unlock()

	found, err := findOverlayHWND()
	if err != nil {
		p.mu.Lock()
		warned := p.warned
		p.warned = true
		p.mu.Unlock()
		if !warned {
			log.Printf("[placer] %v — overlay geometry will not be applied until the window exists", err)
		}
		return 0, false
	}

	p.mu.Lock()
	p.hwnd = found
	p.warned = false
	p.mu.Unlock()
	log.Printf("[placer] overlay window resolved hwnd=%#x", uintptr(found))
	return found, true
}

func (p *placer) run(ctx context.Context) {
	logVirtualScreen()

	ticker := time.NewTicker(placeInterval)
	defer ticker.Stop()

	lastReconcile := time.Now()

	for {
		select {
		case <-ctx.Done():
			return

		case now := <-ticker.C:
			p.mu.Lock()
			remote, have, dirty, visible := p.desired, p.have, p.dirty, p.visible
			p.dirty = false
			lastApply := p.lastApply
			p.mu.Unlock()

			if !have || !visible {
				continue
			}

			// The RDP window moving is as much a reason to re-place the overlay
			// as the video moving.
			if p.refreshViewport(now) {
				dirty = true
			}

			desired := p.localRect(remote)

			// Reject transient garbage (the tracked rect briefly reports far
			// off-screen during layout changes) and keep the last good position.
			if !acceptablePlacement(desired) {
				continue
			}

			hwnd, ok := p.resolveHWND()
			if !ok {
				continue
			}

			if !dirty {
				// Idle: only look for external interference, and only slowly.
				if now.Sub(lastReconcile) < reconcileEvery || now.Sub(lastApply) < reconcileSettle {
					continue
				}
				lastReconcile = now

				// Keep the overlay at the top of the topmost band even when the
				// geometry hasn't changed. Clicking the RDP client window (also
				// topmost in fullscreen) otherwise raises it above the overlay
				// and the video vanishes behind the session.
				setTopmost(hwnd)

				actual, err := getWindowRect(hwnd)
				if err != nil || actual == desired {
					continue
				}
				log.Printf("[placer] reconcile: window drifted to %s, restoring %s", actual, desired)
			}

			if err := applyRectPhysical(hwnd, desired); err != nil {
				log.Printf("[placer] SetWindowPos failed for %s: %v", desired, err)
				continue
			}

			p.mu.Lock()
			p.lastApply = now
			p.mu.Unlock()
		}
	}
}

// acceptablePlacement rejects rects that would leave the overlay effectively
// off-screen.
//
// The previous check (app.go:241) was `y+h < 60 || x+w < 60`, which assumes the
// desktop starts at (0,0). On a multi-monitor desktop with a monitor left of or
// above the primary one, valid coordinates are negative and were silently
// rejected — the overlay simply refused to follow the video to that monitor.
// Intersecting against the real virtual-screen bounds keeps the original intent
// without that assumption.
func acceptablePlacement(r winRect) bool {
	vs := virtualScreenRect()
	overlap := r.intersect(vs)
	return overlap.width() >= minOverlayVisiblePx && overlap.height() >= minOverlayVisiblePx
}
