package main

import "encoding/json"

type Packet struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Meta    json.RawMessage `json:"meta,omitempty"`
}

// Header JSON embedded in binary frame:
// [u32 headerLen LE][headerJSON][rawChunk]
type MediaChunkFrameHeader struct {
	Type    string           `json:"type"`
	Payload MediaChunkHeader `json:"payload"`
	Meta    json.RawMessage  `json:"meta,omitempty"`
}

type MediaChunkHeader struct {
	Seq            int64   `json:"seq"`
	Size           int     `json:"size"`
	TS             float64 `json:"ts"`
	TrackType      string  `json:"trackType"`
	MimeType       string  `json:"mimeType"`
	Codec          string  `json:"codec"`
	SourceBufferID string  `json:"sourceBufferId"`
}

// What host forwards to clients instead of raw bytes.
type MediaChunkLogPayload struct {
	Seq            int64   `json:"seq"`
	Size           int     `json:"size"`
	TS             float64 `json:"ts"`
	TrackType      string  `json:"trackType"`
	MimeType       string  `json:"mimeType"`
	Codec          string  `json:"codec"`
	SourceBufferID string  `json:"sourceBufferId"`
	HostReceivedMS int64   `json:"hostReceivedMs"`
}
