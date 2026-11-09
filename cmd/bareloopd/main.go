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
	"crypto/tls"
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

	"github.com/klauspost/compress/zstd"

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
	// MCPLock freezes every session's MCP server set over the API: sessions may enable or
	// disable and re-policy the servers of defaults.mcp, and nothing else about them; PATCH,
	// create and import carry the daemon's list or nothing. Sessions already on disk keep
	// whatever they have — the lock guards writes, not history — and this file's own
	// "sessions" entries are config, so they are not what the lock speaks to.
	MCPLock bool `json:"mcpLock,omitempty"`
	// Bind is the listening surface around the API itself: whom to believe when something
	// in front speaks for a client, and the key pair served on the port. It lives apart
	// from "auth" because both facts outlive the login gate — TLS is useful with no gate,
	// and trustedProxies names networks whether or not anything behind them must log in.
	Bind bindConfig `json:"bind,omitempty"`
	// Auth is the login gate. Absent — the default — every route answers anonymously
	// exactly as it always did; present, and sessions, websocket, SSE, the profiler and
	// the API all require the credential first, with GET /api/health the one exemption
	// a container healthcheck can knock on. Setting the gate off when a forward-auth
	// proxy takes over is deleting this object, not editing it.
	Auth *authConfig `json:"auth,omitempty"`
}

// bindConfig is the listening surface: see fileConfig.Bind.
type bindConfig struct {
	// TrustedProxies names the networks whose X-Forwarded-For may be believed, as
	// CIDRs ("10.0.0.0/8") or bare addresses ("127.0.0.1", read as /32). Empty — the
	// default — means the TCP peer address is the client and the header is never
	// consulted: believing it without a declared boundary would hand every request the
	// pen that writes the rate limiter's buckets.
	TrustedProxies []string `json:"trustedProxies,omitempty"`
	// TLS is the certificate pair this port serves HTTPS with. Its presence is the
	// switch: with it set the port speaks TLS only (no plaintext, no redirect — an
	// http-to-https redirect needs an external hostname this daemon does not know),
	// and login cookies get the Secure flag from the same fact.
	TLS *tlsBind `json:"tls,omitempty"`
}

type tlsBind struct {
	Cert string `json:"cert,omitempty"`
	Key  string `json:"key,omitempty"`
}

// authConfig is the gate: one operator, one password. Pass arrives through the
// same ${ENV} expansion every other secret in this file uses, and it is stored
// in plain text because the login popup and HTTP Basic both hand the password
// over on every attempt — a hash could serve the popup (one verify per login)
// but would have to be recomputed per request to serve Basic, and nothing
// deserves 0.4 seconds per request. Treat this file as a keystore (0600) and
// give the daemon a password of its own.
type authConfig struct {
	User string `json:"user,omitempty"`
	Pass string `json:"pass,omitempty"`
	// Session bounds a login by inactivity: a cookie untouched for this long
	// (default "168h") stops answering, while use keeps it alive — the deliberate
	// trade being that a stolen cookie in constant use never ages out, and every
	// honest session outlives its deadline without noticing. The cookie itself
	// carries no expiry: closing the browser is a logout with no button.
	Session sessionConfig `json:"session,omitempty"`
	// RateLimit is the failed-login window: MaxAttempts wrong credentials per key
	// per Window, after which the key hears 429 until the window rolls. The key is
	// the TLS fingerprint when one was seen and the client address otherwise. The
	// window always ends by itself; there is nothing to unlock by hand.
	RateLimit rateLimitConfig `json:"rateLimit,omitempty"`
}

type sessionConfig struct {
	IdleTimeout string `json:"idleTimeout,omitempty"` // duration string, default "168h"
}

