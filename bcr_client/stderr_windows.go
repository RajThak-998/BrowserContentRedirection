//go:build windows

package main

import (
	"os"
	"syscall"
)

// stdErrorHandle is STD_ERROR_HANDLE from the Win32 API.
const stdErrorHandle = ^uintptr(11) // -12

// redirectStderr points the process's standard error at f, so panics and other
// runtime output land in the log file instead of only the terminal.
//
// Setting os.Stderr alone is not enough: the Go runtime writes panics straight
// to file descriptor 2, and on Windows it resolves that by calling GetStdHandle
// on every write. Replacing the process-wide STD_ERROR_HANDLE is therefore what
// actually redirects a panic; os.Stderr is reassigned as well so that Go code
// writing to it explicitly follows the same path.
//
// syscall.SetStdHandle is not exported, so the call is made through kernel32
// directly — this adds no dependency.
func redirectStderr(f *os.File) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setStdHandle := kernel32.NewProc("SetStdHandle")

	if r, _, err := setStdHandle.Call(stdErrorHandle, f.Fd()); r == 0 {
		return err
	}
	os.Stderr = f
	return nil
}
