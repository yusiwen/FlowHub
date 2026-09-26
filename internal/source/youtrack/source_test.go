package youtrack

import (
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/source"
)

func decode(t *testing.T, body string) *event.Event {
	t.Helper()
	decoded, err := New(rules.Policy{}).Parse(&source.Request{Body: []byte(body)})
	if err != nil {
		t.Fatalf("Parse(%s): %v", body, err)
	}
	if decoded.Event == nil {
		t.Fatal("Parse returned no event")
	}
	return decoded.Event
}

// TestParseBuildsTheNeutralEvent covers the mapping the dispatcher's decision
// actually consumes: the kind, the workflow state, the comment text, the subject
// and the attachments. The receiver's own tests assert the audit record, which
// never looks at these fields, so this is the only place they are pinned.
func TestParseBuildsTheNeutralEvent(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		kind    event.Kind
		comment string
		state   string
		field   string
	}{
		{
			name: "issue created",
			body: `{"event":"issueCreated","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"summary":"baseline probe","project":{"key":"TEST","name":"TEST","shortName":"TEST"},` +
				`"description":"probe body","reporter":{"login":"yusiwen","fullName":"Siwen Yu"}}`,
			kind: event.KindCreated,
		},
		{
			name: "comment added carries the newest comment",
			body: `{"event":"commentAdded","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"summary":"baseline probe","project":{"key":"TEST","shortName":"TEST"},` +
				`"comments":[{"id":"4-1","text":"first","author":{"login":"a"}},` +
				`{"id":"4-2","text":"/opencode start","author":{"login":"yusiwen"}}]}`,
			kind:    event.KindCommented,
			comment: "/opencode start",
		},
		{
			// The measured payload reports the same custom field twice, under its
			// internal and its localised name, with the same new value.
			name: "issue updated reports the state under both spellings",
			body: `{"event":"issueUpdated","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"summary":"baseline probe","project":{"key":"TEST","shortName":"TEST"},` +
				`"changedFields":[` +
				`{"name":"State","oldValue":{"name":"Open"},"value":{"name":"In Progress","presentation":"进行中"}},` +
				`{"name":"状态","oldValue":{"name":"Open"},"value":{"name":"In Progress","presentation":"进行中"}}]}`,
			kind:  event.KindStateSet,
			state: "In Progress",
			field: "State",
		},
		{
			name: "issue updated on a field that is not the state",
			body: `{"event":"issueUpdated","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"changedFields":[{"name":"description","oldValue":"a","value":"b"}]}`,
			kind: event.KindUpdated,
		},
		{
			// A state field whose value is not the enum shape: the polymorphic
			// value must not be mistaken for a state change.
			name: "issue updated with a non-enum state value",
			body: `{"event":"issueUpdated","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"changedFields":[{"name":"Assignee","oldValue":null,"value":{"login":"yusiwen"}},` +
				`{"name":"State","oldValue":null,"value":"In Progress"}]}`,
			kind: event.KindUpdated,
		},
		{
			name: "deleted issue",
			body: `{"event":"issueDeleted","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27"}`,
			kind: event.KindDeleted,
		},
		{
			// Known to the app, nothing for FlowHub to do: an honest "other" rather
			// than a kind the policy would act on.
			name: "work item added",
			body: `{"event":"workItemAdded","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"workItems":[{"duration":{"minutes":90}}]}`,
			kind: event.KindOther,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := decode(t, tt.body)
			if e.Kind != tt.kind {
				t.Errorf("Kind = %q, want %q", e.Kind, tt.kind)
			}
			if e.Comment != tt.comment {
				t.Errorf("Comment = %q, want %q", e.Comment, tt.comment)
			}
			if e.State != tt.state || e.StateField != tt.field {
				t.Errorf("state = (%q, %q), want (%q, %q)", e.State, e.StateField, tt.state, tt.field)
			}
			if e.Source != SourceName {
				t.Errorf("Source = %q, want %q", e.Source, SourceName)
			}
			if err := e.Validate(); err != nil {
				t.Errorf("the adapter produced an event the dispatcher rejects: %v", err)
			}
		})
	}
}

