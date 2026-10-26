package projectmap

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yusiwen/flowhub/internal/agent"
	"github.com/yusiwen/flowhub/internal/workspace"
)

// agentNamePattern keeps the agent name safe to hand to a runtime as a JSON field
// and to read back in logs.
var agentNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate checks the part of the mapping this package owns: the values a routing
// entry declares about *work*, not about the host that will do it. An empty result
// means the table is safe to route with.
//
// The filesystem checks that used to live here moved behind the workspace seam (ADR
// 0001 step 2): whether a repository exists, is a git work tree, and has the origin
// the entry declares is a fact of the machine that will prepare the checkout, not of
// the configuration file. WorkspaceEntries and the provider's Validator answer it.
//
// Every problem blocks startup: a mapping that claims to route but names an
// impossible agent or a malformed model is exactly how a turn dies after the
// operator believes the hub is running. Disabled entries are skipped, because they
// may deliberately point at a checkout that is not present on this host.
func (m *Map) Validate() []string {
	if m == nil {
		return nil
	}
	var problems []string
	for _, entry := range m.entries {
		if !entry.IsEnabled() {
			continue
		}
		problems = append(problems, entry.validate()...)
	}
	return problems
}

// LockProblems reports the enabled entries of one source that cannot receive a
// delivery because their entry locks are incomplete.
//
// The check is scoped to the source this process serves: the lock material is that
// source's addressing scheme, and a table may declare projects for a source this
// binary does not receive from (ADR 0015). Without a key a delivery cannot be
// attributed to a project, and without a token it cannot be authenticated, so both
// are refused at startup instead of failing one delivery at a time.
func (m *Map) LockProblems(source string) []string {
	if m == nil {
		return nil
	}
	wanted := strings.ToLower(strings.TrimSpace(source))
	v1Hint := ""
	if m.translated {
		v1Hint = " (a version 1 file cannot express entry locks; rewrite it as version 2 and give each project a source object)"
	}
	var problems []string
	for _, entry := range m.entries {
		if !entry.IsEnabled() || entry.Source != wanted {
			continue
		}
		if entry.hookKey == "" {
			problems = append(problems, fmt.Sprintf(
				"project %s: source.hook_key is required (the URL key routes a delivery to this project before its body is read); write \"source\": {\"name\": %q, \"hook_key\": ..., \"token\": ...}%s",
				entry.Label(), entry.Source, v1Hint))
		}
		if entry.token == "" {
			problems = append(problems, fmt.Sprintf(
				"project %s: source.token is required (the header token authenticates the delivery after its key has selected the project)%s",
				entry.Label(), v1Hint))
		}
	}
	return problems
}

func (e *Entry) validate() []string {
	label := e.Label()
	var problems []string
	if strings.TrimSpace(e.Repo.Path) == "" {
		problems = append(problems, fmt.Sprintf("%s: repo.path is not configured", label))
	}
	if e.Agent != "" && !agentNamePattern.MatchString(e.Agent) {
		problems = append(problems, fmt.Sprintf("%s: agent %q is not a valid agent name", label, e.Agent))
	}
	// A runtime takes a provider id and a model id separately, so a bare model name
	// cannot be honoured. Rejected here rather than silently ignored: a task that
	// quietly runs on a different model than the operator configured is worse than
	// one that does not start.
	if e.Model != "" {
		if err := agent.ValidateModel(e.Model); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", label, err))
		}
	}
	return problems
}

