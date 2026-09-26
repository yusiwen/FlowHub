package provision

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	agentruntime "github.com/yusiwen/flowhub/internal/agent/opencode"
	"github.com/yusiwen/flowhub/internal/runtimes"
)

// Version is the build's version string. The CLI entry point sets it so the
// manifest can record which binary wrote a file, and `--version` output stays in
// one place.
var Version = "dev"

// Exit statuses. A capability check is used as a gate, so "the host is not ready"
// has to be distinguishable from "you typed the command wrong".
const (
	ExitOK       = 0
	ExitUsage    = 1
	ExitNotReady = 2
)

// Main dispatches `flowhub <command> …` and returns a process exit status.
//
// It runs before the receiver configuration is loaded: a data-plane host has none
// of the receiver's environment variables, and `flowhub runtime init` must not
// fail because they are missing.
func Main(command string, args []string, stdout, stderr io.Writer) int {
	switch command {
	case "runtime":
		return runtimeCommand(args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "flowhub: unknown command %q\n\n", command)
		usage(stderr)
		return ExitUsage
	}
}

func runtimeCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return ExitUsage
	}
	switch args[0] {
	case "init":
		return initCommand(args[1:], stdout, stderr)
	case "uninstall":
		return uninstallCommand(args[1:], stdout, stderr)
	case "invite", "list", "show", "remove", "rotate":
		// These run on the service host and talk to the running service over its
		// admin listener. They live in the runtimes package because they own the
		// control-plane client, but the subcommand router stays here so `flowhub
		// runtime …` is one surface.
		if code := runtimes.CLI(args[0], args[1:], stdout, stderr, os.Getenv); code != runtimes.ExitOK {
			return code
		}
		return ExitOK
	case "doctor":
		return doctorCommand(args[1:], stdout, stderr)
	case "reconfigure":
		fmt.Fprintf(stderr, "flowhub runtime %s: not implemented in this build\n", args[0])
		fmt.Fprintln(stderr, "this build implements init, uninstall, doctor and the control-plane commands (see docs/adr/0002).")
		return ExitUsage
	case "-h", "--help", "help":
		usage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "flowhub runtime: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return ExitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `flowhub runtime — install and inspect a machine that runs agent turns

  flowhub runtime init [--check] [flags]
      Inspect this host, and install the agent files FlowHub manages.

      --check                 report only: write nothing, register nothing
      --agent <name>          agent runtime (default opencode)
      --config-root <dir>     agent configuration directory (default ~/.config/<agent>)
      --force                 overwrite a file FlowHub did not write, or that a
                              human edited after FlowHub wrote it
      --opencode-url <url>    this host's agent server, whose model catalogue is
                              checked (default http://127.0.0.1:4096)
      --server <url>          control plane to register with
      --token <token>         invite token from 'flowhub runtime invite'
      --name <name>           the runtime name the control plane invited
      --advertise <url>       how the control plane reaches this host (probed before
                              it is activated, so a NAT mistake fails here)
      --repo <remote>[=<clone>]
                              a repository this host must serve; repeatable. The
                              clone path enables the write check (git push --dry-run)
      --forge <host>=<kind>   which tooling a git host needs: github, gitea or none;
                              repeatable. github.com is recognised from the remote
      --require-env <NAME>    environment variable that must be present; repeatable.
                              Only presence is checked, never the value
      --json                  print the machine-readable report instead of the summary
      --strict                treat warnings as failures
      --timeout <duration>    bound a single command (default 20s)

  flowhub runtime doctor [--push] [flags]
      Re-check this host and report what it can and cannot do. With --push the
      report is sent to the control plane under this host's runtime secret, which
      also refreshes the models the dispatcher pins. A report that does not pass
      is not pushed: last_seen has to mean "a host that was verified fit".

  flowhub runtime uninstall [--agent <name>] [--config-root <dir>] [--force]
      Remove exactly the files the manifest records. A file a human edited is
      kept unless --force is given.

  What init installs comes from the binary, never from the network; the manifest
  records a hash of every file so a re-run is a no-op and a hand edit is refused
  with a diff. "init --server … --name … --token …" enrols the host, and
  "doctor --push" keeps the control plane's view of it current.
`)
}

// checkFlags are the flags that configure the capability check itself.
//
// `init --check` and `doctor` run the same check, so they share one parser: a flag
// only one of them honoured would let the two commands disagree about the same
// machine, which is exactly what makes a report untrustworthy.
type checkFlags struct {
	Options
	configRoot  string
	json        bool
	timeout     time.Duration
	openCodeURL string
	repos       listFlag
	forges      listFlag
	requiredEnv listFlag
}

func addCheckFlags(fs *flag.FlagSet, into *checkFlags) {
	fs.StringVar(&into.Agent, "agent", AgentOpenCode, "agent runtime to look for")
	fs.StringVar(&into.configRoot, "config-root", "", "agent configuration directory")
	fs.StringVar(&into.openCodeURL, "opencode-url", DefaultOpenCodeURL, "agent server whose model catalogue is checked")
	fs.Var(&into.repos, "repo", "repository to check: <remote>[=<clone path>]; repeatable")
	fs.Var(&into.forges, "forge", "forge tooling for a git host: <host>=<github|gitea|none>; repeatable")
	fs.Var(&into.requiredEnv, "require-env", "environment variable that must be present; repeatable")
	fs.BoolVar(&into.json, "json", false, "print the machine-readable report")
	fs.BoolVar(&into.Strict, "strict", false, "treat warnings as failures")
	fs.DurationVar(&into.timeout, "timeout", DefaultCommandTimeout, "bound a single command")
}

// finish validates and normalises what the shared flags collected.
func (c *checkFlags) finish(command string, stderr io.Writer) error {
	if c.timeout <= 0 {
		fmt.Fprintf(stderr, "%s: --timeout must be positive\n", command)
		return fmt.Errorf("bad timeout")
	}
	c.Agent = strings.TrimSpace(c.Agent)
	if c.Agent == "" {
		fmt.Fprintf(stderr, "%s: --agent must name the agent runtime to look for\n", command)
		return fmt.Errorf("bad agent")
	}
	c.configRoot = strings.TrimSpace(c.configRoot)
	c.openCodeURL = strings.TrimRight(strings.TrimSpace(c.openCodeURL), "/")

	c.Forges = map[string]string{}
	for _, entry := range c.forges {
		host, kind, found := strings.Cut(entry, "=")
		host, kind = strings.TrimSpace(host), strings.TrimSpace(kind)
		if !found || host == "" || kind == "" {
			fmt.Fprintf(stderr, "%s: --forge %q: want <host>=<github|gitea|none>\n", command, entry)
			return fmt.Errorf("bad forge")
		}
		c.Forges[strings.ToLower(host)] = strings.ToLower(kind)
	}
	for _, entry := range c.repos {
		remote, clone, _ := strings.Cut(entry, "=")
		remote, clone = strings.TrimSpace(remote), strings.TrimSpace(clone)
		if remote == "" {
			fmt.Fprintf(stderr, "%s: --repo %q: the remote is required\n", command, entry)
			return fmt.Errorf("bad repo")
		}
		c.Repos = append(c.Repos, RepoExpectation{Remote: remote, Clone: clone})
	}
	for _, name := range c.requiredEnv {
		if name = strings.TrimSpace(name); name != "" {
			c.RequiredEnv = append(c.RequiredEnv, name)
		}
	}
	return nil
}

// resolve fills in the two values the check derives from the flags: the effective
// configuration directory, and the model catalogue of this host's own agent server.
func (c *checkFlags) resolve(runner Runner) {
	c.OpenCodeURL = c.openCodeURL
	// Without --config-root the effective root is the agent's own directory, and the
	// model check has to look at the profile the server will load — not merely at
	// what this binary would install.
	c.ConfigRoot = ConfigRootFor(runner, InstallOptions{Agent: c.Agent, ConfigRoot: c.configRoot})
	c.Catalogue = catalogueFor(c.openCodeURL)
}

// initOptions is the parsed form of `runtime init`.
type initOptions struct {
	checkFlags
	check     bool
	force     bool
	version   string
	server    string
	token     string
	advertise string
	name      string
}

// doctorOptions is the parsed form of `runtime doctor`.
type doctorOptions struct {
	checkFlags
	push   bool
	server string
}

func initCommand(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseInit(args, stderr)
	if err != nil {
		return ExitUsage
	}

	runner := ExecRunner{Timeout: parsed.timeout}
	parsed.resolve(runner)

	install := InstallOptions{
		Agent:      parsed.Agent,
		ConfigRoot: parsed.ConfigRoot,
		// The resolved root, not the raw flag: the manifest and the runtime identity
		// both live in FlowHub's own configuration directory, and an empty root used
		// to resolve to a relative path there.
		FlowHubVersion: parsed.version,
		Force:          parsed.force,
	}

	// Under --json, stdout carries only the JSON document, so everything written
	// for a human goes to stderr.
	human := stdout
	if parsed.json {
		human = stderr
	}

	var outcome *InstallReport
	if !parsed.check {
		// The one thing that has to be true before writing anything is that there is
		// an agent to write for. Everything else is reported and gated on the exit
		// status: a missing forge tool is fixed while the files are already in place,
		// and re-running the install is a no-op.
		if _, found := runner.Lookup(parsed.Agent); !found {
			report, err := Check(context.Background(), runner, parsed.Options)
			if err != nil {
				fmt.Fprintf(stderr, "flowhub runtime init: %v\n", err)
				return ExitUsage
			}
			printCheck(report, parsed.json, stdout, stderr)
			fmt.Fprintln(stderr, "flowhub runtime init: nothing was installed because the agent runtime is missing")
			return ExitNotReady
		}
		outcome, err = Install(runner, install)
		if err != nil {
			fmt.Fprintf(stderr, "flowhub runtime init: %v\n", err)
			return ExitUsage
		}
	}

	// The report is generated *after* the install so it describes the host as it now
	// is. Reporting the pre-install state told an operator who had just repaired a
	// profile that the profile was still broken, and exited non-zero after a
	// successful repair.
	report, err := Check(context.Background(), runner, parsed.Options)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime init: %v\n", err)
		return ExitUsage
	}
	printCheck(report, parsed.json, stdout, stderr)

	refused := false
	if parsed.check {
		plan, _, err := Plan(runner, install)
		if err != nil {
			fmt.Fprintf(stderr, "flowhub runtime init: %v\n", err)
			return ExitUsage
		}
		fmt.Fprintf(human, "\nfiles FlowHub manages (nothing is written by --check)\n")
		plan.Text(human, "plan")
		fmt.Fprintln(human, "note: --check wrote nothing and registered nothing")
		refused = plan.Refused()
	} else {
		fmt.Fprintf(human, "\nfiles FlowHub manages\n")
		outcome.Text(human, "install")
		refused = outcome.Refused()

		// Enrolment is optional at this point: a host can be prepared before the
		// control plane is reachable, and `init --server … --token …` finishes the
		// job once it is.
		switch {
		case parsed.server == "" && parsed.token == "" && parsed.name == "":
			if identity, identityErr := LoadRuntimeIdentity(parsed.ConfigRoot); identityErr == nil {
				fmt.Fprintf(human, "note: already enrolled as %s with %s; run `flowhub runtime doctor --push` to refresh the report and the models the control plane pins\n",
					identity.Name, identity.Server)
			} else {
				fmt.Fprintln(human, "note: not registered with a control plane; pass --server, --name and --token (from the invite) to enrol")
			}
		case parsed.server == "" || parsed.token == "" || parsed.name == "":
			fmt.Fprintln(stderr, "flowhub runtime init: --server, --name and --token go together; nothing was registered")
			refused = true
		default:
			client := runtimes.NewClient(parsed.server, "")
			registerCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			enrolled, err := Register(registerCtx, client, parsed.token, parsed.name, report, parsed.advertise, install)
			cancel()
			if err != nil {
				fmt.Fprintf(stderr, "flowhub runtime init: registration failed: %v\n", err)
				refused = true
				break
			}
			fmt.Fprintf(human, "\nregistered as %s with %s (the secret is in %s, mode 0600)\n",
				enrolled.Runtime.Name, parsed.server, enrolled.Stored)
		}
	}

	if !report.OK || refused {
		return ExitNotReady
	}
	return ExitOK
}

// doctorCommand re-checks this host and, with --push, reports the result to the
// control plane.
//
// The push is what keeps the control plane's view of a worker current: the claim
// made at enrolment is a snapshot, and a repaired agent profile changes the model
// the dispatcher has to pin. A report that does not pass is deliberately not
// pushed, so `last_seen` keeps meaning "a host that was verified fit" — an expired
// credential has to surface here, while the operator is looking, rather than at the
// first task that needs it.
func doctorCommand(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseDoctor(args, stderr)
	if err != nil {
		return ExitUsage
	}

	runner := ExecRunner{Timeout: parsed.timeout}
	parsed.resolve(runner)

	report, err := Check(context.Background(), runner, parsed.Options)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime doctor: %v\n", err)
		return ExitUsage
	}
	printCheck(report, parsed.json, stdout, stderr)

	// Under --json, stdout carries only the JSON document, so everything written for
	// a human goes to stderr.
	human := stdout
	if parsed.json {
		human = stderr
	}

	pushed := true
	switch {
	case !parsed.push:
		fmt.Fprintln(human, "note: --push was not given, so the control plane still holds the report from the last push")
		if identity, identityErr := LoadRuntimeIdentity(parsed.ConfigRoot); identityErr == nil {
			fmt.Fprintf(human, "this host is enrolled as %s with %s\n", identity.Name, identity.Server)
		}
	case !report.OK:
		fmt.Fprintln(stderr, "flowhub runtime doctor: the report is not pushed while the check fails; fix the gap and run this again")
		pushed = false
	default:
		pushed = false
		identity, identityErr := LoadRuntimeIdentity(parsed.ConfigRoot)
		if identityErr != nil {
			fmt.Fprintf(stderr, "flowhub runtime doctor: %v\n", identityErr)
			break
		}
		server := parsed.server
		if server == "" {
			server = identity.Server
		}
		if server == "" {
			fmt.Fprintln(stderr, "flowhub runtime doctor: this host's identity names no control plane; pass --server")
			break
		}
		client := runtimes.NewClient(server, "")
		pushCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		runtime, pushErr := Push(pushCtx, client, identity, report)
		cancel()
		if pushErr != nil {
			fmt.Fprintf(stderr, "flowhub runtime doctor: the control plane did not accept the report: %v\n", pushErr)
			break
		}
		pushed = true
		fmt.Fprintf(human, "\npushed to %s: %s is %s (last seen %s)\n",
			server, runtime.Name, runtime.State, runtime.LastSeen.Format(time.RFC3339))
		if models := runtimes.ModelsText(runtime.Models); models != "" {
			fmt.Fprintf(human, "models the dispatcher will pin: %s\n", models)
		}
	}

	if !report.OK || !pushed {
		return ExitNotReady
	}
	return ExitOK
}

// catalogueFor builds the model-catalogue reader for a host's own agent server.
func catalogueFor(url string) ModelCatalogue {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	return agentruntime.New(agentruntime.Options{BaseURL: strings.TrimSpace(url), Timeout: catalogueTimeout})
}

// printCheck renders the capability report, in whichever form was asked for.
func printCheck(report *Report, asJSON bool, stdout, stderr io.Writer) {
	if asJSON {
		encoded, err := report.JSON()
		if err != nil {
			fmt.Fprintf(stderr, "flowhub runtime init: cannot encode the report: %v\n", err)
			return
		}
		_, _ = stdout.Write(encoded)
		return
	}
	fmt.Fprintln(stdout)
	report.Text(stdout)
}

func uninstallCommand(args []string, stdout, stderr io.Writer) int {
	var (
		agent      string
		configRoot string
		force      bool
	)
	fs := flag.NewFlagSet("flowhub runtime uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&agent, "agent", AgentOpenCode, "agent runtime whose files to remove")
	fs.StringVar(&configRoot, "config-root", "", "agent configuration directory")
	fs.BoolVar(&force, "force", false, "also delete files a human edited")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "flowhub runtime uninstall: unexpected argument %q\n", fs.Arg(0))
		return ExitUsage
	}

	runner := ExecRunner{}
	report, err := Uninstall(runner, InstallOptions{
		Agent:      strings.TrimSpace(agent),
		ConfigRoot: strings.TrimSpace(configRoot),
		Force:      force,
	})
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime uninstall: %v\n", err)
		return ExitUsage
	}
	report.Text(stdout, "uninstall")
	if report.Refused() {
		return ExitNotReady
	}
	return ExitOK
}

// parseInit turns the command line into options, printing usage on a mistake.
func parseInit(args []string, stderr io.Writer) (initOptions, error) {
	parsed := initOptions{}

	fs := flag.NewFlagSet("flowhub runtime init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addCheckFlags(fs, &parsed.checkFlags)
	fs.BoolVar(&parsed.check, "check", false, "report only: write nothing and register nothing")
	fs.BoolVar(&parsed.force, "force", false, "overwrite files FlowHub did not write")
	fs.StringVar(&parsed.server, "server", "", "control plane to register with (with --token and --name)")
	fs.StringVar(&parsed.token, "token", "", "invite token from the control plane's invite command")
	fs.StringVar(&parsed.name, "name", "", "the runtime name the control plane invited")
	fs.StringVar(&parsed.advertise, "advertise", "", "how the control plane reaches this host")
	if err := fs.Parse(args); err != nil {
		return initOptions{}, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "flowhub runtime init: unexpected argument %q\n", fs.Arg(0))
		return initOptions{}, fmt.Errorf("unexpected argument")
	}
	if err := parsed.checkFlags.finish("flowhub runtime init", stderr); err != nil {
		return initOptions{}, err
	}

	parsed.server = strings.TrimRight(strings.TrimSpace(parsed.server), "/")
	parsed.token = strings.TrimSpace(parsed.token)
	parsed.advertise = strings.TrimSpace(parsed.advertise)
	parsed.name = strings.TrimSpace(parsed.name)
	parsed.version = Version
	return parsed, nil
}

// parseDoctor turns the command line into options for the re-check.
func parseDoctor(args []string, stderr io.Writer) (doctorOptions, error) {
	parsed := doctorOptions{}

	fs := flag.NewFlagSet("flowhub runtime doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addCheckFlags(fs, &parsed.checkFlags)
	fs.BoolVar(&parsed.push, "push", false, "send the report to the control plane")
	fs.StringVar(&parsed.server, "server", "", "control plane to push to (default: the one in the stored identity)")
	if err := fs.Parse(args); err != nil {
		return doctorOptions{}, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "flowhub runtime doctor: unexpected argument %q\n", fs.Arg(0))
		return doctorOptions{}, fmt.Errorf("unexpected argument")
	}
	if err := parsed.checkFlags.finish("flowhub runtime doctor", stderr); err != nil {
		return doctorOptions{}, err
	}
	parsed.server = strings.TrimRight(strings.TrimSpace(parsed.server), "/")
	return parsed, nil
}

// listFlag collects a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(value string) error {
	*l = append(*l, value)
	return nil
}
