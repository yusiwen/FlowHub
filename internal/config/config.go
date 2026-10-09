// Package config resolves the FlowHub receiver configuration from the
// environment.
//
// Every knob has a safe default and no secret is ever defaulted. The receiver is
// meant to sit behind nginx and WireGuard (see the design documents in the
// repository root), so the default listen address is loopback rather than
// 0.0.0.0.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults for the YouTrack webhook receiver.
const (
	DefaultAddr          = "127.0.0.1:8080"
	DefaultHookPath      = "/hooks/youtrack"
	DefaultTokenHeader   = "X-YouTrack-Token"
	DefaultDataDir       = "./data"
	DefaultMaxBodyBytes  = 256 << 10 // 256 KiB; mirrors nginx client_max_body_size
	DefaultReplayWindow  = 10 * time.Minute
	DefaultDedupeTTL     = 24 * time.Hour
	DefaultQueueSize     = 1024
	DefaultShutdown      = 10 * time.Second
	DefaultLogLevel      = "info"
	DefaultLogFormat     = "text"
	DefaultLogFile       = "flowhub.log"
	DefaultLogMaxBytes   = 32 << 20
	DefaultLogMaxFiles   = 5
	DefaultDetailMaxBody = 64 << 10

	// DefaultProjectsDir and DefaultProjectsName locate the routing table inside
	// the XDG config directory.
	DefaultProjectsDir  = "flowhub"
	DefaultProjectsName = "config.json"

	// Defaults for the opencode dispatcher. Dispatch itself defaults to OFF: an
	// unattended agent that edits repositories has to be an explicit decision,
	// and an unset FLOWHUB_DISPATCH leaves the receiver recording only.
	DefaultOpenCodeURL   = "http://127.0.0.1:4096"
	DefaultDispatchAgent = "devops"
	DefaultDispatchQueue = 32
	DefaultTaskDeadline  = 15 * time.Minute
	// DefaultFirstResponse bounds how long a turn may take to produce its first
	// assistant message. It is far below the task deadline on purpose: the failure
	// it catches (a prompt the agent server never admits) is immediate, and a turn
	// that never starts otherwise occupies the only worker until the deadline.
	DefaultFirstResponse = 90 * time.Second
	DefaultMaxTurns      = 8
	// DefaultSourceName is the source a process serves when FLOWHUB_SOURCE is unset.
	// It matches the only adapter this binary registers today.
	DefaultSourceName = "youtrack"

	DefaultTrigger    = "/opencode start"
	DefaultStartState = "In Progress"

	// DefaultAdminAddr is the control API listener. It is not started unless
	// the address is set: an always-on control surface would either be
	// unauthenticated or break every existing deployment, and the default posture
	// of this process is to expose no control surface at all.
	DefaultAdminAddr = ""

	// DefaultRuntimesName is the runtime inventory, which lives with the audit
	// trail and the task registry because it is control-plane state.
	DefaultRuntimesName = "runtimes.json"

	// DefaultRegistryName and DefaultPauseName live inside DataDir. The pause
	// file is the kill switch: touching it stops dispatch without an API, a
	// restart or a credential, which is the only kind of switch that works
	// during an incident.
	DefaultRegistryName = "registry.jsonl"
	DefaultPauseName    = "DISPATCH_OFF"
)

