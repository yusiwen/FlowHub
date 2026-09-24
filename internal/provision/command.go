package provision

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

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
	case "doctor", "uninstall", "invite", "list", "show", "remove", "reconfigure", "rotate":
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

  flowhub runtime init --check [flags]
      Report what this host can and cannot do. Writes nothing, registers nothing.

      --agent <name>          agent runtime to look for (default opencode)
      --repo <remote>[=<clone>]
                              a repository this host must serve; repeatable. The
                              clone path enables the write check (git push --dry-run)
      --forge <host>=<kind>   which tooling a git host needs: github, gitea or none;
                              repeatable. github.com defaults to github
      --require-env <NAME>    environment variable that must be present; repeatable.
                              Only presence is checked, never the value
      --json                  print the machine-readable report instead of the summary
      --strict                treat warnings as failures
      --timeout <duration>    bound a single command (default 20s)

  Not implemented yet, in this order: artifact installation and uninstall, the
  runtime inventory and enrollment (docs/adr/0002 steps 2 and 3).
`)
}

// initOptions is the parsed form of `runtime init`.
type initOptions struct {
	Options
	check   bool
	json    bool
	timeout time.Duration
}

func initCommand(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseInit(args, stderr)
	if err != nil {
		return ExitUsage
	}
	if !parsed.check {
		fmt.Fprintln(stderr, "flowhub runtime init: installation is not implemented in this build")
		fmt.Fprintln(stderr, "re-run with --check to inspect this host without changing it.")
		return ExitUsage
	}

	runner := ExecRunner{Timeout: parsed.timeout}
	report, err := Check(context.Background(), runner, parsed.Options)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime init: %v\n", err)
		return ExitUsage
	}

	if parsed.json {
		encoded, err := report.JSON()
		if err != nil {
			fmt.Fprintf(stderr, "flowhub runtime init: cannot encode the report: %v\n", err)
			return ExitUsage
		}
		if _, err := stdout.Write(encoded); err != nil {
			fmt.Fprintf(stderr, "flowhub runtime init: %v\n", err)
			return ExitUsage
		}
	} else {
		report.Text(stdout)
	}

	if !report.OK {
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
