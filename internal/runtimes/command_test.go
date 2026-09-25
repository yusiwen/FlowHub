package runtimes

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// parsedRequest is one request the fake control plane received.
type parsedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

// fakeControlPlane answers every call with status and records what arrived.
func fakeControlPlane(t *testing.T, status int, response string) (*httptest.Server, *[]parsedRequest) {
	t.Helper()
	var seen []parsedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record := parsedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query()}
		if body, err := io.ReadAll(r.Body); err == nil && len(bytes.TrimSpace(body)) > 0 {
			_ = json.Unmarshal(body, &record.Body)
		}
		seen = append(seen, record)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if response != "" {
			_, _ = w.Write([]byte(response))
		}
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

// TestParseArgsFlagsOnEitherSide pins the reason this helper exists: the flag
// package stops at the first non-flag argument, so the documented
// `runtime remove <name> --force` form used to drop the flag entirely.
func TestParseArgsFlagsOnEitherSide(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantForce  bool
		wantTail   []string
		wantServer string
	}{
		{name: "flag before the name", args: []string{"--force", "probe"}, wantForce: true, wantTail: []string{"probe"}},
		{name: "flag after the name", args: []string{"probe", "--force"}, wantForce: true, wantTail: []string{"probe"}},
		{name: "only the name", args: []string{"probe"}, wantTail: []string{"probe"}},
		{name: "value flag after the name", args: []string{"probe", "--server", "http://example.invalid"},
			wantTail: []string{"probe"}, wantServer: "http://example.invalid"},
		{name: "both sides", args: []string{"--force", "probe", "--server", "http://example.invalid"},
			wantForce: true, wantTail: []string{"probe"}, wantServer: "http://example.invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var force bool
			var server string
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.BoolVar(&force, "force", false, "")
			fs.StringVar(&server, "server", "", "")

			positional, err := parseArgs(fs, test.args)
			if err != nil {
				t.Fatalf("parseArgs(%v): %v", test.args, err)
			}
			if force != test.wantForce {
				t.Errorf("force = %v, want %v", force, test.wantForce)
			}
			if server != test.wantServer {
				t.Errorf("server = %q, want %q", server, test.wantServer)
			}
			if strings.Join(positional, ",") != strings.Join(test.wantTail, ",") {
				t.Errorf("positional = %v, want %v", positional, test.wantTail)
			}
		})
	}
}

// TestRemoveCommandHonoursTrailingForce is the regression: `remove probe --force`
// used to send no force at all, so a runtime with a bound task was refused even
// though the operator had explicitly overridden the refusal.
func TestRemoveCommandHonoursTrailingForce(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantForce bool
	}{
		{name: "force after the name", args: []string{"probe", "--force"}, wantForce: true},
		{name: "force before the name", args: []string{"--force", "probe"}, wantForce: true},
		{name: "no force", args: []string{"probe"}, wantForce: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, seen := fakeControlPlane(t, http.StatusOK, `{"bound_tasks":["TEST-15"]}`)
			args := append([]string{"--server", server.URL, "--token", "t"}, test.args...)
			var stdout, stderr bytes.Buffer

			if code := CLI("remove", args, &stdout, &stderr, func(string) string { return "" }); code != ExitOK {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
			}
			if len(*seen) != 1 {
				t.Fatalf("requests = %d, want 1", len(*seen))
			}
			got := (*seen)[0]
			if got.Method != http.MethodDelete || got.Path != "/control/v1/runtimes/probe" {
				t.Fatalf("request = %s %s, want DELETE /control/v1/runtimes/probe", got.Method, got.Path)
			}
			if force := got.Query.Get("force"); (force == "true") != test.wantForce {
				t.Errorf("force query = %q, want %v", force, test.wantForce)
			}
		})
	}
}

// TestInviteCommandHonoursTrailingFlags covers the other half of the same bug: an
// invite written as `invite probe --projects TEST --ttl 5m` was created with no
// projects and the default TTL, so the worker checked the wrong repositories.
func TestInviteCommandHonoursTrailingFlags(t *testing.T) {
	server, seen := fakeControlPlane(t, http.StatusCreated,
		`{"runtime":{"name":"probe","state":"pending","invite":{"expires_at":"2026-09-25T05:11:37Z"}},"token":"tok"}`)
	args := []string{"probe", "--projects", "TEST,DEMO", "--ttl", "5m", "--server", server.URL, "--token", "t"}
	var stdout, stderr bytes.Buffer

	if code := CLI("invite", args, &stdout, &stderr, func(string) string { return "" }); code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	body := (*seen)[0].Body
	projects, _ := body["projects"].([]any)
	if len(projects) != 2 || projects[0] != "TEST" || projects[1] != "DEMO" {
		t.Errorf("projects = %v, want [TEST DEMO]", body["projects"])
	}
	if ttl, _ := body["ttl_seconds"].(float64); ttl != 300 {
		t.Errorf("ttl_seconds = %v, want 300", body["ttl_seconds"])
	}
	if !strings.Contains(stdout.String(), "tok") {
		t.Errorf("the one-time token is missing from the output:\n%s", stdout.String())
	}
}

// TestControlCommandsNeedCredentials keeps the "no token, no control surface"
// rule honest: the commands must refuse locally instead of calling an endpoint
// that would answer 401.
func TestControlCommandsNeedCredentials(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"show", "probe"}, {"remove", "probe", "--force"}} {
		var stdout, stderr bytes.Buffer
		env := func(key string) string {
			if key == "FLOWHUB_ADMIN_ADDR" {
				return "127.0.0.1:18096"
			}
			return ""
		}
		if code := CLI(args[0], args[1:], &stdout, &stderr, env); code != ExitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitUsage)
		}
		if !strings.Contains(stderr.String(), "admin token") {
			t.Errorf("%v: stderr does not name the missing token: %s", args, stderr.String())
		}
	}
}
