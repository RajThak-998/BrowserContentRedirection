package main

import (
	"context"
	"encoding/base64"
	"log"
	"math"
	"sync"
	"time"

	"bcr_client/internal"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App struct
type App struct {
	ctx context.Context

	// placer owns the overlay window's geometry. Telemetry is fed to it and it
	// applies the latest rect on its own cadence — see placer.go.
	placer *placer

	// Overlay visibility state, all guarded by mseActiveMu.
	//
	// The overlay is shown only when BOTH conditions hold:
	//   mseActive — the frontend is actually rendering frames (canplay fired and
	//               the pipeline has not been torn down)
	//   onScreen  — the remote video is really being displayed: the tab is
	//               visible, the window isn't minimized/occluded, and the player
	//               hasn't been scrolled out of the viewport
	// Tracking them separately matters because they change for different reasons
	// and need different debouncing — see NotifyMSEActive and setOnScreen.
	mseActiveMu  sync.Mutex
	mseActive    bool
	onScreen     bool        // starts false; first VIDEO_UPDATE sets the real value
	overlayShown bool        // what we last told the window to do
	hideTimer    *time.Timer // debounces the MSE hide so teardown churn doesn't flash the window

	// lastClip dedupes the video-within-overlay transform sent to the frontend.
	clipMu   sync.Mutex
	lastClip engine.Clip

	mediaEngine *engine.Engine
}

// overlayHideGrace defers hiding the overlay after MSE goes inactive, so brief
// teardown→rebuild churn (ad boundaries, hover previews, videoID swaps) doesn't
// flash the window. A show within this window cancels the pending hide.
const overlayHideGrace = 2 * time.Second

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// The window exists by now (Wails creates it before invoking OnStartup), but
	// the placer resolves its HWND lazily and retries, so ordering is not critical.
	a.placer = newPlacer()
	go a.placer.run(ctx)

	a.mediaEngine = engine.New(
		engine.Config{ListenAddr: ":8081"},
		engine.Callbacks{
			OnLoopbackOffer: func(bridgeID string, sdp string) {
				log.Printf("[bcr_client][loopback] emitting onLocalLoopbackOffer to frontend bridgeId=%s sdpLen=%d", bridgeID, len(sdp))
				runtime.EventsEmit(a.ctx, "onLocalLoopbackOffer", bridgeID, sdp)
			},
			OnVideoUpdate: func(update engine.VideoUpdate) {
				b := update.Payload.ScreenBounds
				a.applyWindowFromTelemetry(b.X, b.Y, b.Width, b.Height)
				a.applyClip(update.ClipOrIdentity())
				// Overlay visibility is driven by MSE liveness (NotifyMSEActive,
				// from the frontend) combined with whether the remote video is
				// actually on screen. It is deliberately NOT driven by VDI
				// playback state, which flaps during quality switches and makes
				// the window oscillate.
				a.setOnScreen(update.IsOnScreen())
			},
			OnVideoLifecycle: func(evtType string, videoID string) {
				log.Printf("[bcr_client][video] lifecycle evtType=%s videoId=%s", evtType, videoID)
				runtime.EventsEmit(a.ctx, "onVideoLifecycle", evtType, videoID)

				// VIDEO_SOURCE_GONE means the tab is closed or navigated away —
				// definitive, unlike VIDEO_REMOVED. Hide now rather than waiting
				// out overlayHideGrace, and don't let a pending grace timer
				// resurrect the state afterwards.
				if evtType == "VIDEO_SOURCE_GONE" {
					a.forceOverlayHidden("media source gone")
				}
				// NOTE: window visibility is intentionally NOT hidden here.
				// A VIDEO_REMOVED fires for hover-preview thumbnails and for the
				// brief element swap YouTube does at ad boundaries — hiding on it
				// made the overlay flicker/vanish while the frontend was still
				// playing. Visibility is now owned exclusively by the frontend via
				// NotifyMSEActive(false), which it calls only after its debounced
				// teardown actually runs (see scheduleTeardown in main.js).
			},
			OnMediaChunk: func(header engine.MediaChunkHeader, chunkData []byte) {
				// Encode chunk bytes as base64 for JSON-safe Wails event transport.
				chunkB64 := base64.StdEncoding.EncodeToString(chunkData)
				runtime.EventsEmit(a.ctx, "onMediaChunk",
					header.Seq,
					header.TrackType,
					header.MimeType,
					header.Codec,
					header.SourceBufferID,
					header.IsInitSegment,
					chunkB64,
					header.VideoID,
				)
			},
			OnLog: func(message string) {
				log.Println(message)
			},
			OnYTDiag: func(message string) {
				log.Printf("[YT] %s", message)
			},
			// Verbose diagnostics (full SDP dumps) go to the log file only.
			OnVerboseLog: verboseLog,
		},
	)

	// Start core engine (WebSocket signaling + media bridge) in background.
	go func() {
		if err := a.mediaEngine.Run(ctx); err != nil {
			log.Printf("Engine stopped with error: %v", err)
		}
	}()
}

