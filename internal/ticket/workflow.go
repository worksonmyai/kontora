package ticket

import (
	"maps"
	"slices"
	"time"
)

// Ticket.Stage is the cursor.
type GraphState struct {
	Version               int               `yaml:"version" json:"version"`
	ExecutionID           string            `yaml:"execution_id" json:"execution_id"`
	Revision              uint64            `yaml:"revision" json:"revision"`
	DefinitionFingerprint string            `yaml:"definition_fingerprint" json:"definition_fingerprint"`
	VisitID               string            `yaml:"visit_id,omitempty" json:"visit_id,omitempty"`
	Visit                 uint64            `yaml:"visit" json:"visit"`
	NodeTemplates         map[string]string `yaml:"node_templates,omitempty" json:"node_templates,omitempty"`
	LoopLimits            map[string]int    `yaml:"loop_limits,omitempty" json:"loop_limits,omitempty"`
	LoopCounts            map[string]int    `yaml:"loop_counts,omitempty" json:"loop_counts,omitempty"`
	LastResult            *GraphResult      `yaml:"last_result,omitempty" json:"last_result,omitempty"`
	Handoff               *GraphHandoff     `yaml:"handoff,omitempty" json:"handoff,omitempty"`
	Transitions           []GraphTransition `yaml:"transitions,omitempty" json:"transitions,omitempty"`
	PreviousExecutions    []GraphEvidence   `yaml:"previous_executions,omitempty" json:"previous_executions,omitempty"`
	Wait                  *WorkflowWait     `yaml:"wait,omitempty" json:"wait,omitempty"`
	Wake                  *WakeContext      `yaml:"wake,omitempty" json:"wake,omitempty"`
	WakeCount             int               `yaml:"wake_count,omitempty" json:"wake_count,omitempty"`
	WaitHistory           []WorkflowWait    `yaml:"wait_history,omitempty" json:"wait_history,omitempty"`
	Continuation          bool              `yaml:"continuation,omitempty" json:"continuation,omitempty"`
}

func (t *Ticket) HasWorkflowWaitAuthority() bool {
	s := t.GraphState
	return s != nil && (s.Wait != nil || s.Continuation && len(s.WaitHistory) > 0)
}

type GraphEvidence struct {
	ExecutionID           string            `yaml:"execution_id" json:"execution_id"`
	DefinitionFingerprint string            `yaml:"definition_fingerprint" json:"definition_fingerprint"`
	NodeTemplates         map[string]string `yaml:"node_templates,omitempty" json:"node_templates,omitempty"`
	LoopLimits            map[string]int    `yaml:"loop_limits,omitempty" json:"loop_limits,omitempty"`
	LoopCounts            map[string]int    `yaml:"loop_counts,omitempty" json:"loop_counts,omitempty"`
	Transitions           []GraphTransition `yaml:"transitions,omitempty" json:"transitions,omitempty"`
	Wait                  *WorkflowWait     `yaml:"wait,omitempty" json:"wait,omitempty"`
	Wake                  *WakeContext      `yaml:"wake,omitempty" json:"wake,omitempty"`
	WakeCount             int               `yaml:"wake_count,omitempty" json:"wake_count,omitempty"`
	WaitHistory           []WorkflowWait    `yaml:"wait_history,omitempty" json:"wait_history,omitempty"`
	Continuation          bool              `yaml:"continuation,omitempty" json:"continuation,omitempty"`
}

