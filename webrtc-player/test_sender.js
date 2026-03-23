const fs = require('fs');
const WebSocket = require('ws');

const ws = new WebSocket('ws://localhost:8081');
const CHUNK_SIZE = 8 * 1024; // 8KB chunks

ws.on('open', () => {
    console.log('Connected to Wails App via WebSocket. Sending video...');

    // Note: To test this, you must have a sample.webm file in this directory.
    // If not found, to simulate an extension we'll just send random binary data
    if (!fs.existsSync('sample.webm')) {
        console.log('sample.webm not found. Emitting dummy binary chunks to test connection...');
        setInterval(() => {
            const dummyBuf = Buffer.alloc(CHUNK_SIZE);
            ws.send(dummyBuf);
        }, 100);
        return;
    }

    const buffer = fs.readFileSync('sample.webm');
    let offset = 0;

    const sendChunk = setInterval(() => {
        if (offset >= buffer.length) {
            clearInterval(sendChunk);
            console.log('Finished streaming byte chunks.');
            return;
        }

        const chunk = buffer.slice(offset, offset + CHUNK_SIZE);
        ws.send(chunk);
        offset += CHUNK_SIZE;
    }, 10); // Send every 10ms
});

ws.on('error', (err) => {
    console.error('WebSocket Error:', err);
});
