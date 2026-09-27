package dispatch

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
	agentruntime "github.com/yusiwen/flowhub/internal/agent/opencode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/runtimes"
	"github.com/yusiwen/flowhub/internal/source/youtrack"
	"github.com/yusiwen/flowhub/internal/workspace"
	"github.com/yusiwen/flowhub/internal/workspace/localworktree"
)

// newTestRuntime builds the real runtime adapter for one host, the way `main` wires
// it. These tests deliberately drive the real adapter against a fake server rather
// than a stub runtime: the seam is only proven by driving it.
func newTestRuntime(name, url string, timeout time.Duration) agent.Runtime {
	return agentruntime.NewRuntime(
		agentruntime.New(agentruntime.Options{BaseURL: url, Timeout: timeout}),
		agentruntime.RuntimeOptions{Name: name},
	)
}

// runtimeFactory returns the runtime constructor a dispatcher option needs.
func runtimeFactory(timeout time.Duration) func(name, url string) agent.Runtime {
	return func(name, url string) agent.Runtime { return newTestRuntime(name, url, timeout) }
}

// testWorkspaces is the workspace constructor a dispatcher option needs: the
// co-located provider, which is what a single-host deployment uses.
func testWorkspaces(base string) (workspace.Workspace, error) {
	return localworktree.New(localworktree.Options{Base: base})
}

// TestRuntimeAgentNeverUsesTheProductName is a regression test for a live run:
// the enrolled runtime reported agent "opencode" (the product), which was passed
// to the session as the agent to run. opencode then fell back to its own default
// agent, silently dropping the step budget and permission block that the installed
// `devops` profile carries.
func TestRuntimeAgentNeverUsesTheProductName(t *testing.T) {
	binding := runtimeBinding{Name: "builder-a", AgentProfile: "devops"}
	entry := &projectmap.Entry{}

	cases := map[string]struct {
		entry   *projectmap.Entry
		binding runtimeBinding
		want    string
	}{
		"the entry wins": {
			entry: &projectmap.Entry{Agent: "flowhub-analyst"}, binding: binding, want: "flowhub-analyst",
		},
		"the enrolled profile is next": {entry: entry, binding: binding, want: "devops"},
		"the process default is last": {
			entry: entry, binding: runtimeBinding{Name: "builder-a"}, want: "devops",
		},
		"a nil entry is fine": {entry: nil, binding: binding, want: "devops"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := runtimeAgent(tc.entry, tc.binding, "devops"); got != tc.want {
				t.Fatalf("runtimeAgent = %q, want %q", got, tc.want)
			}
			if got := runtimeAgent(tc.entry, tc.binding, "devops"); strings.EqualFold(got, "opencode") {
				t.Fatal("the agent product name was used as the agent profile")
			}
		})
	}
}

// TestModelForUsesTheRuntimeReport is a regression test for a live run on
// 2026-09-25: the installed profile pinned "deepseek/deepseek-flash", the
// enrolment check verified it against the provider catalogue and reported it as
// available, and every turn still died with ProviderModelNotFoundError. The
// running agent server had cached the profile's *previous* model id, so leaving
// the model to the server meant the check verified something the turn never used.
func TestModelForUsesTheRuntimeReport(t *testing.T) {
	binding := runtimeBinding{
		Name:         "builder-a",
		AgentProfile: "devops",
		Models:       map[string]string{"devops": "deepseek/deepseek-flash"},
	}

	cases := map[string]struct {
		binding runtimeBinding
		agent   string
		want    string
	}{
		"the reported model is used":      {binding: binding, agent: "devops", want: "deepseek/deepseek-flash"},
		"whitespace is not significant":   {binding: binding, agent: " devops ", want: "deepseek/deepseek-flash"},
		"an unknown profile falls back":   {binding: binding, agent: "flowhub-analyst", want: ""},
		"no report at all falls back":     {binding: runtimeBinding{Name: "builder-a"}, agent: "devops", want: ""},
		"an environment runtime has none": {binding: runtimeBinding{Name: DefaultRuntimeName}, agent: "devops", want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.binding.modelFor(tc.agent); got != tc.want {
				t.Fatalf("modelFor(%q) = %q, want %q", tc.agent, got, tc.want)
			}
		})
	}
}