// WorkspaceEntries renders the enabled entries as the plain data a workspace
// provider validates, with the process-wide fallback checkout directory already
// applied.
//
// The conversion lives here because this package owns the configuration format: the
// seam stays free of it, so a provider never learns how a routing table is written.
func (m *Map) WorkspaceEntries(fallbackBase string) []workspace.Entry {
	if m == nil {
		return nil
	}
	fallbackBase = strings.TrimSpace(fallbackBase)
	entries := make([]workspace.Entry, 0, len(m.entries))
	for _, entry := range m.entries {
		if !entry.IsEnabled() {
			continue
		}
		base := strings.TrimSpace(entry.Worktrees)
		if base == "" {
			// The fallback is canonicalized like every other path Load sees: the
			// provider's containment check compares it against the repository, and
			// a symlinked spelling of the same directory must not slip past it.
			base = canonicalize(fallbackBase)
		}
		entries = append(entries, workspace.Entry{
			Label:         entry.Label(),
			Repo:          entry.Repo.Path,
			Remote:        entry.Repo.Remote,
			DefaultBranch: entry.Repo.DefaultBranch,
			Base:          base,
		})
	}
	return entries
}

// Report renders the routing table for `flowhub -print-config`.
func (m *Map) Report() string {
	var b strings.Builder
	source := "<unset>"
	if m != nil && m.Path() != "" {
		source = m.Path()
	}
	fmt.Fprintf(&b, "config_file:        %s\n", source)
	fmt.Fprintf(&b, "config_format:      %s\n", m.FormatNote())

	if m == nil || m.Len() == 0 {
		fmt.Fprintf(&b, "projects:           <none> — every delivery is refused as bad_url_key (there is no project key to attribute it to)\n")
		return b.String()
	}

	fmt.Fprintf(&b, "runtime_policy:     %s\n", m.SelectionPolicy())
	fmt.Fprintf(&b, "projects:           %d mapping(s)", m.Len())
	if routable := m.Routable(); routable != m.Len() {
		fmt.Fprintf(&b, ", %d routable", routable)
	}
	fmt.Fprintln(&b)
	for _, entry := range m.entries {
		state := ""
		if !entry.IsEnabled() {
			state = " [disabled]"
		}
		aliases := ""
		if keys := entry.Keys(); len(keys) > 1 {
			// Several tracker projects may share one repository. Showing the primary
			// key as the label and the rest as aliases keeps the source qualification
			// on the entry rather than on every key.
			aliases = " (also: " + strings.Join(keys[1:], ", ") + ")"
		}
		fmt.Fprintf(&b, "  %-14s -> %s%s%s\n", entry.Label(), entry.Repo.Path, aliases, state)
		if entry.Repo.Remote != "" {
			fmt.Fprintf(&b, "  %-14s    remote %s\n", "", entry.Repo.Remote)
		}
		if entry.Repo.DefaultBranch != "" {
			fmt.Fprintf(&b, "  %-14s    branch %s\n", "", entry.Repo.DefaultBranch)
		}
		if entry.Worktrees != "" {
			fmt.Fprintf(&b, "  %-14s    worktrees %s\n", "", entry.Worktrees)
		}
		if entry.IsEnabled() {
			// Presence only: the values are secrets, and a report that can be pasted
			// into a ticket must not carry them (ADR 0015).
			if entry.hookKey != "" && entry.token != "" {
				fmt.Fprintf(&b, "  %-14s    locks url_key,header_token\n", "")
			} else {
				fmt.Fprintf(&b, "  %-14s    locks MISSING (see lock_problem)\n", "")
			}
		}
		if set := entry.RuntimeSet(); len(set) > 0 {
			fmt.Fprintf(&b, "  %-14s    runtimes %s (%s)\n", "", strings.Join(set, ","), entry.Policy())
		} else {
			fmt.Fprintf(&b, "  %-14s    runtimes any (%s)\n", "", entry.Policy())
		}
		var details []string
		if entry.Agent != "" {
			details = append(details, "agent="+entry.Agent)
		}
		if entry.Model != "" {
			details = append(details, "model="+entry.Model)
		}
		if len(entry.Authors) > 0 {
			details = append(details, "authors="+strings.Join(entry.Authors, ","))
		}
		if len(details) > 0 {
			fmt.Fprintf(&b, "  %-14s    %s\n", "", strings.Join(details, " "))
		}
	}
	return b.String()
}
