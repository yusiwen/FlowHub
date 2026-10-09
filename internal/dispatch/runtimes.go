package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/runtimes"
)

// DefaultRuntimeName is the name used for the runtime configured purely through
// the environment (`FLOWHUB_OPENCODE_URL`). A host that was never enrolled still
// records a runtime on every task, so the binding is uniform and the removal
// checks have something to compare.
// DefaultRuntimeName is the runtime that exists without being enrolled, built from
// FLOWHUB_OPENCODE_URL. It is spelled once, in projectmap, because the credentials
// loader needs the same name to allow a credentials entry for it (a runtime the
// routing table never declares).
const DefaultRuntimeName = projectmap.DefaultRuntimeName

// pickRuntime decides which host serves a task.
//
// Two questions, deliberately not conflated:
//
//   - a task that is already bound goes to its runtime, always, even if the
//     project's eligibility set or the host's health has changed. A session cannot
//     move between hosts without losing the analysis and the plan, so a bound task
//     whose runtime is gone is refused rather than re-homed;
//   - a new task picks among the runtimes its project names, by policy, and the
//     choice is logged with the numbers it was made from.
func (d *Dispatcher) pickRuntime(ctx context.Context, task registry.Task, entry *projectmap.Entry) (runtimeBinding, error) {
	if name := strings.TrimSpace(task.Runtime); name != "" {
		binding, ok := d.bindingFor(name)
		if !ok {
			// The binding is absolute: a task whose host is gone is refused, not
			// re-homed, because a new session elsewhere loses the plan.
			return runtimeBinding{}, fmt.Errorf("task is bound to runtime %q, which is no longer configured", name)
		}
		return binding, nil
	}

	loads := d.rankRuntimes(entry)
	if len(loads) == 0 {
		if set := runtimeSet(entry); len(set) > 0 {
			return runtimeBinding{}, fmt.Errorf("this project may only be served by %s, and none of them can take work: %s",
				strings.Join(set, ", "), d.describeUnavailable(set))
		}
		return runtimeBinding{}, errors.New("no runtime is configured")
	}

	var failures []string
	for _, load := range loads {
		if err := d.probe(ctx, load.Binding); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", load.Binding.Name, err))
			continue
		}
		d.log.Info("runtime chosen",
			"runtime", load.Binding.Name, "policy", load.Policy, "eligible", len(loads),
			"busy", load.Busy, "running", load.Running, "max_concurrent", load.Max,
			"in_flight", load.InFlight, "active_tasks", load.Active,
			"candidates", strings.Join(candidateNames(loads), ","))
		return load.Binding, nil
	}
	return runtimeBinding{}, fmt.Errorf("no runtime answered: %s", strings.Join(failures, "; "))
}

// runtimeLoad is one candidate together with what the policy ranked it by. The
// numbers travel into the choice log line, so "why did this land there" is
// answerable from the log without re-deriving the state.
type runtimeLoad struct {
	Binding  runtimeBinding
	Policy   string
	InFlight int
	// Busy counts the work already committed to this runtime: deliveries accepted and
	// not yet started, plus the tasks being served. It is what makes a burst spread —
	// two deliveries that arrive together both reach the router before either has
	// created a task row, so without it the second would see an idle runtime and
	// follow the first onto the same host.
	Busy int
	// Running is how many tasks this host is serving right now, and Max is its
	// breadth; Full is the two compared. A full host is still a candidate — the
	// delivery waits in its scheduler — but a host with a free worker is preferred,
	// so raising the breadth actually uses it.
	Running int
	Max     int
	Full    bool
	Active  int
}

