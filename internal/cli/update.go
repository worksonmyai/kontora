package cli

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/web"
)

// Update applies body/frontmatter changes to a ticket. Only the fields set in
// req (non-nil pointers) are changed. It mirrors the daemon's UpdateTicket rules
// so local and remote edits behave the same: editing is allowed only in
// open/todo/paused/human_review (or a custom status), and pipeline/agent values
// are validated against the config.
func Update(cfg *config.Config, id string, req web.UpdateTicketRequest) error {
	repo := store.NewDiskRepo(cfg.TicketsDir)
	resolved, err := repo.Resolve(id)
	if err != nil {
		return err
	}
	st, err := repo.Get(resolved)
	if err != nil {
		return err
	}
	t := st.Ticket

	switch t.Status {
	case ticket.StatusOpen, ticket.StatusTodo, ticket.StatusPaused, ticket.StatusHumanReview:
		// editable
	case ticket.StatusInProgress, ticket.StatusDone, ticket.StatusCancelled, ticket.StatusArchived:
		return fmt.Errorf("%w: cannot update ticket in status %s", app.ErrInvalidState, t.Status)
	default:
		if !cfg.IsCustomStatus(string(t.Status)) {
			return fmt.Errorf("%w: cannot update ticket in status %s", app.ErrInvalidState, t.Status)
		}
	}

	if req.Pipeline != nil {
		if *req.Pipeline != "" {
			if _, ok := cfg.Pipelines[*req.Pipeline]; !ok {
				return fmt.Errorf("unknown pipeline %q", *req.Pipeline)
			}
		}
		if err := t.SetField("pipeline", *req.Pipeline); err != nil {
			return err
		}
	}
	if req.Path != nil {
		if err := t.SetField("path", *req.Path); err != nil {
			return err
		}
	}
	if req.Agent != nil {
		if *req.Agent != "" {
			if _, ok := cfg.Agents[*req.Agent]; !ok {
				return fmt.Errorf("%w %q", app.ErrUnknownAgent, *req.Agent)
			}
		}
		if err := t.SetField("agent", *req.Agent); err != nil {
			return err
		}
	}
	if req.Branch != nil {
		if err := t.SetField("branch", *req.Branch); err != nil {
			return err
		}
	}
	if req.Body != nil {
		t.SetBody(*req.Body)
	}

	return repo.Save(st)
}
