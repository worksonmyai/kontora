package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

var validStatuses = map[ticket.Status]bool{
	ticket.StatusOpen:      true,
	ticket.StatusTodo:      true,
	ticket.StatusPaused:    true,
	ticket.StatusDone:      true,
	ticket.StatusCancelled: true,
}

func SetStatus(tasksDir string, taskID string, status string) error {
	s := ticket.Status(status)
	if !validStatuses[s] {
		valid := make([]string, 0, len(validStatuses))
		for k := range validStatuses {
			valid = append(valid, string(k))
		}
		return fmt.Errorf("invalid status %q, valid statuses: %v", status, valid)
	}

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

	if t.Status == s {
		return fmt.Errorf("ticket %s is already %s", resolved, status)
	}

	if err := t.SetField("status", status); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}

	if s == ticket.StatusDone {
		now := time.Now().UTC()
		if err := t.SetField("completed_at", now); err != nil {
			return fmt.Errorf("setting completed_at: %w", err)
		}
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