// TestParseCopiesTheSubjectAndTheActor keeps the prompt's inputs pinned: the
// dispatcher hands them straight to the adapter's prompt.
func TestParseCopiesTheSubjectAndTheActor(t *testing.T) {
	e := decode(t, `{"event":"issueCreated","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",`+
		`"summary":"baseline probe","project":{"key":"TEST","name":"TEST","shortName":"TEST"},`+
		`"description":"probe body","reporter":{"login":"yusiwen","fullName":"Siwen Yu"},`+
		`"attachments":[{"name":"log.txt","mimeType":"text/plain","size":42}]}`)

	if e.Subject.Key != "TEST-27" || e.Subject.Project != "TEST" {
		t.Errorf("subject key/project = %q/%q", e.Subject.Key, e.Subject.Project)
	}
	if e.Subject.Title != "baseline probe" || e.Subject.Body != "probe body" {
		t.Errorf("subject title/body = %q/%q", e.Subject.Title, e.Subject.Body)
	}
	if e.Actor != "yusiwen" {
		t.Errorf("actor = %q, want the reporter", e.Actor)
	}
	if len(e.Subject.Attachments) != 1 || e.Subject.Attachments[0].Name != "log.txt" {
		t.Errorf("attachments = %+v", e.Subject.Attachments)
	}
	if !e.Occurred.Equal(time.Date(2026, 9, 25, 10, 58, 42, 0, time.UTC)) {
		t.Errorf("occurred = %s", e.Occurred)
	}
	if len(e.Raw) == 0 {
		t.Error("the raw body must be kept for the audit")
	}
}

// TestDecodeFeedsTheTriggerPolicy is the seam test ADR 0001 step 1 exists for: a
// real payload, decoded by the adapter, has to produce the decision the workflow
// expects. The policy's own tests build events by hand, so only this test would
// catch an adapter that classified a payload differently from what it used to.
func TestDecodeFeedsTheTriggerPolicy(t *testing.T) {
	src := New(rules.Policy{})

	tests := []struct {
		name string
		body string
		want rules.Action
	}{
		{
			name: "a created issue is analysed read-only",
			body: `{"event":"issueCreated","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"summary":"s","project":{"key":"TEST","shortName":"TEST"},"reporter":{"login":"yusiwen"}}`,
			want: rules.ActionAnalyze,
		},
		{
			name: "the trigger comment starts the work",
			body: `{"event":"commentAdded","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"project":{"key":"TEST","shortName":"TEST"},` +
				`"comments":[{"id":"4-1","text":"/opencode start","author":{"login":"yusiwen"}}]}`,
			want: rules.ActionPlan,
		},
		{
			name: "a comment that merely mentions the trigger does nothing",
			body: `{"event":"commentAdded","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"project":{"key":"TEST","shortName":"TEST"},` +
				`"comments":[{"id":"4-1","text":"please run /opencode start later","author":{"login":"yusiwen"}}]}`,
			want: rules.ActionIgnore,
		},
		{
			name: "the state the maintainer moves the issue to starts the work",
			body: `{"event":"issueUpdated","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"project":{"key":"TEST","shortName":"TEST"},` +
				`"changedFields":[{"name":"State","oldValue":{"name":"Open"},` +
				`"value":{"name":"In Progress","presentation":"进行中"}}]}`,
			want: rules.ActionPlan,
		},
		{
			name: "a work item event is ignored",
			body: `{"event":"workItemAdded","timestamp":"2026-09-25T10:58:42.000Z","id":"TEST-27",` +
				`"project":{"key":"TEST","shortName":"TEST"}}`,
			want: rules.ActionIgnore,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := decode(t, tt.body)
			decision := src.Policy().Decide(*e, rules.TaskView{Known: true, Plan: registry.PlanNone})
			if decision.Action != tt.want {
				t.Fatalf("Decide(%s/%s) = %s (%s), want %s",
					e.Kind, e.State, decision.Action, decision.Reason, tt.want)
			}
			// The sign-off has to name the real trigger, so Basis has to work for
			// every decision the adapter can produce.
			if basis := src.Policy().Basis(decision, *e); basis == "" {
				t.Error("the decision has no basis for the reply sign-off")
			}
		})
	}
}
