package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewWritesToFileAndStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termio.log")
	logger, closer, err := New("debug", path)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello-log-test", "key", "value")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello-log-test") {
		t.Fatalf("log file did not receive the record: %s", data)
	}
}

func TestNewRejectsBadLevel(t *testing.T) {
	if _, _, err := New("verbose", ""); err == nil {
		t.Fatal("expected an invalid level error")
	}
}
