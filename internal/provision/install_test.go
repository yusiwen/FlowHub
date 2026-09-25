package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installRoot builds a throwaway configuration tree: <tmp>/opencode for the agent
// and, beside it, <tmp>/flowhub for the manifest.
func installRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "opencode")
}

func installOptions(t *testing.T, force bool) InstallOptions {
	t.Helper()
	return InstallOptions{
		Agent:          AgentOpenCode,
		ConfigRoot:     installRoot(t),
		FlowHubVersion: "test-version",
		Force:          force,
	}
}

func agentTarget(root string) string {
	return filepath.Join(root, "agents", "devops.md")
}

func TestInstallWritesTheEmbeddedAgentAndManifest(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()

	report, err := Install(runner, opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !report.Installed() {
		t.Fatal("Install reported nothing written")
	}

	embedded, err := ReadAsset(Asset{Source: "assets/opencode/agents/devops.md"})
	if err != nil {
		t.Fatalf("ReadAsset: %v", err)
	}
	onDisk, err := os.ReadFile(agentTarget(opts.ConfigRoot))
	if err != nil {
		t.Fatalf("the agent file was not written: %v", err)
	}
	if string(onDisk) != string(embedded) {
		t.Fatal("the written file does not match the embedded asset")
	}

	manifestPath := ManifestPathFor(opts.ConfigRoot)
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest.FlowHub != "test-version" || manifest.Agent != AgentOpenCode {
		t.Fatalf("manifest = %+v", manifest)
	}
	recorded, ok := manifest.Find(agentTarget(opts.ConfigRoot))
	if !ok {
		t.Fatalf("the installed file is not in the manifest: %+v", manifest.Artifacts)
	}
	if recorded.SHA256 != hashBytes(embedded) {
		t.Fatal("the manifest hash does not match what was written")
	}
	// A fresh tree has no agent configuration, so the MCP file is created; an
	// existing one is never touched (see TestMCPFileIsNeverRewritten).
	if !report.MCPWritten {
		t.Fatal("a fresh tree should have had the MCP file created")
	}
	if !fileExists(report.MCPPath) {
		t.Fatal("the MCP file was reported as written but is not there")
	}
}

// TestSecondInstallIsANoOp is the idempotence guarantee: re-running init must not
// rewrite anything, or a fleet of hosts would churn on every run.
func TestSecondInstallIsANoOp(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()

	if _, err := Install(runner, opts); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	before, err := os.Stat(agentTarget(opts.ConfigRoot))
	if err != nil {
		t.Fatal(err)
	}

	second, err := Install(runner, opts)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if second.Installed() {
		t.Fatalf("the second run wrote something: %+v", second.Files)
	}
	for _, file := range second.Files {
		if file.Action != ActionCurrent {
			t.Fatalf("second run action = %s for %s", file.Action, file.Path)
		}
	}
	after, err := os.Stat(agentTarget(opts.ConfigRoot))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the file was rewritten even though it was current")
	}
}

// TestHandEditedFileIsRefusedWithADiff is the guarantee that makes the installer
// acceptable to run: it does not silently destroy an operator's edit.
func TestHandEditedFileIsRefusedWithADiff(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()
	if _, err := Install(runner, opts); err != nil {
		t.Fatalf("Install: %v", err)
	}

	edited := []byte("---\nmode: primary\n---\n\nmy own agent, do not touch\n")
	if err := os.WriteFile(agentTarget(opts.ConfigRoot), edited, 0o644); err != nil {
		t.Fatal(err)
	}

	plan, _, err := Plan(runner, opts)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !plan.Refused() {
		t.Fatal("a hand-edited file was not refused")
	}
	var found bool
	for _, file := range plan.Files {
		if file.Path != agentTarget(opts.ConfigRoot) {
			continue
		}
		found = true
		if file.State != StateModified || file.Action != ActionRefused {
			t.Fatalf("file plan = %+v", file)
		}
		if !strings.Contains(file.Diff, "my own agent, do not touch") {
			t.Fatalf("the diff does not show the edit:\n%s", file.Diff)
		}
		// Something only the embedded file has, so the operator can see both sides.
		if !strings.Contains(file.Diff, "description: FlowHub's unattended DevOps agent") {
			t.Fatalf("the diff does not show the intended content:\n%s", file.Diff)
		}
		assertChangeOrder(t, file.Diff)
	}
	if !found {
		t.Fatal("the agent file was not in the plan")
	}

	// The file on disk is untouched.
	current, err := os.ReadFile(agentTarget(opts.ConfigRoot))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(edited) {
		t.Fatal("the refusal still modified the file")
	}
}

