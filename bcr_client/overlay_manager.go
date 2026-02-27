package main

import "log"

// visibilityThreshold is the minimum IntersectionRatio below which
// the overlay is hidden. 0.0 means hide only when fully invisible.
// Raise to e.g. 0.1 to hide when less than 10% is visible.
const visibilityThreshold = 0.0

// GeometryUpdate is sent over the channel to the render loop
// whenever the overlay window needs to move, resize, or change visibility.
// Coordinates are screen-absolute pixels as computed by the extension.
type GeometryUpdate struct {
	X, Y, W, H int
	Visible    bool
}

// OverlayManager tracks the single active video and forwards
// geometry updates to the render loop via a channel.
// Safe to call from any goroutine.
type OverlayManager struct {
	currentVideoID string
	geomCh         chan GeometryUpdate
}

// NewOverlayManager constructs an OverlayManager.
// geomCh must be the same channel the render loop reads from.
func NewOverlayManager(geomCh chan GeometryUpdate) *OverlayManager {
	return &OverlayManager{geomCh: geomCh}
}

// Create handles VIDEO_ADDED.
// Reuses the existing window — updates currentVideoID and shows at default size.
func (m *OverlayManager) Create(id string) {
	if m.currentVideoID != "" && m.currentVideoID != id {
		log.Printf("[overlay] replacing video %s with %s", m.currentVideoID, id)
	}

	m.currentVideoID = id
	log.Printf("[overlay] video added — id=%s", id)

	// Show at default position — first VIDEO_UPDATE corrects geometry.
	m.send(GeometryUpdate{
		X: 100, Y: 100,
		W: 640, H: 360,
		Visible: true,
	})
}

// Update handles VIDEO_UPDATE.
// Ignored if not for the currently tracked video.
// Hides the overlay when IntersectionRatio is at or below visibilityThreshold.
func (m *OverlayManager) Update(id string, bounds Bounds, visibility Visibility) {
	if id != m.currentVideoID {
		return
	}

	// Hide when video is scrolled out of view.
	visible := visibility.IntersectionRatio > visibilityThreshold

	m.send(GeometryUpdate{
		X:       int(bounds.X),
		Y:       int(bounds.Y),
		W:       int(bounds.Width),
		H:       int(bounds.Height),
		Visible: visible,
	})
}

// Destroy handles VIDEO_REMOVED.
// Ignored if not for the currently tracked video.
func (m *OverlayManager) Destroy(id string) {
	if id != m.currentVideoID {
		log.Printf("[overlay] REMOVED for untracked video %s — ignored", id)
		return
	}

	log.Printf("[overlay] video removed — id=%s", id)
	m.currentVideoID = ""

	m.send(GeometryUpdate{Visible: false})
}

// send performs a non-blocking send to the geometry channel.
// Drops if the render loop is busy — next UPDATE will correct geometry.
func (m *OverlayManager) send(u GeometryUpdate) {
	select {
	case m.geomCh <- u:
	default:
		log.Printf("[overlay] geometry channel full — update dropped")
	}
}
