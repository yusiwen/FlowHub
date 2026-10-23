package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/upstream"
)

type cursorState struct {
	EventID   string    `json:"event_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (e *Engine) cursorPath() string {
	return filepath.Join(e.opts.DataDir, "flow-cursor.json")
}

// loadCursor reads the event id processed by the previous process. A missing file
// is the normal first start; a malformed file is an error, because losing the
// cursor would replay surprising history.
func (e *Engine) loadCursor() error {
	path := e.cursorPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("engine: read cursor %s: %w", path, err)
	}
	var state cursorState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("engine: parse cursor %s: %w", path, err)
	}
	e.cursor = strings.TrimSpace(state.EventID)
	return nil
}

// Cursor returns the last event id the flow worker accepted for processing.
func (e *Engine) Cursor() string {
	if e == nil {
		return ""
	}
	e.cursorMu.Lock()
	defer e.cursorMu.Unlock()
	return e.cursor
}

// SetCursor moves the cursor forward (or initializes it at the current tail).
// It never moves it backwards.
func (e *Engine) SetCursor(eventID string) error {
	if e == nil {
		return nil
	}
	e.cursorMu.Lock()
	defer e.cursorMu.Unlock()
	eventID = strings.TrimSpace(eventID)
	if eventID == "" || eventID == e.cursor {
		return nil
	}
	if err := writeCursor(e.cursorPath(), eventID); err != nil {
		return err
	}
	e.cursor = eventID
	return nil
}

// advance records an event as handled, monotonically.
func (e *Engine) advance(event upstream.Event) {
	if event.ID == "" {
		return
	}
	if err := e.SetCursor(event.ID); err != nil {
		e.log.Warn("could not advance the flow replay cursor", "event", event.ID, "error", err)
	}
}

func writeCursor(path, eventID string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("engine: create cursor directory %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".flow-cursor-*")
	if err != nil {
		return fmt.Errorf("engine: create cursor temp: %w", err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("engine: chmod cursor: %w", err)
	}
	data, err := json.Marshal(cursorState{EventID: eventID, UpdatedAt: time.Now().UTC()})
	if err != nil {
		_ = temp.Close()
		return fmt.Errorf("engine: marshal cursor: %w", err)
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("engine: write cursor: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("engine: close cursor: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("engine: replace cursor: %w", err)
	}
	return nil
}
