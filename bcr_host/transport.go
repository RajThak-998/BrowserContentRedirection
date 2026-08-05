package main

// transport.go — selects how bcr_host reaches bcr_client.
//
// There are exactly two deployments, and they are chosen explicitly. There is
// deliberately no probe-and-fallback mode: an implicit fallback made a real DVC
// failure in the VDI look like a signalling timeout, because bcr_host would
// quietly dial 127.0.0.1:8081 *inside the virtual desktop*, where bcr_client
// does not exist.
//
//	BCR_TRANSPORT=local  (default on this branch)
//	    Everything on one machine. bcr_host dials bcr_client's TCP listener
//	    directly. No virtual channel, no mstsc, no plugin.
//
//	    extension → ws://localhost:8765 → bcr_host → TCP 127.0.0.1:8081 → bcr_client
//
//	BCR_TRANSPORT=vdi
//	    bcr_host runs in the virtual desktop, bcr_client on the thin client.
//	    Signalling rides the RDP Dynamic Virtual Channel "BCR_VC"; the
//	    bcr_dvc_plugin.dll loaded by mstsc.exe bridges it to TCP 8081 on the
//	    thin client.
//
//	    extension → ws://localhost:8765 → bcr_host → DVC "BCR_VC" → plugin → bcr_client

import (
	"os"
	"strings"
)

const (
	// transportLocal dials bcr_client over TCP on this machine.
	transportLocal = "local"
	// transportVDI requires the RDP Dynamic Virtual Channel.
	transportVDI = "vdi"

	// dvcChannelName must match CHANNEL_NAME in dvc_plugin.cpp.
	dvcChannelName = "BCR_VC"

	defaultClientAddr = "127.0.0.1:8081"
)

// transportMode reports the configured transport: "local" or "vdi".
//
// Defaults to local — this is the local-dev branch, and a wrong default that
// fails immediately and loudly is better than one that half-works. The mode is
// logged at startup and on every bridge attempt, so it is never ambiguous which
// one is in force.
func transportMode() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BCR_TRANSPORT")), transportVDI) {
		return transportVDI
	}
	return transportLocal
}

// isLocalMode reports whether bcr_host and bcr_client share a machine.
func isLocalMode() bool {
	return transportMode() == transportLocal
}

// isVDIMode reports whether signalling rides the RDP virtual channel.
func isVDIMode() bool {
	return transportMode() == transportVDI
}

// clientAddr returns the address of bcr_client's TCP signalling listener, used
// in local mode. Override with BCR_CLIENT_ADDR — e.g. to put bcr_client on a
// second machine on the LAN without involving RDP at all.
func clientAddr() string {
	if addr := strings.TrimSpace(os.Getenv("BCR_CLIENT_ADDR")); addr != "" {
		return addr
	}
	return defaultClientAddr
}

// transportSummary is a one-line description of the active transport for logs.
func transportSummary() string {
	if isVDIMode() {
		return "vdi — RDP Dynamic Virtual Channel " + dvcChannelName
	}
	return "local — TCP " + clientAddr()
}
