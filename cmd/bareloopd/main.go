// Command bareloopd runs the bareloop agent server.
//
// Usage:
//
//	bareloopd [-config config.json] [-addr 127.0.0.1:8090] [-data data] [-web web] [-pprof 127.0.0.1:6060]
//
// Everything except the listen address, the data directory and the web directory comes from
// the JSON config file, whose "defaults" block is inherited by every new session (each session
// may still override the model, the system prompt, its own MCP server set and the compaction
// parameters).
//
// The web UI travels inside this binary (see embed.go), so one file is a complete deployment. Naming a
// directory with -web, or with the config's "web" key, serves the UI from disk instead — which is what
// you want while editing it, because an edit then shows up without a rebuild.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bareloop"
	"bareloop/internal/agent"
	"bareloop/internal/server"
	"bareloop/internal/store"
)

// fileConfig is the on-disk configuration.
type fileConfig struct {
	// Addr is the listen address; the -addr flag wins.
	Addr string `json:"addr,omitempty"`
	// Data is where session logs live; the -data flag wins.
	Data string `json:"data,omitempty"`
	// Web is the directory of the static UI; the -web flag wins. Empty — the default — means the copy
	// embedded in the binary.
	Web string `json:"web,omitempty"`
	// Fsync makes every append synchronous (safer, slower).
	Fsync bool `json:"fsync,omitempty"`
	// Defaults is inherited by every session created from now on.
	Defaults agent.Config `json:"defaults,omitempty"`
	// Sessions optionally pre-creates sessions at startup.
	Sessions []agent.Config `json:"sessions,omitempty"`
	// Debug turns on per-request logging.
	Debug bool `json:"debug,omitempty"`
	// Pprof turns Go's runtime profiles on: either an address to listen on ("127.0.0.1:6060") or
	// "same" to serve them on the API port at /debug/pprof/. Empty means off. They are unauthenticated
	// either way, which is why 127.0.0.1 is the point of asking for it.
	Pprof string `json:"pprof,omitempty"`
}

