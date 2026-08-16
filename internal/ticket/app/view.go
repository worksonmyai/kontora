package app

import (
	"slices"
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
	// FinishedAt is when the ticket last stopped running, derived from the
	// history. Unlike the file mtime it does not move when the markdown is
	// edited. See FinishedAt for the precedence rule.
	FinishedAt *time.Time
	Branch     string
	BaseBranch string
	ClaimedBy  string
	Stages     []string
	History    []HistoryView
	Body       string
	LastError  string
	LastLog    string
	Summary    string
	// FinalSummary is the ticket-level outcome, not the latest run's summary.
	FinalSummary string
	// Deps, Links and Parent are the relation ids as written in the frontmatter.
	// They are ids only: resolving one to a title needs the whole store, which
	// this projection does not see.
	Deps   []string
	Links  []string
	Parent string
}

// FinishedAt is when a ticket last stopped running: the completed_at of the
// newest history entry that carries one, and the frontmatter completed_at when
// no run recorded a finish. It is nil for a ticket that never ran.
//
// The board orders HUMAN REVIEW by it. The file mtime cannot do that job: a
// note, a status change or a `ticket link` bumps the mtime, and the card would
// jump to the top of the column without anything having run.
func FinishedAt(t *ticket.Ticket) *time.Time {
	for _, h := range slices.Backward(t.History) {
		if h.CompletedAt != nil {
			return h.CompletedAt
		}
	}
	return t.CompletedAt
}

// HistoryView is a single history entry in a View.
type HistoryView struct {
	Stage         string
	Agent         string
	Model         string
	Effort        string
	ExitCode      int
	Run           int
	StartedAt     *time.Time
	CompletedAt   *time.Time
	Summary       string
	Kind          string
	SessionReused bool
}

// BuildView projects a ticket into a View using config for agent/stage resolution.
// When detail is false (the list/board projection) the heavy detail-only fields
// (History, LastError, LastLog, Body) are left empty; the board cards never read
// them. The detail endpoint and SSE ticket_updated events pass detail=true.
func BuildView(cfg *config.Config, t *ticket.Ticket, detail bool) View {
	v := View{
		ID:          t.ID,
		Title:       t.Title(),
		Status:      string(t.Status),
		Kontora:     t.Kontora,
		Stage:       t.Stage,
		Pipeline:    t.Pipeline,
		Path:        t.Path,
		Attempt:     t.Attempt,
		CreatedAt:   t.Created,
		StartedAt:   t.StartedAt,
		CompletedAt: t.CompletedAt,
		FinishedAt:  FinishedAt(t),
		Branch:      t.Branch,
		BaseBranch:  t.BaseBranch,
		ClaimedBy:   t.ClaimedBy,
		Deps:        t.Deps,
		Links:       t.Links,
		Parent:      t.Parent,
	}

	if t.Agent != "" {
		v.Agent = t.Agent
		v.AgentOverride = true
	} else if pipelineCfg, ok := cfg.Pipelines[t.Pipeline]; ok {
		for _, step := range pipelineCfg {
			if step.Stage == t.Stage {
				v.Agent = step.Agent
				break
			}
		}
	} else if t.Kontora && t.Pipeline == "" {
		v.Agent = cfg.DefaultAgent
	}

	if pipelineCfg, ok := cfg.Pipelines[t.Pipeline]; ok {
		stages := make([]string, len(pipelineCfg))
		for i, step := range pipelineCfg {
			stages[i] = step.Stage
		}
		v.Stages = stages
	}
	if t.Kontora && t.Pipeline == "" && len(v.Stages) == 0 {
		v.Stages = []string{"default"}
	}

	if detail {
		if len(t.History) > 0 {
			v.History = make([]HistoryView, len(t.History))
			for i, h := range t.History {
				v.History[i] = HistoryView{
					Stage:         h.Stage,
					Agent:         h.Agent,
					Model:         h.Model,
					Effort:        h.Effort,
					ExitCode:      h.ExitCode,
					Run:           h.Run,
					StartedAt:     h.StartedAt,
					CompletedAt:   h.CompletedAt,
					Summary:       h.Summary,
					Kind:          h.Kind,
					SessionReused: h.SessionReused,
				}
			}
		}
		v.LastError = t.LastError
		v.LastLog = t.LastLog
		v.Summary = t.Summary
		v.FinalSummary = t.FinalSummary
		v.Body = t.Body
	}

	return v
}
