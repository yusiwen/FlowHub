package provision

import (
	"strings"
	"testing"
)

// assertChangeOrder checks the invariant every diff reader relies on: inside one
// block of changes, removals come before additions.
func assertChangeOrder(t *testing.T, diff string) {
	t.Helper()
	sawAddition := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@"):
			sawAddition = false
		case strings.HasPrefix(line, "+"):
			sawAddition = true
		case strings.HasPrefix(line, "-"):
			if sawAddition {
				t.Fatalf("a removal is printed after an addition:\n%s", diff)
			}
		default:
			// A context line ends the change block.
			sawAddition = false
		}
	}
}

func TestUnifiedDiffShowsBothSides(t *testing.T) {
	oldText := "one\ntwo\nthree\nfour\n"
	newText := "one\nTWO\nthree\nfour\n"

	diff := unifiedDiff(oldText, newText, 1)
	for _, want := range []string{"@@ ", "-two", "+TWO", " one", " three"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff is missing %q:\n%s", want, diff)
		}
	}
	assertChangeOrder(t, diff)
}

func TestUnifiedDiffHandlesInsertionAndDeletion(t *testing.T) {
	insert := unifiedDiff("a\nb\n", "a\nnew\nb\n", 1)
	if !strings.Contains(insert, "+new") {
		t.Fatalf("insertion not shown:\n%s", insert)
	}
	assertChangeOrder(t, insert)

	remove := unifiedDiff("a\ngone\nb\n", "a\nb\n", 1)
	if !strings.Contains(remove, "-gone") {
		t.Fatalf("deletion not shown:\n%s", remove)
	}
	assertChangeOrder(t, remove)
}

func TestUnifiedDiffOfIdenticalTextIsEmpty(t *testing.T) {
	if got := unifiedDiff("same\n", "same\n", 3); got != "" {
		t.Fatalf("identical text produced a diff:\n%s", got)
	}
}

// TestUnifiedDiffIsBounded keeps a replaced file with thousands of lines from
// producing a wall of output in a refusal message.
func TestUnifiedDiffIsBounded(t *testing.T) {
	var oldBuilder, newBuilder strings.Builder
	for i := 0; i < diffLineLimit+50; i++ {
		oldBuilder.WriteString("old line\n")
		newBuilder.WriteString("new line\n")
	}
	bounded := unifiedDiff(oldBuilder.String(), newBuilder.String(), 2)
	if !strings.Contains(bounded, "too different in size") {
		t.Fatalf("a very large diff was not summarised:\n%.200s", bounded)
	}

	// Under the line limit but over the output limit: truncated, not unbounded.
	var text strings.Builder
	for i := 0; i < diffLineLimit; i++ {
		text.WriteString("line\n")
	}
	changed := strings.Replace(text.String(), "line\n", "LINE\n", 1)
	diff := unifiedDiff(text.String(), changed, 1)
	if strings.Count(diff, "\n") > maxDiffLines+8 {
		t.Fatalf("the diff was not truncated: %d lines", strings.Count(diff, "\n"))
	}
}
