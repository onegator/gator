package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/onegator/gator/internal/proto"
)

// journalEntry is one unacked reliable message as stored on disk.
type journalEntry struct {
	Type    proto.MessageType `json:"type"`
	Payload json.RawMessage   `json:"payload"`
	JobID   string            `json:"job_id,omitempty"`
}

// fileJournal keeps unacked events and finishes across runner restarts. The whole list is
// rewritten atomically on every change; it stays short because the server acks quickly.
type fileJournal struct{ path string }

func (j fileJournal) load() ([]journalEntry, error) {
	b, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []journalEntry
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (j fileJournal) save(entries []journalEntry) error {
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, j.path)
}
