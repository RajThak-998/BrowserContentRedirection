//go:build windows

package main

// Direct Win32 window placement for the overlay.
//
// Wails' runtime.WindowSetPosition/WindowSetSize cannot be used to place the
// overlay, for two reasons found by reading wails v2.12.0 winc/controlbase.go:
//
//   - SetPos (controlbase.go:275) does
//         info := getMonitorInfo(hwnd)          // MONITOR_DEFAULTTONEAREST
//         SetWindowPos(hwnd, HWND_TOP, info.RcWork.Left+x, info.RcWork.Top+y, ...)
//     i.e. the coordinates are relative to the work area of whatever monitor the
//     window is CURRENTLY on. On the primary monitor with a bottom taskbar that
//     origin is (0,0) and it looks absolute — but the moment the overlay lands on
//     a second monitor, every later update re-adds that monitor's origin and the
//     window walks off the desktop and can never come back. That is the
//     multi-monitor bug.
//
//   - SetPos passes HWND_TOP with no SWP_NOACTIVATE, so every position update is
//     an activation request. At telemetry rates that steals focus from the RDP
//     session continuously.
//
//   - SetSize (controlbase.go:211) does GetWindowRect + scaleWithWindowDPI +
//     MoveWindow, so a position+size pair costs three Win32 round-trips and shows
//     a visible move-then-resize tear.
//
// Everything here works in absolute physical (device) pixels in virtual-screen
// coordinates, which may legitimately be negative for monitors left of / above
// the primary one.

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// overlayWindowClass must match options.Windows.WindowClassName in main.go.
const overlayWindowClass = "BCROverlayWindow"

// windowHandle keeps placer.go free of any windows-only type.
type windowHandle = windows.HWND

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procEnumWindows                      = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessId         = user32.NewProc("GetWindowThreadProcessId")
	procGetClassNameW                    = user32.NewProc("GetClassNameW")
	procIsWindow                         = user32.NewProc("IsWindow")
	procSetWindowPos                     = user32.NewProc("SetWindowPos")
	procGetWindowRect                    = user32.NewProc("GetWindowRect")
	procGetSystemMetrics                 = user32.NewProc("GetSystemMetrics")
	procSetProcessDpiAwarenessContext    = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetThreadDpiAwarenessContext     = user32.NewProc("GetThreadDpiAwarenessContext")
	procGetAwarenessFromDpiAwarenessCtxt = user32.NewProc("GetAwarenessFromDpiAwarenessContext")
)

const (
	// (DPI_AWARENESS_CONTEXT)-4
	dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3)

	swpNoZOrder       = 0x0004
	swpNoActivate     = 0x0010
	swpNoMove         = 0x0002
	swpNoSize         = 0x0001
	swpNoOwnerZOrder  = 0x0200
	swpAsyncWindowPos = 0x4000

	hwndTopmost = ^uintptr(0) // (HWND)-1

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// winRect is a Win32 RECT in absolute physical pixels. Left/Top may be negative.
type winRect struct {
	Left, Top, Right, Bottom int32
}

func (r winRect) width() int32  { return r.Right - r.Left }
func (r winRect) height() int32 { return r.Bottom - r.Top }
func (r winRect) empty() bool   { return r.width() <= 0 || r.height() <= 0 }

func (r winRect) String() string {
	return fmt.Sprintf("(%d,%d %dx%d)", r.Left, r.Top, r.width(), r.height())
}

// intersect returns the overlap of two rects (possibly empty).
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

// init runs before wails.Run, so before any window exists — early enough for the
// process-wide DPI awareness call to take effect.
//
// The build/windows/wails.exe.manifest already declares permonitorv2, but that
// manifest is only embedded by `wails build`. A plain `go build` produces a
// DPI-unaware binary in which Windows virtualizes every coordinate we pass to
// SetWindowPos — silently breaking placement on any non-100% display. This call
// rescues that case and is a harmless no-op (ERROR_ACCESS_DENIED) when the
// manifest already applied.
func init() {
	if err := procSetProcessDpiAwarenessContext.Find(); err != nil {
		log.Printf("[winplace] SetProcessDpiAwarenessContext unavailable (pre-1703 Windows?): %v", err)
		return
	}
	if r, _, err := procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2); r == 0 {
		// Expected when the manifest already set awareness — not an error.
		log.Printf("[winplace] SetProcessDpiAwarenessContext declined (%v) — manifest likely already applied", err)
	}
	logProcessDpiAwareness()
}

// logProcessDpiAwareness reports the awareness actually in force. If this is not
// per-monitor, every geometry number later in the log is suspect, so it belongs
// at the top of the log rather than at the end of a debugging session.
//
// Note GetAwarenessFromDpiAwarenessContext cannot distinguish per-monitor v1 from
// v2 — both report PER_MONITOR_AWARE(2).
func logProcessDpiAwareness() {
	if err := procGetThreadDpiAwarenessContext.Find(); err != nil {
		return
	}
	ctx, _, _ := procGetThreadDpiAwarenessContext.Call()
	awareness, _, _ := procGetAwarenessFromDpiAwarenessCtxt.Call(ctx)

	name := "UNKNOWN"
	switch int32(awareness) {
	case -1:
		name = "INVALID"
	case 0:
		name = "UNAWARE — placement WILL be wrong on non-100% displays"
	case 1:
		name = "SYSTEM_AWARE — placement will be wrong on mixed-DPI setups"
	case 2:
		name = "PER_MONITOR_AWARE (v1 or v2)"
	}
	log.Printf("[winplace] process DPI awareness: %s", name)
}

