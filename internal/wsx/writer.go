// Package wsx contains small WebSocket helpers shared by the daemon and the
// reference gateway.
package wsx

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Writer serializes writes to one WebSocket connection. coder/websocket allows
// concurrent writes, but serializing them keeps message ordering predictable
// and bounds each write with a timeout.
type Writer struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	timeout time.Duration
}

// New returns a writer for conn with a 10 second per-message timeout.
func New(conn *websocket.Conn) *Writer {
	return &Writer{conn: conn, timeout: 10 * time.Second}
}

// WriteJSON marshals v and writes it as one text frame.
func (w *Writer) WriteJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.Write(ctx, data)
}

// Write writes one text frame.
func (w *Writer) Write(ctx context.Context, data []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	return w.conn.Write(wctx, websocket.MessageText, data)
}
