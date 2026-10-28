// Package daemon implements the local Termio connector. It dials out to the
// gateway, registers the device, and executes forwarded tool calls.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/coder/websocket"

	"termio/internal/config"
	"termio/internal/protocol"
	"termio/internal/version"
	"termio/internal/wsx"
)

const (
	readLimit          = 4 << 20
	dialTimeout        = 15 * time.Second
	registerTimeout    = 10 * time.Second
	defaultHeartbeat   = 20 * time.Second
	minHeartbeat       = 5 * time.Second
	maxHeartbeat       = 60 * time.Second
	defaultBackoffMin  = time.Second
	defaultBackoffMax  = 30 * time.Second
	supersededCooldown = 5 * time.Minute
)

// DialFunc matches websocket.Dial and is replaceable in tests.
type DialFunc func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)

// Daemon is the local connector.
type Daemon struct {
	cfg        *config.Config
	log        *slog.Logger
	workspaces *WorkspaceSet
	dial       DialFunc
}

// New builds a daemon. The workspace set must be non-nil.
func New(cfg *config.Config, log *slog.Logger, workspaces *WorkspaceSet) *Daemon {
	if log == nil {
		log = slog.Default()
	}
	return &Daemon{cfg: cfg, log: log, workspaces: workspaces, dial: websocket.Dial}
}

// Run maintains the connection until ctx is canceled. It never returns a
// connection error: reconnecting is the normal operating mode.
func (d *Daemon) Run(ctx context.Context) error {
	bo := &backoff{min: defaultBackoffMin, max: defaultBackoffMax}
	for {
		if ctx.Err() != nil {
			return nil
		}
		registered, err := d.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if registered {
			bo.reset()
		}

		var superseded *supersededError
		delay := bo.next()
		if errors.As(err, &superseded) {
			bo.reset()
			delay = supersededCooldown + jitter(30*time.Second)
		}
		if err != nil {
			d.log.Warn("connection ended",
				"error", err,
				"device_id", d.cfg.DeviceID,
				"retry_in", delay.String())
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// runOnce returns registered=true once the gateway accepted this session, so
// the reconnect backoff can reset even if the connection later drops.
func (d *Daemon) runOnce(ctx context.Context) (bool, error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	defer cancelDial()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+d.cfg.Token)
	header.Set("X-Termio-Device", d.cfg.DeviceID)
	header.Set("User-Agent", fmt.Sprintf("termio/%s (%s/%s)", version.Version, runtime.GOOS, runtime.GOARCH))

	conn, resp, err := d.dial(dialCtx, d.cfg.GatewayURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("dial gateway: %w (HTTP %d)", err, resp.StatusCode)
		}
		return false, fmt.Errorf("dial gateway: %w", err)
	}
	defer func() {
		if ctx.Err() != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "daemon shutting down")
			return
		}
		conn.CloseNow()
	}()
	conn.SetReadLimit(readLimit)
	writer := wsx.New(conn)

	heartbeat, err := d.register(ctx, conn, writer)
	if err != nil {
		return false, err
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()
	go d.heartbeat(sessionCtx, conn, heartbeat, sessionCancel)

	disp := newDispatcher(d.workspaces, writer, d.log, time.Duration(d.cfg.CallTimeoutMS)*time.Millisecond)
	defer disp.shutdown()

	err = d.readLoop(sessionCtx, conn, writer, disp)
	if err != nil {
		if websocket.CloseStatus(err) == websocket.StatusCode(protocol.CloseSuperseded) {
			return true, &supersededError{}
		}
		return true, err
	}
	return true, nil
}

