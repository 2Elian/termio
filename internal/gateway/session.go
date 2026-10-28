package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"termio/internal/protocol"
	"termio/internal/wsx"
)

var errDeviceClosed = errors.New("device connection closed")

type rpcResult struct {
	msg *protocol.Message
	err error
}

type hub struct {
	mu      sync.RWMutex
	devices map[string]*deviceConn
}

func newHub() *hub {
	return &hub{devices: make(map[string]*deviceConn)}
}

func (h *hub) add(dc *deviceConn) *deviceConn {
	h.mu.Lock()
	old := h.devices[dc.id]
	h.devices[dc.id] = dc
	h.mu.Unlock()
	if old != nil && old != dc {
		old.close(websocket.StatusCode(protocol.CloseSuperseded), "superseded by a new connection")
	}
	return old
}

func (h *hub) remove(dc *deviceConn) {
	h.mu.Lock()
	if h.devices[dc.id] == dc {
		delete(h.devices, dc.id)
	}
	h.mu.Unlock()
}

func (h *hub) get(id string) (*deviceConn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	dc, ok := h.devices[id]
	return dc, ok
}

func (h *hub) snapshot() []*deviceConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*deviceConn, 0, len(h.devices))
	for _, dc := range h.devices {
		out = append(out, dc)
	}
	return out
}

type deviceSnapshot struct {
	ID                 string                   `json:"id"`
	Name               string                   `json:"name,omitempty"`
	OS                 string                   `json:"os"`
	Arch               string                   `json:"arch"`
	Version            string                   `json:"version"`
	ConnectedAt        time.Time                `json:"connected_at"`
	Tools              []protocol.ToolSpec      `json:"tools"`
	Workspaces         []protocol.WorkspaceInfo `json:"workspaces"`
	DefaultWorkspaceID string                   `json:"default_workspace_id,omitempty"`
	MaxInflight        int                      `json:"max_inflight"`
}

type deviceConn struct {
	id                 string
	conn               *websocket.Conn
	writer             *wsx.Writer
	log                *slog.Logger
	info               protocol.DeviceInfo
	tools              []protocol.ToolSpec
	workspaces         []protocol.WorkspaceInfo
	defaultWorkspaceID string
	maxInflight        int
	connectedAt        time.Time
	hub                *hub

	done      chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	pending map[string]chan rpcResult
}

func newDeviceConn(id string, conn *websocket.Conn, writer *wsx.Writer, log *slog.Logger, info protocol.DeviceInfo, tools []protocol.ToolSpec, workspaces []protocol.WorkspaceInfo, defaultWorkspaceID string, maxInflight int, h *hub) *deviceConn {
	if workspaces == nil {
		workspaces = []protocol.WorkspaceInfo{}
	}
	return &deviceConn{
		id:                 id,
		conn:               conn,
		writer:             writer,
		log:                log,
		info:               info,
		tools:              tools,
		workspaces:         workspaces,
		defaultWorkspaceID: defaultWorkspaceID,
		maxInflight:        maxInflight,
		connectedAt:        time.Now().UTC(),
		hub:                h,
		done:               make(chan struct{}),
		pending:            make(map[string]chan rpcResult),
	}
}

func (d *deviceConn) hasWorkspace(id string) bool {
	for _, ws := range d.workspaces {
		if ws.ID == id {
			return true
		}
	}
	return false
}

func (d *deviceConn) snapshot() deviceSnapshot {
	d.mu.Lock()
	tools := append([]protocol.ToolSpec(nil), d.tools...)
	maxInflight := d.maxInflight
	d.mu.Unlock()
	return deviceSnapshot{
		ID:                 d.id,
		Name:               d.info.Name,
		OS:                 d.info.OS,
		Arch:               d.info.Arch,
		Version:            d.info.Version,
		ConnectedAt:        d.connectedAt,
		Tools:              tools,
		Workspaces:         d.workspaces,
		DefaultWorkspaceID: d.defaultWorkspaceID,
		MaxInflight:        maxInflight,
	}
}

