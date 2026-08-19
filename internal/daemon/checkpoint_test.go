package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/tmux"
)

// errCompactTimeout stands in for the error the runner reports when a
// compaction it typed never lands.
var errCompactTimeout = errors.New("compaction did not finish within 5m0s")

func TestScanTranscript(t *testing.T) {
	tests := []struct {
		name           string
		file           string
		wantTokens     int
		wantHasContext bool
		wantManual     int
		wantPreTokens  int
	}{
		{
			name:           "last assistant entry wins",
			file:           "transcript_linear.jsonl",
			wantTokens:     5000,
			wantHasContext: true,
		},
		{
			name:           "a sidechain turn is not the context",
			file:           "transcript_sidechain.jsonl",
			wantTokens:     1205,
			wantHasContext: true,
		},
		{
			name:           "no usable usage",
			file:           "transcript_no_usage.jsonl",
			wantHasContext: false,
		},
		{
			name:           "only manual boundaries count",
			file:           "transcript_compacted.jsonl",
			wantTokens:     8503,
			wantHasContext: true,
			wantManual:     1,
			wantPreTokens:  121500,
		},
		{
			name:           "a half-written line is skipped",
			file:           "transcript_malformed.jsonl",
			wantTokens:     12,
			wantHasContext: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join("testdata", tt.file)
			got, err := scanTranscript(path)
			require.NoError(t, err)
			assert.Equal(t, tt.wantTokens, got.contextTokens)
			assert.Equal(t, tt.wantHasContext, got.hasContext)
			assert.Equal(t, tt.wantManual, got.manualCompacts)
			assert.Equal(t, tt.wantPreTokens, got.lastPreTokens)

			tokens, ok := lastContextTokens(path)
			assert.Equal(t, tt.wantTokens, tokens)
			assert.Equal(t, tt.wantHasContext, ok)
		})
	}
}

func TestLastContextTokensMissingFile(t *testing.T) {
	tokens, ok := lastContextTokens(filepath.Join(t.TempDir(), "absent.jsonl"))
	assert.Zero(t, tokens)
	assert.False(t, ok)
}

// checkpointHarness wires a controller to a temp sidecar and a fake Claude
// transcript the test can grow between decisions.
type checkpointHarness struct {
	t          *testing.T
	ctl        *checkpointController
	sidecar    string
	transcript string
}

func newCheckpointHarness(t *testing.T, threshold int) *checkpointHarness {
	t.Helper()
	dir := t.TempDir()
	configDir := filepath.Join(dir, "claude")
	sessionID := "11111111-2222-3333-4444-555555555555"
	projects := filepath.Join(configDir, "projects", "proj")
	require.NoError(t, os.MkdirAll(projects, 0o755))

	h := &checkpointHarness{
		t:          t,
		sidecar:    filepath.Join(dir, "logs", "tst-1", "code.0.checkpoints.jsonl"),
		transcript: filepath.Join(projects, sessionID+".jsonl"),
	}
	h.ctl = newCheckpointController(
		checkpointSetup{sidecar: h.sidecar, threshold: threshold, log: discardLogger()},
		map[string]string{"CLAUDE_CONFIG_DIR": configDir},
		sessionID,
	)
	h.ctl.poll = 5 * time.Millisecond
	h.ctl.quiet = 60 * time.Millisecond
	h.ctl.stuckQuiet = 600 * time.Millisecond
	h.ctl.staleWindow = 2 * time.Second
	return h
}

