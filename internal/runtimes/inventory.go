// Package runtimes is the control plane's inventory of agent hosts: who is
// allowed to run work, how they enrolled, and when they were last seen.
//
// It is the only writer of its own file, and every mutation goes through it —
// the admin API and the CLI both call these methods rather than editing JSON.
// Invite tokens and runtime secrets are stored as SHA-256 only, the same rule the
// webhook URL key follows: the file must be safe to back up and read.
package runtimes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Prober proves that the control plane can reach a host before activating it.
type Prober interface {
	Probe(ctx context.Context, advertiseURL string) error
}

// State is where a runtime is in its life.
type State string

const (
	// StatePending: invited, waiting for a host to claim the name.
	StatePending State = "pending"
	// StateActive: the host claimed it and the control plane could reach it.
	StateActive State = "active"
	// StateRevoked: removed by an operator. The name stays reserved so a removed
	// host cannot silently re-enrol itself under it.
	StateRevoked State = "revoked"
)

// SchemaVersion is the on-disk version of the inventory.
const SchemaVersion = 1

// DefaultInviteTTL is how long an invite token is valid.
const DefaultInviteTTL = 30 * time.Minute

// namePattern keeps runtime names safe as a URL path segment, a file name and a
// log field.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Invite is a one-time claim on a runtime name.
type Invite struct {
	TokenSHA  string    `json:"token_sha256,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	// Projects are the YouTrack keys this host is expected to serve. They travel
	// with the invite so the host knows which repositories and forges to check
	// without an operator retyping them.
	Projects []string `json:"projects,omitempty"`
}

// Runtime is one enrolled agent host.
type Runtime struct {
	Name      string `json:"name"`
	State     State  `json:"state"`
	URL       string `json:"url,omitempty"`
	Advertise string `json:"advertise,omitempty"`
	// Agent is the agent *product* this host runs, e.g. "opencode".
	Agent string `json:"agent,omitempty"`
	// AgentProfile is the profile inside that product, e.g. "devops" — the name
	// the agent file declares. It is what a turn asks for, and it is separate from
	// Agent so a product name is never mistaken for a profile name.
	AgentProfile   string `json:"agent_profile,omitempty"`
	AgentVersion   string `json:"agent_version,omitempty"`
	FlowHubVersion string `json:"flowhub_version,omitempty"`
	MaxConcurrent  int    `json:"max_concurrent,omitempty"`
	// Models is the model each installed agent profile pins, keyed by profile
	// name. The host reports it at enrolment and the control plane echoes it back
	// when it asks for a turn, so the model a capability check verified is the
	// model the turn actually uses. It is data, not a secret.
	Models       map[string]string `json:"models,omitempty"`
	RegisteredAt time.Time         `json:"registered_at,omitempty"`
	LastSeen     time.Time         `json:"last_seen,omitempty"`
	ReportSHA256 string            `json:"report_sha256,omitempty"`
	Note         string            `json:"note,omitempty"`
	Invite       *Invite           `json:"invite,omitempty"`
	// SecretSHA256 is the hash of the runtime's long-lived secret. It is never
	// served by the API and never logged.
	SecretSHA256 string `json:"secret_sha256,omitempty"`
}

// Redacted returns a copy safe to send over the API: the hashes stay here.
func (r Runtime) Redacted() Runtime {
	r.SecretSHA256 = ""
	if r.Invite != nil {
		invite := *r.Invite
		invite.TokenSHA = ""
		r.Invite = &invite
	}
	return r
}

// Claim is what a host says about itself when it enrols.
type Claim struct {
	Name           string         `json:"name"`
	URL            string         `json:"url"`
	Advertise      string         `json:"advertise"`
	Agent          string         `json:"agent"`
	AgentProfile   string         `json:"agent_profile"`
	AgentVersion   string         `json:"agent_version"`
	FlowHubVersion string         `json:"flowhub_version"`
	MaxConcurrent  int            `json:"max_concurrent,omitempty"`
	Report         map[string]any `json:"report,omitempty"`
	// Models is what the host's agent profiles pin, keyed by profile name. It
	// travels separately from the report because the dispatcher needs it on every
	// turn, not only when an operator reads the report.
	Models map[string]string `json:"models,omitempty"`
}

// fileFormat is the on-disk shape.
type fileFormat struct {
	Version  int                 `json:"version"`
	Runtimes map[string]*Runtime `json:"runtimes"`
}

// Inventory is the runtime set, held in memory and persisted on every change.
type Inventory struct {
	mu       sync.Mutex
	path     string
	runtimes map[string]*Runtime
	// Now is injectable so invite expiry is testable without sleeping. A clock the
	// caller cannot control is how a test ends up asserting against wall time.
	Now func() time.Time
}

func (i *Inventory) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// Open loads the inventory, returning an empty one when the file does not exist.
func Open(path string) (*Inventory, error) {
	inventory := &Inventory{path: path, runtimes: map[string]*Runtime{}}
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return inventory, nil
		}
		return nil, fmt.Errorf("read runtime inventory %s: %w", path, err)
	}
	var file fileFormat
	if err := json.Unmarshal(content, &file); err != nil {
		// A corrupt inventory must stop the start rather than be treated as empty:
		// "no runtimes" and "unreadable runtimes" mean opposite things.
		return nil, fmt.Errorf("runtime inventory %s is not readable (%w); fix or move it aside", path, err)
	}
	if file.Version != SchemaVersion {
		return nil, fmt.Errorf("runtime inventory %s has version %d, this build understands %d", path, file.Version, SchemaVersion)
	}
	for name, runtime := range file.Runtimes {
		if runtime == nil {
			continue
		}
		runtime.Name = name
		inventory.runtimes[name] = runtime
	}
	return inventory, nil
}

// Path is the file this inventory persists to.
func (i *Inventory) Path() string { return i.path }

// save writes the inventory atomically. The caller holds the lock.
func (i *Inventory) save() error {
	if i.path == "" {
		return nil
	}
	file := fileFormat{Version: SchemaVersion, Runtimes: i.runtimes}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	dir := filepath.Dir(i.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".runtimes-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, i.path)
}

// Invite creates or refreshes a pending entry and returns the one-time token.
// The token is returned once and never stored: only its hash is.
func (i *Inventory) Invite(name string, projects []string, ttl time.Duration) (string, Runtime, error) {
	name = normaliseName(name)
	if !namePattern.MatchString(name) {
		return "", Runtime{}, fmt.Errorf("runtime name %q must match %s", name, namePattern)
	}
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}
	token, err := newToken()
	if err != nil {
		return "", Runtime{}, err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	existing := i.runtimes[name]
	if existing != nil && existing.State == StateActive {
		return "", Runtime{}, fmt.Errorf("runtime %s is already active: remove it first, or invite a different name", name)
	}
	now := i.now().UTC()
	runtime := &Runtime{
		Name:  name,
		State: StatePending,
		Invite: &Invite{
			TokenSHA:  hashToken(token),
			ExpiresAt: now.Add(ttl),
			Projects:  cleanProjects(projects),
		},
	}
	if existing != nil {
		runtime.Note = existing.Note
	}
	i.runtimes[name] = runtime
	if err := i.save(); err != nil {
		return "", Runtime{}, err
	}
	return token, runtime.Redacted(), nil
}

// Register claims a pending name with an invite token.
//
// The runtime is only activated once the control plane has proved it can reach the
// advertised address, so a NAT or firewall mistake fails here rather than at the
// first task. A failed probe leaves the entry pending, which is exactly what the
// operator needs to retry after fixing the network.
func (i *Inventory) Register(ctx context.Context, claim Claim, inviteToken string, prober Prober, now time.Time) (Runtime, string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	name := normaliseName(claim.Name)
	if name == "" {
		return Runtime{}, "", errors.New("a runtime name is required")
	}
	runtime, ok := i.runtimes[name]
	if !ok || runtime.State != StatePending || runtime.Invite == nil {
		return Runtime{}, "", fmt.Errorf("no pending invite for runtime %s", name)
	}
	if !sameToken(inviteToken, runtime.Invite.TokenSHA) {
		return Runtime{}, "", errors.New("the invite token is not valid for this runtime")
	}
	if now.After(runtime.Invite.ExpiresAt) {
		return Runtime{}, "", fmt.Errorf("the invite for %s expired at %s; invite it again", name, runtime.Invite.ExpiresAt.Format(time.RFC3339))
	}
	if strings.TrimSpace(claim.Advertise) == "" {
		return Runtime{}, "", errors.New("the host did not say how to reach it (advertise)")
	}
	if claim.Agent == "" {
		return Runtime{}, "", errors.New("the host did not name its agent runtime")
	}

	runtime.URL = claim.URL
	runtime.Advertise = claim.Advertise
	runtime.Agent = claim.Agent
	runtime.AgentProfile = claim.AgentProfile
	runtime.AgentVersion = claim.AgentVersion
	runtime.FlowHubVersion = claim.FlowHubVersion
	runtime.MaxConcurrent = claim.MaxConcurrent
	runtime.Models = claim.Models
	if claim.Report != nil {
		encoded, err := json.Marshal(claim.Report)
		if err != nil {
			return Runtime{}, "", fmt.Errorf("the capability report could not be read: %w", err)
		}
		runtime.ReportSHA256 = hashToken(string(encoded))
	}
	runtime.RegisteredAt = now.UTC()

	if prober != nil {
		if err := prober.Probe(ctx, claim.Advertise); err != nil {
			runtime.Note = fmt.Sprintf("registered but not reachable at %s: %v", claim.Advertise, err)
			if saveErr := i.save(); saveErr != nil {
				return Runtime{}, "", saveErr
			}
			return runtime.Redacted(), "", fmt.Errorf("cannot reach %s: %w", claim.Advertise, err)
		}
	}

	secret, err := newToken()
	if err != nil {
		return Runtime{}, "", err
	}
	runtime.SecretSHA256 = hashToken(secret)
	runtime.State = StateActive
	runtime.Note = ""
	runtime.Invite = nil
	runtime.LastSeen = now.UTC()
	if err := i.save(); err != nil {
		return Runtime{}, "", err
	}
	return runtime.Redacted(), secret, nil
}

// Heartbeat records that a host is still there and replaces what the control plane
// knows about it with the report it just pushed.
//
// A host that is not active is refused, and the refusal names the state: a revoked
// host has to learn that it was revoked instead of quietly believing it still works.
func (i *Inventory) Heartbeat(name, secret string, report map[string]any, models map[string]string, now time.Time) (Runtime, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	runtime, ok := i.runtimes[normaliseName(name)]
	if !ok {
		return Runtime{}, fmt.Errorf("runtime %s is not registered", name)
	}
	if runtime.State != StateActive {
		return Runtime{}, fmt.Errorf("runtime %s is %s, not active", name, runtime.State)
	}
	if !sameToken(secret, runtime.SecretSHA256) {
		return Runtime{}, errors.New("the runtime secret is not valid")
	}
	if report != nil {
		if encoded, err := json.Marshal(report); err == nil {
			runtime.ReportSHA256 = hashToken(string(encoded))
		}
	}
	// A host that repaired an agent profile reports the model it now pins, and the
	// dispatcher pins that on the next turn. An empty map leaves what enrolment
	// recorded alone: "this build did not report models" is not the same claim as
	// "this host runs no model", and clearing it would silently return every turn to
	// whatever the agent server happens to have cached.
	if len(models) > 0 {
		runtime.Models = models
	}
	runtime.LastSeen = now.UTC()
	if err := i.save(); err != nil {
		return Runtime{}, err
	}
	return runtime.Redacted(), nil
}

// Rotate issues a new secret for an active runtime.
func (i *Inventory) Rotate(name string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	runtime, ok := i.runtimes[normaliseName(name)]
	if !ok {
		return "", fmt.Errorf("runtime %s is not registered", name)
	}
	secret, err := newToken()
	if err != nil {
		return "", err
	}
	runtime.SecretSHA256 = hashToken(secret)
	if runtime.State == StateRevoked {
		runtime.State = StatePending
		runtime.Invite = &Invite{TokenSHA: hashToken(secret), ExpiresAt: i.now().UTC().Add(DefaultInviteTTL)}
	}
	if err := i.save(); err != nil {
		return "", err
	}
	return secret, nil
}

// BoundTasksFunc reports the non-terminal tasks bound to a runtime. It is
// injected so this package does not depend on the task registry.
type BoundTasksFunc func(runtime string) []string

// Remove revokes a runtime.
//
// A runtime with tasks bound to it is refused unless force is set: those tasks
// cannot move (ADR 0001), so removing their host silently would leave them to fail
// one at a time. With force they stay bound and are refused at their next turn,
// which is visible in the log.
func (i *Inventory) Remove(name string, force bool, bound BoundTasksFunc) (Runtime, []string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	runtime, ok := i.runtimes[normaliseName(name)]
	if !ok {
		return Runtime{}, nil, fmt.Errorf("runtime %s is not registered", name)
	}
	var tasks []string
	if bound != nil {
		tasks = bound(runtime.Name)
	}
	if len(tasks) > 0 && !force {
		return Runtime{}, tasks, fmt.Errorf("runtime %s has %d task(s) bound to it (%s); re-run with force to remove it anyway",
			runtime.Name, len(tasks), summarise(tasks))
	}
	runtime.State = StateRevoked
	runtime.SecretSHA256 = ""
	runtime.Invite = nil
	if len(tasks) > 0 {
		runtime.Note = fmt.Sprintf("removed with %d task(s) still bound: %s", len(tasks), summarise(tasks))
	} else {
		runtime.Note = ""
	}
	if err := i.save(); err != nil {
		return Runtime{}, nil, err
	}
	return runtime.Redacted(), tasks, nil
}

// Get returns one runtime.
func (i *Inventory) Get(name string) (Runtime, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	runtime, ok := i.runtimes[normaliseName(name)]
	if !ok {
		return Runtime{}, false
	}
	return runtime.Redacted(), true
}

// List returns every runtime, sorted by name.
func (i *Inventory) List() []Runtime {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.list()
}

func (i *Inventory) list() []Runtime {
	out := make([]Runtime, 0, len(i.runtimes))
	for _, runtime := range i.runtimes {
		out = append(out, runtime.Redacted())
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// Active returns the runtimes usable for work, sorted by name. The dispatcher uses
// it; the ordering is what makes a choice reproducible.
func (i *Inventory) Active() []Runtime {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []Runtime
	for _, runtime := range i.list() {
		if runtime.State == StateActive {
			out = append(out, runtime)
		}
	}
	return out
}

// SecretMatches reports whether a secret belongs to a runtime, without revealing
// the stored hash to the caller.
func (i *Inventory) SecretMatches(name, secret string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	runtime, ok := i.runtimes[normaliseName(name)]
	if !ok || runtime.State != StateActive {
		return false
	}
	return sameToken(secret, runtime.SecretSHA256)
}

// Len is the number of runtimes in the inventory, whatever their state.
func (i *Inventory) Len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.runtimes)
}

// Counts summarises the inventory for /healthz.
func (i *Inventory) Counts() map[string]int {
	i.mu.Lock()
	defer i.mu.Unlock()
	counts := map[string]int{}
	for _, runtime := range i.runtimes {
		counts[string(runtime.State)]++
	}
	return counts
}

func normaliseName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func cleanProjects(projects []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, project := range projects {
		project = strings.TrimSpace(project)
		if project == "" || seen[project] {
			continue
		}
		seen[project] = true
		out = append(out, project)
	}
	sort.Strings(out)
	return out
}

func summarise(tasks []string) string {
	if len(tasks) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(tasks[:3], ", "), len(tasks)-3)
	}
	return strings.Join(tasks, ", ")
}

// newToken is a 256-bit random value, hex encoded — the same shape the webhook URL
// key uses, so an operator has one mental model for every FlowHub secret.
func newToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("cannot generate a token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sameToken compares two hashes in constant time. An empty stored hash never
// matches, so a revoked runtime stays revoked.
func sameToken(token, storedHash string) bool {
	if storedHash == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(storedHash)) == 1
}