// rankRuntimes lists the runtimes a new task may use, ordered by the project's
// policy.
//
//   - first-healthy keeps the declared order, which is what makes the list a
//     failover order — including onto a host that is already at its breadth, because
//     the order is the operator's preference and not a load-balancing one;
//   - spread sorts by "has a free worker", then by work already in flight, then by
//     active tasks in the registry, then by name, so a second machine is used instead
//     of being idle and a fresh registry still behaves predictably.
func (d *Dispatcher) rankRuntimes(entry *projectmap.Entry) []runtimeLoad {
	policy := projectmap.PolicySpread
	if entry != nil {
		policy = entry.Policy()
	}

	candidates := d.runtimeCandidates(entry)
	loads := make([]runtimeLoad, 0, len(candidates))
	for _, candidate := range candidates {
		breadth := d.breadthFor(candidate)
		running := d.runningFor(candidate.Name)
		loads = append(loads, runtimeLoad{
			Binding:  candidate,
			Policy:   policy,
			InFlight: d.inFlightFor(candidate.Name),
			Busy:     d.busyFor(candidate.Name),
			Running:  running,
			Max:      breadth,
			Full:     running >= breadth,
			Active:   len(d.boundTasksFor(candidate.Name)),
		})
	}
	if policy == projectmap.PolicySpread {
		sort.SliceStable(loads, func(a, b int) bool {
			// A free worker first: "in flight or queued right now" is still what orders
			// the rest, exactly as the ADR orders it.
			if loads[a].Full != loads[b].Full {
				return !loads[a].Full
			}
			if loads[a].Busy != loads[b].Busy {
				return loads[a].Busy < loads[b].Busy
			}
			if loads[a].Active != loads[b].Active {
				return loads[a].Active < loads[b].Active
			}
			return loads[a].Binding.Name < loads[b].Binding.Name
		})
	}
	return loads
}

// candidateNames renders the ranked candidates for the log line.
func candidateNames(loads []runtimeLoad) []string {
	names := make([]string, 0, len(loads))
	for _, load := range loads {
		names = append(names, load.Binding.Name)
	}
	return names
}

// describeUnavailable explains why none of a project's declared runtimes can take
// work. "Not enrolled" and "revoked" are different problems with different fixes,
// and the delivery log is where an operator sees them.
func (d *Dispatcher) describeUnavailable(names []string) string {
	reasons := make([]string, 0, len(names))
	for _, name := range names {
		if d.opts.Runtimes == nil {
			reasons = append(reasons, name+" is not enrolled (no runtime inventory is configured)")
			continue
		}
		runtime, ok := d.opts.Runtimes.Get(name)
		switch {
		case !ok:
			reasons = append(reasons, name+" is not enrolled")
		case runtime.State != runtimes.StateActive:
			reasons = append(reasons, fmt.Sprintf("%s is %s", name, runtime.State))
		default:
			reasons = append(reasons, name+" has no usable address")
		}
	}
	return strings.Join(reasons, "; ")
}

// runtimeSet is a project's declared runtime names, empty when it names none.
func runtimeSet(entry *projectmap.Entry) []string {
	if entry == nil {
		return nil
	}
	return entry.RuntimeSet()
}

// runtimeCandidates lists the runtimes a new task may use, in a deterministic
// order: the project's declared set when it names one, otherwise the enrolled ones
// by name, then the environment-configured default.
func (d *Dispatcher) runtimeCandidates(entry *projectmap.Entry) []runtimeBinding {
	all := d.allRuntimes()
	set := runtimeSet(entry)
	if len(set) == 0 {
		return all
	}
	byName := make(map[string]runtimeBinding, len(all))
	for _, candidate := range all {
		byName[candidate.Name] = candidate
	}
	var out []runtimeBinding
	for _, name := range set {
		if candidate, ok := byName[name]; ok {
			out = append(out, candidate)
		}
	}
	return out
}

// allRuntimes is every runtime the process could use, in a deterministic order:
// the active enrolled ones by name, then the ones the configuration file declares
// that enrolment does not already cover, then the environment-configured default
// when nothing else exists.
//
// Three sources of truth, each answering a different question: enrolment says which
// hosts were prepared and verified (and where they are now), the file says what
// policy to ask of a host, and the environment is the outermost default for a
// single-host deployment.
func (d *Dispatcher) allRuntimes() []runtimeBinding {
	declared := make(map[string]DeclaredRuntime, len(d.opts.Declared))
	for _, runtime := range d.opts.Declared {
		declared[runtime.Name] = runtime
	}

	var out []runtimeBinding
	if d.opts.Runtimes != nil {
		active := d.opts.Runtimes.Active()
		sort.Slice(active, func(a, b int) bool { return active[a].Name < active[b].Name })
		for _, runtime := range active {
			url := strings.TrimSpace(runtime.Advertise)
			if url == "" {
				url = strings.TrimSpace(runtime.URL)
			}
			if url == "" {
				continue
			}
			// Enrolment answers where the host is; the file answers what to ask of it.
			policy := declared[runtime.Name]
			out = append(out, runtimeBinding{
				Name:         runtime.Name,
				AgentProfile: runtime.AgentProfile,
				URL:          url,
				Runtime:      d.runtimeFor(runtime.Name, url),
				Models:       runtime.Models,
				HostMax:      runtime.MaxConcurrent,
				Declared:     policy,
			})
		}
	}

	names := make([]string, 0, len(d.opts.Declared))
	for _, runtime := range d.opts.Declared {
		names = append(names, runtime.Name)
	}
	sort.Strings(names)
	enrolled := map[string]bool{}
	for _, binding := range out {
		enrolled[binding.Name] = true
	}
	for _, name := range names {
		runtime := declared[name]
		if enrolled[name] || strings.TrimSpace(runtime.URL) == "" {
			continue
		}
		out = append(out, runtimeBinding{
			Name:         runtime.Name,
			AgentProfile: runtime.Agent,
			URL:          runtime.URL,
			Runtime:      d.runtimeFor(runtime.Name, runtime.URL),
			Declared:     runtime,
		})
	}

	if len(out) == 0 && d.opts.DefaultRuntime != nil {
		out = append(out, runtimeBinding{
			Name: DefaultRuntimeName,
			// The environment-configured runtime has no enrolled profile, so the
			// process-wide default is the honest answer here.
			AgentProfile: d.opts.Agent,
			Runtime:      d.opts.DefaultRuntime,
			Declared:     declared[DefaultRuntimeName],
		})
	}
	return out
}

