package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"termio/internal/protocol"
	"termio/internal/tools"
	"termio/internal/wsx"
)

const (
	dedupeTTL        = 10 * time.Minute
	dedupeLimit      = 1024
	progressInterval = 250 * time.Millisecond
	maxToolTimeout   = 5 * time.Minute
	minToolTimeout   = time.Second
	fallbackTimeout  = 30 * time.Second
)

var (
	errCallCancelled = errors.New("call cancelled")
	errCallTimeout   = errors.New("call timed out")
	errCallDone      = errors.New("call finished")
	errShutdown      = errors.New("daemon shutting down")
)

type dispatcher struct {
	workspaces     *WorkspaceSet
	writer         *wsx.Writer
	log            *slog.Logger
	defaultTimeout time.Duration

	mu        sync.Mutex
	inflight  map[string]*callState
	rpcToCall map[string]string
	completed map[string]cachedResult
}

type callState struct {
	key         string
	workspaceID string
	sessionID   string
	callID      string
	cancel      context.CancelCauseFunc
}

type cachedResult struct {
	fingerprint string
	result      json.RawMessage
	expires     time.Time
}

type reserveStatus int

const (
	reserveStarted reserveStatus = iota
	reserveDuplicate
	reserveCached
	reserveMismatch
)

func newDispatcher(workspaces *WorkspaceSet, writer *wsx.Writer, log *slog.Logger, defaultTimeout time.Duration) *dispatcher {
	if defaultTimeout <= 0 {
		defaultTimeout = fallbackTimeout
	}
	return &dispatcher{
		workspaces:     workspaces,
		writer:         writer,
		log:            log,
		defaultTimeout: defaultTimeout,
		inflight:       make(map[string]*callState),
		rpcToCall:      make(map[string]string),
		completed:      make(map[string]cachedResult),
	}
}

