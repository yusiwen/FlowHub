package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/config"
	"github.com/yusiwen/flowhub/internal/dispatch"
	"github.com/yusiwen/flowhub/internal/runtimes"
)

// versionLine feeds -version, the startup banner and /healthz. The Makefile
// injects Version from `git describe --always --dirty`, which already contains
// the short sha while the repository has no tags, so the line must not repeat it.
func TestVersionLineDoesNotRepeatTheCommit(t *testing.T) {
	original := struct{ version, commit, buildTime string }{Version, CommitSHA, BuildTime}
	t.Cleanup(func() {
		Version, CommitSHA, BuildTime = original.version, original.commit, original.buildTime
	})

	cases := map[string]struct {
		version, commit, buildTime string
		want                       string
	}{
		"describe already carries the sha": {
			version: "23ece6e-dirty", commit: "23ece6e",
			want: "23ece6e-dirty",
		},
		"tagged release prepends the commit once": {
			version: "v1.2.3", commit: "23ece6e",
			want: "v1.2.3 23ece6e",
		},
		"unknown commit is omitted": {
			version: "dev", commit: "unknown",
			want: "dev",
		},
		"build time is appended": {
			version: "v1.2.3", commit: "23ece6e", buildTime: "2026-09-20T12:00:00Z",
			want: "v1.2.3 23ece6e built 2026-09-20T12:00:00Z",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			Version, CommitSHA, BuildTime = tc.version, tc.commit, tc.buildTime
			got := versionLine()
			// vcsRevision() may add a revision when the commit is unknown; that is
			// environment dependent, so only assert the injected parts.
			if tc.commit == "unknown" {
				if !strings.HasPrefix(got, "dev") {
					t.Fatalf("versionLine() = %q, want it to start with %q", got, "dev")
				}
				return
			}
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("versionLine() = %q, want it to start with %q", got, tc.want)
			}
			if tc.commit != "" && strings.Count(got, tc.commit) > 1 {
				t.Fatalf("versionLine() = %q repeats the commit", got)
			}
		})
	}
}

// TestWebhookOptionsNeverHoldATypedNilDispatcher guards a regression measured
// against a real process: with FLOWHUB_DISPATCH off, the receiver answered every
// delivery with an empty reply. The cause was a typed nil (*dispatch.Dispatcher)
// stored in the webhook.Dispatcher interface, which is not == nil, so the hook
// was called and panicked after the 202 had been written.
func TestWebhookOptionsNeverHoldATypedNilDispatcher(t *testing.T) {
	var absent *dispatch.Dispatcher
	if absent != nil {
		t.Fatal("precondition: a nil pointer must compare equal to nil")
	}
	if opts := webhookOptions(config.Config{}, absent); opts.Dispatcher != nil {
		t.Fatal("webhookOptions put a nil dispatcher behind the interface")
	}
}

func TestWebhookOptionsCarryTheReceiverLocks(t *testing.T) {
	cfg := config.Config{
		HookKey:      "k",
		TokenHeader:  "X-YouTrack-Token",
		Token:        "t",
		MaxBodyBytes: 1024,
		ReplayWindow: time.Minute,
		LogHeaders:   true,
	}
	opts := webhookOptions(cfg, &dispatch.Dispatcher{})
	if opts.HookKey != "k" || opts.Token != "t" || opts.Dispatcher == nil {
		t.Fatalf("options = %+v", opts)
	}
}

// fakeAgentServer stands in for an opencode host during activation. Each case
// changes exactly one of the three things the probe reads.
func fakeAgentServer(t *testing.T, agents string, providers string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.31"}`))
		case "/agent":
			_, _ = w.Write([]byte(agents))
		case "/provider":
			_, _ = w.Write([]byte(providers))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestProberRefusesAHostWithoutTheClaimedAgent is ADR 0002 step 5's acceptance
// criterion: a host whose agent server answers but has no such agent must fail
// activation with that reason. opencode would otherwise accept the session and run
// it under its own default agent, silently dropping the profile's step budget and
// permission block.
func TestProberRefusesAHostWithoutTheClaimedAgent(t *testing.T) {
	server := fakeAgentServer(t,
		`[{"name":"build","mode":"primary"}]`,
		`{"all":[{"id":"deepseek","models":{"deepseek-flash":{}}}]}`)

	err := opencodeProber{timeout: 2 * time.Second}.Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, Agent: "opencode", AgentProfile: "devops",
		Models: map[string]string{"devops": "deepseek/deepseek-flash"},
	})
	if err == nil {
		t.Fatal("a host without the claimed agent was activated")
	}
	if !strings.Contains(err.Error(), `no agent named "devops"`) {
		t.Fatalf("the refusal does not name the missing agent: %v", err)
	}
}

// TestProberRefusesAModelTheHostCannotRun covers the other half: the model the
// host reported has to exist on the server that will run it, or the first turn is
// where the operator finds out.
func TestProberRefusesAModelTheHostCannotRun(t *testing.T) {
	server := fakeAgentServer(t,
		`[{"name":"devops","mode":"primary","model":{"providerID":"deepseek","modelID":"deepseek-v4-flash"}}]`,
		`{"all":[{"id":"deepseek","models":{"deepseek-flash":{},"deepseek-v4-pro":{}}}]}`)

	err := opencodeProber{timeout: 2 * time.Second}.Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, Agent: "opencode", AgentProfile: "devops",
		Models: map[string]string{"devops": "deepseek/deepseek-v4-flash"},
	})
	if err == nil {
		t.Fatal("a model the provider does not offer was activated")
	}
	if !strings.Contains(err.Error(), "does not offer model deepseek/deepseek-v4-flash") ||
		!strings.Contains(err.Error(), "deepseek-flash") {
		t.Fatalf("the refusal does not name the gap and what is offered: %v", err)
	}
}

// TestProberAcceptsAStaleServerModel: the server's own registry entry may be older
// than the profile on disk, and that is no longer fatal — the dispatcher pins the
// model the host reported. It is still worth a warning, which is why this case
// asserts success with the stale entry present.
func TestProberAcceptsAStaleServerModel(t *testing.T) {
	server := fakeAgentServer(t,
		`[{"name":"devops","mode":"primary","model":{"providerID":"deepseek","modelID":"deepseek-v4-flash"}}]`,
		`{"all":[{"id":"deepseek","models":{"deepseek-flash":{}}}]}`)
	var logged bytes.Buffer

	err := opencodeProber{
		timeout: 2 * time.Second,
		log:     slog.New(slog.NewTextHandler(&logged, nil)),
	}.Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, Agent: "opencode", AgentProfile: "devops",
		Models: map[string]string{"devops": "deepseek/deepseek-flash"},
	})
	if err != nil {
		t.Fatalf("a repaired profile was refused because the server's cache is stale: %v", err)
	}
	if !strings.Contains(logged.String(), "still reports an older model") {
		t.Fatalf("the stale server registry was not reported: %s", logged.String())
	}
}

// TestProberNeedsOnlyLivenessWhenNoProfileIsClaimed keeps the check honest for a
// host that names no agent profile: there is nothing to compare, so liveness is
// the whole question.
func TestProberNeedsOnlyLivenessWhenNoProfileIsClaimed(t *testing.T) {
	server := fakeAgentServer(t, `[]`, `{"all":[]}`)
	if err := (opencodeProber{timeout: 2 * time.Second}).Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, Agent: "opencode",
	}); err != nil {
		t.Fatalf("a host with no claimed profile was refused: %v", err)
	}
}
