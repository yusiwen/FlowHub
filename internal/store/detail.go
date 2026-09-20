package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Detail writes one human readable block per delivery, rotating daily.
//
// It exists for exactly one job: letting a human read the real YouTrack payload
// as it arrived, without reaching for a JSON viewer. The machine readable truth
// stays in the JSONL audit log, which keeps the raw body in full.
type Detail struct {
	dir          string
	maxBodyBytes int
	now          func() time.Time

	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	day  string
	path string
}

const detailPrefix = "payload-"

// OpenDetail prepares dir for the human readable payload log. maxBodyBytes caps
// how much of a body is pretty printed per record.
func OpenDetail(dir string, maxBodyBytes int) (*Detail, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = 64 << 10
	}
	return &Detail{dir: dir, maxBodyBytes: maxBodyBytes, now: time.Now}, nil
}

// Record appends one delivery block.
func (d *Detail) Record(rec *Record) error {
	day := d.now().UTC().Format(time.DateOnly)

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.f == nil || d.day != day {
		if err := d.rotateLocked(day); err != nil {
			return err
		}
	}
	if _, err := d.w.WriteString(d.render(rec)); err != nil {
		return err
	}
	return d.w.Flush()
}

// Path returns the file currently being written to, or "" before the first
// record of the process.
func (d *Detail) Path() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.path
}

// Close flushes and closes the current file.
func (d *Detail) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closeLocked()
}

func (d *Detail) rotateLocked(day string) error {
	if err := d.closeLocked(); err != nil {
		return err
	}
	path := filepath.Join(d.dir, detailPrefix+day+".log")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open payload log %s: %w", path, err)
	}
	d.f = file
	d.w = bufio.NewWriter(file)
	d.day = day
	d.path = path
	return nil
}

func (d *Detail) closeLocked() error {
	var err error
	if d.w != nil {
		err = d.w.Flush()
		d.w = nil
	}
	if d.f != nil {
		if cerr := d.f.Close(); err == nil {
			err = cerr
		}
		d.f = nil
	}
	return err
}

