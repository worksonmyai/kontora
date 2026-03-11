package cli

import (
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

func SetStatus(tasksDir string, taskID string, status string) error {
	repo := store.NewDiskRepo(tasksDir)
	svc := app.New(&config.Config{}, repo, app.NoopRuntime{})
	_, err := svc.SetStatus(taskID, ticket.Status(status))
	return err
}
