package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// Pause sets a ticket's status to paused.
func Pause(tasksDir, taskID string) error {
	return SetStatus(tasksDir, taskID, string(ticket.StatusPaused))
}

// Retry sets a ticket's status to todo for re-processing.
func Retry(tasksDir, taskID string) error {
	return SetStatus(tasksDir, taskID, string(ticket.StatusTodo))
}

// Cancel sets a ticket's status to cancelled.
func Cancel(tasksDir, taskID string) error {
	return SetStatus(tasksDir, taskID, string(ticket.StatusCancelled))
}

// SetStage moves a ticket to a specific pipeline stage by name.
func SetStage(cfg *config.Config, taskID, targetStage string) error {
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	filePath := filepath.Join(tasksDir, resolvedID+".md")
	t, err := ticket.ParseFile(filePath)
	if err != nil {
		return fmt.Errorf("parsing ticket: %w", err)
	}

	pipelineCfg, ok := cfg.Pipelines[t.Pipeline]
	if !ok {
		return fmt.Errorf("unknown pipeline %q for ticket %s", t.Pipeline, resolvedID)
	}

	found := false
	for _, stage := range pipelineCfg {
		if stage.Role == targetStage {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("stage %q not found in pipeline %q", targetStage, t.Pipeline)
	}

	if err := t.SetField("role", targetStage); err != nil {
		return fmt.Errorf("setting role: %w", err)
	}

	out, err := t.Marshal()
	if err != nil {
		return fmt.Errorf("marshalling ticket: %w", err)
	}
	return os.WriteFile(filePath, out, 0o644)
}

// Skip advances a ticket to the next pipeline stage, or marks it done
// if it is on the final stage. The ticket file is modified directly.
func Skip(cfg *config.Config, taskID string) error {
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	filePath := filepath.Join(tasksDir, resolvedID+".md")
	t, err := ticket.ParseFile(filePath)
	if err != nil {
		return fmt.Errorf("parsing ticket: %w", err)
	}

	pipelineCfg, ok := cfg.Pipelines[t.Pipeline]
	if !ok {
		return fmt.Errorf("unknown pipeline %q for ticket %s", t.Pipeline, resolvedID)
	}

	currentIdx := -1
	for i, stage := range pipelineCfg {
		if stage.Role == t.Role {
			currentIdx = i
			break
		}
	}
	if currentIdx < 0 {
		return fmt.Errorf("role %q not found in pipeline %q", t.Role, t.Pipeline)
	}

	if currentIdx+1 >= len(pipelineCfg) {
		if err := t.SetField("status", string(ticket.StatusDone)); err != nil {
			return fmt.Errorf("setting status: %w", err)
		}
		now := time.Now().UTC()
		if err := t.SetField("completed_at", now); err != nil {
			return fmt.Errorf("setting completed_at: %w", err)
		}
	} else {
		if err := t.SetField("role", pipelineCfg[currentIdx+1].Role); err != nil {
			return fmt.Errorf("setting role: %w", err)
		}
		if err := t.SetField("status", string(ticket.StatusTodo)); err != nil {
			return fmt.Errorf("setting status: %w", err)
		}
		if err := t.SetField("attempt", 0); err != nil {
			return fmt.Errorf("setting attempt: %w", err)
		}
	}

	out, err := t.Marshal()
	if err != nil {
		return fmt.Errorf("marshalling ticket: %w", err)
	}
	return os.WriteFile(filePath, out, 0o644)
}
