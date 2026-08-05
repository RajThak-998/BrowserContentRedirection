package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const listenAddr = ":8765"

// defaultLogPath returns the log file location, relative to the working
// directory. It deliberately does NOT use os.Executable(): under `go run` that
// resolves to a throwaway binary in a temp build directory, so the log would
// land somewhere different on every run.
func defaultLogPath() string {
	return "bcr_host.log"
}

// initFileLogging mirrors bcr_client: route the standard log package to both
// stdout and a file truncated on every start, with microsecond timestamps so
// the bcr_host and bcr_client timelines can be lined up against each other.
// Standard error is redirected into the same file so that a panic is recorded
// rather than being lost with the terminal.
func initFileLogging(path string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("WARNING: could not open log file %q: %v (logging to stdout only)", path, err)
		return nil
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(os.Stdout, f))

	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		abs = path
	}
	log.Printf("[bcr_host] file logging started — %s (truncated each run)", abs)

	if err := redirectStderr(f); err != nil {
		log.Printf("[bcr_host] WARNING: could not redirect stderr to the log file: %v (panics will only appear in the terminal)", err)
	}
	return f
}

func main() {
	logPath := flag.String("log", defaultLogPath(), "Path to the bcr_host log file (truncated on each start)")
	flag.Parse()

	if f := initFileLogging(*logPath); f != nil {
		defer f.Close()
	}

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

	log.Printf("[DIAG] Transport: %s", transportSummary())

	// RDP session/elevation checks only mean something when the virtual channel
	// is in play. In local mode they are noise: logElevationStatus shells out to
	// `net session` (slow) and warns about mstsc.exe privileges that are
	// irrelevant when bcr_host and bcr_client share a machine.
	if runtime.GOOS == "windows" && isVDIMode() {
		// Log session ID and elevation status
		LogSessionDiagnostics()
		logElevationStatus()
	} else if isLocalMode() {
		log.Println("[DIAG] local mode — RDP session/elevation checks skipped")
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
