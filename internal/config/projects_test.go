package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// The routing table lives outside the repository, in the XDG config directory.
// os.UserConfigDir is deliberately not used (it returns
// ~/Library/Application Support on macOS), so pin the XDG behaviour here.
func TestDefaultProjectsFileHonoursXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := DefaultProjectsFile(), filepath.Join(home, ".config", "flowhub", "config.json"); got != want {
		t.Fatalf("DefaultProjectsFile() = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "/xdg-config")
	if got, want := DefaultProjectsFile(), filepath.Join("/xdg-config", "flowhub", "config.json"); got != want {
		t.Fatalf("DefaultProjectsFile() = %q, want %q", got, want)
	}
}

func TestResolveProjectsFileExpandsHomeAndMakesAbsolute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got, want := ResolveProjectsFile("~/flowhub/config.json"), filepath.Join(home, "flowhub", "config.json"); got != want {
		t.Fatalf("ResolveProjectsFile(\"~/...\") = %q, want %q", got, want)
	}
	if got := ResolveProjectsFile("~"); got != home {
		t.Fatalf("ResolveProjectsFile(\"~\") = %q, want %q", got, home)
	}
	// Surrounding whitespace is tolerated: this value often comes from a shell
	// export copied out of documentation.
	if got, want := ResolveProjectsFile("  ~/x.json  "), filepath.Join(home, "x.json"); got != want {
		t.Fatalf("ResolveProjectsFile with whitespace = %q, want %q", got, want)
	}
	// A relative path becomes absolute so the logs name the file that was read.
	if got := ResolveProjectsFile("relative/config.json"); !filepath.IsAbs(got) {
		t.Fatalf("ResolveProjectsFile(relative) = %q, want an absolute path", got)
	}
	// An already absolute path is left alone.
	if got := ResolveProjectsFile("/already/absolute.json"); got != "/already/absolute.json" {
		t.Fatalf("ResolveProjectsFile(absolute) = %q", got)
	}
	// An empty value falls back to the default, not to the working directory.
	if got := ResolveProjectsFile(""); !strings.HasSuffix(got, filepath.Join("flowhub", "config.json")) {
		t.Fatalf("ResolveProjectsFile(\"\") = %q, want the default path", got)
	}
}

func TestLoadResolvesTheProjectsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("FLOWHUB_PROJECTS_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(home, ".config", "flowhub", "config.json")
	if cfg.ProjectsFile != want {
		t.Fatalf("ProjectsFile = %q, want %q", cfg.ProjectsFile, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "/xdg-config")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join("/xdg-config", "flowhub", "config.json"); cfg.ProjectsFile != want {
		t.Fatalf("ProjectsFile = %q, want %q", cfg.ProjectsFile, want)
	}

	// An explicit value wins, with "~" expanded.
	t.Setenv("FLOWHUB_PROJECTS_FILE", "~/custom.json")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, "custom.json"); cfg.ProjectsFile != want {
		t.Fatalf("ProjectsFile = %q, want %q", cfg.ProjectsFile, want)
	}
}
