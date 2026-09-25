package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/opencode"
	"github.com/yusiwen/flowhub/internal/registry"
)

// DefaultRuntimeName is the name used for the runtime configured purely through
// the environment (`FLOWHUB_OPENCODE_URL`). A host that was never enrolled still
// records a runtime on every task, so the binding is uniform and the removal
// checks have something to compare.
const DefaultRuntimeName = "default"

// pickRuntime decides which host serves a task.
//
// This is the interim form of ADR 0001's addressing: a task already bound stays
// where it is (a session cannot move), and a new task takes the first healthy
// runtime in name order. Per-project runtime lists and the spread/first-healthy
// policy arrive with ADR 0001 step 5, which replaces this function only.
func (d *Dispatcher) pickRuntime(ctx context.Context, task registry.Task) (runtimeBinding, error) {
	if name := strings.TrimSpace(task.Runtime); name != "" {
		binding, ok := d.bindingFor(name)
		if !ok {
			// The binding is absolute: a task whose host is gone is refused, not
			// re-homed, because a new session elsewhere loses the plan.
			return runtimeBinding{}, fmt.Errorf("task is bound to runtime %q, which is no longer configured", name)
		}
		return binding, nil
	}

	candidates := d.runtimeCandidates()
	if len(candidates) == 0 {
		return runtimeBinding{}, errors.New("no runtime is configured")
	}
	var failures []string
	for _, candidate := range candidates {
		if err := d.probe(ctx, candidate); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", candidate.Name, err))
			continue
		}
		return candidate, nil
	}
	return runtimeBinding{}, fmt.Errorf("no runtime answered: %s", strings.Join(failures, "; "))
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

// runtimeCandidates lists the runtimes a new task may use, in a deterministic
// order: the enrolled ones by name, then the environment-configured default.
func (d *Dispatcher) runtimeCandidates() []runtimeBinding {
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

// bindingFor resolves a name to a usable binding, or reports that it is gone.
func (d *Dispatcher) bindingFor(name string) (runtimeBinding, bool) {
	for _, candidate := range d.runtimeCandidates() {
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
