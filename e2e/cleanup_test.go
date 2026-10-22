//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanupTestResources deletes issues created by this suite and removes the
// suite's temp directories. It never deletes the operator's existing E2E_ISSUE and
// only matches the suite's summary prefixes.
func TestCleanupTestResources(t *testing.T) {
	if os.Getenv("E2E_CLEANUP") != "1" {
		t.Skip("set E2E_CLEANUP=1 (or use make e2e-clean)")
	}
	var issues []struct {
		ID         string `json:"id"`
		IDReadable string `json:"idReadable"`
		Summary    string `json:"summary"`
	}
	query := "/api/issues?query=" + url.QueryEscape("project: "+suite.ytProject) + "&fields=id,idReadable,summary&$top=500"
	if err := suite.ytRequest(http.MethodGet, query, nil, &issues); err != nil {
		t.Fatalf("list issues: %v", err)
	}
	deleted := 0
	for _, issue := range issues {
		if issue.IDReadable == suite.ytIssue || !isE2EIssueSummary(issue.Summary) {
			continue
		}
		if os.Getenv("E2E_CLEANUP_DRY") == "1" {
			t.Logf("would delete %s: %s", issue.IDReadable, issue.Summary)
			continue
		}
		if err := suite.ytRequest(http.MethodDelete, "/api/issues/"+issue.ID, nil, nil); err != nil {
			t.Fatalf("delete %s: %v", issue.IDReadable, err)
		}
		t.Logf("deleted %s: %s", issue.IDReadable, issue.Summary)
		deleted++
	}
	if os.Getenv("E2E_CLEANUP_DRY") == "1" {
		t.Logf("dry run: no issues deleted")
		return
	}
	removeE2ETempDirs(t)
	pruneTestRepoWorktrees(t)
	t.Logf("deleted %d issue(s)", deleted)
}

func isE2EIssueSummary(summary string) bool {
	for _, prefix := range []string{"FlowHub E2E ", "FlowHub live journey "} {
		if strings.HasPrefix(summary, prefix) {
			return true
		}
	}
	return false
}

func removeE2ETempDirs(t *testing.T) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "flowhub-e2e-*"))
	if err != nil {
		t.Logf("temp dirs: %v", err)
		return
	}
	for _, dir := range matches {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("remove %s: %v", dir, err)
			continue
		}
		t.Logf("removed temp dir %s", dir)
	}
}

func pruneTestRepoWorktrees(t *testing.T) {
	t.Helper()
	command := exec.Command("git", "-C", suite.repo, "worktree", "prune")
	if output, err := command.CombinedOutput(); err != nil {
		t.Logf("git worktree prune: %v: %s", err, output)
	}
}