// idle asks the controller for a decision the way the runner does.
func (h *checkpointHarness) idle(ev tmux.IdleEvent) tmux.IdleDecision {
	h.t.Helper()
	return h.ctl.onIdle(h.t.Context(), ev)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// appendTranscript adds the assistant entry that ends a turn, with a context
// summing to tokens.
func (h *checkpointHarness) appendTranscript(tokens int) {
	h.t.Helper()
	h.append(h.transcript, `{"type":"assistant","isSidechain":false,"message":{"content":[{"type":"text","text":"done"}],"usage":{"input_tokens":0,"cache_read_input_tokens":`+
		strconv.Itoa(tokens)+`,"cache_creation_input_tokens":0}}}`+"\n")
}

// appendPrompt adds the user entry Claude writes when a prompt is submitted,
// which is all a turn holds until its first thinking block completes.
func (h *checkpointHarness) appendPrompt(text string) {
	h.t.Helper()
	h.append(h.transcript, `{"type":"user","isSidechain":false,"message":{"role":"user","content":"`+text+`"}}`+"\n")
}

// appendManualBoundary adds the compact_boundary a kontora-driven /compact
// leaves behind.
func (h *checkpointHarness) appendManualBoundary(preTokens int) {
	h.t.Helper()
	h.append(h.transcript, `{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"manual","preTokens":`+
		strconv.Itoa(preTokens)+`}}`+"\n")
}

func (h *checkpointHarness) phaseComplete(completed, next string) {
	h.t.Helper()
	require.NoError(h.t, cli.AppendCheckpointRecord(h.sidecar, cli.CheckpointRecord{
		Kind:           cli.CheckpointKindPhaseComplete,
		Time:           time.Now(),
		CompletedPhase: completed,
		NextPhase:      next,
	}))
}

func (h *checkpointHarness) append(path, line string) {
	h.t.Helper()
	require.NoError(h.t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(h.t, err)
	_, err = f.WriteString(line)
	require.NoError(h.t, err)
	require.NoError(h.t, f.Close())
}

// outcomes reads back every outcome record the controller wrote.
func (h *checkpointHarness) outcomes() []cli.CheckpointRecord {
	h.t.Helper()
	data, err := os.ReadFile(h.sidecar)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(h.t, err)

	var out []cli.CheckpointRecord
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec cli.CheckpointRecord
		require.NoError(h.t, json.Unmarshal([]byte(line), &rec))
		if rec.Kind == cli.CheckpointKindOutcome {
			out = append(out, rec)
		}
	}
	return out
}

func TestCheckpointControllerDecisions(t *testing.T) {
	tests := []struct {
		name string
		// setup prepares the sidecar and the transcript before the first idle.
		setup       func(h *checkpointHarness)
		threshold   int
		wantAction  tmux.IdleAction
		wantPrompt  string
		wantCompact string
		wantOutcome string
		wantTokens  int
	}{
		{
			name:       "no checkpoint finishes the run",
			threshold:  1000,
			setup:      func(h *checkpointHarness) { h.appendTranscript(50000) },
			wantAction: tmux.IdleFinish,
		},
		{
			name:      "context within the threshold is skipped",
			threshold: 100000,
			setup: func(h *checkpointHarness) {
				h.appendTranscript(40000)
				h.phaseComplete("Phase 1: a", "Phase 2: b")
			},
			wantAction:  tmux.IdlePrompt,
			wantPrompt:  "Continue with Phase 2: b.",
			wantOutcome: cli.CheckpointOutcomeSkipped,
			wantTokens:  40000,
		},
		{
			name:      "context over the threshold compacts",
			threshold: 30000,
			setup: func(h *checkpointHarness) {
				h.appendTranscript(120000)
				h.phaseComplete("Phase 1: a", "Phase 2: b")
			},
			wantAction:  tmux.IdleCompact,
			wantPrompt:  "Continue with Phase 2: b.",
			wantCompact: "the named next phase: Phase 2: b.",
		},
		{
			name:      "an unreadable transcript is skipped",
			threshold: 1000,
			setup: func(h *checkpointHarness) {
				h.phaseComplete("Phase 1: a", "Phase 2: b")
			},
			wantAction:  tmux.IdlePrompt,
			wantPrompt:  "Continue with Phase 2: b.",
			wantOutcome: cli.CheckpointOutcomeSkipped,
		},
		{
			name:      "a transcript with no usage is skipped",
			threshold: 1000,
			setup: func(h *checkpointHarness) {
				h.append(h.transcript, `{"type":"assistant","isSidechain":false,"message":{"content":[]}}`+"\n")
				h.phaseComplete("Phase 1: a", "Phase 2: b")
			},
			wantAction:  tmux.IdlePrompt,
			wantOutcome: cli.CheckpointOutcomeSkipped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCheckpointHarness(t, tt.threshold)
			tt.setup(h)

			got := h.idle(tmux.IdleEvent{})
			assert.Equal(t, tt.wantAction, got.Action)
			if tt.wantPrompt != "" {
				assert.Contains(t, got.Continuation, tt.wantPrompt)
				assert.Contains(t, got.Continuation, "inspect git status")
			}
			if tt.wantCompact != "" {
				assert.Contains(t, got.CompactInstructions, tt.wantCompact)
			}

			outcomes := h.outcomes()
			if tt.wantOutcome == "" {
				assert.Empty(t, outcomes)
				return
			}
			require.Len(t, outcomes, 1)
			assert.Equal(t, tt.wantOutcome, outcomes[0].Outcome)
			assert.Equal(t, tt.wantTokens, outcomes[0].ContextTokens)
			assert.Equal(t, tt.threshold, outcomes[0].Threshold)
			assert.Equal(t, "Phase 1: a", outcomes[0].CompletedPhase)
		})
	}
}

