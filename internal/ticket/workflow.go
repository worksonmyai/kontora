package ticket

import (
	"maps"
	"slices"
)

// Ticket.Stage is the cursor.
type GraphState struct {
	Version               int               `yaml:"version" json:"version"`
	ExecutionID           string            `yaml:"execution_id" json:"execution_id"`
	Revision              uint64            `yaml:"revision" json:"revision"`
	DefinitionFingerprint string            `yaml:"definition_fingerprint" json:"definition_fingerprint"`
	VisitID               string            `yaml:"visit_id,omitempty" json:"visit_id,omitempty"`
	Visit                 uint64            `yaml:"visit" json:"visit"`
	LoopCounts            map[string]int    `yaml:"loop_counts,omitempty" json:"loop_counts,omitempty"`
	LastResult            *GraphResult      `yaml:"last_result,omitempty" json:"last_result,omitempty"`
	Transitions           []GraphTransition `yaml:"transitions,omitempty" json:"transitions,omitempty"`
}

type GraphResult struct {
	NodeID       string `yaml:"node_id" json:"node_id"`
	VisitID      string `yaml:"visit_id" json:"visit_id"`
	InvocationID string `yaml:"invocation_id" json:"invocation_id"`
	Verdict      string `yaml:"verdict,omitempty" json:"verdict,omitempty"`
	ExitCode     *int   `yaml:"exit_code,omitempty" json:"exit_code,omitempty"`
	Outcome      string `yaml:"outcome" json:"outcome"`
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
	out.LoopCounts = maps.Clone(s.LoopCounts)
	out.Transitions = slices.Clone(s.Transitions)
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
