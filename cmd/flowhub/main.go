// Command flowhub runs the FlowHub orchestration hub.
//
// Phase 1 scope: receive YouTrack webhook deliveries, enforce the three entry
// locks, deduplicate, audit every delivery to disk and answer 202 immediately.
// No opencode call happens on this path yet — see the design documents in the
// repository root (sections 12.1 and 12.2).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/yusiwen/flowhub/internal/config"
	"github.com/yusiwen/flowhub/internal/dedupe"
	"github.com/yusiwen/flowhub/internal/logging"
	"github.com/yusiwen/flowhub/internal/metrics"
	"github.com/yusiwen/flowhub/internal/store"
	"github.com/yusiwen/flowhub/internal/webhook"
)

// Build metadata, injected by the Makefile:
//
//	go build -ldflags "-X main.Version=1.2.3 -X main.CommitSHA=abc1234 -X main.BuildTime=..."
//
// A plain `go build` leaves the defaults and the revision falls back to whatever
// the Go toolchain stamped into the binary (vcs.revision).
var (
	Version   = "dev"
	CommitSHA = "unknown"
	BuildTime = ""
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "flowhub: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		printConfig = flag.Bool("print-config", false, "print the effective configuration (secrets masked) and exit")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(versionLine())
		return nil
	}
	if *printConfig {
		fmt.Print(cfg.Report())
		return nil
	}
	// Refuse to start on configuration that would silently widen the attack
	// surface (a wildcard bind without an explicit acknowledgement). Checked
	// before any file is created so a refused start leaves nothing behind.
	if problems := cfg.Problems(); len(problems) > 0 {
		return fmt.Errorf("refusing to start:\n  - %s", strings.Join(problems, "\n  - "))
	}

	logger, logCloser, err := logging.New(logging.Config{
		Level:    cfg.LogLevel,
		Format:   cfg.LogFormat,
		File:     cfg.ResolvedLogFile(),
		MaxBytes: cfg.LogMaxBytes,
		MaxFiles: cfg.LogMaxFiles,
	})
	if err != nil {
		return err
	}
	if logCloser != nil {
		defer logCloser.Close()
	}
	slog.SetDefault(logger)

	started := time.Now()
	stats := metrics.New()

	audit, err := store.OpenJSONL(cfg.DataDir)
	if err != nil {
		return err
	}

	// The same delivery is written twice on purpose: JSONL is the machine
	// readable audit trail, the detail log is the human readable view of the real
	// payload. Both writes happen on the queue goroutine, off the request path.
	recorders := []store.Recorder{audit}
	var detail *store.Detail
	if cfg.DetailLog {
		detail, err = store.OpenDetail(cfg.DetailLogDir(), cfg.DetailLogMaxBodyBytes)
		if err != nil {
			return err
		}
		recorders = append(recorders, detail)
	}
	sink := store.NewAsync(store.NewMulti(recorders...), cfg.QueueSize, logger, stats)
	cache := dedupe.New(cfg.DedupeTTL)

	handler := webhook.New(webhook.Options{
		HookKey:        cfg.HookKey,
		TokenHeader:    cfg.TokenHeader,
		Token:          cfg.Token,
		AllowedSources: cfg.AllowedSources,
		MaxBodyBytes:   cfg.MaxBodyBytes,
		ReplayWindow:   cfg.ReplayWindow,
		LogHeaders:     cfg.LogHeaders,
	}, sink, cache, logger, stats)

	// The paths are registered without a method so that a wrong method is still
	// audited (and answered with 405) instead of being swallowed by ServeMux.
	base := strings.TrimSuffix(cfg.HookPath, "/")
	mux := http.NewServeMux()
	mux.Handle(base, handler)
	mux.Handle(base+"/{key}", handler)
	mux.Handle("GET /healthz", healthHandler(cfg, stats, sink, cache, audit, detail, started))

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}

	logStartup(logger, cfg, listener.Addr().String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	stopSweeper := startSweeper(ctx, cache, cfg.DedupeTTL, logger)

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown incomplete", "error", err)
	}
	stopSweeper()
	if err := sink.Close(shutdownCtx); err != nil {
		return fmt.Errorf("draining audit queue: %w", err)
	}

	logger.Info("stopped",
		"persisted", stats.Persisted.Load(),
		"dropped", stats.Dropped.Load(),
		"persist_errors", stats.PersistErrors.Load())
	return nil
}