// WorkflowWait is durable authority for one suspension, including its frozen
// workspace identity and diagnostic evidence. It is not an agent invocation.
type WorkflowWait struct {
	ID              string           `yaml:"id,omitempty" json:"id,omitempty"`
	Kind            string           `yaml:"kind,omitempty" json:"kind,omitempty"`
	Phase           string           `yaml:"phase,omitempty" json:"phase,omitempty"`
	ExecutionID     string           `yaml:"execution_id,omitempty" json:"execution_id,omitempty"`
	NodeID          string           `yaml:"node_id,omitempty" json:"node_id,omitempty"`
	VisitID         string           `yaml:"visit_id,omitempty" json:"visit_id,omitempty"`
	InvocationID    string           `yaml:"invocation_id,omitempty" json:"invocation_id,omitempty"`
	RequestedAt     time.Time        `yaml:"requested_at,omitempty" json:"requested_at,omitzero"`
	Until           time.Time        `yaml:"until,omitempty" json:"until,omitzero"`
	Deadline        time.Time        `yaml:"deadline,omitempty" json:"deadline,omitzero"`
	NextPoll        time.Time        `yaml:"next_poll,omitempty" json:"next_poll,omitzero"`
	Command         string           `yaml:"command,omitempty" json:"command,omitempty"`
	Poll            time.Duration    `yaml:"poll,omitempty" json:"poll,omitempty"`
	PollID          string           `yaml:"poll_id,omitempty" json:"poll_id,omitempty"`
	PollCount       int              `yaml:"poll_count,omitempty" json:"poll_count,omitempty"`
	Output          string           `yaml:"output,omitempty" json:"output,omitempty"`
	OutputTruncated bool             `yaml:"output_truncated,omitempty" json:"output_truncated,omitempty"`
	LogPath         string           `yaml:"log_path,omitempty" json:"log_path,omitempty"`
	Next            string           `yaml:"next,omitempty" json:"next,omitempty"`
	Worktree        string           `yaml:"worktree,omitempty" json:"worktree,omitempty"`
	Repository      string           `yaml:"repository,omitempty" json:"repository,omitempty"`
	Branch          string           `yaml:"branch,omitempty" json:"branch,omitempty"`
	RequiredCommit  string           `yaml:"required_commit,omitempty" json:"required_commit,omitempty"`
	Environment     string           `yaml:"environment,omitempty" json:"environment,omitempty"`
	Disarmed        bool             `yaml:"disarmed,omitempty" json:"disarmed,omitempty"`
	Receipt         *WorkflowReceipt `yaml:"receipt,omitempty" json:"receipt,omitempty"`
}

// WakeContext is frozen for a continuation and its failure retries.
type WakeContext struct {
	Reason  string           `yaml:"reason,omitempty" json:"reason,omitempty"`
	Output  string           `yaml:"output,omitempty" json:"output,omitempty"`
	Count   int              `yaml:"count,omitempty" json:"count,omitempty"`
	Next    string           `yaml:"next,omitempty" json:"next,omitempty"`
	WaitID  string           `yaml:"wait_id,omitempty" json:"wait_id,omitempty"`
	Receipt *WorkflowReceipt `yaml:"receipt,omitempty" json:"receipt,omitempty"`
}

// WorkflowReceipt records accepted release evidence, not inferred live state.
type WorkflowReceipt struct {
	ID          string    `yaml:"id,omitempty" json:"id,omitempty"`
	RequestID   string    `yaml:"request_id,omitempty" json:"request_id,omitempty"`
	Reason      string    `yaml:"reason,omitempty" json:"reason,omitempty"`
	Operator    string    `yaml:"operator,omitempty" json:"operator,omitempty"`
	Commit      string    `yaml:"commit,omitempty" json:"commit,omitempty"`
	Repository  string    `yaml:"repository,omitempty" json:"repository,omitempty"`
	Environment string    `yaml:"environment,omitempty" json:"environment,omitempty"`
	AcceptedAt  time.Time `yaml:"accepted_at,omitempty" json:"accepted_at,omitzero"`
}

func (r *WorkflowReceipt) Clone() *WorkflowReceipt {
	if r == nil {
		return nil
	}
	out := *r
	return &out
}

func (w *WorkflowWait) Clone() *WorkflowWait {
	if w == nil {
		return nil
	}
	out := *w
	out.Receipt = w.Receipt.Clone()
	return &out
}

func (w *WakeContext) Clone() *WakeContext {
	if w == nil {
		return nil
	}
	out := *w
	out.Receipt = w.Receipt.Clone()
	return &out
}

func cloneWaitHistory(history []WorkflowWait) []WorkflowWait {
	out := slices.Clone(history)
	for i := range out {
		out[i].Receipt = history[i].Receipt.Clone()
	}
	return out
}

func (s *GraphState) RetainExecution(previous *GraphState) {
	if previous == nil {
		return
	}
	cloned := previous.Clone()
	s.PreviousExecutions = cloned.PreviousExecutions
	s.PreviousExecutions = append(s.PreviousExecutions, GraphEvidence{
		ExecutionID: cloned.ExecutionID, DefinitionFingerprint: cloned.DefinitionFingerprint,
		NodeTemplates: cloned.NodeTemplates, LoopLimits: cloned.LoopLimits,
		LoopCounts: cloned.LoopCounts, Transitions: cloned.Transitions,
		Wait: cloned.Wait, Wake: cloned.Wake, WakeCount: cloned.WakeCount,
		WaitHistory: cloned.WaitHistory, Continuation: cloned.Continuation,
	})
}