// Config is the fully resolved receiver configuration.
type Config struct {
	// Addr is the TCP listen address. Bind the WireGuard address (or loopback
	// behind a reverse proxy), never 0.0.0.0.
	Addr string

	// HookPath is the base path of the webhook endpoint; the URL embedded
	// secret is appended as a final path segment.
	HookPath string

	// HookKey is the URL embedded shared secret (lock 1). Empty disables it.
	HookKey string

	// TokenHeader is the header carrying the shared token (lock 2).
	TokenHeader string

	// Token is the expected header value (lock 2). Empty disables it.
	Token string

	// AllowedSources restricts accepted source IPs/CIDRs (lock 3). Empty
	// disables it.
	AllowedSources []*net.IPNet

	// DataDir holds the append-only audit log.
	DataDir string

	// MaxBodyBytes caps the request body.
	MaxBodyBytes int64

	// ReplayWindow rejects payloads whose timestamp is further away than this
	// from local time. Zero disables the check.
	ReplayWindow time.Duration

	// DedupeTTL is how long an idempotency key is remembered.
	DedupeTTL time.Duration

	// QueueSize is the audit queue depth between the HTTP handler and the
	// writer goroutine.
	QueueSize int

	// LogHeaders records the received request headers (names and values, with
	// sensitive values masked) plus a token fingerprint in the audit log. The raw
	// token value is never recorded.
	LogHeaders bool

	// AllowWildcardListen acknowledges a bind on every interface (":8080",
	// "0.0.0.0:8080", "[::]:8080"). It is required for container images, where the
	// process must listen on the pod address, and it is the only way to permit a
	// wildcard bind: without it such an address is a startup error.
	AllowWildcardListen bool

	// LogFile is the application log file. Empty resolves to
	// <DataDir>/flowhub.log; "-" or "none" disables file logging.
	LogFile string

	// LogMaxBytes rotates the application log at this size.
	LogMaxBytes int64

	// LogMaxFiles is how many rotated application log archives to keep.
	LogMaxFiles int

	// DetailLog enables the human readable per-delivery payload log.
	DetailLog bool

	// DetailLogMaxBodyBytes caps how much of a body is pretty printed there.
	DetailLogMaxBodyBytes int

	// ConfigFile is the FlowHub configuration file: which sources exist, what
	// starting work means for each, which runtimes are declared, and the
	// project -> repository table. A missing
	// file only warns (phase 1 records without routing); a file that exists but
	// does not parse or validate is a startup error.
	//
	// Load resolves it to an absolute path, so the logs, the loader and
	// -print-config always name the file that was actually read.
	ConfigFile string

	// Source names the event source this receiver serves, e.g. "youtrack". It selects
	// which registered adapter decodes deliveries and which `sources.<name>` block
	// supplies the trigger policy, so a process handling Gitea and one handling
	// YouTrack differ by this variable and not by a rebuild.
	Source string

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// LogFormat is "text" or "json".
	LogFormat string

	// ShutdownTimeout bounds the graceful drain on SIGINT/SIGTERM.
	ShutdownTimeout time.Duration

	// --- Phase 2: the opencode dispatcher ---------------------------------

	// Dispatch turns on the dispatcher. It is off by default so the receiver
	// can run (and be audited) without an agent that edits repositories.
	Dispatch bool

	// OpenCodeURL is the base URL of the headless opencode server. It is expected
	// to be a loopback address unless credentials are configured: opencode turns on
	// HTTP Basic Auth only when OPENCODE_SERVER_PASSWORD is set on the server, so
	// without them the only safe boundary is the machine itself.
	OpenCodeURL string

	// OpenCodeUser and OpenCodePassword enable HTTP Basic Auth for every call to an
	// agent server, which is what makes a runtime usable across a network instead of
	// only over loopback. The password never reaches the audit log or `-print-config`
	// (Report masks it), and the pair is set together or not at all: a username with
	// no password is how a half-configured deployment looks, and it would fail every
	// turn instead of refusing the start.
	OpenCodeUser     string
	OpenCodePassword string

	// DispatchAgent is the opencode agent used when a routing entry does not
	// name one. Which agent runs matters for the permission model, so it is
	// never silently defaulted to "build".
	DispatchAgent string

	// DispatchQueueSize bounds the deliveries waiting for the single worker. A
	// full queue drops work instead of blocking the publisher's request.
	DispatchQueueSize int

	// TaskDeadline bounds one opencode turn. A deadline is not a failure: the
	// session keeps running and the task is marked executing.
	TaskDeadline time.Duration

	// FirstResponse bounds how long a turn may take to produce its first assistant
	// message before it is failed. Without it, a prompt the agent server rejects
	// immediately looks like a slow turn until TaskDeadline.
	FirstResponse time.Duration

	// ShellWrappers are command prefixes an agent host's plugins are trusted to put
	// in front of a bash command before the permission request is raised. It is the
	// process-wide default; `runtimes.<name>.shell_wrappers` overrides it per host.
	// Empty means nothing is stripped, which is the safe default: see
	// internal/agent/opencode/wrapper.go. The *syntax* of a name is checked there,
	// not here, because the package that will trust the name owns what it may be.
	ShellWrappers []string

	// MaxTurns stops a task that keeps triggering; the cheap runaway guard.
	MaxTurns int

	// Trigger is the comment phrase that means "start implementing".
	Trigger string

	// StartStates are the workflow state values that mean "start implementing".
	StartStates []string

	// SkipAnalyzeOnCreate turns off the automatic read-only analysis of a newly
	// created issue, which is on by default.
	SkipAnalyzeOnCreate bool

	// RegistryFile is the append-only task registry (issue -> repository,
	// worktree, session). Empty resolves to <DataDir>/registry.jsonl.
	RegistryFile string

	// PauseFile disables dispatch while it exists. Empty resolves to
	// <DataDir>/DISPATCH_OFF.
	PauseFile string

	// TaskMaxCost stops a task whose accumulated opencode cost passes this
	// value. Zero disables the check.
	TaskMaxCost float64

	// WorktreeBase is the fallback worktree directory for routing entries that
	// do not declare one. Entries normally declare their own, because the base
	// has to sit outside the repository it belongs to.
	WorktreeBase string

	// AdminAddr is the control API listen address. Empty means no control API,
	// which is the default: `flowhub runtime invite` and enrolment need it, and
	// nothing else does.
	AdminAddr string

	// AdminToken authenticates every control API call. It is required whenever
	// AdminAddr is set — an unauthenticated control surface is the worst outcome,
	// the same rule the entry locks follow.
	AdminToken string

	// RuntimesFile is the runtime inventory. Empty resolves to
	// <DataDir>/runtimes.json.
	RuntimesFile string
}

