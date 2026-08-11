package cli

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/config"
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

	if !cfg.StatusAllowsEdit(string(t.Status)) {
		return fmt.Errorf("%w: cannot update ticket in status %s", app.ErrInvalidState, t.Status)
	}

	if req.Pipeline != nil {
		// There is no project default to skip here, but "none" still has to
		// mean "no pipeline", or the word would work in new and init and error
		// out in update.
		pipeline := config.ClearNone(*req.Pipeline)
		if pipeline != "" {
			if _, ok := cfg.Pipelines[pipeline]; !ok {
				return fmt.Errorf("unknown pipeline %q", pipeline)
			}
		}
		if err := t.SetField("pipeline", pipeline); err != nil {
			return err
		}
	}
	if req.Path != nil {
		if err := t.SetField("path", *req.Path); err != nil {
			return err
		}
	}
	if req.Agent != nil {
		agent := config.ClearNone(*req.Agent)
		if agent != "" {
			if _, ok := cfg.Agents[agent]; !ok {
				return fmt.Errorf("%w %q", app.ErrUnknownAgent, agent)
			}
		}
		if err := t.SetField("agent", agent); err != nil {
			return err
		}
	}
	if req.Branch != nil {
		if err := t.SetField("branch", *req.Branch); err != nil {
			return err
		}
	}
	if req.BaseBranch != nil {
		if err := t.SetField("base_branch", *req.BaseBranch); err != nil {
			return err
		}
	}
	if req.Body != nil {
		t.SetBody(*req.Body)
	}

	return repo.Save(st)
}
