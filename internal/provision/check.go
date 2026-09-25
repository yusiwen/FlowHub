package provision

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Supported agent runtimes. Only opencode is implemented, and an unknown name is
// an error rather than a silently skipped check: init has to look in the right
// place for the agent's configuration, and guessing would produce a report that
// looks fine and means nothing.
const AgentOpenCode = "opencode"

// Forge kinds decide which command-line tool a host needs.
const (
	ForgeGitHub = "github" // gh
	ForgeGitea  = "gitea"  // tea
	ForgeNone   = "none"   // no forge tooling needed (a local or bare remote)
)

// Options is what this host is expected to be able to do. It is the local
// equivalent of the invitation the control plane will send (ADR 0002 step 3).
type Options struct {
	// Agent is the agent runtime to look for.
	Agent string
	// Forges maps a git host to a forge kind: "git.yusiwen.cn" -> "gitea".
	Forges map[string]string
	// Repos are the repositories this host must be able to work on.
	Repos []RepoExpectation
	// RequiredEnv names environment variables that must be present. Only presence
	// is checked; no value is read into the report.
	RequiredEnv []string
	// Strict turns warnings into failures, for use as a gate in automation.
	Strict bool
	// OpenCodeURL is the agent server whose model catalogue is checked. Empty uses
	// DefaultOpenCodeURL.
	OpenCodeURL string
	// ConfigRoot is where the agent's configuration lives, so the model check reads
	// the profile the server will actually load. Empty means "use the embedded
	// assets only".
	ConfigRoot string
	// Catalogue is injected so the model check is testable. Nil skips the check
	// with a note.
	Catalogue ModelCatalogue
}

// RepoExpectation is one repository this host must serve.
type RepoExpectation struct {
	Remote string
	// Clone is the local checkout, when there is one. Without it the read check
	// still runs against the remote, but the write check cannot.
	Clone string
}

// Report is the capability document: what was found, what is missing, and who the
// check ran as. It contains no secret values, by construction.
type Report struct {
	SchemaVersion int              `json:"schema_version"`
	CollectedAt   time.Time        `json:"collected_at"`
	Invoker       Identity         `json:"invoker"`
	Host          HostInfo         `json:"host"`
	Agent         *Agent           `json:"agent,omitempty"`
	Tools         map[string]Tool  `json:"tools"`
	Forges        map[string]Forge `json:"forges,omitempty"`
	Repos         []Repo           `json:"repos,omitempty"`
	EnvPresent    map[string]bool  `json:"env_present,omitempty"`
	// Profiles are the agent profiles this host would run, with the model each
	// pins resolved against the agent server's catalogue.
	Profiles []ProfileCheck `json:"profiles,omitempty"`
	// Artifacts lists managed files once installation exists (ADR 0002 step 2).
	Artifacts []Artifact `json:"artifacts"`
	Failures  []string   `json:"failures"`
	Warnings  []string   `json:"warnings"`
	OK        bool       `json:"ok"`
}

