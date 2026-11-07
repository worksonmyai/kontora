package cli

import (
	"fmt"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// NoteOptions carries the byline fields beside the note's text. An empty
// Author writes a note with no name on it, which is what a legacy note has.
type NoteOptions struct {
	Author  string
	ReplyTo string
}

// Note appends a note to a ticket file directly, without a daemon. The fresh
// read and write share the board mutation lock with other local mutations.
func Note(tasksDir string, taskID string, text string, opts NoteOptions) error {
	tasksDir = config.ExpandTilde(tasksDir)
	repo := store.NewDiskRepo(tasksDir)
	return repo.WithMutation(func(repo app.MutationRepository) error {
		resolved, err := repo.Resolve(taskID)
		if err != nil {
			return err
		}
		st, err := repo.Get(resolved)
		if err != nil {
			return fmt.Errorf("parsing ticket: %w", err)
		}
		if _, err := st.Ticket.AddNote(ticket.AddNoteOptions{
			Text:     text,
			Author:   opts.Author,
			ParentID: opts.ReplyTo,
			At:       time.Now(),
		}); err != nil {
			return err
		}
		if err := repo.Save(st); err != nil {
			return fmt.Errorf("writing ticket file: %w", err)
		}
		return nil
	})
}
