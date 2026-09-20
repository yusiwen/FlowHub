package config

import (
	"net"
	"strings"
	"testing"
)

// The kernel treats ":8080", "0.0.0.0:8080" and "[::]:8080" as the same wildcard
// bind, so the warning must fire for all three spellings. Missing ":8080" would
// silently expose the receiver on every interface.
func TestListensOnAllInterfacesDetectsEveryWildcardSpelling(t *testing.T) {
	cases := map[string]bool{
		"0.0.0.0:8080":   true,
		":8080":          true,
		"[::]:8080":      true,
		"[::0]:8080":     true,
		"127.0.0.1:8080": false,
		"10.8.0.5:8080":  false,
		"[fd00::1]:8080": false,
		"localhost:8080": false,
	}

	for addr, want := range cases {
		cfg := Config{Addr: addr}
		if got := cfg.ListensOnAllInterfaces(); got != want {
			t.Errorf("ListensOnAllInterfaces(%q) = %t, want %t", addr, got, want)
		}

		// Unacknowledged: a wildcard is a startup problem, other addresses are not.
		if got := len(cfg.Problems()); (got == 1) != want {
			t.Errorf("Problems(%q) = %d entries, want a problem only for wildcards", addr, got)
		}
		// Acknowledged: allowed, but never silent.
		cfg.AllowWildcardListen = true
		if problems := cfg.Problems(); len(problems) != 0 {
			t.Errorf("acknowledged %q still refused: %v", addr, problems)
		}
		warned := false
		for _, warning := range cfg.Warnings() {
			if strings.Contains(warning, "listens on every interface") {
				warned = true
			}
		}
		if warned != want {
			t.Errorf("acknowledged Warnings(%q) exposure warning = %t, want %t", addr, warned, want)
		}
	}
}

func TestWildcardWarningNamesTheOffendingAddress(t *testing.T) {
	cfg := Config{Addr: ":8080", AllowWildcardListen: true}
	warnings := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(warnings, "FLOWHUB_ADDR :8080 listens on every interface") {
		t.Fatalf("warning does not name the address:\n%s", warnings)
	}
}

// A wildcard bind is a startup error unless it is explicitly acknowledged, so
// that a container-only requirement can never leak into a host deployment by
// accident. `-print-config` must still be able to show the settings.
func TestWildcardBindIsRefusedWithoutAcknowledgement(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		cfg := Config{Addr: addr}
		problems := cfg.Problems()
		if len(problems) != 1 {
			t.Fatalf("Problems(%q) = %v, want exactly one", addr, problems)
		}
		if !strings.Contains(problems[0], "FLOWHUB_ALLOW_WILDCARD_LISTEN=1") {
			t.Fatalf("problem does not say how to opt in: %s", problems[0])
		}
		if strings.Contains(problems[0], "listens on every interface") == false {
			t.Fatalf("problem does not explain itself: %s", problems[0])
		}

		// The refusal must not double up as a warning.
		for _, warning := range cfg.Warnings() {
			if strings.Contains(warning, "listens on every interface") {
				t.Fatalf("Warnings(%q) still warns while refusing to start: %s", addr, warning)
			}
		}

		// -print-config renders problems rather than failing.
		report := cfg.Report()
		if !strings.Contains(report, "problem:") {
			t.Fatalf("Report(%q) hides the problem:\n%s", addr, report)
		}
		if !strings.Contains(report, "allow_wildcard_listen: false") {
			t.Fatalf("Report(%q) hides the switch:\n%s", addr, report)
		}
	}
}

func TestAcknowledgedWildcardIsAllowedAndStillWarns(t *testing.T) {
	cfg := Config{Addr: ":8080", AllowWildcardListen: true}
	if problems := cfg.Problems(); len(problems) != 0 {
		t.Fatalf("acknowledged wildcard still refused: %v", problems)
	}

	warnings := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(warnings, "FLOWHUB_ALLOW_WILDCARD_LISTEN=1") {
		t.Fatalf("acknowledged wildcard is silent:\n%s", warnings)
	}
	// No source allowlist plus a wildcard bind gets its own nudge.
	if !strings.Contains(warnings, "FLOWHUB_ALLOWED_SOURCES is unset") {
		t.Fatalf("missing the source-allowlist warning:\n%s", warnings)
	}

	cfg.AllowedSources = []*net.IPNet{{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(32, 32)}}
	if strings.Contains(strings.Join(cfg.Warnings(), "\n"), "any source that can reach the port") {
		t.Fatal("source-allowlist warning must disappear once the allowlist is set")
	}
}

func TestSpecificAddressNeedsNoAcknowledgement(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "10.8.0.5:8080", "192.168.8.135:8080"} {
		cfg := Config{Addr: addr}
		if problems := cfg.Problems(); len(problems) != 0 {
			t.Fatalf("Problems(%q) = %v, want none", addr, problems)
		}
	}
}

func TestLoadReadsTheWildcardAcknowledgement(t *testing.T) {
	t.Setenv("FLOWHUB_ADDR", "0.0.0.0:8080")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AllowWildcardListen {
		t.Fatal("wildcard must not be acknowledged by default")
	}
	if len(cfg.Problems()) != 1 {
		t.Fatalf("expected a startup problem, got %v", cfg.Problems())
	}

	t.Setenv("FLOWHUB_ALLOW_WILDCARD_LISTEN", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AllowWildcardListen {
		t.Fatal("FLOWHUB_ALLOW_WILDCARD_LISTEN was ignored")
	}
	if len(cfg.Problems()) != 0 {
		t.Fatalf("acknowledged wildcard still refused: %v", cfg.Problems())
	}
}
