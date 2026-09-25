package runtimes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// probeOK accepts any address; probeFail refuses with a message.
type probeOK struct{ calls int }

func (p *probeOK) Probe(context.Context, string) error { p.calls++; return nil }

type probeFail struct{ err error }

func (p *probeFail) Probe(context.Context, string) error { return p.err }

func newInventory(t *testing.T) *Inventory {
	t.Helper()
	inventory, err := Open(filepath.Join(t.TempDir(), "runtimes.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	inventory.Now = func() time.Time { return fixedNow }
	return inventory
}

func validClaim() Claim {
	return Claim{
		Name:           "builder-a",
		URL:            "https://builder-a.lan:4096",
		Advertise:      "https://builder-a.lan:4096",
		Agent:          "opencode",
		AgentVersion:   "1.18.31",
		FlowHubVersion: "test",
		Models:         map[string]string{"devops": "deepseek/deepseek-flash"},
	}
}

func TestInviteThenRegisterActivates(t *testing.T) {
	inventory := newInventory(t)
	token, pending, err := inventory.Invite("builder-a", []string{"BEAP_BE", "TEST", "TEST"}, time.Hour)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if token == "" {
		t.Fatal("no token was issued")
	}
	if pending.State != StatePending || pending.Invite == nil || pending.Invite.TokenSHA != "" {
		t.Fatalf("the returned invite leaks or lacks its hash: %+v", pending)
	}
	if len(pending.Invite.Projects) != 2 {
		t.Fatalf("projects were not deduplicated: %v", pending.Invite.Projects)
	}

	prober := &probeOK{}
	runtime, secret, err := inventory.Register(context.Background(), validClaim(), token, prober, fixedNow)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if prober.calls != 1 {
		t.Fatalf("the host was not probed before activation (calls=%d)", prober.calls)
	}
	if runtime.State != StateActive || secret == "" {
		t.Fatalf("runtime = %+v secret=%q", runtime, secret)
	}
	if runtime.AgentVersion != "1.18.31" || runtime.LastSeen.IsZero() {
		t.Fatalf("runtime = %+v", runtime)
	}
	// The models the host reported must survive: the dispatcher pins the model a
	// turn uses from here, so losing them silently returns every turn to whatever
	// the agent server happens to have cached.
	if runtime.Models["devops"] != "deepseek/deepseek-flash" {
		t.Fatalf("the claim's models were not recorded: %+v", runtime.Models)
	}
	// The stored file must hold hashes, never the tokens themselves.
	raw, err := os.ReadFile(inventory.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), secret) {
		t.Fatal("the inventory stored a token in the clear")
	}
	if !strings.Contains(string(raw), "secret_sha256") {
		t.Fatal("the inventory does not record a secret hash")
	}
}

// TestRegisterRequiresTheRightToken covers the whole point of the invite: a host
// cannot enrol itself without an operator having asked for it.
func TestRegisterRequiresTheRightToken(t *testing.T) {
	inventory := newInventory(t)
	if _, _, err := inventory.Invite("builder-a", nil, time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, _, err := inventory.Register(context.Background(), validClaim(), "not-the-token", &probeOK{}, fixedNow); err == nil {
		t.Fatal("a wrong token was accepted")
	}
	// An uninvited name is refused too.
	claim := validClaim()
	claim.Name = "builder-b"
	if _, _, err := inventory.Register(context.Background(), claim, "whatever", &probeOK{}, fixedNow); err == nil {
		t.Fatal("an uninvited name was accepted")
	}
}

func TestExpiredInviteIsRefused(t *testing.T) {
	inventory := newInventory(t)
	token, _, err := inventory.Invite("builder-a", nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow.Add(2*time.Minute)); err == nil {
		t.Fatal("an expired invite was accepted")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v", err)
	}
}

// TestProbeFailureLeavesTheInvitePending is what makes a NAT mistake recoverable:
// the entry stays pending after the checks pass, so the operator fixes the network
// and retries with the same invite.
func TestProbeFailureLeavesTheInvitePending(t *testing.T) {
	inventory := newInventory(t)
	token, _, err := inventory.Invite("builder-a", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	prober := &probeFail{err: errors.New("connection refused")}
	if _, _, err := inventory.Register(context.Background(), validClaim(), token, prober, fixedNow); err == nil {
		t.Fatal("an unreachable host was activated")
	}

	runtime, ok := inventory.Get("builder-a")
	if !ok {
		t.Fatal("the runtime disappeared")
	}
	if runtime.State != StatePending {
		t.Fatalf("state = %s, want pending", runtime.State)
	}
	if !strings.Contains(runtime.Note, "connection refused") {
		t.Fatalf("the reason was not recorded: %q", runtime.Note)
	}
	// The same invite still works once the host is reachable.
	if _, secret, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow); err != nil || secret == "" {
		t.Fatalf("retry after the probe was fixed: %v", err)
	}
}

func TestRegisterRejectsAMissingAdvertiseOrAgent(t *testing.T) {
	for name, mutate := range map[string]func(*Claim){
		"no advertise": func(c *Claim) { c.Advertise = "" },
		"no agent":     func(c *Claim) { c.Agent = "" },
	} {
		t.Run(name, func(t *testing.T) {
			inventory := newInventory(t)
			token, _, err := inventory.Invite("builder-a", nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			claim := validClaim()
			mutate(&claim)
			if _, _, err := inventory.Register(context.Background(), claim, token, &probeOK{}, fixedNow); err == nil {
				t.Fatal("an incomplete claim was accepted")
			}
		})
	}
}

func TestActiveRuntimeCannotBeInvitedAgain(t *testing.T) {
	inventory := newInventory(t)
	token, _, err := inventory.Invite("builder-a", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inventory.Invite("builder-a", nil, time.Hour); err == nil {
		t.Fatal("re-inviting an active runtime was allowed")
	}
}

func TestHeartbeatRequiresTheSecretAndUpdatesLastSeen(t *testing.T) {
	inventory := newInventory(t)
	token, _, err := inventory.Invite("builder-a", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := inventory.Heartbeat("builder-a", "wrong", nil, fixedNow.Add(time.Minute)); err == nil {
		t.Fatal("a wrong secret was accepted")
	}
	later := fixedNow.Add(10 * time.Minute)
	runtime, err := inventory.Heartbeat("builder-a", secret, map[string]any{"ok": true}, later)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !runtime.LastSeen.Equal(later) {
		t.Fatalf("last seen = %s, want %s", runtime.LastSeen, later)
	}
	if runtime.ReportSHA256 == "" {
		t.Fatal("the report hash was not recorded")
	}
}

func TestRotateInvalidatesTheOldSecret(t *testing.T) {
	inventory := newInventory(t)
	token, _, _ := inventory.Invite("builder-a", nil, time.Hour)
	_, oldSecret, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	newSecret, err := inventory.Rotate("builder-a")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if newSecret == oldSecret {
		t.Fatal("rotation returned the same secret")
	}
	if _, err := inventory.Heartbeat("builder-a", oldSecret, nil, fixedNow); err == nil {
		t.Fatal("the old secret still works")
	}
	if _, err := inventory.Heartbeat("builder-a", newSecret, nil, fixedNow); err != nil {
		t.Fatalf("the new secret does not work: %v", err)
	}
}

// TestRemoveRefusesWhileTasksAreBound is ADR 0001's sticky binding made
// operational: those tasks cannot move to another host, so removing theirs must be
// a deliberate act.
func TestRemoveRefusesWhileTasksAreBound(t *testing.T) {
	inventory := newInventory(t)
	token, _, _ := inventory.Invite("builder-a", nil, time.Hour)
	if _, _, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow); err != nil {
		t.Fatal(err)
	}
	bound := func(name string) []string {
		if name == "builder-a" {
			return []string{"TEST-17", "BEAP_BE-42"}
		}
		return nil
	}

	if _, _, err := inventory.Remove("builder-a", false, bound); err == nil {
		t.Fatal("removal succeeded while tasks were bound")
	}
	runtime, _, err := inventory.Remove("builder-a", true, bound)
	if err != nil {
		t.Fatalf("Remove with force: %v", err)
	}
	if runtime.State != StateRevoked {
		t.Fatalf("state = %s, want revoked", runtime.State)
	}
	if !strings.Contains(runtime.Note, "TEST-17") || !strings.Contains(runtime.Note, "BEAP_BE-42") {
		t.Fatalf("the bound tasks were not recorded: %q", runtime.Note)
	}
	// A revoked runtime is not usable and cannot authenticate any more.
	if len(inventory.Active()) != 0 {
		t.Fatal("a revoked runtime is still active")
	}
	if inventory.SecretMatches("builder-a", "anything") {
		t.Fatal("a revoked runtime still matches a secret")
	}
}

// TestRevokedNameIsReservedButReinvitable: the name stays taken so a removed host
// cannot silently come back, and an operator can deliberately re-enrol it.
func TestRevokedNameIsReservedButReinvitable(t *testing.T) {
	inventory := newInventory(t)
	token, _, _ := inventory.Invite("builder-a", nil, time.Hour)
	if _, _, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inventory.Remove("builder-a", false, nil); err != nil {
		t.Fatal(err)
	}
	runtime, ok := inventory.Get("builder-a")
	if !ok || runtime.State != StateRevoked {
		t.Fatalf("runtime = %+v ok=%t", runtime, ok)
	}
	if _, _, err := inventory.Invite("builder-a", nil, time.Hour); err != nil {
		t.Fatalf("a revoked name should be invitable again: %v", err)
	}
	// And the fresh invite needs its own token.
	if _, _, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow); err == nil {
		t.Fatal("the old invite token was accepted after removal")
	}
}

func TestInventoryPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtimes.json")
	inventory, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	inventory.Now = func() time.Time { return fixedNow }
	token, _, _ := inventory.Invite("builder-a", []string{"TEST"}, time.Hour)
	if _, _, err := inventory.Register(context.Background(), validClaim(), token, &probeOK{}, fixedNow); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open after save: %v", err)
	}
	runtime, ok := reopened.Get("builder-a")
	if !ok || runtime.State != StateActive || runtime.AgentVersion != "1.18.31" {
		t.Fatalf("runtime = %+v ok=%t", runtime, ok)
	}
	if runtime.Models["devops"] != "deepseek/deepseek-flash" {
		t.Fatalf("the models did not survive the reload: %+v", runtime.Models)
	}
}

func TestCorruptInventoryStopsRatherThanLookingEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtimes.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("a corrupt inventory was accepted")
	}
}

