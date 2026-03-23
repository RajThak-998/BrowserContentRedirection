package main

import (
	"encoding/binary"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ReadLoop blocks on reading messages from a single connection.
// It routes messages based on the connection's role:
//   - extension text:   broadcast unchanged (existing VIDEO_* telemetry)
//   - extension binary: parse MEDIA_CHUNK frame, log summary, broadcast MEDIA_CHUNK_LOG text
//   - client:           log and ignore (future: handle control messages)
//
// When the connection closes or errors, it cleans up from the registry
// and returns — letting the goroutine started in server.go exit cleanly.
func ReadLoop(conn *Connection, registry *Registry) {
	defer func() {
		registry.Remove(conn)
		conn.WS.Close()
		log.Printf("[router] read loop exited (id=%s, role=%s)", conn.ID, conn.Role)
	}()

	for {
		msgType, data, err := conn.WS.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
				websocket.CloseNoStatusReceived,
			) {
				log.Printf("[router] unexpected close error (id=%s, role=%s): %v", conn.ID, conn.Role, err)
			}
			return
		}

		switch conn.Role {
		case "extension":
			handleExtensionMessage(msgType, data, registry)

		case "client":
			log.Printf("[router] received %d bytes from client (id=%s) — ignored", len(data), conn.ID)

		default:
			log.Printf("[router] received message from unknown role %q (id=%s) — dropped", conn.Role, conn.ID)
		}
	}
}

func handleExtensionMessage(msgType int, data []byte, registry *Registry) {
	switch msgType {
	case websocket.TextMessage:
		// Existing telemetry path unchanged.
		registry.Broadcast(msgType, data)

	case websocket.BinaryMessage:
		handleMediaBinaryFrame(data, registry)

	default:
		log.Printf("[router] unsupported extension msgType=%d (%d bytes) — dropped", msgType, len(data))
	}
}

func handleMediaBinaryFrame(data []byte, registry *Registry) {
	// Expected frame: [4-byte LE headerLen][headerJSON][rawChunk]
	if len(data) < 4 {
		log.Printf("[router] MEDIA_CHUNK frame too small (%d bytes) — dropped", len(data))
		return
	}

	headerLen := int(binary.LittleEndian.Uint32(data[:4]))
	if headerLen <= 0 || 4+headerLen > len(data) {
		log.Printf("[router] MEDIA_CHUNK invalid header length=%d frame=%d — dropped", headerLen, len(data))
		return
	}

	headerBytes := data[4 : 4+headerLen]
	chunkBytes := data[4+headerLen:]

	var hdr MediaChunkFrameHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		log.Printf("[router] MEDIA_CHUNK header decode failed: %v", err)
		return
	}

	if hdr.Type != "MEDIA_CHUNK" {
		log.Printf("[router] binary frame with unexpected header type=%q — dropped", hdr.Type)
		return
	}

	if hdr.Payload.Size != len(chunkBytes) {
		log.Printf("[router] MEDIA_CHUNK size mismatch header=%d actual=%d", hdr.Payload.Size, len(chunkBytes))
	}

	log.Printf("[router] MEDIA_CHUNK seq=%d size=%d track=%s init=%v",
		hdr.Payload.Seq,
		len(chunkBytes),
		hdr.Payload.TrackType,
		hdr.Payload.IsInitSegment,
	)

	if hdr.Payload.IsInitSegment || hdr.Payload.Seq%50 == 0 {
		log.Printf("[Host] track=%s codec=%s size=%d init=%v",
			hdr.Payload.TrackType,
			hdr.Payload.Codec,
			len(chunkBytes),
			hdr.Payload.IsInitSegment,
		)
	}

	registry.Broadcast(websocket.BinaryMessage, data)

	// Forward lightweight summary to clients (no raw chunk yet).
	logPayload := MediaChunkLogPayload{
		Seq:            hdr.Payload.Seq,
		Size:           len(chunkBytes),
		TS:             hdr.Payload.TS,
		TrackType:      hdr.Payload.TrackType,
		MimeType:       hdr.Payload.MimeType,
		Codec:          hdr.Payload.Codec,
		SourceBufferID: hdr.Payload.SourceBufferID,
		HostReceivedMS: time.Now().UnixMilli(),
		IsInitSegment:  hdr.Payload.IsInitSegment,
	}

	payloadRaw, err := json.Marshal(logPayload)
	if err != nil {
		log.Printf("[router] failed to marshal MEDIA_CHUNK_LOG payload: %v", err)
		return
	}

	out := Packet{
		Type:    "MEDIA_CHUNK_LOG",
		Payload: payloadRaw,
		Meta:    hdr.Meta,
	}

	outBytes, err := json.Marshal(out)
	if err != nil {
		log.Printf("[router] failed to marshal MEDIA_CHUNK_LOG packet: %v", err)
		return
	}

	mediaBridge.tryForward(
		hdr.Payload.TrackType,
		hdr.Payload.MimeType,
		hdr.Payload.Seq,
		hdr.Payload.IsInitSegment,
		data, // forward framed packet for webrtc-player metadata parsing
	)

	registry.Broadcast(websocket.TextMessage, outBytes)
}