type rateLimitConfig struct {
	MaxAttempts int    `json:"maxAttempts,omitempty"` // default 5
	Window      string `json:"window,omitempty"`      // duration string, default "15m"
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
		comp      = flag.String("compress", "zstd", `compression of new payloads: zstd | gzip | none (zstd writes stamp the dictionary embedded in the binary, or -zdict; old stores read whichever codec wrote them)`)
		zdictPath = flag.String("zdict", "", "zstd dictionary file: new zstd payloads are stamped with it — the file is data, not a toggle: frames it writes need it to read back forever")
		zstdSlots = flag.Int("zstd-slots", 2, `resident zstd encoder pool depth, ~10MB of tables per slot; 0 or -1 means auto: one slot per CPU core (80MB+ of RAM that never leaves on an 8-core box, 300MB+ on a 32-core one — the depth only matters when several sessions write at once, which the per-store lock serializes anyway)`)
	)
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmsgprefix)
	logf := logger.Printf

	// The compression knob is set before any store opens — it speaks to new writes only,
	// and every store still reads every store whichever codec wrote it.
	switch *comp {
	case "gzip", "zstd", "none":
		store.Compressor = *comp
	default:
		logger.Fatalf("compress: %q is not gzip, zstd or none", *comp)
	}
	store.ZstdSlots = *zstdSlots // ≤0 leaves the library's per-core sizing in charge; read at first compression
	if *zdictPath != "" {
		b, err := os.ReadFile(*zdictPath)
		if err != nil {
			logger.Fatalf("zdict: %v", err)
		}
		if err := store.SetZdict(b); err != nil {
			logger.Fatalf("%v", err)
		}
		if info, err := zstd.InspectDictionary(b); err == nil {
			logf("zstd dictionary: %d bytes id=0x%08x from %s — keep this file as long as any store written under it must open", len(b), info.ID(), *zdictPath)
		}
	}

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
	mgr.SetMCPLock(cfg.MCPLock)

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
		// CreateFromConfig, not Create: this file is the trust root the lock itself lives in,
		// and refusing the operator's own startup lines would be the lock holding a door shut
		// on its own house.
		if _, err := mgr.CreateFromConfig(ctx, sc); err != nil {
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

	scheme := "http"
	if cfg.Bind.TLS != nil {
		scheme = "https"
	}

	// The boundary for X-Forwarded-For parses even with no gate in front of it: it is a
	// fact about the listening surface, and a typo'd CIDR deserves to be fatal at
	// startup either way.
	trusted, err := server.ParseNets(cfg.Bind.TrustedProxies)
	if err != nil {
		logger.Fatalf("bind.trustedProxies: %v", err)
	}

	// The login gate is off until this file names one, and off is the shape of every
	// config written before it existed. When it is on, everything it wraps stops
	// answering strangers — the UI pops a login dialog, curl -u and go tool pprof speak
	// Basic, and the browser's cookie is accepted everywhere a password was asked for.
	if cfg.Auth != nil {
		ac := cfg.Auth
		if ac.User == "" || ac.Pass == "" {
			logger.Fatalf(`auth: user and pass are both required (write the password as "${SOME_ENV}" to keep it out of this file)`)
		}
		idle := 7 * 24 * time.Hour
		if ac.Session.IdleTimeout != "" {
			if idle, err = time.ParseDuration(ac.Session.IdleTimeout); err != nil {
				logger.Fatalf("auth.session.idleTimeout: %v", err)
			}
		}
		window := 15 * time.Minute
		if ac.RateLimit.Window != "" {
			if window, err = time.ParseDuration(ac.RateLimit.Window); err != nil {
				logger.Fatalf("auth.rateLimit.window: %v", err)
			}
		}
		attempts := ac.RateLimit.MaxAttempts
		if attempts == 0 {
			attempts = 5
		}
		srv.EnableAuth(server.NewAuth(ac.User, ac.Pass, idle, window, attempts, cfg.Bind.TLS != nil, trusted, logf))
		logf("auth: this port requires a login (user %q, session idle %v, %d failed logins per %v) — /api/health stays free", ac.User, idle, attempts, window)
		if cfg.Bind.TLS == nil {
			if h, _, e := net.SplitHostPort(cfg.Addr); e == nil && h != "127.0.0.1" && h != "::1" && h != "localhost" {
				logf(`auth on, bound to %s over plain HTTP — the password crosses that wire on every login: set bind.tls, or terminate TLS at a proxy named in bind.trustedProxies`, cfg.Addr)
			}
		}
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
		guarded := "unauthenticated"
		if cfg.Auth != nil {
			guarded = "behind the login gate"
		}
		if h, _, err := net.SplitHostPort(cfg.Addr); err == nil && h != "127.0.0.1" && h != "::1" && h != "localhost" && cfg.Auth == nil {
			logf(`pprof on the API port at %s, which is not loopback — these endpoints are unauthenticated: bind 127.0.0.1, or use -pprof 127.0.0.1:6060 instead`, cfg.Addr)
		} else {
			logf("pprof on %s://%s/debug/pprof/ (the API port, %s)", scheme, cfg.Addr, guarded)
		}
	default:
		if _, _, err := net.SplitHostPort(cfg.Pprof); err != nil {
			logger.Fatalf(`pprof: %q is neither an address like 127.0.0.1:6060 nor "same"`, cfg.Pprof)
		}
		// Its own listener, because the API port is what the browser talks to and its catch-all route is
		// the UI. A bind failure is fatal on purpose: a daemon quietly running without the profiler you
		// asked for is the confusing outcome. Wrapped like every other handler: with a gate configured,
		// this port shares its credentials rather than its anonymity — the point of asking for pprof
		// from a machine that also runs the daemon is not to expose it to the network uninvited.
		pprofSrv = &http.Server{Addr: cfg.Pprof, Handler: srv.Wrap(server.PprofHandler()), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Fatalf("pprof listener: %v", err)
			}
		}()
		guarded := "its own port, unauthenticated"
		if cfg.Auth != nil {
			guarded = "its own port, login required (the UI cookie or -u)"
		}
		logf("pprof on http://%s/debug/pprof/ (%s)", cfg.Pprof, guarded)
	}

	// TLS is the listener's own fact: with bind.tls set, this port speaks TLS only,
	// and its config carries the fingerprint hook (tlsfp.go) that gives failed logins
	// a key that survives the guesser rotating source addresses. GetConfigForClient
	// returning nil from that hook means "handshake with this same config"; an operator
	// later adding SNI handling calls srv.FPHook from their own hook first.
	var tlsConf *tls.Config
	if cfg.Bind.TLS != nil {
		cert, err := tls.LoadX509KeyPair(cfg.Bind.TLS.Cert, cfg.Bind.TLS.Key)
		if err != nil {
			logger.Fatalf("bind.tls: %v", err)
		}
		tlsConf = &tls.Config{
			Certificates:       []tls.Certificate{cert},
			GetConfigForClient: srv.FPHook,
		}
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		TLSConfig:         tlsConf,
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
	fmt.Fprintf(os.Stderr, "bareloopd listening on %s://%s (data=%s web=%s)\n", scheme, cfg.Addr, cfg.Data, webWhat)
	if cfg.Debug {
		fmt.Fprintln(os.Stderr, "debug logging on")
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if tlsConf != nil {
			// The certificate is already in TLSConfig, so the two file-name arguments stay empty.
			err = httpSrv.ListenAndServeTLS("", "")
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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
