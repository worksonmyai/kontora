package app

import (
	"slices"
	"strings"
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
	GraphDefinition *GraphDefinitionView
	GraphExecution  *GraphExecutionView
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
	WorkflowExecutionID   string
	NodeVisitID           string
	DefinitionFingerprint string
	Template              string
	VisitAttempt          int
	Imported              bool
	RunOutcome            string
	OutcomeReason         string
	InvocationID          string
	Stage                 string
	Agent                 string
	Model                 string
	Effort                string
	ExitCode              int
	Run                   int
	StartedAt             *time.Time
	CompletedAt           *time.Time
	Summary               string
	Kind                  string
	SessionReused         bool
}

// GraphDefinitionView describes topology, not an execution order.
type GraphDefinitionView struct {
	Version int             `json:"version"`
	Entry   string          `json:"entry"`
	Nodes   []GraphNodeView `json:"nodes"`
	Edges   []GraphEdgeView `json:"edges"`
}

type GraphNodeView struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Template string              `json:"template,omitempty"`
	Agent    string              `json:"agent,omitempty"`
	Verdicts []string            `json:"verdicts,omitempty"`
	Command  string              `json:"command,omitempty"`
	Timeout  string              `json:"timeout,omitempty"`
	When     *GraphConditionView `json:"when,omitempty"`
	Max      int                 `json:"max,omitempty"`
	Status   string              `json:"status,omitempty"`
}

type GraphConditionView struct {
	Field  string `json:"field"`
	Equals any    `json:"equals"`
}

type GraphEdgeView struct {
	From GraphPortView `json:"from"`
	To   string        `json:"to"`
}

type GraphPortView struct {
	Node string `json:"node"`
	Port string `json:"port"`
}

type GraphLoopView struct {
	NodeID string `json:"node_id"`
	Used   int    `json:"used"`
	// Older executions can lack saved loop limits. Zero means unknown.
	Max       int  `json:"max,omitempty"`
	Exhausted bool `json:"exhausted"`
}

type GraphExecutionView struct {
	Version               int                      `json:"version"`
	ExecutionID           string                   `json:"execution_id"`
	Revision              uint64                   `json:"revision"`
	DefinitionFingerprint string                   `json:"definition_fingerprint"`
	CurrentNode           string                   `json:"current_node"`
	CurrentTemplate       string                   `json:"current_template,omitempty"`
	VisitID               string                   `json:"visit_id,omitempty"`
	Visit                 uint64                   `json:"visit"`
	Attempt               int                      `json:"attempt"`
	LastResult            *ticket.GraphResult      `json:"last_result,omitempty"`
	SelectedEdges         []ticket.GraphTransition `json:"selected_edges,omitempty"`
	Loops                 []GraphLoopView          `json:"loops,omitempty"`
	// Full traces are detail-only; selected edges contain one row per port.
	Transitions        []ticket.GraphTransition `json:"transitions,omitempty"`
	PreviousExecutions []ticket.GraphEvidence   `json:"previous_executions,omitempty"`
}

func BuildGraphDefinition(graph *config.WorkflowGraph) *GraphDefinitionView {
	if graph == nil {
		return nil
	}
	v := &GraphDefinitionView{Version: graph.Version, Entry: graph.Entry,
		Nodes: make([]GraphNodeView, 0, len(graph.Nodes)), Edges: make([]GraphEdgeView, 0, len(graph.Edges))}
	for _, n := range graph.Nodes {
		node := GraphNodeView{ID: n.ID, Type: n.Type, Template: n.Stage, Agent: n.Agent,
			Verdicts: slices.Clone(n.Verdicts), Command: n.Command, Max: n.Max, Status: n.Status}
		if n.Timeout.Duration != 0 {
			node.Timeout = n.Timeout.String()
		}
		if n.When != nil {
			node.When = &GraphConditionView{Field: n.When.Field, Equals: n.When.Equals}
		}
		v.Nodes = append(v.Nodes, node)
	}
	for _, e := range graph.Edges {
		v.Edges = append(v.Edges, GraphEdgeView{From: GraphPortView{Node: e.From.Node, Port: e.From.Port}, To: e.To})
	}
	return v
}

