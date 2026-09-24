package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ManifestVersion is the on-disk schema version of the manifest.
const ManifestVersion = 1

// ManifestFileName lives in FlowHub's own configuration directory rather than
// beside the artifacts. The agent's configuration directory belongs to the agent:
// dropping FlowHub bookkeeping into it would be one more foreign file for an
// operator to wonder about, and it would not be found again if the agent's config
// directory moved.
const ManifestFileName = "manifest.json"

// Manifest records what FlowHub installed, so a later run can tell "already
// current" from "the binary changed" from "a human edited this".
type Manifest struct {
	Version    int         `json:"version"`
	Home       string      `json:"home"`
	Agent      string      `json:"agent"`
	FlowHub    string      `json:"flowhub_version"`
	Installed  time.Time   `json:"installed_at"`
	Artifacts  []Installed `json:"artifacts"`
	CreatedDir []string    `json:"created_dirs,omitempty"`
}

// Installed is one file the manifest vouches for.
type Installed struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// SHA256 is the hash of what FlowHub wrote, not of what is there now.
	SHA256    string `json:"sha256"`
	Installer string `json:"installer"`
	Gone      bool   `json:"-"`
}

// LoadManifest reads the manifest, returning an empty one when it does not exist.
func LoadManifest(path string) (*Manifest, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Manifest{Version: ManifestVersion}, nil
		}
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	manifest := &Manifest{}
	if err := json.Unmarshal(content, manifest); err != nil {
		// A corrupt manifest must stop the run. Guessing what was installed is how
		// an uninstall deletes a file FlowHub never wrote.
		return nil, fmt.Errorf("manifest %s is not readable (%w); fix or remove it before continuing", path, err)
	}
	if manifest.Version != ManifestVersion {
		return nil, fmt.Errorf("manifest %s has version %d, this build understands %d", path, manifest.Version, ManifestVersion)
	}
	return manifest, nil
}

// Save writes the manifest atomically, so an interrupted run cannot leave a
// half-written record of what is on disk.
func (m *Manifest) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].Path < m.Artifacts[j].Path })
	sort.Strings(m.CreatedDir)

	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	temp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.tmp")
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
	return os.Rename(tempName, path)
}

// Find returns the manifest entry for a path.
func (m *Manifest) Find(path string) (Installed, bool) {
	for _, artifact := range m.Artifacts {
		if artifact.Path == path {
			return artifact, true
		}
	}
	return Installed{}, false
}

// Remove drops a path from the manifest.
func (m *Manifest) Remove(path string) {
	kept := m.Artifacts[:0]
	for _, artifact := range m.Artifacts {
		if artifact.Path != path {
			kept = append(kept, artifact)
		}
	}
	m.Artifacts = kept
}

// State classifies a managed file against what FlowHub wrote and what this build
// would write.
type State string

const (
	// StateMissing: the file is not on disk.
	StateMissing State = "missing"
	// StateCurrent: the file matches this build's content.
	StateCurrent State = "current"
	// StateOutdated: the file matches what FlowHub installed, but this build
	// embeds something newer — the normal upgrade path.
	StateOutdated State = "outdated"
	// StateModified: the file on disk is not what FlowHub wrote. A human edited
	// it, and FlowHub does not overwrite that without being told to.
	StateModified State = "modified"
	// StateUnmanaged: the file exists and FlowHub has no record of writing it.
	StateUnmanaged State = "unmanaged"
	// StateUnreadable: the file could not be read. Never treated as "missing".
	StateUnreadable State = "unreadable"
)

// classify compares a target file with the manifest entry and the embedded
// content. It never treats "could not read" as "absent": reporting a permission
// failure as a missing file sends an operator looking for something that is right
// there.
func classify(target string, recorded Installed, managed bool, embedded []byte) (State, string, error) {
	content, err := os.ReadFile(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StateMissing, "", nil
		}
		return StateUnreadable, "", fmt.Errorf("read %s: %w", target, err)
	}
	current := hashBytes(content)
	if !managed {
		return StateUnmanaged, current, nil
	}
	if current == hashBytes(embedded) {
		return StateCurrent, current, nil
	}
	if current == recorded.SHA256 {
		return StateOutdated, current, nil
	}
	return StateModified, current, nil
}