// DeclaredRuntimes projects the configuration file's `runtimes` blocks into the policy
// the dispatcher applies, with `fallbackDeadline` used for a block that names none.
//
// It lives here rather than in `main` because the tests need the same projection: a
// harness that left Declared empty silently ran every host at a breadth of one, so the
// per-task serialization the scheduler exists for was never exercised (found while
// fixing the flaky timing assertions, #17). One projection means a test and a start
// cannot disagree about what the file declared.
func DeclaredRuntimes(projects *projectmap.Map, fallbackDeadline time.Duration) []DeclaredRuntime {
	if projects == nil {
		return nil
	}
	out := make([]DeclaredRuntime, 0, len(projects.Runtimes()))
	for _, name := range projects.Runtimes() {
		block, _ := projects.RuntimeBlock(name)
		deadline := fallbackDeadline
		if declared := strings.TrimSpace(block.Deadline); declared != "" {
			if parsed, err := time.ParseDuration(declared); err == nil {
				deadline = parsed
			}
		}
		out = append(out, DeclaredRuntime{
			Name:          name,
			URL:           strings.TrimSpace(block.URL),
			Agent:         strings.TrimSpace(block.Agent),
			Model:         strings.TrimSpace(block.Model),
			Deadline:      deadline,
			MaxConcurrent: block.MaxConcurrentTasks(),
		})
	}
	return out
}

// runtimeBinding is a runtime together with the adapter that talks to it.
type runtimeBinding struct {
	Name string
	// AgentProfile is the name inside the agent product (e.g. "devops"). It is
	// never the product name: passing "opencode" as an opencode agent makes the
	// server fall back to its own default agent, which is a looser one.
	AgentProfile string
	URL          string
	// Runtime is the adapter, injected by the control plane: the dispatcher names
	// no agent product (ADR 0001).
	Runtime agent.Runtime
	// Models is what the runtime reported its agent profiles pin, keyed by profile
	// name. Empty for the environment-configured runtime, which never enrolled.
	Models map[string]string
	// HostMax is the breadth the host itself claimed at enrolment: how many tasks it
	// says it can serve at once. Zero means the host made no claim.
	HostMax int
	// Declared is the configuration file's policy for this host, when the file names
	// it. Enrolment reports facts; this decides what to ask.
	Declared DeclaredRuntime
}

// maxConcurrent is how many distinct tasks this host may serve at the same time: the
// operator's number from the configuration file, and never more than the host itself
// claimed it can take. An absent file value means 1, so a host only gets a wider
// breadth when someone wrote one down (ADR 0003 §6).
func (b runtimeBinding) maxConcurrent() int {
	breadth := 1
	if b.Declared.MaxConcurrent > 0 {
		breadth = b.Declared.MaxConcurrent
	}
	if b.HostMax > 0 && b.HostMax < breadth {
		breadth = b.HostMax
	}
	return breadth
}

// deadline is how long one turn on this runtime may run: the file's per-runtime
// value, then the process default.
func (b runtimeBinding) deadline(fallback time.Duration) time.Duration {
	if b.Declared.Deadline > 0 {
		return b.Declared.Deadline
	}
	return fallback
}

// declaredModel is the model the configuration file pins for this host, if any.
func (b runtimeBinding) declaredModel() string { return strings.TrimSpace(b.Declared.Model) }

