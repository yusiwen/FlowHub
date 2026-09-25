package runtimes

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Exit statuses for the control-plane commands. They mirror the data-plane ones:
// 0 done, 2 refused or not reachable, 1 a usage mistake.
const (
	ExitOK      = 0
	ExitUsage   = 1
	ExitRefused = 2
)

// CLIOptions resolves where the control plane is and how to authenticate.
type CLIOptions struct {
	Server string
	Token  string
}

// DefaultCLIOptions reads the environment the service itself uses, so an operator
// on the control-plane host does not have to repeat anything.
func DefaultCLIOptions(getenv func(string) string) CLIOptions {
	if getenv == nil {
		getenv = os.Getenv
	}
	options := CLIOptions{Token: strings.TrimSpace(getenv("FLOWHUB_ADMIN_TOKEN"))}
	if addr := strings.TrimSpace(getenv("FLOWHUB_ADMIN_ADDR")); addr != "" {
		options.Server = "http://" + addr
	}
	return options
}

// CLI runs one of the control-plane subcommands and returns a process exit status.
//
// These commands run on the service host: they reach the running service over its
// admin listener rather than editing the inventory file, because the service is
// the only writer and a mutation must take effect without a restart.
func CLI(subcommand string, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	defaults := DefaultCLIOptions(getenv)

	switch subcommand {
	case "invite":
		return inviteCommand(args, stdout, stderr, defaults)
	case "list":
		return listCommand(args, stdout, stderr, defaults)
	case "show":
		return showCommand(args, stdout, stderr, defaults)
	case "remove":
		return removeCommand(args, stdout, stderr, defaults)
	case "rotate":
		return rotateCommand(args, stdout, stderr, defaults)
	default:
		fmt.Fprintf(stderr, "flowhub runtime: unknown control subcommand %q\n\n", subcommand)
		fmt.Fprint(stderr, controlUsage)
		return ExitUsage
	}
}

const controlUsage = `flowhub runtime — inspect and manage agent hosts from the service host

  flowhub runtime invite <name> [--projects A,B] [--ttl 30m]
  flowhub runtime list [--json]
  flowhub runtime show <name>
  flowhub runtime remove <name> [--force]
  flowhub runtime rotate <name>

  --server <url>   control API address (default http://$FLOWHUB_ADMIN_ADDR)
  --token <token>  admin token (default $FLOWHUB_ADMIN_TOKEN)

These commands talk to the running service, so the change applies without a
restart. On the host that will run work, enrol with the printed token:

  flowhub runtime init --server <url> --token <token>
`

func addCommonFlags(fs *flag.FlagSet, defaults *CLIOptions) {
	fs.StringVar(&defaults.Server, "server", defaults.Server, "control API address")
	fs.StringVar(&defaults.Token, "token", defaults.Token, "admin token")
}

// parseArgs parses a subcommand's arguments, allowing flags on either side of the
// positional ones.
//
// The flag package stops at the first non-flag argument, so the documented forms
// `runtime invite <name> --projects A,B --ttl 30m` and `runtime remove <name>
// --force` would silently ignore everything after the name: an invite recorded
// with the default TTL and no projects at all, and a removal that refuses even
// though the operator passed --force. Measured 2026-09-25; both are worse than a
// usage error because they look like success.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
		if len(args) == 0 || !strings.HasPrefix(args[0], "-") {
			// Nothing but positionals left; parsing again would loop forever.
			return append(positional, args...), nil
		}
	}
}

// firstPositional returns the first positional argument, or "" when there is none.
func firstPositional(positional []string) string {
	if len(positional) == 0 {
		return ""
	}
	return positional[0]
}

func clientFor(options CLIOptions, stderr io.Writer) (*Client, bool) {
	if options.Server == "" {
		fmt.Fprintln(stderr, "flowhub runtime: no control plane address is configured")
		fmt.Fprintln(stderr, "set FLOWHUB_ADMIN_ADDR on this host, or pass --server")
		return nil, false
	}
	if options.Token == "" {
		fmt.Fprintln(stderr, "flowhub runtime: no admin token is configured (set FLOWHUB_ADMIN_TOKEN, or pass --token)")
		return nil, false
	}
	return NewClient(options.Server, options.Token), true
}

func inviteCommand(args []string, stdout, stderr io.Writer, defaults CLIOptions) int {
	var projects string
	var ttl time.Duration
	fs := flag.NewFlagSet("flowhub runtime invite", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addCommonFlags(fs, &defaults)
	fs.StringVar(&projects, "projects", "", "comma separated project keys this host will serve")
	fs.DurationVar(&ttl, "ttl", DefaultInviteTTL, "how long the invite token is valid")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "flowhub runtime invite: a runtime name is required")
		return ExitUsage
	}
	client, ok := clientFor(defaults, stderr)
	if !ok {
		return ExitUsage
	}

	invite, err := client.Invite(context.Background(), name, splitList(projects), ttl)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime invite: %v\n", err)
		return ExitRefused
	}

	fmt.Fprintf(stdout, "runtime %s invited (expires %s)\n", invite.Runtime.Name, invite.Runtime.Invite.ExpiresAt.Format(time.RFC3339))
	if len(invite.Runtime.Invite.Projects) > 0 {
		fmt.Fprintf(stdout, "projects: %s\n", strings.Join(invite.Runtime.Invite.Projects, ", "))
	}
	fmt.Fprintf(stdout, "\ntoken (shown once):\n  %s\n", invite.Token)
	fmt.Fprintf(stdout, "\non the host that will run work:\n  flowhub runtime init --server %s --name %s --token %s --advertise <how this service reaches that host>\n",
		defaults.Server, invite.Runtime.Name, invite.Token)
	fmt.Fprintln(stdout, "the control plane probes --advertise before activating the runtime, so a network mistake fails at enrolment instead of at the first task")
	return ExitOK
}

