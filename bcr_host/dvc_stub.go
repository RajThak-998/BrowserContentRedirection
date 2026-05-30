//go:build !windows

package main

import (
	"errors"
	"log"
)

var ErrTimeout = errors.New("dvc stub: timeout")

type DVCConn struct{}

func (d *DVCConn) Close() error {
	return nil
}

func (d *DVCConn) WriteMessage(payload []byte) error {
	return errors.New("dvc stub: not supported")
}

func (d *DVCConn) ReadMessage(timeoutMs uint32) ([]byte, error) {
	return nil, errors.New("dvc stub: not supported")
}

func OpenDVC(channelName string) (*DVCConn, error) {
	return nil, errors.New("RDP Dynamic Virtual Channels are only supported on Windows")
}

func GetCurrentSessionID() uint32 {
	return 0xFFFFFFFF
}

func LogSessionDiagnostics() {
	log.Println("[DVC DIAG] Non-Windows platform — DVC not available")
}
