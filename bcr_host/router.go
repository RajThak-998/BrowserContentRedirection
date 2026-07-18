package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
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

		if isVideoUpdatePacket(data) || isRTCShadowUpstreamPacket(data) || isVideoLifecyclePacket(data) {
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
	case PacketTypeRTCShadowRemote, PacketTypeRTCShadowLocal, PacketTypeRTCShadowClose, PacketTypeRTCShadowCandidate, PacketTypeRTCShadowIceServers, PacketTypeRTCShadowPreWarm:
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
	case PacketTypeRTCShadowReady, PacketTypeRTCShadowError, PacketTypeRTCShadowCandidate, PacketTypeRTCShadowPreWarmReady:
		return true
	default:
		return false
	}
}

func isVideoLifecyclePacket(data []byte) bool {
	var pkt Packet
	if err := json.Unmarshal(data, &pkt); err != nil {
		return false
	}
	return pkt.Type == "VIDEO_ADDED" || pkt.Type == "VIDEO_REMOVED"
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
		VideoID:        hdr.Payload.VideoID,
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
	bridgeReconnectDelay = 2 * time.Second
	bridgeControlQueue   = 512
)

type bridgeMessage struct {
	msgType    int
	data       []byte
	packetType string
	bridgeID   string
}

type SignalingConn interface {
	WriteMessage(msgType int, data []byte) error
	ReadMessage() (int, []byte, error)
	Close() error
}

type TCPSignalingConn struct {
	net.Conn
}

func (c *TCPSignalingConn) WriteMessage(msgType int, data []byte) error {
	length := uint32(len(data))
	buf := make([]byte, 4+length)
	binary.BigEndian.PutUint32(buf[0:4], length)
	copy(buf[4:], data)
	_, err := c.Conn.Write(buf)
	return err
}

func (c *TCPSignalingConn) ReadMessage() (int, []byte, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c.Conn, lenBuf); err != nil {
		return 0, nil, err
	}

	length := binary.BigEndian.Uint32(lenBuf)
	if length > 10*1024*1024 { // 10MB safety cap
		leLength := binary.LittleEndian.Uint32(lenBuf)
		if leLength <= 10*1024*1024 {
			length = leLength
		}
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.Conn, payload); err != nil {
		return 0, nil, err
	}
	return 1, payload, nil // 1 = TextMessage
}

type DVCSignalingConn struct {
	*DVCConn
	buf []byte
}

func (d *DVCSignalingConn) WriteMessage(msgType int, data []byte) error {
	length := uint32(len(data))
	payload := make([]byte, 4+length)
	binary.BigEndian.PutUint32(payload[0:4], length)
	copy(payload[4:], data)
	log.Printf("[DVC DIAG] WriteMessage: %d bytes total, prefix=[%02X %02X %02X %02X]",
		len(payload), payload[0], payload[1], payload[2], payload[3])
	return d.DVCConn.WriteMessage(payload)
}