func (d *Daemon) register(ctx context.Context, conn *websocket.Conn, writer *wsx.Writer) (time.Duration, error) {
	regID := protocol.NewID("reg_")
	msg, err := protocol.NewRequest(regID, protocol.MethodRegister, protocol.RegisterParams{
		ProtocolVersion: protocol.ProtocolVersion,
		MCPVersion:      protocol.MCPVersion,
		Device: protocol.DeviceInfo{
			ID:      d.cfg.DeviceID,
			Name:    d.cfg.DeviceName,
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
			Version: version.Version,
		},
		Tools:              d.workspaces.Specs(),
		Workspaces:         d.workspaces.List(),
		DefaultWorkspaceID: d.workspaces.DefaultID(),
		MaxInflight:        d.cfg.MaxConcurrentCalls,
	})
	if err != nil {
		return 0, fmt.Errorf("build register request: %w", err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, registerTimeout)
	err = writer.WriteJSON(writeCtx, msg)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("send register: %w", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, registerTimeout)
	typ, data, err := conn.Read(readCtx)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("read register response: %w", err)
	}
	if typ != websocket.MessageText {
		return 0, fmt.Errorf("register response was not text")
	}
	var resp protocol.Message
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("parse register response: %w", err)
	}
	if resp.ID != regID {
		return 0, fmt.Errorf("unexpected register response id %q", resp.ID)
	}
	if resp.Error != nil {
		if resp.Error.Code == protocol.CodeUnsupportedProto {
			return 0, fmt.Errorf("gateway rejected protocol version %d: %s", protocol.ProtocolVersion, resp.Error.Message)
		}
		return 0, fmt.Errorf("gateway rejected registration (%d): %s", resp.Error.Code, resp.Error.Message)
	}
	var result protocol.RegisterResult
	if err := protocol.DecodeResult(&resp, &result); err != nil {
		return 0, fmt.Errorf("decode register result: %w", err)
	}
	if result.ProtocolVersion != protocol.ProtocolVersion {
		return 0, fmt.Errorf("gateway protocol version %d is incompatible with %d", result.ProtocolVersion, protocol.ProtocolVersion)
	}
	heartbeat := time.Duration(result.HeartbeatIntervalMS) * time.Millisecond
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeat
	}
	if heartbeat < minHeartbeat {
		heartbeat = minHeartbeat
	}
	if heartbeat > maxHeartbeat {
		heartbeat = maxHeartbeat
	}
	d.log.Info("registered with gateway",
		"device_id", d.cfg.DeviceID,
		"tools", len(d.workspaces.Specs()),
		"workspaces", len(d.workspaces.List()),
		"heartbeat", heartbeat.String())
	return heartbeat, nil
}

func (d *Daemon) heartbeat(ctx context.Context, conn *websocket.Conn, interval time.Duration, cancel context.CancelFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, done := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			done()
			if err != nil {
				d.log.Warn("heartbeat failed; reconnecting", "error", err)
				cancel()
				conn.CloseNow()
				return
			}
		}
	}
}

func (d *Daemon) readLoop(ctx context.Context, conn *websocket.Conn, writer *wsx.Writer, disp *dispatcher) error {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			d.log.Warn("discarding malformed frame", "error", err)
			continue
		}
		if msg.JSONRPC != "2.0" {
			d.log.Warn("discarding frame with unsupported jsonrpc version", "jsonrpc", msg.JSONRPC)
			continue
		}
		if len(msg.ID) > 256 {
			d.log.Warn("discarding frame with an oversized id")
			continue
		}

		switch {
		case msg.Method == protocol.MethodToolsCall && msg.ID != "":
			go disp.handleCall(ctx, &msg)
		case msg.Method == protocol.MethodToolsList && msg.ID != "":
			go d.handleToolsList(ctx, writer, &msg)
		case msg.Method == protocol.MethodCancelled:
			var params protocol.CancelledParams
			if err := protocol.DecodeParams(&msg, &params); err != nil {
				d.log.Warn("invalid cancel notification", "error", err)
				continue
			}
			disp.cancelCall(params)
		case msg.Method != "" && msg.ID != "":
			resp := protocol.NewErrorResponse(msg.ID, protocol.CodeMethodNotFound, "method not found: "+msg.Method)
			if err := writer.WriteJSON(ctx, resp); err != nil {
				d.log.Debug("write method-not-found response failed", "error", err)
			}
		case msg.ID != "":
			d.log.Debug("ignoring unexpected response", "id", msg.ID)
		default:
			d.log.Debug("ignoring unexpected notification", "method", msg.Method)
		}
	}
}

func (d *Daemon) handleToolsList(ctx context.Context, writer *wsx.Writer, msg *protocol.Message) {
	resp, err := protocol.NewResult(msg.ID, protocol.ToolsListResult{Tools: d.workspaces.Specs()})
	if err != nil {
		resp = protocol.NewErrorResponse(msg.ID, protocol.CodeInternalError, err.Error())
	}
	if err := writer.WriteJSON(ctx, resp); err != nil {
		d.log.Debug("write tools/list response failed", "error", err)
	}
}
