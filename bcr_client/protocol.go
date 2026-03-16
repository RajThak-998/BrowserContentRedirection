package main

import "encoding/json"

// Packet is the top-level message structure received from bcr_host.
// Payload and Meta are deferred as raw JSON so the handler can
// decode them into the appropriate concrete type per packet.Type.
type Packet struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Meta    json.RawMessage `json:"meta,omitempty"`
}

// Meta is attached by background.js before forwarding to bcr_host.
type Meta struct {
	TabID   int    `json:"tabId"`
	TabURL  string `json:"tabUrl"`
	FrameID int    `json:"frameId"`
}

// AddedPayload is the payload for VIDEO_ADDED events.
type AddedPayload struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
}

// RemovedPayload is the payload for VIDEO_REMOVED events.
type RemovedPayload struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
}

// UpdatePayload is the payload for VIDEO_UPDATE events.
type UpdatePayload struct {
	ID           string     `json:"id"`
	Timestamp    int64      `json:"timestamp"`
	Bounds       Bounds     `json:"bounds"`       // viewport-relative (for browser overlay)
	ScreenBounds Bounds     `json:"screenBounds"` // screen-absolute   (for GLFW window)
	Visibility   Visibility `json:"visibility"`
	Playback     Playback   `json:"playback"`
	Fullscreen   bool       `json:"fullscreen"`
	Delta        Delta      `json:"delta"`
}

// Bounds represents the screen-space rectangle of the video element.
type Bounds struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Visibility describes how much of the video is in the viewport.
type Visibility struct {
	InViewport        bool    `json:"inViewport"`
	IntersectionRatio float64 `json:"intersectionRatio"`
}

// Playback describes current playback state of the video element.
type Playback struct {
	State       string  `json:"state"`
	CurrentTime float64 `json:"currentTime"`
	Rate        float64 `json:"rate"`
}

// Delta describes the change in bounds since the last UPDATE event.
type Delta struct {
	DX float64 `json:"dx"`
	DY float64 `json:"dy"`
	DW float64 `json:"dw"`
	DH float64 `json:"dh"`
}

// MediaChunkHeader is the header for each media chunk.
type MediaChunkHeader struct {
	Seq            int64   `json:"seq"`
	Size           int     `json:"size"`
	TS             float64 `json:"ts"`
	TrackType      string  `json:"trackType"`
	MimeType       string  `json:"mimeType"`
	Codec          string  `json:"codec"`
	SourceBufferID string  `json:"sourceBufferId"`
	IsInitSegment  bool    `json:"isInitSegment"`
}

// MediaChunkLogPayload is the lightweight media summary forwarded by bcr_host.
// It does NOT contain raw chunk bytes.
type MediaChunkLogPayload struct {
	Seq            int64   `json:"seq"`
	Size           int     `json:"size"`
	TS             float64 `json:"ts"`
	TrackType      string  `json:"trackType"`
	MimeType       string  `json:"mimeType"`
	Codec          string  `json:"codec"`
	SourceBufferID string  `json:"sourceBufferId"`
	HostReceivedMS int64   `json:"hostReceivedMs"`
	IsInitSegment  bool    `json:"isInitSegment"`
}

// MediaChunkFrameHeader is the JSON header embedded in each binary media frame.
type MediaChunkFrameHeader struct {
	Type    string           `json:"type"`
	Payload MediaChunkHeader `json:"payload"`
	Meta    json.RawMessage  `json:"meta,omitempty"`
}
