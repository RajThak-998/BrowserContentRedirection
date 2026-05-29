//go:build !windows

package main

import (
	"errors"
	"io"
)

type DVCConn struct{}

func (d *DVCConn) Close() error {
	return nil
}

func (d *DVCConn) Write(data []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func (d *DVCConn) Read(buf []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func OpenDVC(channelName string) (*DVCConn, error) {
	return nil, errors.New("RDP Dynamic Virtual Channels are only supported on Windows")
}
