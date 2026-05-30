//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	wtsapi32                = syscall.NewLazyDLL("wtsapi32.dll")
	wtsVirtualChannelOpenEx = wtsapi32.NewProc("WTSVirtualChannelOpenEx")
	wtsVirtualChannelClose  = wtsapi32.NewProc("WTSVirtualChannelClose")
	wtsVirtualChannelRead   = wtsapi32.NewProc("WTSVirtualChannelRead")
	wtsVirtualChannelWrite  = wtsapi32.NewProc("WTSVirtualChannelWrite")

	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentPID    = kernel32.NewProc("GetCurrentProcessId")
	procPIDToSessionID   = kernel32.NewProc("ProcessIdToSessionId")
	procGetLastError     = kernel32.NewProc("GetLastError")
)

const (
	WTS_CURRENT_SESSION        = 0xFFFFFFFF
	WTS_CHANNEL_OPTION_DYNAMIC = 0x00000001
)

const (
	channelFlagFirst = 0x00000001
	channelFlagLast  = 0x00000002
	pduHeaderSize    = 8
)

// Windows error codes relevant to DVC operations
const (
	ERROR_IO_INCOMPLETE  = 996
	ERROR_IO_PENDING     = 997
	WAIT_TIMEOUT         = 258
	ERROR_NO_DATA        = 232
	ERROR_INVALID_HANDLE = 6
)

var (
	ErrClosed  = errors.New("dvc: closed")
	ErrTimeout = errors.New("dvc: read timeout (no data)")
)

type DVCConn struct {
	mu     sync.Mutex
	handle syscall.Handle
	closed bool

	// internal stream buffer for PDU parsing
	inbuf []byte

	// reassembly buffer for logical messages
	assembling bool
	msgbuf     []byte

	// hard cap to prevent OOM on corrupt header
	maxMessage int
}

// GetCurrentSessionID returns the Terminal Services session ID for the
// current process. Returns 0xFFFFFFFF on failure.
func GetCurrentSessionID() uint32 {
	pid, _, _ := procGetCurrentPID.Call()

	var sessionID uint32
	r1, _, _ := procPIDToSessionID.Call(
		pid,
		uintptr(unsafe.Pointer(&sessionID)),
	)
	if r1 == 0 {
		return 0xFFFFFFFF
	}
	return sessionID
}

// LogSessionDiagnostics logs session/process information useful for debugging
// DVC handle issues.
func LogSessionDiagnostics() {
	sessionID := GetCurrentSessionID()

	if sessionID == 0 {
		log.Println("[DVC DIAG] ⚠️  WARNING: Process is in Session 0 (services session)!")
		log.Println("[DVC DIAG]    WTS_CURRENT_SESSION will NOT reach the interactive RDP desktop.")
		log.Println("[DVC DIAG]    bcr_host MUST run inside the interactive RDP session (Session 1+).")
	} else if sessionID == 0xFFFFFFFF {
		log.Println("[DVC DIAG] ⚠️  WARNING: Could not determine session ID (ProcessIdToSessionId failed)")
	} else {
		log.Printf("[DVC DIAG] ✅ Process session ID = %d (interactive session)", sessionID)
	}

	pid, _, _ := procGetCurrentPID.Call()
	log.Printf("[DVC DIAG] Process PID = %d", pid)
}

func OpenDVC(channelName string) (*DVCConn, error) {
	if err := wtsapi32.Load(); err != nil {
		return nil, fmt.Errorf("wtsapi32.dll not available: %w", err)
	}

	// Log session diagnostics on first DVC open attempt
	sessionID := GetCurrentSessionID()
	log.Printf("[DVC] Opening channel %q (WTS_CURRENT_SESSION=0x%X, process_session=%d)",
		channelName, WTS_CURRENT_SESSION, sessionID)

	if sessionID == 0 {
		log.Println("[DVC] ⚠️  Session 0 detected — DVC will likely fail. Run bcr_host from the RDP desktop session.")
	}

	// Allow override via environment variable for testing
	targetSession := uintptr(WTS_CURRENT_SESSION)
	if envSession := os.Getenv("BCR_SESSION_ID"); envSession != "" {
		if sid, err := strconv.ParseUint(envSession, 10, 32); err == nil {
			targetSession = uintptr(sid)
			log.Printf("[DVC] Using explicit session ID from BCR_SESSION_ID=%d", sid)
		}
	}

	namePtr, err := syscall.BytePtrFromString(channelName)
	if err != nil {
		return nil, err
	}

	r1, _, callErr := wtsVirtualChannelOpenEx.Call(
		targetSession,
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(WTS_CHANNEL_OPTION_DYNAMIC),
	)
	h := syscall.Handle(r1)
	if h == 0 {
		errno, _ := callErr.(syscall.Errno)
		return nil, fmt.Errorf("WTSVirtualChannelOpenEx failed: %v (win32 error=%d/0x%X, session=%d)",
			callErr, uint32(errno), uint32(errno), sessionID)
	}

	log.Printf("[DVC] ✅ Channel %q opened successfully (handle=0x%X)", channelName, h)

	return &DVCConn{
		handle:     h,
		inbuf:      make([]byte, 0, 128*1024),
		msgbuf:     make([]byte, 0, 128*1024),
		maxMessage: 64 * 1024 * 1024,
	}, nil
}

func (d *DVCConn) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	h := d.handle
	d.handle = 0
	d.mu.Unlock()

	if h != 0 {
		log.Printf("[DVC] Closing channel handle 0x%X", h)
		_, _, _ = wtsVirtualChannelClose.Call(uintptr(h))
	}
	return nil
}