// SetLoopbackAnswer takes the SDP answer from Javascript and passes it to the loopback session
func (a *App) SetLoopbackAnswer(bridgeID string, sdp string) {
	if a.mediaEngine != nil {
		a.mediaEngine.SetLoopbackAnswer(bridgeID, sdp)
	}
}

// RequestLoopbackOffer is called by the Wails frontend immediately after it
// registers its EventsOn("onLocalLoopbackOffer") listener. This recovers from
// the cold-start race where the Go backend fired the offer before the JS
// listener was alive — the engine re-emits any cached offer SDPs for active
// loopback sessions.
func (a *App) RequestLoopbackOffer() {
	log.Println("[bcr_client] RequestLoopbackOffer called by frontend — re-emitting any cached loopback offers")
	if a.mediaEngine != nil {
		a.mediaEngine.ReEmitAllLoopbackOffers()
	}
}

// ReportPlayback relays the frontend MSE buffer state (bufferedEnd + playhead, in
// seconds) back to the extension interceptor so it can drive the YouTube seek pulse.
func (a *App) ReportPlayback(bufferedEnd float64, playhead float64) {
	if a.mediaEngine != nil {
		a.mediaEngine.SendYTPlayback(bufferedEnd, playhead)
	}
}

// -------------------------------------------------------------
// UI Window Controls (Exposed to Javascript or called from Go)
// -------------------------------------------------------------

// ShowWindow makes the application visible
func (a *App) ShowWindow() {
	runtime.WindowShow(a.ctx)
}

// HideWindow hides the application
func (a *App) HideWindow() {
	runtime.WindowHide(a.ctx)
}

// applyWindowFromTelemetry hands the latest video rect to the placer.
//
// The incoming values are absolute physical (device) pixels in the desktop's
// virtual-screen coordinate space, as measured by the extension. Left/Top may be
// negative when the video sits on a monitor left of / above the primary one.
//
// This used to call runtime.WindowSetPosition + runtime.WindowSetSize directly,
// behind a 33ms leading-edge throttle. Both parts were wrong: Wails' SetPos
// treats coordinates as relative to the current monitor's work area (so the
// overlay could never stay on a second monitor), and the throttle discarded the
// final rect of every burst. See winplace_windows.go and placer.go.
func (a *App) applyWindowFromTelemetry(x, y, w, h float64) {
	iw := int32(math.Round(w))
	ih := int32(math.Round(h))
	if iw < 1 || ih < 1 {
		return
	}

	ix := int32(math.Round(x))
	iy := int32(math.Round(y))

	a.placer.Update(winRect{
		Left:   ix,
		Top:    iy,
		Right:  ix + iw,
		Bottom: iy + ih,
	})
}

// applyClip forwards the video-within-overlay transform to the frontend.
//
// When the player is partially scrolled out of the browser viewport the overlay
// window is clipped to the visible part, so the <video> inside it has to be
// scaled up and shifted by the clipped-away amount to keep the picture lined up
// with the real player. Identity (1,1,0,0) for the normal unclipped case.
//
// Deduplicated: telemetry arrives ~30/s but the clip only changes while the user
// is actually scrolling the player off screen.
func (a *App) applyClip(c engine.Clip) {
	a.clipMu.Lock()
	if a.lastClip == c {
		a.clipMu.Unlock()
		return
	}
	a.lastClip = c
	a.clipMu.Unlock()

	runtime.EventsEmit(a.ctx, "onVideoClip", c.ScaleX, c.ScaleY, c.OffsetX, c.OffsetY)
}

