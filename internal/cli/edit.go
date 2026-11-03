package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// Edit opens a ticket file in the user's editor.
func Edit(tasksDir, editor, taskID string) error {
	tasksDir = config.ExpandTilde(tasksDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	path := filepath.Join(tasksDir, resolvedID+".md")
	var before []byte
	if err := ticket.WithOfflineWrite(tasksDir, func() error {
		var err error
		before, err = os.ReadFile(path)
		return err
	}); err != nil {
		return err
	}

	f, err := os.CreateTemp("", "kontora-edit-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(before)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := openEditor(editor, f.Name()); err != nil {
		return err
	}
	after, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	t, err := ticket.ParseBytes(after)
	if err != nil {
		return err
	}
	if err := ticket.RejectImportedOwnership(t); err != nil {
		return err
	}
	if t.ID != resolvedID {
		return fmt.Errorf("editor cannot change ticket id")
	}
	return ticket.WithOfflineWrite(tasksDir, func() error {
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(before, current) {
			return fmt.Errorf("ticket changed while editor was open")
		}
		return os.WriteFile(path, after, 0o644)
	})
}
