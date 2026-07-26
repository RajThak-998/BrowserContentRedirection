//go:build !windows

package main

// No-op placement layer for non-Windows builds. The overlay is a Windows-only
// feature (it exists to sit on top of an RDP client session), but keeping these
// stubs lets the rest of bcr_client cross-compile.

import (
	"errors"
	"fmt"
)

// Declared here too so main.go's windows.Options block compiles everywhere.
const overlayWindowClass = "BCROverlayWindow"

type windowHandle uintptr

type winRect struct {
	Left, Top, Right, Bottom int32
}

func (r winRect) width() int32  { return r.Right - r.Left }
func (r winRect) height() int32 { return r.Bottom - r.Top }
func (r winRect) empty() bool   { return r.width() <= 0 || r.height() <= 0 }

func (r winRect) String() string {
	return fmt.Sprintf("(%d,%d %dx%d)", r.Left, r.Top, r.width(), r.height())
}

func (r winRect) intersect(o winRect) winRect {
	return winRect{
		Left:   max32(r.Left, o.Left),
		Top:    max32(r.Top, o.Top),
		Right:  min32(r.Right, o.Right),
		Bottom: min32(r.Bottom, o.Bottom),
	}
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func min32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func findOverlayHWND() (windowHandle, error) {
	return 0, errors.New("overlay placement is only implemented on Windows")
}

func isWindowAlive(windowHandle) bool { return false }

func applyRectPhysical(windowHandle, winRect) error {
	return errors.New("overlay placement is only implemented on Windows")
}

func setTopmost(windowHandle) {}

func getWindowRect(windowHandle) (winRect, error) {
	return winRect{}, errors.New("overlay placement is only implemented on Windows")
}

func findRDPViewport() (winRect, bool) { return winRect{}, false }

// virtualScreenRect reports an unbounded desktop so the off-screen guard in
// placer.go never rejects anything on platforms without a real implementation.
func virtualScreenRect() winRect {
	const lim = 1 << 20
	return winRect{Left: -lim, Top: -lim, Right: lim, Bottom: lim}
}

func logVirtualScreen() {}
