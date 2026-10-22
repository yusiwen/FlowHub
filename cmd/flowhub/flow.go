package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yusiwen/flowhub/internal/config"
	"github.com/yusiwen/flowhub/internal/flow/builtin"
	"github.com/yusiwen/flowhub/internal/flow/engine"
	"github.com/yusiwen/flowhub/internal/flow/mode"
)

// runFlowCommand is the local operator surface for the builtin flow. It runs
// before the receiver configuration is loaded, like `runtime`, because it only
// needs the data directory and the embedded definition.
func runFlowCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		flowUsage(stderr)
		return 1
	}
	dataDir := os.Getenv("FLOWHUB_DATA_DIR")
	if dataDir == "" {
		dataDir = config.DefaultDataDir
	}
	compiled, raw, err := builtin.Load()
	if err != nil {
		fmt.Fprintf(stderr, "flowhub flow: %v\n", err)
		return 1
	}
	modes, err := mode.Open(dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "flowhub flow: %v\n", err)
		return 1
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			flowUsage(stderr)
			return 1
		}
		runs, err := engine.NewStore(dataDir)
		if err != nil {
			fmt.Fprintf(stderr, "flowhub flow: %v\n", err)
			return 1
		}
		list, err := runs.List()
		if err != nil {
			fmt.Fprintf(stderr, "flowhub flow: %v\n", err)
			return 1
		}
		revision := fmt.Sprintf("%x", sha256.Sum256(raw))
		if len(revision) > 12 {
			revision = revision[:12]
		}
		entry, _ := modes.Entry(compiled.Definition.ID)
		projects := strings.Join(entry.Projects, ",")
		if projects == "" {
			projects = "all"
		}
		fmt.Fprintf(stdout, "%-24s %-8s %-12s %-10s %s\n", "ID", "MODE", "REVISION", "PROJECTS", "RUNS")
		fmt.Fprintf(stdout, "%-24s %-8s %-12s %-10s %d\n", compiled.Definition.ID, modes.Mode(compiled.Definition.ID), revision, projects, len(list))
		return 0
	case "show":
		if len(args) != 2 || args[1] != compiled.Definition.ID {
			fmt.Fprintln(stderr, "flowhub flow show: unknown flow")
			return 1
		}
		fmt.Fprintln(stdout, string(raw))
		return 0
	case "mode":
		if len(args) < 3 {
			flowUsage(stderr)
			return 1
		}
		id, next := args[1], args[2]
		if id != compiled.Definition.ID {
			fmt.Fprintln(stderr, "flowhub flow mode: unknown flow")
			return 1
		}
		switch next {
		case mode.Off, mode.Shadow:
		case mode.On:
			fmt.Fprintln(stderr, "flowhub flow mode: on needs the task service (M3-1); use off or shadow")
			return 1
		default:
			fmt.Fprintln(stderr, "flowhub flow mode: mode must be off or shadow")
			return 1
		}
		projects := []string{}
		for args = args[3:]; len(args) > 0; args = args[1:] {
			if args[0] != "--project" || len(args) < 2 {
				fmt.Fprintln(stderr, "flowhub flow mode: use --project <KEY> for each project")
				return 1
			}
			projects = append(projects, args[1])
			args = args[1:]
		}
		if err := modes.Set(id, next, projects, "cli"); err != nil {
			fmt.Fprintf(stderr, "flowhub flow: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: %s\n", id, next)
		return 0
	default:
		flowUsage(stderr)
		return 1
	}
}

func flowUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: flowhub flow list|show <id>|mode <id> <off|shadow> [--project KEY]...")
}
