// Package gateway is a single-instance reference implementation of the Termio
// cloud gateway. It holds daemon WebSocket sessions and exposes an HTTP API that
// cloud agents use to invoke remote tools. It is intentionally not a production
// multi-tenant service.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"

	"termio/internal/protocol"
	"termio/internal/version"
	"termio/internal/wsx"
)

const (
	defaultHeartbeat   = 20 * time.Second
	defaultCallTimeout = 30 * time.Second
	maxCallTimeout     = 5 * time.Minute
	registerTimeout    = 10 * time.Second
	maxFrameBytes      = 4 << 20
	maxRequestBody     = 1 << 20
)

// Options configures the reference gateway. Exactly one of Token or Devices
// must be non-empty. Devices maps device IDs to per-device bearer tokens.
type Options struct {
	Token             string
	Devices           map[string]string
	Logger            *slog.Logger
	HeartbeatInterval time.Duration
	CallTimeout       time.Duration
}

// Server is the reference gateway.
type Server struct {
	opts    Options
	hub     *hub
	handler http.Handler
}

// New validates options and builds the HTTP handler.
func New(opts Options) (*Server, error) {
	if len(opts.Devices) == 0 && strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("gateway requires --token or --devices-file")
	}
	for id, token := range opts.Devices {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(token) == "" {
			return nil, errors.New("devices file entries require non-empty id and token")
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = defaultHeartbeat
	}
	if opts.HeartbeatInterval < 5*time.Second {
		opts.HeartbeatInterval = 5 * time.Second
	}
	if opts.HeartbeatInterval > 60*time.Second {
		opts.HeartbeatInterval = 60 * time.Second
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	if opts.CallTimeout > maxCallTimeout {
		opts.CallTimeout = maxCallTimeout
	}

	s := &Server{opts: opts, hub: newHub()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /v1/devices", s.handleDevices)
	mux.HandleFunc("GET /v1/devices/{id}/tools", s.handleTools)
	mux.HandleFunc("POST /v1/devices/{id}/tools/call", s.handleCall)
	s.handler = mux
	return s, nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Close terminates all active daemon sessions.
func (s *Server) Close() {
	for _, dc := range s.hub.snapshot() {
		dc.close(websocket.StatusGoingAway, "gateway shutting down")
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": version.Version,
		"devices": len(s.hub.snapshot()),
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	all, allowed, ok := s.authorizeList(token)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	devices := make([]deviceSnapshot, 0)
	for _, dc := range s.hub.snapshot() {
		if !all && !allowed[dc.id] {
			continue
		}
		devices = append(devices, dc.snapshot())
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !s.tokenMatchesDevice(deviceID, bearerToken(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	dc, ok := s.hub.get(deviceID)
	if !ok {
		writeError(w, http.StatusNotFound, protocol.CodeDeviceOffline, "device is not connected")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": deviceID,
		"tools":     dc.snapshot().Tools,
	})
}

type callRequest struct {
	Name          string          `json:"name"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	TimeoutMS     int             `json:"timeout_ms,omitempty"`
	CallID        string          `json:"call_id,omitempty"`
	ProgressToken string          `json:"progress_token,omitempty"`
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !s.tokenMatchesDevice(deviceID, bearerToken(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var body callRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidParams, "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidParams, "tool name is required")
		return
	}
	callID := body.CallID
	if callID == "" {
		callID = protocol.NewID("call_")
	}
	if !validCallID(callID) {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidParams, "call_id must be 1-128 characters of letters, digits, dot, dash, or underscore")
		return
	}

	dc, ok := s.hub.get(deviceID)
	if !ok {
		writeError(w, http.StatusNotFound, protocol.CodeDeviceOffline, "device is not connected")
		return
	}

	timeout := s.opts.CallTimeout
	if body.TimeoutMS > 0 {
		requested := time.Duration(body.TimeoutMS) * time.Millisecond
		if requested < time.Second {
			requested = time.Second
		}
		if requested < timeout {
			timeout = requested
		}
	}
	if timeout > maxCallTimeout {
		timeout = maxCallTimeout
	}

	args := body.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	rpcID := protocol.NewID("g_")
	req, err := protocol.NewRequest(rpcID, protocol.MethodToolsCall, protocol.ToolCallParams{
		Name:      body.Name,
		Arguments: args,
		Meta: protocol.CallMeta{
			CallID:        callID,
			ProgressToken: body.ProgressToken,
			TimeoutMS:     int(timeout / time.Millisecond),
		},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.CodeInternalError, err.Error())
		return
	}

	// Give the daemon's own deadline a grace period to win, so normal tool
	// timeouts arrive as isError results rather than gateway 504s.
	ctx, cancel := context.WithTimeout(r.Context(), timeout+2*time.Second)
	defer cancel()

	started := time.Now()
	resp, err := dc.request(ctx, req)
	elapsed := time.Since(started)
	if err != nil {
		if ctx.Err() != nil {
			dc.notifyCancel(rpcID, callID, "client_cancelled")
		}
		switch {
		case r.Context().Err() != nil:
			// The cloud client went away; no one is listening for a response.
			return
		case errors.Is(err, context.DeadlineExceeded):
			s.logCall(deviceID, body.Name, callID, http.StatusGatewayTimeout, elapsed)
			writeError(w, http.StatusGatewayTimeout, protocol.CodeRequestTimeout, "device did not answer before the deadline")
		case errors.Is(err, errDeviceClosed):
			s.logCall(deviceID, body.Name, callID, http.StatusBadGateway, elapsed)
			writeError(w, http.StatusBadGateway, protocol.CodeDeviceOffline, "device disconnected")
		default:
			s.logCall(deviceID, body.Name, callID, http.StatusBadGateway, elapsed)
			writeError(w, http.StatusBadGateway, protocol.CodeInternalError, err.Error())
		}
		return
	}

	if resp.Error != nil {
		status := httpStatusForCode(resp.Error.Code)
		s.logCall(deviceID, body.Name, callID, status, elapsed)
		writeError(w, status, resp.Error.Code, resp.Error.Message)
		return
	}
	if len(resp.Result) == 0 {
		s.logCall(deviceID, body.Name, callID, http.StatusBadGateway, elapsed)
		writeError(w, http.StatusBadGateway, protocol.CodeInternalError, "empty tool result")
		return
	}

	s.logCall(deviceID, body.Name, callID, http.StatusOK, elapsed)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Result)
}

func (s *Server) logCall(deviceID, tool, callID string, status int, elapsed time.Duration) {
	s.opts.Logger.Info("tool call",
		"device_id", deviceID,
		"tool", tool,
		"call_id", callID,
		"status", status,
		"duration_ms", elapsed.Milliseconds())
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimSpace(r.Header.Get("X-Termio-Device"))
	if deviceID == "" {
		deviceID = strings.TrimSpace(r.URL.Query().Get("device_id"))
	}
	if deviceID == "" {
		http.Error(w, "missing device id", http.StatusBadRequest)
		return
	}
	if !s.tokenMatchesDevice(deviceID, bearerToken(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.opts.Logger.Warn("websocket accept failed", "device_id", deviceID, "error", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxFrameBytes)
	writer := wsx.New(conn)

	regCtx, cancel := context.WithTimeout(context.Background(), registerTimeout)
	typ, data, err := conn.Read(regCtx)
	cancel()
	if err != nil {
		_ = conn.Close(websocket.StatusCode(protocol.CloseRegisterTimeout), "registration timeout")
		return
	}
	if typ != websocket.MessageText {
		_ = conn.Close(websocket.StatusPolicyViolation, "registration must be a text frame")
		return
	}
	var msg protocol.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "malformed registration")
		return
	}
	if msg.JSONRPC != "2.0" || msg.Method != protocol.MethodRegister || msg.ID == "" {
		_ = conn.Close(websocket.StatusPolicyViolation, "expected termio/register")
		return
	}
	var params protocol.RegisterParams
	if err := protocol.DecodeParams(&msg, &params); err != nil {
		_ = writer.WriteJSON(context.Background(), protocol.NewErrorResponse(msg.ID, protocol.CodeInvalidParams, err.Error()))
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid registration")
		return
	}
	if params.Device.ID != deviceID {
		_ = writer.WriteJSON(context.Background(), protocol.NewErrorResponse(msg.ID, protocol.CodeInvalidParams, "device id does not match the authenticated device"))
		_ = conn.Close(websocket.StatusCode(protocol.CloseUnauthorized), "device id mismatch")
		return
	}
	if params.ProtocolVersion != protocol.ProtocolVersion {
		_ = writer.WriteJSON(context.Background(), protocol.NewErrorResponse(msg.ID, protocol.CodeUnsupportedProto,
			fmt.Sprintf("unsupported protocol version %d", params.ProtocolVersion)))
		_ = conn.Close(websocket.StatusCode(protocol.CloseUnsupportedVersion), "unsupported protocol version")
		return
	}

	maxInflight := params.MaxInflight
	if maxInflight < 1 {
		maxInflight = 4
	}
	if maxInflight > 32 {
		maxInflight = 32
	}
	regResult := protocol.RegisterResult{
		ProtocolVersion:     protocol.ProtocolVersion,
		MCPVersion:          protocol.MCPVersion,
		SessionID:           protocol.NewID("sess_"),
		ServerTime:          time.Now().UTC().Format(time.RFC3339),
		HeartbeatIntervalMS: int(s.opts.HeartbeatInterval / time.Millisecond),
		CallTimeoutMS:       int(s.opts.CallTimeout / time.Millisecond),
		MaxInflight:         maxInflight,
	}
	regResp, err := protocol.NewResult(msg.ID, regResult)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "encode registration response")
		return
	}
	if err := writer.WriteJSON(context.Background(), regResp); err != nil {
		s.opts.Logger.Warn("write registration response failed", "device_id", deviceID, "error", err)
		return
	}

	dc := newDeviceConn(deviceID, conn, writer, s.opts.Logger, params.Device, params.Tools, maxInflight, s.hub)
	if old := s.hub.add(dc); old != nil {
		s.opts.Logger.Info("replaced previous device session", "device_id", deviceID)
	}
	s.opts.Logger.Info("device connected",
		"device_id", deviceID,
		"os", params.Device.OS,
		"arch", params.Device.Arch,
		"tools", len(params.Tools),
		"remote", r.RemoteAddr)

	go dc.heartbeat(s.opts.HeartbeatInterval)
	dc.readLoop()
	dc.close(websocket.StatusGoingAway, "session ended")
	s.opts.Logger.Info("device disconnected", "device_id", deviceID)
}

func (s *Server) tokenMatchesDevice(deviceID, token string) bool {
	if token == "" || deviceID == "" {
		return false
	}
	if expected, ok := s.opts.Devices[deviceID]; ok && constantTimeEqual(expected, token) {
		return true
	}
	if s.opts.Token != "" && constantTimeEqual(s.opts.Token, token) {
		return true
	}
	return false
}

func (s *Server) authorizeList(token string) (all bool, allowed map[string]bool, ok bool) {
	if token == "" {
		return false, nil, false
	}
	allowed = make(map[string]bool)
	for id, expected := range s.opts.Devices {
		if constantTimeEqual(expected, token) {
			allowed[id] = true
		}
	}
	if s.opts.Token != "" && constantTimeEqual(s.opts.Token, token) {
		return true, nil, true
	}
	return false, allowed, len(allowed) > 0
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func validCallID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func httpStatusForCode(code int) int {
	switch code {
	case protocol.CodeInvalidParams, protocol.CodeMethodNotFound:
		return http.StatusBadRequest
	case protocol.CodeRequestCancelled, protocol.CodeDuplicateCall:
		return http.StatusConflict
	case protocol.CodeRequestTimeout:
		return http.StatusGatewayTimeout
	case protocol.CodeOverloaded:
		return http.StatusTooManyRequests
	case protocol.CodeDeviceOffline:
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}
