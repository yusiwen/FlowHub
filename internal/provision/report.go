package provision

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// JSON renders the report as indented JSON. It is the machine-readable form the
// control plane will receive at registration (ADR 0002 step 3).
func (r *Report) JSON() ([]byte, error) {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Text renders the same report for a human.
//
// The order is fixed and the maps are sorted, because a report that reorders
// itself between runs is impossible to diff — and diffing two hosts' reports is
// the main way this document is used.
func (r *Report) Text(w io.Writer) {
	fmt.Fprintf(w, "host       : %s (%s/%s)\n", orUnknown(r.Host.Hostname), r.Host.OS, r.Host.Arch)
	fmt.Fprintf(w, "ran as     : %s (uid %s) home=%s\n", orUnknown(r.Invoker.User), orUnknown(r.Invoker.UID), orUnknown(r.Invoker.Home))

	fmt.Fprintln(w, "\nagent")
	if r.Agent == nil {
		fmt.Fprintln(w, "  (not checked)")
	} else {
		version := r.Agent.Version
		if version == "" {
			version = "version unknown"
		}
		status := "missing from PATH"
		if r.Agent.Path != "" {
			status = r.Agent.Path
		}
		fmt.Fprintf(w, "  %-9s %-20s %s\n", r.Agent.Name, version, status)
		state := "does not exist"
		switch {
		case r.Agent.ConfigDirExists:
			state = "exists"
		case r.Agent.ConfigDirError != "":
			state = "could not be inspected: " + r.Agent.ConfigDirError
		}
		fmt.Fprintf(w, "  %-9s %-20s %s\n", "config", r.Agent.ConfigDir, state)
	}

	fmt.Fprintln(w, "\ntools")
	for _, name := range sortedKeys(r.Tools) {
		tool := r.Tools[name]
		auth := ""
		if tool.Authenticated != nil {
			auth = "auth=no"
			if *tool.Authenticated {
				auth = "auth=yes"
			}
		}
		required := ""
		if tool.Required {
			required = "required"
		}
		fmt.Fprintf(w, "  %-5s %-24s %-9s %s %s\n",
			name, orDash(tool.Version), auth, orDash(tool.Path), required)
		if tool.ConfigFile != "" {
			fmt.Fprintf(w, "        config: %s\n", tool.ConfigFile)
		}
		if len(tool.Accounts) > 0 {
			fmt.Fprintf(w, "        accounts: %s\n", strings.Join(tool.Accounts, ", "))
		}
	}

	if len(r.Forges) > 0 {
		fmt.Fprintln(w, "\nforges")
		for _, host := range sortedKeys(r.Forges) {
			forge := r.Forges[host]
			via := forge.Tool
			if via == "" {
				via = "no tool needed"
			}
			ready := "not ready"
			if forge.Kind == ForgeNone || forge.ToolReady {
				ready = "ready"
			}
			fmt.Fprintf(w, "  %-24s %-7s via %-4s %s\n", host, forge.Kind, via, ready)
		}
	}

	if len(r.Repos) > 0 {
		fmt.Fprintln(w, "\nrepositories")
		for _, repo := range r.Repos {
			fmt.Fprintf(w, "  %s\n", repo.Remote)
			if repo.Clone != "" {
				fmt.Fprintf(w, "    clone  %s  exists=%t work_tree=%t origin_ok=%t\n",
					repo.Clone, repo.CloneExists, repo.IsWorkTree, repo.OriginMatches)
			}
			read := "failed (git ls-remote)"
			if repo.LSRemote {
				read = "ok (git ls-remote)"
			}
			fmt.Fprintf(w, "    read   %s\n", read)
			write := "ok (git push --dry-run)"
			if repo.PushDryRun != "ok" {
				write = repo.PushDryRun
			}
			fmt.Fprintf(w, "    write  %s\n", write)
		}
	}

	if len(r.EnvPresent) > 0 {
		fmt.Fprintln(w, "\nenvironment (presence only; values are never read)")
		for _, name := range sortedKeys(r.EnvPresent) {
			fmt.Fprintf(w, "  %-24s %s\n", name, presentAbsent(r.EnvPresent[name]))
		}
	}

	if len(r.Failures) > 0 {
		fmt.Fprintf(w, "\nfailures (%d)\n", len(r.Failures))
		for _, failure := range r.Failures {
			fmt.Fprintf(w, "  - %s\n", failure)
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintf(w, "\nwarnings (%d)\n", len(r.Warnings))
		for _, warning := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}

	fmt.Fprintln(w)
	if r.OK {
		fmt.Fprintln(w, "result: OK")
	} else {
		fmt.Fprintf(w, "result: FAILED (%d failure(s), %d warning(s))\n", len(r.Failures), len(r.Warnings))
	}
	// Nothing was installed and nothing was registered: this command only reports.
	// Saying so here is what stops a reader assuming --check did more than it did.
	fmt.Fprintln(w, "note: nothing was written or registered by this check")
	if len(r.Artifacts) == 0 {
		fmt.Fprintln(w, "note: artifact installation and the manifest are not implemented yet")
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func presentAbsent(present bool) string {
	if present {
		return "present"
	}
	return "ABSENT"
}
