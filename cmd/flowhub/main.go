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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
	agentruntime "github.com/yusiwen/flowhub/internal/agent/opencode"
	"github.com/yusiwen/flowhub/internal/config"
	"github.com/yusiwen/flowhub/internal/dedupe"
	"github.com/yusiwen/flowhub/internal/dispatch"
	"github.com/yusiwen/flowhub/internal/logging"
	"github.com/yusiwen/flowhub/internal/metrics"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/provision"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/runtimes"
	"github.com/yusiwen/flowhub/internal/source"
	"github.com/yusiwen/flowhub/internal/source/youtrack"
	"github.com/yusiwen/flowhub/internal/store"
	"github.com/yusiwen/flowhub/internal/webhook"
	"github.com/yusiwen/flowhub/internal/workspace"
	"github.com/yusiwen/flowhub/internal/workspace/localworktree"
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
		// The build identity travels into the manifest, so a host can tell which
		// binary wrote the files it is running.
		provision.Version = versionLine()
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
	// The compile-time source registry. A configuration file names sources, so a name
	// this binary cannot build has to be refused by name at startup — with the list of
	// what it knows — rather than surfacing later as a delivery that can never be
	// decoded. Adding a source is one Register call plus its package.
	sources := source.NewRegistry()
	if err := sources.Register(youtrack.SourceName, func(policy rules.Policy) source.Source {
		return youtrack.New(policy)
	}); err != nil {
		return err
	}

	// Load the configuration file before anything else touches the filesystem. A
	// missing file is only a warning (phase 1 records without routing), but a file
	// that exists and does not parse or validate must stop the start: a broken
	// mapping is how an agent ends up in the wrong repository.
	projects, projectsErr := projectmap.Load(cfg.ConfigFile)
	if projectsErr != nil && !errors.Is(projectsErr, projectmap.ErrNotFound) {
		return projectsErr
	}
	if problems := projects.Validate(); len(problems) > 0 {
		return fmt.Errorf("refusing to start, configuration %s:\n  - %s",
			cfg.ConfigFile, strings.Join(problems, "\n  - "))
	}
	if problems := projects.SourceProblems(sources.Names()); len(problems) > 0 {
		return fmt.Errorf("refusing to start, configuration %s:\n  - %s",
			cfg.ConfigFile, strings.Join(problems, "\n  - "))
	}
	if problems := projects.RuntimeProblems(); len(problems) > 0 {
		return fmt.Errorf("refusing to start, configuration %s:\n  - %s",
			cfg.ConfigFile, strings.Join(problems, "\n  - "))
	}

	// The source seam: the receiver and the dispatcher take the same adapter, and
	// neither names a tracker. Which adapter this receiver serves is configuration
	// (`FLOWHUB_SOURCE`), and its trigger policy is resolved through the levels below,
	// so the phrase the dispatcher matches can come from the file without the
	// environment forgetting what the operator typed.
	resolved := projects.Policy(cfg.Source, sourcePolicyDefaults(cfg))
	src, err := sources.Build(cfg.Source, resolved.Policy)
	if err != nil {
		return err
	}

	// The runtime inventory is control-plane state: it lives with the audit trail
	// and the task registry, and this process is its only writer. It is opened here,
	// before -print-config returns, because a project may name runtimes and both the
	// report and the startup check have to answer "does that name exist?".
	inventory, err := runtimes.Open(cfg.ResolvedRuntimesFile())
	if err != nil {
		return err
	}

	if *printConfig {
		fmt.Print(cfg.Report())
		fmt.Print(projects.Report())
		// Every effective value is printed with the level that supplied it, which is
		// what makes a three-level file answerable: "why is the trigger this?" has to
		// be a line in the report, not a hunt through the environment and the file.
		for _, value := range projects.Provenance(sourcePolicyDefaults(cfg)) {
			fmt.Printf("effective:          %s %s=%s (level: %s)\n", value.Scope, value.Field, value.Value, value.Level)
		}
		if errors.Is(projectsErr, projectmap.ErrNotFound) {
			fmt.Printf("warning:            no configuration file at %s\n", cfg.ConfigFile)
		}
		for _, problem := range projects.Validate() {
			fmt.Printf("mapping_problem:    %s\n", problem)
		}
		for _, problem := range projects.SourceProblems(sources.Names()) {
			fmt.Printf("source_problem:     %s\n", problem)
		}
		for _, problem := range projects.RuntimeProblems() {
			fmt.Printf("runtime_problem:    %s\n", problem)
		}
		for _, problem := range projects.RuntimeCredentialProblems(os.Getenv) {
			fmt.Printf("credential_problem: %s\n", problem)
		}
		for _, credential := range projects.CredentialReport() {
			fmt.Printf("effective:          %s %s=%s (level: %s)\n",
				credential.Scope, credential.Field, credential.Value, credential.Level)
		}
		if cfg.Dispatch {
			for _, problem := range workspace.ValidateEntries(localworktree.Validator{}, projects.WorkspaceEntries(cfg.WorktreeBase)) {
				fmt.Printf("dispatch_problem:   %s\n", problem)
			}
			problems, warnings := dispatch.RuntimeProblems(projects, inventory)
			for _, warning := range warnings {
				fmt.Printf("runtime_warning:    %s\n", warning)
			}
			for _, problem := range problems {
				fmt.Printf("runtime_problem:    %s\n", problem)
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
	// checked here, before anything is created — now in two halves: what the
	// configuration says about the work (above) and what this host can actually do
	// with it (the workspace provider's own checks).
	if cfg.Dispatch {
		if errors.Is(projectsErr, projectmap.ErrNotFound) {
			return fmt.Errorf("FLOWHUB_DISPATCH=1 but no configuration file exists at %s: every delivery would be refused", cfg.ConfigFile)
		}
		if problems := workspace.ValidateEntries(localworktree.Validator{}, projects.WorkspaceEntries(cfg.WorktreeBase)); len(problems) > 0 {
			return fmt.Errorf("refusing to start, dispatch is not possible:\n  - %s", strings.Join(problems, "\n  - "))
		}
		if problems := declaredAuthProblems(projects); len(problems) > 0 {
			return fmt.Errorf("refusing to start, configuration %s:\n  - %s", cfg.ConfigFile, strings.Join(problems, "\n  - "))
		}
		if problems := configFileSecretProblems(cfg.ConfigFile, projects); len(problems) > 0 {
			return fmt.Errorf("refusing to start, configuration %s:\n  - %s", cfg.ConfigFile, strings.Join(problems, "\n  - "))
		}
		if problems, _ := dispatch.RuntimeProblems(projects, inventory); len(problems) > 0 {
			return fmt.Errorf("refusing to start, a project names a runtime that cannot take work:\n  - %s", strings.Join(problems, "\n  - "))
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
			"path", cfg.ConfigFile, "hint", "copy config/config.example.json and set FLOWHUB_CONFIG_FILE")
	} else {
		logger.Info("project mapping loaded", "path", cfg.ConfigFile, "mappings", projects.Len(), "routable", projects.Routable(), "keys", strings.Join(projects.Keys(), ","))
	}

	// Cancelled on SIGINT/SIGTERM and on a fatal serve error, so the dispatcher
	// stops taking work before the audit queue is drained.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if inventory.Len() > 0 {
		logger.Info("runtime inventory loaded",
			"path", inventory.Path(), "runtimes", inventory.Len(), "counts", inventory.Counts())
	}

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
	dispatcher, dispatchDone, err := startDispatcher(ctx, cfg, projects, inventory, src, logger)
	if err != nil {
		return err
	}

	// The control API is the only authenticated write surface in this process, and
	// it is opt-in: an operator who does not enrol hosts from here never exposes
	// it. It reads and writes the inventory the dispatcher is already using, so a
	// change applies without a restart.
	adminServer, err := startControlAPI(ctx, cfg, projects, inventory, dispatcher, logger)
	if err != nil {
		return err
	}

	handler := webhook.New(webhookOptions(cfg, dispatcher, src), sink, cache, logger, stats)

	// The paths are registered without a method so that a wrong method is still
	// audited (and answered with 405) instead of being swallowed by ServeMux.
	base := strings.TrimSuffix(cfg.HookPath, "/")
	mux := http.NewServeMux()
	mux.Handle(base, handler)
	mux.Handle(base+"/{key}", handler)
	mux.Handle("GET /healthz", healthHandler(cfg, projects, stats, sink, cache, audit, detail, dispatcher, inventory, started))

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

	// Stop the control API first: it must not accept an enrolment while the
	// process is winding down.
	if adminServer != nil {
		if err := adminServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("control API shutdown incomplete", "error", err)
		}
	}

	// Stop the dispatcher next: no new turn may start while the process is
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
func webhookOptions(cfg config.Config, dispatcher *dispatch.Dispatcher, src source.Source) webhook.Options {
	opts := webhook.Options{
		Source:         src,
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

// sourcePolicyDefaults is the outermost level of a source's trigger policy: the
// environment. A `sources.<name>.policy` block in the configuration file overrides
// whatever this names, and the adapter's own defaults fill the rest.
func sourcePolicyDefaults(cfg config.Config) projectmap.PolicyDefaults {
	return projectmap.PolicyDefaults{
		Trigger:             cfg.Trigger,
		StartStates:         cfg.StartStates,
		SkipAnalyzeOnCreate: cfg.SkipAnalyzeOnCreate,
		MaxTurns:            cfg.MaxTurns,
	}
}

// newRuntimeFactory returns the constructor the dispatcher uses to build one
// runtime adapter per host.
//
// It is the single place in the binary that names an agent product, and the only
// place that hands a runtime its credentials: everything above the seam passes an
// agent.Runtime around and never learns which product is behind it (ADR 0001).
// The dispatcher needs a factory rather than one runtime because it learns a host's
// address at routing time — the inventory can change while the process runs.
//
// One HTTP request is bounded by the client, the turn by its own deadline; the
// liveness probes bound themselves with a much shorter context.
func newRuntimeFactory(cfg config.Config, projects *projectmap.Map, logger *slog.Logger) func(name, url string) agent.Runtime {
	return func(name, url string) agent.Runtime {
		// One resolution path for every caller: the runtime's own `auth` block (an
		// inline password, a variable name, or a password file), then the shared
		// credentials file, then the environment pair. A runtime-specific declaration
		// deliberately wins over the process-wide default, which is the opposite of
		// what a silent override would do.
		credential, _ := projects.ResolveCredential(name, os.Getenv)
		return agentruntime.NewRuntime(
			agentruntime.New(agentruntime.Options{
				BaseURL:  url,
				Username: credential.User,
				Password: credential.Password,
				Timeout:  runtimeDeadline(cfg, projects, name),
			}),
			agentruntime.RuntimeOptions{Name: name, Log: logger, FirstResponse: cfg.FirstResponse},
		)
	}
}

// runtimeDeadline resolves how long one turn on a host may run: the environment is
// the outermost default and a `runtimes.<name>.deadline` block overrides it.
func runtimeDeadline(cfg config.Config, projects *projectmap.Map, name string) time.Duration {
	if block, ok := projects.RuntimeBlock(name); ok {
		if deadline := strings.TrimSpace(block.Deadline); deadline != "" {
			if parsed, err := time.ParseDuration(deadline); err == nil {
				return parsed
			}
		}
	}
	return cfg.TaskDeadline
}

// declaredAuthProblems reports a `runtimes.<name>.auth` block whose password cannot
// be resolved from the level that block names: an environment variable that is empty,
// a password file that is missing or too permissive, or a credentials file with no
// entry for this runtime. Half a credential pair fails every turn, late, so it
// refuses the start — the same gate the environment-only version enforced.
//
// A block that names more than one password source, or a credentials file that
// cannot be read, is reported by RuntimeCredentialProblems; both are consulted here
// so a start is refused whichever way the problem is shaped.
func declaredAuthProblems(projects *projectmap.Map) []string {
	return projects.RuntimeCredentialProblems(os.Getenv)
}

// configFileSecretProblems enforces the configuration file's own permissions, but
// only when that file holds a secret.
//
// `runtimes.<name>.auth.password` writes the password into the file the operator
// copies between machines and may commit; a group- or world-readable file then
// leaks it. A file that only names variables and paths carries no secret, and
// refusing it would break every existing deployment for no gain — so the check is
// conditional rather than blanket.
//
// An unreadable file is reported as unreadable, never as "no secret inside": the two
// need different fixes.
func configFileSecretProblems(path string, projects *projectmap.Map) []string {
	if !projects.HasInlinePassword() {
		return nil
	}
	if err := projectmap.CheckSecretPermissions("configuration file", path); err != nil {
		return []string{fmt.Sprintf(
			"%v; this file holds an inline `password`, so it is itself a secret — move the password to `password_env`, `password_file` or `credentials_file` to drop this requirement", err)}
	}
	return nil
}

// declaredRuntimes renders the file's `runtimes` table for the dispatcher.
//
// A block answers two different questions and either may be absent. A block *with*
// a URL declares a runtime that exists without being enrolled — the single-host case
// FLOWHUB_OPENCODE_URL used to be the only way to express. A block *without* a URL
// declares policy for a runtime the inventory enrols, which is where an enrolled
// host's address comes from; dropping those blocks is how a `deadline` or `agent`
// written in the file silently stopped applying (measured live on 2026-09-26, fixed
// the same run). Both are passed through, and the dispatcher adds a declared runtime
// as a candidate only when it has an address of its own.
func declaredRuntimes(cfg config.Config, projects *projectmap.Map) []dispatch.DeclaredRuntime {
	out := make([]dispatch.DeclaredRuntime, 0, len(projects.Runtimes()))
	for _, name := range projects.Runtimes() {
		block, _ := projects.RuntimeBlock(name)
		out = append(out, dispatch.DeclaredRuntime{
			Name:          name,
			URL:           strings.TrimSpace(block.URL),
			Agent:         strings.TrimSpace(block.Agent),
			Model:         strings.TrimSpace(block.Model),
			Deadline:      runtimeDeadline(cfg, projects, name),
			MaxConcurrent: block.MaxConcurrentTasks(),
		})
	}
	return out
}

// opencodeProber decides whether a host that just claimed a name is usable.
//
// Answering /global/health is not enough. opencode accepts a session for an agent
// it does not have and falls back to its own default — a looser agent with a
// different step budget and permission block — so activation also requires the
// profile the host claimed to exist on *that* server, and the model the host
// reported to be one the server offers. Those are the checks `init --check` runs
// locally, repeated from the side that will actually send the turns.
//
// It speaks opencode's own API rather than the agent seam, deliberately: the agent
// registry and the model catalogue are what *this product* exposes, and a capability
// check that pretended to be runtime-neutral would have to invent a speculative
// interface for them. The dispatcher, which must stay neutral, does not use this.
type opencodeProber struct {
	cfg      config.Config
	projects *projectmap.Map
	timeout  time.Duration
	log      *slog.Logger
}

func (p opencodeProber) Probe(ctx context.Context, claim runtimes.Claim) error {
	timeout := p.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	// The credential is resolved per claimed runtime, through the same function the
	// dispatcher's factory uses: an enrolled host may have its own `auth` block, its
	// own entry in the credentials file, or fall back to the environment pair. Two
	// independent lookups is how a check passes against one pair while the turn later
	// fails against another.
	credential, _ := p.projects.ResolveCredential(claim.Name, os.Getenv)
	client := agentruntime.New(agentruntime.Options{
		BaseURL:  claim.Advertise,
		Username: credential.User,
		Password: credential.Password,
		Timeout:  timeout,
	})
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := client.Health(probeCtx); err != nil {
		return err
	}

	profile := strings.TrimSpace(claim.AgentProfile)
	if profile == "" {
		// A host that claims no profile is judged by liveness alone: there is nothing
		// to compare against the server.
		return nil
	}
	agents, err := client.Agents(probeCtx)
	if err != nil {
		return fmt.Errorf("the host answered but its agent list could not be read: %w", err)
	}
	var claimed *agentruntime.AgentInfo
	for index := range agents {
		if agents[index].Name == profile {
			claimed = &agents[index]
			break
		}
	}
	if claimed == nil {
		return fmt.Errorf("the host answers but has no agent named %q, so every turn would run under the server's default agent instead; install the agent files there (`flowhub runtime init`) and restart its agent server", profile)
	}

	model := strings.TrimSpace(claim.Models[profile])
	if model == "" {
		return nil
	}
	provider, id, ok := agent.SplitModel(model)
	if !ok {
		return fmt.Errorf("the host reported model %q for agent %s, which is not spelled provider/model", model, profile)
	}
	catalogue, err := client.Catalogue(probeCtx)
	if err != nil {
		return fmt.Errorf("the host answered but its model catalogue could not be read: %w", err)
	}
	if !slices.Contains(catalogue[provider], id) {
		return fmt.Errorf("this host's agent server does not offer model %s: provider %s offers %s",
			model, provider, orNone(catalogue[provider]))
	}
	if effective := claimed.ModelString(); effective != "" && effective != model && p.log != nil {
		// Not fatal — the dispatcher pins the model the host reported, so the turn
		// uses the repaired one — but the server is answering from a registry it built
		// before the profile was installed, and that is worth saying out loud.
		p.log.Warn("the agent server still reports an older model for this agent; restart it to pick the profile up",
			"runtime", claim.Name, "agent", profile, "server_model", effective, "reported_model", model)
	}
	return nil
}

// orNone renders a list for an error message, so an unknown provider does not
// produce a trailing "offers: ".
func orNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

// startControlAPI starts the control listener, or returns (nil, nil) when it is
// not configured.
func startControlAPI(ctx context.Context, cfg config.Config, projects *projectmap.Map, inventory *runtimes.Inventory, dispatcher *dispatch.Dispatcher, logger *slog.Logger) (*http.Server, error) {
	if !cfg.ControlAPIEnabled() {
		logger.Info("control API is off: runtimes are configured through the environment only",
			"hint", "set FLOWHUB_ADMIN_ADDR and FLOWHUB_ADMIN_TOKEN to invite and enrol agent hosts")
		return nil, nil
	}
	boundTasks := func(string) []string { return nil }
	if dispatcher != nil {
		boundTasks = dispatcher.BoundTasks
	}
	control := &runtimes.Server{
		Inventory:  inventory,
		AdminToken: cfg.AdminToken,
		Prober:     opencodeProber{cfg: cfg, projects: projects, timeout: 5 * time.Second, log: logger},
		BoundTasks: boundTasks,
		Log:        logger,
	}
	server := &http.Server{
		Handler:           control.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	listener, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		return nil, fmt.Errorf("control API: listen on %s: %w", cfg.AdminAddr, err)
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("control API stopped", "error", err)
		}
	}()
	logger.Info("control API listening", "addr", listener.Addr().String(),
		"runtimes_file", inventory.Path(), "wildcard", cfg.ControlAPIListensOnAllInterfaces())
	return server, nil
}

// startDispatcher builds and starts the opencode dispatcher, or returns (nil,
// nil, nil) when FLOWHUB_DISPATCH is off.
//
// The returned channel closes when the worker has stopped, so shutdown can wait
// for an in-flight turn instead of killing the process under it.
func startDispatcher(ctx context.Context, cfg config.Config, projects *projectmap.Map, inventory *runtimes.Inventory, src source.Source, logger *slog.Logger) (*dispatch.Dispatcher, <-chan struct{}, error) {
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

	client := agentruntime.New(agentruntime.Options{
		BaseURL:  cfg.OpenCodeURL,
		Username: cfg.OpenCodeUser,
		Password: cfg.OpenCodePassword,
		Timeout:  cfg.TaskDeadline,
	})
	defaultRuntime := agentruntime.NewRuntime(client, agentruntime.RuntimeOptions{
		Name: dispatch.DefaultRuntimeName, Log: logger, FirstResponse: cfg.FirstResponse,
	})
	factory := newRuntimeFactory(cfg, projects, logger)
	// Refuse to start when nothing can serve a turn: a dispatcher that accepts
	// deliveries and then fails every one of them is worse than a refused start.
	// With an inventory the rule becomes "at least one runtime answers", because
	// the point of several hosts is that one may be down.
	if err := checkRuntimesReachable(ctx, cfg, inventory, defaultRuntime, factory, logger); err != nil {
		return nil, nil, err
	}

	tasks, err := registry.Open(cfg.ResolvedRegistryFile())
	if err != nil {
		return nil, nil, err
	}

	dispatcher, err := dispatch.New(dispatch.Options{
		Runtimes:       inventory,
		NewRuntime:     factory,
		DefaultRuntime: defaultRuntime,
		Declared:       declaredRuntimes(cfg, projects),
		NewWorkspace: func(base string) (workspace.Workspace, error) {
			// The co-located provider: FlowHub and the runtime share a filesystem, so
			// a task's directory is a git worktree this process created.
			return localworktree.New(localworktree.Options{Base: base})
		},
		Registry: tasks,
		Projects: projects,
		// One policy, owned by the source: what the dispatcher decides on and what
		// the prompt tells the maintainer to type cannot drift apart.
		Source:         src,
		Log:            logger,
		WorktreeBase:   cfg.WorktreeBase,
		Agent:          cfg.DispatchAgent,
		Deadline:       cfg.TaskDeadline,
		FirstResponse:  cfg.FirstResponse,
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

// checkRuntimesReachable probes every runtime the dispatcher could use, warns about
// each one that does not answer, and fails only when none does.
func checkRuntimesReachable(ctx context.Context, cfg config.Config, inventory *runtimes.Inventory, fallback agent.Runtime, factory func(name, url string) agent.Runtime, logger *slog.Logger) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	active := inventory.Active()
	if len(active) == 0 {
		version, err := fallback.Health(probeCtx)
		if err != nil {
			return fmt.Errorf("FLOWHUB_DISPATCH=1 but opencode at %s is not answering: %w", cfg.OpenCodeURL, err)
		}
		logger.Info("opencode is reachable", "url", cfg.OpenCodeURL, "version", version, "runtime", dispatch.DefaultRuntimeName)
		return nil
	}

	var reachable []string
	for _, runtime := range active {
		address := strings.TrimSpace(runtime.Advertise)
		if address == "" {
			address = strings.TrimSpace(runtime.URL)
		}
		if address == "" {
			logger.Warn("a runtime has no address; it cannot be used", "runtime", runtime.Name)
			continue
		}
		runtimeCtx, runtimeCancel := context.WithTimeout(probeCtx, 5*time.Second)
		version, err := factory(runtime.Name, address).Health(runtimeCtx)
		runtimeCancel()
		if err != nil {
			logger.Warn("runtime is not answering; new work will avoid it", "runtime", runtime.Name, "url", address, "error", err)
			continue
		}
		logger.Info("runtime is reachable", "runtime", runtime.Name, "url", address, "version", version)
		reachable = append(reachable, runtime.Name)
	}
	if len(reachable) == 0 {
		return fmt.Errorf("FLOWHUB_DISPATCH=1 but none of the %d enrolled runtime(s) answered; the hosts or the network between them need attention", len(active))
	}
	return nil
}

func logStartup(logger *slog.Logger, cfg config.Config, projects *projectmap.Map, addr string) {
	logger.Info("flowhub receiver starting",
		"version", versionLine(),
		"addr", addr,
		"hook_url", "http://"+addr+strings.TrimSuffix(cfg.HookPath, "/")+"/<url-key>",
		"data_dir", cfg.DataDir,
		"log_file", cfg.ResolvedLogFile(),
		"detail_log", cfg.DetailLog,
		"config_file", cfg.ConfigFile,
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
		"max_turns", cfg.MaxTurns,
		"control_api", cfg.ControlAPIEnabled(),
		"control_api_addr", cfg.AdminAddr)
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
func healthHandler(cfg config.Config, projects *projectmap.Map, stats *metrics.Counters, sink *store.Async, cache *dedupe.Cache, audit *store.JSONL, detail *store.Detail, dispatcher *dispatch.Dispatcher, inventory *runtimes.Inventory, started time.Time) http.HandlerFunc {
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
			"projects":                  map[string]any{"file": cfg.ConfigFile, "mappings": projects.Len(), "routable": projects.Routable(), "keys": projects.Keys()},
			"dispatch":                  dispatchState,
			"runtimes":                  runtimeHealth(inventory),
			"control_api":               controlAPIHealth(cfg),
			"counters":                  stats.Snapshot(),
		})
	}
}

// runtimeHealth reports the inventory, including the enrolled runtimes so a
// registration is visible here without a restart.
func runtimeHealth(inventory *runtimes.Inventory) map[string]any {
	if inventory == nil {
		return map[string]any{"enabled": false}
	}
	runtimes_ := inventory.List()
	names := make([]string, 0, len(runtimes_))
	states := map[string]string{}
	for _, runtime := range runtimes_ {
		names = append(names, runtime.Name)
		states[runtime.Name] = string(runtime.State)
	}
	return map[string]any{
		"enabled":  true,
		"file":     inventory.Path(),
		"counts":   inventory.Counts(),
		"runtimes": names,
		"states":   states,
	}
}

// controlAPIHealth describes the control API without leaking its token.
func controlAPIHealth(cfg config.Config) map[string]any {
	if !cfg.ControlAPIEnabled() {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":  true,
		"addr":     cfg.AdminAddr,
		"wildcard": cfg.ControlAPIListensOnAllInterfaces(),
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
