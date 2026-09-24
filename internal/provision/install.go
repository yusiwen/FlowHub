package provision

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// InstallOptions configures installation and removal.
type InstallOptions struct {
	// Agent is the agent runtime whose configuration is being managed.
	Agent string
	// ConfigRoot is the agent's configuration directory. Empty resolves to
	// $XDG_CONFIG_HOME/<agent> or ~/.config/<agent>. It is a flag because a test
	// (and an unusual deployment) must be able to install into a throwaway tree
	// instead of the operator's real configuration.
	ConfigRoot string
	// FlowHubVersion is recorded in the manifest so an operator can tell which
	// binary wrote what.
	FlowHubVersion string
	// Force overwrites a file a human changed, and deletes one on uninstall.
	Force bool
}

// Action is what the plan does to one file.
type Action string

const (
	ActionCurrent Action = "current"
	ActionInstall Action = "install"
	ActionUpdate  Action = "update"
	ActionAdopt   Action = "adopt"
	ActionRefused Action = "refused"
)

// FilePlan is the decision for one artifact.
type FilePlan struct {
	Asset  Asset
	Path   string
	State  State
	Action Action
	// Diff is filled when the action is refused, so the operator sees what they
	// changed rather than being told to trust the tool.
	Diff string
	// Note explains an unusual decision in one line.
	Note string
}

// InstallReport is the outcome of one install or uninstall run.
type InstallReport struct {
	ManifestPath string
	Files        []FilePlan
	// Removed are the paths uninstall deleted.
	Removed []string
	// MCP is the snippet an operator has to merge, when the agent's configuration
	// file already exists and therefore is not touched.
	MCP string
	// MCPPath is where the snippet belongs.
	MCPPath string
	// MCPWritten reports whether FlowHub created that file.
	MCPWritten bool
	// Warnings are non-fatal problems, such as a leftover directory.
	Warnings []string
}

// Installed reports whether anything was written.
func (r *InstallReport) Installed() bool {
	for _, file := range r.Files {
		switch file.Action {
		case ActionInstall, ActionUpdate, ActionAdopt:
			return true
		}
	}
	return r.MCPWritten || len(r.Removed) > 0
}

// Refused reports whether anything was left alone because a human changed it.
func (r *InstallReport) Refused() bool {
	for _, file := range r.Files {
		if file.Action == ActionRefused {
			return true
		}
	}
	return false
}

// ConfigRootFor resolves the agent's configuration directory.
func ConfigRootFor(runner Runner, opts InstallOptions) string {
	if root := strings.TrimSpace(opts.ConfigRoot); root != "" {
		return root
	}
	return agentConfigDir(runner)
}

// ManifestPathFor places the manifest in FlowHub's own configuration directory,
// which is the sibling of the agent's root: ~/.config/opencode and
// ~/.config/flowhub. Keeping it out of the agent's tree means uninstall can find
// it even if the agent's layout changes, and the agent's directory holds only the
// agent's own files.
func ManifestPathFor(configRoot string) string {
	parent := filepath.Dir(filepath.Clean(configRoot))
	if parent == "." || parent == string(filepath.Separator) {
		// A relative or root-level configuration directory: keep the manifest
		// beside it rather than in the filesystem root.
		parent = filepath.Clean(configRoot)
	}
	return filepath.Join(parent, "flowhub", ManifestFileName)
}

// Plan decides what would happen, without touching anything. Install and
// uninstall both go through it, so `--check`-style reporting comes for free.
func Plan(runner Runner, opts InstallOptions) (*InstallReport, []Asset, error) {
	assets, err := AssetsFor(opts.Agent)
	if err != nil {
		return nil, nil, err
	}
	configRoot := ConfigRootFor(runner, opts)
	manifestPath := ManifestPathFor(configRoot)

	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, nil, err
	}

	report := &InstallReport{ManifestPath: manifestPath}
	for _, asset := range assets {
		target := filepath.Join(configRoot, asset.Target)
		embed, err := ReadAsset(asset)
		if err != nil {
			return nil, nil, err
		}
		recorded, managed := manifest.Find(target)
		state, _, err := classify(target, recorded, managed, embed)
		if err != nil {
			return nil, nil, err
		}

		plan := FilePlan{Asset: asset, Path: target, State: state}
		switch state {
		case StateCurrent:
			plan.Action = ActionCurrent
		case StateMissing:
			plan.Action = ActionInstall
		case StateOutdated:
			plan.Action = ActionUpdate
			plan.Note = "this build embeds a newer version"
		case StateUnmanaged:
			// The file is there and FlowHub has no record of writing it. If it
			// already matches, adopt it: recording it costs nothing and makes a
			// later uninstall able to clean up. If it differs, it is somebody's
			// file and overwriting it needs --force.
			onDisk, readErr := os.ReadFile(target)
			if readErr == nil && hashBytes(onDisk) == hashBytes(embed) {
				plan.Action = ActionAdopt
				plan.Note = "already identical, now managed by FlowHub"
				break
			}
			plan.Action = ActionRefused
			if readErr != nil {
				plan.Diff = fmt.Sprintf("(could not read %s: %v)\n", target, readErr)
			} else {
				plan.Diff = unifiedDiff(string(onDisk), string(embed), 3)
			}
			if opts.Force {
				plan.Action = ActionUpdate
				plan.Note = "overwriting a file FlowHub did not write (--force)"
			}
		case StateModified:
			plan.Action = ActionRefused
			if content, readErr := os.ReadFile(target); readErr == nil {
				plan.Diff = unifiedDiff(string(content), string(embed), 3)
			}
			plan.Note = "edited since FlowHub wrote it"
			if opts.Force {
				plan.Action = ActionUpdate
				plan.Note = "overwriting your edit (--force)"
			}
		default:
			plan.Action = ActionRefused
			plan.Note = "the file could not be read"
		}
		report.Files = append(report.Files, plan)
	}
	report.MCP, err = MCPConfigSnippet()
	if err != nil {
		return nil, nil, err
	}
	report.MCPPath = filepath.Join(configRoot, "opencode.json")
	return report, assets, nil
}

