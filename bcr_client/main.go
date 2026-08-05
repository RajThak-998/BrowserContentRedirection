package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// fileLogger implements the Wails logger.Logger interface so that frontend logs
// (runtime.LogInfo/LogError → the [MSE]/[Video]/[Loopback] lines) are written into
// bcr_client.log, not just stdout. Without a Logger set on options.App, Wails routes
// those frontend logs to stdout only and the file misses all of them.
type fileLogger struct{}

func (fileLogger) write(level, message string) {
	line := fmt.Sprintf("%s %s | %s", time.Now().Format("2006/01/02 15:04:05.000000"), level, message)
	fmt.Println(line)
	logFileMu.Lock()
	if logFile != nil {
		fmt.Fprintln(logFile, line)
	}
	logFileMu.Unlock()
}

func (l fileLogger) Print(m string)   { l.write("PRT", m) }
func (l fileLogger) Trace(m string)   { l.write("TRC", m) }
func (l fileLogger) Debug(m string)   { l.write("DBG", m) }
func (l fileLogger) Info(m string)    { l.write("INF", m) }
func (l fileLogger) Warning(m string) { l.write("WRN", m) }
func (l fileLogger) Error(m string)   { l.write("ERR", m) }
func (l fileLogger) Fatal(m string)   { l.write("FTL", m) }

// defaultLogPath returns "bcr_client.log" resolved against the current working
// directory. CWD is used rather than the executable directory because `wails dev`
// builds to a transient location, whereas CWD is the stable project directory.
func defaultLogPath() string {
	return "bcr_client.log"
}

var (
	logFileMu sync.Mutex
	logFile   *os.File
)

// initFileLogging routes the standard log package to both stdout and a file that
// is truncated on every start (only the latest run is preserved). Microsecond
// timestamps are enabled so the ICE candidate-gathering timeline can be measured
// precisely. Returns the open file so the caller can close it on exit.
func initFileLogging(path string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("WARNING: could not open log file %q: %v (logging to stdout only)", path, err)
		return nil
	}

	logFileMu.Lock()
	logFile = f
	logFileMu.Unlock()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(os.Stdout, f))

	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		abs = path
	}
	log.Printf("[bcr_client] file logging started — %s (truncated each run)", abs)

	// Route stderr into the same file. The log package only covers log.* calls;
	// Go runtime panics are written straight to fd 2 and would otherwise be lost
	// with the terminal — which is exactly what happened when bcr_client
	// disappeared mid-call and left no trace in this file.
	if err := redirectStderr(f); err != nil {
		log.Printf("[bcr_client] WARNING: could not redirect stderr to the log file: %v (panics will only appear in the terminal)", err)
	}
	return f
}

// verboseLog writes high-volume diagnostics (full SDP dumps, etc.) to the log
// file ONLY, so the terminal stays readable while the file keeps full detail.
func verboseLog(msg string) {
	logFileMu.Lock()
	defer logFileMu.Unlock()
	if logFile == nil {
		return
	}
	fmt.Fprintf(logFile, "%s %s\n", time.Now().Format("2006/01/02 15:04:05.000000"), msg)
}

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Bypass WebView2 autoplay restrictions to allow unmuted autoplay
	os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--autoplay-policy=no-user-gesture-required")

	// Parse command-line arguments for window size
	width := flag.Int("w", 1280, "Window width")
	height := flag.Int("h", 720, "Window height")
	logPath := flag.String("log", defaultLogPath(), "Path to the bcr_client log file (truncated on each start)")
	flag.Parse()

	// Route all std-log diagnostics to a truncate-on-start file (and stdout).
	if f := initFileLogging(*logPath); f != nil {
		defer f.Close()
	}

	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title:       "WebRTC Player",
		Width:       *width,
		Height:      *height,
		Frameless:   true,
		AlwaysOnTop: true,
		StartHidden: true,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup: app.startup,
		// Route frontend logs (runtime.LogInfo/LogError) into bcr_client.log too.
		Logger:   fileLogger{},
		LogLevel: logger.INFO,
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			DisableWindowIcon:    true,
			// Naming the window class lets placer.go find this window's HWND
			// deterministically (EnumWindows filtered by our PID + this class).
			// Wails v2 exposes no HWND of its own. Must stay in sync with
			// overlayWindowClass in winplace_windows.go.
			WindowClassName: overlayWindowClass,
		},
	})

	if err != nil {
		log.Fatal(err)
	}
}
