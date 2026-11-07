package cli

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// Summary sets the ticket's summary frontmatter field.
func Summary(tasksDir string, taskID string, text string) error {
	tasksDir = config.ExpandTilde(tasksDir)
	repo := store.NewDiskRepo(tasksDir)
	return repo.WithMutation(func(repo app.MutationRepository) error {
		resolved, err := repo.Resolve(taskID)
		if err != nil {
			return err
		}
		st, err := repo.Get(resolved)
		if err != nil {
			return fmt.Errorf("parsing ticket: %w", err)
		}
		if err := st.Ticket.SetField("summary", text); err != nil {
			return fmt.Errorf("setting summary: %w", err)
		}
		if err := repo.Save(st); err != nil {
			return fmt.Errorf("writing ticket file: %w", err)
		}
		return nil
	})
}
