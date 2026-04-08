package main

import (
	"encoding/binary"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var _bridgeRegistry *Registry

// ReadLoop blocks on reading messages from a single connection.
// It routes messages based on the connection's role:
//   - extension text:   broadcast unchanged (existing VIDEO_* telemetry)
//   - extension binary: parse MEDIA_CHUNK frame, log summary, broadcast MEDIA_CHUNK_LOG text
//   - client:           log and ignore (future: handle control messages)
//
// When the connection closes or errors, it cleans up from the registry
// and returns — letting the goroutine started in server.go exit cleanly.
func ReadLoop(conn *Connection, registry *Registry) {
	if _bridgeRegistry == nil {
		_bridgeRegistry = registry
	}

	defer func() {
		registry.Remove(conn)
		conn.WS.Close()
		log.Printf("[router] read loop exited (id=%s, role=%s)", conn.ID, conn.Role)
	}()

	if conn.Role == "extension" {
		handleExtensionIngressLoop(conn, registry)
		return
	}

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
		case "client":
			handleClientMessage(msgType, data, registry, conn.ID)

		default:
			log.Printf("[router] received message from unknown role %q (id=%s) — dropped", conn.Role, conn.ID)
		}
	}
}

type extensionIngressMessage struct {
	msgType int
	data    []byte
}

func handleExtensionIngressLoop(conn *Connection, registry *Registry) {
	const (
		highPriorityQueueSize = 256
		lowPriorityQueueSize  = 1024
	)

	highPriorityCh := make(chan extensionIngressMessage, highPriorityQueueSize)
	lowPriorityCh := make(chan extensionIngressMessage, lowPriorityQueueSize)

	var workers sync.WaitGroup
	workers.Add(2)

	go func() {
		defer workers.Done()
		for msg := range highPriorityCh {
			handleExtensionMessage(msg.msgType, msg.data, registry)
		}
	}()

	go func() {
		defer workers.Done()
		for msg := range lowPriorityCh {
			handleExtensionMessage(msg.msgType, msg.data, registry)
		}
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
			break
		}

		msg := extensionIngressMessage{msgType: msgType, data: data}

		switch msgType {
		case websocket.TextMessage:
			if isRTCShadowUpstreamPacket(data) {
				select {
				case highPriorityCh <- msg:
				default:
					select {
					case highPriorityCh <- msg:
					case <-time.After(250 * time.Millisecond):
						packetType, bridgeID := packetTypeAndBridgeID(data)
						log.Printf("[router] SHADOW_UP dropped reason=high_priority_backpressure type=%s bridgeId=%s", packetType, bridgeID)
					}
				}
				continue
			}

			select {
			case lowPriorityCh <- msg:
			default:
				log.Printf("[router] extension telemetry dropped reason=low_priority_queue_full bytes=%d", len(data))
			}

		case websocket.BinaryMessage:
			select {
			case lowPriorityCh <- msg:
			default:
				log.Printf("[router] MEDIA_CHUNK dropped reason=low_priority_queue_full bytes=%d", len(data))
			}

		default:
			log.Printf("[router] unsupported extension msgType=%d (%d bytes) — dropped", msgType, len(data))
		}
	}

	close(highPriorityCh)
	close(lowPriorityCh)
	workers.Wait()
}

func handleExtensionMessage(msgType int, data []byte, registry *Registry) {
	switch msgType {
	case websocket.TextMessage:
		// Existing telemetry path unchanged.
		registry.Broadcast(msgType, data)

		if isVideoUpdatePacket(data) || isRTCShadowUpstreamPacket(data) {
			mediaBridge.tryForwardText(data)
		}

	case websocket.BinaryMessage:
		handleMediaBinaryFrame(data, registry)

	default:
		log.Printf("[router] unsupported extension msgType=%d (%d bytes) — dropped", msgType, len(data))
	}
}

func handleClientMessage(msgType int, data []byte, registry *Registry, connID string) {
	if msgType != websocket.TextMessage {
		log.Printf("[router] received %d bytes from client (id=%s) — ignored", len(data), connID)
		return
	}

	if !isRTCShadowDownstreamPacket(data) {
		log.Printf("[router] client text ignored (id=%s, bytes=%d)", connID, len(data))
		return
	}

	if !registry.SendToExtension(websocket.TextMessage, data) {
		log.Printf("[router] no extension connected for shadow response (id=%s)", connID)
	}
}

