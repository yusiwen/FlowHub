// Command flowhub runs the FlowHub orchestration hub.
//
// Two modes, one process:
//
//   - the receiver, always on: accept YouTrack webhook deliveries, enforce the
//     three entry locks, deduplicate, audit every delivery and answer 202
//     immediately;
//   - the dispatcher, opt-in through FLOWHUB_DISPATCH=1: turn an accepted
//     delivery into one opencode turn inside a per-task git worktree.
//
// Nothing on the receiving path ever waits for opencode; see the design documents
// in the repository root.
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
	"github.com/yusiwen/flowhub/internal/dispatch"
	"github.com/yusiwen/flowhub/internal/logging"
	"github.com/yusiwen/flowhub/internal/metrics"
	"github.com/yusiwen/flowhub/internal/opencode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/provision"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
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
	// A subcommand is dispatched before anything else, and in particular before
	// the receiver configuration is loaded. `flowhub runtime init` runs on a
	// machine that has none of the receiver's environment variables, and it must
	// not fail because they are absent. The exit status is the subcommand's: a
	// capability check is a gate, so "not ready" has to be distinguishable from
	// "you typed it wrong".
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(provision.Main(os.Args[1], os.Args[2:], os.Stdout, os.Stderr))
	}
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
	// Load the routing table before anything else touches the filesystem. A
	// missing file is only a warning (phase 1 records without routing), but a file
	// that exists and does not parse or validate must stop the start: a broken
	// mapping is how an agent ends up in the wrong repository.
	projects, projectsErr := projectmap.Load(cfg.ProjectsFile)
	if projectsErr != nil && !errors.Is(projectsErr, projectmap.ErrNotFound) {
		return projectsErr
	}
	if problems := projects.Validate(); len(problems) > 0 {
		return fmt.Errorf("refusing to start, project mapping %s:\n  - %s",
			cfg.ProjectsFile, strings.Join(problems, "\n  - "))
	}

	if *printConfig {
		fmt.Print(cfg.Report())
		fmt.Print(projects.Report())
		if errors.Is(projectsErr, projectmap.ErrNotFound) {
			fmt.Printf("warning:            no project mapping file at %s\n", cfg.ProjectsFile)
		}
		for _, problem := range projects.Validate() {
			fmt.Printf("mapping_problem:    %s\n", problem)
		}
		if cfg.Dispatch {
			for _, problem := range dispatch.Problems(projects, cfg.WorktreeBase) {
				fmt.Printf("dispatch_problem:   %s\n", problem)
			}
		}
		return nil
	}
	// Refuse to start on configuration that would silently widen the attack
	// surface (a wildcard bind without an explicit acknowledgement). Checked
	// before any file is created so a refused start leaves nothing behind.
	if problems := cfg.Problems(); len(problems) > 0 {
		return fmt.Errorf("refusing to start:\n  - %s", strings.Join(problems, "\n  - "))
	}
	// A dispatcher that cannot reach a repository would fail one issue at a time
	// while the operator believes the hub is running, so the same mapping is
	// checked here, before anything is created.
	if cfg.Dispatch {
		if errors.Is(projectsErr, projectmap.ErrNotFound) {
			return fmt.Errorf("FLOWHUB_DISPATCH=1 but no project mapping exists at %s: every delivery would be refused", cfg.ProjectsFile)
		}
		if problems := dispatch.Problems(projects, cfg.WorktreeBase); len(problems) > 0 {
			return fmt.Errorf("refusing to start, dispatch is not possible:\n  - %s", strings.Join(problems, "\n  - "))
		}
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

	if errors.Is(projectsErr, projectmap.ErrNotFound) {
		logger.Warn("no project mapping configured: deliveries are recorded but never routed",
			"path", cfg.ProjectsFile, "hint", "copy config/config.example.json and set FLOWHUB_PROJECTS_FILE")
	} else {
		logger.Info("project mapping loaded", "path", cfg.ProjectsFile, "mappings", projects.Len(), "routable", projects.Routable(), "keys", strings.Join(projects.Keys(), ","))
	}

	// Cancelled on SIGINT/SIGTERM and on a fatal serve error, so the dispatcher
	// stops taking work before the audit queue is drained.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	// The dispatcher is built before the handler so that a delivery can never be
	// accepted before there is something to hand it to.
	dispatcher, dispatchDone, err := startDispatcher(ctx, cfg, projects, logger)
	if err != nil {
		return err
	}

	handler := webhook.New(webhookOptions(cfg, dispatcher), sink, cache, logger, stats)

	// The paths are registered without a method so that a wrong method is still
	// audited (and answered with 405) instead of being swallowed by ServeMux.
	base := strings.TrimSuffix(cfg.HookPath, "/")
	mux := http.NewServeMux()
	mux.Handle(base, handler)
	mux.Handle(base+"/{key}", handler)
	mux.Handle("GET /healthz", healthHandler(cfg, projects, stats, sink, cache, audit, detail, dispatcher, started))

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

	logStartup(logger, cfg, projects, listener.Addr().String())

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

	// Stop the dispatcher first: no new turn may start while the process is
	// shutting down. A turn already in flight sees the cancelled context and
	// unwinds; the wait is bounded so a stuck session cannot block the exit.
	if stopAndWait := stopDispatcher(stop, dispatchDone, shutdownCtx, logger); stopAndWait != nil {
		stopAndWait()
	}
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

// webhookOptions assembles the receiver options, attaching the dispatcher only
// when there is one.
//
// The "only when there is one" part is load bearing: webhook.Dispatcher is an
// interface, and a typed nil pointer stored in an interface is not == nil, so
// assigning a nil *dispatch.Dispatcher would make the receiver call
// Dispatch on a nil receiver. Measured: with FLOWHUB_DISPATCH off, every
// delivery was answered with an empty reply because that panic happened after
// the 202 was written.
func webhookOptions(cfg config.Config, dispatcher *dispatch.Dispatcher) webhook.Options {
	opts := webhook.Options{
		HookPath:       cfg.HookPath,
		HookKey:        cfg.HookKey,
		TokenHeader:    cfg.TokenHeader,
		Token:          cfg.Token,
		AllowedSources: cfg.AllowedSources,
		MaxBodyBytes:   cfg.MaxBodyBytes,
		ReplayWindow:   cfg.ReplayWindow,
		LogHeaders:     cfg.LogHeaders,
	}
	if dispatcher != nil {
		opts.Dispatcher = dispatcher
	}
	return opts
}

// startDispatcher builds and starts the opencode dispatcher, or returns (nil,
// nil, nil) when FLOWHUB_DISPATCH is off.
//
// The returned channel closes when the worker has stopped, so shutdown can wait
// for an in-flight turn instead of killing the process under it.
func startDispatcher(ctx context.Context, cfg config.Config, projects *projectmap.Map, logger *slog.Logger) (*dispatch.Dispatcher, <-chan struct{}, error) {
	if !cfg.Dispatch {
		return nil, nil, nil
	}

	// The pause file is the kill switch, and a start with it already present is
	// almost certainly a leftover from an incident: say so loudly rather than
	// silently not working.
	if _, err := os.Stat(cfg.ResolvedPauseFile()); err == nil {
		logger.Warn("the dispatch pause file exists, so no work will be dispatched until it is removed",
			"pause_file", cfg.ResolvedPauseFile())
	}

	client := opencode.New(opencode.Options{BaseURL: cfg.OpenCodeURL})
	// Refuse to start when opencode is not answering: a dispatcher that accepts
	// deliveries and then fails every turn is worse than a refused start.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	health, err := client.Health(probeCtx)
	if err != nil {
		return nil, nil, fmt.Errorf("FLOWHUB_DISPATCH=1 but opencode at %s is not answering: %w", cfg.OpenCodeURL, err)
	}
	logger.Info("opencode is reachable", "url", cfg.OpenCodeURL, "version", health.Version)

	tasks, err := registry.Open(cfg.ResolvedRegistryFile())
	if err != nil {
		return nil, nil, err
	}

	dispatcher, err := dispatch.New(dispatch.Options{
		Client:   client,
		Registry: tasks,
		Projects: projects,
		Rules: rules.Policy{
			Trigger:             cfg.Trigger,
			StartStates:         cfg.StartStates,
			SkipAnalyzeOnCreate: cfg.SkipAnalyzeOnCreate,
			MaxTurns:            cfg.MaxTurns,
		},
		Log:            logger,
		WorktreeBase:   cfg.WorktreeBase,
		Agent:          cfg.DispatchAgent,
		Deadline:       cfg.TaskDeadline,
		QueueSize:      cfg.DispatchQueueSize,
		PauseFile:      cfg.ResolvedPauseFile(),
		MaxCostPerTask: cfg.TaskMaxCost,
	})
	if err != nil {
		return nil, nil, err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatcher.Run(ctx)
	}()
	return dispatcher, done, nil
}

// stopDispatcher returns a function that cancels the dispatcher and waits for it
// to stop. It returns nil when there is no dispatcher, so the caller can call the
// result unconditionally.
func stopDispatcher(stop context.CancelFunc, done <-chan struct{}, ctx context.Context, logger *slog.Logger) func() {
	if done == nil {
		return nil
	}
	return func() {
		stop()
		select {
		case <-done:
		case <-ctx.Done():
			logger.Warn("dispatcher did not stop before the shutdown deadline; a turn is still in flight")
		}
	}
}

func logStartup(logger *slog.Logger, cfg config.Config, projects *projectmap.Map, addr string) {
	logger.Info("flowhub receiver starting",
		"version", versionLine(),
		"addr", addr,
		"hook_url", "http://"+addr+strings.TrimSuffix(cfg.HookPath, "/")+"/<url-key>",
		"data_dir", cfg.DataDir,
		"log_file", cfg.ResolvedLogFile(),
		"detail_log", cfg.DetailLog,
		"projects_file", cfg.ProjectsFile,
		"project_mappings", projects.Len(),
		"project_routable", projects.Routable(),
		"wildcard_listen_acknowledged", cfg.AllowWildcardListen,
		"active_locks", strings.Join(cfg.ActiveLocks(), ","),
		"max_body_bytes", cfg.MaxBodyBytes,
		"replay_window", cfg.ReplayWindow.String(),
		"dedupe_ttl", cfg.DedupeTTL.String(),
		"queue_size", cfg.QueueSize,
		"log_headers", cfg.LogHeaders,
		"dispatch", cfg.Dispatch,
		"opencode_url", cfg.OpenCodeURL,
		"dispatch_agent", cfg.DispatchAgent,
		"trigger", cfg.Trigger,
		"start_states", strings.Join(cfg.StartStates, ","),
		"task_deadline", cfg.TaskDeadline.String(),
		"max_turns", cfg.MaxTurns)
	for _, warning := range cfg.Warnings() {
		logger.Warn(warning)
	}
	if !cfg.Dispatch {
		// Named explicitly because "nothing happens when I create an issue" is
		// the confusing failure mode of a receiver that only records.
		logger.Info("dispatcher is off: deliveries are recorded and audited, but no opencode turn is started",
			"hint", "set FLOWHUB_DISPATCH=1 to let accepted deliveries reach opencode")
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
func healthHandler(cfg config.Config, projects *projectmap.Map, stats *metrics.Counters, sink *store.Async, cache *dedupe.Cache, audit *store.JSONL, detail *store.Detail, dispatcher *dispatch.Dispatcher, started time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Payload and audit paths are only known after the first record of the
		// day, so they are read per request rather than captured at startup.
		payloadFile := ""
		if detail != nil {
			payloadFile = detail.Path()
		}
		// Dispatch is reported even when it is off, so "no opencode turn is
		// happening" is visible here instead of only in the startup log.
		dispatchState := map[string]any{"enabled": false}
		if dispatcher != nil {
			dispatchState = dispatcher.Snapshot()
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
			"projects":                  map[string]any{"file": cfg.ProjectsFile, "mappings": projects.Len(), "routable": projects.Routable(), "keys": projects.Keys()},
			"dispatch":                  dispatchState,
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
	// `git describe --always --dirty` already yields the short sha while the
	// repository has no tags, so appending it again would print it twice.
	if commit != "" && !strings.Contains(Version, commit) {
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