func TestCheckpointControllerCompactionOutcome(t *testing.T) {
	tests := []struct {
		name string
		// landed adds the compact boundary a real compaction leaves behind.
		landed      bool
		compactErr  error
		wantOutcome string
		wantPre     int
		wantError   string
	}{
		{
			name:        "compaction lands",
			landed:      true,
			wantOutcome: cli.CheckpointOutcomeCompacted,
			wantPre:     121500,
		},
		{
			name:        "the wait times out but the boundary is there",
			landed:      true,
			compactErr:  errCompactTimeout,
			wantOutcome: cli.CheckpointOutcomeCompacted,
			wantPre:     121500,
		},
		{
			name:        "neither the hook nor a boundary",
			compactErr:  errCompactTimeout,
			wantOutcome: cli.CheckpointOutcomeFailed,
			wantError:   "did not finish within",
		},
		{
			name:        "the runner reported nothing but no boundary appeared",
			wantOutcome: cli.CheckpointOutcomeFailed,
			wantError:   "no compact boundary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCheckpointHarness(t, 30000)
			h.appendTranscript(120000)
			h.phaseComplete("Phase 1: a", "Phase 2: b")

			first := h.idle(tmux.IdleEvent{})
			require.Equal(t, tmux.IdleCompact, first.Action)

			if tt.landed {
				h.appendManualBoundary(121500)
			}
			// The run is over after the boundary: the second idle carries no new
			// checkpoint, so the controller settles the compaction and finishes.
			second := h.idle(tmux.IdleEvent{CompactErr: tt.compactErr})
			assert.Equal(t, tmux.IdleFinish, second.Action)

			outcomes := h.outcomes()
			require.Len(t, outcomes, 1)
			assert.Equal(t, tt.wantOutcome, outcomes[0].Outcome)
			assert.Equal(t, 120000, outcomes[0].ContextTokens)
			assert.Equal(t, tt.wantPre, outcomes[0].PreTokens)
			if tt.wantError != "" {
				assert.Contains(t, outcomes[0].Error, tt.wantError)
			}
		})
	}
}

func TestCheckpointControllerStaleWake(t *testing.T) {
	h := newCheckpointHarness(t, 100000)
	h.appendTranscript(1000)
	h.phaseComplete("Phase 1: a", "Phase 2: b")

	require.Equal(t, tmux.IdlePrompt, h.idle(tmux.IdleEvent{}).Action)

	// A signal latched by the turn that just ended wakes the controller while the
	// agent is still working on the next phase. It must not finish the run.
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.appendTranscript(2000)
		time.Sleep(20 * time.Millisecond)
		h.phaseComplete("Phase 2: b", "Phase 3: c")
	}()

	got := h.idle(tmux.IdleEvent{})
	assert.Equal(t, tmux.IdlePrompt, got.Action)
	assert.Contains(t, got.Continuation, "Continue with Phase 3: c.")
}

func TestCheckpointControllerFinishesWhenTheAgentIsDone(t *testing.T) {
	tests := []struct {
		name string
		// aged moves the last continuation out of the stale-wake window, which
		// is what a wake arriving a whole phase later looks like.
		aged bool
	}{
		{name: "a wake soon after the continuation waits for the transcript to go quiet"},
		{name: "a wake a phase later is taken at face value", aged: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCheckpointHarness(t, 100000)
			h.appendTranscript(1000)
			h.phaseComplete("Phase 1: a", "Phase 2: b")

			require.Equal(t, tmux.IdlePrompt, h.idle(tmux.IdleEvent{}).Action)
			if tt.aged {
				h.ctl.deliveredAt = time.Now().Add(-time.Hour)
			}

			started := time.Now()
			assert.Equal(t, tmux.IdleFinish, h.idle(tmux.IdleEvent{}).Action)
			if tt.aged {
				assert.Less(t, time.Since(started), h.ctl.quiet, "an unsuspicious wake must not poll")
			} else {
				assert.GreaterOrEqual(t, time.Since(started), h.ctl.quiet)
			}
		})
	}
}

