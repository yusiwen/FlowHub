package youtrack

import "testing"

// FuzzParseAndSchema drives the untrusted-input boundary: the webhook body comes
// from the network and is parsed leniently on purpose, so the only acceptable
// outcomes are "decoded" or "rejected", never a panic, and never a payload that
// Parse accepts but Schema cannot describe.
func FuzzParseAndSchema(f *testing.F) {
	seeds := []string{
		`{}`,
		`{"event":"issueUpdated","timestamp":"2026-09-20T01:02:03.000Z","id":"2-123"}`,
		`{"event":"commentAdded","comments":[{"id":"4-1","text":"/opencode go","author":{"login":"a"}}]}`,
		`{"event":"issueCreated","project":{"key":"SP"},"numberInProject":7}`,
		`{"event":"issueUpdated","changedFields":[{"name":"State","oldValue":null,"value":{"name":"Done"}}]}`,
		`{"event":123}`,
		`{"event":"workItemAdded","workItems":[{"duration":{"minutes":90}}]}`,
		`{"event":"issueUpdated","timestamp":"1732708800000","id":"SP-1"}`,
		`[]`,
		`null`,
		`"scalar"`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		payload, keys, err := Parse(body)
		if err != nil {
			return // rejecting malformed JSON is the expected path
		}
		if payload == nil {
			t.Fatal("Parse returned a nil payload without an error")
		}
		if keys == nil {
			t.Fatal("Parse returned nil keys without an error")
		}

		// None of the derived accessors may panic on hostile input.
		_ = KnownEvent(payload.Event)
		_ = payload.ProjectKey()
		_ = payload.PrimaryActor()
		_ = payload.ActorLogins()
		_ = payload.ChangedFieldNames()
		_ = payload.CommentIDs()
		_ = IssueIDForm(payload.ID)
		if payload.Timestamp != "" {
			_, _ = ParseTimestamp(payload.Timestamp)
		}

		// A body Parse accepted must always be describable.
		if _, err := Schema(body); err != nil {
			t.Fatalf("Schema rejected a body Parse accepted: %v", err)
		}

		// Redaction is the receiver's, not the adapter's: it is asserted in the
		// webhook package, where the route shape it depends on lives.
	})
}
