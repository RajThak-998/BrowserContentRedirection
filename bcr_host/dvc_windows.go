//go:build windows

package main

import (
	"fmt"
	"io"
	"syscall"
	"unsafe"
)

var (
	wtsapi32                = syscall.NewLazyDLL("wtsapi32.dll")
	wtsVirtualChannelOpenEx = wtsapi32.NewProc("WTSVirtualChannelOpenEx")
	wtsVirtualChannelClose  = wtsapi32.NewProc("WTSVirtualChannelClose")

	kernel32  = syscall.NewLazyDLL("kernel32.dll")
	readFile  = kernel32.NewProc("ReadFile")
	writeFile = kernel32.NewProc("WriteFile")
)

const (
	WTS_CURRENT_SESSION        = 0xFFFFFFFF
	WTS_CHANNEL_OPTION_DYNAMIC = 0x00000001
)

type DVCConn struct {
	handle syscall.Handle
}

func OpenDVC(channelName string) (*DVCConn, error) {
	if err := wtsapi32.Load(); err != nil {
		return nil, fmt.Errorf("wtsapi32.dll not available: %w", err)
	}

	namePtr, err := syscall.BytePtrFromString(channelName)
	if err != nil {
		return nil, err
	}

	r1, _, err := wtsVirtualChannelOpenEx.Call(
		uintptr(WTS_CURRENT_SESSION),
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(WTS_CHANNEL_OPTION_DYNAMIC),
	)
	handle := syscall.Handle(r1)
	if handle == 0 || handle == syscall.InvalidHandle {
		return nil, fmt.Errorf("failed to open dynamic virtual channel (RDP session might not be active): %v", err)
	}

	return &DVCConn{handle: handle}, nil
}

func (d *DVCConn) Close() error {
	if d.handle != 0 && d.handle != syscall.InvalidHandle {
		_, _, _ = wtsVirtualChannelClose.Call(uintptr(d.handle))
		d.handle = syscall.InvalidHandle
	}
	return nil
}

func (d *DVCConn) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	var written uint32
	r1, _, err := writeFile.Call(
		uintptr(d.handle),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
		0,
	)
	if r1 == 0 {
		return 0, err
	}
	return int(written), nil
}

func (d *DVCConn) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	var read uint32
	r1, _, err := readFile.Call(
		uintptr(d.handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&read)),
		0,
	)
	if r1 == 0 {
		return 0, err
	}
	if read == 0 {
		return 0, io.EOF
	}
	return int(read), nil
}
