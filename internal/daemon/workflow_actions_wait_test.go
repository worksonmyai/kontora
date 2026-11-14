package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func workflowWaitActionFixture(t *testing.T) *resultFixture {
	t.Helper()
	f := workflowActionFixture(t)
	require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
	require.NoError(t, f.tkt.SetField("invocation_id", ""))
	state := f.tkt.GraphState.Clone()
	state.Version = 2
	state.Wait = &ticket.WorkflowWait{
		ID: "operator-wait", Kind: "timer", Phase: "pending", ExecutionID: state.ExecutionID,
		NodeID: f.tkt.Stage, VisitID: state.VisitID, InvocationID: f.rec.Invocation.ID,
		Until: time.Now().Add(time.Hour).UTC(), Deadline: time.Now().Add(2 * time.Hour).UTC(), Next: "continue",
	}
	require.NoError(t, f.tkt.SetField("graph_state", state))
	f.write(t)
	f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	return f
}

func TestWorkflowWaitOperatorPauseRetry(t *testing.T) {
	f := workflowWaitActionFixture(t)
	before := f.h.readTask(f.tkt.ID + ".md")
	f.d.workflowWaitWake = make(chan struct{}, 1)
	cancelled := false
	f.d.workflowProbes = map[string]*workflowProbe{f.tkt.ID: {cancel: func() { cancelled = true }}}
	require.NoError(t, f.d.PauseTicket(f.tkt.ID))
	assert.True(t, cancelled)
	paused := f.h.readTask(f.tkt.ID + ".md")
	assert.True(t, paused.GraphState.Wait.Disarmed)
	assert.Equal(t, before.GraphState.Wait.Deadline, paused.GraphState.Wait.Deadline)
	require.ErrorIs(t, f.d.RetryTicket(f.tkt.ID), web.ErrInvalidState, "a stopped ticket can still have a live probe")
	delete(f.d.workflowProbes, f.tkt.ID)
	require.NoError(t, f.d.RetryTicket(f.tkt.ID))
	got := f.h.readTask(f.tkt.ID + ".md")
	assert.Equal(t, before.GraphState.Wait, got.GraphState.Wait)
	assert.Equal(t, ticket.StatusPaused, got.Status)
	assert.False(t, f.d.queued[f.tkt.ID])
	assert.Len(t, f.d.workflowWaitWake, 1)
}

func TestWorkflowWaitOperatorBypassRefused(t *testing.T) {
	for _, action := range []string{"run", "move todo", "move in_progress", "move open", "move done", "schedule", "delete"} {
		t.Run(action, func(t *testing.T) {
			f := workflowWaitActionFixture(t)
			before := f.h.readTask(f.tkt.ID + ".md")
			var err error
			switch action {
			case "run":
				err = f.d.RunTicket(f.tkt.ID)
			case "schedule":
				err = f.d.ScheduleTicket(f.tkt.ID, web.ScheduleTicketRequest{ScheduledAt: ticket.FormatSchedule(time.Now().Add(time.Hour))})
			case "delete":
				err = f.d.DeleteTicket(f.tkt.ID)
			default:
				err = f.d.MoveTicket(f.tkt.ID, action[len("move "):])
			}
			require.ErrorIs(t, err, web.ErrInvalidState)
			assert.Equal(t, before, f.h.readTask(f.tkt.ID+".md"))
			assert.False(t, f.d.queued[f.tkt.ID])
		})
	}
}

func TestWorkflowWaitMoveDoesNotRetryUnobservedWait(t *testing.T) {
	f := workflowWaitActionFixture(t)
	// The watcher has not yet seen the wait. Move's fallback must not treat
	// a refusal from Run as permission for an implicit wait Retry.
	cached := f.tkt.Clone()
	cached.GraphState.Wait = nil
	f.d.tickets[f.tkt.ID] = newTicketState(cached, f.path)
	before := f.h.readTask(f.tkt.ID + ".md")
	require.ErrorIs(t, f.d.MoveTicket(f.tkt.ID, "todo"), web.ErrInvalidState)
	assert.Equal(t, before, f.h.readTask(f.tkt.ID+".md"))
}

func TestWorkflowWaitDeleteAndClaimUseDurableAuthority(t *testing.T) {
	f := workflowWaitActionFixture(t)
	cached := f.tkt.Clone()
	cached.GraphState.Wait = nil
	f.d.tickets[f.tkt.ID] = newTicketState(cached, f.path)
	require.ErrorIs(t, f.d.DeleteTicket(f.tkt.ID), web.ErrInvalidState)
	require.FileExists(t, f.path)
	for _, disarmed := range []bool{false, true} {
		fresh := f.h.readTask(f.tkt.ID + ".md")
		fresh.GraphState.Wait.Disarmed = disarmed
		require.ErrorContains(t, secondaryClaimRefusal(fresh), "active workflow wait")
	}
}

