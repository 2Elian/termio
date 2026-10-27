// Package tools implements the file and system tools exposed by the daemon.
// Every filesystem access goes through Sandbox.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"termio/internal/protocol"
)

// ProgressFunc reports best-effort progress for a running tool call.
type ProgressFunc func(progress, total float64, message string)

// Tool is a single callable capability.
type Tool interface {
	Spec() protocol.ToolSpec
	Call(ctx context.Context, args json.RawMessage, progress ProgressFunc) (protocol.CallToolResult, error)
}

// ArgError marks a caller-visible argument problem. The daemon maps it to
// JSON-RPC -32602 instead of a tool error result.
type ArgError struct {
	Message string
}

func (e *ArgError) Error() string { return e.Message }

// UnknownToolError marks an unknown tool name.
type UnknownToolError struct {
	Name string
}

func (e *UnknownToolError) Error() string { return fmt.Sprintf("unknown tool %q", e.Name) }

// Registry holds the available tools.
type Registry struct {
	tools map[string]Tool
	specs []protocol.ToolSpec
}

// NewRegistry builds a registry and sorts tool specs by name.
func NewRegistry(list ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(list))}
	for _, tool := range list {
		spec := tool.Spec()
		r.tools[spec.Name] = tool
		r.specs = append(r.specs, spec)
	}
	sort.Slice(r.specs, func(i, j int) bool { return r.specs[i].Name < r.specs[j].Name })
	return r
}

// DefaultRegistry builds the v1 read-only tool set.
func DefaultRegistry(sb *Sandbox) *Registry {
	return NewRegistry(
		&SearchFiles{sandbox: sb},
		&ReadFile{sandbox: sb},
		&SystemInfo{sandbox: sb},
	)
}

// Specs returns a copy of the sorted tool definitions.
func (r *Registry) Specs() []protocol.ToolSpec {
	out := make([]protocol.ToolSpec, len(r.specs))
	copy(out, r.specs)
	return out
}

// Call invokes a tool by name.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage, progress ProgressFunc) (protocol.CallToolResult, error) {
	tool, ok := r.tools[name]
	if !ok {
		return protocol.CallToolResult{}, &UnknownToolError{Name: name}
	}
	return tool.Call(ctx, args, progress)
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &ArgError{Message: "invalid arguments: " + err.Error()}
	}
	return nil
}

func textResult(text string, structured any) protocol.CallToolResult {
	return protocol.CallToolResult{
		Content:           []protocol.ContentBlock{{Type: "text", Text: text}},
		StructuredContent: structured,
	}
}