// Install writes the embedded artifacts and records them in the manifest.
//
// It refuses to overwrite anything a human changed unless Force is set, and it
// never touches the agent's MCP configuration when that file already exists.
func Install(runner Runner, opts InstallOptions) (*InstallReport, error) {
	report, _, err := Plan(runner, opts)
	if err != nil {
		return nil, err
	}
	manifest, err := LoadManifest(report.ManifestPath)
	if err != nil {
		return nil, err
	}
	manifest.Agent = opts.Agent
	manifest.Home = runner.Identity().Home
	manifest.FlowHub = opts.FlowHubVersion
	if manifest.Installed.IsZero() {
		manifest.Installed = time.Now().UTC()
	}

	for index := range report.Files {
		plan := &report.Files[index]
		if plan.Action == ActionCurrent || plan.Action == ActionRefused {
			continue
		}
		embed, err := ReadAsset(plan.Asset)
		if err != nil {
			return nil, err
		}
		created, err := writeFile(plan.Path, embed, plan.Asset.Mode)
		if err != nil {
			return nil, err
		}
		manifest.CreatedDir = append(manifest.CreatedDir, created...)
		manifest.Remove(plan.Path)
		manifest.Artifacts = append(manifest.Artifacts, Installed{
			Name:      plan.Asset.Name,
			Path:      plan.Path,
			SHA256:    hashBytes(embed),
			Installer: plan.Asset.Installer,
		})
	}

	// The MCP block is printed, never merged. v1 only creates the file when there
	// is nothing to destroy; an existing configuration may hold comments and other
	// servers, and a JSON round-trip would quietly delete both.
	if !fileExists(report.MCPPath) {
		created, err := writeFile(report.MCPPath, []byte(report.MCP), 0o644)
		if err != nil {
			return nil, err
		}
		manifest.CreatedDir = append(manifest.CreatedDir, created...)
		manifest.Remove(report.MCPPath)
		manifest.Artifacts = append(manifest.Artifacts, Installed{
			Name:      "opencode-mcp",
			Path:      report.MCPPath,
			SHA256:    hashBytes([]byte(report.MCP)),
			Installer: "flowhub runtime init --agent=opencode",
		})
		report.MCPWritten = true
	}

	// The manifest's own directory counts too: uninstall should be able to leave
	// the tree exactly as it found it.
	createdForManifest, err := ensureDir(filepath.Dir(report.ManifestPath))
	if err != nil {
		return nil, err
	}
	manifest.CreatedDir = append(manifest.CreatedDir, createdForManifest...)
	if err := manifest.Save(report.ManifestPath); err != nil {
		return nil, err
	}
	return report, nil
}

