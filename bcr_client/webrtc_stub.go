//go:build !linux || !webkit2_41

package main

// EnableWebRTC is a no-op on non-Linux or non-webkit2_41 builds.
// The actual implementation is in webrtc_linux.go (linux && webkit2_41).
func EnableWebRTC() {}
