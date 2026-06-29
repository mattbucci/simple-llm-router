// Command router is the composition root for simple-llm-router (ADR-0003): it is
// the only place that constructs concrete implementations and wires the layers
// together. It loads and fully validates the configuration before listening
// (ADR-0010), starts the background health/discovery loop, builds the router and
// HTTP server, and runs until SIGINT/SIGTERM triggers a graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/mattbucci/simple-llm-router/internal/backend"
	"github.com/mattbucci/simple-llm-router/internal/config"
	"github.com/mattbucci/simple-llm-router/internal/model"
	"github.com/mattbucci/simple-llm-router/internal/observability"
	"github.com/mattbucci/simple-llm-router/internal/router"
	"github.com/mattbucci/simple-llm-router/internal/server"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight requests
// (including draining streams, ADR-0007) before forcing the listener closed.
const shutdownTimeout = 30 * time.Second

// Build metadata, stamped at release time by GoReleaser via -ldflags (see
// .goreleaser.yaml). The defaults apply to `go build`/`go run` development
// builds; a released binary reports its tag, commit, and build date via
// --version.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run())
}

// run wires the process and returns the exit code. It is split out from main so
// that deferred cleanup executes before os.Exit (which would otherwise skip
// deferred calls).
func run() int {
	configPath := flag.String("config", "", "path to the YAML configuration file (required)")
	logLevel := flag.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	showVersion := flag.Bool("version", false, "print build version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("simple-llm-router %s (commit %s, built %s, %s/%s)\n",
			version, commit, date, runtime.GOOS, runtime.GOARCH)
		return 0
	}

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "fatal: --config is required")
		return 1
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: invalid --log-level %q: %v\n", *logLevel, err)
		return 1
	}
	logger := observability.NewLogger(level)

	// ADR-0010: load and fully validate the config before the server begins
	// listening; any error aborts the process non-zero with a clear message.
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		return 1
	}

	// The root context is cancelled on SIGINT/SIGTERM and drives the lifetime of
	// every background goroutine (metrics owner, health loop) per ADR-0015.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics := observability.New(ctx)

	// Build one backend client per configured upstream. The client carries the
	// provider protocol and credentials; protocol translation is the backend's
	// concern (ADR-0002, ADR-0016).
	timeouts := backend.ClientTimeouts{
		Connect: cfg.Timeouts.Connect.Std(),
		Request: cfg.Timeouts.Request.Std(),
		Idle:    cfg.Timeouts.Idle.Std(),
	}
	clients := make([]*backend.Client, 0, len(cfg.Backends))
	for _, b := range cfg.Backends {
		clients = append(clients, backend.NewClient(
			b.Name,
			b.BaseURL,
			// Config validation (ADR-0010) guarantees Protocol is a known enum, so
			// the raw string maps straight onto the domain Protocol.
			model.Protocol(b.Protocol),
			b.Credentials.APIKey,
			b.Credentials.AnthropicVersion,
			timeouts,
		))
	}

	// The discovery/health loop owns a single background goroutine bound to ctx
	// (ADR-0005). It publishes its health gauge to metrics via the callback so the
	// backend layer never imports observability (ADR-0003).
	monitor := backend.NewMonitor(
		clients,
		cfg.Health.Interval.Std(),
		cfg.Health.Timeout.Std(),
		func(name string, healthy bool) { metrics.SetHealth(name, healthy) },
	)
	monitor.Start(ctx)

	// The *backend.Client values structurally satisfy router.Backend, and
	// *backend.Monitor structurally satisfies router.HealthView (ADR-0003).
	backends := make(map[string]router.Backend, len(clients))
	for _, c := range clients {
		backends[c.Name()] = c
	}

	aliases := make(map[string]*router.Alias, len(cfg.Aliases))
	for name, a := range cfg.Aliases {
		aliases[name] = toAlias(name, a)
	}

	rt := router.New(backends, monitor, metrics, aliases, logger)

	auth := server.NewStaticTokenAuth(cfg.Auth.Tokens)

	// The optional audio gateway proxy runs parallel to the chat router
	// (ADR-0022); an empty base_url leaves it unserved.
	audio := server.AudioConfig{
		Gateway: toAudioTarget(cfg.Audio),
		Connect: cfg.Timeouts.Connect.Std(),
	}
	srv := server.New(rt, monitor, metrics, auth, int64(cfg.MaxBodySize), audio, logger)

	httpServer := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}

	// Serve in the background so the main goroutine can wait for either a shutdown
	// signal or a fatal listen error.
	srvErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()
	logger.Info("router listening",
		slog.String("addr", cfg.Listen),
		slog.Int("backends", len(clients)),
		slog.Int("aliases", len(aliases)),
		slog.Bool("audio", cfg.Audio.Configured()),
	)

	exitCode := 0
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	case err := <-srvErr:
		logger.Error("http server failed", slog.String("error", err.Error()))
		exitCode = 1
	}

	// Graceful shutdown stops accepting new connections and waits for in-flight
	// requests to finish, up to shutdownTimeout. Use a fresh context because the
	// root ctx is already cancelled once a signal arrives.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
		exitCode = 1
	}
	logger.Info("router stopped")
	return exitCode
}

// toAlias converts a config alias into the router's resolved Alias view so the
// router never imports internal/config (ADR-0003). The map key becomes the alias
// name and max_completion_tokens maps onto the router's MaxTokens field.
func toAlias(name string, a config.Alias) *router.Alias {
	return &router.Alias{
		Name:        name,
		Type:        a.Type,
		Selector:    a.Selector,
		Model:       a.Model,
		Backends:    a.Backends,
		MinQuality:  a.MinQuality,
		Pool:        toPool(a.Pool),
		Panel:       toPool(a.Panel),
		Judge:       toTarget(a.Judge),
		Synthesis:   toTarget(a.Synthesis),
		MinPanel:    a.MinPanelResponses,
		Temperature: a.Temperature,
		MaxTokens:   a.MaxCompletionTokens,
	}
}

// toAudioTarget converts the config audio gateway into the server's AudioTarget
// so the server never imports internal/config (ADR-0003). An empty base_url maps
// to a zero target the server leaves unserved (ADR-0022).
func toAudioTarget(a config.AudioBackend) server.AudioTarget {
	return server.AudioTarget{BaseURL: a.BaseURL, Token: a.Credentials.APIKey}
}

// toPool converts a config pool/panel into the router's PoolEntry slice.
func toPool(in []config.PoolEntry) []router.PoolEntry {
	if in == nil {
		return nil
	}
	out := make([]router.PoolEntry, len(in))
	for i, p := range in {
		out[i] = router.PoolEntry{Model: p.Model, Backends: p.Backends, Quality: p.Quality}
	}
	return out
}

// toTarget converts a config fusion target into the router's Target.
func toTarget(t config.Target) router.Target {
	return router.Target{Model: t.Model, Backends: t.Backends}
}