// testProbeServer answers the liveness probe every candidate goes through. It is a
// real server because pickRuntime probes before it commits to a runtime, which is
// exactly the behaviour the selection tests are about.
func testProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true,"version":"fake"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// testInventory builds an inventory holding the named runtimes, all active and
// advertised at the given address.
func testInventory(t *testing.T, advertise string, names ...string) *runtimes.Inventory {
	t.Helper()
	inventory, err := runtimes.Open(filepath.Join(t.TempDir(), "runtimes.json"))
	if err != nil {
		t.Fatalf("runtimes.Open: %v", err)
	}
	now := time.Now().UTC()
	for _, name := range names {
		token, _, err := inventory.Invite(name, nil, time.Hour)
		if err != nil {
			t.Fatalf("Invite(%s): %v", name, err)
		}
		claim := runtimes.Claim{
			Name: name, URL: advertise, Advertise: advertise,
			Agent: "opencode", AgentProfile: "devops",
			Models: map[string]string{"devops": "deepseek/deepseek-flash"},
		}
		// A nil prober skips the activation probe: these tests are about selection,
		// and no HTTP server is involved.
		if _, _, err := inventory.Register(context.Background(), claim, token, nil, now); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	return inventory
}

// testProjects loads a one-entry routing table carrying the given extra JSON.
func testProjects(t *testing.T, extra string) *projectmap.Map {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"projects":[{"youtrack_key":"TEST","repo":{"path":"/tmp/test-repo"},"worktrees":"/tmp/test-wt"` + extra + `}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}
	return projects
}

// testDispatcherFor builds a dispatcher over the given inventory, routing table and
// registry, which is all the selection code reads.
func testDispatcherFor(t *testing.T, inventory *runtimes.Inventory, projects *projectmap.Map, reg *registry.Registry) *Dispatcher {
	t.Helper()
	dispatcher, err := New(Options{
		// A default runtime is required by New even when every runtime is enrolled: it
		// is the fallback for a host that was never enrolled. Nothing here calls it.
		NewRuntime:     runtimeFactory(time.Second),
		DefaultRuntime: newTestRuntime(DefaultRuntimeName, "http://127.0.0.1:1", time.Second),
		NewWorkspace:   testWorkspaces,
		Source:         youtrack.New(rules.Policy{}),
		Runtimes:       inventory,
		Registry:       reg,
		Projects:       projects,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agent:          "devops",
		Deadline:       time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return dispatcher
}

// TestSpreadBalancesNewTasks is ADR 0001 step 5's acceptance case: with two
// runtimes and `spread`, consecutive new tasks land on different hosts, because the
// second one is ranked by how much work the first already carries.
func TestSpreadBalancesNewTasks(t *testing.T) {
	inventory := testInventory(t, "http://127.0.0.1:1", "builder-a", "builder-b")
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TEST-1", "TEST-2"} {
		if _, _, err := reg.Ensure(youtrack.SourceName, key, func(task *registry.Task) {
			task.Runtime = "builder-a"
			task.State = registry.StateAnalyzing
		}); err != nil {
			t.Fatal(err)
		}
	}

	entry := testProjects(t, `,"runtimes":["builder-a","builder-b"]`).Entries()[0]
	dispatcher := testDispatcherFor(t, inventory, testProjects(t, `,"runtimes":["builder-a","builder-b"]`), reg)

	loads := dispatcher.rankRuntimes(entry)
	if len(loads) != 2 {
		t.Fatalf("loads = %+v, want both runtimes eligible", loads)
	}
	if loads[0].Binding.Name != "builder-b" {
		t.Fatalf("first candidate = %s, want builder-b (builder-a already has %d tasks)",
			loads[0].Binding.Name, loads[0].Active)
	}
	if loads[0].Active != 0 || loads[1].Active != 2 {
		t.Fatalf("active counts = %d/%d, want 0 then 2", loads[0].Active, loads[1].Active)
	}
	if loads[0].Policy != projectmap.PolicySpread {
		t.Fatalf("policy = %q, want spread", loads[0].Policy)
	}
}

// TestFirstHealthyKeepsTheDeclaredOrder is the other policy: the list is a failover
// order, so the first entry takes everything while it answers.
func TestFirstHealthyKeepsTheDeclaredOrder(t *testing.T) {
	inventory := testInventory(t, "http://127.0.0.1:1", "builder-a", "builder-b")
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Ensure(youtrack.SourceName, "TEST-1", func(task *registry.Task) {
		task.Runtime = "builder-a"
		task.State = registry.StateExecuting
	}); err != nil {
		t.Fatal(err)
	}

	projects := testProjects(t, `,"runtimes":["builder-a","builder-b"],"runtime_policy":"first-healthy"`)
	dispatcher := testDispatcherFor(t, inventory, projects, reg)
	loads := dispatcher.rankRuntimes(projects.Entries()[0])
	if len(loads) != 2 || loads[0].Binding.Name != "builder-a" {
		t.Fatalf("loads = %+v, want builder-a first even though builder-b is idle", loads)
	}
}

// TestProjectRuntimeSetFiltersCandidates: a project that names runtimes may only be
// served by those, so an idle host it does not name is not a candidate.
func TestProjectRuntimeSetFiltersCandidates(t *testing.T) {
	inventory := testInventory(t, "http://127.0.0.1:1", "builder-a", "builder-b")
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	projects := testProjects(t, `,"runtime":"builder-b"`)
	dispatcher := testDispatcherFor(t, inventory, projects, reg)

	loads := dispatcher.rankRuntimes(projects.Entries()[0])
	if len(loads) != 1 || loads[0].Binding.Name != "builder-b" {
		t.Fatalf("loads = %+v, want only builder-b", loads)
	}
	// A project that names none may use any of them.
	open := testProjects(t, "")
	loads = dispatcher.rankRuntimes(open.Entries()[0])
	if len(loads) != 2 {
		t.Fatalf("loads = %+v, want both runtimes when the project names none", loads)
	}
}

// TestBoundTaskStaysOnItsRuntimeWhateverThePolicySays is the rule that matters most:
// a session cannot move between hosts, so a bound task ignores the project's
// eligibility set entirely.
func TestBoundTaskStaysOnItsRuntimeWhateverThePolicySays(t *testing.T) {
	inventory := testInventory(t, "http://127.0.0.1:1", "builder-a", "builder-b")
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The project now names only builder-b; the task was created on builder-a.
	projects := testProjects(t, `,"runtime":"builder-b"`)
	dispatcher := testDispatcherFor(t, inventory, projects, reg)

	binding, err := dispatcher.pickRuntime(context.Background(), registry.Task{Key: "TEST-9", Runtime: "builder-a"}, projects.Entries()[0])
	if err != nil {
		t.Fatalf("pickRuntime: %v", err)
	}
	if binding.Name != "builder-a" {
		t.Fatalf("binding = %s, want the task's own runtime", binding.Name)
	}

	// A bound runtime that is gone is refused, and the refusal names it.
	if _, err := dispatcher.pickRuntime(context.Background(), registry.Task{Key: "TEST-9", Runtime: "ghost"}, projects.Entries()[0]); err == nil {
		t.Fatal("a task bound to an unconfigured runtime was accepted")
	} else if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("the refusal does not name the runtime: %v", err)
	}
}

// TestUnavailableRuntimeSetExplainsItself: "this project may only be served by X"
// has to say why X cannot take work, because that is the whole message an operator
// gets when a delivery is dropped.
func TestUnavailableRuntimeSetExplainsItself(t *testing.T) {
	inventory := testInventory(t, testProbeServer(t).URL, "builder-a")
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	projects := testProjects(t, `,"runtimes":["ghost","builder-a"]`)
	dispatcher := testDispatcherFor(t, inventory, projects, reg)

	// builder-a is eligible, so the delivery is served by it.
	binding, err := dispatcher.pickRuntime(context.Background(), registry.Task{Key: "TEST-10"}, projects.Entries()[0])
	if err != nil {
		t.Fatalf("pickRuntime: %v", err)
	}
	if binding.Name != "builder-a" {
		t.Fatalf("binding = %s, want the one eligible runtime", binding.Name)
	}

	// With only the unknown name, nothing is eligible and the reason says so.
	missing := testProjects(t, `,"runtime":"ghost"`)
	_, err = dispatcher.pickRuntime(context.Background(), registry.Task{Key: "TEST-11"}, missing.Entries()[0])
	if err == nil {
		t.Fatal("a project whose only runtime is unknown was served")
	}
	for _, want := range []string{"ghost", "not enrolled"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

// TestRuntimeProblemsSeparatesTyposFromNotYetEnrolled: a revoked name is an
// operator's decision and stops the start, while a name that is merely not enrolled
// yet is the normal state between `invite` and `init` and must not block it.
func TestRuntimeProblemsSeparatesTyposFromNotYetEnrolled(t *testing.T) {
	inventory := testInventory(t, "http://127.0.0.1:1", "builder-a", "dead")
	if _, _, err := inventory.Remove("dead", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inventory.Invite("later", nil, time.Hour); err != nil {
		t.Fatal(err)
	}

	projects := testProjects(t, `,"runtimes":["builder-a","dead","later","ghost"]`)
	problems, warnings := RuntimeProblems(projects, inventory)
	if len(problems) != 1 || !strings.Contains(problems[0], "dead") || !strings.Contains(problems[0], "revoked") {
		t.Fatalf("problems = %v, want the revoked runtime to stop the start", problems)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want one for the pending and one for the unknown runtime", warnings)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "later") || !strings.Contains(joined, "ghost") {
		t.Fatalf("warnings = %v", warnings)
	}

	// A project that names only usable runtimes produces nothing.
	clean := testProjects(t, `,"runtime":"builder-a"`)
	if problems, warnings := RuntimeProblems(clean, inventory); len(problems) != 0 || len(warnings) != 0 {
		t.Fatalf("problems=%v warnings=%v, want none", problems, warnings)
	}
	// A disabled entry is never matched, so it is not validated.
	disabled := testProjects(t, `,"runtime":"ghost","enabled":false`)
	if problems, warnings := RuntimeProblems(disabled, inventory); len(problems) != 0 || len(warnings) != 0 {
		t.Fatalf("problems=%v warnings=%v, want a disabled entry to be ignored", problems, warnings)
	}
}

// TestDeclaredPolicyAppliesToAnEnrolledRuntime is a regression test for a live bug:
// a `runtimes.<name>` block that carries policy but no URL is how an *enrolled* host
// gets its agent, model and deadline from the configuration file, and the first
// version of the wiring dropped those blocks — so the file's `deadline: 20m` never
// reached the turn and the process default was used instead.
func TestDeclaredPolicyAppliesToAnEnrolledRuntime(t *testing.T) {
	inventory := testInventory(t, "http://127.0.0.1:1", "builder-a")
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	projects := testProjects(t, `,"runtimes":["builder-a"]`)
	dispatcher, err := New(Options{
		NewRuntime:     runtimeFactory(time.Second),
		DefaultRuntime: newTestRuntime(DefaultRuntimeName, "http://127.0.0.1:1", time.Second),
		NewWorkspace:   testWorkspaces,
		Source:         youtrack.New(rules.Policy{}),
		Runtimes:       inventory,
		Registry:       reg,
		Projects:       projects,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Declared: []DeclaredRuntime{{
			// No URL: the inventory is where this host's address comes from.
			Name: "builder-a", Agent: "declared-agent", Model: "deepseek/declared", Deadline: 20 * time.Minute,
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	binding, ok := dispatcher.bindingFor("builder-a")
	if !ok {
		t.Fatal("the enrolled runtime is not a candidate")
	}
	if binding.URL != "http://127.0.0.1:1" {
		t.Fatalf("URL = %q, want the enrolled address", binding.URL)
	}
	if got := binding.deadline(time.Minute); got != 20*time.Minute {
		t.Fatalf("deadline = %s, want the file's 20m", got)
	}
	if got := binding.declaredModel(); got != "deepseek/declared" {
		t.Fatalf("declared model = %q", got)
	}
	if got := runtimeAgent(projects.Entries()[0], binding, "env-agent"); got != "devops" {
		t.Fatalf("agent = %q, want the profile the host attested at enrolment", got)
	}
	// A project's own agent still wins over both, and a runtime that was never
	// enrolled has nothing attested, so the file supplies both values.
	entry := projects.Entries()[0]
	entry.Agent = "project-agent"
	if got := runtimeAgent(entry, binding, "env-agent"); got != "project-agent" {
		t.Fatalf("agent = %q, want the project's override", got)
	}
	entry.Agent = ""

	standalone, err := New(Options{
		NewRuntime:     runtimeFactory(time.Second),
		DefaultRuntime: newTestRuntime(DefaultRuntimeName, "http://127.0.0.1:1", time.Second),
		NewWorkspace:   testWorkspaces,
		Source:         youtrack.New(rules.Policy{}),
		Runtimes:       testInventory(t, "http://127.0.0.1:1"),
		Registry:       reg,
		Projects:       projects,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Declared: []DeclaredRuntime{{
			Name: "only-declared", URL: "http://127.0.0.1:9", Agent: "declared-agent", Model: "deepseek/declared",
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sole, ok := standalone.bindingFor("only-declared")
	if !ok {
		t.Fatal("a declared runtime with its own address is not a candidate")
	}
	if sole.AgentProfile != "declared-agent" || sole.declaredModel() != "deepseek/declared" {
		t.Fatalf("declared runtime = %+v, want the file's agent and model", sole)
	}
	if got := runtimeAgent(nil, sole, "env-agent"); got != "declared-agent" {
		t.Fatalf("agent = %q, want the file's profile when nothing was enrolled", got)
	}
}
