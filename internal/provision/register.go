package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/runtimes"
)

// RuntimeIdentityFileName is the runtime's own credentials: which control plane it
// belongs to and the secret it authenticates heartbeats with.
//
// It sits beside the manifest in FlowHub's configuration directory and is written
// 0600. The secret is shown once at registration and never printed again.
const RuntimeIdentityFileName = "runtime.json"

// RuntimeIdentity is what a host needs to report in later. It is distinct from
// Identity in runner.go, which is the *user* the checks ran as.
type RuntimeIdentity struct {
	Version    int       `json:"version"`
	Name       string    `json:"name"`
	Server     string    `json:"server"`
	Secret     string    `json:"secret"`
	Registered time.Time `json:"registered_at"`
}

// IdentityPathFor is where a host keeps its identity.
func RuntimeIdentityPathFor(configRoot string) string {
	return filepath.Join(filepath.Dir(ManifestPathFor(configRoot)), RuntimeIdentityFileName)
}

// LoadIdentity reads a host's identity, returning an error when it is missing or
// unreadable — a host that cannot prove who it is must say so rather than pretend.
func LoadRuntimeIdentity(configRoot string) (RuntimeIdentity, error) {
	path := RuntimeIdentityPathFor(configRoot)
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RuntimeIdentity{}, fmt.Errorf("this host is not registered: no identity at %s", path)
		}
		return RuntimeIdentity{}, err
	}
	var identity RuntimeIdentity
	if err := json.Unmarshal(content, &identity); err != nil {
		return RuntimeIdentity{}, fmt.Errorf("identity %s is not readable: %w", path, err)
	}
	return identity, nil
}

func saveRuntimeIdentity(configRoot string, identity RuntimeIdentity) error {
	path := RuntimeIdentityPathFor(configRoot)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	encoded, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".identity-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(append(encoded, '\n')); err != nil {
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
	return os.Rename(tempName, path)
}

// RegisterOutcome is what the control plane answered.
type RegisterOutcome struct {
	Runtime runtimes.Runtime
	Secret  string
	// Stored is where the identity was written.
	Stored string
}

// Register enrols this host with a control plane.
//
// The capability report travels with the claim, so the control plane records what
// this machine is rather than what it was at invite time. The secret that comes
// back is written to the host's identity file and never printed: it is shown once
// by the control plane and is not recoverable from there either.
func Register(ctx context.Context, client *runtimes.Client, inviteToken, name string, report *Report, advertise string, opts InstallOptions) (RegisterOutcome, error) {
	if inviteToken == "" {
		return RegisterOutcome{}, errors.New("an invite token is required (get one from `flowhub runtime invite` on the control-plane host)")
	}
	if name == "" {
		// The control plane looks the runtime up by the name it was invited under,
		// so guessing one here would fail with "no pending invite".
		return RegisterOutcome{}, errors.New("--name is required: it is the name the control plane invited (see the output of `flowhub runtime invite`)")
	}
	if advertise == "" {
		return RegisterOutcome{}, errors.New("--advertise is required: the control plane probes that address before activating this host")
	}
	claim := runtimes.Claim{
		Name:           name,
		URL:            advertise,
		Advertise:      advertise,
		Agent:          opts.Agent,
		AgentProfile:   AgentProfileFor(opts.Agent),
		FlowHubVersion: opts.FlowHubVersion,
	}
	if report != nil {
		claim.AgentVersion = versionOf(report)
		claim.Report = reportAsMap(report)
		claim.Models = profileModelsOf(report)
	}

	runtime, secret, err := client.Register(ctx, inviteToken, claim)
	if err != nil {
		return RegisterOutcome{}, err
	}
	identity := RuntimeIdentity{
		Version:    1,
		Name:       runtime.Name,
		Server:     client.BaseURL,
		Secret:     secret,
		Registered: time.Now().UTC(),
	}
	if err := saveRuntimeIdentity(opts.ConfigRoot, identity); err != nil {
		return RegisterOutcome{}, err
	}
	return RegisterOutcome{Runtime: runtime, Secret: secret, Stored: RuntimeIdentityPathFor(opts.ConfigRoot)}, nil
}

func versionOf(report *Report) string {
	if report != nil && report.Agent != nil {
		return report.Agent.Version
	}
	return ""
}

// profileModelsOf lists the model each checked agent profile pins, keyed by
// profile name.
//
// The control plane echoes this back when it asks for a turn, so the model a
// capability check verified is the model the turn actually uses. Without it the
// turn relies on the agent server's own resolution, and the server keeps its agent
// list per instance: a server that has been running since before the profile was
// installed answers with the model id it cached, so `init --check` reports the
// pinned model as available while every turn dies with ProviderModelNotFoundError.
// That is exactly what happened on 2026-09-25, and it cost a task to find out.
func profileModelsOf(report *Report) map[string]string {
	if report == nil || len(report.Profiles) == 0 {
		return nil
	}
	models := make(map[string]string, len(report.Profiles))
	for _, profile := range report.Profiles {
		name := strings.TrimSpace(profile.Agent)
		model := strings.TrimSpace(profile.Model)
		if name == "" || model == "" {
			continue
		}
		models[name] = model
	}
	if len(models) == 0 {
		return nil
	}
	return models
}

// reportAsMap renders the capability report as the free-form document the control
// plane stores a fingerprint of. It carries no secret: the report only ever says
// whether an environment variable is present.
func reportAsMap(report *Report) map[string]any {
	if report == nil {
		return nil
	}
	encoded, err := report.JSON()
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}
