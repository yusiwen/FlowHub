package provision

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultOpenCodeURL is the agent server a capability check talks to when it needs
// the model catalogue. It is the same default the dispatcher uses.
const DefaultOpenCodeURL = "http://127.0.0.1:4096"

// ModelCatalogue reports which models a running agent server offers.
// providerID -> modelIDs. It is an interface so the check is testable without a
// server.
type ModelCatalogue interface {
	Catalogue(ctx context.Context) (map[string][]string, error)
}

// ProfileCheck is one agent profile's model, resolved against the server.
type ProfileCheck struct {
	Agent string `json:"agent"`
	// Source says which copy was checked: "installed" for the file on disk (the one
	// the agent server actually loads) or "embedded" for what this binary would
	// install. An installed file that pins a dead model is the case worth catching.
	Source string `json:"source,omitempty"`
	// Model is the "provider/model" the profile pins.
	Model string `json:"model"`
	// Available is true when the server's catalogue lists it.
	Available bool `json:"available"`
	// Checked is false when the server could not be asked, which is not the same as
	// "missing": an unreachable server must not be reported as a bad model.
	Checked bool   `json:"checked"`
	Detail  string `json:"detail,omitempty"`
}

// catalogueTimeout bounds the model lookup. The server is local to the host being
// checked, so a slow answer means something is wrong with it.
const catalogueTimeout = 10 * time.Second

// checkProfiles verifies that every model the installed agent profiles pin exists
// on the agent server that will run them.
//
// This is the check that turns a provider quietly dropping a model id into an
// enrolment failure instead of a task that dies with ProviderModelNotFoundError.
// It deliberately does not fail when the server cannot be reached: the binary check
// already covers "is the agent installed", and a server that is not running yet is
// a different problem with a different message.
func checkProfiles(ctx context.Context, catalogue ModelCatalogue, opts Options, report *Report) {
	profiles, err := agentProfiles(opts.Agent, opts.ConfigRoot)
	if err != nil {
		return
	}
	if len(profiles) == 0 {
		return
	}
	if catalogue == nil {
		for _, profile := range profiles {
			report.Profiles = append(report.Profiles, ProfileCheck{
				Agent: profile.Agent, Source: profile.Source, Model: profile.Model,
				Detail: "the model catalogue was not checked",
			})
		}
		return
	}

	lookupCtx, cancel := context.WithTimeout(ctx, catalogueTimeout)
	defer cancel()
	models, err := catalogue.Catalogue(lookupCtx)
	if err != nil {
		for _, profile := range profiles {
			report.Profiles = append(report.Profiles, ProfileCheck{
				Agent: profile.Agent, Source: profile.Source, Model: profile.Model,
				Detail: fmt.Sprintf("the model catalogue could not be read: %v", err),
			})
		}
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"the agent server did not answer, so the model(s) %s could not be verified", strings.Join(profileModels(profiles), ", ")))
		return
	}

	for _, profile := range profiles {
		check := ProfileCheck{Agent: profile.Agent, Source: profile.Source, Model: profile.Model, Checked: true}
		provider, model, ok := splitModel(profile.Model)
		if !ok {
			check.Detail = "the profile's model is not spelled provider/model"
			report.Profiles = append(report.Profiles, check)
			report.Failures = append(report.Failures, fmt.Sprintf(
				"agent profile %s pins model %q, which is not provider/model", profile.Agent, profile.Model))
			continue
		}
		available := false
		for _, candidate := range models[provider] {
			if candidate == model {
				available = true
				break
			}
		}
		check.Available = available
		if !available {
			check.Detail = fmt.Sprintf("provider %s does not offer it (it offers: %s)", provider, strings.Join(models[provider], ", "))
			report.Failures = append(report.Failures, fmt.Sprintf(
				"agent profile %s pins model %s, which this host's agent server does not offer: %s",
				profile.Agent, profile.Model, check.Detail))
		}
		report.Profiles = append(report.Profiles, check)
	}
}

// AgentProfile describes one agent profile and the model it pins.
type AgentProfile struct {
	Agent  string
	Model  string
	Source string
	Path   string
}

// agentProfiles resolves the profiles to check.
//
// The installed file wins when it is there, because that is the copy the agent
// server loads: checking only the embedded asset would report a healthy host while
// the file on disk pins a model its provider no longer offers. That happened here,
// and it cost a task to find out.
func agentProfiles(agent, configRoot string) ([]AgentProfile, error) {
	assets, err := AssetsFor(agent)
	if err != nil {
		return nil, err
	}
	var profiles []AgentProfile
	for _, asset := range assets {
		content, source, path := embeddedAssetContent(asset)
		if content == nil {
			continue
		}
		if configRoot != "" {
			installed := filepath.Join(configRoot, asset.Target)
			if onDisk, err := os.ReadFile(installed); err == nil {
				content, source, path = onDisk, "installed", installed
			}
		}
		model := frontmatterValue(string(content), "model")
		if model == "" {
			continue
		}
		profiles = append(profiles, AgentProfile{
			Agent:  strings.TrimSuffix(baseName(asset.Target), ".md"),
			Model:  model,
			Source: source,
			Path:   path,
		})
	}
	return profiles, nil
}

func embeddedAssetContent(asset Asset) ([]byte, string, string) {
	content, err := ReadAsset(asset)
	if err != nil {
		return nil, "", ""
	}
	return content, "embedded", asset.Source
}

// frontmatterValue reads one key from the frontmatter block at the top of a file.
func frontmatterValue(content, key string) string {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		name, value, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), key) {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

func splitModel(model string) (provider, id string, ok bool) {
	provider, id, found := strings.Cut(strings.TrimSpace(model), "/")
	provider, id = strings.TrimSpace(provider), strings.TrimSpace(id)
	if !found || provider == "" || id == "" {
		return "", "", false
	}
	return provider, id, true
}

func profileModels(profiles []AgentProfile) []string {
	var out []string
	for _, profile := range profiles {
		out = append(out, profile.Model)
	}
	return out
}

func baseName(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}
