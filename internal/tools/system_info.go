package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"termio/internal/protocol"
	"termio/internal/version"
)

// SystemInfo reports basic daemon and sandbox information.
type SystemInfo struct {
	sandbox *Sandbox
}

type systemInfoResult struct {
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	Hostname      string   `json:"hostname"`
	TermioVersion string   `json:"termio_version"`
	Workspace     string   `json:"workspace"`
	AllowedRoots  []string `json:"allowed_roots"`
}

func (t *SystemInfo) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name:        "system_info",
		Description: "Return the daemon's OS, architecture, hostname, version, workspace, and effective roots.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func (t *SystemInfo) Call(ctx context.Context, raw json.RawMessage, _ ProgressFunc) (protocol.CallToolResult, error) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return protocol.CallToolResult{}, err
	}
	select {
	case <-ctx.Done():
		return protocol.CallToolResult{}, ctx.Err()
	default:
	}
	hostname, _ := os.Hostname()
	result := systemInfoResult{
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		Hostname:      hostname,
		TermioVersion: version.Version,
		Workspace:     t.sandbox.Workspace(),
		AllowedRoots:  t.sandbox.Roots(),
	}
	var b strings.Builder
	fmt.Fprintf(&b, "os: %s\narch: %s\nhostname: %s\ntermio_version: %s\nworkspace: %s\nallowed_roots:",
		result.OS, result.Arch, result.Hostname, result.TermioVersion, result.Workspace)
	for _, root := range result.AllowedRoots {
		b.WriteString("\n  - ")
		b.WriteString(root)
	}
	return textResult(b.String(), result), nil
}
