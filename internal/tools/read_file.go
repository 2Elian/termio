package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"termio/internal/protocol"
)

const (
	defaultReadBytes = 256 * 1024
	// 512 KiB keeps the JSON-escaped result comfortably below the 4 MiB
	// WebSocket frame limit even for control-heavy text.
	maxReadBytes = 512 * 1024
)

// ReadFile reads a bounded amount of a regular file under an allowed root.
type ReadFile struct {
	sandbox *Sandbox
}

type readFileArgs struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type readFileResult struct {
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	BytesReturned int    `json:"bytes_returned"`
	Truncated     bool   `json:"truncated"`
	IsBinary      bool   `json:"is_binary"`
	Encoding      string `json:"encoding"`
}

func (t *ReadFile) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name: "read_file",
		Description: "Read a regular file under the workspace or an allowed root. Returns UTF-8 text directly; " +
			"binary content is returned as base64. Reads are truncated at max_bytes.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Absolute path, or a path relative to the workspace."},
				"max_bytes":{"type":"integer","minimum":1,"maximum":524288,"description":"Maximum bytes to return (default 262144)."}
			},
			"required":["path"],
			"additionalProperties":false
		}`),
	}
}

func (t *ReadFile) Call(ctx context.Context, raw json.RawMessage, _ ProgressFunc) (protocol.CallToolResult, error) {
	var args readFileArgs
	if err := decodeArgs(raw, &args); err != nil {
		return protocol.CallToolResult{}, err
	}
	if args.Path == "" {
		return protocol.CallToolResult{}, &ArgError{Message: "path is required"}
	}
	if args.MaxBytes < 0 {
		return protocol.CallToolResult{}, &ArgError{Message: "max_bytes must be positive"}
	}
	limit := args.MaxBytes
	if limit == 0 {
		limit = defaultReadBytes
	}
	if limit > maxReadBytes {
		limit = maxReadBytes
	}

	resolved, err := t.sandbox.Resolve(args.Path)
	if err != nil {
		return protocol.CallToolResult{}, err
	}
	f, err := resolved.Entry.root.Open(resolved.RelOS)
	if err != nil {
		return protocol.CallToolResult{}, fmt.Errorf("open %s: %w", resolved.Abs, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return protocol.CallToolResult{}, fmt.Errorf("stat %s: %w", resolved.Abs, err)
	}
	if !info.Mode().IsRegular() {
		return protocol.CallToolResult{}, fmt.Errorf("%s is not a regular file", resolved.Abs)
	}
	select {
	case <-ctx.Done():
		return protocol.CallToolResult{}, ctx.Err()
	default:
	}

	buf := make([]byte, limit+1)
	n, readErr := io.ReadFull(f, buf)
	switch {
	case readErr == nil:
	case errors.Is(readErr, io.EOF), errors.Is(readErr, io.ErrUnexpectedEOF):
	default:
		return protocol.CallToolResult{}, fmt.Errorf("read %s: %w", resolved.Abs, readErr)
	}
	truncated := n > limit
	if truncated {
		n = limit
	}
	data := buf[:n]

	isText := utf8.Valid(data) && !bytes.Contains(data, []byte{0})
	if !isText && truncated {
		// A multibyte rune may be cut at the limit; trim at most one rune.
		for i := 1; i <= utf8.UTFMax && i <= len(data); i++ {
			if utf8.Valid(data[:len(data)-i]) && !bytes.Contains(data[:len(data)-i], []byte{0}) {
				data = data[:len(data)-i]
				isText = true
				break
			}
		}
	}

	result := readFileResult{
		Path:          resolved.Abs,
		Size:          info.Size(),
		BytesReturned: len(data),
		Truncated:     truncated,
		IsBinary:      !isText,
		Encoding:      "utf-8",
	}
	text := string(data)
	if !isText {
		result.Encoding = "base64"
		text = base64.StdEncoding.EncodeToString(data)
	}
	return textResult(text, result), nil
}
