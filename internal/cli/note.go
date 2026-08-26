package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// NoteOptions carries the byline fields beside the note's text. An empty
// Author writes a note with no name on it, which is what a legacy note has.
type NoteOptions struct {
	Author  string
	ReplyTo string
}

// Note appends a note to a ticket file directly, without a daemon. The write is
// read-modify-write with no lock, so a daemon running against the same store
// can lose the note; `kontora note` routes through the daemon when one answers
// and only falls back here when none does.
func Note(tasksDir string, taskID string, text string, opts NoteOptions) error {
	tasksDir = config.ExpandTilde(tasksDir)
	resolved, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	path := filepath.Join(tasksDir, resolved+".md")
	t, err := ticket.ParseFile(path)
	if err != nil {
		return fmt.Errorf("parsing ticket: %w", err)
	}

	if _, err := t.AddNote(ticket.AddNoteOptions{
		Text:     text,
		Author:   opts.Author,
		ParentID: opts.ReplyTo,
		At:       time.Now(),
	}); err != nil {
		return err
	}

	out, err := t.Marshal()
	if err != nil {
		return fmt.Errorf("marshalling ticket: %w", err)
	}

	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing ticket file: %w", err)
	}
	return nil
}