// The EnumWindows callback is created exactly once and reused.
//
// windows.NewCallback allocates from a fixed-size, never-freed table (~2000
// slots for the whole process), so creating one per call would exhaust it and
// panic — findOverlayHWND is retried on a timer until the window exists. The
// enumeration state is package-level and guarded by enumMu instead of being
// captured in a closure.
var (
	enumMu       sync.Mutex
	enumSelfPID  uint32
	enumFound    windowHandle
	enumCallback uintptr
	enumCbOnce   sync.Once
)

func enumOverlayProc(hwnd uintptr, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid != enumSelfPID {
		return 1 // keep enumerating
	}

	var buf [256]uint16
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || windows.UTF16ToString(buf[:n]) != overlayWindowClass {
		return 1
	}

	enumFound = windowHandle(hwnd)
	return 0 // stop
}

// findOverlayHWND locates this process's Wails window.
//
// Wails v2 exposes no HWND, but it does let us name the window class
// (options.Windows.WindowClassName, read at window.go:82) and it creates exactly
// one top-level window with parent = nil. Filtering EnumWindows by both our PID
// and that class name is therefore deterministic, and stays correct even if a
// second bcr_client instance is running.
func findOverlayHWND() (windowHandle, error) {
	enumMu.Lock()
	defer enumMu.Unlock()

	enumCbOnce.Do(func() {
		enumCallback = windows.NewCallback(enumOverlayProc)
	})

	enumSelfPID = windows.GetCurrentProcessId()
	enumFound = 0

	procEnumWindows.Call(enumCallback, 0)

	if enumFound == 0 {
		return 0, errors.New("overlay window not found (class " + overlayWindowClass + ")")
	}
	return enumFound, nil
}

func isWindowAlive(hwnd windowHandle) bool {
	r, _, _ := procIsWindow.Call(uintptr(hwnd))
	return r != 0
}

// applyRectPhysical moves and sizes the window in one atomic operation, and
// re-asserts topmost z-order while it is at it.
//
// Flag choices:
//   - HWND_TOPMOST on every apply. WS_EX_TOPMOST (from the AlwaysOnTop option)
//     only puts the window in the topmost *band*; it does not keep it at the top
//     of that band. The RDP client window is itself topmost when running
//     fullscreen, so clicking it raised it above the overlay and the video
//     disappeared behind the session. Re-asserting on each apply (plus the
//     heartbeat in placer.go for when nothing is moving) keeps it on top.
//   - SWP_NOACTIVATE is mandatory. Without it every update yanks focus off the
//     RDP session window (this is what Wails' SetPos does wrong).
//   - SWP_ASYNCWINDOWPOS because the placer goroutine is not the window's UI
//     thread; without it a busy WebView2 blocks the placer.
func applyRectPhysical(hwnd windowHandle, r winRect) error {
	// Negative Left/Top are normal (monitors left of / above the primary).
	// uintptr(int32(-1600)) sign-extends to 0xFFFF_FFFF_FFFF_F9C0, and the Win32
	// ABI reads the low 32 bits, so the callee correctly sees -1600.
	ret, _, err := procSetWindowPos.Call(
		uintptr(hwnd),
		hwndTopmost,
		uintptr(r.Left),
		uintptr(r.Top),
		uintptr(r.width()),
		uintptr(r.height()),
		uintptr(swpNoActivate|swpNoOwnerZOrder|swpAsyncWindowPos),
	)
	if ret == 0 {
		return err
	}
	return nil
}

// setTopmost re-asserts topmost z-order without moving or resizing. Called on
// show, since other topmost windows (a fullscreen RDP client is itself topmost)
// can otherwise end up above the overlay.
func setTopmost(hwnd windowHandle) {
	procSetWindowPos.Call(
		uintptr(hwnd),
		hwndTopmost,
		0, 0, 0, 0,
		uintptr(swpNoMove|swpNoSize|swpNoActivate|swpNoOwnerZOrder),
	)
}

func getWindowRect(hwnd windowHandle) (winRect, error) {
	var r winRect
	ret, _, err := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	if ret == 0 {
		return winRect{}, err
	}
	return r, nil
}