func TestInviteRejectsBadNames(t *testing.T) {
	inventory := newInventory(t)
	for _, name := range []string{"", "Builder A", "builder/a", "../escape", strings.Repeat("x", 80)} {
		if _, _, err := inventory.Invite(name, nil, time.Hour); err == nil {
			t.Errorf("Invite(%q) was accepted", name)
		}
	}
}

// --- the control API -------------------------------------------------------

func newControlServer(t *testing.T, inventory *Inventory, prober Prober, bound BoundTasksFunc) (*httptest.Server, *Client) {
	t.Helper()
	server := &Server{
		Inventory:  inventory,
		AdminToken: "admin-token",
		Prober:     prober,
		BoundTasks: bound,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return fixedNow },
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer, NewClient(httpServer.URL, "admin-token")
}

func TestControlAPIRefusesAnUnauthenticatedAdminCall(t *testing.T) {
	inventory := newInventory(t)
	server := &Server{
		Inventory:  inventory,
		AdminToken: "admin-token",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/control/v1/runtimes")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if strings.Contains(string(body), "sha256") {
		t.Fatalf("the refusal leaked inventory internals: %s", body)
	}
	// Health is the one open route, and it says nothing useful to an attacker.
	health, err := http.Get(httpServer.URL + "/control/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", health.StatusCode)
	}
}

