// Package protocol defines the JSON-RPC messages exchanged between the Termio
// gateway and the local daemon. The tool methods and result shapes follow MCP
// conventions, but the transport is a plain WebSocket and is not the MCP
// Streamable HTTP transport.
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// ProtocolVersion is the Termio wire version. Bump it only for breaking changes.
const ProtocolVersion = 1

// MCPVersion records the MCP method/result shapes this protocol follows.
const MCPVersion = "2025-06-18"

// Methods used on the wire.
const (
	MethodRegister  = "termio/register"
	MethodToolsList = "tools/list"
	MethodToolsCall = "tools/call"
	MethodProgress  = "notifications/progress"
	MethodCancelled = "notifications/cancelled"
)

// JSON-RPC and application error codes.
const (
	CodeParseError       = -32700
	CodeInvalidRequest   = -32600
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	CodeRequestCancelled = -32800
	CodeRequestTimeout   = -32001
	CodeDuplicateCall    = -32005
	CodeOverloaded       = -32010
	CodeDeviceOffline    = -32011
	CodeUnsupportedProto = -32020
)

// WebSocket close codes in the private application range.
const (
	CloseUnauthorized       = 4001
	CloseUnsupportedVersion = 4002
	CloseSuperseded         = 4003
	CloseRegisterTimeout    = 4004
	CloseOverloaded         = 4005
)

// Message is a JSON-RPC 2.0 request, notification, or response. The zero value
// is not meaningful. IDs are always strings in this protocol.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// DeviceInfo describes the machine running the daemon.
type DeviceInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

// ToolSpec is the MCP-style description of a tool.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// RegisterParams is sent by the daemon as the first frame on a connection.
type RegisterParams struct {
	ProtocolVersion int        `json:"protocol_version"`
	MCPVersion      string     `json:"mcp_version,omitempty"`
	Device          DeviceInfo `json:"device"`
	Tools           []ToolSpec `json:"tools"`
	MaxInflight     int        `json:"max_inflight,omitempty"`
}

// RegisterResult is the gateway's acknowledgement of a registration.
type RegisterResult struct {
	ProtocolVersion     int    `json:"protocol_version"`
	MCPVersion          string `json:"mcp_version,omitempty"`
	SessionID           string `json:"session_id"`
	ServerTime          string `json:"server_time"`
	HeartbeatIntervalMS int    `json:"heartbeat_interval_ms"`
	CallTimeoutMS       int    `json:"call_timeout_ms"`
	MaxInflight         int    `json:"max_inflight"`
}

// ToolsListResult is the response to tools/list.
type ToolsListResult struct {
	Tools []ToolSpec `json:"tools"`
}

// CallMeta carries Termio extensions inside MCP's _meta object.
type CallMeta struct {
	ProgressToken string `json:"progressToken,omitempty"`
	CallID        string `json:"termio/callId,omitempty"`
	TimeoutMS     int    `json:"termio/timeoutMs,omitempty"`
}

// ToolCallParams is the parameters object of a tools/call request.
type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Meta      CallMeta        `json:"_meta,omitempty"`
}

// ContentBlock is a text content block in a tool result.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult is the MCP-shaped result of a tool invocation. Tool failures
// are reported with IsError=true, not as JSON-RPC errors.
type CallToolResult struct {
	Content           []ContentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	Meta              CallMeta       `json:"_meta,omitempty"`
}

// ProgressParams is a notifications/progress payload.
type ProgressParams struct {
	ProgressToken string   `json:"progressToken"`
	Progress      float64  `json:"progress"`
	Total         float64  `json:"total,omitempty"`
	Message       string   `json:"message,omitempty"`
	Meta          CallMeta `json:"_meta,omitempty"`
}

// CancelledParams is a notifications/cancelled payload.
type CancelledParams struct {
	RequestID string   `json:"requestId"`
	Reason    string   `json:"reason,omitempty"`
	Meta      CallMeta `json:"_meta,omitempty"`
}

// NewRequest builds a JSON-RPC request. params may be nil.
func NewRequest(id, method string, params any) (*Message, error) {
	p, err := marshalOptional(params)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: "2.0", ID: id, Method: method, Params: p}, nil
}

// NewNotification builds a JSON-RPC notification (no id).
func NewNotification(method string, params any) (*Message, error) {
	p, err := marshalOptional(params)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: "2.0", Method: method, Params: p}, nil
}

// NewResult builds a successful JSON-RPC response. A nil result becomes {}.
func NewResult(id string, result any) (*Message, error) {
	var raw json.RawMessage
	if result == nil {
		raw = json.RawMessage("{}")
	} else {
		b, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return &Message{JSONRPC: "2.0", ID: id, Result: raw}, nil
}

// NewErrorResponse builds an error JSON-RPC response.
func NewErrorResponse(id string, code int, message string) *Message {
	return &Message{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: message}}
}

// DecodeParams unmarshals a request or notification's params. Missing params
// decode as an empty object, which is convenient for structs with optional
// fields.
func DecodeParams(m *Message, v any) error {
	if m == nil {
		return fmt.Errorf("nil message")
	}
	data := m.Params
	if len(data) == 0 || string(data) == "null" {
		data = json.RawMessage("{}")
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode params: %w", err)
	}
	return nil
}

// DecodeResult unmarshals a response's result.
func DecodeResult(m *Message, v any) error {
	if m == nil {
		return fmt.Errorf("nil message")
	}
	if len(m.Result) == 0 || string(m.Result) == "null" {
		return fmt.Errorf("empty result")
	}
	if err := json.Unmarshal(m.Result, v); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
}

// NewID returns prefix followed by 16 cryptographically random hex characters.
func NewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic; this fallback only keeps the
		// process alive long enough to log the real error elsewhere.
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}

func marshalOptional(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
