package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// Pause sets a ticket's status to paused.
func Pause(tasksDir, taskID string) error {
	return SetStatus(tasksDir, taskID, "paused")
}

// Retry resets a ticket to todo with attempt=0 for re-processing.
func Retry(tasksDir, taskID string) error {
	repo := store.NewDiskRepo(tasksDir)
	svc := app.New(nil, repo, app.NoopRuntime{})
	_, err := svc.Retry(taskID)
	return err
}

// Cancel sets a ticket's status to cancelled.
func Cancel(tasksDir, taskID string) error {
	return SetStatus(tasksDir, taskID, "cancelled")
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
// if it is on the final stage.
func Skip(cfg *config.Config, taskID string) error {
	repo := store.NewDiskRepo(cfg.TicketsDir)
	svc := app.New(cfg, repo, app.NoopRuntime{})
	_, err := svc.Skip(taskID)
	return err
}
