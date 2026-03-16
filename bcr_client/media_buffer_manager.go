package main

import "sync"

const (
	MaxChunksPerTrack   = 512
	MediaBufferLogEvery = 10
)

type MediaChunk struct {
	Seq  int64
	Data []byte
}

type TrackBuffer struct {
	TrackKey    string
	InitChunk   []byte
	InitSeq     int64
	MediaChunks []MediaChunk
	LastSeq     int64
	TotalBytes  int64
	GapCount    int64
	Dropped     int64

	storedCount int64
}

type BufferSnapshot struct {
	TrackKey        string
	MediaChunkCount int
	TotalBytes      int64
	GapCount        int64
	Dropped         int64
	InitSeq         int64
	LastSeq         int64
}

type MediaBufferManager struct {
	mu      sync.Mutex
	buffers map[string]*TrackBuffer
}

func NewMediaBufferManager() *MediaBufferManager {
	return &MediaBufferManager{
		buffers: make(map[string]*TrackBuffer),
	}
}

var mediaBufferManager = NewMediaBufferManager()

func BuildTrackKey(trackType, sourceBufferID, codec string) string {
	if trackType == "" {
		trackType = "unknown"
	}
	if sourceBufferID == "" {
		sourceBufferID = "unknown"
	}
	if codec == "" {
		codec = "unknown"
	}
	return trackType + "|" + sourceBufferID + "|" + codec
}

// StoreChunk stores one chunk for a track.
// It returns a snapshot and whether sampled logging should occur.
func (m *MediaBufferManager) StoreChunk(trackKey string, seq int64, isInit bool, data []byte) (BufferSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tb, ok := m.buffers[trackKey]
	if !ok {
		tb = &TrackBuffer{TrackKey: trackKey}
		m.buffers[trackKey] = tb
	}

	// Sequence continuity check.
	if tb.LastSeq != 0 && seq != tb.LastSeq+1 {
		tb.GapCount++
	}

	// IMPORTANT tweak: update LastSeq for init and media segments.
	tb.LastSeq = seq

	// Copy bytes for memory safety; never keep references to frame backing storage.
	copied := append([]byte(nil), data...)

	if isInit {
		// Replace init segment; keep newest.
		if tb.InitChunk != nil {
			tb.TotalBytes -= int64(len(tb.InitChunk))
		}
		tb.InitChunk = copied
		tb.InitSeq = seq
		tb.TotalBytes += int64(len(copied))
	} else {
		tb.MediaChunks = append(tb.MediaChunks, MediaChunk{
			Seq:  seq,
			Data: copied,
		})
		tb.TotalBytes += int64(len(copied))

		if len(tb.MediaChunks) > MaxChunksPerTrack {
			oldest := tb.MediaChunks[0]
			tb.MediaChunks = tb.MediaChunks[1:]
			tb.TotalBytes -= int64(len(oldest.Data))
			tb.Dropped++
		}
	}

	tb.storedCount++
	shouldLog := tb.storedCount == 1 || tb.storedCount%MediaBufferLogEvery == 0

	snapshot := BufferSnapshot{
		TrackKey:        tb.TrackKey,
		MediaChunkCount: len(tb.MediaChunks),
		TotalBytes:      tb.TotalBytes,
		GapCount:        tb.GapCount,
		Dropped:         tb.Dropped,
		InitSeq:         tb.InitSeq,
		LastSeq:         tb.LastSeq,
	}

	return snapshot, shouldLog
}
