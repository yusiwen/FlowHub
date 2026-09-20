package main

import (
	"strings"
	"testing"
)

// versionLine feeds -version, the startup banner and /healthz. The Makefile
// injects Version from `git describe --always --dirty`, which already contains
// the short sha while the repository has no tags, so the line must not repeat it.
func TestVersionLineDoesNotRepeatTheCommit(t *testing.T) {
	original := struct{ version, commit, buildTime string }{Version, CommitSHA, BuildTime}
	t.Cleanup(func() {
		Version, CommitSHA, BuildTime = original.version, original.commit, original.buildTime
	})

	cases := map[string]struct {
		version, commit, buildTime string
		want                       string
	}{
		"describe already carries the sha": {
			version: "23ece6e-dirty", commit: "23ece6e",
			want: "23ece6e-dirty",
		},
		"tagged release prepends the commit once": {
			version: "v1.2.3", commit: "23ece6e",
			want: "v1.2.3 23ece6e",
		},
		"unknown commit is omitted": {
			version: "dev", commit: "unknown",
			want: "dev",
		},
		"build time is appended": {
			version: "v1.2.3", commit: "23ece6e", buildTime: "2026-09-20T12:00:00Z",
			want: "v1.2.3 23ece6e built 2026-09-20T12:00:00Z",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			Version, CommitSHA, BuildTime = tc.version, tc.commit, tc.buildTime
			got := versionLine()
			// vcsRevision() may add a revision when the commit is unknown; that is
			// environment dependent, so only assert the injected parts.
			if tc.commit == "unknown" {
				if !strings.HasPrefix(got, "dev") {
					t.Fatalf("versionLine() = %q, want it to start with %q", got, "dev")
				}
				return
			}
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("versionLine() = %q, want it to start with %q", got, tc.want)
			}
			if tc.commit != "" && strings.Count(got, tc.commit) > 1 {
				t.Fatalf("versionLine() = %q repeats the commit", got)
			}
		})
	}
}