// HostInfo is the machine the check ran on.
type HostInfo struct {
	Hostname string `json:"hostname,omitempty"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// Agent is the agent runtime found on this host.
type Agent struct {
	Name            string `json:"name"`
	Path            string `json:"path,omitempty"`
	Version         string `json:"version,omitempty"`
	ConfigDir       string `json:"config_dir,omitempty"`
	ConfigDirExists bool   `json:"config_dir_exists"`
	// ConfigDirError is set when the directory could not be inspected at all,
	// which must never be reported as "missing".
	ConfigDirError string `json:"config_dir_error,omitempty"`
}

// Tool is a command-line tool this host may need.
type Tool struct {
	Required       bool     `json:"required"`
	Path           string   `json:"path,omitempty"`
	Version        string   `json:"version,omitempty"`
	ConfigFile     string   `json:"config_file,omitempty"`
	ConfigFileSeen bool     `json:"config_file_present"`
	Authenticated  *bool    `json:"authenticated,omitempty"`
	Accounts       []string `json:"accounts,omitempty"`
	Detail         string   `json:"detail,omitempty"`
}

// Forge is the tooling status for one git host.
type Forge struct {
	Kind          string `json:"kind"`
	Tool          string `json:"tool,omitempty"`
	ToolReady     bool   `json:"tool_ready"`
	Authenticated *bool  `json:"authenticated,omitempty"`
}

// Repo is one repository's readiness.
type Repo struct {
	Remote        string `json:"remote"`
	Clone         string `json:"clone,omitempty"`
	CloneExists   bool   `json:"clone_exists"`
	IsWorkTree    bool   `json:"is_work_tree"`
	OriginMatches bool   `json:"origin_matches"`
	LSRemote      bool   `json:"ls_remote_ok"`
	// PushDryRun is "ok", "skipped: …" or "failed: …". A dry run is the only
	// non-destructive proof that this host may push, and pushing is what the
	// workflow needs.
	PushDryRun string `json:"push_dry_run"`
	Detail     string `json:"detail,omitempty"`
}

// Artifact is a file FlowHub installed and manages (ADR 0002 step 2).
type Artifact struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Managed bool   `json:"managed"`
}

// Check inspects the host and returns the report. It writes nothing, changes
// nothing on the remote, and never records a secret value.
func Check(ctx context.Context, runner Runner, opts Options) (*Report, error) {
	if opts.Agent == "" {
		opts.Agent = AgentOpenCode
	}
	if opts.Agent != AgentOpenCode {
		return nil, fmt.Errorf("unknown agent %q: this build knows %q", opts.Agent, AgentOpenCode)
	}

	// Which git hosts this machine must be able to open pull requests on is
	// derived from the repositories it is asked to serve, plus anything declared
	// explicitly. Deriving matters: requiring the GitHub CLI on a host that only
	// serves Gitea repositories would reject a perfectly usable machine, and that
	// is the sort of false failure that teaches an operator to ignore the report.
	forges, err := effectiveForges(opts)
	if err != nil {
		return nil, err
	}
	opts.Forges = forges

	report := &Report{
		SchemaVersion: 1,
		CollectedAt:   time.Now().UTC(),
		Invoker:       runner.Identity(),
		Host: HostInfo{
			Hostname: runner.Hostname(),
			OS:       runner.GOOS(),
			Arch:     runner.GOARCH(),
		},
		Tools:     map[string]Tool{},
		Forges:    map[string]Forge{},
		Artifacts: []Artifact{},
	}

	checkAgent(ctx, runner, opts, report)
	checkGit(ctx, runner, report)
	checkProfiles(ctx, opts.Catalogue, opts, report)
	checkForgeTools(ctx, runner, opts, report)
	checkRepos(ctx, runner, opts, report)
	checkEnv(runner, opts, report)

	report.OK = len(report.Failures) == 0 && (!opts.Strict || len(report.Warnings) == 0)
	return report, nil
}

func checkAgent(ctx context.Context, runner Runner, opts Options, report *Report) {
	// The directory the report names must be the one this check actually read.
	// With --config-root the agent's files live somewhere other than the default,
	// and naming the default anyway sends the operator to look at a profile this
	// host does not run: the model check reads opts.ConfigRoot, so the report has
	// to agree with it.
	configDir := ConfigRootFor(runner, InstallOptions{Agent: opts.Agent, ConfigRoot: opts.ConfigRoot})
	agent := &Agent{Name: opts.Agent, ConfigDir: tildify(runner.Identity().Home, configDir)}
	report.Agent = agent

	// The configuration directory is checked even when the binary is missing. It
	// is tempting to return early here, and doing so reported a directory that
	// exists as "does not exist", because a zero-valued bool is indistinguishable
	// from a checked-and-absent one.
	exists, _, err := statDir(runner, configDir)
	switch {
	case err != nil:
		agent.ConfigDirError = err.Error()
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"%s configuration directory %s could not be inspected: %v", opts.Agent, agent.ConfigDir, err))
	case !exists:
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"%s configuration directory %s does not exist yet", opts.Agent, agent.ConfigDir))
	}
	agent.ConfigDirExists = exists

	path, found := runner.Lookup(opts.Agent)
	if !found {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"%s is not on PATH: install the agent runtime, or add its directory to the PATH of the user that will run FlowHub",
			opts.Agent))
		return
	}
	agent.Path = path
	if out, versionErr := runner.Run(ctx, configDirFor(runner, configDir), opts.Agent, "--version"); versionErr == nil {
		agent.Version = firstLine(out)
	} else {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%s --version failed: %s", opts.Agent, boundedError(out, versionErr)))
	}
}

func checkGit(ctx context.Context, runner Runner, report *Report) {
	tool := Tool{Required: true}
	path, found := runner.Lookup("git")
	if !found {
		report.Tools["git"] = tool
		report.Failures = append(report.Failures, "git is not on PATH")
		return
	}
	tool.Path = path
	out, err := runner.Run(ctx, "", "git", "--version")
	if err != nil {
		tool.Detail = boundedError(out, err)
		report.Failures = append(report.Failures, "git --version failed: "+tool.Detail)
	} else {
		tool.Version = firstLine(out)
	}
	report.Tools["git"] = tool
}

// checkForgeTools resolves which command-line tool each declared git host needs and
// whether it is authenticated. The requirement is per host on purpose: a machine
// that only serves Gitea repositories does not need the GitHub CLI, and demanding
// it would reject a perfectly usable host.
// DefaultForges maps a git host to its forge kind without being told. Only the
// one host that cannot be confused with a self-hosted instance is in here;
// everything else has to be declared, because guessing wrong would demand the
// wrong tool and then fail the check for the wrong reason.
var DefaultForges = map[string]string{"github.com": ForgeGitHub}

// effectiveForges merges the declared forges with the ones the repositories imply.
func effectiveForges(opts Options) (map[string]string, error) {
	effective := map[string]string{}
	for host, kind := range opts.Forges {
		host = strings.ToLower(strings.TrimSpace(host))
		kind = strings.ToLower(strings.TrimSpace(kind))
		switch kind {
		case ForgeGitHub, ForgeGitea, ForgeNone:
		default:
			return nil, fmt.Errorf("forge %s=%s: want %s, %s or %s", host, kind, ForgeGitHub, ForgeGitea, ForgeNone)
		}
		effective[host] = kind
	}
	for _, repo := range opts.Repos {
		host := remoteHost(repo.Remote)
		if host == "" {
			continue
		}
		if _, declared := effective[host]; declared {
			continue
		}
		if kind, known := DefaultForges[host]; known {
			effective[host] = kind
			continue
		}
		return nil, fmt.Errorf(
			"repository %s is on %s and no forge is declared for it: pass --forge %s=<%s|%s|%s>",
			repo.Remote, host, host, ForgeGitHub, ForgeGitea, ForgeNone)
	}
	return effective, nil
}

// remoteHost extracts the host from the remote URL spellings git accepts.
func remoteHost(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if strings.Contains(remote, "://") {
		if parsed, err := url.Parse(remote); err == nil {
			return strings.ToLower(parsed.Hostname())
		}
		return ""
	}
	// scp-like: [user@]host:path
	rest := remote
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if colon := strings.Index(rest, ":"); colon > 0 {
		return strings.ToLower(rest[:colon])
	}
	if slash := strings.Index(rest, "/"); slash > 0 {
		return strings.ToLower(rest[:slash])
	}
	return ""
}

func checkForgeTools(ctx context.Context, runner Runner, opts Options, report *Report) {
	kinds := map[string]bool{}
	for _, kind := range opts.Forges {
		if kind != ForgeNone {
			kinds[kind] = true
		}
	}
	probed := map[string]Tool{}
	for kind := range kinds {
		probed[kind] = probeForgeTool(ctx, runner, kind, report)
	}
	for host, kind := range opts.Forges {
		forge := Forge{Kind: kind}
		if kind == ForgeNone {
			report.Forges[host] = forge
			continue
		}
		tool := probed[kind]
		forge.Tool = forgeToolName(kind)
		forge.ToolReady = tool.Path != ""
		forge.Authenticated = tool.Authenticated
		report.Forges[host] = forge

		if !forge.ToolReady {
			report.Failures = append(report.Failures, fmt.Sprintf(
				"%s needs the %s command-line tool, which is not on PATH", host, forge.Tool))
			continue
		}
		if tool.Authenticated != nil && !*tool.Authenticated {
			report.Failures = append(report.Failures, fmt.Sprintf(
				"%s is a %s host, but %s is not authenticated in %s", host, kind, forge.Tool, orUnknown(tool.ConfigFile)))
		}
	}
}

func probeForgeTool(ctx context.Context, runner Runner, kind string, report *Report) Tool {
	name := forgeToolName(kind)
	tool := Tool{Required: true}

	path, found := runner.Lookup(name)
	if !found {
		report.Tools[name] = tool
		return tool
	}
	tool.Path = path
	if out, err := runner.Run(ctx, "", name, "--version"); err == nil {
		tool.Version = firstLine(out)
	}

	tool.ConfigFile = tildify(runner.Identity().Home, toolConfigFile(runner, name))
	tool.ConfigFileSeen = dirExists(runner, toolConfigFile(runner, name))

	authenticated, accounts, detail := probeAuth(ctx, runner, name)
	tool.Authenticated = &authenticated
	tool.Accounts = accounts
	tool.Detail = detail

	report.Tools[name] = tool
	return tool
}

// probeAuth asks a forge tool whether it holds usable credentials.
//
// Only the exit status and, for gh, the account names are derived from what the
// tool prints. Nothing else of its output is kept: it can contain a masked token,
// a device code or an internal hostname, and a capability report is a document
// that gets copied around and pasted into tickets.
func probeAuth(ctx context.Context, runner Runner, name string) (bool, []string, string) {
	var args []string
	switch name {
	case "gh":
		args = []string{"auth", "status"}
	case "tea":
		args = []string{"login", "list"}
	default:
		return false, nil, "no authentication probe is defined for this tool"
	}

	out, err := runner.Run(ctx, "", name, args...)
	if err != nil {
		return false, nil, fmt.Sprintf("%s %s failed: %s", name, strings.Join(args, " "), boundedError(out, err))
	}
	return true, extractAccounts(name, out), fmt.Sprintf("%s %s succeeded", name, strings.Join(args, " "))
}

// accountPattern is deliberately narrow: it extracts account names and drops
// everything else gh prints.
var accountPattern = regexp.MustCompile(`account ([A-Za-z0-9_.-]+)`)

func extractAccounts(name, output string) []string {
	if name != "gh" {
		// tea's table format varies by version, and guessing at its columns is how
		// a report ends up quoting a token by accident.
		return nil
	}
	seen := map[string]bool{}
	var accounts []string
	for _, match := range accountPattern.FindAllStringSubmatch(output, -1) {
		if account := match[1]; !seen[account] {
			seen[account] = true
			accounts = append(accounts, account)
		}
	}
	return accounts
}

func checkRepos(ctx context.Context, runner Runner, opts Options, report *Report) {
	for _, expectation := range opts.Repos {
		repo := Repo{
			Remote: strings.TrimSpace(expectation.Remote),
			Clone:  tildify(runner.Identity().Home, strings.TrimSpace(expectation.Clone)),
		}
		if repo.Remote == "" {
			report.Failures = append(report.Failures, "a repository expectation has an empty remote")
			continue
		}

		clone := strings.TrimSpace(expectation.Clone)
		if clone == "" {
			// Without a local checkout the read check still runs; the write check
			// cannot, because a dry-run push needs a repository to push from.
			if out, err := runner.Run(ctx, runner.TempDir(), "git", "ls-remote", "--exit-code", repo.Remote, "HEAD"); err == nil {
				repo.LSRemote = true
			} else {
				repo.Detail = boundedError(out, err)
				report.Failures = append(report.Failures, fmt.Sprintf("%s: git ls-remote failed: %s", repo.Remote, repo.Detail))
			}
			repo.PushDryRun = "skipped: no local clone was given"
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"%s: write access was not checked; pass --repo %s=<clone path> to check it", repo.Remote, repo.Remote))
			report.Repos = append(report.Repos, repo)
			continue
		}

		if exists, _, err := statDir(runner, clone); err != nil {
			report.Failures = append(report.Failures, fmt.Sprintf("%s: clone %s could not be inspected: %v", repo.Remote, repo.Clone, err))
			report.Repos = append(report.Repos, repo)
			continue
		} else if !exists {
			report.Failures = append(report.Failures, fmt.Sprintf("%s: clone %s does not exist", repo.Remote, repo.Clone))
			report.Repos = append(report.Repos, repo)
			continue
		}
		repo.CloneExists = true

		if out, err := runner.Run(ctx, clone, "git", "rev-parse", "--is-inside-work-tree"); err == nil && strings.TrimSpace(out) == "true" {
			repo.IsWorkTree = true
		} else {
			repo.Detail = boundedError(out, err)
			report.Failures = append(report.Failures, fmt.Sprintf("%s: %s is not a git work tree", repo.Remote, repo.Clone))
		}

		if out, err := runner.Run(ctx, clone, "git", "remote", "get-url", "origin"); err == nil {
			repo.OriginMatches = sameRemote(strings.TrimSpace(out), repo.Remote)
			if !repo.OriginMatches {
				report.Failures = append(report.Failures, fmt.Sprintf(
					"%s: clone %s has origin %q", repo.Remote, repo.Clone, strings.TrimSpace(out)))
			}
		} else {
			repo.Detail = boundedError(out, err)
			report.Failures = append(report.Failures, fmt.Sprintf("%s: clone %s has no origin remote", repo.Remote, repo.Clone))
		}

		if out, err := runner.Run(ctx, clone, "git", "ls-remote", "--exit-code", "origin", "HEAD"); err == nil {
			repo.LSRemote = true
		} else {
			repo.Detail = boundedError(out, err)
			report.Failures = append(report.Failures, fmt.Sprintf(
				"%s: git ls-remote origin failed: %s (read credentials)", repo.Remote, repo.Detail))
		}

		// The dry run is the check the workflow depends on: the agent's work gets
		// pushed, so a read-only clone is a failure even when everything else looks
		// healthy. A dry run contacts the remote and updates nothing.
		if out, err := runner.Run(ctx, clone, "git", "push", "--dry-run", "origin", "HEAD:refs/heads/flowhub/probe-check"); err == nil {
			repo.PushDryRun = "ok"
		} else {
			repo.PushDryRun = "failed: " + boundedError(out, err)
			report.Failures = append(report.Failures, fmt.Sprintf(
				"%s: git push --dry-run failed: %s (write access)", repo.Remote, boundedError(out, err)))
		}
		report.Repos = append(report.Repos, repo)
	}
}

// checkEnv records which required variables are present. Values are never read
// into the report: the point is only whether the agent will find a credential at
// all, and the document has to stay safe to copy.
func checkEnv(runner Runner, opts Options, report *Report) {
	if len(opts.RequiredEnv) == 0 {
		return
	}
	report.EnvPresent = map[string]bool{}
	for _, name := range opts.RequiredEnv {
		value, present := runner.Getenv(name)
		report.EnvPresent[name] = present && strings.TrimSpace(value) != ""
		if !report.EnvPresent[name] {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"%s is not set: the agent may be unable to authenticate; set it in the agent's own environment file", name))
		}
	}
}

// agentConfigDir is where the agent keeps its configuration on this host.
func agentConfigDir(runner Runner) string {
	if dir, ok := runner.Getenv("XDG_CONFIG_HOME"); ok && strings.TrimSpace(dir) != "" {
		return strings.TrimSuffix(strings.TrimSpace(dir), "/") + "/" + AgentOpenCode
	}
	return strings.TrimSuffix(runner.Identity().Home, "/") + "/.config/" + AgentOpenCode
}

// configDirFor returns an existing directory to run the agent binary in, or "" to
// inherit the caller's working directory. The agent's own configuration directory
// is preferred when it exists, so a version probe runs where the agent expects to.
func configDirFor(runner Runner, configDir string) string {
	if configDir != "" && dirExists(runner, configDir) {
		return configDir
	}
	if dir := agentConfigDir(runner); dirExists(runner, dir) {
		return dir
	}
	return ""
}

// toolConfigFile is where a forge tool stores its credentials.
//
// The file name is per tool and cannot be templated from the command name: gh
// writes hosts.yml and tea writes config.yml. Getting this wrong makes the report
// point an operator at a file that does not exist, which is worse than saying
// nothing.
func toolConfigFile(runner Runner, name string) string {
	base := ""
	if dir, ok := runner.Getenv("XDG_CONFIG_HOME"); ok && strings.TrimSpace(dir) != "" {
		base = strings.TrimSuffix(strings.TrimSpace(dir), "/")
	} else {
		base = strings.TrimSuffix(runner.Identity().Home, "/") + "/.config"
	}
	file := name + ".yml"
	switch name {
	case "gh":
		file = "hosts.yml"
	case "tea":
		file = "config.yml"
	}
	return base + "/" + name + "/" + file
}

func forgeToolName(kind string) string {
	switch kind {
	case ForgeGitHub:
		return "gh"
	case ForgeGitea:
		return "tea"
	default:
		return ""
	}
}

// sameRemote compares two remote URLs the way a human would: the case of the host
// and a trailing ".git" must not decide whether a clone is the right one.
func sameRemote(a, b string) bool { return normalizeRemote(a) == normalizeRemote(b) }

func normalizeRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimSuffix(remote, "/")
	remote = strings.TrimSuffix(remote, ".git")
	return strings.ToLower(remote)
}

func tildify(home, path string) string {
	if home == "" || path == "" {
		return path
	}
	home = strings.TrimSuffix(home, "/")
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// statDir reports the state of a path, keeping "could not look" separate from
// "absent".
func statDir(runner Runner, path string) (bool, bool, error) {
	if path == "" {
		return false, false, nil
	}
	return runner.Stat(path)
}

func dirExists(runner Runner, path string) bool {
	exists, isDir, err := statDir(runner, path)
	return err == nil && exists && isDir
}

func orUnknown(value string) string {
	if value == "" {
		return "an unknown location"
	}
	return value
}