type GraphResult struct {
	NodeID       string `yaml:"node_id" json:"node_id"`
	VisitID      string `yaml:"visit_id" json:"visit_id"`
	InvocationID string `yaml:"invocation_id" json:"invocation_id"`
	Verdict      string `yaml:"verdict,omitempty" json:"verdict,omitempty"`
	ExitCode     *int   `yaml:"exit_code,omitempty" json:"exit_code,omitempty"`
	Outcome      string `yaml:"outcome" json:"outcome"`
}

// GraphHandoff holds materialized evidence from one accepted executable. It
// is pending at a control cursor until bound to a DestinationVisitID. It is
// local execution state, not part of run history or public projections.
type GraphHandoff struct {
	ExecutionID        string             `yaml:"execution_id" json:"execution_id"`
	NodeID             string             `yaml:"node_id" json:"node_id"`
	VisitID            string             `yaml:"visit_id" json:"visit_id"`
	InvocationID       string             `yaml:"invocation_id" json:"invocation_id"`
	DestinationVisitID string             `yaml:"destination_visit_id,omitempty" json:"destination_visit_id,omitempty"`
	Check              *GraphCheckHandoff `yaml:"check,omitempty" json:"check,omitempty"`
	Summary            string             `yaml:"summary,omitempty" json:"summary,omitempty"`
	Verdict            string             `yaml:"verdict,omitempty" json:"verdict,omitempty"`
}

type GraphCheckHandoff struct {
	ExitCode        int    `yaml:"exit_code" json:"exit_code"`
	LogPath         string `yaml:"log_path,omitempty" json:"log_path,omitempty"`
	Output          string `yaml:"output,omitempty" json:"output,omitempty"`
	OutputAvailable bool   `yaml:"output_available,omitempty" json:"output_available,omitempty"`
	OutputTruncated bool   `yaml:"output_truncated,omitempty" json:"output_truncated,omitempty"`
}

func (h *GraphHandoff) Clone() *GraphHandoff {
	if h == nil {
		return nil
	}
	out := *h
	if h.Check != nil {
		check := *h.Check
		out.Check = &check
	}
	return &out
}

type GraphTransition struct {
	From      string `yaml:"from" json:"from"`
	Port      string `yaml:"port" json:"port"`
	To        string `yaml:"to" json:"to"`
	LoopCount int    `yaml:"loop_count,omitempty" json:"loop_count,omitempty"`
	Revision  uint64 `yaml:"revision" json:"revision"`
}

func (s *GraphState) Clone() *GraphState {
	if s == nil {
		return nil
	}
	out := *s
	out.NodeTemplates = maps.Clone(s.NodeTemplates)
	out.LoopLimits = maps.Clone(s.LoopLimits)
	out.LoopCounts = maps.Clone(s.LoopCounts)
	out.Transitions = slices.Clone(s.Transitions)
	out.Handoff = s.Handoff.Clone()
	out.Wait = s.Wait.Clone()
	out.Wake = s.Wake.Clone()
	out.WaitHistory = cloneWaitHistory(s.WaitHistory)
	out.PreviousExecutions = slices.Clone(s.PreviousExecutions)
	for i := range out.PreviousExecutions {
		out.PreviousExecutions[i].NodeTemplates = maps.Clone(s.PreviousExecutions[i].NodeTemplates)
		out.PreviousExecutions[i].LoopLimits = maps.Clone(s.PreviousExecutions[i].LoopLimits)
		out.PreviousExecutions[i].LoopCounts = maps.Clone(s.PreviousExecutions[i].LoopCounts)
		out.PreviousExecutions[i].Transitions = slices.Clone(s.PreviousExecutions[i].Transitions)
		out.PreviousExecutions[i].Wait = s.PreviousExecutions[i].Wait.Clone()
		out.PreviousExecutions[i].Wake = s.PreviousExecutions[i].Wake.Clone()
		out.PreviousExecutions[i].WaitHistory = cloneWaitHistory(s.PreviousExecutions[i].WaitHistory)
	}
	if s.LastResult != nil {
		result := *s.LastResult
		if result.ExitCode != nil {
			exitCode := *result.ExitCode
			result.ExitCode = &exitCode
		}
		out.LastResult = &result
	}
	return &out
}
