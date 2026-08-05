//go:build !windows

package main

import (
	"os"
	"syscall"
)

// redirectStderr points the process's standard error at f, so panics and other
// runtime output land in the log file instead of only the terminal.
//
// Dup2 replaces file descriptor 2 itself, which is where the Go runtime writes
// panics; os.Stderr is reassigned as well so Go code writing to it explicitly
// follows the same path.
func redirectStderr(f *os.File) error {
	if err := syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		return err
	}
	os.Stderr = f
	return nil
}