// render formats one delivery. It is deliberately verbose: this file is read by
// a human comparing the real payload against the documented one.
func (d *Detail) render(rec *Record) string {
	var b strings.Builder

	verdict := "ACCEPTED"
	if !rec.Accepted {
		verdict = "REJECTED (" + orDash(rec.Reason) + ")"
	}

	separator := strings.Repeat("=", 100)
	fmt.Fprintf(&b, "%s\n", separator)
	fmt.Fprintf(&b, "%s  %-26s event=%s\n",
		rec.Time.Local().Format("2006-01-02 15:04:05.000"), verdict, orDash(rec.Event))
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 100))

	fmt.Fprintf(&b, "request   %s %s\n", rec.Method, orDash(rec.Path))
	fmt.Fprintf(&b, "source    remote=%s  x-forwarded-for=%s  x-real-ip=%s\n",
		orDash(rec.RemoteIP), orDash(rec.ForwardedFor), orDash(rec.RealIP))
	fmt.Fprintf(&b, "client    user-agent=%s  content-type=%s\n",
		orDash(rec.UserAgent), orDash(rec.ContentType))
	fmt.Fprintf(&b, "body      %d bytes  sha256=%s\n", rec.BodyBytes, orDash(rec.BodySHA256))
	fmt.Fprintf(&b, "locks     url_key=%s  header_token=%s  source_ip=%s  (configured: %s)\n",
		ok(rec.Locks.URLKey), ok(rec.Locks.HeaderToken), ok(rec.Locks.SourceIP), orNone(rec.ActiveLocks))

	verdictLine := fmt.Sprintf("handled=%dms", rec.HandledMS)
	if rec.Duplicate {
		verdictLine += "  duplicate=true"
	}
	if rec.DedupeKey != "" {
		verdictLine += "  dedupe=" + shorten(rec.DedupeKey, 16)
	}
	if rec.TimestampSkewMS != nil {
		verdictLine += fmt.Sprintf("  clock_skew=%dms", *rec.TimestampSkewMS)
	}
	fmt.Fprintf(&b, "verdict   %s\n", verdictLine)

	if rec.Event != "" || rec.IssueID != "" {
		fmt.Fprintf(&b, "issue     id=%s  form=%s  project=%s  summary=%s\n",
			orDash(rec.IssueID), orDash(rec.IssueIDForm), orDash(rec.ProjectKey), orDash(rec.Summary))
		fmt.Fprintf(&b, "actors    primary=%s  all=%s\n",
			orDash(rec.PrimaryActor), orNone(rec.ActorLogins))
		fmt.Fprintf(&b, "payload   timestamp=%s  numberInProject_present=%t  changedFields=%s  comments=%s\n",
			orDash(rec.PayloadTimestamp), rec.HasNumberInProject, orNone(rec.ChangedFields), orNone(rec.CommentIDs))
	}

	if len(rec.Headers) > 0 {
		fmt.Fprintf(&b, "headers   (%d, sensitive values masked)\n", len(rec.Headers))
		for _, name := range rec.HeaderNames {
			values, present := rec.Headers[name]
			if !present {
				continue
			}
			fmt.Fprintf(&b, "          %s: %s\n", displayHeaderName(name, rec.TokenHeader), strings.Join(values, ", "))
		}
	}
	if rec.TokenHeader != nil {
		fmt.Fprintf(&b, "token     %s\n", describeToken(rec.TokenHeader))
	}

	if len(rec.PayloadKeys) > 0 {
		fmt.Fprintf(&b, "keys      (%d)  %s\n", len(rec.PayloadKeys), strings.Join(rec.PayloadKeys, ", "))
	}
	if len(rec.PayloadSchema) > 0 {
		fmt.Fprintf(&b, "schema    (%d paths)\n", len(rec.PayloadSchema))
		for _, line := range rec.PayloadSchema {
			fmt.Fprintf(&b, "          %s\n", line)
		}
	}
	if rec.RawBody != "" {
		fmt.Fprintf(&b, "payload\n%s\n", d.pretty(rec.RawBody))
	}

	fmt.Fprintf(&b, "%s\n\n", separator)
	return b.String()
}

// displayHeaderName prefers the operator's spelling of the token header over the
// canonical form net/http stores ("X-Youtrack-Token"), because that is the name
// they configured in YouTrack and in nginx.
func displayHeaderName(name string, token *TokenHeaderInfo) string {
	if token != nil && token.Name != "" && strings.EqualFold(name, token.Name) {
		return token.Name
	}
	return name
}

func describeToken(info *TokenHeaderInfo) string {
	parts := []string{
		info.Name,
		fmt.Sprintf("present=%t", info.Present),
		fmt.Sprintf("len=%d", info.ValueLen),
	}
	if info.SHA256Prefix != "" {
		parts = append(parts, "sha256:"+info.SHA256Prefix)
	}
	parts = append(parts,
		fmt.Sprintf("matches_configured=%t", info.MatchesConfigured),
		fmt.Sprintf("literal_secret=%t", info.LooksLikeLiteralSecret))
	return strings.Join(parts, " ")
}

// pretty indents the body for reading, truncating very large bodies. The full
// body is always available in the JSONL audit record.
func (d *Detail) pretty(body string) string {
	truncated := false
	if len(body) > d.maxBodyBytes {
		body = body[:d.maxBodyBytes]
		truncated = true
	}

	var rendered string
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(body), "", "  "); err != nil {
		rendered = body + "\n(not valid JSON)"
	} else {
		rendered = out.String()
	}
	if truncated {
		rendered += fmt.Sprintf("\n(truncated at %d bytes; the full body is in the JSONL audit record)", d.maxBodyBytes)
	}

	// json.Indent does not indent its first line; indent the whole block so it
	// lines up with the rest of the record.
	return "  " + strings.ReplaceAll(rendered, "\n", "\n  ")
}

func ok(value bool) string {
	if value {
		return "ok"
	}
	return "-"
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func orNone(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

func shorten(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n] + "…"
}
