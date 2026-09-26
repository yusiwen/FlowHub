package youtrack

import (
	"strings"
	"testing"
)

func TestSchemaDescribesEveryPathAndType(t *testing.T) {
	body := []byte(`{
		"event": "issueUpdated",
		"timestamp": "2026-09-20T01:02:03.000Z",
		"id": "2-123",
		"summary": "Fix login",
		"project": {"key": "SP", "name": "Sample", "shortName": "SP"},
		"updated": 1732708800000,
		"updatedBy": {"login": "jane.doe", "fullName": "Jane Doe", "email": "jane@example.com"},
		"changedFields": [
			{"name": "State", "oldValue": {"name": "Open", "presentation": "Open"}, "value": {"name": "In Progress", "presentation": "In Progress"}},
			{"name": "Assignee", "oldValue": null, "value": {"login": "john.doe", "fullName": "John Doe"}}
		],
		"tags": ["a", "b"],
		"empty": [],
		"nothing": null
	}`)

	paths, err := Schema(body)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	got := strings.Join(paths, "\n")

	for _, want := range []string{
		"changedFields: array",
		"changedFields[].name: string",
		"changedFields[].oldValue: object",
		"changedFields[].oldValue.name: string",
		"changedFields[].value.login: string",
		"empty: array",
		"event: string",
		"id: string",
		"nothing: null",
		"project: object",
		"project.key: string",
		"project.shortName: string",
		"tags: array",
		"tags[]: string",
		"updated: number",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("schema is missing %q:\n%s", want, got)
		}
	}

	// Array elements are merged, so a key present on only one element still shows
	// up exactly once.
	if strings.Count(got, "changedFields[].oldValue: object") != 1 {
		t.Fatalf("array paths must be merged:\n%s", got)
	}
	if strings.Contains(got, "changedFields[].value.name.presentation") {
		t.Fatalf("nested paths were flattened incorrectly:\n%s", got)
	}
}

func TestSchemaIsSortedAndStable(t *testing.T) {
	first, err := Schema([]byte(`{"b":1,"a":{"z":true,"y":"s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Schema([]byte(`{"a":{"y":"s","z":true},"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(first, "|") != strings.Join(second, "|") {
		t.Fatalf("schema is not stable:\n%v\n%v", first, second)
	}
	if first[0] != "a: object" {
		t.Fatalf("schema is not sorted: %v", first)
	}
}

func TestSchemaRejectsNonJSON(t *testing.T) {
	if _, err := Schema([]byte("{oops")); err == nil {
		t.Fatal("want an error for malformed JSON")
	}
}
