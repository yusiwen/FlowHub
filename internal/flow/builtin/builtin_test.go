package builtin

import (
	"os"
	"testing"
)

func TestBuiltinMatchesTheCanonicalFixture(t *testing.T) {
	canonical, err := os.ReadFile("../../../testdata/flows/default-youtrack.json")
	if err != nil {
		t.Fatalf("read canonical fixture: %v", err)
	}
	if string(canonical) != string(definition) {
		t.Fatal("internal/flow/builtin/default-youtrack.json has drifted from testdata/flows/default-youtrack.json")
	}
	compiled, raw, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if compiled.Definition.ID != "default-youtrack" || len(raw) == 0 {
		t.Fatalf("unexpected builtin: %+v", compiled.Definition)
	}
}
