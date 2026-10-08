package youtrack

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/source"
)

// SourceName is what the adapter reports as its name, in the routing table, in the
// audit record and in the log.
const SourceName = "youtrack"

// Source is the YouTrack adapter. Its state is the trigger policy — which the
// environment may override and which the prompt has to name exactly (a prompt that
// tells the maintainer to type a phrase the trigger does not recognise is worse
// than no prompt at all) — and the source's own `sources.youtrack.prompt_file`,
// read when the configuration was loaded.
type Source struct {
	policy rules.Policy
	// instructions is the source-level prompt_file's content. It applies to every
	// turn this source produces, which is the point of declaring it at the source
	// level rather than per project; a per-project file arrives per turn instead
	// (rules.PromptContext.Instructions).
	instructions string
}

// New builds the adapter. A zero policy means "the measured defaults".
//
// instructions is the source's own `prompt_file`, already read and validated by the
// configuration loader; an empty string means no file was declared.
func New(policy rules.Policy, instructions string) *Source {
	if strings.TrimSpace(policy.Trigger) == "" {
		policy = rules.Policy{}
	}
	return &Source{policy: policy.Defaults(), instructions: strings.TrimSpace(instructions)}
}

// Name implements source.Source.
func (s *Source) Name() string { return SourceName }

// Policy implements source.Source. The trigger phrase, the states that mean "start
// implementing", the markers that identify FlowHub's own replies and the turn
// budget default to the measured values from the design documents.
func (s *Source) Policy() rules.Policy { return s.policy }

// Tools implements source.Source.
func (s *Source) Tools() source.ToolPolicy {
	return source.ToolPolicy{
		Allowed: AllowedMCPTools,
		Reply:   ReplyTools,
		Download: source.DownloadPolicy{
			Hosts:  DownloadHosts,
			Prefix: DownloadPrefix,
		},
	}
}

// Parse implements source.Source: it decodes one delivery body into the neutral
// event plus the facts the audit record keeps.
//
// The three entry locks are the receiver's; there is no signature to verify here,
// because the published app signs nothing (§4.2).
func (s *Source) Parse(req *source.Request) (source.Decoded, error) {
	payload, keys, err := Parse(req.Body)
	if err != nil {
		return source.Decoded{}, err
	}

	occurred, timestampErr := ParseTimestamp(payload.Timestamp)
	occurredOK := timestampErr == nil
	decoded := source.Decoded{
		Name:                   payload.Event,
		Known:                  KnownEvent(payload.Event),
		Occurred:               occurred,
		TimestampRaw:           payload.Timestamp,
		ActorLogins:            payload.ActorLogins(),
		IssueIDForm:            IssueIDForm(payload.ID),
		NumberInProjectPresent: payload.NumberInProject != nil,
		ChangedFields:          payload.ChangedFieldNames(),
		CommentIDs:             payload.CommentIDs(),
		PayloadKeys:            keys,
	}
	decoded.PayloadSchema, _ = Schema(req.Body)

	decoded.Event = s.event(payload, req.Body, occurred, occurredOK)
	return decoded, nil
}

// event converts the payload into the neutral event.
//
// The state normalisation lives here, not in the policy: the adapter knows which
// field carries the workflow state (the measured payload repeats it under its
// internal and its localised name), and the policy only lists the *values* that
// mean "start implementing".
func (s *Source) event(payload *Payload, body []byte, occurred time.Time, occurredOK bool) *event.Event {
	e := &event.Event{
		Source: SourceName,
		Subject: event.Subject{
			Project: payload.ProjectKey(),
			Key:     payload.ID,
			Title:   payload.Summary,
			Body:    payload.Description,
		},
		Actor:    payload.PrimaryActor(),
		Occurred: occurred,
		Raw:      body,
	}
	if !occurredOK {
		e.Occurred = time.Time{}
	}
	// The payload carries no download URL: the signed URL lives on the live issue
	// and the agent fetches it through the MCP tool. The metadata is still worth
	// carrying, because the prompt tells the agent which files exist.
	for _, attachment := range payload.Attachments {
		e.Subject.Attachments = append(e.Subject.Attachments, event.Attachment{
			Name:  attachment.Name,
			MIME:  attachment.MimeType,
			Bytes: attachment.Size,
		})
	}

	switch payload.Event {
	case "issueCreated":
		e.Kind = event.KindCreated
	case "issueDeleted":
		e.Kind = event.KindDeleted
	case "commentAdded", "commentUpdated":
		e.Kind = event.KindCommented
		if len(payload.Comments) > 0 {
			// The newest comment is the one that triggered a comment event.
			e.Comment = payload.Comments[len(payload.Comments)-1].Text
		}
	case "issueUpdated":
		if value, field, ok := stateChange(payload); ok {
			e.Kind, e.State, e.StateField = event.KindStateSet, value, field
		} else {
			e.Kind = event.KindUpdated
		}
	default:
		// commentDeleted, the work-item and attachment events: known to the app,
		// nothing for FlowHub to do. Saying `other` is honest; the policy ignores it.
		e.Kind = event.KindOther
	}
	return e
}

// stateChange looks for a workflow-state change and returns its new value.
//
// The value is polymorphic: only the enum shape carries a name. A field name that
// is not one of the measured state fields is not a state change, whatever its value
// looks like.
//
// The canonical spellings are consulted in order and, within one spelling, the last
// enum-valued occurrence wins. That is exactly how the trigger policy resolved a
// state before ADR 0001 moved the normalisation into the adapter, so a payload that
// repeats the field (the measured one repeats it under its internal and its
// localised name) decides the same way it always did.
func stateChange(payload *Payload) (value, field string, ok bool) {
	for _, name := range stateFields {
		value, found := "", false
		for _, changed := range payload.ChangedFields {
			if changed.Name != name {
				continue
			}
			var enum struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(changed.Value, &enum); err == nil && enum.Name != "" {
				value, found = enum.Name, true
			}
		}
		if found {
			return value, name, true
		}
	}
	return "", "", false
}

// stateFields are the changedFields names that carry the workflow state. The
// measured payload repeats a custom field under its internal and its localised
// name, so both spellings have to be listed. The match is exact, as it was in the
// policy: a differently cased name was never a state change.
var stateFields = []string{"State", "状态"}