func buildGraphExecution(t *ticket.Ticket, detail bool) *GraphExecutionView {
	s := t.GraphState
	if s == nil {
		return nil
	}
	v := &GraphExecutionView{Version: s.Version, ExecutionID: s.ExecutionID, Revision: s.Revision,
		DefinitionFingerprint: s.DefinitionFingerprint, CurrentNode: t.Stage, CurrentTemplate: s.NodeTemplates[t.Stage], VisitID: s.VisitID,
		Visit: s.Visit, Attempt: graphVisitAttempt(t), LastResult: s.LastResult}
	selected := make(map[GraphPortView]int)
	exhausted := make(map[string]bool)
	for _, edge := range s.Transitions {
		key := GraphPortView{Node: edge.From, Port: edge.Port}
		if i, ok := selected[key]; ok {
			v.SelectedEdges[i] = edge
		} else {
			selected[key] = len(v.SelectedEdges)
			v.SelectedEdges = append(v.SelectedEdges, edge)
		}
		if edge.Port == "exhausted" {
			exhausted[edge.From] = true
		}
	}
	if v.CurrentTemplate == "" {
		for _, row := range slices.Backward(t.History) {
			if row.Kind == "" && row.WorkflowExecutionID == s.ExecutionID && row.NodeVisitID == s.VisitID &&
				row.DefinitionFingerprint == s.DefinitionFingerprint && row.Stage == t.Stage {
				v.CurrentTemplate = row.Template
				break
			}
		}
	}
	for id, limit := range s.LoopLimits {
		v.Loops = append(v.Loops, GraphLoopView{NodeID: id, Used: s.LoopCounts[id], Max: max(0, limit), Exhausted: exhausted[id]})
	}
	for id, used := range s.LoopCounts {
		if _, known := s.LoopLimits[id]; !known {
			v.Loops = append(v.Loops, GraphLoopView{NodeID: id, Used: used, Exhausted: exhausted[id]})
		}
	}
	slices.SortFunc(v.Loops, func(a, b GraphLoopView) int { return strings.Compare(a.NodeID, b.NodeID) })
	if detail {
		v.Transitions = s.Transitions
		v.PreviousExecutions = s.PreviousExecutions
	}
	return v
}

func graphVisitAttempt(t *ticket.Ticket) int {
	attempt := 0
	for _, row := range t.History {
		if row.WorkflowExecutionID == t.GraphState.ExecutionID && row.NodeVisitID == t.GraphState.VisitID && row.Kind == "" {
			attempt++
		}
	}
	if !t.IsRunning() && t.Status != ticket.StatusTodo && attempt > 0 {
		attempt--
	}
	return attempt
}

// ResolveViewAgent shares BuildView's agent rules without cloning preserved ticket YAML.
func ResolveViewAgent(cfg *config.Config, t *ticket.Ticket) string {
	if definition, ok := cfg.Pipelines[t.Pipeline]; ok && definition.IsGraph() {
		resolved, err := definition.Resolve(t.Stage, t.Agent)
		if err != nil {
			return ""
		}
		return resolved.Agent
	}
	if t.Agent != "" {
		return t.Agent
	}
	if pipelineCfg, ok := cfg.Pipelines[t.Pipeline]; ok {
		for _, step := range pipelineCfg.Legacy {
			if step.Stage == t.Stage {
				return step.Agent
			}
		}
		// Keep the built-in stage's fallback in sync with the daemon's reworkAgent.
		if cfg.ReworkIsBuiltin && t.Stage == config.ReworkStageName {
			for _, h := range slices.Backward(t.History) {
				if h.Agent != "" {
					return h.Agent
				}
			}
			return cfg.DefaultAgent
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
		stages := make([]string, len(pipelineCfg.Legacy))
		for i, step := range pipelineCfg.Legacy {
			stages[i] = step.Stage
		}
		v.Stages = stages
		if pipelineCfg.Graph != nil {
			v.Stages = nil
			v.GraphDefinition = BuildGraphDefinition(pipelineCfg.Graph)
		}
	}
	// A passive epic gets neither the default agent nor a stage ribbon: it has no
	// pipeline because it is not work, not because its pipeline is implied.
	if t.Kontora && t.Pipeline == "" && len(v.Stages) == 0 && !t.IsPassiveEpic() {
		v.Stages = []string{"default"}
		if t.IsAgentEpic() {
			v.Stage = "default"
		}
	}

	v.GraphExecution = buildGraphExecution(t, detail)

	if detail {
		if len(t.History) > 0 {
			visitAttempts := make(map[string]int)
			v.History = make([]HistoryView, len(t.History))
			for i, h := range t.History {
				visitKey := h.WorkflowExecutionID + "/" + h.NodeVisitID
				visitAttempt := 0
				if h.NodeVisitID != "" && h.Kind == "" {
					visitAttempts[visitKey]++
					visitAttempt = visitAttempts[visitKey]
				}
				v.History[i] = HistoryView{
					WorkflowExecutionID:   h.WorkflowExecutionID,
					NodeVisitID:           h.NodeVisitID,
					DefinitionFingerprint: h.DefinitionFingerprint,
					Template:              h.Template,
					VisitAttempt:          visitAttempt,
					Imported:              t.IsImportedHistory(i),
					Stage:                 h.Stage,
					Agent:                 h.Agent,
					Model:                 h.Model,
					Effort:                h.Effort,
					ExitCode:              h.ExitCode,
					RunOutcome:            string(h.RunOutcome),
					OutcomeReason:         h.OutcomeReason,
					InvocationID:          h.InvocationID,
					Run:                   h.Run,
					StartedAt:             h.StartedAt,
					CompletedAt:           h.CompletedAt,
					Summary:               h.Summary,
					Kind:                  h.Kind,
					SessionReused:         h.SessionReused,
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
