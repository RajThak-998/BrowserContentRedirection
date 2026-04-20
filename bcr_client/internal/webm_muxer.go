package engine

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/at-wat/ebml-go/webm"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

type trackMuxer struct {
	ws            []webm.BlockWriteCloser
	writeBuffer   *bytes.Buffer
	seq           uint32
	isInitEmitted bool
	trackType     string // "audio" or "video"
	mimeType      string
	codec         string
	lastFlushTime time.Time
}

func (tm *trackMuxer) Write(p []byte) (n int, err error) {
	return tm.writeBuffer.Write(p)
}

func (tm *trackMuxer) Close() error {
	return nil
}

func (tm *trackMuxer) flush(onVideoChunk func([]byte)) {
	if tm.writeBuffer.Len() == 0 {
		return
	}

	data := make([]byte, tm.writeBuffer.Len())
	copy(data, tm.writeBuffer.Bytes())
	tm.writeBuffer.Reset()
	tm.lastFlushTime = time.Now()

	isInit := !tm.isInitEmitted
	tm.isInitEmitted = true
	tm.seq++

	hdr := map[string]interface{}{
		"type": "MEDIA_CHUNK",
		"payload": map[string]interface{}{
			"trackType":     tm.trackType,
			"mimeType":      tm.mimeType,
			"codec":         tm.codec,
			"seq":           tm.seq,
			"isInitSegment": isInit,
		},
	}
	hdrBytes, _ := json.Marshal(hdr)

	out := make([]byte, 4+len(hdrBytes)+len(data))
	out[0] = byte(len(hdrBytes))
	out[1] = byte(len(hdrBytes) >> 8)
	out[2] = byte(len(hdrBytes) >> 16)
	out[3] = byte(len(hdrBytes) >> 24)
	copy(out[4:], hdrBytes)
	copy(out[4+len(hdrBytes):], data)

	if onVideoChunk != nil {
		onVideoChunk(out)
	}
}

// webmMuxer handles reassembling RTP packets into codec frames and muxing them
// into independent WebM streams (one for audio, one for video) compatible with MSE.
type webmMuxer struct {
	bridgeID     string
	logf         func(format string, v ...interface{})
	onVideoChunk func(data []byte)

	mu sync.Mutex

	audioMuxer *trackMuxer
	videoMuxer *trackMuxer

	vp8Depack     *codecs.VP8Packet
	vp8FrameBuf   *bytes.Buffer
	baseTimestamp uint32
}

func newWebMMuxer(bridgeID string, logf func(format string, v ...interface{}), onVideoChunk func([]byte), ptCodecMap map[uint8]CodecInfo) *webmMuxer {
	m := &webmMuxer{
		bridgeID:     bridgeID,
		logf:         logf,
		onVideoChunk: onVideoChunk,
		vp8Depack:    &codecs.VP8Packet{},
		vp8FrameBuf:  &bytes.Buffer{},
	}

	// Initialize separate muxers for audio and video
	for _, c := range ptCodecMap {
		mime := strings.ToLower(c.MimeType)
		if strings.Contains(mime, "video/vp8") && m.videoMuxer == nil {
			m.videoMuxer = &trackMuxer{
				writeBuffer: &bytes.Buffer{},
				trackType:   "video",
				mimeType:    "video/webm; codecs=\"vp8\"",
				codec:       "vp8",
			}
			tracks := []webm.TrackEntry{
				{
					Name:        "Video",
					TrackNumber: 1,
					TrackType:   1, // Video
					CodecID:     "V_VP8",
				},
			}
			writers, err := webm.NewSimpleBlockWriter(m.videoMuxer, tracks)
			if err != nil {
				m.logf("[webm][%s] failed to create video writer: %v", bridgeID, err)
				m.videoMuxer = nil
			} else {
				m.videoMuxer.ws = writers
				m.logf("[webm][%s] initialized video track", bridgeID)
			}
		}

		if strings.Contains(mime, "audio/opus") && m.audioMuxer == nil {
			m.audioMuxer = &trackMuxer{
				writeBuffer: &bytes.Buffer{},
				trackType:   "audio",
				mimeType:    "audio/webm; codecs=\"opus\"",
				codec:       "opus",
			}
			tracks := []webm.TrackEntry{
				{
					Name:        "Audio",
					TrackNumber: 1,
					TrackType:   2, // Audio
					CodecID:     "A_OPUS",
					Audio: &webm.Audio{
						SamplingFrequency: float64(c.ClockRate),
						Channels:          uint64(c.Channels),
					},
				},
			}
			writers, err := webm.NewSimpleBlockWriter(m.audioMuxer, tracks)
			if err != nil {
				m.logf("[webm][%s] failed to create audio writer: %v", bridgeID, err)
				m.audioMuxer = nil
			} else {
				m.audioMuxer.ws = writers
				m.logf("[webm][%s] initialized audio track", bridgeID)
			}
		}
	}

	if m.videoMuxer == nil && m.audioMuxer == nil {
		m.logf("[webm][%s] no supported codecs found to mux", bridgeID)
	}

	return m
}

func (m *webmMuxer) WriteRTP(pkt *rtp.Packet, ptCodecMap map[uint8]CodecInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	codec, ok := ptCodecMap[pkt.Header.PayloadType]
	if !ok {
		return
	}

	mime := strings.ToLower(codec.MimeType)

	if m.baseTimestamp == 0 {
		m.baseTimestamp = pkt.Timestamp
	}

	if strings.Contains(mime, "audio/opus") && m.audioMuxer != nil && len(m.audioMuxer.ws) > 0 {
		tsMs := (pkt.Timestamp - m.baseTimestamp) / (codec.ClockRate / 1000)
		m.audioMuxer.ws[0].Write(true, int64(tsMs), pkt.Payload)
		
		if time.Since(m.audioMuxer.lastFlushTime) > 100*time.Millisecond {
			m.audioMuxer.flush(m.onVideoChunk)
		}
	} else if strings.Contains(mime, "video/vp8") && m.videoMuxer != nil && len(m.videoMuxer.ws) > 0 {
		if len(pkt.Payload) == 0 {
			return
		}
		
		payload, err := m.vp8Depack.Unmarshal(pkt.Payload)
		if err != nil {
			return
		}

		isKeyframe := len(payload) > 0 && (payload[0]&0x01) == 0

		m.vp8FrameBuf.Write(payload)

		if pkt.Marker {
			frameData := make([]byte, m.vp8FrameBuf.Len())
			copy(frameData, m.vp8FrameBuf.Bytes())
			m.vp8FrameBuf.Reset()

			tsMs := (pkt.Timestamp - m.baseTimestamp) / (codec.ClockRate / 1000)
			
			m.videoMuxer.ws[0].Write(isKeyframe, int64(tsMs), frameData)
			m.videoMuxer.flush(m.onVideoChunk)
		}
	}
}

func (m *webmMuxer) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if m.audioMuxer != nil {
		for _, w := range m.audioMuxer.ws {
			w.Close()
		}
		m.audioMuxer.flush(m.onVideoChunk)
	}
	
	if m.videoMuxer != nil {
		for _, w := range m.videoMuxer.ws {
			w.Close()
		}
		m.videoMuxer.flush(m.onVideoChunk)
	}
	
	return nil
}
