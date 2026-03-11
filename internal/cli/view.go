package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

// View prints ticket details to the given writer.
func View(cfg *config.Config, taskID string, w io.Writer) error {
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	t, err := ticket.ParseFile(filepath.Join(tasksDir, resolvedID+".md"))
	if err != nil {
		return fmt.Errorf("reading ticket: %w", err)
	}

	fmt.Fprintf(w, "%s  %s\n", t.ID, string(t.Status))
	fmt.Fprintf(w, "%s\n", t.Title())

	if t.Pipeline != "" {
		fmt.Fprintf(w, "pipeline:  %s\n", t.Pipeline)

		if pipeline, ok := cfg.Pipelines[t.Pipeline]; ok {
			var stages []string
			for _, stage := range pipeline {
				if stage.Role == t.Role {
					stages = append(stages, "["+stage.Role+"]")
				} else {
					stages = append(stages, stage.Role)
				}
			}
			fmt.Fprintf(w, "stage:     %s\n", strings.Join(stages, " → "))
		}
	}
	if t.Path != "" {
		fmt.Fprintf(w, "path:      %s\n", t.Path)
	}
	if t.Branch != "" {
		fmt.Fprintf(w, "branch:    %s\n", t.Branch)
	}
	if t.Role != "" {
		agent := app.AgentForStage(cfg, t.Pipeline, t.Role)
		if agent != "" {
			fmt.Fprintf(w, "agent:     %s\n", agent)
		}
	}
	if t.Status == ticket.StatusInProgress && t.StartedAt != nil {
		fmt.Fprintf(w, "running:   %s\n", FormatDuration(time.Since(*t.StartedAt)))
	} else if t.StartedAt != nil {
		fmt.Fprintf(w, "started:   %s\n", FormatTimestamp(t.StartedAt))
	}
	if t.Attempt > 0 {
		fmt.Fprintf(w, "attempt:   %d\n", t.Attempt)
	}
	if t.CompletedAt != nil {
		fmt.Fprintf(w, "completed: %s\n", FormatTimestamp(t.CompletedAt))
	}

	if len(t.History) > 0 {
		fmt.Fprintf(w, "\nHistory:\n")
		for _, h := range t.History {
			exit := "✓"
			if h.ExitCode != 0 {
				exit = fmt.Sprintf("✗ exit %d", h.ExitCode)
			}
			fmt.Fprintf(w, "  %s (%s) %s\n", h.Stage, h.Agent, exit)
		}
	}

	fmt.Fprintf(w, "\n%s", t.Body)
	return nil
}