func isVideoUpdatePacket(data []byte) bool {
	var pkt Packet
	if err := json.Unmarshal(data, &pkt); err != nil {
		return false
	}
	return pkt.Type == "VIDEO_UPDATE"
}

func isRTCShadowUpstreamPacket(data []byte) bool {
	var pkt Packet
	if err := json.Unmarshal(data, &pkt); err != nil {
		return false
	}

	switch pkt.Type {
	case PacketTypeRTCShadowRemote, PacketTypeRTCShadowLocal, PacketTypeRTCShadowClose, PacketTypeRTCShadowCandidate:
		return true
	default:
		return false
	}
}

func isRTCShadowDownstreamPacket(data []byte) bool {
	var pkt Packet
	if err := json.Unmarshal(data, &pkt); err != nil {
		return false
	}

	switch pkt.Type {
	case PacketTypeRTCShadowReady, PacketTypeRTCShadowError, PacketTypeRTCShadowCandidate:
		return true
	default:
		return false
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

	mediaBridge.tryForwardBinary(
		hdr.Payload.TrackType,
		hdr.Payload.MimeType,
		hdr.Payload.Seq,
		hdr.Payload.IsInitSegment,
		data, // forward framed packet for bcr_client metadata parsing
	)

	registry.Broadcast(websocket.TextMessage, outBytes)
}

const (
	bridgeControlURL     = "ws://localhost:8081?channel=control"
	bridgeDataURL        = "ws://localhost:8081?channel=data"
	bridgeReconnectDelay = 2 * time.Second
	bridgeControlQueue   = 512
	bridgeDataQueue      = 512

	// Safe mode toggle:
	// "all" (default), "video-only", "audio-only"
	bridgeFilterMode = "all"
)

type bridgeMessage struct {
	msgType    int
	kind       string
	data       []byte
	packetType string
	bridgeID   string

	track string
	mime  string
	seq   int64
	init  bool
}

type bridgeChunk struct {
	track string
	mime  string
	seq   int64
	init  bool
	data  []byte
}

type bridgeForwarder struct {
	startOnce sync.Once
	controlCh chan bridgeMessage
	dataCh    chan bridgeMessage
}

var mediaBridge = newBridgeForwarder()

func newBridgeForwarder() *bridgeForwarder {
	return &bridgeForwarder{
		controlCh: make(chan bridgeMessage, bridgeControlQueue),
		dataCh:    make(chan bridgeMessage, bridgeDataQueue),
	}
}

func (b *bridgeForwarder) writeLoop(conn *websocket.Conn, sendCh <-chan bridgeMessage, channel string) error {
	for msg := range sendCh {
		if err := conn.WriteMessage(msg.msgType, msg.data); err != nil {
			log.Printf("[Bridge] ERROR send failed channel=%s type=%d kind=%s: %v", channel, msg.msgType, msg.kind, err)
			return err
		}

		if msg.kind == "telemetry" {
			if msg.packetType == PacketTypeRTCShadowRemote ||
				msg.packetType == PacketTypeRTCShadowLocal ||
				msg.packetType == PacketTypeRTCShadowClose ||
				msg.packetType == PacketTypeRTCShadowCandidate {
				log.Printf("[Bridge] SHADOW_UP -> client type=%s bridgeId=%s", msg.packetType, msg.bridgeID)
			}
			continue
		}
	}

	return nil
}

func (b *bridgeForwarder) start() {
	b.startOnce.Do(func() {
		go b.runControlPath()
		go b.runDataPath()
	})
}

func (b *bridgeForwarder) runControlPath() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial(bridgeControlURL, nil)
		if err != nil {
			log.Printf("[Bridge] control reconnecting... dial failed: %v", err)
			time.Sleep(bridgeReconnectDelay)
			continue
		}

		log.Printf("[Bridge] control connected to bcr_client")

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			for {
				msgType, data, err := conn.ReadMessage()
				if err != nil {
					return
				}

				if msgType != websocket.TextMessage {
					continue
				}

				if !isRTCShadowDownstreamPacket(data) {
					continue
				}

				packetType, bridgeID := packetTypeAndBridgeID(data)

				if _bridgeRegistry == nil {
					log.Printf("[Bridge] SHADOW_DOWN dropped (no registry) type=%s bridgeId=%s", packetType, bridgeID)
					continue
				}

				if !_bridgeRegistry.SendToExtension(websocket.TextMessage, data) {
					log.Printf("[Bridge] SHADOW_DOWN dropped (no extension) type=%s bridgeId=%s", packetType, bridgeID)
					continue
				}

				log.Printf("[Bridge] SHADOW_DOWN -> extension type=%s bridgeId=%s", packetType, bridgeID)
			}
		}()

		err = b.writeLoop(conn, b.controlCh, "control")
		if err != nil {
			log.Printf("[Bridge] control disconnected: %v", err)
		} else {
			log.Printf("[Bridge] control disconnected")
		}

		_ = conn.Close()
		<-readDone
		log.Printf("[Bridge] control reconnecting...")
		time.Sleep(bridgeReconnectDelay)
	}
}