// NotifyMSEActive is called by the frontend when the MSE pipeline becomes live
// (canplay event fired) or is torn down. It drives the overlay window visibility
// so the window only shows when the thin client is actually rendering video.
//
// Shows are immediate; hides are debounced by overlayHideGrace so the brief
// teardown→rebuild churn YouTube produces (ad boundaries, hover previews, videoID
// swaps) doesn't flash the overlay in and out. A show during the grace cancels the
// pending hide.
func (a *App) NotifyMSEActive(active bool) {
	a.mseActiveMu.Lock()
	if active {
		// A rebuild within the grace window cancels a pending hide.
		if a.hideTimer != nil {
			a.hideTimer.Stop()
			a.hideTimer = nil
		}
		a.mseActive = true
		a.mseActiveMu.Unlock()
		a.syncOverlayVisibility("mse active")
		return
	}

	if a.mseActive && a.hideTimer == nil {
		// Defer the hide; MSE teardown/rebuild churn is common and transient.
		a.hideTimer = time.AfterFunc(overlayHideGrace, a.hideOverlayAfterGrace)
	}
	a.mseActiveMu.Unlock()
}

// hideOverlayAfterGrace fires when the overlay has been inactive for overlayHideGrace
// with no rebuild — the video really stopped, so hide the window.
func (a *App) hideOverlayAfterGrace() {
	a.mseActiveMu.Lock()
	a.hideTimer = nil
	a.mseActive = false
	a.mseActiveMu.Unlock()

	a.syncOverlayVisibility("mse inactive (grace elapsed)")
}

// NotifyMediaStarved is called by the frontend when the MSE pipeline has run out
// of buffer with no new chunks arriving — the source stopped feeding without a
// clean signal (VDI browser crash, extension unloaded, bridge dropped). Hides the
// overlay immediately instead of waiting out overlayHideGrace, since the frontend
// already spent MEDIA_STARVATION_MS confirming it.
func (a *App) NotifyMediaStarved() {
	a.forceOverlayHidden("media starved — source stopped feeding")
}

// forceOverlayHidden hides the overlay right away and cancels any pending
// grace-period hide, for events that are definitive rather than transient (the
// media tab closing, navigating away, or the media stream drying up).
func (a *App) forceOverlayHidden(reason string) {
	a.mseActiveMu.Lock()
	if a.hideTimer != nil {
		a.hideTimer.Stop()
		a.hideTimer = nil
	}
	a.mseActive = false
	a.mseActiveMu.Unlock()

	a.syncOverlayVisibility(reason)
}

// setOnScreen records whether the remote video is actually being displayed.
//
// Unlike the MSE signal this is NOT debounced: it reflects a deliberate user
// action (minimizing the window, switching tab, scrolling the player out of
// view), so there is no churn to ride out and any delay just leaves a stale
// overlay floating over whatever the user switched to.
func (a *App) setOnScreen(onScreen bool) {
	a.mseActiveMu.Lock()
	if a.onScreen == onScreen {
		a.mseActiveMu.Unlock()
		return
	}
	a.onScreen = onScreen
	a.mseActiveMu.Unlock()

	if onScreen {
		a.syncOverlayVisibility("video back on screen")
	} else {
		a.syncOverlayVisibility("video off screen")
	}
}

// syncOverlayVisibility drives the window to match the desired state:
// visible only when MSE is producing frames AND the remote video is on screen.
func (a *App) syncOverlayVisibility(reason string) {
	a.mseActiveMu.Lock()
	want := a.mseActive && a.onScreen
	if want == a.overlayShown {
		a.mseActiveMu.Unlock()
		return
	}
	a.overlayShown = want
	a.mseActiveMu.Unlock()

	if want {
		log.Printf("[bcr_client][video] showing overlay window (%s)", reason)
		// Place the window before revealing it, so it never flashes at the
		// position it happened to be left at when it was last hidden.
		if r, ok := a.placer.Pending(); ok {
			if hwnd, found := a.placer.resolveHWND(); found {
				_ = applyRectPhysical(hwnd, r)
			}
		}
		a.placer.SetVisible(true)
		runtime.WindowShow(a.ctx)
		return
	}

	log.Printf("[bcr_client][video] hiding overlay window (%s)", reason)
	a.placer.SetVisible(false)
	runtime.WindowHide(a.ctx)
}
