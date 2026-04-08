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
	BridgeID string `json:"bridgeId"`
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