// ─── RDP session viewport ────────────────────────────────────────────────────
//
// The extension measures the video in the VDI's OWN desktop coordinate space.
// That desktop is a virtual framebuffer with its origin at (0,0); it has no idea
// which physical monitor you happen to be viewing it on. bcr_client then applies
// those coordinates to the LOCAL desktop, where (0,0) is by definition the
// top-left of the primary monitor.
//
// So when the RDP session is displayed on a secondary monitor, a video at remote
// (100, 200) is placed at local (100, 200) — on the primary monitor — while the
// session it should cover is over at local (2020, 200). The overlay is off by
// exactly the origin of the RDP client's content area.
//
// The fix is to translate remote → local:
//
//	local = rdpClientOrigin + remote        (assuming 1:1 pixel scale)
//
// When the session is fullscreen on the primary monitor rdpClientOrigin is
// (0,0), so this is a no-op and behaviour is unchanged from before.
//
// NOT handled: smart-sizing / scaled sessions, where the client area is a
// different size than the remote resolution and a scale factor would be needed
// as well.

// rdpClientProcesses are the executables that host an RDP session view. Matching
// on process name rather than window class is deliberate: the window classes are
// undocumented and differ between mstsc and the newer store/MSIX clients,
// whereas the process names are stable and observable.
var rdpClientProcesses = []string{
	"mstsc.exe",    // classic Remote Desktop Connection
	"msrdc.exe",    // Windows App / Remote Desktop client (AVD, W365)
	"msrdcw.exe",   // Windows App windowed host
	"rdclient.exe", // older store Remote Desktop
}

var (
	rdpEnumMu       sync.Mutex
	rdpEnumBest     windowHandle
	rdpEnumBestArea int64
	rdpEnumSelfPID  uint32
	rdpCallback     uintptr
	rdpCbOnce       sync.Once

	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procIsIconic                 = user32.NewProc("IsIconic")
	procGetClientRect            = user32.NewProc("GetClientRect")
	procClientToScreen           = user32.NewProc("ClientToScreen")
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procQueryFullProcessImageNam = kernel32.NewProc("QueryFullProcessImageNameW")
)

func processImageName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNam.Call(
		uintptr(h), 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return ""
	}
	full := windows.UTF16ToString(buf[:size])
	if i := strings.LastIndexAny(full, `\/`); i >= 0 {
		return strings.ToLower(full[i+1:])
	}
	return strings.ToLower(full)
}

func isRDPClientProcess(name string) bool {
	for _, candidate := range rdpClientProcesses {
		if name == candidate {
			return true
		}
	}
	return false
}

func enumRDPProc(hwnd uintptr, _ uintptr) uintptr {
	if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
		return 1
	}
	if ic, _, _ := procIsIconic.Call(hwnd); ic != 0 {
		return 1 // minimized — its coordinates are meaningless
	}

	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 || pid == rdpEnumSelfPID {
		return 1
	}
	if !isRDPClientProcess(processImageName(pid)) {
		return 1
	}

	var cr winRect
	if r, _, _ := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr))); r == 0 {
		return 1
	}

	// Pick the biggest one — the session view, not a toolbar or dialog.
	area := int64(cr.width()) * int64(cr.height())
	if area > rdpEnumBestArea {
		rdpEnumBestArea = area
		rdpEnumBest = windowHandle(hwnd)
	}
	return 1
}

// findRDPViewport returns the client area of the RDP session window in local
// physical screen coordinates. ok is false when no RDP client is running (or all
// of them are minimized), in which case callers should apply no translation and
// behave exactly as before.
func findRDPViewport() (winRect, bool) {
	rdpEnumMu.Lock()
	defer rdpEnumMu.Unlock()

	rdpCbOnce.Do(func() {
		rdpCallback = windows.NewCallback(enumRDPProc)
	})

	rdpEnumSelfPID = windows.GetCurrentProcessId()
	rdpEnumBest = 0
	rdpEnumBestArea = 0

	procEnumWindows.Call(rdpCallback, 0)

	if rdpEnumBest == 0 || rdpEnumBestArea == 0 {
		return winRect{}, false
	}

	var cr winRect
	if r, _, _ := procGetClientRect.Call(uintptr(rdpEnumBest), uintptr(unsafe.Pointer(&cr))); r == 0 {
		return winRect{}, false
	}

	// GetClientRect is client-relative (origin 0,0); map it onto the screen.
	pt := struct{ X, Y int32 }{0, 0}
	if r, _, _ := procClientToScreen.Call(uintptr(rdpEnumBest), uintptr(unsafe.Pointer(&pt))); r == 0 {
		return winRect{}, false
	}

	return winRect{
		Left:   pt.X,
		Top:    pt.Y,
		Right:  pt.X + cr.width(),
		Bottom: pt.Y + cr.height(),
	}, true
}

func sysMetric(index int) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int32(r) // truncate to the low 32 bits; may be negative
}

// virtualScreenRect is the bounding box of all monitors. Its origin is negative
// whenever a monitor sits left of / above the primary one.
func virtualScreenRect() winRect {
	x := sysMetric(smXVirtualScreen)
	y := sysMetric(smYVirtualScreen)
	return winRect{
		Left:   x,
		Top:    y,
		Right:  x + sysMetric(smCXVirtualScreen),
		Bottom: y + sysMetric(smCYVirtualScreen),
	}
}

func logVirtualScreen() {
	log.Printf("[winplace] virtual screen: %s", virtualScreenRect())
}