func (d *dispatcher) handleCall(parent context.Context, msg *protocol.Message) {
	var params protocol.ToolCallParams
	if err := protocol.DecodeParams(msg, &params); err != nil {
		d.sendError(parent, msg.ID, protocol.CodeInvalidParams, err.Error())
		return
	}
	if params.Name == "" {
		d.sendError(parent, msg.ID, protocol.CodeInvalidParams, "tool name is required")
		return
	}
	callID := params.Meta.CallID
	if callID == "" {
		d.sendError(parent, msg.ID, protocol.CodeInvalidParams, "missing _meta termio/callId")
		return
	}
	if !validCallID(callID) {
		d.sendError(parent, msg.ID, protocol.CodeInvalidParams, "callId must be 1-128 printable characters without NUL")
		return
	}
	workspaceID := strings.TrimSpace(params.Meta.WorkspaceID)
	sessionID := strings.TrimSpace(params.Meta.SessionID)
	if !validSessionID(sessionID) {
		d.sendError(parent, msg.ID, protocol.CodeInvalidParams, "sessionId must be at most 256 printable characters")
		return
	}
	ws, err := d.workspaces.Get(workspaceID)
	if err != nil {
		requested := workspaceID
		if requested == "" {
			requested = d.workspaces.DefaultID()
		}
		d.sendError(parent, msg.ID, protocol.CodeUnknownWorkspace, fmt.Sprintf("unknown workspace %q", requested))
		return
	}
	workspaceID = ws.ID
	key := callKey(workspaceID, sessionID, callID)
	fingerprint := params.Name + "\x00" + string(params.Arguments)

	// Fast path: replay a completed call without consuming an execution slot.
	if replay, found, mismatch := d.lookupCache(key, fingerprint, msg.ID); found {
		if mismatch {
			d.sendError(parent, msg.ID, protocol.CodeInvalidParams, "callId was reused with different tool or arguments")
			return
		}
		d.send(parent, replay)
		return
	}

	select {
	case ws.Sem <- struct{}{}:
		defer func() { <-ws.Sem }()
	default:
		d.sendError(parent, msg.ID, protocol.CodeOverloaded, "workspace is busy")
		return
	}

	timeout := d.callTimeout(params.Meta.TimeoutMS)
	callCtx, cancel := context.WithCancelCause(parent)
	defer cancel(errCallDone)

	replay, status := d.reserve(key, workspaceID, sessionID, callID, fingerprint, msg.ID, cancel)
	switch status {
	case reserveDuplicate:
		d.sendError(parent, msg.ID, protocol.CodeDuplicateCall, "callId is already in flight")
		return
	case reserveMismatch:
		d.sendError(parent, msg.ID, protocol.CodeInvalidParams, "callId was reused with different tool or arguments")
		return
	case reserveCached:
		if replay != nil {
			replay.ID = msg.ID
			d.send(parent, replay)
		}
		return
	case reserveStarted:
	}
	defer d.release(key, msg.ID)

	timer := time.AfterFunc(timeout, func() { cancel(errCallTimeout) })
	defer timer.Stop()

	token := params.Meta.ProgressToken
	if token == "" {
		token = callID
	}
	progress := &progressSender{
		writer:      d.writer,
		log:         d.log,
		ctx:         callCtx,
		token:       token,
		callID:      callID,
		workspaceID: workspaceID,
		sessionID:   sessionID,
		minInterval: progressInterval,
	}

	var (
		result  protocol.CallToolResult
		callErr error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				d.log.Error("tool panicked", "tool", params.Name, "workspace", workspaceID, "panic", fmt.Sprint(r))
				result = toolErrorResult(fmt.Sprintf("internal tool error: %v", r))
				callErr = nil
			}
		}()
		result, callErr = ws.Registry.Call(callCtx, params.Name, params.Arguments, progress.send)
	}()

	cause := context.Cause(callCtx)
	if cause != nil {
		switch {
		case errors.Is(cause, errShutdown):
			d.log.Debug("call abandoned during shutdown", "call_id", callID, "workspace", workspaceID)
			return
		case errors.Is(cause, errCallCancelled):
			d.sendError(parent, msg.ID, protocol.CodeRequestCancelled, "call cancelled")
			return
		case errors.Is(cause, errCallTimeout):
			result = toolErrorResult(fmt.Sprintf("tool timed out after %s", timeout))
			callErr = nil
		case parent.Err() != nil:
			d.log.Debug("call abandoned because the session closed", "call_id", callID, "workspace", workspaceID)
			return
		}
	}

	if callErr != nil {
		var argErr *tools.ArgError
		var unknown *tools.UnknownToolError
		if errors.As(callErr, &argErr) || errors.As(callErr, &unknown) {
			d.sendError(parent, msg.ID, protocol.CodeInvalidParams, callErr.Error())
			return
		}
		result = toolErrorResult(callErr.Error())
	}

	result.Meta.CallID = callID
	result.Meta.WorkspaceID = workspaceID
	result.Meta.SessionID = sessionID
	resp, err := protocol.NewResult(msg.ID, result)
	if err != nil {
		d.sendError(parent, msg.ID, protocol.CodeInternalError, "encode result: "+err.Error())
		return
	}
	d.cachePut(key, fingerprint, resp.Result)
	d.send(parent, resp)
}

func callKey(workspaceID, sessionID, callID string) string {
	return workspaceID + "\x00" + sessionID + "\x00" + callID
}

// validCallID prevents NUL injection into the composite idempotency key.
// Workspace IDs are validated in config; session IDs are only checked here.
func validCallID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validSessionID(s string) bool {
	if len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (d *dispatcher) lookupCache(key, fingerprint, rpcID string) (*protocol.Message, bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cached, ok := d.completed[key]
	if !ok {
		return nil, false, false
	}
	if time.Now().After(cached.expires) {
		delete(d.completed, key)
		return nil, false, false
	}
	if cached.fingerprint != fingerprint {
		return nil, true, true
	}
	return &protocol.Message{JSONRPC: "2.0", ID: rpcID, Result: cached.result}, true, false
}

func (d *dispatcher) reserve(key, workspaceID, sessionID, callID, fingerprint, rpcID string, cancel context.CancelCauseFunc) (*protocol.Message, reserveStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cached, ok := d.completed[key]; ok {
		if time.Now().Before(cached.expires) {
			if cached.fingerprint != fingerprint {
				return nil, reserveMismatch
			}
			return &protocol.Message{JSONRPC: "2.0", ID: rpcID, Result: cached.result}, reserveCached
		}
		delete(d.completed, key)
	}
	if _, ok := d.inflight[key]; ok {
		return nil, reserveDuplicate
	}
	d.inflight[key] = &callState{
		key:         key,
		workspaceID: workspaceID,
		sessionID:   sessionID,
		callID:      callID,
		cancel:      cancel,
	}
	d.rpcToCall[rpcID] = key
	return nil, reserveStarted
}

func (d *dispatcher) release(key, rpcID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inflight, key)
	if d.rpcToCall[rpcID] == key {
		delete(d.rpcToCall, rpcID)
	}
}

