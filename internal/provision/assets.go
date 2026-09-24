package provision

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Assets are the files FlowHub installs on a runtime host.
//
// They are embedded in the binary on purpose. The agent definition *is* the
// agent's system prompt and the MCP snippet decides which tools exist, so a
// compromised control plane must not be able to push content to every worker:
// updating a fleet means shipping a binary and re-running init (ADR 0002).
//
//go:embed assets
var assets embed.FS

// Asset is one file to install. Source is a path inside the embedded tree,
// Target is relative to the agent's configuration directory.
type Asset struct {
	// Name identifies the asset in reports and in the manifest.
	Name string
	// Source is the path inside the embed.FS.
	Source string
	// Target is the path relative to the configuration root.
	Target string
	// Mode is the file mode to create it with.
	Mode fs.FileMode
	// Installer names why the file is there, so an operator reading the manifest
	// can tell which part of FlowHub owns it.
	Installer string
}

// AssetsFor returns the files this build installs for an agent runtime, in a
// deterministic order.
func AssetsFor(agent string) ([]Asset, error) {
	switch agent {
	case AgentOpenCode:
		return []Asset{{
			Name:      "opencode-agent-devops",
			Source:    "assets/opencode/agents/devops.md",
			Target:    "agents/devops.md",
			Mode:      0o644,
			Installer: "flowhub runtime init --agent=opencode",
		}}, nil
	default:
		return nil, fmt.Errorf("no assets are defined for agent %q", agent)
	}
}

// ReadAsset returns the embedded content of an asset.
func ReadAsset(asset Asset) ([]byte, error) {
	content, err := assets.ReadFile(asset.Source)
	if err != nil {
		return nil, fmt.Errorf("embedded asset %s is missing from this build: %w", asset.Source, err)
	}
	return content, nil
}

// MCPConfigSnippet returns the MCP block an operator has to merge into the
// agent's own configuration.
//
// v1 prints this instead of editing the file: the agent's configuration may
// contain comments and is maintained by a human, and a JSON round-trip would
// quietly delete both comments and key order (ADR 0002, "MCP configuration, v1").
func MCPConfigSnippet() (string, error) {
	content, err := assets.ReadFile("assets/opencode/mcp.json")
	if err != nil {
		return "", fmt.Errorf("embedded MCP snippet is missing from this build: %w", err)
	}
	return string(content), nil
}

// hashBytes is the fingerprint recorded in the manifest and compared on every
// later run.
func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// embeddedAssetPaths lists the embedded tree, which a test uses to prove every
// declared asset actually exists in the binary.
func embeddedAssetPaths() ([]string, error) {
	var paths []string
	err := fs.WalkDir(assets, "assets", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, path.Clean(strings.TrimPrefix(name, "./")))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}
