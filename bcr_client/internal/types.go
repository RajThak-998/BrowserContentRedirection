package engine

import "encoding/json"

type Config struct {
	ListenAddr string
}

type Callbacks struct {
	OnVideoChunk  func(data []byte)
	OnVideoUpdate func(update VideoUpdate)
	OnLog         func(message string)
}

type Packet struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Meta    json.RawMessage `json:"meta,omitempty"`
}

type Bounds struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type VideoUpdate struct {
	Type    string `json:"type"`
	Payload struct {
		ScreenBounds Bounds `json:"screenBounds"`
	} `json:"payload"`
}

type RTCShadowRemotePayload struct {
	BridgeID   string      `json:"bridgeId"`
	SDPType    string      `json:"sdpType"`
	SDP        string      `json:"sdp"`
	IceServers []IceServer `json:"iceServers,omitempty"`
	Timestamp  int64       `json:"timestamp"`
}

type IceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

type RTCShadowLocalPayload struct {
	BridgeID   string      `json:"bridgeId"`
	SDPType    string      `json:"sdpType"`
	SDP        string      `json:"sdp"`
	IceServers []IceServer `json:"iceServers"`
	Timestamp  int64       `json:"timestamp"`
}

type RTCShadowCandidatePayload struct {
	BridgeID  string `json:"bridgeId"`
	Candidate string `json:"candidate"`
	SDPMid    string `json:"sdpMid"`
	Timestamp int64  `json:"timestamp"`
}

type RTCShadowClosePayload struct {
	BridgeID string `json:"bridgeId"`
}

type RTCShadowReadyPayload struct {
	BridgeID        string   `json:"bridgeId"`
	SDPType         string   `json:"sdpType"`                // "offer" or "answer" — which SDP flow generated this READY
	ICEUfrag        string   `json:"iceUfrag"`
	ICEPwd          string   `json:"icePwd"`
	DTLSFingerprint string   `json:"dtlsFingerprint"`
	LocalIP         string   `json:"localIp"`
	Candidates      []string `json:"candidates,omitempty"` // a=candidate lines from shadow PC's local description
	GeneratedAt     int64    `json:"generatedAt"`
	ExpiresAt       int64    `json:"expiresAt"`
}

type RTCShadowErrorPayload struct {
	BridgeID  string `json:"bridgeId"`
	Stage     string `json:"stage"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
	Timestamp int64  `json:"timestamp"`
}

// CodecInfo is a minimal PT → codec descriptor used by the raw transport layer.
// It avoids importing pion/webrtc in sdp.go while carrying the information the
// relay needs to create TrackLocalStaticRTP with the correct MIME and clock rate.
type CodecInfo struct {
	MimeType    string // e.g. "audio/opus", "video/VP8", "video/H264"
	ClockRate   uint32 // e.g. 90000 for video, 48000 for Opus
	Channels    uint16 // e.g. 2 for stereo Opus, 0 for video
	PayloadType uint8  // the dynamic PT negotiated in the browser SDP
}

// MediaSection represents a single m= line and its associated formats.
type MediaSection struct {
	Type     string
	Port     string
	Protocol string
	Formats  []string
}