// Uninstall removes exactly what the manifest records, and nothing else.
//
// A file a human edited afterwards is kept unless Force is set: the manifest says
// FlowHub wrote it, not that FlowHub owns the current content.
func Uninstall(runner Runner, opts InstallOptions) (*InstallReport, error) {
	configRoot := ConfigRootFor(runner, opts)
	manifestPath := ManifestPathFor(configRoot)
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	report := &InstallReport{ManifestPath: manifestPath}
	if len(manifest.Artifacts) == 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"no manifest at %s: FlowHub has nothing recorded on this host", manifestPath))
		return report, nil
	}

	for _, artifact := range manifest.Artifacts {
		plan := FilePlan{
			Path:  artifact.Path,
			State: StateCurrent,
			Asset: Asset{Name: artifact.Name, Installer: artifact.Installer},
		}
		content, readErr := os.ReadFile(artifact.Path)
		switch {
		case readErr != nil && os.IsNotExist(readErr):
			plan.State = StateMissing
			plan.Action = ActionCurrent
			plan.Note = "already gone"
		case readErr != nil:
			plan.State = StateUnreadable
			plan.Action = ActionRefused
			plan.Note = readErr.Error()
		case hashBytes(content) != artifact.SHA256:
			plan.State = StateModified
			plan.Action = ActionRefused
			plan.Note = "edited since FlowHub wrote it; kept"
			if opts.Force {
				plan.Action = ActionUpdate
				plan.Note = "deleting your edit (--force)"
			}
		default:
			plan.Action = ActionUpdate
			plan.Note = "removed"
		}
		if plan.Action == ActionUpdate {
			if err := os.Remove(artifact.Path); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove %s: %w", artifact.Path, err)
			}
			report.Removed = append(report.Removed, artifact.Path)
			continue
		}
		report.Files = append(report.Files, plan)
	}

	remaining := &Manifest{
		Version:   ManifestVersion,
		Home:      manifest.Home,
		Agent:     manifest.Agent,
		FlowHub:   manifest.FlowHub,
		Installed: manifest.Installed,
	}
	for _, file := range report.Files {
		if recorded, ok := manifest.Find(file.Path); ok {
			remaining.Artifacts = append(remaining.Artifacts, recorded)
		}
	}
	if len(remaining.Artifacts) == 0 {
		if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove manifest %s: %w", manifestPath, err)
		}
	} else if err := remaining.Save(manifestPath); err != nil {
		return nil, err
	}

	// Directories FlowHub created are removed only when empty, deepest first, so a
	// directory that gained other files (the routing table lives beside the
	// manifest in a real deployment) stays where it is.
	//
	// This runs after the manifest is gone: its own directory is one of them.
	dirs := append([]string(nil), manifest.CreatedDir...)
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		if err := os.Remove(dir); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) > 0 {
				report.Warnings = append(report.Warnings, fmt.Sprintf("%s was kept: it is not empty", dir))
			}
			continue
		}
		report.Removed = append(report.Removed, dir)
	}
	return report, nil
}

// writeFile writes atomically and reports every directory it had to create, so a
// later uninstall can take the tree back to exactly what it was.
//
// It works on the real filesystem rather than through the Runner: creating a file
// atomically is the thing under test, so a fake would test nothing.
func writeFile(path string, content []byte, mode os.FileMode) (created []string, err error) {
	dir := filepath.Dir(path)
	created, err = ensureDir(dir)
	if err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return created, err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return created, err
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return created, err
	}
	if err := temp.Close(); err != nil {
		return created, err
	}
	return created, os.Rename(tempName, path)
}

// ensureDir creates a directory and returns every directory it had to create,
// including intermediate ones. Without the intermediates an uninstall leaves empty
// parents behind, which is not "exactly as it was".
func ensureDir(dir string) ([]string, error) {
	if dir == "" || dir == "." {
		return nil, nil
	}
	var missing []string
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		if fileExists(current) {
			break
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	// Deepest first is what the caller appends; uninstall reverses its own order.
	sort.Sort(sort.Reverse(sort.StringSlice(missing)))
	return missing, nil
}

// fileExists reports whether a path exists, whatever its type.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Text renders the install or uninstall outcome for a human.
func (r *InstallReport) Text(w io.Writer, verb string) {
	changed := 0
	for _, file := range r.Files {
		marker := "  keep   "
		switch file.Action {
		case ActionInstall:
			marker = "  create "
		case ActionUpdate:
			marker = "  write  "
		case ActionAdopt:
			marker = "  adopt  "
		case ActionRefused:
			marker = "  REFUSE "
		}
		if file.Action != ActionCurrent {
			changed++
		}
		fmt.Fprintf(w, "%s %s  [%s]%s\n", marker, file.Path, file.State, noteSuffix(file.Note))
		if file.Diff != "" {
			for _, line := range strings.Split(strings.TrimSuffix(file.Diff, "\n"), "\n") {
				fmt.Fprintf(w, "          %s\n", line)
			}
		}
	}
	for _, removed := range r.Removed {
		changed++
		fmt.Fprintf(w, "  remove %s\n", removed)
	}

	if r.MCPPath != "" {
		if r.MCPWritten {
			fmt.Fprintf(w, "  create %s  (the MCP server this agent needs)\n", r.MCPPath)
		} else {
			fmt.Fprintf(w, "\n%s already exists and was not modified. Merge this block yourself:\n\n%s",
				r.MCPPath, indent(r.MCP, "  "))
		}
	}

	fmt.Fprintf(w, "\nmanifest: %s\n", r.ManifestPath)
	if r.Refused() {
		fmt.Fprintln(w, "some files were left alone because a human changed them; re-run with --force to overwrite")
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warning)
	}
	if changed == 0 && !r.MCPWritten {
		fmt.Fprintf(w, "%s: already up to date\n", verb)
	}
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return "  — " + note
}

func indent(text, prefix string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		out.WriteString(prefix)
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
