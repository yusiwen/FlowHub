package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/opencode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/runtimes"
)

// DefaultRuntimeName is the name used for the runtime configured purely through
// the environment (`FLOWHUB_OPENCODE_URL`). A host that was never enrolled still
// records a runtime on every task, so the binding is uniform and the removal
// checks have something to compare.
const DefaultRuntimeName = "default"

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
	Active   int
}

// rankRuntimes lists the runtimes a new task may use, ordered by the project's
// policy.
//
//   - first-healthy keeps the declared order, which is what makes the list a
//     failover order;
//   - spread sorts by work already in flight, then by active tasks in the registry,
//     then by name, so a second machine is used instead of being idle and a fresh
//     registry still behaves predictably.
func (d *Dispatcher) rankRuntimes(entry *projectmap.Entry) []runtimeLoad {
	policy := projectmap.PolicySpread
	if entry != nil {
		policy = entry.Policy()
	}

	candidates := d.runtimeCandidates(entry)
	loads := make([]runtimeLoad, 0, len(candidates))
	for _, candidate := range candidates {
		loads = append(loads, runtimeLoad{
			Binding:  candidate,
			Policy:   policy,
			InFlight: d.inFlightFor(candidate.Name),
			Active:   len(d.boundTasksFor(candidate.Name)),
		})
	}
	if policy == projectmap.PolicySpread {
		sort.SliceStable(loads, func(a, b int) bool {
			if loads[a].InFlight != loads[b].InFlight {
				return loads[a].InFlight < loads[b].InFlight
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

// allRuntimes is every runtime the process could use: the active enrolled ones by
// name, then the environment-configured default.
func (d *Dispatcher) allRuntimes() []runtimeBinding {
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
			out = append(out, runtimeBinding{
				Name:         runtime.Name,
				AgentProfile: runtime.AgentProfile,
				URL:          url,
				Client:       d.clientFor(runtime.Name, url),
				Models:       runtime.Models,
			})
		}
	}
	if len(out) == 0 && d.opts.Client != nil {
		out = append(out, runtimeBinding{
			Name: DefaultRuntimeName,
			// The environment-configured runtime has no enrolled profile, so the
			// process-wide default is the honest answer here.
			AgentProfile: d.opts.Agent,
			URL:          d.opts.Client.BaseURL(),
			Client:       d.opts.Client,
		})
	}
	return out
}

// runtimeBinding is a runtime together with the client that talks to it.
type runtimeBinding struct {
	Name string
	// AgentProfile is the name inside the agent product (e.g. "devops"). It is
	// never the product name: passing "opencode" as an opencode agent makes the
	// server fall back to its own default agent, which is a looser one.
	AgentProfile string
	URL          string
	Client       *opencode.Client
	// Models is what the runtime reported its agent profiles pin, keyed by profile
	// name. Empty for the environment-configured runtime, which never enrolled.
	Models map[string]string
}

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

// clientFor caches one client per runtime: they are stateless, but a client per
// call would build a new connection pool every turn.
func (d *Dispatcher) clientFor(name, url string) *opencode.Client {
	if client, ok := d.clients[name]; ok && client.BaseURL() == url {
		return client
	}
	client := opencode.New(opencode.Options{BaseURL: url, Timeout: d.opts.Deadline})
	d.clients[name] = client
	return client
}

// probe asks a runtime whether it is alive, with a short bound: a candidate that
// needs longer than this is not a candidate for this task.
func (d *Dispatcher) probe(ctx context.Context, binding runtimeBinding) error {
	probeCtx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
	defer cancel()
	if _, err := binding.Client.Health(probeCtx); err != nil {
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
		out = append(out, task.Key)
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
					entry.YouTrackKey, name))
				continue
			}
			runtime, ok := inventory.Get(name)
			switch {
			case !ok:
				warnings = append(warnings, fmt.Sprintf(
					"%s names runtime %s, which is not enrolled yet; run `flowhub runtime invite %s` and then `flowhub runtime init` on that host",
					entry.YouTrackKey, name, name))
			case runtime.State == runtimes.StateRevoked:
				problems = append(problems, fmt.Sprintf(
					"%s names runtime %s, which was revoked; remove it from the project's runtime set or enrol the host again",
					entry.YouTrackKey, name))
			case runtime.State != runtimes.StateActive:
				warnings = append(warnings, fmt.Sprintf(
					"%s names runtime %s, which is %s and cannot take work yet",
					entry.YouTrackKey, name, runtime.State))
			}
		}
	}
	sort.Strings(problems)
	sort.Strings(warnings)
	return problems, warnings
}