func isAllZeros(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func (d *DVCSignalingConn) ReadMessage() (int, []byte, error) {
	for {
		if len(d.buf) >= 4 {
			length := binary.BigEndian.Uint32(d.buf[0:4])
			if length == 0 {
				// Skip/discard leading zero byte (likely RDP channel padding/init)
				d.buf = d.buf[1:]
				continue
			}
			if length > 10*1024*1024 { // 10MB safety cap
				leLength := binary.LittleEndian.Uint32(d.buf[0:4])
				if leLength <= 10*1024*1024 {
					length = leLength
				} else {
					log.Printf("[DVC DIAG] Huge packet length BE=%d LE=%d. Clearing buffer to realign.", length, leLength)
					d.buf = nil
					continue
				}
			}

			if len(d.buf) >= 4+int(length) {
				payload := make([]byte, length)
				copy(payload, d.buf[4:4+length])
				d.buf = d.buf[4+length:]
				return 1, payload, nil // 1 = TextMessage
			}
		}

		data, err := d.DVCConn.ReadMessage(20)
		if err != nil {
			// ErrTimeout means 20ms elapsed with no data — this is normal.
			// Loop and retry rather than killing the bridge.
			if errors.Is(err, ErrTimeout) {
				continue
			}
			return 0, nil, err
		}

		if isAllZeros(data) {
			log.Printf("[DVC DIAG] Discarding %d bytes of pure zero padding/keep-alive packet", len(data))
			continue
		}

		d.buf = append(d.buf, data...)
	}
}

type bridgeForwarder struct {
	startOnce sync.Once
	sendCh    chan bridgeMessage
}

var mediaBridge = newBridgeForwarder()

func newBridgeForwarder() *bridgeForwarder {
	return &bridgeForwarder{
		sendCh: make(chan bridgeMessage, bridgeControlQueue),
	}
}

func (b *bridgeForwarder) start() {
	b.startOnce.Do(func() {
		go b.runBridgeLoop()
	})
}

func (b *bridgeForwarder) runBridgeLoop() {
	for {
		var conn SignalingConn
		var err error

		log.Println("[Bridge] Attempting to open RDP Dynamic Virtual Channel 'BCR_VC'...")
		dvcConn, err := OpenDVC("BCR_VC")
		if err == nil {
			log.Println("[Bridge] ✅ DVC transport active — signaling via RDP Dynamic Virtual Channel 'BCR_VC'")
			conn = &DVCSignalingConn{DVCConn: dvcConn}
		} else {
			log.Printf("[Bridge] DVC not available (%v). Falling back to TCP loopback to localhost:8081...", err)
			tcpConn, tcpErr := net.Dial("tcp", "127.0.0.1:8081")
			if tcpErr != nil {
				log.Printf("[Bridge] TCP fallback dial failed: %v. Retrying in %v...", tcpErr, bridgeReconnectDelay)
				time.Sleep(bridgeReconnectDelay)
				continue
			}
			log.Println("[Bridge] ✅ TCP transport active — signaling via TCP loopback 127.0.0.1:8081")
			conn = &TCPSignalingConn{Conn: tcpConn}
		}

		stopCh := make(chan struct{})
		var stopOnce sync.Once
		triggerStop := func(reason string) {
			stopOnce.Do(func() {
				log.Printf("[Bridge] Triggering bridge shutdown, reason: %s", reason)
				close(stopCh)
				_ = conn.Close() // Force-close underlying connection to unblock any pending read/write operations
			})
		}

		var wg sync.WaitGroup

		// Read loop
		wg.Add(1)
		go func() {
			defer func() {
				triggerStop("read loop exited")
				wg.Done()
			}()
			for {
				select {
				case <-stopCh:
					return
				default:
				}

				msgType, data, err := conn.ReadMessage()
				if err != nil {
					// If we were stopped, ignore any errors as they are expected due to connection close
					select {
					case <-stopCh:
						return
					default:
					}
					log.Printf("[Bridge] connection read error: %v", err)
					return
				}

				if msgType != 1 {
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

		// Write loop
		wg.Add(1)
		go func() {
			defer func() {
				triggerStop("write loop exited")
				wg.Done()
			}()
			for {
				select {
				case msg := <-b.sendCh:
					select {
					case <-stopCh:
						return
					default:
					}

					if err := conn.WriteMessage(msg.msgType, msg.data); err != nil {
						select {
						case <-stopCh:
							return
						default:
						}
						log.Printf("[Bridge] connection write error: %v", err)
						return
					}

					if msg.packetType == PacketTypeRTCShadowRemote ||
						msg.packetType == PacketTypeRTCShadowLocal ||
						msg.packetType == PacketTypeRTCShadowClose ||
						msg.packetType == PacketTypeRTCShadowCandidate ||
						msg.packetType == PacketTypeRTCShadowIceServers ||
						msg.packetType == PacketTypeRTCShadowPreWarm {
						log.Printf("[Bridge] SHADOW_UP -> client type=%s bridgeId=%s", msg.packetType, msg.bridgeID)
					}

				case <-stopCh:
					return
				}
			}
		}()

		// Wait for both read and write loops to fully exit
		<-stopCh
		wg.Wait()

		log.Println("[Bridge] Reconnecting bridge path...")
		time.Sleep(bridgeReconnectDelay)
	}
}

func (b *bridgeForwarder) tryForwardBinary(track, mime string, seq int64, isInit bool, data []byte) {
	b.start()

	if len(b.sendCh) > (bridgeControlQueue*3)/4 {
		if isInit {
			log.Printf("[Bridge] MEDIA_CHUNK_BIN dropped reason=bridge_busy track=%s seq=%d", track, seq)
		}
		return
	}

	// data is the full framed binary packet: [u32 headerLen LE][headerJSON][rawChunk].
	// Copy it so the caller's buffer can be reused safely.
	payload := append([]byte(nil), data...)

	select {
	case b.sendCh <- bridgeMessage{
		msgType:    websocket.BinaryMessage,
		data:       payload,
		packetType: "MEDIA_CHUNK",
		bridgeID:   "",
	}:
	default:
		if isInit {
			log.Printf("[Bridge] MEDIA_CHUNK_BIN dropped reason=bridge_queue_full track=%s seq=%d", track, seq)
		}
	}
}

func (b *bridgeForwarder) tryForwardText(data []byte) {
	b.start()
	packetType, bridgeID := packetTypeAndBridgeID(data)
	isShadow := packetType == PacketTypeRTCShadowRemote ||
		packetType == PacketTypeRTCShadowLocal ||
		packetType == PacketTypeRTCShadowClose ||
		packetType == PacketTypeRTCShadowCandidate ||
		packetType == PacketTypeRTCShadowIceServers ||
		packetType == PacketTypeRTCShadowPreWarm
	// Video lifecycle events forwarded so bcr_client can manage overlay window.
	isVideoLifecycle := packetType == "VIDEO_ADDED" || packetType == "VIDEO_REMOVED"

	if len(b.sendCh) > (bridgeControlQueue*3)/4 {
		if isShadow {
			log.Printf("[Bridge] SHADOW_UP dropped reason=bridge_busy type=%s bridgeId=%s", packetType, bridgeID)
		}
		if !isVideoLifecycle {
			return
		}
	}

	payload := append([]byte(nil), data...)

	select {
	case b.sendCh <- bridgeMessage{
		msgType:    1,
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
