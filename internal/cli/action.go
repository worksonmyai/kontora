package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// Pause sets a ticket's status to paused.
func Pause(cfg *config.Config, taskID string) error {
	return SetStatus(cfg, taskID, string(ticket.StatusPaused))
}

// Retry resets a ticket to todo with attempt=0 for re-processing.
func Retry(cfg *config.Config, taskID string) error {
	repo := retryRepo{store.NewDiskRepo(cfg.TicketsDir), config.ExpandTilde(cfg.LogsDir)}
	svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})
	_, err := svc.Retry(taskID)
	return err
}

type retryRepo struct {
	*store.DiskRepo
	logsDir string
}

func (r retryRepo) WithMutation(fn func(app.MutationRepository) error) error {
	return r.DiskRepo.WithMutation(func(scoped app.MutationRepository) error {
		return fn(retryMutationRepo{scoped, r.logsDir})
	})
}

type retryMutationRepo struct {
	app.MutationRepository
	logsDir string
}

func (r retryMutationRepo) Save(st *app.StoredTicket) error {
	// Check the resolved ticket's journal under the board mutation lock.
	path := filepath.Join(session.TicketDir(r.logsDir, st.Ticket.ID), "workflow-condition.json")
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%w: retry blocked by workflow condition journal %s; use workflow-recover only if the ticket asks you to acknowledge termination", app.ErrInvalidState, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: cannot check workflow condition journal: %v", app.ErrInvalidState, err)
	}
	return r.MutationRepository.Save(st)
}

// Cancel sets a ticket's status to cancelled.
func Cancel(cfg *config.Config, taskID string) error {
	return SetStatus(cfg, taskID, string(ticket.StatusCancelled))
}

// Done sets a ticket's status to done.
func Done(cfg *config.Config, taskID string) error {
	return SetStatus(cfg, taskID, string(ticket.StatusDone))
}

// SetStage moves a ticket to a specific pipeline stage by name.
func SetStage(cfg *config.Config, taskID, targetStage string) error {
	repo := store.NewDiskRepo(cfg.TicketsDir)
	svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})
	_, err := svc.SetStage(taskID, targetStage)
	return err
}

// Run enqueues a ticket for processing via the local daemon API.
// It returns the dependency ids that hold the ticket back, so the caller can
// say "blocked" rather than "queued" for a ticket the daemon will not pick up.
func Run(cfg *config.Config, taskID string) ([]string, error) {
	// Resolve ticket ID prefix against local files for the API call.
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return nil, err
	}

	client, err := LocalClient(cfg)
	if err != nil {
		return nil, err
	}
	info, err := client.Run(resolvedID)
	if err != nil {
		return nil, err
	}
	return info.Blockers, nil
}

// Skip advances a ticket to the next pipeline stage, or marks it done
// if it is on the final stage.
func Skip(cfg *config.Config, taskID string) error {
	repo := store.NewDiskRepo(cfg.TicketsDir)
	svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})
	_, err := svc.Skip(taskID)
	return err
}
