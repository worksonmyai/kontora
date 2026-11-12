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
// req (non-nil pointers or slices) are changed. It mirrors the daemon's UpdateTicket rules
// so local and remote edits behave the same: editing is allowed only in
// open/todo/paused/human_review (or a custom status), and pipeline/agent values
// are validated against the config.
func Update(cfg *config.Config, id string, req web.UpdateTicketRequest) error {
	if err := req.ValidateDescription(); err != nil {
		return err
	}
	repo := store.NewDiskRepo(cfg.TicketsDir)
	var prepared *app.PreparedExecutionSettings
	if req.Path != nil || req.Branch != nil {
		// Resolving Git repository identities can run subprocesses, so prepare
		// the proposal before taking the board mutation lock. Nothing is saved.
		resolved, err := repo.Resolve(id)
		if err != nil {
			return err
		}
		st, err := repo.Get(resolved)
		if err != nil {
			return err
		}
		if err := applyUpdate(cfg, st.Ticket, req); err != nil {
			return err
		}
		svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})
		prepared, err = svc.PrepareExecutionSettings(st.Ticket)
		if err != nil {
			return err
		}
	}
	return repo.WithMutation(func(repo app.MutationRepository) error {
		resolved, err := repo.Resolve(id)
		if err != nil {
			return err
		}
		st, err := repo.Get(resolved)
		if err != nil {
			return err
		}
		if err := applyUpdate(cfg, st.Ticket, req); err != nil {
			return err
		}
		if prepared != nil {
			// Reject changed execution inputs; this check reads fresh state but
			// never invokes Git while the transaction is held.
			if err := prepared.Validate(repo, st.Ticket); err != nil {
				return err
			}
		}
		return repo.Save(st)
	})
}

func applyUpdate(cfg *config.Config, t *ticket.Ticket, req web.UpdateTicketRequest) error {
	// Only a passive epic's derived status does not constrain brief editing.
	// Agent epics use the same edit guard as ordinary work.
	if !t.IsPassiveEpic() && (t.IsRunning() || !cfg.StatusAllowsEdit(string(t.Status))) {
		return fmt.Errorf("%w: cannot update ticket in status %s", app.ErrInvalidState, t.Status)
	}

	if err := cfg.ValidateNotifyFields(req.Notify, req.NotifyChannels); err != nil {
		return fmt.Errorf("%w: %v", web.ErrInvalidNotify, err)
	}

	if req.Pipeline != nil {
		// There is no project default to skip here, but "none" still has to
		// mean "no pipeline", or the word would work in new and init and error
		// out in update.
		pipeline := config.ClearNone(*req.Pipeline)
		if t.Kind == ticket.KindEpic && pipeline != "" {
			return fmt.Errorf("%w: epics have no pipeline", app.ErrInvalidState)
		}
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
	if err := app.SetNotifyFields(t, req.Notify, req.NotifyChannels); err != nil {
		return err
	}
	if req.Body != nil {
		t.SetBody(*req.Body)
	}
	if req.Description != nil {
		return t.SetDescription(*req.Description)
	}

	return nil
}
