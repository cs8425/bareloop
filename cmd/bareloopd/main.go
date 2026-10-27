// Command bareloopd runs the bareloop agent server.
//
// Usage:
//
//	bareloopd [-config config.json] [-addr 127.0.0.1:8090] [-data data] [-web web]
//
// Everything except the listen address, the data directory and the web directory comes from
// the JSON config file, whose "defaults" block is inherited by every new session (each session
// may still override the model, the system prompt, its own MCP server set and the compaction
// parameters).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bareloop/internal/agent"
	"bareloop/internal/server"
)

// fileConfig is the on-disk configuration.
type fileConfig struct {
	// Addr is the listen address; the -addr flag wins.
	Addr string `json:"addr,omitempty"`
	// Data is where session logs live; the -data flag wins.
	Data string `json:"data,omitempty"`
	// Web is the directory of the static UI; the -web flag wins.
	Web string `json:"web,omitempty"`
	// Fsync makes every append synchronous (safer, slower).
	Fsync bool `json:"fsync,omitempty"`
	// Defaults is inherited by every session created from now on.
	Defaults agent.Config `json:"defaults,omitempty"`
	// Sessions optionally pre-creates sessions at startup.
	Sessions []agent.Config `json:"sessions,omitempty"`
	// Debug turns on per-request logging.
	Debug bool `json:"debug,omitempty"`
}

func main() {
	var (
		cfgPath = flag.String("config", "config.json", "JSON config file (optional)")
		addr    = flag.String("addr", "", "listen address, e.g. 127.0.0.1:8090")
		dataDir = flag.String("data", "", "directory for session logs (default ./data)")
		webDir  = flag.String("web", "", "directory of the web UI (default ./web)")
		fsync   = flag.Bool("fsync", false, "fsync every log append")
		verbose = flag.Bool("v", false, "log every HTTP request under /api")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmsgprefix)
	logf := logger.Printf

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8090"
	}
	if *dataDir != "" {
		cfg.Data = *dataDir
	}
	if cfg.Data == "" {
		cfg.Data = "data"
	}
	if *webDir != "" {
		cfg.Web = *webDir
	}
	if cfg.Web == "" {
		cfg.Web = findWeb()
	}
	if *fsync {
		cfg.Fsync = true
	}
	if *verbose {
		cfg.Debug = true
	}

	if err := os.MkdirAll(cfg.Data, 0o755); err != nil {
		logger.Fatalf("data dir: %v", err)
	}

	hub := server.NewHub(logf)
	mgr := agent.NewManager(cfg.Data, cfg.Fsync, cfg.Defaults, hub.Broadcast, logf)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mgr.LoadAll(ctx); err != nil {
		logf("load existing sessions: %v", err)
	}
	// Anything that already has data/<id>/ wins over this file: it is loaded above and its config is
	// never re-merged with defaults, so say what was started from where.
	for _, s := range mgr.List() {
		logf("session %q: using %s/%s/config.json (this file's defaults applied when it was created)", s.ID, cfg.Data, s.ID)
	}
	for _, sc := range cfg.Sessions {
		if _, err := mgr.Create(ctx, sc); err != nil {
			logf("session %q from this file: %v (an existing session keeps its own config; edit %s/%s/config.json)", sc.ID, err, cfg.Data, sc.ID)
		}
	}
	if len(mgr.List()) == 0 {
		if _, err := mgr.Create(ctx, mgr.DefaultSessionConfig("main", "main")); err != nil {
			logf("create the default session: %v", err)
		}
	}

	srv := server.New(mgr, hub, cfg.Web, logf)
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No read/write timeout: websockets and SSE live for a long time.
		IdleTimeout: 120 * time.Second,
	}

	host := cfg.Addr
	if h, _, err := net.SplitHostPort(cfg.Addr); err == nil {
		host = h
	}
	if host == "0.0.0.0" || host == "" || host == "::" {
		host = "127.0.0.1"
	}
	fmt.Fprintf(os.Stderr, "bareloopd listening on http://%s (data=%s web=%s)\n", cfg.Addr, cfg.Data, cfg.Web)
	if cfg.Debug {
		fmt.Fprintln(os.Stderr, "debug logging on")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logf("shutting down")
	case err := <-errCh:
		logger.Fatalf("serve: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logf("http shutdown: %v", err)
	}
	mgr.Close()
	logf("bye")
}

// loadConfig reads the JSON file, expanding ${ENV} references so secrets can stay out of it.
func loadConfig(path string) (fileConfig, error) {
	var cfg fileConfig
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && path == "config.json" {
			// The default path is a convenience: running without a config is allowed.
			return cfg, nil
		}
		return cfg, err
	}
	expanded := os.Expand(string(raw), func(key string) string {
		if key == "$" {
			return "$"
		}
		return os.Getenv(key)
	})
	if err := json.Unmarshal([]byte(expanded), &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// findWeb looks for the UI in the usual places so `go run ./cmd/bareloopd` works from the repo
// root as well as from an installed binary.
func findWeb() string {
	for _, cand := range []string{"web", "web/dist", "./web", "../web", "ui"} {
		if st, err := os.Stat(filepath.Join(cand, "index.html")); err == nil && !st.IsDir() {
			return cand
		}
	}
	return "web"
}
