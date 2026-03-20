package app

import (
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// View is the domain-level projection of a ticket.
// Adapters (HTTP handlers, TUI renderers) convert from this.
type View struct {
	ID            string
	Title         string
	Status        string
	Kontora       bool
	Stage         string
	Pipeline      string
	Path          string
	Agent         string
	AgentOverride bool
	Attempt       int
	CreatedAt     *time.Time
	StartedAt     *time.Time
	CompletedAt   *time.Time
	Branch        string
	Stages        []string
	History       []HistoryView
	Body          string
	LastError     string
}

// HistoryView is a single history entry in a View.
type HistoryView struct {
	Stage       string
	Agent       string
	ExitCode    int
	StartedAt   *time.Time
	CompletedAt *time.Time
}

// BuildView projects a ticket into a View using config for agent/stage resolution.
func BuildView(cfg *config.Config, t *ticket.Ticket, includeBody bool) View {
	v := View{
		ID:          t.ID,
		Title:       t.Title(),
		Status:      string(t.Status),
		Kontora:     t.Kontora,
		Stage:       t.Role,
		Pipeline:    t.Pipeline,
		Path:        t.Path,
		Attempt:     t.Attempt,
		CreatedAt:   t.Created,
		StartedAt:   t.StartedAt,
		CompletedAt: t.CompletedAt,
		Branch:      t.Branch,
	}

	if t.Agent != "" {
		v.Agent = t.Agent
		v.AgentOverride = true
	} else if pipelineCfg, ok := cfg.Pipelines[t.Pipeline]; ok {
		for _, stage := range pipelineCfg {
			if stage.Role == t.Role {
				v.Agent = stage.Agent
				break
			}
		}
	} else if t.Kontora && t.Pipeline == "" {
		v.Agent = cfg.DefaultAgent
	}

	if pipelineCfg, ok := cfg.Pipelines[t.Pipeline]; ok {
		stages := make([]string, len(pipelineCfg))
		for i, stage := range pipelineCfg {
			stages[i] = stage.Role
		}
		v.Stages = stages
	}
	if t.Kontora && t.Pipeline == "" && len(v.Stages) == 0 {
		v.Stages = []string{"default"}
	}

	if len(t.History) > 0 {
		v.History = make([]HistoryView, len(t.History))
		for i, h := range t.History {
			v.History[i] = HistoryView{
				Stage:       h.Stage,
				Agent:       h.Agent,
				ExitCode:    h.ExitCode,
				StartedAt:   h.StartedAt,
				CompletedAt: h.CompletedAt,
			}
		}
	}

	v.LastError = t.LastError

	if includeBody {
		v.Body = t.Body
	}

	return v
}
