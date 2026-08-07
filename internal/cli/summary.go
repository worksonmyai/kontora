package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// Summary sets the ticket's summary frontmatter field.
func Summary(tasksDir string, taskID string, text string) error {
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

	if err := t.SetField("summary", text); err != nil {
		return fmt.Errorf("setting summary: %w", err)
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