func TestControlAPIEnrolmentRoundTrip(t *testing.T) {
	inventory := newInventory(t)
	prober := &probeOK{}
	_, client := newControlServer(t, inventory, prober, nil)

	invite, err := client.Invite(context.Background(), "builder-a", []string{"TEST"}, time.Hour)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if invite.Token == "" || invite.Runtime.State != StatePending {
		t.Fatalf("invite = %+v", invite)
	}

	runtime, secret, err := client.Register(context.Background(), invite.Token, validClaim())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if runtime.State != StateActive || secret == "" {
		t.Fatalf("runtime = %+v secret=%q", runtime, secret)
	}

	list, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "builder-a" {
		t.Fatalf("list = %+v", list)
	}
	// The API must never serve the hashes.
	encoded, _ := json.Marshal(list)
	if strings.Contains(string(encoded), "sha256") {
		t.Fatalf("the API served internal hashes: %s", encoded)
	}

	shown, bound, err := client.Show(context.Background(), "builder-a")
	if err != nil || shown.Name != "builder-a" || len(bound) != 0 {
		t.Fatalf("Show: %+v %v %v", shown, bound, err)
	}

	if err := client.Heartbeat(context.Background(), "builder-a", secret, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := client.Heartbeat(context.Background(), "builder-a", "wrong", nil); err == nil {
		t.Fatal("a wrong runtime secret was accepted")
	}
}

func TestControlAPIRemoveReportsBoundTasks(t *testing.T) {
	inventory := newInventory(t)
	bound := func(string) []string { return []string{"TEST-17"} }
	_, client := newControlServer(t, inventory, &probeOK{}, bound)

	invite, err := client.Invite(context.Background(), "builder-a", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Register(context.Background(), invite.Token, validClaim()); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Remove(context.Background(), "builder-a", false); err == nil {
		t.Fatal("removal succeeded while a task was bound")
	} else if !strings.Contains(err.Error(), "TEST-17") {
		t.Fatalf("the refusal does not name the task: %v", err)
	}
	tasks, err := client.Remove(context.Background(), "builder-a", true)
	if err != nil {
		t.Fatalf("Remove with force: %v", err)
	}
	if len(tasks) != 1 || tasks[0] != "TEST-17" {
		t.Fatalf("tasks = %v", tasks)
	}
}

func TestControlAPIRoundTripsAReportAndRotates(t *testing.T) {
	inventory := newInventory(t)
	_, client := newControlServer(t, inventory, &probeOK{}, nil)

	invite, err := client.Invite(context.Background(), "builder-a", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claim := validClaim()
	claim.Report = map[string]any{
		"invoker": map[string]any{"user": "yusiwen", "home": "/home/yusiwen"},
		"tools":   map[string]any{"git": map[string]any{"version": "2.43.0"}},
	}
	if _, _, err := client.Register(context.Background(), invite.Token, claim); err != nil {
		t.Fatalf("Register with a report: %v", err)
	}
	shown, _, err := client.Show(context.Background(), "builder-a")
	if err != nil {
		t.Fatal(err)
	}
	if shown.ReportSHA256 == "" {
		t.Fatal("the report was not fingerprinted")
	}

	secret, err := client.Rotate(context.Background(), "builder-a")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if secret == "" {
		t.Fatal("no secret was returned")
	}
	if err := client.Heartbeat(context.Background(), "builder-a", secret, nil); err != nil {
		t.Fatalf("the rotated secret does not work: %v", err)
	}
}

// TestConcurrentMutationsStayConsistent exercises the one-writer rule: the
// inventory is mutated from several goroutines and must never lose an entry.
func TestConcurrentMutationsStayConsistent(t *testing.T) {
	inventory := newInventory(t)
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(n int) {
			defer wait.Done()
			name := fmt.Sprintf("builder-%02d", n)
			token, _, err := inventory.Invite(name, nil, time.Hour)
			if err != nil {
				t.Errorf("Invite(%s): %v", name, err)
				return
			}
			claim := validClaim()
			claim.Name = name
			if _, _, err := inventory.Register(context.Background(), claim, token, &probeOK{}, fixedNow); err != nil {
				t.Errorf("Register(%s): %v", name, err)
			}
		}(index)
	}
	wait.Wait()
	if got := len(inventory.Active()); got != 20 {
		t.Fatalf("active runtimes = %d, want 20", got)
	}
}
