package cli

import (
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/config"
)

// Edit opens a ticket file in the user's editor.
func Edit(tasksDir, editor, taskID string) error {
	tasksDir = config.ExpandTilde(tasksDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	return openEditor(editor, filepath.Join(tasksDir, resolvedID+".md"))
}
