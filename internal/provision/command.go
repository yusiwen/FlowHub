package provision

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
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
	case "doctor", "invite", "list", "show", "remove", "reconfigure", "rotate":
		fmt.Fprintf(stderr, "flowhub runtime %s: not implemented in this build\n", args[0])
		fmt.Fprintln(stderr, "this build implements `flowhub runtime init --check` (see docs/adr/0002).")
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

  flowhub runtime uninstall [--agent <name>] [--config-root <dir>] [--force]
      Remove exactly the files the manifest records. A file a human edited is
      kept unless --force is given.

  What init installs comes from the binary, never from the network; the manifest
  records a hash of every file so a re-run is a no-op and a hand edit is refused
  with a diff. Enrollment in the control plane is not implemented yet
  (docs/adr/0002 step 3): this command installs and reports, it does not register.
`)
}

// initOptions is the parsed form of `runtime init`.
type initOptions struct {
	Options
	check      bool
	json       bool
	force      bool
	configRoot string
	version    string
	timeout    time.Duration
}

func initCommand(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseInit(args, stderr)
	if err != nil {
		return ExitUsage
	}

	runner := ExecRunner{Timeout: parsed.timeout}
	report, err := Check(context.Background(), runner, parsed.Options)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime init: %v\n", err)
		return ExitUsage
	}

	// Under --json, stdout carries only the JSON document, so everything written
	// for a human goes to stderr. A report that has to be parsed must not have a
	// file listing glued in front of it.
	human := stdout
	if parsed.json {
		human = stderr
	}

	// The agent has to exist before anything can be installed into its tree. The
	// other gaps are reported and gated on the exit status without blocking the
	// installation: a missing forge tool is something an operator fixes while the
	// files are already in place, and a re-run is a no-op.
	if report.Agent == nil || report.Agent.Path == "" {
		printCheck(report, parsed.json, stdout, stderr)
		fmt.Fprintln(stderr, "flowhub runtime init: nothing was installed because the agent runtime is missing")
		return ExitNotReady
	}

	install := InstallOptions{
		Agent:          parsed.Agent,
		ConfigRoot:     parsed.configRoot,
		FlowHubVersion: parsed.version,
		Force:          parsed.force,
	}

	printCheck(report, parsed.json, stdout, stderr)

	refused := false
	if parsed.check {
		// --check reports; it must not write, so the plan is computed and rendered
		// without applying it.
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
		outcome, err := Install(runner, install)
		if err != nil {
			fmt.Fprintf(stderr, "flowhub runtime init: %v\n", err)
			return ExitUsage
		}
		fmt.Fprintf(human, "\nfiles FlowHub manages\n")
		outcome.Text(human, "install")
		fmt.Fprintln(human, "note: this host is not registered with a control plane yet (docs/adr/0002 step 3)")
		refused = outcome.Refused()
	}

	if !report.OK || refused {
		return ExitNotReady
	}
	return ExitOK
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
	var (
		agent       string
		repos       listFlag
		forges      listFlag
		requiredEnv listFlag
	)
	parsed := initOptions{}

	fs := flag.NewFlagSet("flowhub runtime init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&parsed.check, "check", false, "report only: write nothing and register nothing")
	fs.StringVar(&agent, "agent", AgentOpenCode, "agent runtime to look for")
	fs.Var(&repos, "repo", "repository to check: <remote>[=<clone path>]; repeatable")
	fs.Var(&forges, "forge", "forge tooling for a git host: <host>=<github|gitea|none>; repeatable")
	fs.Var(&requiredEnv, "require-env", "environment variable that must be present; repeatable")
	fs.StringVar(&parsed.configRoot, "config-root", "", "agent configuration directory")
	fs.BoolVar(&parsed.force, "force", false, "overwrite files FlowHub did not write")
	fs.BoolVar(&parsed.json, "json", false, "print the machine-readable report")
	fs.BoolVar(&parsed.Strict, "strict", false, "treat warnings as failures")
	fs.DurationVar(&parsed.timeout, "timeout", DefaultCommandTimeout, "bound a single command")
	if err := fs.Parse(args); err != nil {
		return initOptions{}, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "flowhub runtime init: unexpected argument %q\n", fs.Arg(0))
		return initOptions{}, fmt.Errorf("unexpected argument")
	}
	if parsed.timeout <= 0 {
		fmt.Fprintln(stderr, "flowhub runtime init: --timeout must be positive")
		return initOptions{}, fmt.Errorf("bad timeout")
	}

	parsed.Agent = strings.TrimSpace(agent)
	parsed.configRoot = strings.TrimSpace(parsed.configRoot)
	parsed.version = Version
	parsed.Forges = map[string]string{}
	for _, entry := range forges {
		host, kind, found := strings.Cut(entry, "=")
		host, kind = strings.TrimSpace(host), strings.TrimSpace(kind)
		if !found || host == "" || kind == "" {
			fmt.Fprintf(stderr, "flowhub runtime init: --forge %q: want <host>=<github|gitea|none>\n", entry)
			return initOptions{}, fmt.Errorf("bad forge")
		}
		parsed.Forges[strings.ToLower(host)] = strings.ToLower(kind)
	}
	for _, entry := range repos {
		remote, clone, _ := strings.Cut(entry, "=")
		remote, clone = strings.TrimSpace(remote), strings.TrimSpace(clone)
		if remote == "" {
			fmt.Fprintf(stderr, "flowhub runtime init: --repo %q: the remote is required\n", entry)
			return initOptions{}, fmt.Errorf("bad repo")
		}
		parsed.Repos = append(parsed.Repos, RepoExpectation{Remote: remote, Clone: clone})
	}
	for _, name := range requiredEnv {
		if name = strings.TrimSpace(name); name != "" {
			parsed.RequiredEnv = append(parsed.RequiredEnv, name)
		}
	}
	return parsed, nil
}

// listFlag collects a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(value string) error {
	*l = append(*l, value)
	return nil
}
