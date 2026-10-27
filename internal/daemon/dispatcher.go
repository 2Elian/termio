package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"termio/internal/protocol"
	"termio/internal/tools"
	"termio/internal/wsx"
)

const (
	dedupeTTL         = 10 * time.Minute
	dedupeLimit       = 1024
	progressInterval  = 250 * time.Millisecond
	maxToolTimeout    = 5 * time.Minute
	minToolTimeout    = time.Second
	fallbackTimeout   = 30 * time.Second
	maxInFlightPerDev = 32
)

var (
	errCallCancelled = errors.New("call cancelled")
	errCallTimeout   = errors.New("call timed out")
	errCallDone      = errors.New("call finished")
	errShutdown      = errors.New("daemon shutting down")
)

type dispatcher struct {
	registry       *tools.Registry
	writer         *wsx.Writer
	log            *slog.Logger
	defaultTimeout time.Duration
	sem            chan struct{}

	mu        sync.Mutex
	inflight  map[string]*callState
	rpcToCall map[string]string
	completed map[string]cachedResult
}

type callState struct {
	callID string
	cancel context.CancelCauseFunc
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

func newDispatcher(registry *tools.Registry, writer *wsx.Writer, log *slog.Logger, maxConcurrent int, defaultTimeout time.Duration) *dispatcher {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if maxConcurrent > maxInFlightPerDev {
		maxConcurrent = maxInFlightPerDev
	}
	if defaultTimeout <= 0 {
		defaultTimeout = fallbackTimeout
	}
	return &dispatcher{
		registry:       registry,
		writer:         writer,
		log:            log,
		defaultTimeout: defaultTimeout,
		sem:            make(chan struct{}, maxConcurrent),
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
	if len(callID) > 128 {
		d.sendError(parent, msg.ID, protocol.CodeInvalidParams, "callId is too long")
		return
	}
	fingerprint := params.Name + "\x00" + string(params.Arguments)

	// Fast path: replay a completed call without consuming an execution slot.
	if replay, found, mismatch := d.lookupCache(callID, fingerprint, msg.ID); found {
		if mismatch {
			d.sendError(parent, msg.ID, protocol.CodeInvalidParams, "callId was reused with different tool or arguments")
			return
		}
		d.send(parent, replay)
		return
	}

	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	default:
		d.sendError(parent, msg.ID, protocol.CodeOverloaded, "device is busy")
		return
	}

	timeout := d.callTimeout(params.Meta.TimeoutMS)
	callCtx, cancel := context.WithCancelCause(parent)
	defer cancel(errCallDone)

	replay, status := d.reserve(callID, fingerprint, msg.ID, cancel)
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
	defer d.release(callID, msg.ID)

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
		minInterval: progressInterval,
	}

	var (
		result  protocol.CallToolResult
		callErr error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				d.log.Error("tool panicked", "tool", params.Name, "panic", fmt.Sprint(r))
				result = toolErrorResult(fmt.Sprintf("internal tool error: %v", r))
				callErr = nil
			}
		}()
		result, callErr = d.registry.Call(callCtx, params.Name, params.Arguments, progress.send)
	}()

	cause := context.Cause(callCtx)
	if cause != nil {
		switch {
		case errors.Is(cause, errShutdown):
			d.log.Debug("call abandoned during shutdown", "call_id", callID)
			return
		case errors.Is(cause, errCallCancelled):
			d.sendError(parent, msg.ID, protocol.CodeRequestCancelled, "call cancelled")
			return
		case errors.Is(cause, errCallTimeout):
			result = toolErrorResult(fmt.Sprintf("tool timed out after %s", timeout))
			callErr = nil
		case parent.Err() != nil:
			d.log.Debug("call abandoned because the session closed", "call_id", callID)
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
	resp, err := protocol.NewResult(msg.ID, result)
	if err != nil {
		d.sendError(parent, msg.ID, protocol.CodeInternalError, "encode result: "+err.Error())
		return
	}
	d.cachePut(callID, fingerprint, resp.Result)
	d.send(parent, resp)
}

func (d *dispatcher) lookupCache(callID, fingerprint, rpcID string) (*protocol.Message, bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cached, ok := d.completed[callID]
	if !ok {
		return nil, false, false
	}
	if time.Now().After(cached.expires) {
		delete(d.completed, callID)
		return nil, false, false
	}
	if cached.fingerprint != fingerprint {
		return nil, true, true
	}
	return &protocol.Message{JSONRPC: "2.0", ID: rpcID, Result: cached.result}, true, false
}

func (d *dispatcher) reserve(callID, fingerprint, rpcID string, cancel context.CancelCauseFunc) (*protocol.Message, reserveStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cached, ok := d.completed[callID]; ok {
		if time.Now().Before(cached.expires) {
			if cached.fingerprint != fingerprint {
				return nil, reserveMismatch
			}
			return &protocol.Message{JSONRPC: "2.0", ID: rpcID, Result: cached.result}, reserveCached
		}
		delete(d.completed, callID)
	}
	if _, ok := d.inflight[callID]; ok {
		return nil, reserveDuplicate
	}
	d.inflight[callID] = &callState{callID: callID, cancel: cancel}
	d.rpcToCall[rpcID] = callID
	return nil, reserveStarted
}

func (d *dispatcher) release(callID, rpcID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inflight, callID)
	if d.rpcToCall[rpcID] == callID {
		delete(d.rpcToCall, rpcID)
	}
}

func (d *dispatcher) cachePut(callID, fingerprint string, result json.RawMessage) {
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.completed) >= dedupeLimit {
		for key, cached := range d.completed {
			if now.After(cached.expires) {
				delete(d.completed, key)
			}
		}
		for len(d.completed) >= dedupeLimit {
			for key := range d.completed {
				delete(d.completed, key)
			}
		}
	}
	d.completed[callID] = cachedResult{
		fingerprint: fingerprint,
		result:      append(json.RawMessage(nil), result...),
		expires:     now.Add(dedupeTTL),
	}
}

func (d *dispatcher) cancelCall(params protocol.CancelledParams) {
	d.mu.Lock()
	callID := ""
	if params.RequestID != "" {
		callID = d.rpcToCall[params.RequestID]
	}
	if callID == "" {
		callID = params.Meta.CallID
	}
	state := d.inflight[callID]
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
		Meta:          protocol.CallMeta{CallID: p.callID},
	})
	if err != nil {
		return
	}
	if err := p.writer.WriteJSON(p.ctx, msg); err != nil {
		p.log.Debug("write progress failed", "call_id", p.callID, "error", err)
	}
}
