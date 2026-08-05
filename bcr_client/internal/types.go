package engine

import "encoding/json"

type Config struct {
	ListenAddr      string
	PreferredCodecs PreferredCodecs
}

// PreferredCodecs controls which codecs the BCR pipeline accepts. The SDP
// codec filter in the browser extension strips all other codecs from the SDP
// so the SFU is constrained to send only these. The Go-side filter acts as a
// safety net to drop any unexpected PTs that slip through renegotiation.
type PreferredCodecs struct {
	Video string // e.g. "VP8", "H264", "VP9" — matched case-insensitively against MimeType
	Audio string // e.g. "opus" — matched case-insensitively against MimeType
}

type Callbacks struct {
	OnLoopbackOffer  func(bridgeID string, sdp string)
	OnVideoUpdate    func(update VideoUpdate)
	OnVideoLifecycle func(evtType string, videoID string)
	OnMediaChunk     func(header MediaChunkHeader, chunkData []byte)
	OnLog            func(message string)

	// OnYTDiag receives the extension's YouTube virtual-clock diagnostic lines so
	// they can be logged into bcr_client.log. If nil, they are dropped.
	OnYTDiag func(message string)

	// OnVerboseLog receives high-volume diagnostics (full SDP dumps, etc.).
	// It is intended to be wired to the log file only, so the terminal stays
	// readable. If nil, verbose messages are dropped entirely.
	OnVerboseLog func(message string)
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

// Clip describes how the <video> inside the overlay must be scaled and offset
// when the overlay window has been clipped to the browser viewport (i.e. the
// player is partially scrolled off screen). Values are ratios of the clipped
// box, so the client applies them as CSS percentages. The zero value is
// deliberately NOT the identity — use ClipOrIdentity().
type Clip struct {
	ScaleX  float64 `json:"scaleX"`
	ScaleY  float64 `json:"scaleY"`
	OffsetX float64 `json:"offsetX"`
	OffsetY float64 `json:"offsetY"`
}

type VideoUpdate struct {
	Type    string `json:"type"`
	Payload struct {
		ID           string `json:"id"`
		ScreenBounds Bounds `json:"screenBounds"`
		Clip         *Clip  `json:"clip"`
		// OnScreen is false when the video is not actually being displayed —
		// tab backgrounded, window minimized/occluded, or the player scrolled
		// out of the viewport. Pointer so that telemetry from an older
		// extension (field absent) reads as "unknown" and keeps the previous
		// always-visible behaviour rather than hiding the overlay forever.
		OnScreen *bool `json:"onScreen"`
		Playback struct {
			State string `json:"state"`
		} `json:"playback"`
	} `json:"payload"`
}

// ClipOrIdentity returns the clip transform, or the identity when absent.
func (v VideoUpdate) ClipOrIdentity() Clip {
	if v.Payload.Clip == nil {
		return Clip{ScaleX: 1, ScaleY: 1}
	}
	return *v.Payload.Clip
}

// IsOnScreen reports whether the overlay should be displayed. Absent field
// (older extension) is treated as on-screen.
func (v VideoUpdate) IsOnScreen() bool {
	return v.Payload.OnScreen == nil || *v.Payload.OnScreen
}

// VideoLifecycle carries VIDEO_ADDED / VIDEO_REMOVED events.
type VideoLifecycle struct {
	Type    string `json:"type"`
	Payload struct {
		ID string `json:"id"`
	} `json:"payload"`
}

// MediaChunkHeader matches the binary frame header produced by bcr_host.
type MediaChunkHeader struct {
	Seq            int64   `json:"seq"`
	Size           int     `json:"size"`
	TS             float64 `json:"ts"`
	TrackType      string  `json:"trackType"`
	MimeType       string  `json:"mimeType"`
	Codec          string  `json:"codec"`
	SourceBufferID string  `json:"sourceBufferId"`
	VideoID        string  `json:"videoId"`
	IsInitSegment  bool    `json:"isInitSegment"`
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
	PreWarmID  string      `json:"preWarmId,omitempty"`
	Timestamp  int64       `json:"timestamp"`
}

type RTCShadowCandidatePayload struct {
	BridgeID  string `json:"bridgeId"`
	Candidate string `json:"candidate"`
	SDPMid    string `json:"sdpMid"`
	// EndOfCandidates marks the final message of the trickle stream. Candidate
	// is empty when this is set; the JS turns it into the end-of-candidates
	// event rather than a candidate.
	EndOfCandidates bool  `json:"endOfCandidates,omitempty"`
	Timestamp       int64 `json:"timestamp"`
}

type RTCShadowClosePayload struct {
	BridgeID string `json:"bridgeId"`
}

type RTCShadowReadyPayload struct {
	BridgeID        string   `json:"bridgeId"`
	SDPType         string   `json:"sdpType"` // "offer" or "answer" — which SDP flow generated this READY
	ICEUfrag        string   `json:"iceUfrag"`
	ICEPwd          string   `json:"icePwd"`
	DTLSFingerprint string   `json:"dtlsFingerprint"`
	LocalIP         string   `json:"localIp"`
	Candidates      []string `json:"candidates,omitempty"` // a=candidate lines from shadow PC's local description
	// GatheringComplete reports whether ICE gathering had already finished when
	// this payload was built. When false, more candidates will arrive via
	// RTC_SHADOW_ICE_CANDIDATE and the JS must NOT signal end-of-candidates yet.
	GatheringComplete bool  `json:"gatheringComplete"`
	GeneratedAt       int64 `json:"generatedAt"`
	ExpiresAt         int64 `json:"expiresAt"`
}

type PreWarmReadyPayload struct {
	PreWarmID       string   `json:"preWarmId"`
	ICEUfrag        string   `json:"iceUfrag"`
	ICEPwd          string   `json:"icePwd"`
	DTLSFingerprint string   `json:"dtlsFingerprint"`
	Candidates      []string `json:"candidates"`
	GeneratedAt     int64    `json:"generatedAt"`
}

type RTCShadowErrorPayload struct {
	BridgeID  string `json:"bridgeId"`
	Stage     string `json:"stage"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
	Timestamp int64  `json:"timestamp"`
}

type RTCShadowIceServersPayload struct {
	BridgeID   string      `json:"bridgeId"`
	IceServers []IceServer `json:"iceServers"`
	Timestamp  int64       `json:"timestamp"`
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