func TestWorkflowWaitCancelStopsProbeAndRevokesWait(t *testing.T) {
	f := workflowWaitActionFixture(t)
	cancelled := false
	f.d.workflowProbes = map[string]*workflowProbe{f.tkt.ID: {cancel: func() { cancelled = true }}}
	require.NoError(t, f.d.MoveTicket(f.tkt.ID, "cancelled"))
	got := f.h.readTask(f.tkt.ID + ".md")
	assert.True(t, cancelled)
	assert.Nil(t, got.GraphState.Wait)
	assert.False(t, got.GraphState.Continuation)
	require.Len(t, got.GraphState.WaitHistory, 1)
	assert.Equal(t, "operator-wait", got.GraphState.WaitHistory[0].ID)
	require.ErrorIs(t, f.d.RetryTicket(f.tkt.ID), web.ErrInvalidState, "cancel must not allow a new run before probe termination")
	require.ErrorIs(t, f.d.WorkflowRestart(f.tkt.ID), web.ErrInvalidState)
}

func TestWorkflowWaitRestartRetainsInvalidatedEvidence(t *testing.T) {
	f := workflowWaitActionFixture(t)
	before := f.h.readTask(f.tkt.ID + ".md")
	require.NoError(t, f.d.WorkflowRestart(f.tkt.ID))
	got := f.h.readTask(f.tkt.ID + ".md")
	assert.NotEqual(t, before.GraphState.ExecutionID, got.GraphState.ExecutionID)
	assert.Nil(t, got.GraphState.Wait)
	assert.False(t, got.GraphState.Continuation)
	require.Len(t, got.GraphState.PreviousExecutions, 1)
	assert.Equal(t, before.GraphState.Wait, got.GraphState.PreviousExecutions[0].Wait)
}

func TestWorkflowWaitProbeRecoveryAction(t *testing.T) {
	f := workflowWaitActionFixture(t)
	state := f.tkt.GraphState.Clone()
	state.Wait.Phase, state.Wait.Disarmed = "recovery_required", true
	require.NoError(t, f.tkt.SetField("graph_state", state))
	f.write(t)
	f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	f.d.workflowProbes = map[string]*workflowProbe{f.tkt.ID: {retained: true}}
	f.d.sem.used = 1
	require.ErrorIs(t, f.d.RetryTicket(f.tkt.ID), web.ErrInvalidState)
	require.ErrorIs(t, f.d.WorkflowRecover(f.tkt.ID, state.ExecutionID, state.Revision-1), web.ErrInvalidState)
	require.NoError(t, f.d.WorkflowRecover(f.tkt.ID, state.ExecutionID, state.Revision))
	got := f.h.readTask(f.tkt.ID + ".md")
	assert.Equal(t, "pending", got.GraphState.Wait.Phase)
	assert.True(t, got.GraphState.Wait.Disarmed)
	assert.Equal(t, state.Wait.ID, got.GraphState.Wait.ID)
	assert.Zero(t, f.d.sem.used)
	require.NoError(t, f.d.RetryTicket(f.tkt.ID))
	assert.False(t, f.h.readTask(f.tkt.ID+".md").GraphState.Wait.Disarmed)
}

func TestWorkflowWaitScheduleRefusesHandEditedOpenWait(t *testing.T) {
	f := workflowWaitActionFixture(t)
	require.NoError(t, f.tkt.SetField("status", "open"))
	require.NoError(t, f.tkt.SetSchedule(time.Now().Add(-time.Hour)))
	f.write(t)
	f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	f.d.promoteDueSchedules(time.Now())
	got := f.h.readTask(f.tkt.ID + ".md")
	assert.Equal(t, ticket.StatusOpen, got.Status)
	assert.NotEmpty(t, got.ScheduledAt)
	assert.False(t, f.d.queued[f.tkt.ID])
	require.NoError(t, f.d.ScheduleTicket(f.tkt.ID, web.ScheduleTicketRequest{Clear: true}))
}

func TestWorkflowWaitRestartAtControlWait(t *testing.T) {
	f := workflowWaitActionFixture(t)
	f.h.cfg.Pipelines[f.tkt.Pipeline] = config.PipelineDefinition{Graph: &config.WorkflowGraph{
		Version: 2, Entry: "approval", Nodes: []config.WorkflowNode{
			{ID: "approval", Type: "wait", Mode: "human"}, {ID: "end", Type: "end", Status: "done"},
		}, Edges: []config.WorkflowEdge{{From: config.WorkflowPort{Node: "approval", Port: "released"}, To: "end"}},
	}}
	require.NoError(t, f.d.WorkflowRestart(f.tkt.ID))
	got := f.h.readTask(f.tkt.ID + ".md")
	assert.Equal(t, 2, got.GraphState.Version)
	assert.Equal(t, ticket.StatusPaused, got.Status)
	require.NotNil(t, got.GraphState.Wait)
	assert.Equal(t, "human", got.GraphState.Wait.Kind)
	assert.NotEqual(t, "operator-wait", got.GraphState.Wait.ID)
}
