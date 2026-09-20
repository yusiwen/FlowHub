// Package webhook implements the public YouTrack webhook entry point.
//
// It is deliberately a pure receiver: it authenticates, validates, deduplicates
// and persists. It never calls YouTrack, opencode or any other network service
// on the request path, because the published Webhook Triggers app delivers
// synchronously with a 5s timeout and no retry.
package webhook

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Known events emitted by the published Webhook Triggers app (v1.0.5). The
// catch-all rule uses allEvents internally but sends the concrete event name.
var knownEvents = map[string]struct{}{
	"issueCreated":           {},
	"issueUpdated":           {},
	"issueDeleted":           {},
	"commentAdded":           {},
	"commentUpdated":         {},
	"commentDeleted":         {},
	"workItemAdded":          {},
	"workItemUpdated":        {},
	"workItemDeleted":        {},
	"issueAttachmentAdded":   {},
	"issueAttachmentDeleted": {},
}

// KnownEvent reports whether name is one of the eleven events the app can send.
func KnownEvent(name string) bool {
	_, ok := knownEvents[name]
	return ok
}

// KnownEventNames returns the known event names, sorted. Used by the CLI help.
func KnownEventNames() []string {
	out := make([]string, 0, len(knownEvents))
	for name := range knownEvents {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// User is the subset of a YouTrack user object the app actually sends
// ({login, fullName, email} — the documented id is not on the wire).
type User struct {
	Login    string `json:"login"`
	FullName string `json:"fullName"`
	Email    string `json:"email"`
}

// Project is the project object. The released app sends key/name/shortName.
type Project struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	ShortName string `json:"shortName"`
}

// ChangedField is one entry of issueUpdated.changedFields. Field values are
// polymorphic (string, number, bool, {name,presentation}, user object, array),
// so both sides stay raw and are never assumed to be strings.
type ChangedField struct {
	Name     string          `json:"name"`
	OldValue json.RawMessage `json:"oldValue"`
	Value    json.RawMessage `json:"value"`
}

// Comment is one entry of comments[].
type Comment struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	TextPreview string `json:"textPreview"`
	Created     int64  `json:"created"`
	Updated     *int64 `json:"updated"`
	Author      *User  `json:"author"`
}

// TypeRef is a work item type reference.
type TypeRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// WorkItem is one entry of workItems[].
type WorkItem struct {
	ID          string   `json:"id"`
	Date        int64    `json:"date"`
	Duration    any      `json:"duration"`
	Description string   `json:"description"`
	Created     int64    `json:"created"`
	Updated     *int64   `json:"updated"`
	Author      *User    `json:"author"`
	Type        *TypeRef `json:"type"`
}

// Attachment is one entry of attachments[].
type Attachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
	Created  int64  `json:"created"`
	Author   *User  `json:"author"`
}

// Payload is the union of every field the app can send. Unknown fields are
// ignored on purpose: the released app, its main branch and the official
// documentation disagree about this payload, so nothing is required except
// `event`.
type Payload struct {
	Event         string         `json:"event"`
	Timestamp     string         `json:"timestamp"`
	ID            string         `json:"id"`
	Summary       string         `json:"summary"`
	Project       *Project       `json:"project"`
	Description   string         `json:"description"`
	Created       int64          `json:"created"`
	Updated       int64          `json:"updated"`
	Reporter      *User          `json:"reporter"`
	UpdatedBy     *User          `json:"updatedBy"`
	ChangedFields []ChangedField `json:"changedFields"`
	Comments      []Comment      `json:"comments"`
	WorkItems     []WorkItem     `json:"workItems"`
	Attachments   []Attachment   `json:"attachments"`

	// NumberInProject is documented but reported absent from the released app.
	// Keeping the field lets the first real delivery answer that question.
	NumberInProject *int `json:"numberInProject"`
}

// Parse decodes a webhook body leniently and also returns the sorted top-level
// key set, which is what the first-run payload probe reports.
func Parse(body []byte) (*Payload, []string, error) {
	var payload Payload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return &payload, keys, nil
}

// ProjectKey returns the best available project identifier.
func (p *Payload) ProjectKey() string {
	if p.Project == nil {
		return ""
	}
	if p.Project.Key != "" {
		return p.Project.Key
	}
	return p.Project.ShortName
}

// PrimaryActor returns the login most relevant to this event, with no
// authorization meaning: phase 1 only records it for observation.
func (p *Payload) PrimaryActor() string {
	if p.UpdatedBy != nil && p.UpdatedBy.Login != "" {
		return p.UpdatedBy.Login
	}
	if p.Reporter != nil && p.Reporter.Login != "" {
		return p.Reporter.Login
	}
	if logins := p.ActorLogins(); len(logins) > 0 {
		return logins[0]
	}
	return ""
}

// ActorLogins returns every login mentioned by the payload, deduplicated and
// sorted.
func (p *Payload) ActorLogins() []string {
	seen := map[string]struct{}{}
	add := func(u *User) {
		if u != nil && u.Login != "" {
			seen[u.Login] = struct{}{}
		}
	}
	add(p.Reporter)
	add(p.UpdatedBy)
	for i := range p.Comments {
		add(p.Comments[i].Author)
	}
	for i := range p.WorkItems {
		add(p.WorkItems[i].Author)
	}
	for i := range p.Attachments {
		add(p.Attachments[i].Author)
	}

	out := make([]string, 0, len(seen))
	for login := range seen {
		out = append(out, login)
	}
	sort.Strings(out)
	return out
}

// ChangedFieldNames lists the changed custom field names of issueUpdated.
func (p *Payload) ChangedFieldNames() []string {
	out := make([]string, 0, len(p.ChangedFields))
	for _, f := range p.ChangedFields {
		if f.Name != "" {
			out = append(out, f.Name)
		}
	}
	return out
}

// CommentIDs lists the comment ids carried by comment* events.
func (p *Payload) CommentIDs() []string {
	out := make([]string, 0, len(p.Comments))
	for _, c := range p.Comments {
		if c.ID != "" {
			out = append(out, c.ID)
		}
	}
	return out
}

var (
	databaseIDPattern = regexp.MustCompile(`^\d+-\d+$`)
	readableIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-\d+$`)
)

// IssueIDForm classifies a payload issue id. The workflow API is documented to
// send the database form ("2-123"); whether it also sends the readable form
// ("SP-123") is an open question in the design documents, and recording the form
// answers it without any extra request.
func IssueIDForm(id string) string {
	switch {
	case id == "":
		return ""
	case databaseIDPattern.MatchString(id):
		return "database"
	case readableIDPattern.MatchString(id):
		return "readable"
	default:
		return "unknown"
	}
}

// ParseTimestamp accepts the RFC3339 timestamp the app sends and, defensively,
// epoch milliseconds.
func ParseTimestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts, nil
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, nil
	}
	if isDigits(raw) {
		millis, err := parseInt64(raw)
		if err == nil {
			return time.UnixMilli(millis).UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", raw)
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func parseInt64(s string) (int64, error) {
	var out int64
	for _, r := range s {
		out = out*10 + int64(r-'0')
	}
	return out, nil
}
