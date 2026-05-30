package main

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const listenAddr = ":8765"

func main() {
	log.Println("┌─────────────────────────────────────────┐")
	log.Println("│           bcr_host starting             │")
	log.Println("│  WebSocket endpoint: ws://localhost:8765/ws  │")
	log.Println("│  Roles: ?role=extension | ?role=client  │")
	log.Println("└─────────────────────────────────────────┘")

	// ── Startup Diagnostics ──────────────────────────────────────────────
	logStartupDiagnostics()

	registry := NewRegistry()
	server := NewServer(registry)

	http.HandleFunc("/ws", server.HandleWS)

	log.Printf("[main] listening on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("[main] server error: %v", err)
	}
}

func logStartupDiagnostics() {
	log.Println("[DIAG] ─── Startup Diagnostics ───")
	log.Printf("[DIAG] Go version: %s", runtime.Version())
	log.Printf("[DIAG] OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH)
	log.Printf("[DIAG] PID: %d", os.Getpid())

	if runtime.GOOS == "windows" {
		// Log session ID and elevation status
		LogSessionDiagnostics()
		logElevationStatus()
	}

	log.Println("[DIAG] ─── End Diagnostics ───")
}

func logElevationStatus() {
	// Quick check: try to open a file that only admins can access
	// A more reliable check is to use IsUserAnAdmin() from shell32.dll,
	// but this is simpler and works for our diagnostic purposes.
	cmd := exec.Command("net", "session")
	err := cmd.Run()
	if err == nil {
		log.Println("[DIAG] ⚠️  Process is running ELEVATED (Administrator)")
		log.Println("[DIAG]    If mstsc.exe is running as standard user, DVC will fail!")
		log.Println("[DIAG]    Run bcr_host from a non-elevated terminal.")
	} else {
		log.Println("[DIAG] ✅ Process is running as standard user (non-elevated)")
	}

	// Check BCR_SESSION_ID override
	if envSession := os.Getenv("BCR_SESSION_ID"); envSession != "" {
		log.Printf("[DIAG] BCR_SESSION_ID override set: %s", envSession)
	}

	// Log username and computername for context
	if user := os.Getenv("USERNAME"); user != "" {
		log.Printf("[DIAG] User: %s", user)
	}
	if comp := os.Getenv("COMPUTERNAME"); comp != "" {
		log.Printf("[DIAG] Computer: %s", comp)
	}

	// Check if this looks like an RDS session
	if sessionName := os.Getenv("SESSIONNAME"); sessionName != "" {
		log.Printf("[DIAG] SESSIONNAME: %s", sessionName)
		if strings.HasPrefix(sessionName, "RDP-Tcp") {
			log.Println("[DIAG] ✅ Running inside an RDP session")
		} else if sessionName == "Console" {
			log.Println("[DIAG] ⚠️  Running on console session (not RDP)")
		}
	}
}
