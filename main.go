// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Command tsheadroom is an aperture pre_request guardrail hook that compresses
// LLM request bodies with Headroom. It listens on the tailnet via tsnet and,
// for each hook call, hands request_body.messages to a pool of persistent
// Python workers (which call `headroom.compress`) and returns a modify action.
//
// Operators must supply a Python interpreter that has `headroom-ai` installed
// (pip install headroom-ai) via the -python flag.
//
// Auth: set TS_AUTHKEY in the environment for unattended tailnet login.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

func main() {
	var (
		hostname    = flag.String("hostname", "tsheadroom", "tsnet hostname (how this node appears on the tailnet)")
		poolSize    = flag.Int("pool-size", 8, "number of Python compression workers")
		maxCompress = flag.Duration("max-compress", 60*time.Second, "hard cap on a single worker call before it's recycled (covers one-time model loads); the sole worker-side timeout")
		python      = flag.String("python", "python3", "Python interpreter with headroom-ai installed")
		script      = flag.String("worker", "worker.py", "path to worker.py")
		addr        = flag.String("addr", ":80", "listen address on the tsnet node")
		stateDir    = flag.String("state-dir", "", "tsnet state directory (default: tsnet's own default)")
		configF     = flag.String("config", "tsheadroom.config.json", "path to the tunable compress-config file (created/updated via PUT /config)")
		localAddr   = flag.String("local-addr", "", "if set, serve plain HTTP here instead of tsnet (for local testing)")
		verbose     = flag.Bool("v", false, "log a per-request summary (in/out sizes, modify/allow) to stdout")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	scriptPath, err := filepath.Abs(*script)
	if err != nil {
		log.Error("resolve worker path", "err", err)
		os.Exit(1)
	}

	configPath, err := filepath.Abs(*configF)
	if err != nil {
		log.Error("resolve config path", "err", err)
		os.Exit(1)
	}
	settings := loadSettings(configPath, log)

	// Workers preload the ML model at startup when text compression is enabled;
	// the decision lives here (single source of truth) and is re-evaluated at
	// each spawn, so a worker respawned after a runtime change is up to date.
	pool := NewPool(*poolSize, *python, scriptPath, func() bool {
		return settings.get().textEnabled()
	}, *maxCompress, log)
	defer pool.Shutdown()

	handler := &Handler{
		comp:     pool,
		settings: settings,
		log:      log,
		verbose:  *verbose,
		out:      os.Stdout,
	}

	// /config is the runtime tuning API; everything else is the aperture hook.
	mux := http.NewServeMux()
	mux.Handle("/config", &configHandler{store: settings, log: log})
	mux.Handle("/", handler)
	httpSrv := &http.Server{Handler: mux}

	ln, cleanup, err := listen(*localAddr, *addr, *hostname, *stateDir, log)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	defer cleanup()

	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
		}
	}()
	log.Info("tsheadroom listening",
		"mode", modeName(*localAddr),
		"hostname", *hostname,
		"addr", listenAddr(*localAddr, *addr),
		"pool_size", *poolSize,
		"max_compress", *maxCompress,
		"python", *python,
		"config", configPath,
		"verbose", *verbose,
	)

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	<-sigc
	log.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx) // stop accepting; defers tear down pool + tsnet
}

// listen returns a net.Listener either on a plain local address (testing) or on
// a tsnet node (production), plus a cleanup func.
func listen(localAddr, addr, hostname, stateDir string, log *slog.Logger) (net.Listener, func(), error) {
	if localAddr != "" {
		ln, err := net.Listen("tcp", localAddr)
		if err != nil {
			return nil, func() {}, err
		}
		return ln, func() { _ = ln.Close() }, nil
	}

	srv := &tsnet.Server{Hostname: hostname}
	if stateDir != "" {
		srv.Dir = stateDir
	}
	if k := os.Getenv("TS_AUTHKEY"); k != "" {
		srv.AuthKey = k
	}
	// Keep tsnet's own logs off for now; a future -vv will surface them.
	srv.Logf = func(string, ...any) {}

	ln, err := srv.Listen("tcp", addr)
	if err != nil {
		_ = srv.Close()
		return nil, func() {}, err
	}
	cleanup := func() {
		_ = ln.Close()
		_ = srv.Close()
	}
	return ln, cleanup, nil
}

func modeName(localAddr string) string {
	if localAddr != "" {
		return "local"
	}
	return "tsnet"
}

func listenAddr(localAddr, addr string) string {
	if localAddr != "" {
		return localAddr
	}
	return addr
}