// Load reads the configuration from the environment and validates it.
func Load() (Config, error) {
	cfg := Config{
		Addr:     env("FLOWHUB_ADDR", DefaultAddr),
		HookPath: env("FLOWHUB_HOOK_PATH", DefaultHookPath),
		// Locks are per source: a process serving two trackers must not share one URL
		// key. The unsuffixed names stay accepted as aliases for the default source, so
		// an existing deployment keeps working unchanged.
		HookKey:     firstEnv("", "FLOWHUB_"+strings.ToUpper(DefaultSourceName)+"_HOOK_KEY", "FLOWHUB_HOOK_KEY"),
		TokenHeader: env("FLOWHUB_TOKEN_HEADER", DefaultTokenHeader),
		Token:       firstEnv("", "FLOWHUB_"+strings.ToUpper(DefaultSourceName)+"_TOKEN", "FLOWHUB_TOKEN"),
		DataDir:     env("FLOWHUB_DATA_DIR", DefaultDataDir),
		LogFile:     env("FLOWHUB_LOG_FILE", ""),
		ConfigFile:  firstEnv(DefaultProjectsFile(), "FLOWHUB_CONFIG_FILE", "FLOWHUB_PROJECTS_FILE"),
		Source:      env("FLOWHUB_SOURCE", DefaultSourceName),
		LogLevel:    env("FLOWHUB_LOG_LEVEL", DefaultLogLevel),
		LogFormat:   env("FLOWHUB_LOG_FORMAT", DefaultLogFormat),
	}

	var err error
	if cfg.MaxBodyBytes, err = envInt64("FLOWHUB_MAX_BODY_BYTES", DefaultMaxBodyBytes); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_MAX_BODY_BYTES: %w", err)
	}
	if cfg.QueueSize, err = envInt("FLOWHUB_QUEUE_SIZE", DefaultQueueSize); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_QUEUE_SIZE: %w", err)
	}
	if cfg.ReplayWindow, err = envDuration("FLOWHUB_REPLAY_WINDOW", DefaultReplayWindow); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_REPLAY_WINDOW: %w", err)
	}
	if cfg.DedupeTTL, err = envDuration("FLOWHUB_DEDUPE_TTL", DefaultDedupeTTL); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_DEDUPE_TTL: %w", err)
	}
	if cfg.ShutdownTimeout, err = envDuration("FLOWHUB_SHUTDOWN_TIMEOUT", DefaultShutdown); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_SHUTDOWN_TIMEOUT: %w", err)
	}
	if cfg.LogHeaders, err = envBool("FLOWHUB_LOG_HEADERS", true); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_LOG_HEADERS: %w", err)
	}
	if cfg.AllowWildcardListen, err = envBool("FLOWHUB_ALLOW_WILDCARD_LISTEN", false); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_ALLOW_WILDCARD_LISTEN: %w", err)
	}
	if cfg.LogMaxBytes, err = envInt64("FLOWHUB_LOG_MAX_BYTES", DefaultLogMaxBytes); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_LOG_MAX_BYTES: %w", err)
	}
	if cfg.LogMaxFiles, err = envInt("FLOWHUB_LOG_MAX_FILES", DefaultLogMaxFiles); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_LOG_MAX_FILES: %w", err)
	}
	if cfg.DetailLog, err = envBool("FLOWHUB_DETAIL_LOG", true); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_DETAIL_LOG: %w", err)
	}
	if cfg.DetailLogMaxBodyBytes, err = envInt("FLOWHUB_DETAIL_MAX_BODY_BYTES", DefaultDetailMaxBody); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_DETAIL_MAX_BODY_BYTES: %w", err)
	}
	if cfg.AllowedSources, err = ParseSources(firstEnv("", "FLOWHUB_"+strings.ToUpper(DefaultSourceName)+"_ALLOWED_SOURCES", "FLOWHUB_ALLOWED_SOURCES")); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_ALLOWED_SOURCES: %w", err)
	}

	cfg.AdminAddr = env("FLOWHUB_ADMIN_ADDR", DefaultAdminAddr)
	cfg.AdminToken = os.Getenv("FLOWHUB_ADMIN_TOKEN")
	cfg.RuntimesFile = env("FLOWHUB_RUNTIMES_FILE", "")
	cfg.OpenCodeURL = env("FLOWHUB_OPENCODE_URL", DefaultOpenCodeURL)
	cfg.OpenCodeUser = os.Getenv("FLOWHUB_OPENCODE_USER")
	cfg.OpenCodePassword = os.Getenv("FLOWHUB_OPENCODE_PASSWORD")
	cfg.DispatchAgent = env("FLOWHUB_DISPATCH_AGENT", DefaultDispatchAgent)
	cfg.Trigger = env("FLOWHUB_TRIGGER", DefaultTrigger)
	cfg.StartStates = splitList(env("FLOWHUB_START_STATES", DefaultStartState))
	cfg.RegistryFile = env("FLOWHUB_REGISTRY_FILE", "")
	cfg.PauseFile = env("FLOWHUB_PAUSE_FILE", "")
	cfg.WorktreeBase = env("FLOWHUB_WORKTREE_BASE", "")
	if cfg.Dispatch, err = envBool("FLOWHUB_DISPATCH", false); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_DISPATCH: %w", err)
	}
	if cfg.SkipAnalyzeOnCreate, err = envBool("FLOWHUB_SKIP_ANALYZE_ON_CREATE", false); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_SKIP_ANALYZE_ON_CREATE: %w", err)
	}
	if cfg.DispatchQueueSize, err = envInt("FLOWHUB_DISPATCH_QUEUE", DefaultDispatchQueue); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_DISPATCH_QUEUE: %w", err)
	}
	if cfg.MaxTurns, err = envInt("FLOWHUB_MAX_TURNS", DefaultMaxTurns); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_MAX_TURNS: %w", err)
	}
	if cfg.TaskDeadline, err = envDuration("FLOWHUB_TASK_DEADLINE", DefaultTaskDeadline); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_TASK_DEADLINE: %w", err)
	}
	if cfg.FirstResponse, err = envDuration("FLOWHUB_FIRST_RESPONSE", DefaultFirstResponse); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_FIRST_RESPONSE: %w", err)
	}
	cfg.ShellWrappers = splitList(env("FLOWHUB_SHELL_WRAPPERS", ""))
	if cfg.TaskMaxCost, err = envFloat("FLOWHUB_TASK_MAX_COST", 0); err != nil {
		return Config{}, fmt.Errorf("FLOWHUB_TASK_MAX_COST: %w", err)
	}
	cfg.ConfigFile = ResolveProjectsFile(cfg.ConfigFile)

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DefaultProjectsFile returns the routing table path:
//
//	$XDG_CONFIG_HOME/flowhub/config.json   when XDG_CONFIG_HOME is set
//	$HOME/.config/flowhub/config.json      otherwise
//
// os.UserConfigDir is deliberately not used: on macOS it returns
// ~/Library/Application Support, while the XDG layout is what this project and
// the surrounding tooling expect.
func DefaultProjectsFile() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, DefaultProjectsDir, DefaultProjectsName)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", DefaultProjectsDir, DefaultProjectsName)
	}
	return filepath.Join(DefaultProjectsDir, DefaultProjectsName)
}