func (d *dispatcher) cachePut(key, fingerprint string, result json.RawMessage) {
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.completed) >= dedupeLimit {
		for k, cached := range d.completed {
			if now.After(cached.expires) {
				delete(d.completed, k)
			}
		}
		for len(d.completed) >= dedupeLimit {
			for k := range d.completed {
				delete(d.completed, k)
			}
		}
	}
	d.completed[key] = cachedResult{
		fingerprint: fingerprint,
		result:      append(json.RawMessage(nil), result...),
		expires:     now.Add(dedupeTTL),
	}
}

func (d *dispatcher) cancelCall(params protocol.CancelledParams) {
	d.mu.Lock()
	key := ""
	if params.RequestID != "" {
		key = d.rpcToCall[params.RequestID]
	}
	if key == "" {
		workspaceID := strings.TrimSpace(params.Meta.WorkspaceID)
		if workspaceID == "" {
			workspaceID = d.workspaces.DefaultID()
		}
		key = callKey(workspaceID, params.Meta.SessionID, params.Meta.CallID)
	}
	state := d.inflight[key]
	d.mu.Unlock()
	if state != nil {
		state.cancel(errCallCancelled)
	}
}

func (d *dispatcher) shutdown() {
	d.mu.Lock()
	states := make([]*callState, 0, len(d.inflight))
	for _, state := range d.inflight {
		states = append(states, state)
	}
	d.mu.Unlock()
	for _, state := range states {
		state.cancel(errShutdown)
	}
}

func (d *dispatcher) callTimeout(requestedMS int) time.Duration {
	timeout := d.defaultTimeout
	if requestedMS > 0 {
		requested := time.Duration(requestedMS) * time.Millisecond
		if requested < minToolTimeout {
			requested = minToolTimeout
		}
		if requested < timeout {
			timeout = requested
		}
	}
	if timeout > maxToolTimeout {
		timeout = maxToolTimeout
	}
	if timeout <= 0 {
		timeout = fallbackTimeout
	}
	return timeout
}

func (d *dispatcher) send(ctx context.Context, msg *protocol.Message) {
	if msg == nil || msg.ID == "" {
		return
	}
	if err := d.writer.WriteJSON(ctx, msg); err != nil {
		d.log.Debug("write response failed", "error", err)
	}
}

func (d *dispatcher) sendError(ctx context.Context, id string, code int, message string) {
	if id == "" {
		return
	}
	d.send(ctx, protocol.NewErrorResponse(id, code, message))
}

func toolErrorResult(text string) protocol.CallToolResult {
	return protocol.CallToolResult{
		Content: []protocol.ContentBlock{{Type: "text", Text: text}},
		IsError: true,
	}
}

type progressSender struct {
	writer      *wsx.Writer
	log         *slog.Logger
	ctx         context.Context
	token       string
	callID      string
	workspaceID string
	sessionID   string
	minInterval time.Duration

	mu   sync.Mutex
	last time.Time
}

func (p *progressSender) send(progress, total float64, message string) {
	if p == nil || p.token == "" {
		return
	}
	now := time.Now()
	p.mu.Lock()
	if !p.last.IsZero() && now.Sub(p.last) < p.minInterval {
		p.mu.Unlock()
		return
	}
	p.last = now
	p.mu.Unlock()

	msg, err := protocol.NewNotification(protocol.MethodProgress, protocol.ProgressParams{
		ProgressToken: p.token,
		Progress:      progress,
		Total:         total,
		Message:       message,
		Meta: protocol.CallMeta{
			CallID:      p.callID,
			WorkspaceID: p.workspaceID,
			SessionID:   p.sessionID,
		},
	})
	if err != nil {
		return
	}
	if err := p.writer.WriteJSON(p.ctx, msg); err != nil {
		p.log.Debug("write progress failed", "call_id", p.callID, "error", err)
	}
}
