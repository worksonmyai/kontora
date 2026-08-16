package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// SetStatus moves a ticket to status, applying the same validation the daemon's
// API applies. It takes the whole config rather than the tickets directory
// because a custom status is only valid when the config declares it.
func SetStatus(cfg *config.Config, taskID string, status string) error {
	repo := store.NewDiskRepo(cfg.TicketsDir)
	svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})
	if _, err := svc.SetStatus(taskID, ticket.Status(status)); err != nil {
		return err
	}
	return nil
}

// MoveStatuses returns every status `kontora move` accepts, built-ins first and
// then the config's custom ones, so the error text and the shell completions
// list the same set.
func MoveStatuses(cfg *config.Config) []string {
	statuses := []string{
		string(ticket.StatusOpen),
		string(ticket.StatusTodo),
		string(ticket.StatusPaused),
		string(ticket.StatusHumanReview),
		string(ticket.StatusDone),
		string(ticket.StatusCancelled),
	}
	custom := slices.Clone(cfg.Statuses)
	slices.Sort(custom)
	return append(statuses, custom...)
}

// Move sets a ticket's status to any status the config allows, including the
// custom ones from `statuses:`. The named verbs (pause, cancel, done) are the
// common cases of the same operation.
func Move(cfg *config.Config, taskID, status string) error {
	if !slices.Contains(MoveStatuses(cfg), status) {
		return fmt.Errorf("unknown status %q (available: %s)", status, strings.Join(MoveStatuses(cfg), ", "))
	}
	return SetStatus(cfg, taskID, status)
}
