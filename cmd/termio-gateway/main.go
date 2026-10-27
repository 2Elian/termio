// Command termio-gateway is a single-instance reference gateway for local
// development and integration tests. It is not a production multi-tenant
// service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"termio/internal/gateway"
	"termio/internal/logging"
	"termio/internal/version"
)

type deviceEntry struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "termio-gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	token := flag.String("token", "", "shared bearer token (development convenience)")
	devicesFile := flag.String("devices-file", "", "JSON file with per-device tokens: [{\"id\":\"dev_...\",\"token\":\"...\"}]")
	heartbeat := flag.Duration("heartbeat", 20*time.Second, "WebSocket heartbeat interval")
	callTimeout := flag.Duration("call-timeout", 30*time.Second, "default tool call timeout")
	logLevel := flag.String("log-level", "info", "debug, info, warn, or error")
	logFile := flag.String("log-file", "", "optional log file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("termio-gateway %s (%s, built %s)\n", version.Version, version.Commit, version.BuildTime)
		return nil
	}
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	if strings.TrimSpace(*token) == "" && strings.TrimSpace(*devicesFile) == "" {
		return errors.New("provide --token or --devices-file")
	}

	devices, err := loadDevices(*devicesFile)
	if err != nil {
		return err
	}
	log, closer, err := logging.New(*logLevel, *logFile)
	if err != nil {
		return err
	}
	defer closer.Close()

	gw, err := gateway.New(gateway.Options{
		Token:             *token,
		Devices:           devices,
		Logger:            log,
		HeartbeatInterval: *heartbeat,
		CallTimeout:       *callTimeout,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info("gateway listening",
		"addr", ln.Addr().String(),
		"per_device_tokens", len(devices) > 0,
		"shared_token", strings.TrimSpace(*token) != "",
		"version", version.Version)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("gateway shutting down")
	}

	gw.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return nil
}

func loadDevices(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read devices file: %w", err)
	}
	var entries []deviceEntry
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("parse devices file: %w", err)
	}
	devices := make(map[string]string, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.Token) == "" {
			return nil, errors.New("devices file entries require id and token")
		}
		devices[entry.ID] = entry.Token
	}
	return devices, nil
}