func (b *bridgeForwarder) runDataPath() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial(bridgeDataURL, nil)
		if err != nil {
			log.Printf("[Bridge] data reconnecting... dial failed: %v", err)
			time.Sleep(bridgeReconnectDelay)
			continue
		}

		log.Printf("[Bridge] data connected to bcr_client")

		err = b.writeLoop(conn, b.dataCh, "data")
		if err != nil {
			log.Printf("[Bridge] data disconnected: %v", err)
		} else {
			log.Printf("[Bridge] data disconnected")
		}

		_ = conn.Close()
		log.Printf("[Bridge] data reconnecting...")
		time.Sleep(bridgeReconnectDelay)
	}
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

func (b *bridgeForwarder) tryForwardBinary(track, mime string, seq int64, isInit bool, data []byte) {
	b.start()
	ok, reason := bridgeShouldForward(track, isInit)
	if !ok {
		log.Printf("[Bridge] SKIP track=%s reason=%s", track, reason)
		return
	}

	payload := append([]byte(nil), data...)

	select {
	case b.dataCh <- bridgeMessage{
		msgType: websocket.BinaryMessage,
		kind:    "media",
		data:    payload,
		track:   track,
		mime:    mime,
		seq:     seq,
		init:    isInit,
	}:
	default:
		if isInit {
			log.Printf("[Bridge] SKIP media init reason=data_queue_full track=%s", track)
		}
	}
}

func (b *bridgeForwarder) tryForwardText(data []byte) {
	b.start()
	packetType, bridgeID := packetTypeAndBridgeID(data)
	isShadow := packetType == PacketTypeRTCShadowRemote ||
		packetType == PacketTypeRTCShadowLocal ||
		packetType == PacketTypeRTCShadowClose ||
		packetType == PacketTypeRTCShadowCandidate

	if len(b.controlCh) > (bridgeControlQueue*3)/4 {
		if isShadow {
			log.Printf("[Bridge] SHADOW_UP dropped reason=bridge_busy type=%s bridgeId=%s", packetType, bridgeID)
		}
		return
	}

	payload := append([]byte(nil), data...)

	select {
	case b.controlCh <- bridgeMessage{
		msgType:    websocket.TextMessage,
		kind:       "telemetry",
		data:       payload,
		packetType: packetType,
		bridgeID:   bridgeID,
	}:
		if isShadow {
			log.Printf("[Bridge] SHADOW_UP queued type=%s bridgeId=%s", packetType, bridgeID)
		}
	default:
		if isShadow {
			log.Printf("[Bridge] SHADOW_UP dropped reason=bridge_queue_full type=%s bridgeId=%s", packetType, bridgeID)
		}
	}
}

func packetTypeAndBridgeID(data []byte) (string, string) {
	var pkt Packet
	if err := json.Unmarshal(data, &pkt); err != nil {
		return "unknown", ""
	}

	type withBridgeID struct {
		BridgeID string `json:"bridgeId"`
	}

	var payload withBridgeID
	_ = json.Unmarshal(pkt.Payload, &payload)

	return pkt.Type, payload.BridgeID
}
