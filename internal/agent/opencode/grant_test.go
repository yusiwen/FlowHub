package opencode

import "testing"

// grantedArbiter is the read-only policy of a turn a human has answered: the listed
// commands are the exact strings the previous turn was refused for.
func grantedArbiter(commands ...string) *Arbiter {
	arbiter := DefaultArbiter()
	arbiter.Granted = commands
	return arbiter
}

// TestAGrantedCommandIsJudgedByTheDenyListAlone is the point of the feature: a command
// the allowlist does not recognise — the shape the agent was refused for — runs because
// a human authorised that exact string.
func TestAGrantedCommandIsJudgedByTheDenyListAlone(t *testing.T) {
	for _, command := range []string{
		"mvn -q verify",
		"git stash pop",
		"make integration-test",
		"gotcha --now",
	} {
		if decision := DefaultArbiter().Decide(bashRequest(command)); decision.Allowed() {
			t.Fatalf("the control failed: %q is allowed without a grant", command)
		}
		decision := grantedArbiter(command).Decide(bashRequest(command))
		if !decision.Allowed() {
			t.Errorf("a granted %q was refused: %s", command, decision.Reason)
		}
	}
}

// TestAGrantIsLiteral is the property that keeps the authorisation as small as the
// human made it: the string they read in the comment, and nothing that merely resembles
// it.
func TestAGrantIsLiteral(t *testing.T) {
	arbiter := grantedArbiter("mvn -q verify")
	for _, command := range []string{
		"mvn -q verify -DskipTests", // an extra flag is a different command
		"mvn  -q verify",            // different whitespace is a different string
		"mvn verify",
		"mvn -q verify --also-make",
	} {
		if decision := arbiter.Decide(bashRequest(command)); decision.Allowed() {
			t.Errorf("a grant of %q also allowed %q", "mvn -q verify", command)
		}
	}
	// The authorised string itself, and only it, is allowed.
	if decision := arbiter.Decide(bashRequest("mvn -q verify")); !decision.Allowed() {
		t.Fatalf("the granted command was refused: %s", decision.Reason)
	}
}

// TestAGrantNeverBeatsTheDenyListTheWorktreeOrThePhase is the safety half, and each
// case is a separate reason not to read `Granted` as "this command is safe".
func TestAGrantNeverBeatsTheDenyListTheWorktreeOrThePhase(t *testing.T) {
	dangerous := map[string]string{
		"rm -rf /":               "deny list",
		"git push origin master": "deny list",
		"sudo cat /etc/shadow":   "deny list",
		// git remote is denied on purpose — a remote URL can embed credentials — so a
		// human authorising it changes nothing, which is the property under test.
		"git remote -v":            "deny list",
		"cat /etc/passwd":          "outside the task worktree",
		"ls > /tmp/out":            "redirection",
		"curl https://example.com": "curl may only reach",
		"python3 -c 'print(1)'":    "deny list",
		"sh -c 'rm -rf /'":         "deny list",
	}
	for command, want := range dangerous {
		decision := grantedArbiter(command).Decide(bashRequest(command))
		if decision.Allowed() {
			t.Errorf("a grant of %q allowed it, want refused", command)
			continue
		}
		if want != "" && !contains(decision.Reason, want) {
			t.Errorf("a granted %q was refused with %q, want it to mention %q", command, decision.Reason, want)
		}
	}

	// The phase rule is not a permission. `gofmt -w` is on the execution allowlist and
	// on no deny list, so in a read-only turn the phase rule is the *only* thing that
	// refuses it — which makes it the case that proves a grant cannot turn a read-only
	// turn into a writable one. (`git add -A` is refused earlier, by the deny list, so
	// it cannot show this on its own.)
	for _, command := range []string{"gofmt -w .", "go mod tidy"} {
		if decision := DefaultArbiter().Decide(bashRequest(command)); decision.Allowed() {
			t.Fatalf("the control failed: %q is allowed in a read-only turn", command)
		}
		if decision := grantedArbiter(command).Decide(bashRequest(command)); decision.Allowed() {
			t.Errorf("a grant turned a read-only turn into a writable one for %q", command)
		}
		execution := grantedArbiter(command)
		execution.Phase = PhaseExecution
		if decision := execution.Decide(bashRequest(command)); !decision.Allowed() {
			t.Errorf("the execution phase refused a granted %q: %s", command, decision.Reason)
		}
	}
	// And the deny list still wins over a grant in the execution phase too.
	staged := grantedArbiter("git push origin master")
	staged.Phase = PhaseExecution
	if decision := staged.Decide(bashRequest("git push origin master")); decision.Allowed() {
		t.Fatal("a granted push was allowed in the execution phase")
	}
}

// TestAGrantFollowsTheWrapperItWasRecordedWith: a human authorises the string they read
// in the agent's comment, and on a host whose plugin rewrote the command that string is
// the rewritten one — while the policy judges what follows the wrapper. Both spellings
// therefore have to match, or the fix for one defect would break the other.
func TestAGrantFollowsTheWrapperItWasRecordedWith(t *testing.T) {
	rewritten := grantedArbiter("rtk git stash pop")
	rewritten.Wrappers = []string{"rtk"}
	if decision := rewritten.Decide(bashRequest("rtk git stash pop")); !decision.Allowed() {
		t.Fatalf("the granted rewritten command was refused: %s", decision.Reason)
	}

	// The same grant recorded without the wrapper still matches the target.
	plain := grantedArbiter("git stash pop")
	plain.Wrappers = []string{"rtk"}
	if decision := plain.Decide(bashRequest("rtk git stash pop")); !decision.Allowed() {
		t.Fatalf("a grant of the unwrapped command did not match the wrapped one: %s", decision.Reason)
	}
}

// TestAGrantedCommandAgreesWithTheRequestsOwnPatterns: the cross-check must accept a
// granted command too, or every granted compound line would be refused by its own
// patterns — a permission that the auditor half of the policy does not believe in.
func TestAGrantedCommandAgreesWithTheRequestsOwnPatterns(t *testing.T) {
	arbiter := grantedArbiter("mvn -q verify")
	decision := arbiter.Decide(bashRequest("mvn -q verify && ls", "mvn -q verify", "ls"))
	if !decision.Allowed() {
		t.Fatalf("a granted command inside a compound line was refused: %s", decision.Reason)
	}
	// The pattern cross-check still refuses what the command path would.
	disagreement := arbiter.Decide(bashRequest("mvn -q verify", "mvn -q verify", "rm -rf /"))
	if disagreement.Allowed() {
		t.Fatal("a granted command smuggled a denied pattern through")
	}
}

// TestAnEmptyGrantAuthorisesNothing: a blank entry must never become "everything is
// authorised", which is the failure mode a naive substring check would have. The
// control is the same command with no grant at all.
func TestAnEmptyGrantAuthorisesNothing(t *testing.T) {
	arbiter := grantedArbiter("", "   ")
	for _, command := range []string{"mvn -q verify", "git stash pop"} {
		if decision := arbiter.Decide(bashRequest(command)); decision.Allowed() {
			t.Errorf("an empty grant allowed %q", command)
		}
		if decision := DefaultArbiter().Decide(bashRequest(command)); decision.Allowed() {
			t.Errorf("%q is allowed with no grant at all", command)
		}
	}
}