// refreshTools asks the daemon for its current tools/list and updates the
// cached snapshot. The reference gateway otherwise uses the registration-time
// list; this is the live path for the daemon's tools/list handler.
func (d *deviceConn) refreshTools(ctx context.Context) error {
	req, err := protocol.NewRequest(protocol.NewID("g_"), protocol.MethodToolsList, struct{}{})
	if err != nil {
		return err
	}
	resp, err := d.request(ctx, req)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("tools/list failed (%d): %s", resp.Error.Code, resp.Error.Message)
	}
	var result protocol.ToolsListResult
	if err := protocol.DecodeResult(resp, &result); err != nil {
		return err
	}
	d.mu.Lock()
	d.tools = append([]protocol.ToolSpec(nil), result.Tools...)
	d.mu.Unlock()
	return nil
}

func (d *deviceConn) request(ctx context.Context, msg *protocol.Message) (*protocol.Message, error) {
	ch := make(chan rpcResult, 1)

	d.mu.Lock()
	select {
	case <-d.done:
		d.mu.Unlock()
		return nil, errDeviceClosed
	default:
	}
	d.pending[msg.ID] = ch
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		if d.pending[msg.ID] == ch {
			delete(d.pending, msg.ID)
		}
		d.mu.Unlock()
	}()

	if err := d.writer.WriteJSON(ctx, msg); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.done:
		return nil, errDeviceClosed
	case res := <-ch:
		return res.msg, res.err
	}
}

func (d *deviceConn) readLoop() {
	for {
		typ, data, err := d.conn.Read(context.Background())
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			d.log.Warn("discarding malformed daemon frame", "device_id", d.id, "error", err)
			continue
		}
		if msg.JSONRPC != "2.0" {
			continue
		}

		switch {
		case msg.Method == protocol.MethodProgress:
			var progress protocol.ProgressParams
			if err := protocol.DecodeParams(&msg, &progress); err == nil {
				d.log.Debug("tool progress",
					"device_id", d.id,
					"call_id", progress.Meta.CallID,
					"progress", progress.Progress,
					"message", progress.Message)
			}
		case msg.Method != "" && msg.ID != "":
			// A daemon-initiated request is not part of v1; answer politely
			// without blocking the read loop.
			m := msg
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = d.writer.WriteJSON(ctx, protocol.NewErrorResponse(m.ID, protocol.CodeMethodNotFound, "unsupported daemon request: "+m.Method))
			}()
		case msg.Method != "":
			d.log.Debug("ignoring daemon notification", "device_id", d.id, "method", msg.Method)
		case msg.ID != "":
			d.deliver(&msg)
		}
	}
}

func (d *deviceConn) deliver(msg *protocol.Message) {
	d.mu.Lock()
	ch := d.pending[msg.ID]
	delete(d.pending, msg.ID)
	d.mu.Unlock()
	if ch != nil {
		select {
		case ch <- rpcResult{msg: msg}:
		default:
		}
	}
}

func (d *deviceConn) failPending(err error) {
	d.mu.Lock()
	pending := d.pending
	d.pending = make(map[string]chan rpcResult)
	d.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- rpcResult{err: err}:
		default:
		}
	}
}

func (d *deviceConn) close(code websocket.StatusCode, reason string) {
	d.closeOnce.Do(func() {
		close(d.done)
		d.failPending(errDeviceClosed)
		d.hub.remove(d)
		_ = d.conn.Close(code, reason)
	})
}

func (d *deviceConn) heartbeat(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := d.conn.Ping(ctx)
			cancel()
			if err != nil {
				d.log.Debug("gateway heartbeat failed", "device_id", d.id, "error", err)
				d.close(websocket.StatusGoingAway, "heartbeat failed")
				return
			}
		}
	}
}

func (d *deviceConn) notifyCancel(requestID, callID, workspaceID, sessionID, reason string) {
	msg, err := protocol.NewNotification(protocol.MethodCancelled, protocol.CancelledParams{
		RequestID: requestID,
		Reason:    reason,
		Meta: protocol.CallMeta{
			CallID:      callID,
			WorkspaceID: workspaceID,
			SessionID:   sessionID,
		},
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.writer.WriteJSON(ctx, msg); err != nil {
		d.log.Debug("write cancel failed", "device_id", d.id, "call_id", callID, "error", err)
	}
}
