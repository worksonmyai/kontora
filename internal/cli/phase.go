package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// CheckpointFileEnvVar names the sidecar JSONL a checkpointing run exports to
// its agent. It is set only for runs whose agent has a positive
// checkpoint_compaction_tokens, so its absence means the run does not
// checkpoint.
const CheckpointFileEnvVar = "KONTORA_CHECKPOINT_FILE"

// Kinds of checkpoint sidecar record. The agent writes phase_complete; the
// daemon writes accepted the moment it takes that boundary on, and outcome once
// it knows how the boundary went. The accepted marker is what a daemon that
// died between the two reads on resume, so it does not replay the boundary.
const (
	CheckpointKindPhaseComplete = "phase_complete"
	CheckpointKindAccepted      = "accepted"
	CheckpointKindOutcome       = "outcome"
)

// Outcomes the daemon records for a phase boundary.
const (
	CheckpointOutcomeSkipped   = "skipped"
	CheckpointOutcomeCompacted = "compacted"
	CheckpointOutcomeFailed    = "failed"
)

// CheckpointRecord is one line of the checkpoint sidecar.
type CheckpointRecord struct {
	Kind           string    `json:"kind"`
	Time           time.Time `json:"time"`
	CompletedPhase string    `json:"completed_phase,omitempty"`
	NextPhase      string    `json:"next_phase,omitempty"`
	Outcome        string    `json:"outcome,omitempty"`
	ContextTokens  int       `json:"context_tokens,omitempty"`
	Threshold      int       `json:"threshold,omitempty"`
	PreTokens      int       `json:"pre_tokens,omitempty"`
	Error          string    `json:"error,omitempty"`
}

// AppendCheckpointRecord appends rec to the sidecar at path as one JSON line.
func AppendCheckpointRecord(path string, rec CheckpointRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// PhaseComplete records a phase boundary for the daemon to act on and tells
// the agent what to do next.
//
// An empty path means the run does not checkpoint. That is not an error: the
// same prompt can reach an agent in a run with no threshold, and failing there
// would derail work that is otherwise fine.
func PhaseComplete(path, completed, next string, now time.Time, out io.Writer) error {
	if completed == "" || next == "" {
		return fmt.Errorf("--completed and --next are both required")
	}
	if path == "" {
		fmt.Fprintf(out, "Checkpoint compaction is not enabled for this run, nothing recorded.\nContinue with %s yourself.\n", next)
		return nil
	}

	err := AppendCheckpointRecord(path, CheckpointRecord{
		Kind:           CheckpointKindPhaseComplete,
		Time:           now,
		CompletedPhase: completed,
		NextPhase:      next,
	})
	if err != nil {
		return fmt.Errorf("appending checkpoint record: %w", err)
	}

	fmt.Fprintf(out, "Checkpoint recorded for %s.\nEnd your turn now. Do not start %s: kontora compacts the context if it needs to and then prompts you to continue.\n", completed, next)
	return nil
}