func TestCheckpointControllerPerRunCap(t *testing.T) {
	h := newCheckpointHarness(t, 100000)
	h.appendTranscript(1000)
	h.ctl.delivered = maxCheckpointsPerRun
	h.phaseComplete("Phase 21: a", "Phase 22: b")

	got := h.idle(tmux.IdleEvent{})
	assert.Equal(t, tmux.IdleFinish, got.Action)

	outcomes := h.outcomes()
	require.Len(t, outcomes, 1)
	assert.Equal(t, cli.CheckpointOutcomeSkipped, outcomes[0].Outcome)
	assert.Contains(t, outcomes[0].Error, "checkpoint cap")
}

func TestCheckpointControllerResumeSkipsHandledBoundaries(t *testing.T) {
	tests := []struct {
		name string
		// died leaves the sidecar as a daemon killed between taking the boundary
		// on and recording its outcome leaves it.
		died bool
	}{
		{name: "the previous daemon recorded the outcome"},
		{name: "the previous daemon died before the outcome", died: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCheckpointHarness(t, 100000)
			h.appendTranscript(1000)
			h.phaseComplete("Phase 1: a", "Phase 2: b")
			require.Equal(t, tmux.IdlePrompt, h.idle(tmux.IdleEvent{}).Action)

			if tt.died {
				dropOutcomes(t, h.sidecar)
			}

			// A daemon restart builds a fresh controller over the same sidecar. The
			// boundary the previous one answered must not be replayed.
			resumed := newCheckpointController(
				checkpointSetup{sidecar: h.sidecar, threshold: 100000, log: discardLogger()},
				map[string]string{},
				"",
			)
			assert.Equal(t, 1, resumed.consumed)
			assert.Equal(t, tmux.IdleFinish, resumed.onIdle(t.Context(), tmux.IdleEvent{}).Action)
		})
	}
}

// dropOutcomes rewrites the sidecar without its outcome records, which is what
// a daemon killed between taking a boundary on and settling it leaves behind.
func dropOutcomes(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var kept []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var rec cli.CheckpointRecord
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if rec.Kind != cli.CheckpointKindOutcome {
			kept = append(kept, line)
		}
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644))
}

// TestCheckpointControllerWaitsOutAWorkingAgent covers the wake that arrives
// while the agent is still on the phase it was just given: the transcript stops
// growing during a thinking block, and finishing there would type /exit into a
// working agent.
func TestCheckpointControllerWaitsOutAWorkingAgent(t *testing.T) {
	h := newCheckpointHarness(t, 100000)
	h.appendTranscript(1000)
	h.phaseComplete("Phase 1: a", "Phase 2: b")
	require.Equal(t, tmux.IdlePrompt, h.idle(tmux.IdleEvent{}).Action)

	// The continuation is in the transcript and nothing follows it for several
	// quiet windows, which is what a long thinking block looks like.
	h.appendPrompt("Continue with Phase 2: b.")
	go func() {
		time.Sleep(10 * h.ctl.quiet)
		h.appendTranscript(2000)
		h.phaseComplete("Phase 2: b", "Phase 3: c")
	}()

	got := h.idle(tmux.IdleEvent{})
	assert.Equal(t, tmux.IdlePrompt, got.Action)
	assert.Contains(t, got.Continuation, "Continue with Phase 3: c.")
}

func TestCheckpointControllerStopsWaitingOnAStuckAgent(t *testing.T) {
	h := newCheckpointHarness(t, 100000)
	h.appendTranscript(1000)
	h.phaseComplete("Phase 1: a", "Phase 2: b")
	require.Equal(t, tmux.IdlePrompt, h.idle(tmux.IdleEvent{}).Action)

	// The agent took the continuation and then wrote nothing at all.
	h.appendPrompt("Continue with Phase 2: b.")

	started := time.Now()
	assert.Equal(t, tmux.IdleFinish, h.idle(tmux.IdleEvent{}).Action)
	assert.GreaterOrEqual(t, time.Since(started), h.ctl.stuckQuiet)
}

func TestCheckpointControllerReturnsOnCancel(t *testing.T) {
	h := newCheckpointHarness(t, 100000)
	h.appendTranscript(1000)
	h.phaseComplete("Phase 1: a", "Phase 2: b")
	require.Equal(t, tmux.IdlePrompt, h.idle(tmux.IdleEvent{}).Action)
	h.appendPrompt("Continue with Phase 2: b.")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	started := time.Now()
	assert.Equal(t, tmux.IdleFinish, h.ctl.onIdle(ctx, tmux.IdleEvent{}).Action)
	assert.Less(t, time.Since(started), h.ctl.stuckQuiet, "a cancelled run must not sit out the wait")
}