func TestForceOverwritesTheEdit(t *testing.T) {
	opts := installOptions(t, true)
	runner := healthyHost()
	if _, err := Install(runner, opts); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := os.WriteFile(agentTarget(opts.ConfigRoot), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Install(runner, opts)
	if err != nil {
		t.Fatalf("Install with force: %v", err)
	}
	if report.Refused() {
		t.Fatal("--force still refused")
	}
	content, err := os.ReadFile(agentTarget(opts.ConfigRoot))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) == "mine\n" {
		t.Fatal("--force did not overwrite")
	}
}

// TestOutdatedIsUpdatedWithoutForce covers the upgrade path: the file matches what
// FlowHub wrote, and this build embeds something newer.
func TestOutdatedIsUpdatedWithoutForce(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()
	if _, err := Install(runner, opts); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Pretend the previous install wrote something else: same hash in the manifest,
	// different content on disk.
	manifestPath := ManifestPathFor(opts.ConfigRoot)
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	stale := []byte("previous version\n")
	if err := os.WriteFile(agentTarget(opts.ConfigRoot), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	for index := range manifest.Artifacts {
		if manifest.Artifacts[index].Path == agentTarget(opts.ConfigRoot) {
			manifest.Artifacts[index].SHA256 = hashBytes(stale)
		}
	}
	if err := manifest.Save(manifestPath); err != nil {
		t.Fatal(err)
	}

	report, err := Install(runner, opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if report.Refused() {
		t.Fatal("an outdated file was refused; it should be updated")
	}
	content, _ := os.ReadFile(agentTarget(opts.ConfigRoot))
	if string(content) == string(stale) {
		t.Fatal("the outdated file was not updated")
	}
}

// TestMissingManagedFileIsRestored covers the third case in the manifest rules.
func TestMissingManagedFileIsRestored(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()
	if _, err := Install(runner, opts); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := os.Remove(agentTarget(opts.ConfigRoot)); err != nil {
		t.Fatal(err)
	}

	report, err := Install(runner, opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	var restored bool
	for _, file := range report.Files {
		if file.Path == agentTarget(opts.ConfigRoot) && file.State == StateMissing && file.Action == ActionInstall {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("a missing managed file was not restored: %+v", report.Files)
	}
	if !fileExists(agentTarget(opts.ConfigRoot)) {
		t.Fatal("the file is still missing")
	}
}

// TestUnmanagedIdenticalFileIsAdopted keeps the first install on a host that
// already has the hand-written file from working correctly: nothing is
// overwritten, and the file becomes managed so a later uninstall can clean it up.
func TestUnmanagedIdenticalFileIsAdopted(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()
	embedded, err := ReadAsset(Asset{Source: "assets/opencode/agents/devops.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(agentTarget(opts.ConfigRoot)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentTarget(opts.ConfigRoot), embedded, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Install(runner, opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	var adopted bool
	for _, file := range report.Files {
		if file.Path == agentTarget(opts.ConfigRoot) && file.Action == ActionAdopt {
			adopted = true
		}
	}
	if !adopted {
		t.Fatalf("an identical unmanaged file was not adopted: %+v", report.Files)
	}
	manifest, err := LoadManifest(ManifestPathFor(opts.ConfigRoot))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest.Find(agentTarget(opts.ConfigRoot)); !ok {
		t.Fatal("the adopted file is not in the manifest")
	}
}

func TestUninstallRemovesOnlyWhatItWrote(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()
	if _, err := Install(runner, opts); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// A file FlowHub never wrote, in the same directory, must survive.
	stranger := filepath.Join(opts.ConfigRoot, "agents", "triage.md")
	if err := os.WriteFile(stranger, []byte("someone else's agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Uninstall(runner, opts)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(report.Removed) == 0 {
		t.Fatal("nothing was removed")
	}
	if fileExists(agentTarget(opts.ConfigRoot)) {
		t.Fatal("the managed file is still there")
	}
	if !fileExists(stranger) {
		t.Fatal("uninstall deleted a file FlowHub never wrote")
	}
	if fileExists(ManifestPathFor(opts.ConfigRoot)) {
		t.Fatal("the manifest should be gone once nothing is left")
	}
	// The directory stays because it is not empty, and that is worth saying.
	if !containsSubstring(report.Warnings, "was kept") {
		t.Fatalf("expected a warning about the surviving directory: %v", report.Warnings)
	}
}

func TestUninstallKeepsAnEditedFileUnlessForced(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()
	if _, err := Install(runner, opts); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := os.WriteFile(agentTarget(opts.ConfigRoot), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Uninstall(runner, opts)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !report.Refused() {
		t.Fatal("uninstall deleted an edited file without --force")
	}
	if !fileExists(agentTarget(opts.ConfigRoot)) {
		t.Fatal("the edited file was deleted anyway")
	}
	if !fileExists(ManifestPathFor(opts.ConfigRoot)) {
		t.Fatal("the manifest should survive while a file is still managed")
	}

	forced := opts
	forced.Force = true
	if _, err := Uninstall(runner, forced); err != nil {
		t.Fatalf("Uninstall --force: %v", err)
	}
	if fileExists(agentTarget(opts.ConfigRoot)) {
		t.Fatal("--force did not delete the edited file")
	}
}

func TestUninstallWithNoManifestSaysSo(t *testing.T) {
	report, err := Uninstall(healthyHost(), installOptions(t, false))
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(report.Warnings) == 0 || !containsSubstring(report.Warnings, "nothing recorded") {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

// TestMCPFileIsNeverRewritten is the v1 decision: an existing agent configuration
// is the operator's, and a JSON round-trip would delete its comments.
func TestMCPFileIsNeverRewritten(t *testing.T) {
	opts := installOptions(t, false)
	runner := healthyHost()
	existing := "{\n  // my own comment, keep it\n  \"mcp\": {}\n}\n"
	if err := os.MkdirAll(opts.ConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(opts.ConfigRoot, "opencode.json")
	if err := os.WriteFile(mcpPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Install(runner, opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if report.MCPWritten {
		t.Fatal("the existing configuration was overwritten")
	}
	content, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != existing {
		t.Fatalf("the file changed:\n%s", content)
	}
	if !strings.Contains(report.MCP, "youtrack") {
		t.Fatalf("the snippet does not mention the MCP server: %s", report.MCP)
	}
	if _, err := LoadManifest(ManifestPathFor(opts.ConfigRoot)); err != nil {
		t.Fatal(err)
	}
	manifest, _ := LoadManifest(ManifestPathFor(opts.ConfigRoot))
	if _, managed := manifest.Find(mcpPath); managed {
		t.Fatal("a file FlowHub did not write was recorded as managed")
	}
}

func TestEveryDeclaredAssetExistsInTheBuild(t *testing.T) {
	paths, err := embeddedAssetPaths()
	if err != nil {
		t.Fatalf("walking the embedded tree: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no embedded assets; the embed directive is wrong")
	}
	for _, asset := range []Asset{{Source: "assets/opencode/agents/devops.md"}, {Source: "assets/opencode/mcp.json"}} {
		content, err := ReadAsset(asset)
		if err != nil {
			t.Fatalf("ReadAsset(%s): %v", asset.Source, err)
		}
		if len(content) == 0 {
			t.Fatalf("%s is empty", asset.Source)
		}
	}
	// The agent definition must keep the frontmatter fields the whole permission
	// model depends on: without them the agent is not hidden, has no step budget,
	// and gating edit/bash is gone.
	agent, err := ReadAsset(Asset{Source: "assets/opencode/agents/devops.md"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mode: primary", "steps:", "edit: ask", "bash: ask", "external_directory: deny", "<!-- flowhub-auto -->"} {
		if !strings.Contains(string(agent), want) {
			t.Errorf("the embedded agent definition is missing %q", want)
		}
	}
}

// TestPlanReportsWithoutWriting is what makes --check safe to run anywhere: the
// plan describes the same decisions as the install, and touches nothing.
func TestPlanReportsWithoutWriting(t *testing.T) {
	opts := installOptions(t, false)
	plan, _, err := Plan(healthyHost(), opts)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Files) == 0 {
		t.Fatal("the plan is empty")
	}
	for _, file := range plan.Files {
		if file.Action != ActionInstall {
			t.Fatalf("a fresh tree should plan an install, got %s for %s", file.Action, file.Path)
		}
	}
	if fileExists(agentTarget(opts.ConfigRoot)) || fileExists(plan.MCPPath) || fileExists(plan.ManifestPath) {
		t.Fatal("Plan wrote a file")
	}
}

// TestTheInstalledProfileMatchesTheClaim keeps the name FlowHub installs and the
// name a turn asks for from drifting apart: the claim reports the profile, and it
// has to be the profile the asset actually defines.
func TestTheInstalledProfileMatchesTheClaim(t *testing.T) {
	profile := AgentProfileFor(AgentOpenCode)
	if profile == "" {
		t.Fatal("no agent profile is defined for opencode")
	}
	assets, err := AssetsFor(AgentOpenCode)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSuffix(filepath.Base(assets[0].Target), ".md")
	if profile != want {
		t.Fatalf("AgentProfileFor = %q but the installed asset defines %q", profile, want)
	}
	if profile == AgentOpenCode {
		t.Fatal("the profile must not be the product name")
	}
}
