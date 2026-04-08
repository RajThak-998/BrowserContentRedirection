package main

import "encoding/json"

const (
	PacketTypeRTCShadowRemote    = "RTC_SHADOW_REMOTE"
	PacketTypeRTCShadowLocal     = "RTC_SHADOW_LOCAL"
	PacketTypeRTCShadowClose     = "RTC_SHADOW_CLOSE"
	PacketTypeRTCShadowReady     = "RTC_SHADOW_READY"
	PacketTypeRTCShadowError     = "RTC_SHADOW_ERROR"
	PacketTypeRTCShadowCandidate = "RTC_SHADOW_ICE_CANDIDATE"
)

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
	IsInitSegment  bool    `json:"isInitSegment"`
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
	IsInitSegment  bool    `json:"isInitSegment"`
	HostReceivedMS int64   `json:"hostReceivedMs"`
}

type RTCShadowRemotePayload struct {
	BridgeID  string `json:"bridgeId"`
	SDPType   string `json:"sdpType"`
	SDP       string `json:"sdp"`
	Timestamp int64  `json:"timestamp"`
}

type RTCShadowLocalPayload struct {
	BridgeID  string `json:"bridgeId"`
	SDPType   string `json:"sdpType"`
	SDP       string `json:"sdp"`
	Timestamp int64  `json:"timestamp"`
}

type RTCShadowClosePayload struct {
	BridgeID  string `json:"bridgeId"`
	Reason    string `json:"reason"`
	Timestamp int64  `json:"timestamp"`
}

type RTCShadowReadyPayload struct {
	BridgeID        string `json:"bridgeId"`
	ICEUfrag        string `json:"iceUfrag"`
	ICEPwd          string `json:"icePwd"`
	DTLSFingerprint string `json:"dtlsFingerprint"`
	LocalIP         string `json:"localIp"`
	GeneratedAt     int64  `json:"generatedAt"`
	ExpiresAt       int64  `json:"expiresAt"`
}

type RTCShadowErrorPayload struct {
	BridgeID  string `json:"bridgeId"`
	Stage     string `json:"stage"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
	Timestamp int64  `json:"timestamp"`
}
