package app

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func workflowWaitFixture(t *testing.T) (*config.Config, *memRepo, *spyRuntime, *Service) {
	t.Helper()
	cfg, repo, runtime, svc := workflowFixture(t)
	tkt := repo.tickets["graph-1"].Ticket
	state := tkt.GraphState.Clone()
	state.Version = 2
	state.Wait = &ticket.WorkflowWait{
		ID: "wait-1", Kind: "timer", Phase: "pending", ExecutionID: state.ExecutionID,
		NodeID: tkt.Stage, VisitID: state.VisitID, InvocationID: "waiting-invocation",
		Until:    time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		Deadline: time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC), Next: "continue here",
	}
	require.NoError(t, tkt.SetField("graph_state", state))
	return cfg, repo, runtime, svc
}

func TestWorkflowWaitPauseAndRetry(t *testing.T) {
	_, repo, runtime, svc := workflowWaitFixture(t)
	before := repo.tickets["graph-1"].Ticket.Clone()
	result, err := svc.SetStatus("graph-1", ticket.StatusPaused)
	require.NoError(t, err, "pending waits are already paused but still need disarming")
	assert.Equal(t, "paused", result.Status)
	paused := repo.tickets["graph-1"].Ticket
	assert.True(t, paused.GraphState.Wait.Disarmed)
	assert.Equal(t, before.GraphState.Revision+1, paused.GraphState.Revision)
	result, err = svc.Retry("graph-1")
	require.NoError(t, err)
	got := repo.tickets["graph-1"].Ticket
	assert.Equal(t, "paused", result.Status)
	assert.Equal(t, ticket.StatusPaused, got.Status)
	assert.Equal(t, before.GraphState.Wait, got.GraphState.Wait)
	assert.Equal(t, before.GraphState.Revision+2, got.GraphState.Revision)
	assert.Equal(t, before.GraphState.LastResult, got.GraphState.LastResult)
	assert.Equal(t, before.GraphState.LoopCounts, got.GraphState.LoopCounts)
	assert.Equal(t, []string{"graph-1"}, runtime.cancelled)
}

func TestWorkflowWaitControlsCannotBypassAuthority(t *testing.T) {
	for _, action := range []string{"todo", "open", "done", "human_review", "init", "skip", "stage", "reentry"} {
		t.Run(action, func(t *testing.T) {
			cfg, repo, runtime, svc := workflowWaitFixture(t)
			before := repo.tickets["graph-1"].Ticket.Clone()
			beforeBytes, err := before.Marshal()
			require.NoError(t, err)
			switch action {
			case "init":
				_, err = svc.Init("graph-1", InitRequest{Status: "todo"})
			case "skip":
				_, err = svc.Skip("graph-1")
			case "stage":
				_, err = svc.SetStage("graph-1", "test")
			case "reentry":
				err = ValidateWorkflowReentry(cfg, before)
			default:
				_, err = svc.SetStatus("graph-1", ticket.Status(action))
			}
			require.ErrorIs(t, err, ErrInvalidState)
			afterBytes, marshalErr := repo.tickets["graph-1"].Ticket.Marshal()
			require.NoError(t, marshalErr)
			assert.Equal(t, beforeBytes, afterBytes)
			assert.Empty(t, repo.saved)
			assert.Empty(t, runtime.enqueued)
		})
	}
}

func TestWorkflowWaitRetryRequiresRecovery(t *testing.T) {
	for _, phase := range []string{"probing", "recovery_required", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			_, repo, runtime, svc := workflowWaitFixture(t)
			state := repo.tickets["graph-1"].Ticket.GraphState.Clone()
			state.Wait.Phase, state.Wait.Disarmed = phase, true
			require.NoError(t, repo.tickets["graph-1"].Ticket.SetField("graph_state", state))
			_, err := svc.Retry("graph-1")
			require.ErrorIs(t, err, ErrInvalidState)
			assert.ErrorContains(t, err, "workflow-recover")
			assert.Empty(t, repo.saved)
			assert.Empty(t, runtime.enqueued)
		})
	}
}

