package main

import (
	"fmt"
	"time"
)

// formatTime converts a Unix millisecond timestamp to a readable
// UTC string matching the Node.js prototype format:
// "2026-02-27 10:14:04.000"
func formatTime(ms int64) string {
	t := time.UnixMilli(ms).UTC()
	return t.Format("2006-01-02 15:04:05.000")
}

// fn formats a float64 to 2 decimal places — matches Node.js _n().
func fn(f float64) string {
	return fmt.Sprintf("%.2f", f)
}

// LogAdded prints a structured VIDEO_ADDED log block.
func LogAdded(clientID string, p AddedPayload, m Meta) {
	fmt.Println()
	fmt.Println("┌─ VIDEO_ADDED ─────────────────────────────")
	fmt.Printf("│  Client   : %s\n", clientID)
	fmt.Printf("│  Video ID : %s\n", p.ID)
	fmt.Printf("│  Tab      : %s\n", tabURL(m))
	fmt.Printf("│  Time     : %s\n", formatTime(p.Timestamp))
	fmt.Println("└────────────────────────────────────────────")
}

// LogRemoved prints a structured VIDEO_REMOVED log block.
func LogRemoved(clientID string, p RemovedPayload, m Meta) {
	fmt.Println()
	fmt.Println("┌─ VIDEO_REMOVED ────────────────────────────")
	fmt.Printf("│  Client   : %s\n", clientID)
	fmt.Printf("│  Video ID : %s\n", p.ID)
	fmt.Printf("│  Tab      : %s\n", tabURL(m))
	fmt.Printf("│  Time     : %s\n", formatTime(p.Timestamp))
	fmt.Println("└────────────────────────────────────────────")
}

// LogUpdate prints a structured VIDEO_UPDATE log block.
func LogUpdate(clientID string, p UpdatePayload, m Meta) {
	fmt.Println()
	fmt.Println("┌─ VIDEO_UPDATE ─────────────────────────────")
	fmt.Printf("│  Client     : %s\n", clientID)
	fmt.Printf("│  Video ID   : %s\n", p.ID)
	fmt.Printf("│  Time       : %s\n", formatTime(p.Timestamp))
	fmt.Printf("│  Tab        : %s\n", tabURL(m))
	fmt.Println("│  ── Position ──────────────────────────────")
	fmt.Printf("│  Bounds     : x=%s y=%s w=%s h=%s\n",
		fn(p.Bounds.X), fn(p.Bounds.Y), fn(p.Bounds.Width), fn(p.Bounds.Height))
	fmt.Printf("│  Delta      : dx=%s dy=%s dw=%s dh=%s\n",
		fn(p.Delta.DX), fn(p.Delta.DY), fn(p.Delta.DW), fn(p.Delta.DH))
	fmt.Println("│  ── Visibility ────────────────────────────")
	fmt.Printf("│  In Viewport: %v\n", p.Visibility.InViewport)
	fmt.Printf("│  Ratio      : %.1f%%\n", p.Visibility.IntersectionRatio*100)
	fmt.Println("│  ── Playback ──────────────────────────────")
	fmt.Printf("│  State      : %s\n", p.Playback.State)
	fmt.Printf("│  Time       : %.2fs\n", p.Playback.CurrentTime)
	fmt.Printf("│  Rate       : %gx\n", p.Playback.Rate)
	fmt.Println("│  ── Fullscreen ────────────────────────────")
	fmt.Printf("│  Active     : %v\n", p.Fullscreen)
	fmt.Println("└────────────────────────────────────────────")
}

// tabURL safely extracts the tab URL from Meta,
// returning "unknown" if Meta was absent in the packet.
func tabURL(m Meta) string {
	if m.TabURL == "" {
		return "unknown"
	}
	return m.TabURL
}