func (d *DVCConn) WriteMessage(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}

	for attempt := 1; attempt <= 10; attempt++ {
		d.mu.Lock()
		if d.closed || d.handle == 0 {
			d.mu.Unlock()
			return ErrClosed
		}
		h := d.handle
		d.mu.Unlock()

		var written uint32
		r1, _, callErr := wtsVirtualChannelWrite.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&payload[0])),
			uintptr(len(payload)),
			uintptr(unsafe.Pointer(&written)),
		)
		if r1 != 0 {
			if int(written) != len(payload) {
				return fmt.Errorf("short write: wrote=%d expected=%d", written, len(payload))
			}
			return nil
		}

		errno, _ := callErr.(syscall.Errno)
		errCode := uint32(errno)
		if errCode == ERROR_INVALID_HANDLE {
			log.Printf("[DVC] Write got ERROR_INVALID_HANDLE (attempt %d/10), channel might not be fully established yet. Retrying in 100ms...", attempt)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		return fmt.Errorf("WTSVirtualChannelWrite failed: %v (win32=%d/0x%X, size=%d)",
			callErr, errCode, errCode, len(payload))
	}
	return fmt.Errorf("WTSVirtualChannelWrite failed: ERROR_INVALID_HANDLE (6) after 10 attempts")
}

// ReadMessage reads and reassembles FIRST..LAST PDUs into one logical message.
// It tolerates partial reads and multiple PDUs in one buffer.
// A timeout return from WTSVirtualChannelRead is NOT treated as a fatal error —
// it returns ErrTimeout so callers can retry.
func (d *DVCConn) ReadMessage(timeoutMs uint32) ([]byte, error) {
	tmp := make([]byte, 64*1024)

	for {
		// Parse any complete PDUs already in buffer
		if msg, ok, err := d.tryParseFromBuffer(); err != nil {
			return nil, err
		} else if ok {
			return msg, nil
		}

		// Need more bytes -> do a WTS read
		var n uint32
		var r1 uintptr
		var callErr error

		for attempt := 1; attempt <= 10; attempt++ {
			d.mu.Lock()
			if d.closed || d.handle == 0 {
				d.mu.Unlock()
				return nil, ErrClosed
			}
			h := d.handle
			d.mu.Unlock()

			r1, _, callErr = wtsVirtualChannelRead.Call(
				uintptr(h),
				uintptr(timeoutMs),
				uintptr(unsafe.Pointer(&tmp[0])),
				uintptr(len(tmp)),
				uintptr(unsafe.Pointer(&n)),
			)
			if r1 != 0 {
				break
			}

			errno, isErrno := callErr.(syscall.Errno)
			if isErrno {
				errCode := uint32(errno)
				if errCode == ERROR_INVALID_HANDLE {
					log.Printf("[DVC] Read got ERROR_INVALID_HANDLE (attempt %d/10), channel might not be fully established yet. Retrying in 100ms...", attempt)
					time.Sleep(100 * time.Millisecond)
					continue
				}
			}
			break
		}

		if r1 == 0 {
			d.mu.Lock()
			closed := d.closed
			d.mu.Unlock()
			if closed {
				return nil, ErrClosed
			}

			// Check if this is a timeout/no-data condition (non-fatal)
			errno, isErrno := callErr.(syscall.Errno)
			if isErrno {
				errCode := uint32(errno)
				switch errCode {
				case ERROR_IO_INCOMPLETE, ERROR_IO_PENDING, WAIT_TIMEOUT, ERROR_NO_DATA:
					// Timeout or no data available — this is expected and NOT an error.
					// Return ErrTimeout so the caller can decide to retry.
					return nil, ErrTimeout
				}
			}

			// Genuine error
			return nil, fmt.Errorf("WTSVirtualChannelRead failed: %v (win32=%d/0x%X)",
				callErr, uint32(errno), uint32(errno))
		}
		if n == 0 {
			// treat as "no data" not EOF; loop again
			continue
		}

		d.inbuf = append(d.inbuf, tmp[:n]...)
	}
}

func (d *DVCConn) tryParseFromBuffer() ([]byte, bool, error) {
	// Need at least a header
	if len(d.inbuf) < pduHeaderSize {
		return nil, false, nil
	}

	// Peek header
	pduLen := binary.LittleEndian.Uint32(d.inbuf[0:4])
	flags := binary.LittleEndian.Uint32(d.inbuf[4:8])

	if int(pduLen) > d.maxMessage {
		// Corrupt; reset all state
		d.inbuf = d.inbuf[:0]
		d.msgbuf = d.msgbuf[:0]
		d.assembling = false
		return nil, false, fmt.Errorf("dvc: pdu length too large: %d", pduLen)
	}

	total := pduHeaderSize + int(pduLen)
	if len(d.inbuf) < total {
		// Not enough yet
		return nil, false, nil
	}

	// Consume exactly one PDU from buffer
	payload := d.inbuf[pduHeaderSize:total]
	d.inbuf = d.inbuf[total:]

	// Reassembly rules
	if flags&channelFlagFirst != 0 {
		d.msgbuf = d.msgbuf[:0]
		d.assembling = true
	}
	if !d.assembling {
		// middle/last without FIRST => protocol violation; reset
		d.msgbuf = d.msgbuf[:0]
		return nil, false, errors.New("dvc: got non-FIRST chunk without an active message")
	}

	d.msgbuf = append(d.msgbuf, payload...)

	if flags&channelFlagLast != 0 {
		out := make([]byte, len(d.msgbuf))
		copy(out, d.msgbuf)
		d.msgbuf = d.msgbuf[:0]
		d.assembling = false
		return out, true, nil
	}

	return nil, false, nil
}
