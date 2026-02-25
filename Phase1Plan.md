# Phase 1: Video Telemetry & Position Tracking

## 1. Objective
Build a Chrome extension that detects video elements, tracks their position and state in real time, and emits structured telemetry to a local endpoint. Continuous monitoring must cover fullscreen, theater, scroll, and resize states. No redirection logic in this phase.

## 2. Scope
Included
- Video detection
- Position tracking (bounding box)
- Visibility tracking
- Fullscreen detection
- Playback state tracking
- Delta-based emission
- WebSocket transport (prototype)

Not Included
- DRM handling
- URL extraction
- Media redirection
- Windows RDP integration

## 3. Architecture Overview
Content Script → Background → Transport → Local App

Core modules (modular design):
- videoRegistry
- videoTracker
- stateManager
- observers (MutationObserver, ResizeObserver, IntersectionObserver)
- emitter
- transport

## 4. Video Detection Strategy
- Use document.querySelectorAll("video") on load
- MutationObserver to detect dynamic additions/removals
- Assign UUID to each video
- Support multiple videos concurrently

## 5. Continuous Monitoring Strategy
- ResizeObserver: watch element size changes
- IntersectionObserver: viewport visibility
- Throttled scroll listener: update bounding box without overload
- Fullscreen API: listen to fullscreenchange
- Playback events: play, pause, seeking, timeupdate, ratechange

## 6. State Management
- Maintain previous state per-video
- Compute delta on change
- Emit only when:
  - Position delta > 1px
  - Playback state changed
  - Fullscreen changed
  - Visibility changed
- Throttle position updates to max 30 FPS

## 7. Data Contract
All events follow:
```json
{
  "type": "VIDEO_UPDATE",
  "payload": {
    "id": "<uuid>",
    "timestamp": 0,
    "bounds": {"x":0,"y":0,"width":0,"height":0},
    "visibility": {"intersectionRatio":0,"inViewport":false},
    "playback": {"state":"paused|playing","currentTime":0,"rate":1},
    "fullscreen": false,
    "delta": {"dx":0,"dy":0,"dw":0,"dh":0}
  }
}
```
Future event types: VIDEO_ADDED, VIDEO_REMOVED, VIDEO_ERROR

## 8. Transport
Prototype: WebSocket (ws://localhost:PORT)  
Production: Native Messaging Host  
Transport API:
- connect()
- send(event)
- disconnect()

## 9. Local Endpoint Prototype
Minimal overlay app:
- Transparent, frameless, always-on-top window
- Moves/resizes based on telemetry
- Purpose: validate tracking accuracy and perf

## 10. Performance Constraints
- Max 30 updates/sec per video
- No constant polling
- Avoid layout thrashing and global reflows
- Efficient observer usage and throttling

## 11. Testing Plan
Test scenarios:
- YouTube normal, theater, fullscreen, mini player
- Scrolling pages
- Multiple videos in same tab
- Tab switching
- Rapid resize
- Stress with many videos

## 12. Definition of Done (Phase 1)
- Video detected automatically
- Real-time bounding box tracking
- Fullscreen transitions detected
- Visibility detection working
- Delta-based emission implemented
- Overlay window mimics position/sizing
- No CPU spikes during normal use


## Todo List
- convert all the function into classes -> prototypical Inheritance (javascript good parts)
- BCR_host.exe(golang)
- BCR_client.exe(golang)
- Client side first have to tracedown the video in real time and then intercept the actual 