// ResolveProjectsFile expands a leading "~" and makes the path absolute.
//
// The "~" expansion matters because this value comes from an environment
// variable or a default, never from a shell: without it a path like
// "~/.config/flowhub/config.json" would be looked up as a literal directory
// named "~". Making it absolute means the logs and -print-config always name the
// file that was actually read, whatever the working directory was.
func ResolveProjectsFile(path string) string {
	path = expandHome(strings.TrimSpace(path))
	if path == "" {
		path = DefaultProjectsFile()
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return path
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func (c Config) validate() error {
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return fmt.Errorf("FLOWHUB_ADDR %q: %w", c.Addr, err)
	}
	if !strings.HasPrefix(c.HookPath, "/") {
		return fmt.Errorf("FLOWHUB_HOOK_PATH %q: must start with /", c.HookPath)
	}
	if strings.ContainsAny(c.HookPath, "{}\\") {
		return fmt.Errorf("FLOWHUB_HOOK_PATH %q: must not contain braces or backslashes", c.HookPath)
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("FLOWHUB_MAX_BODY_BYTES must be positive, got %d", c.MaxBodyBytes)
	}
	if c.QueueSize <= 0 {
		return fmt.Errorf("FLOWHUB_QUEUE_SIZE must be positive, got %d", c.QueueSize)
	}
	if c.DedupeTTL <= 0 {
		return fmt.Errorf("FLOWHUB_DEDUPE_TTL must be positive, got %s", c.DedupeTTL)
	}
	if c.ReplayWindow < 0 {
		return fmt.Errorf("FLOWHUB_REPLAY_WINDOW must not be negative, got %s", c.ReplayWindow)
	}
	if c.LogMaxBytes <= 0 {
		return fmt.Errorf("FLOWHUB_LOG_MAX_BYTES must be positive, got %d", c.LogMaxBytes)
	}
	if c.LogMaxFiles <= 0 {
		return fmt.Errorf("FLOWHUB_LOG_MAX_FILES must be positive, got %d", c.LogMaxFiles)
	}
	if c.DetailLogMaxBodyBytes <= 0 {
		return fmt.Errorf("FLOWHUB_DETAIL_MAX_BODY_BYTES must be positive, got %d", c.DetailLogMaxBodyBytes)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("FLOWHUB_LOG_FORMAT %q: want text or json", c.LogFormat)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("FLOWHUB_LOG_LEVEL %q: want debug, info, warn or error", c.LogLevel)
	}
	if c.DispatchQueueSize <= 0 {
		return fmt.Errorf("FLOWHUB_DISPATCH_QUEUE must be positive, got %d", c.DispatchQueueSize)
	}
	if c.TaskDeadline <= 0 {
		return fmt.Errorf("FLOWHUB_TASK_DEADLINE must be positive, got %s", c.TaskDeadline)
	}
	if c.FirstResponse <= 0 {
		return fmt.Errorf("FLOWHUB_FIRST_RESPONSE must be positive, got %s", c.FirstResponse)
	}
	if c.MaxTurns <= 0 {
		return fmt.Errorf("FLOWHUB_MAX_TURNS must be positive, got %d", c.MaxTurns)
	}
	if c.TaskMaxCost < 0 {
		return fmt.Errorf("FLOWHUB_TASK_MAX_COST must not be negative, got %v", c.TaskMaxCost)
	}
	if c.AdminAddr != "" {
		if _, _, err := net.SplitHostPort(c.AdminAddr); err != nil {
			return fmt.Errorf("FLOWHUB_ADMIN_ADDR %q: %w", c.AdminAddr, err)
		}
		if c.AdminToken == "" {
			// Fail closed rather than start an unauthenticated control surface.
			return fmt.Errorf("FLOWHUB_ADMIN_ADDR is set but FLOWHUB_ADMIN_TOKEN is empty: the control API refuses to run unauthenticated")
		}
	}
	// The URL is checked even when dispatch is off, because a typo in the
	// environment is a mistake either way and startup is the only cheap place
	// to catch it.
	url, err := neturl.Parse(c.OpenCodeURL)
	if err != nil {
		return fmt.Errorf("FLOWHUB_OPENCODE_URL %q: %w", c.OpenCodeURL, err)
	}
	if url.Scheme != "http" && url.Scheme != "https" {
		return fmt.Errorf("FLOWHUB_OPENCODE_URL %q: want an http or https URL", c.OpenCodeURL)
	}
	if url.Host == "" {
		return fmt.Errorf("FLOWHUB_OPENCODE_URL %q: missing host", c.OpenCodeURL)
	}
	if (c.OpenCodeUser == "") != (c.OpenCodePassword == "") {
		// Set together or not at all: half a credential pair fails every turn, and
		// it fails late. Refusing the start is the cheap place to catch it.
		return fmt.Errorf("FLOWHUB_OPENCODE_USER and FLOWHUB_OPENCODE_PASSWORD must be set together (one of them is empty)")
	}
	return nil
}

// ActiveLocks lists the entry locks currently enforced, in evaluation order.
func (c Config) ActiveLocks() []string {
	var locks []string
	if c.HookKey != "" {
		locks = append(locks, "url_key")
	}
	if c.Token != "" {
		locks = append(locks, "header_token")
	}
	if len(c.AllowedSources) > 0 {
		locks = append(locks, "source_ip")
	}
	return locks
}

// Warnings describes configuration that weakens the receiver. Each item is
// deliberately explicit: the design documents treat the three entry locks as the
// minimum bar, and a silently disabled lock is the worst outcome.
func (c Config) Warnings() []string {
	var warns []string
	if c.HookKey == "" {
		warns = append(warns, "FLOWHUB_HOOK_KEY is unset: lock 1 (URL embedded key) is DISABLED")
	}
	if c.Token == "" {
		warns = append(warns, "FLOWHUB_TOKEN is unset: lock 2 (header token) is DISABLED")
	}
	if len(c.AllowedSources) == 0 {
		warns = append(warns, "FLOWHUB_ALLOWED_SOURCES is unset: lock 3 (source IP allowlist) is DISABLED")
	}
	if c.ReplayWindow == 0 {
		warns = append(warns, "FLOWHUB_REPLAY_WINDOW=0: replay window is DISABLED")
	}
	if c.Dispatch && c.FirstResponse >= c.TaskDeadline {
		warns = append(warns, fmt.Sprintf("FLOWHUB_FIRST_RESPONSE (%s) is not below FLOWHUB_TASK_DEADLINE (%s), so a turn that never starts is only noticed at the deadline",
			c.FirstResponse, c.TaskDeadline))
	}
	if c.ControlAPIEnabled() && c.AdminToken == "" {
		warns = append(warns, "FLOWHUB_ADMIN_ADDR is set with no FLOWHUB_ADMIN_TOKEN: the control API will refuse to start")
	}
	if c.ListensOnAllInterfaces() {
		if c.AllowWildcardListen {
			warns = append(warns, fmt.Sprintf("FLOWHUB_ALLOW_WILDCARD_LISTEN=1: FLOWHUB_ADDR %s listens on every interface; make sure only the container/pod network can reach the port", c.Addr))
			if len(c.AllowedSources) == 0 {
				warns = append(warns, "wildcard bind with FLOWHUB_ALLOWED_SOURCES unset: any source that can reach the port passes locks 1 and 2 only (set the source allowlist, or publish the port to loopback)")
			}
		}
		// Without the acknowledgement this is a startup problem, not a warning;
		// see Problems.
	}
	return warns
}

// Problems lists configuration that must block startup but must not prevent
// `-print-config` from showing the effective settings. Syntactic mistakes stay in
// validate (Load fails); this is the semantic/security gate.
func (c Config) Problems() []string {
	var problems []string
	if c.ControlAPIListensOnAllInterfaces() && !c.AllowWildcardListen {
		problems = append(problems, fmt.Sprintf(
			"FLOWHUB_ADMIN_ADDR %s listens on every interface: the control API can invite, remove and rotate runtimes, so bind it to loopback or the WireGuard address (or acknowledge a wildcard with FLOWHUB_ALLOW_WILDCARD_LISTEN=1 if a container genuinely requires it)",
			c.AdminAddr))
	}
	if c.ListensOnAllInterfaces() && !c.AllowWildcardListen {
		problems = append(problems, fmt.Sprintf(
			"FLOWHUB_ADDR %s listens on every interface, which exposes the receiver on whatever network the host is attached to (LAN, Wi-Fi) instead of only the intended path: bind a specific address (the WireGuard address, the LAN address a router forwards to, or 127.0.0.1 behind a reverse proxy), or set FLOWHUB_ALLOW_WILDCARD_LISTEN=1 if a wildcard bind is genuinely required (for example inside a container)",
			c.Addr))
	}
	return problems
}

// ListensOnAllInterfaces reports whether Addr binds every interface, which
// exposes the receiver to whatever network the host is attached to (office LAN,
// café Wi-Fi) instead of only the WireGuard path.
//
// The kernel treats ":8080", "0.0.0.0:8080" and "[::]:8080" as the same wildcard
// bind, so all three spellings are detected — an earlier check only matched the
// two literal forms and let ":8080" through silently.
func (c Config) ListensOnAllInterfaces() bool {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return false
	}
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// ListenHost returns the host part of Addr.
func (c Config) ListenHost() string {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return c.Addr
	}
	return host
}

