const Transport = (()=> {
    const WS_URL = "ws://localhost:8765";
    const MAX_BACKOFF_MS = 5000;
    const INITIAL_BACKOFF_MS = 100;

    let _socket = null;
    let _retryCount = 0;
    let _retryTimer = null;
    let _intentionalClose = false;
    let _pendingQueue = [];


    function connect() {
        if(_socket && _socket.readyState === WebSocket.OPEN) {
            console.log("[Transport] Already connected.");
            return;
        }

        _intentionalClose = false;
        _createSocket();
    }

    function disconnect() {
        _intentionalClose = true;
        if(_retryTimer) {
            clearTimeout(_retryTimer);
            _retryTimer = null;
        }

        if(_socket) {
            _socket.close();
            _socket = null;
        }

        console.log("[Transport] Disconnected intentionally.");
    }

    /**
   * Send a structured event object over the WebSocket.
   * If not connected, queues the message and attempts reconnect.
   *
   * @param {object} event - Structured telemetry event
   */

    function send(event) {
        if(_socket && _socket.readyState === WebSocket.OPEN) {
            try {
                _socket.send(JSON.stringify(event));
            } catch (err) {
                console.warn("[Transport] Send failed:", err);
                _pendingQueue.push(event);
            }
        } else {
            _pendingQueue.push(event);

            if(!_retryTimer && !_intentionalClose) {
                console.warn("[Transport] Not connected. Queuing message and reconnecting...");
                _scheduleReconnect();
            }
        }
    }

    function _createSocket() {
        console.log(`[Transport] Connecting to ${WS_URL}...`);

        try {
            _socket = new WebSocket(WS_URL);
        } catch (err) {
            console.error("[Transport] WebSocket construction failed:", err);
            _scheduleReconnect();
            return;
        }

        _socket.onopen = _onOpen;
        _socket.onclose = _onClose;
        _socket.onerror = _onError;
        _socket.onmessage = _onMessage;
    }

    function _onOpen() {
        console.log("[Transport] Connected.");
        _retryCount = 0;

        if(_pendingQueue.length > 0) {
            console.log(`[Transport] Flushing ${_pendingQueue.length} queued messages.`);
            const queue = [..._pendingQueue];
            _pendingQueue = [];
            queue.forEach((event) => send(event));
        }
    }

    function _onClose(event) {
        console.warn(`[Transport] Connection closed. Code: ${event.code}`);
        if(!_intentionalClose) {
            _scheduleReconnect();
        }
    }

    function _onError(error) {
        console.error("[Transport] WebSocket error:", error);
    }

    function _onMessage(event) {
        console.log("[Transport] Received from endpoint:", event.data);
    }

    function _scheduleReconnect() {
        if(_retryTimer) return;
        
        const backoff = Math.min(
            INITIAL_BACKOFF_MS*Math.pow(2, _retryCount),
            MAX_BACKOFF_MS
        );

        console.log(`[Transport] Reconnecting in ${backoff}ms (attempt ${_retryCount+1})...`);

        _retryTimer = setTimeout(()=>{
            _retryTimer = null;
            _retryCount++;
            _createSocket();
        }, backoff);
    }

    /**
   * Returns current connection state as a readable string.
   * Useful for background.js health checks.
   *
   * @returns {"CONNECTING" | "OPEN" | "CLOSING" | "CLOSED" | "NO_SOCKET"}
   */

    function getStatus() {
        if(!_socket) return "NO_SOCKET";
        return ["CONNECTING", "OPEN", "CLOSING", "CLOSED"][_socket.readyState];
    }

    return { connect, disconnect, send, getStatus};
})();