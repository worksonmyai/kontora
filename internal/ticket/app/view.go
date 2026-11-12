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
	Managed         *ticket.ManagedOwnership
	ID              string
	Title           string
	Status          string
	ExecutionActive bool
	Kontora         bool
	Stage           string
	Pipeline        string
	Path            string
	Agent           string
	AgentOverride   bool
	Attempt         int
	InvocationID    string
	CreatedAt       *time.Time
	StartedAt       *time.Time
	CompletedAt     *time.Time
	// FinishedAt is when the ticket last stopped running, derived from the
	// history. Unlike the file mtime it does not move when the markdown is
	// edited. See FinishedAt for the precedence rule.
	FinishedAt *time.Time
	Branch     string
	BaseBranch string
	ClaimedBy  string
	// ScheduledAt is the raw scheduled_at frontmatter value, so a malformed
	// hand edit is shown rather than hidden. It is a board field, not a
	// detail-only one: an Open card renders it.
	ScheduledAt     string
	Stages          []string
	History         []HistoryView
	Body            string
	Description     *string
	DescriptionTail *string
	LastError       string
	LastLog         string
	Summary         string
	// FinalSummary is the ticket-level outcome, not the latest run's summary.
	FinalSummary string
	// Notify and NotifyChannels are the ticket's own notification fields as
	// written in the frontmatter: which statuses ask to be told about, and
	// where. Neither is resolved here — an empty NotifyChannels means the
	// ticket defers to its project and the global default, which is a
	// different thing from naming none.
	Notify         []string
	NotifyChannels []string
	// Deps, Links and Parent are the relation ids as written in the frontmatter.
	// They are ids only: resolving one to a title needs the whole store, which
	// this projection does not see.
	Deps   []string
	Links  []string
	Parent string
	// Kind is the ticket's kind field, empty for ordinary work and "epic" for
	// a ticket that groups others. Children is the epic's manual child order.
	Kind          string
	EpicExecution string
	Children      []string
	// The archive stamp, all empty unless Status is archived. ArchivedFrom is
	// the closed status the ticket held before it was archived.
	ArchivedFrom string
	ArchivedAt   *time.Time
	ArchivedBy   string
	ArchiveNote  string
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
	Imported      bool
	RunOutcome    string
	OutcomeReason string
	InvocationID  string
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

// ResolveViewAgent shares BuildView's agent rules without cloning preserved ticket YAML.
func ResolveViewAgent(cfg *config.Config, t *ticket.Ticket) string {
	if t.Agent != "" {
		return t.Agent
	}
	if pipelineCfg, ok := cfg.Pipelines[t.Pipeline]; ok {
		for _, step := range pipelineCfg {
			if step.Stage == t.Stage {
				return step.Agent
			}
		}
	} else if t.Kontora && t.Pipeline == "" && !t.IsPassiveEpic() {
		return cfg.DefaultAgent
	}
	return ""
}

// BuildView projects a ticket into a View using config for agent/stage resolution.
// When detail is false (the list/board projection) the heavy detail-only fields
// (History, LastError, LastLog, Body) are left empty; the board cards never read
// them. The detail endpoint and SSE ticket_updated events pass detail=true.
func BuildView(cfg *config.Config, t *ticket.Ticket, detail bool) View {
	t = t.Clone()
	v := View{
		Managed:         t.Managed,
		ID:              t.ID,
		Title:           t.Title(),
		Status:          string(t.Status),
		ExecutionActive: t.IsRunning() && !t.IsPassiveEpic(),
		Kontora:         t.Kontora,
		Stage:           t.Stage,
		Pipeline:        t.Pipeline,
		Path:            t.Path,
		Attempt:         t.Attempt,
		InvocationID:    t.InvocationID,
		CreatedAt:       t.Created,
		StartedAt:       t.StartedAt,
		CompletedAt:     t.CompletedAt,
		FinishedAt:      FinishedAt(t),
		Branch:          t.Branch,
		BaseBranch:      t.BaseBranch,
		ClaimedBy:       t.ClaimedBy,
		ScheduledAt:     t.ScheduledAt,
		Deps:            t.Deps,
		Links:           t.Links,
		Parent:          t.Parent,
		Kind:            string(t.Kind),
		EpicExecution:   t.EpicExecution,
		Children:        t.Children,

		Notify:         t.Notify.Statuses,
		NotifyChannels: t.NotifyChannels,

		ArchivedFrom: string(t.ArchivedFrom),
		ArchivedAt:   t.ArchivedAt,
		ArchivedBy:   t.ArchivedBy,
		ArchiveNote:  t.ArchiveNote,
	}

	v.Agent = ResolveViewAgent(cfg, t)
	v.AgentOverride = t.Agent != ""

	if pipelineCfg, ok := cfg.Pipelines[t.Pipeline]; ok {
		stages := make([]string, len(pipelineCfg))
		for i, step := range pipelineCfg {
			stages[i] = step.Stage
		}
		v.Stages = stages
	}
	// A passive epic gets neither the default agent nor a stage ribbon: it has no
	// pipeline because it is not work, not because its pipeline is implied.
	if t.Kontora && t.Pipeline == "" && len(v.Stages) == 0 && !t.IsPassiveEpic() {
		v.Stages = []string{"default"}
		if t.IsAgentEpic() {
			v.Stage = "default"
		}
	}

	if detail {
		if len(t.History) > 0 {
			v.History = make([]HistoryView, len(t.History))
			for i, h := range t.History {
				v.History[i] = HistoryView{
					Imported:      t.IsImportedHistory(i),
					Stage:         h.Stage,
					Agent:         h.Agent,
					Model:         h.Model,
					Effort:        h.Effort,
					ExitCode:      h.ExitCode,
					RunOutcome:    string(h.RunOutcome),
					OutcomeReason: h.OutcomeReason,
					InvocationID:  h.InvocationID,
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
		description, tail := ticket.SplitDescription(t.Body)
		v.Description = &description
		v.DescriptionTail = &tail
	}

	return v
}
