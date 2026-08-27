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

// historyStage labels a history row with its stage, marking a run that rewrote
// the ticket from Plannotator annotations rather than doing the stage's work.
func historyStage(h ticket.HistoryEntry) string {
	if h.Kind == ticket.KindAnnotation {
		return h.Stage + " · annotation"
	}
	return h.Stage
}

// View prints ticket details to the given writer.
func View(cfg *config.Config, taskID string, w io.Writer) error {
	t, err := readTicketByID(cfg, taskID)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "%s  %s\n", t.ID, string(t.Status))
	fmt.Fprintf(w, "%s\n", t.Title())

	if t.Pipeline != "" {
		fmt.Fprintf(w, "pipeline:  %s\n", t.Pipeline)

		if pipeline, ok := cfg.Pipelines[t.Pipeline]; ok {
			var stages []string
			for _, step := range pipeline {
				if step.Stage == t.Stage {
					stages = append(stages, "["+step.Stage+"]")
				} else {
					stages = append(stages, step.Stage)
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
	if t.BaseBranch != "" {
		fmt.Fprintf(w, "base:      %s\n", t.BaseBranch)
	}
	if t.Stage != "" {
		agent := app.AgentForStage(cfg, t.Pipeline, t.Stage)
		if agent != "" {
			fmt.Fprintf(w, "agent:     %s\n", agent)
		}
	}
	if t.ScheduledAt != "" {
		fmt.Fprintf(w, "starts:    %s\n", FormatSchedule(t.ScheduledAt))
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
			fmt.Fprintf(w, "  %s (%s) %s\n", historyStage(h), h.Agent, exit)
		}
	}

	if t.LastError != "" {
		fmt.Fprintf(w, "\n%s\n", styleFail.Render(fmt.Sprintf("⚠ Last error: %s", t.LastError)))
	}

	fmt.Fprintf(w, "\n%s", t.Body)
	return nil
}

// ViewBody prints the ticket's stored markdown body and nothing else: no
// metadata, no styling, and no relation sections. It is the counterpart of
// `kontora update --body-file`, so a caller can read a body out, edit it, and
// write it back without the round trip changing anything.
func ViewBody(cfg *config.Config, taskID string, w io.Writer) error {
	t, err := readTicketByID(cfg, taskID)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, t.Body)
	return err
}

func readTicketByID(cfg *config.Config, taskID string) (*ticket.Ticket, error) {
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return nil, err
	}
	t, err := ticket.ParseFile(filepath.Join(tasksDir, resolvedID+".md"))
	if err != nil {
		return nil, fmt.Errorf("reading ticket: %w", err)
	}
	return t, nil
}