const (
	bridgeURL            = "ws://localhost:8081"
	bridgeReconnectDelay = 2 * time.Second
	bridgeQueueSize      = 512

	// Safe mode toggle:
	// "all" (default), "video-only", "audio-only"
	bridgeFilterMode = "all"
)

type bridgeChunk struct {
	track string
	mime  string
	seq   int64
	init  bool
	data  []byte
}

type bridgeForwarder struct {
	startOnce sync.Once
	sendCh    chan bridgeChunk
}

var mediaBridge = newBridgeForwarder()

func newBridgeForwarder() *bridgeForwarder {
	return &bridgeForwarder{
		sendCh: make(chan bridgeChunk, bridgeQueueSize),
	}
}

func (b *bridgeForwarder) start() {
	b.startOnce.Do(func() {
		go b.run()
	})
}

func (b *bridgeForwarder) run() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial(bridgeURL, nil)
		if err != nil {
			log.Printf("[Bridge] Reconnecting... dial failed: %v", err)
			time.Sleep(bridgeReconnectDelay)
			continue
		}

		log.Printf("[Bridge] Connected to webrtc-player")

		err = b.writeLoop(conn)
		if err != nil {
			log.Printf("[Bridge] Disconnected: %v", err)
		} else {
			log.Printf("[Bridge] Disconnected")
		}

		_ = conn.Close()
		log.Printf("[Bridge] Reconnecting...")
		time.Sleep(bridgeReconnectDelay)
	}
}

func (b *bridgeForwarder) writeLoop(conn *websocket.Conn) error {
	for msg := range b.sendCh {
		if err := conn.WriteMessage(websocket.BinaryMessage, msg.data); err != nil {
			log.Printf("[Bridge] ERROR send failed: %v", err)
			return err
		}

		if msg.init {
			log.Printf("[Bridge] INIT track=%s mime=%s size=%d", msg.track, msg.mime, len(msg.data))
		}

		if msg.init || msg.seq%50 == 0 {
			log.Printf("[Bridge] FORWARD track=%s mime=%s size=%d seq=%d",
				msg.track, msg.mime, len(msg.data), msg.seq)
		}
	}

	return nil
}

func bridgeShouldForward(track string, isInit bool) (bool, string) {
	// Always forward init segments.
	if isInit {
		return true, ""
	}

	// Optional safety gate by track.
	switch bridgeFilterMode {
	case "all":
		return true, ""
	case "video-only":
		if track == "video" {
			return true, ""
		}
		return false, "filter_mode_video_only"
	case "audio-only":
		if track == "audio" {
			return true, ""
		}
		return false, "filter_mode_audio_only"
	default:
		return false, "invalid_filter_mode"
	}
}

func (b *bridgeForwarder) tryForward(track, mime string, seq int64, isInit bool, data []byte) {
	b.start()

	ok, reason := bridgeShouldForward(track, isInit)
	if !ok {
		log.Printf("[Bridge] SKIP track=%s reason=%s", track, reason)
		return
	}

	payload := append([]byte(nil), data...) // decouple from caller memory

	select {
	case b.sendCh <- bridgeChunk{
		track: track,
		mime:  mime,
		seq:   seq,
		init:  isInit,
		data:  payload,
	}:
	default:
		log.Printf("[Bridge] SKIP track=%s reason=bridge_queue_full", track)
	}
}