// ParseSources parses a comma separated list of IPs and CIDRs.
func ParseSources(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			_, network, err := net.ParseCIDR(part)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid CIDR", part)
			}
			out = append(out, network)
			continue
		}
		ip := net.ParseIP(part)
		if ip == nil {
			return nil, fmt.Errorf("%q is neither an IP address nor a CIDR", part)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// MaskSecret renders a non-reversible fingerprint of a secret, safe to print.
func MaskSecret(secret string) string {
	if secret == "" {
		return "<unset>"
	}
	sum := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("<set len=%d sha256:%s>", len(secret), hex.EncodeToString(sum[:4]))
}

// ResolvedLogFile returns the application log file path, or "" when file logging
// is disabled. An unset FLOWHUB_LOG_FILE defaults to <DataDir>/flowhub.log.
func (c Config) ResolvedLogFile() string {
	if c.LogFile != "" {
		return c.LogFile
	}
	return filepath.Join(c.DataDir, DefaultLogFile)
}

// DetailLogDir returns the directory holding the human readable payload log.
func (c Config) DetailLogDir() string { return c.DataDir }

// ResolvedRegistryFile returns the task registry path. An unset
// FLOWHUB_REGISTRY_FILE defaults to <DataDir>/registry.jsonl, so the registry
// travels with the audit trail.
func (c Config) ResolvedRegistryFile() string {
	if c.RegistryFile != "" {
		return c.RegistryFile
	}
	return filepath.Join(c.DataDir, DefaultRegistryName)
}

// ControlAPIEnabled reports whether the control API should be started.
func (c Config) ControlAPIEnabled() bool { return c.AdminAddr != "" }

// ControlAPIListensOnAllInterfaces reports whether the control API would be
// reachable from every network the host is attached to.
func (c Config) ControlAPIListensOnAllInterfaces() bool {
	if c.AdminAddr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(c.AdminAddr)
	if err != nil {
		return false
	}
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// ResolvedRuntimesFile returns the runtime inventory path. An unset
// FLOWHUB_RUNTIMES_FILE defaults to <DataDir>/runtimes.json.
func (c Config) ResolvedRuntimesFile() string {
	if c.RuntimesFile != "" {
		return c.RuntimesFile
	}
	return filepath.Join(c.DataDir, DefaultRuntimesName)
}

// ResolvedPauseFile returns the dispatcher kill switch. An unset
// FLOWHUB_PAUSE_FILE defaults to <DataDir>/DISPATCH_OFF.
func (c Config) ResolvedPauseFile() string {
	if c.PauseFile != "" {
		return c.PauseFile
	}
	return filepath.Join(c.DataDir, DefaultPauseName)
}

// Report renders the effective configuration for `flowhub -print-config`.
func (c Config) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "addr:               %s\n", c.Addr)
	fmt.Fprintf(&b, "hook_path:          %s\n", c.HookPath)
	fmt.Fprintf(&b, "hook_key:           %s\n", MaskSecret(c.HookKey))
	fmt.Fprintf(&b, "token_header:       %s\n", c.TokenHeader)
	fmt.Fprintf(&b, "token:              %s\n", MaskSecret(c.Token))
	sources := make([]string, 0, len(c.AllowedSources))
	for _, n := range c.AllowedSources {
		sources = append(sources, n.String())
	}
	fmt.Fprintf(&b, "allowed_sources:    %s\n", orNone(strings.Join(sources, ",")))
	fmt.Fprintf(&b, "data_dir:           %s\n", c.DataDir)
	fmt.Fprintf(&b, "max_body_bytes:     %d\n", c.MaxBodyBytes)
	fmt.Fprintf(&b, "replay_window:      %s\n", c.ReplayWindow)
	fmt.Fprintf(&b, "dedupe_ttl:         %s\n", c.DedupeTTL)
	fmt.Fprintf(&b, "queue_size:         %d\n", c.QueueSize)
	fmt.Fprintf(&b, "log_file:           %s\n", c.ResolvedLogFile())
	fmt.Fprintf(&b, "log_rotate:         %d bytes x %d files\n", c.LogMaxBytes, c.LogMaxFiles)
	fmt.Fprintf(&b, "detail_log:         %t (%s/payload-<date>.log, body <= %d bytes)\n",
		c.DetailLog, c.DetailLogDir(), c.DetailLogMaxBodyBytes)
	fmt.Fprintf(&b, "log_headers:        %t (values masked, token never stored)\n", c.LogHeaders)
	fmt.Fprintf(&b, "allow_wildcard_listen: %t\n", c.AllowWildcardListen)
	fmt.Fprintf(&b, "log_level:          %s\n", c.LogLevel)
	fmt.Fprintf(&b, "log_format:         %s\n", c.LogFormat)
	fmt.Fprintf(&b, "shutdown_timeout:   %s\n", c.ShutdownTimeout)
	fmt.Fprintf(&b, "dispatch:           %t\n", c.Dispatch)
	fmt.Fprintf(&b, "opencode_url:       %s\n", c.OpenCodeURL)
	fmt.Fprintf(&b, "opencode_auth:      %s\n", authReport(c))
	fmt.Fprintf(&b, "dispatch_agent:     %s\n", c.DispatchAgent)
	fmt.Fprintf(&b, "dispatch_queue:     %d\n", c.DispatchQueueSize)
	fmt.Fprintf(&b, "task_deadline:      %s\n", c.TaskDeadline)
	fmt.Fprintf(&b, "first_response:     %s\n", c.FirstResponse)
	fmt.Fprintf(&b, "shell_wrappers:     %s (env default; a runtime block overrides it)\n", orNone(strings.Join(c.ShellWrappers, ",")))
	fmt.Fprintf(&b, "max_turns:          %d\n", c.MaxTurns)
	fmt.Fprintf(&b, "task_max_cost:      %s\n", formatCost(c.TaskMaxCost))
	fmt.Fprintf(&b, "trigger:            %q\n", c.Trigger)
	fmt.Fprintf(&b, "start_states:       %s\n", strings.Join(c.StartStates, ","))
	fmt.Fprintf(&b, "skip_analyze_on_create: %t\n", c.SkipAnalyzeOnCreate)
	fmt.Fprintf(&b, "registry_file:      %s\n", c.ResolvedRegistryFile())
	fmt.Fprintf(&b, "pause_file:         %s (touch it to pause dispatch)\n", c.ResolvedPauseFile())
	fmt.Fprintf(&b, "control_api:        %s\n", controlAPIReport(c))
	fmt.Fprintf(&b, "runtimes_file:      %s\n", c.ResolvedRuntimesFile())
	worktreeBase := c.WorktreeBase
	if worktreeBase == "" {
		worktreeBase = "<none: every routing entry must declare worktrees>"
	}
	fmt.Fprintf(&b, "worktree_base:      %s\n", worktreeBase)
	locks := c.ActiveLocks()
	fmt.Fprintf(&b, "active_locks:       %s\n", orNone(strings.Join(locks, ",")))
	for _, w := range c.Warnings() {
		fmt.Fprintf(&b, "warning:            %s\n", w)
	}
	for _, p := range c.Problems() {
		fmt.Fprintf(&b, "problem:            %s\n", p)
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// controlAPIReport describes the control API without printing its token.
func controlAPIReport(c Config) string {
	if c.AdminAddr == "" {
		return "disabled (set FLOWHUB_ADMIN_ADDR to manage runtimes)"
	}
	return fmt.Sprintf("%s (admin_token %s)", c.AdminAddr, MaskSecret(c.AdminToken))
}

// authReport says whether the agent servers are called with credentials, without
// ever printing the password: the report is a document that gets pasted around.
func authReport(c Config) string {
	if c.OpenCodeUser == "" {
		return "none (loopback only: set FLOWHUB_OPENCODE_USER and FLOWHUB_OPENCODE_PASSWORD to reach a remote runtime)"
	}
	return fmt.Sprintf("basic auth as %s (password %s)", c.OpenCodeUser, MaskSecret(c.OpenCodePassword))
}

// formatCost renders the cost budget so that "disabled" is visible instead of
// looking like a budget of zero dollars.
func formatCost(cost float64) string {
	if cost == 0 {
		return "<disabled>"
	}
	return "$" + strconv.FormatFloat(cost, 'f', -1, 64)
}

// firstEnv returns the first environment variable that is set, or the fallback.
//
// It exists for the one rename this format introduces: the file is no longer only a
// routing table, so FLOWHUB_CONFIG_FILE is the preferred spelling while
// FLOWHUB_PROJECTS_FILE keeps working. An upgrade must not silently stop a live
// receiver because an operator's existing variable stopped being read.
func firstEnv(fallback string, names ...string) string {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return fallback
}

// splitList parses a comma separated list, dropping empty items.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", raw, err)
	}
	return v, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", raw, err)
	}
	return v, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%q: %w", raw, err)
	}
	return v, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	if raw == "0" {
		return 0, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", raw, err)
	}
	return v, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", raw, err)
	}
	return v, nil
}