func main() {
	var (
		cfgPath   = flag.String("config", "config.json", "JSON config file (optional)")
		addr      = flag.String("addr", "", "listen address, e.g. 127.0.0.1:8090")
		dataDir   = flag.String("data", "", "directory for session logs (default ./data)")
		webDir    = flag.String("web", "", "serve the UI from this directory instead of the copy embedded in the binary")
		testdata  = flag.String("testdata", "", "development files: a directory served at /testdata/ (off by default; nothing authenticates on this port)")
		fsync     = flag.Bool("fsync", false, "fsync every log append")
		verbose   = flag.Bool("v", false, "log every HTTP request under /api")
		pprofAddr = flag.String("pprof", "", "runtime profiles: an address like 127.0.0.1:6060, or the word same to mount them on the API port; empty = off")
		nocomp    = flag.Bool("nocompress", false, "store entry payloads uncompressed (default: gzip when it pays; old stores read either way)")
	)
	flag.Parse()
	// The compression knob says so before any store opens — it speaks to new writes only,
	// and every store still reads every store whichever way each was written.
	if *nocomp {
		store.Compress = false
	}

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
	// An empty cfg.Web is the UI embedded in the binary. There is deliberately no search of the usual
	// places any more: a binary that carries its own UI should use it, and quietly serving a stale ./web
	// because it happened to be the working directory is the worse surprise.
	//
	// Naming a directory is a request, so getting it wrong is fatal: the alternative is a daemon that
	// serves a 404 in the browser and never mentions it on stderr.
	if cfg.Web != "" {
		if st, err := os.Stat(filepath.Join(cfg.Web, "index.html")); err != nil || st.IsDir() {
			logger.Fatalf("web dir: %s holds no index.html — name a directory that has the UI, or drop the -web flag and this file's \"web\" key to serve the copy embedded in the binary", cfg.Web)
		}
	}
	// Development files are off unless a directory is named, and the mount is only ever a window onto
	// that one directory — a name is cleaned against it before it is used, so nothing climbs out of it.
	// What it is a window onto is the operator's business, except for one case: the directory bareloopd
	// runs from holds the config file with its API key and the session logs with their conversations, and
	// there is nothing in front of the API port. So that spelling is refused rather than warned about.
	if *testdata != "" {
		if st, err := os.Stat(*testdata); err != nil || !st.IsDir() {
			logger.Fatalf("testdata dir: %s is not a directory — name a directory of test files, or drop the flag and there is no mount at all", *testdata)
		}
		switch filepath.Clean(*testdata) {
		case ".", "..":
			logger.Fatalf("testdata dir: %s is the directory bareloopd runs from — it holds the config file and the session logs, and this port is unauthenticated; name a directory of test files instead", *testdata)
		}
	}
	if *fsync {
		cfg.Fsync = true
	}
	if *verbose {
		cfg.Debug = true
	}
	if *pprofAddr != "" {
		cfg.Pprof = *pprofAddr
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

	// The UI that ships with this binary, rooted at the UI itself rather than at the "web" prefix.
	embeddedUI, err := fs.Sub(bareloop.WebUI, "web")
	if err != nil {
		logger.Fatalf("embedded UI: %v", err)
	}
	srv := server.New(mgr, hub, embeddedUI, cfg.Web, logf)

	// The development mount, put in place before Handler builds the routes (ServeTestdata). The startup
	// line makes the same promise pprof's does: this is an unauthenticated port, so it says what is on it.
	if *testdata != "" {
		srv.ServeTestdata(*testdata)
		logf("testdata: %s is served at http://%s/testdata/ — unauthenticated, like everything else on this port", *testdata, cfg.Addr)
	}

	// pprof is off unless asked for, and asking has two shapes: an address (a listener of its own) or
	// "same" (the API port, under /debug/pprof/). The difference matters because /debug/pprof/profile is
	// a 30-second CPU profile with nothing in front of it, so the startup line names which one you got.
	// This switch is above srv.Handler() on purpose: EnablePprof has to be seen before the mux is built.
	var pprofSrv *http.Server
	switch cfg.Pprof {
	case "":
	case "same":
		srv.EnablePprof()
		if h, _, err := net.SplitHostPort(cfg.Addr); err == nil && h != "127.0.0.1" && h != "::1" && h != "localhost" {
			logf(`pprof on the API port at %s, which is not loopback — these endpoints are unauthenticated: bind 127.0.0.1, or use -pprof 127.0.0.1:6060 instead`, cfg.Addr)
		} else {
			logf("pprof on http://%s/debug/pprof/ (the API port, unauthenticated)", cfg.Addr)
		}
	default:
		if _, _, err := net.SplitHostPort(cfg.Pprof); err != nil {
			logger.Fatalf(`pprof: %q is neither an address like 127.0.0.1:6060 nor "same"`, cfg.Pprof)
		}
		// Its own listener, because the API port is what the browser talks to and its catch-all route is
		// the UI. A bind failure is fatal on purpose: a daemon quietly running without the profiler you
		// asked for is the confusing outcome.
		pprofSrv = &http.Server{Addr: cfg.Pprof, Handler: server.PprofHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Fatalf("pprof listener: %v", err)
			}
		}()
		logf("pprof on http://%s/debug/pprof/ (its own port, unauthenticated)", cfg.Pprof)
	}

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
	webWhat := cfg.Web
	if webWhat == "" {
		webWhat = "embedded"
	}
	fmt.Fprintf(os.Stderr, "bareloopd listening on http://%s (data=%s web=%s)\n", cfg.Addr, cfg.Data, webWhat)
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
	if pprofSrv != nil {
		if err := pprofSrv.Shutdown(shutdownCtx); err != nil {
			logf("pprof shutdown: %v", err)
		}
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
