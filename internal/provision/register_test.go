package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/runtimes"
)

// TestProfileModelsOf covers what the host tells the control plane about the
// models it will run. The dispatcher pins that value on every turn, so a profile
// the report left out falls back to the agent server's own — possibly stale —
// resolution.
func TestProfileModelsOf(t *testing.T) {
	tests := []struct {
		name    string
		report  *Report
		want    map[string]string
		wantNil bool
	}{
		{name: "no report", report: nil, wantNil: true},
		{name: "no profiles", report: &Report{}, wantNil: true},
		{
			name: "one profile",
			report: &Report{Profiles: []ProfileCheck{
				{Agent: "devops", Model: "deepseek/deepseek-flash", Available: true},
			}},
			want: map[string]string{"devops": "deepseek/deepseek-flash"},
		},
		{
			name: "a blank model is skipped, not carried as empty",
			report: &Report{Profiles: []ProfileCheck{
				{Agent: "devops", Model: "  "},
				{Agent: "analyst", Model: "deepseek/deepseek-v4-pro"},
			}},
			want: map[string]string{"analyst": "deepseek/deepseek-v4-pro"},
		},
		{
			name: "an unavailable model is still reported, the check already failed",
			report: &Report{Profiles: []ProfileCheck{
				{Agent: "devops", Model: "deepseek/dropped", Available: false},
			}},
			want: map[string]string{"devops": "deepseek/dropped"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := profileModelsOf(test.report)
			if test.wantNil {
				if got != nil {
					t.Fatalf("profileModelsOf = %v, want nil", got)
				}
				return
			}
			if len(got) != len(test.want) {
				t.Fatalf("profileModelsOf = %v, want %v", got, test.want)
			}
			for name, model := range test.want {
				if got[name] != model {
					t.Errorf("model for %q = %q, want %q", name, got[name], model)
				}
			}
		})
	}
}

// TestRegisterSendsThePinnedModels is the integration point that broke live on
// 2026-09-25: `init --check` verified the installed profile's model and reported it
// available, while the running agent server kept answering with the model id it
// had cached before the profile was repaired. The claim has to carry the model, or
// the control plane has nothing to pin.
func TestRegisterSendsThePinnedModels(t *testing.T) {
	var claims []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var claim map[string]any
		if err := json.NewDecoder(r.Body).Decode(&claim); err != nil {
			t.Errorf("claim is not readable: %v", err)
		}
		claims = append(claims, claim)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"runtime":{"name":"builder-a","state":"active"},"secret":"s3cret"}`))
	}))
	defer server.Close()

	report := &Report{
		Agent:    &Agent{Name: "opencode", Version: "1.18.31"},
		Profiles: []ProfileCheck{{Agent: "devops", Model: "deepseek/deepseek-flash", Source: "installed"}},
	}
	configRoot := t.TempDir()
	outcome, err := Register(context.Background(), runtimes.NewClient(server.URL, "admin"), "invite-token", "builder-a",
		report, "http://127.0.0.1:4096", InstallOptions{Agent: "opencode", ConfigRoot: configRoot, FlowHubVersion: "test"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(claims))
	}
	models, _ := claims[0]["models"].(map[string]any)
	if models["devops"] != "deepseek/deepseek-flash" {
		t.Fatalf("the claim does not carry the pinned model: %v", claims[0]["models"])
	}
	if claims[0]["agent"] != "opencode" || claims[0]["agent_profile"] != "devops" {
		t.Errorf("claim = %v, want the product and the profile kept apart", claims[0])
	}

	// The registration secret goes to the host's identity file and nowhere else.
	if !strings.Contains(outcome.Stored, "runtime.json") {
		t.Errorf("Stored = %q, want the identity path", outcome.Stored)
	}
	identity, err := LoadRuntimeIdentity(configRoot)
	if err != nil {
		t.Fatalf("LoadRuntimeIdentity: %v", err)
	}
	if identity.Secret != "s3cret" || identity.Name != "builder-a" {
		t.Fatalf("identity = %+v", identity)
	}
}