func logStartup(logger *slog.Logger, cfg config.Config, addr string) {
	logger.Info("flowhub receiver starting",
		"version", versionLine(),
		"addr", addr,
		"hook_url", "http://"+addr+strings.TrimSuffix(cfg.HookPath, "/")+"/<url-key>",
		"data_dir", cfg.DataDir,
		"log_file", cfg.ResolvedLogFile(),
		"detail_log", cfg.DetailLog,
		"wildcard_listen_acknowledged", cfg.AllowWildcardListen,
		"active_locks", strings.Join(cfg.ActiveLocks(), ","),
		"max_body_bytes", cfg.MaxBodyBytes,
		"replay_window", cfg.ReplayWindow.String(),
		"dedupe_ttl", cfg.DedupeTTL.String(),
		"queue_size", cfg.QueueSize,
		"log_headers", cfg.LogHeaders)
	for _, warning := range cfg.Warnings() {
		logger.Warn(warning)
	}
}

// startSweeper evicts expired idempotency keys and returns a function that waits
// for the goroutine to finish.
func startSweeper(ctx context.Context, cache *dedupe.Cache, ttl time.Duration, logger *slog.Logger) func() {
	interval := ttl / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	if interval > time.Hour {
		interval = time.Hour
	}

	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if removed := cache.Sweep(); removed > 0 {
					logger.Debug("dedupe sweep", "removed", removed, "retained", cache.Len())
				}
			}
		}
	}()
	return func() { <-done }
}

// healthHandler serves GET /healthz for local operations. nginx never proxies
// this path, so it is not part of the public attack surface.
func healthHandler(cfg config.Config, stats *metrics.Counters, sink *store.Async, cache *dedupe.Cache, audit *store.JSONL, detail *store.Detail, started time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Payload and audit paths are only known after the first record of the
		// day, so they are read per request rather than captured at startup.
		payloadFile := ""
		if detail != nil {
			payloadFile = detail.Path()
		}
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(map[string]any{
			"status":         "ok",
			"version":        versionLine(),
			"uptime_seconds": int64(time.Since(started).Seconds()),
			"addr":           cfg.Addr,
			"data_dir":       cfg.DataDir,
			// Surfaced so a wildcard bind is visible in the operational view, not
			// only in the startup warning.
			"listens_on_all_interfaces": cfg.ListensOnAllInterfaces(),
			"audit_file":                audit.Path(),
			"payload_log":               payloadFile,
			"app_log":                   cfg.ResolvedLogFile(),
			"active_locks":              cfg.ActiveLocks(),
			"queue_depth":               sink.QueueDepth(),
			"queue_capacity":            cfg.QueueSize,
			"dedupe_keys":               cache.Len(),
			"counters":                  stats.Snapshot(),
		})
	}
}

// versionLine renders "version commit built-at" for -version, the startup banner
// and /healthz. Missing pieces are simply omitted.
func versionLine() string {
	parts := []string{Version}

	commit := CommitSHA
	if commit == "" || commit == "unknown" {
		commit = vcsRevision()
	}
	if commit != "" {
		parts = append(parts, commit)
	}
	if BuildTime != "" {
		parts = append(parts, "built "+BuildTime)
	}
	return strings.Join(parts, " ")
}

// vcsRevision reads the revision the Go toolchain embedded when the binary was
// built without ldflags.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 7 {
			return setting.Value[:7]
		}
	}
	return ""
}
