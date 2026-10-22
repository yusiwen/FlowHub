package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestProtocolDetection is the compatibility seam: a v1 server exposes
// /global/health and a v2 one exposes /api/info. The order matters because a v2
// server answers /global/health with the web app's HTML page and status 200, so
// the old probe would classify it as "v1, answering nonsense".
func TestProtocolDetection(t *testing.T) {
	t.Run("v2", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/info":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version":"2.0.32","pid":1,"urls":[],"paths":{"tmp":"/tmp"}}`))
			case "/global/health":
				// The measured v2 fallback: the SPA shell with status 200.
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(`<!doctype html><html></html>`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		client := New(Options{BaseURL: server.URL, Timeout: 2 * time.Second})
		proto, err := client.Protocol(context.Background())
		if err != nil {
			t.Fatalf("Protocol: %v", err)
		}
		if proto != protocolV2 {
			t.Fatalf("protocol = %s, want v2", proto)
		}
		health, err := client.Health(context.Background())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if !health.Healthy || health.Version != "2.0.32" {
			t.Fatalf("health = %+v", health)
		}
	})

	t.Run("v1", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/global/health" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.35"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		client := New(Options{BaseURL: server.URL, Timeout: 2 * time.Second})
		proto, err := client.Protocol(context.Background())
		if err != nil {
			t.Fatalf("Protocol: %v", err)
		}
		if proto != protocolV1 {
			t.Fatalf("protocol = %s, want v1", proto)
		}
	})

	t.Run("neither", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		defer server.Close()

		client := New(Options{BaseURL: server.URL, Timeout: 2 * time.Second})
		_, err := client.Protocol(context.Background())
		if err == nil {
			t.Fatal("Protocol succeeded, want an error")
		}
		for _, want := range []string{"/api/info", "/global/health", "HTML"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("unauthorized", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`))
		}))
		defer server.Close()

		client := New(Options{BaseURL: server.URL, Timeout: 2 * time.Second})
		_, err := client.Protocol(context.Background())
		if err == nil || !strings.Contains(err.Error(), "credentials") {
			t.Fatalf("error = %v, want a credential failure", err)
		}
	})
}

