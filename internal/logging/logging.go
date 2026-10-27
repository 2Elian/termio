// Package logging builds the daemon and gateway structured loggers.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// New returns a JSON slog logger. When file is non-empty, records go to both
// stderr and the file; if stderr is unavailable (for example a Windows GUI
// process), the file write still succeeds.
func New(level, file string) (*slog.Logger, io.Closer, error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, nil, fmt.Errorf("invalid log level %q", level)
	}

	var w io.Writer = os.Stderr
	var closer io.Closer = nopCloser{}
	if file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return nil, nil, fmt.Errorf("create log directory: %w", err)
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		w = tolerantWriter{a: os.Stderr, b: f}
		closer = f
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler), closer, nil
}

// tolerantWriter writes to both destinations and tolerates one of them
// failing. It exists so a daemon without a console still logs to its file,
// while a normal foreground run also logs to stderr.
type tolerantWriter struct {
	a, b io.Writer
}

func (w tolerantWriter) Write(p []byte) (int, error) {
	nA, errA := w.a.Write(p)
	nB, errB := w.b.Write(p)
	switch {
	case errA == nil && errB == nil:
		return max(nA, nB), nil
	case errA == nil:
		return nA, nil
	case errB == nil:
		return nB, nil
	default:
		return 0, errA
	}
}