func listCommand(args []string, stdout, stderr io.Writer, defaults CLIOptions) int {
	var asJSON bool
	fs := flag.NewFlagSet("flowhub runtime list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addCommonFlags(fs, &defaults)
	fs.BoolVar(&asJSON, "json", false, "print JSON")
	if _, err := parseArgs(fs, args); err != nil {
		return ExitUsage
	}
	client, ok := clientFor(defaults, stderr)
	if !ok {
		return ExitUsage
	}

	runtimes, err := client.List(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime list: %v\n", err)
		return ExitRefused
	}
	if asJSON {
		encoded, err := json.MarshalIndent(map[string]any{"runtimes": runtimes}, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "flowhub runtime list: %v\n", err)
			return ExitRefused
		}
		fmt.Fprintf(stdout, "%s\n", encoded)
		return ExitOK
	}
	if len(runtimes) == 0 {
		fmt.Fprintln(stdout, "no runtimes are registered")
		return ExitOK
	}
	fmt.Fprintf(stdout, "%-20s %-9s %-28s %-16s %-34s %s\n", "NAME", "STATE", "ADVERTISE", "AGENT", "MODELS", "LAST SEEN")
	for _, runtime := range runtimes {
		lastSeen := "-"
		if !runtime.LastSeen.IsZero() {
			lastSeen = runtime.LastSeen.Format(time.RFC3339)
		}
		agent := runtime.Agent
		if runtime.AgentVersion != "" {
			agent += " " + runtime.AgentVersion
		}
		fmt.Fprintf(stdout, "%-20s %-9s %-28s %-16s %-34s %s\n",
			runtime.Name, runtime.State, orDash(runtime.Advertise), orDash(agent), orDash(modelsOf(runtime)), lastSeen)
	}
	return ExitOK
}

func showCommand(args []string, stdout, stderr io.Writer, defaults CLIOptions) int {
	fs := flag.NewFlagSet("flowhub runtime show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addCommonFlags(fs, &defaults)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "flowhub runtime show: a runtime name is required")
		return ExitUsage
	}
	client, ok := clientFor(defaults, stderr)
	if !ok {
		return ExitUsage
	}

	runtime, bound, err := client.Show(context.Background(), name)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime show: %v\n", err)
		return ExitRefused
	}
	encoded, err := json.MarshalIndent(runtime, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime show: %v\n", err)
		return ExitRefused
	}
	fmt.Fprintf(stdout, "%s\n", encoded)
	if models := modelsOf(runtime); models != "" {
		fmt.Fprintf(stdout, "models: %s\n", models)
	}
	if len(bound) > 0 {
		fmt.Fprintf(stdout, "bound tasks: %s\n", strings.Join(bound, ", "))
	}
	return ExitOK
}

func removeCommand(args []string, stdout, stderr io.Writer, defaults CLIOptions) int {
	var force bool
	fs := flag.NewFlagSet("flowhub runtime remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addCommonFlags(fs, &defaults)
	fs.BoolVar(&force, "force", false, "remove even when tasks are still bound to it")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "flowhub runtime remove: a runtime name is required")
		return ExitUsage
	}
	client, ok := clientFor(defaults, stderr)
	if !ok {
		return ExitUsage
	}

	bound, err := client.Remove(context.Background(), name, force)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime remove: %v\n", err)
		if len(bound) > 0 {
			fmt.Fprintf(stderr, "tasks bound to %s: %s\n", name, strings.Join(bound, ", "))
			fmt.Fprintln(stderr, "those tasks cannot move to another host; --force removes the runtime and leaves them to be refused one by one")
		}
		return ExitRefused
	}
	fmt.Fprintf(stdout, "runtime %s removed\n", name)
	if len(bound) > 0 {
		fmt.Fprintf(stdout, "note: %d task(s) are still bound and will be refused at their next turn: %s\n",
			len(bound), strings.Join(bound, ", "))
	}
	return ExitOK
}

func rotateCommand(args []string, stdout, stderr io.Writer, defaults CLIOptions) int {
	fs := flag.NewFlagSet("flowhub runtime rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addCommonFlags(fs, &defaults)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return ExitUsage
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "flowhub runtime rotate: a runtime name is required")
		return ExitUsage
	}
	client, ok := clientFor(defaults, stderr)
	if !ok {
		return ExitUsage
	}

	secret, err := client.Rotate(context.Background(), name)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub runtime rotate: %v\n", err)
		return ExitRefused
	}
	fmt.Fprintf(stdout, "secret for %s (shown once; the previous one no longer works):\n  %s\n", name, secret)
	return ExitOK
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// modelsOf renders the models a runtime reported, keyed by agent profile, in a
// stable order so two runs are comparable. It answers the operator's question
// "which model will this host run", which otherwise needs a look inside the
// runtime's own agent files on the other machine.
func modelsOf(runtime Runtime) string {
	if len(runtime.Models) == 0 {
		return ""
	}
	names := make([]string, 0, len(runtime.Models))
	for name := range runtime.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+runtime.Models[name])
	}
	return strings.Join(parts, ",")
}