// TestV2AgentsAndCatalogue checks the two capability reads the enrolment check
// performs, against the measured v2 shapes.
func TestV2AgentsAndCatalogue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/info":
			_, _ = w.Write([]byte(`{"version":"2.0.32"}`))
		case "/api/agent":
			// id is what a session is created with; name is a display label.
			_, _ = w.Write([]byte(`{"location":{"directory":"/"},"data":[
				{"id":"devops","name":"devops","mode":"primary","hidden":false,
				 "model":{"id":"deepseek-flash","providerID":"deepseek"}},
				{"id":"build","name":"Build","mode":"primary","hidden":false,"model":null}]}`))
		case "/api/model":
			_, _ = w.Write([]byte(`{"location":{"directory":"/"},"data":[
				{"id":"deepseek-flash","modelID":"deepseek-flash","providerID":"deepseek","status":"active","enabled":true},
				{"id":"old","modelID":"old","providerID":"deepseek","status":"deprecated","enabled":true},
				{"id":"off","modelID":"off","providerID":"deepseek","status":"active","enabled":false},
				{"id":"m2","modelID":"m2","providerID":"other","status":"active","enabled":true}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := New(Options{BaseURL: server.URL, Timeout: 2 * time.Second})
	agents, err := client.Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 2 || agents[0].Name != "devops" || agents[1].Name != "build" {
		t.Fatalf("agents = %+v", agents)
	}
	if got := agents[0].ModelString(); got != "deepseek/deepseek-flash" {
		t.Fatalf("model = %q", got)
	}
	if agents[1].Model != nil {
		t.Fatalf("build carries a model: %+v", agents[1].Model)
	}

	catalogue, err := client.Catalogue(context.Background())
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	if got := strings.Join(catalogue["deepseek"], ","); got != "deepseek-flash" {
		t.Fatalf("deepseek catalogue = %q, want only the enabled, non-deprecated model", got)
	}
	if got := strings.Join(catalogue["other"], ","); got != "m2" {
		t.Fatalf("other catalogue = %q", got)
	}
}

// TestV2PermissionTranslation is the policy seam: the arbiter and its tests speak
// v1 names, so the transport translates rather than teaching the policy a second
// vocabulary.
func TestV2PermissionTranslation(t *testing.T) {
	request := permissionV2{
		ID: "per_1", SessionID: "ses_1", Action: "shell",
		Resources: []string{"echo one", "echo two"},
		Save:      []string{"echo *"},
	}.toPermissionRequest()

	if request.Permission != "bash" {
		t.Fatalf("permission = %q, want bash", request.Permission)
	}
	if request.Metadata.Command != "echo one && echo two" {
		t.Fatalf("command = %q", request.Metadata.Command)
	}
	if len(request.Patterns) != 2 || request.Patterns[1] != "echo two" {
		t.Fatalf("patterns = %+v", request.Patterns)
	}
	if len(request.Always) != 1 || request.Always[0] != "echo *" {
		t.Fatalf("always = %+v", request.Always)
	}

	edit := permissionV2{ID: "per_2", SessionID: "ses_1", Action: "edit", Resources: []string{"a.go"}}.toPermissionRequest()
	if edit.Permission != "edit" || edit.Metadata.Command != "" {
		t.Fatalf("edit = %+v, want the action kept and no shell command", edit)
	}

	// A request that carries the whole command in metadata keeps it verbatim.
	withMetadata := permissionV2{
		ID: "per_3", SessionID: "ses_1", Action: "shell",
		Resources: []string{"pwd"},
		Metadata:  map[string]any{"command": "pwd && ls"},
	}.toPermissionRequest()
	if withMetadata.Metadata.Command != "pwd && ls" {
		t.Fatalf("command = %q, want the server's own metadata", withMetadata.Metadata.Command)
	}
}

// TestV2MessagesProjection checks the union flattening the runner relies on: a
// user text, an assistant's text and tool parts, and an idle marker with its
// outcome and error.
func TestV2MessagesProjection(t *testing.T) {
	user := messageV2{ID: "m1", Type: "user", Text: "hello"}
	user.Time.Created = 1
	if got := user.toMessage(); got.Info.Role != "user" || got.Parts[0].Text != "hello" {
		t.Fatalf("user = %+v", got)
	}

	assistant := messageV2{
		ID: "m2", Type: "assistant", Agent: "devops", Finish: "tool-calls",
		Model: &modelRefV2{ID: "deepseek-flash", ProviderID: "deepseek"},
	}
	assistant.Time.Created, assistant.Time.Completed = 2, 3
	if err := json.Unmarshal([]byte(`[
		{"type":"text","text":"working"},
		{"type":"tool","id":"t1","name":"shell","state":{"status":"completed","input":{"command":"pwd"},"content":[{"type":"text","text":"/tmp"}]}}]`), &assistant.Content); err != nil {
		t.Fatal(err)
	}
	message := assistant.toMessage()
	if message.Info.Role != "assistant" || message.Info.Time.Completed != 3 {
		t.Fatalf("info = %+v", message.Info)
	}
	if len(message.Parts) != 2 || message.Parts[1].Tool != "shell" || message.Parts[1].State.Output != "/tmp" {
		t.Fatalf("parts = %+v", message.Parts)
	}

	// A v2 execute call carries the real MCP calls in metadata.toolCalls; the
	// outer part is only "execute". Flattening is what lets reply detection see
	// the comment call the model actually made.
	execute := messageV2{ID: "m4", Type: "assistant"}
	if err := json.Unmarshal([]byte(`[{
		"type":"tool","name":"execute",
		"state":{"status":"completed","input":{"code":"..."},
		"metadata":{"toolCalls":[
			{"tool":"youtrack.add_issue_comment","status":"completed","input":{"issueId":"TEST-1"}}
		]}}}]`), &execute.Content); err != nil {
		t.Fatal(err)
	}
	executeMsg := execute.toMessage()
	found := false
	for _, part := range executeMsg.Parts {
		if part.Tool == "youtrack.add_issue_comment" && part.State != nil && part.State.Status == "completed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("nested MCP call not projected: %+v", executeMsg.Parts)
	}

	idle := messageV2{ID: "m3", Type: "idle", Outcome: "failed"}
	idle.Time.Created = 4
	idle.Error = &structuredErrorV2{Type: "provider.auth", Message: "bad key"}
	projected := idle.toMessage()
	if projected.Info.Role != "idle" || projected.Info.Outcome != "failed" {
		t.Fatalf("idle = %+v", projected.Info)
	}
	if got := structuredMessage(projected.Info.Error); got != "provider.auth: bad key" {
		t.Fatalf("error = %q", got)
	}
}