// modelFor resolves the model a turn on this runtime should pin for the given
// agent profile. The runtime reports these at enrolment, so this is the model the
// capability check verified against the provider catalogue — not whatever the
// agent server happens to have cached.
func (b runtimeBinding) modelFor(agent string) string {
	return strings.TrimSpace(b.Models[strings.TrimSpace(agent)])
}

// bindingFor resolves a name to a usable binding, or reports that it is gone.
func (d *Dispatcher) bindingFor(name string) (runtimeBinding, bool) {
	for _, candidate := range d.allRuntimes() {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return runtimeBinding{}, false
}

// cachedRuntime is one host's adapter together with the address it was built for.
// The address is kept so that re-enrolling a name at a new address takes effect
// without a restart, which is the whole point of the control API applying its
// mutations to the running process.
type cachedRuntime struct {
	URL     string
	Runtime agent.Runtime
}

// runtimeFor returns the adapter for one host, building it on first use and
// rebuilding it when the host's address changes.
func (d *Dispatcher) runtimeFor(name, url string) agent.Runtime {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	if cached, ok := d.runtimes[name]; ok && cached.URL == url {
		return cached.Runtime
	}
	built := d.opts.NewRuntime(name, url)
	d.runtimes[name] = cachedRuntime{URL: url, Runtime: built}
	return built
}

// probe asks a runtime whether it is alive, with a short bound: a candidate that
// needs longer than this is not a candidate for this task.
func (d *Dispatcher) probe(ctx context.Context, binding runtimeBinding) error {
	probeCtx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
	defer cancel()
	if _, err := binding.Runtime.Health(probeCtx); err != nil {
		return err
	}
	return nil
}

// runtimeProbeTimeout bounds one liveness check. It is deliberately much shorter
// than a turn: this only decides whether to hand work to a host.
const runtimeProbeTimeout = 5 * time.Second

// BoundTasks reports the tasks an operator would strand by removing a runtime. It
// is the admission check the control API runs before revoking a host.
func (d *Dispatcher) BoundTasks(name string) []string { return d.boundTasksFor(name) }

// boundTasksFor implements the inventory's view of "which tasks would this break":
// every task bound to the runtime whose state is not terminal.
func (d *Dispatcher) boundTasksFor(name string) []string {
	if d.opts.Registry == nil {
		return nil
	}
	var out []string
	for _, task := range d.opts.Registry.List() {
		if task.Runtime != name {
			continue
		}
		switch task.State {
		case registry.StateDone, registry.StateFailed:
			continue
		}
		out = append(out, task.Qualified())
	}
	sort.Strings(out)
	return out
}

// RuntimeProblems checks every project's declared runtime set against the
// inventory.
//
// The two answers are deliberately different, because the fixes are. A name that
// is not enrolled yet is the normal state between the operator's `invite` and the
// host's `init`, and refusing to start would block the very command that resolves
// it — so it is a warning. A name that was *revoked* is a decision someone made,
// so a project still pointing at it is a configuration error and stops the start.
func RuntimeProblems(projects *projectmap.Map, inventory *runtimes.Inventory) (problems, warnings []string) {
	if projects == nil {
		return nil, nil
	}
	for _, entry := range projects.Entries() {
		if !entry.IsEnabled() {
			continue
		}
		for _, name := range entry.RuntimeSet() {
			if inventory == nil {
				warnings = append(warnings, fmt.Sprintf(
					"%s names runtime %s, but this process has no runtime inventory; set FLOWHUB_ADMIN_ADDR and enrol it",
					entry.Label(), name))
				continue
			}
			runtime, ok := inventory.Get(name)
			switch {
			case !ok:
				warnings = append(warnings, fmt.Sprintf(
					"%s names runtime %s, which is not enrolled yet; run `flowhub runtime invite %s` and then `flowhub runtime init` on that host",
					entry.Label(), name, name))
			case runtime.State == runtimes.StateRevoked:
				problems = append(problems, fmt.Sprintf(
					"%s names runtime %s, which was revoked; remove it from the project's runtime set or enrol the host again",
					entry.Label(), name))
			case runtime.State != runtimes.StateActive:
				warnings = append(warnings, fmt.Sprintf(
					"%s names runtime %s, which is %s and cannot take work yet",
					entry.Label(), name, runtime.State))
			}
		}
	}
	sort.Strings(problems)
	sort.Strings(warnings)
	return problems, warnings
}