func TestWorkflowReadyContinuationReentry(t *testing.T) {
	cfg, repo, _, svc := workflowWaitFixture(t)
	tkt := repo.tickets["graph-1"].Ticket
	state := tkt.GraphState.Clone()
	state.Wait.Phase, state.Continuation = "ready", true
	require.NoError(t, tkt.SetField("graph_state", state))
	require.NoError(t, ValidateWorkflowReentry(cfg, tkt))
	_, err := svc.SetStatus("graph-1", ticket.StatusTodo)
	require.ErrorIs(t, err, ErrInvalidState, "a move is not a wait release")
	_, err = svc.SetStatus("graph-1", ticket.StatusPaused)
	require.NoError(t, err)
	require.ErrorIs(t, ValidateWorkflowReentry(cfg, repo.tickets["graph-1"].Ticket), ErrInvalidState)
	result, err := svc.Retry("graph-1")
	require.NoError(t, err)
	assert.Equal(t, "todo", result.Status)
	got := repo.tickets["graph-1"].Ticket
	assert.True(t, got.GraphState.Continuation)
	assert.Equal(t, "ready", got.GraphState.Wait.Phase)
	assert.Equal(t, "wait-1", got.GraphState.Wait.ID)
	assert.False(t, got.GraphState.Wait.Disarmed)
}

func TestWorkflowWaitCancellationInvalidatesAuthority(t *testing.T) {
	_, repo, runtime, svc := workflowWaitFixture(t)
	before := repo.tickets["graph-1"].Ticket.Clone()
	_, err := svc.SetStatus("graph-1", ticket.StatusCancelled)
	require.NoError(t, err)
	got := repo.tickets["graph-1"].Ticket
	assert.Nil(t, got.GraphState.Wait)
	assert.Nil(t, got.GraphState.Wake)
	assert.False(t, got.GraphState.Continuation)
	require.Len(t, got.GraphState.WaitHistory, 1)
	assert.Equal(t, *before.GraphState.Wait, got.GraphState.WaitHistory[0])
	assert.Equal(t, before.GraphState.Revision+1, got.GraphState.Revision)
	assert.Equal(t, []string{"graph-1"}, runtime.cancelled)
}

func TestWorkflowWaitRetryIdentityGuards(t *testing.T) {
	for _, change := range []string{"version", "execution", "visit", "node", "overflow", "invocation"} {
		t.Run(change, func(t *testing.T) {
			_, repo, _, svc := workflowWaitFixture(t)
			tkt := repo.tickets["graph-1"].Ticket
			state := tkt.GraphState.Clone()
			switch change {
			case "version":
				state.Version = 1
			case "execution":
				state.Wait.ExecutionID = "stale"
			case "visit":
				state.Wait.VisitID = "stale"
			case "node":
				state.Wait.NodeID = "test"
			case "overflow":
				state.Revision = math.MaxUint64
			case "invocation":
				require.NoError(t, tkt.SetField("invocation_id", "unrecovered"))
			}
			require.NoError(t, tkt.SetField("graph_state", state))
			_, err := svc.Retry("graph-1")
			require.ErrorIs(t, err, ErrInvalidState)
			assert.Empty(t, repo.saved)
		})
	}
}

func TestWorkflowWaitControlNodeRetry(t *testing.T) {
	cfg, repo, _, svc := workflowWaitFixture(t)
	cfg.Pipelines["graph"].Graph.Nodes = append(cfg.Pipelines["graph"].Graph.Nodes, config.WorkflowNode{ID: "gate", Type: "wait", Mode: "human"})
	tkt := repo.tickets["graph-1"].Ticket
	state := tkt.GraphState.Clone()
	state.Wait.NodeID, state.Wait.Kind, state.Wait.Disarmed = "gate", "human", true
	// An entry control wait has no executable node visit yet.
	state.Visit, state.VisitID, state.Wait.VisitID = 0, "", ""
	require.NoError(t, tkt.SetField("stage", "gate"))
	require.NoError(t, tkt.SetField("graph_state", state))
	_, err := svc.Retry("graph-1")
	require.NoError(t, err)
	assert.Equal(t, ticket.StatusPaused, repo.tickets["graph-1"].Ticket.Status)
	assert.False(t, repo.tickets["graph-1"].Ticket.GraphState.Wait.Disarmed)
}
