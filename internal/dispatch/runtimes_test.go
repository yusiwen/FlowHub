package dispatch

import (
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/projectmap"
)

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
