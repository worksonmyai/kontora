package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func workflowEventFixture(t *testing.T, kind string, terminal bool) *resultFixture {
	t.Helper()
	h := newHarness(t)
	target := "work"
	if terminal {
		target = "done"
	}
	graph := &config.WorkflowGraph{Version: 2, Entry: "gate", Nodes: []config.WorkflowNode{
		{ID: "gate", Type: "wait", Mode: kind},
		{ID: "work", Type: "agent", Stage: "step1", Agent: "agent1"},
		{ID: "done", Type: "end", Status: "done"},
		{ID: "stop", Type: "end", Status: "paused"},
	}, Edges: []config.WorkflowEdge{
		{From: config.WorkflowPort{Node: "gate", Port: "released"}, To: target},
		{From: config.WorkflowPort{Node: "work", Port: "complete"}, To: "done"},
		{From: config.WorkflowPort{Node: "work", Port: "failed"}, To: "stop"},
	}}
	if kind == "deployment" {
		graph.Nodes[0].Environment = "production"
	}
	h.cfg.Pipelines["release"] = config.PipelineDefinition{Graph: graph}
	h.cfg.AutoPickUp = new(false)
	path := h.writeTicket("tst-release.md", h.taskMD("tst-release", "paused", "release"))
	tkt := h.readTask("tst-release.md")
	require.NoError(t, tkt.SetField("stage", "gate"))
	require.NoError(t, tkt.SetField("branch", "kontora/release"))
	fingerprint, err := workflowFingerprint(h.cfg, tkt, graph)
	require.NoError(t, err)
	w := &ticket.WorkflowWait{ID: "wait-release", Kind: kind, Phase: "pending", ExecutionID: "execution-release", NodeID: "gate", RequestedAt: time.Now(), Branch: tkt.Branch}
	if kind == "deployment" {
		path := filepath.Join(h.wtDir, "release")
		eventTestGit(t, h.repoDir, "worktree", "add", "-b", tkt.Branch, path, "HEAD")
		w.Worktree = path
		w.Repository, w.RequiredCommit, err = worktree.DeploymentEvidence(h.repoDir, path)
		require.NoError(t, err)
		w.Environment = "production"
	}
	require.NoError(t, tkt.SetField("graph_state", &ticket.GraphState{Version: 2, ExecutionID: w.ExecutionID, Revision: 7, DefinitionFingerprint: fingerprint, Wait: w}))
	d := h.newDaemon(h.cfg)
	f := &resultFixture{h: h, d: d, path: path, tkt: tkt}
	f.write(t)
	d.tickets[tkt.ID] = newTicketState(tkt, path)
	return f
}

func eventTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func eventHumanRequest(f *resultFixture) web.WorkflowReleaseRequest {
	return web.WorkflowReleaseRequest{ExecutionID: f.tkt.GraphState.ExecutionID, WaitID: f.tkt.GraphState.Wait.ID,
		Revision: f.tkt.GraphState.Revision, RequestID: "approval-1", Reason: "Approved"}
}

func TestWorkflowHumanRelease(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "continuation", true: "control completion"}[terminal], func(t *testing.T) {
			f := workflowEventFixture(t, "human", terminal)
			req := eventHumanRequest(f)
			req.Operator = "operator-alice"
			receipt, err := f.d.WorkflowRelease(f.tkt.ID, req)
			require.NoError(t, err)
			require.NotEmpty(t, receipt.ID)
			assert.Equal(t, "operator-alice", receipt.Operator)
			got := f.h.readTask("tst-release.md")
			assert.Equal(t, req.Revision+1, got.GraphState.Revision)
			assert.Nil(t, got.GraphState.Wait)
			require.Len(t, got.GraphState.WaitHistory, 1)
			assert.Equal(t, receipt, got.GraphState.WaitHistory[0].Receipt)
			assert.Empty(t, got.History, "a control wait is not an agent invocation")
			assert.Empty(t, got.InvocationID)
			if terminal {
				assert.Equal(t, ticket.StatusDone, got.Status)
				assert.False(t, got.CompletedAt.IsZero())
				_, err := os.Stat(controlCleanupPath(f.h.cfg, got.ID))
				require.NoError(t, err)
			} else {
				assert.Equal(t, ticket.StatusTodo, got.Status)
				assert.True(t, got.GraphState.Continuation)
				assert.True(t, f.d.queued[got.ID], "explicit requeue even with automatic pickup disabled")
			}
			stored, err := readWorkflowEvent(f.d.workflowEventPath("human", req.RequestID))
			require.NoError(t, err)
			require.Len(t, stored.Applications, 1)
			assert.True(t, stored.Applications[0].Applied)
			// Idempotency is receipt-based, not current ticket lifecycle-based.
			req.Operator = "operator-bob"
			retried, err := f.d.WorkflowRelease(f.tkt.ID, req)
			require.NoError(t, err)
			assert.Equal(t, receipt, retried)
			assert.Equal(t, got.GraphState, f.h.readTask("tst-release.md").GraphState)
			conflict := req
			conflict.Reason = "different payload"
			_, err = f.d.WorkflowRelease(f.tkt.ID, conflict)
			require.ErrorIs(t, err, web.ErrInvalidState)
			conflict = req
			conflict.RequestID = "new-stale-request"
			_, err = f.d.WorkflowRelease(f.tkt.ID, conflict)
			require.ErrorIs(t, err, web.ErrInvalidState)
		})
	}
}

func TestWorkflowHandoffAcrossDeclaredWait(t *testing.T) {
	for _, port := range []string{"released", "timeout"} {
		t.Run(port, func(t *testing.T) {
			h := newHarness(t)
			edge := func(from, port, to string) config.WorkflowEdge {
				return config.WorkflowEdge{From: config.WorkflowPort{Node: from, Port: port}, To: to}
			}
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{Version: 2, Entry: "tests", Nodes: []config.WorkflowNode{
				{ID: "tests", Type: "check", Command: "true", Timeout: config.Duration{Duration: time.Minute}},
				{ID: "gate", Type: "wait", Mode: "human", Timeout: config.Duration{Duration: time.Hour}},
				{ID: "repair", Type: "agent", Stage: "step1", Agent: "agent1", Handoff: config.WorkflowHandoffPrevious},
				{ID: "stop", Type: "end", Status: "paused"},
			}, Edges: []config.WorkflowEdge{
				edge("tests", "pass", "stop"), edge("tests", "fail", "gate"),
				edge("gate", "released", "repair"), edge("gate", "timeout", "repair"),
				edge("repair", "complete", "stop"), edge("repair", "failed", "stop"),
			}}}
			h.cfg = loadWorkflowTestConfig(t, h.cfg)
			clock := newWorkflowWaitTestClock()
			var checkLog string
			d := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				checkLog = p.LogFile
				return workflowWaitTestResult(clock, 1), os.WriteFile(checkLog, []byte("before wait"), 0o600)
			}))
			const id = "tst-gated-handoff"
			name := id + ".md"
			path := h.writeTicket(name, h.taskMD(id, "todo", "graph"))
			d.tickets[id] = newTicketState(h.readTask(name), path)
			ctx := context.Background()
			d.runTicket(ctx, id)
			parked := h.readTask(name)
			require.NotNil(t, parked.GraphState.Wait)
			pending := parked.GraphState.Handoff
			require.NotNil(t, pending)
			require.Empty(t, pending.DestinationVisitID)
			require.Equal(t, "before wait", pending.Check.Output)
			require.NoError(t, os.Remove(checkLog))
			if port == "released" {
				req := eventHumanRequest(&resultFixture{tkt: parked})
				_, err := d.WorkflowRelease(id, req)
				require.NoError(t, err)
				event, err := readWorkflowEvent(d.workflowEventPath("human", req.RequestID))
				require.NoError(t, err)
				require.Equal(t, pending.Check, event.Applications[0].After.State.Handoff.Check)
			} else {
				clock.Advance(time.Hour)
				d.reconcileWorkflowWaits(ctx)
			}
			ready := h.readTask(name)
			require.Equal(t, ticket.StatusTodo, ready.Status)
			require.Equal(t, "repair", ready.Stage)
			require.Equal(t, port, ready.GraphState.Transitions[len(ready.GraphState.Transitions)-1].Port)
			expected := pending.Clone()
			expected.DestinationVisitID = ready.GraphState.VisitID
			require.Equal(t, expected, ready.GraphState.Handoff)
		})
	}
}

func TestWorkflowReleaseRegistersNextControlWait(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	graph := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph
	graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "second-gate", Type: "wait", Mode: "human"})
	graph.Edges[0].To = "second-gate"
	graph.Edges = append(graph.Edges, config.WorkflowEdge{From: config.WorkflowPort{Node: "second-gate", Port: "released"}, To: "done"})
	fingerprint, err := workflowFingerprint(f.h.cfg, f.tkt, graph)
	require.NoError(t, err)
	s := f.tkt.GraphState.Clone()
	s.DefinitionFingerprint = fingerprint
	require.NoError(t, f.tkt.SetField("graph_state", s))
	f.write(t)
	req := eventHumanRequest(f)
	receipt, err := f.d.WorkflowRelease(f.tkt.ID, req)
	require.NoError(t, err)
	got := f.h.readTask("tst-release.md")
	assert.Equal(t, ticket.StatusPaused, got.Status)
	assert.Equal(t, "second-gate", got.Stage)
	require.NotNil(t, got.GraphState.Wait)
	assert.NotEqual(t, req.WaitID, got.GraphState.Wait.ID)
	assert.Equal(t, "second-gate", got.GraphState.Wait.NodeID)
	assert.Equal(t, "pending", got.GraphState.Wait.Phase)
	require.Len(t, got.GraphState.WaitHistory, 1)
	assert.Equal(t, receipt, got.GraphState.WaitHistory[0].Receipt)
	assert.Empty(t, got.History)
	_, err = f.d.WorkflowRelease(f.tkt.ID, req)
	require.NoError(t, err)
	assert.Equal(t, got.GraphState, f.h.readTask("tst-release.md").GraphState)
}

func TestWorkflowReleaseRefusals(t *testing.T) {
	for _, change := range []string{"execution", "wait", "revision", "fingerprint", "disarmed", "recovery", "timer", "running", "invocation", "journal", "expired", "wrong kind", "retained runner"} {
		t.Run(change, func(t *testing.T) {
			f := workflowEventFixture(t, "human", false)
			req := eventHumanRequest(f)
			s := f.tkt.GraphState
			switch change {
			case "execution":
				req.ExecutionID = "old"
			case "wait":
				req.WaitID = "old"
			case "revision":
				req.Revision--
			case "fingerprint":
				s.DefinitionFingerprint = "changed"
			case "disarmed":
				s.Wait.Disarmed = true
			case "recovery":
				s.Wait.Phase = "recovery_required"
			case "timer":
				s.Wait.Kind = "timer"
			case "running":
				require.NoError(t, f.tkt.SetField("status", "in_progress"))
			case "invocation":
				require.NoError(t, f.tkt.SetField("invocation_id", "old"))
			case "journal":
				require.NoError(t, os.MkdirAll(filepath.Dir(executionPath(f.h.cfg, f.tkt.ID)), 0700))
				require.NoError(t, os.WriteFile(executionPath(f.h.cfg, f.tkt.ID), []byte("{}"), 0600))
			case "expired":
				s.Wait.Deadline = time.Now().Add(-time.Minute)
			case "wrong kind":
				s.Wait.Kind = "deployment"
			case "retained runner":
				f.d.running[f.tkt.ID] = func() {}
			}
			require.NoError(t, f.tkt.SetField("graph_state", s))
			f.write(t)
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			_, err = f.d.WorkflowRelease(f.tkt.ID, req)
			require.ErrorIs(t, err, web.ErrInvalidState)
			after, err := os.ReadFile(f.path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			_, err = os.Stat(f.d.workflowEventPath("human", req.RequestID))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestWorkflowReceiptPersistenceFailure(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	require.NoError(t, os.WriteFile(f.d.workflowEventsDir(), []byte("not a directory"), 0600))
	_, err := f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
	require.Error(t, err)
	assert.Equal(t, f.tkt.GraphState, f.h.readTask("tst-release.md").GraphState)
	assert.False(t, f.d.queued[f.tkt.ID])
}

func TestWorkflowReceiptConcurrentRetries(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	req := eventHumanRequest(f)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			receipt, err := f.d.WorkflowRelease(f.tkt.ID, req)
			assert.NoError(t, err)
			assert.NotNil(t, receipt)
		})
	}
	wg.Wait()
	got := f.h.readTask("tst-release.md")
	assert.Len(t, got.GraphState.WaitHistory, 1)
	assert.Len(t, got.GraphState.Transitions, 1)
	assert.Equal(t, req.Revision+1, got.GraphState.Revision)
}

func TestWorkflowReceiptReplayBoundaries(t *testing.T) {
	for _, boundary := range []string{"acceptance", "ticket saved", "ticket changed", "execution replaced", "wait replaced", "body edit"} {
		t.Run(boundary, func(t *testing.T) {
			f := workflowEventFixture(t, "human", false)
			req := eventHumanRequest(f)
			receipt := &ticket.WorkflowReceipt{ID: workflowEventKey("human", req.RequestID), RequestID: req.RequestID, Reason: req.Reason, AcceptedAt: time.Now().UTC()}
			application, err := f.d.releaseApplication(f.tkt, f.path, receipt)
			require.NoError(t, err)
			event := &workflowEvent{Version: 1, Kind: "human", Human: &req, TicketID: f.tkt.ID, Receipt: receipt, Applications: []*workflowEventApplication{application}}
			path := f.d.workflowEventPath("human", req.RequestID)
			require.NoError(t, saveWorkflowEvent(path, event))
			if boundary == "ticket saved" {
				require.NoError(t, f.d.applyWorkflowEvent(event, application))
			}
			if boundary == "ticket changed" || boundary == "execution replaced" || boundary == "wait replaced" {
				s := f.tkt.GraphState.Clone()
				switch boundary {
				case "ticket changed":
					s.Revision++
					s.Wait.Disarmed = true
				case "execution replaced":
					s.ExecutionID = "new"
					s.Wait.ExecutionID = "new"
				case "wait replaced":
					s.Wait.ID = "new"
				}
				require.NoError(t, f.tkt.SetField("graph_state", s))
				f.write(t)
			}
			if boundary == "body edit" {
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, []byte("\nOperator body edit\n")...), 0600))
			}
			// New daemon, empty in-memory cache: reconciliation must fresh-read.
			d := f.h.newDaemon(f.h.cfg)
			d.reconcileWorkflowEvents()
			d.reconcileWorkflowEvents()
			got := f.h.readTask("tst-release.md")
			if boundary == "ticket changed" || boundary == "execution replaced" || boundary == "wait replaced" {
				assert.Equal(t, ticket.StatusPaused, got.Status)
				assert.Empty(t, got.GraphState.WaitHistory)
				return
			}
			assert.Equal(t, ticket.StatusTodo, got.Status)
			assert.Len(t, got.GraphState.WaitHistory, 1)
			assert.Len(t, got.GraphState.Transitions, 1)
			assert.True(t, got.GraphState.Continuation)
			if boundary == "body edit" {
				assert.Contains(t, got.Body, "Operator body edit")
			}
		})
	}
}

func TestDeploymentReceiptHistoricalAndLive(t *testing.T) {
	for _, order := range []string{"event first", "wait first"} {
		t.Run(order, func(t *testing.T) {
			f := workflowEventFixture(t, "deployment", false)
			req := web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "deploy-1"}
			if order == "event first" {
				require.NoError(t, os.Remove(f.path))
				delete(f.d.tickets, f.tkt.ID)
			}
			receipt, err := f.d.DeploymentRelease(req)
			require.NoError(t, err)
			assert.Equal(t, req.Commit, receipt.Commit)
			assert.Empty(t, receipt.Operator, "shared-token/open access is not a named person")
			if order == "event first" {
				f.write(t)
				f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
				f.d.reconcileWorkflowEvents()
			}
			got := f.h.readTask("tst-release.md")
			assert.Equal(t, ticket.StatusTodo, got.Status)
			require.Len(t, got.GraphState.WaitHistory, 1)
			assert.Equal(t, receipt, got.GraphState.WaitHistory[0].Receipt)
			retried, err := f.d.DeploymentRelease(req)
			require.NoError(t, err)
			assert.Equal(t, receipt, retried)
			req.Environment = "staging"
			_, err = f.d.DeploymentRelease(req)
			require.ErrorIs(t, err, web.ErrInvalidState)
			// A fresh occurrence in the same execution can consume the same
			// historical evidence, but cannot consume its old application.
			s := f.tkt.GraphState.Clone()
			s.Revision = got.GraphState.Revision + 1
			s.Wait.ID = "later-wait"
			s.Wait.RequestedAt = time.Now().Add(time.Hour)
			require.NoError(t, f.tkt.SetField("graph_state", s))
			f.write(t)
			f.d.reconcileWorkflowEvents()
			got = f.h.readTask("tst-release.md")
			require.Len(t, got.GraphState.WaitHistory, 1)
			assert.Equal(t, "later-wait", got.GraphState.WaitHistory[0].ID)
			assert.Equal(t, receipt, got.GraphState.WaitHistory[0].Receipt)
		})
	}
}

func TestDeploymentReceiptDescendantAndUnavailableRetry(t *testing.T) {
	f := workflowEventFixture(t, "deployment", false)
	required := f.tkt.GraphState.Wait.RequiredCommit
	eventTestGit(t, f.h.repoDir, "commit", "--allow-empty", "-m", "deployed descendant")
	deployed := eventTestGit(t, f.h.repoDir, "rev-parse", "HEAD")
	require.NotEqual(t, required, deployed)
	req := web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: deployed, EventID: "descendant-deployment"}
	receipt, err := f.d.DeploymentRelease(req)
	require.NoError(t, err)
	got := f.h.readTask("tst-release.md")
	assert.Equal(t, ticket.StatusTodo, got.Status)
	require.Len(t, got.GraphState.WaitHistory, 1)
	assert.Equal(t, required, got.GraphState.WaitHistory[0].RequiredCommit)
	assert.Equal(t, deployed, got.GraphState.WaitHistory[0].Receipt.Commit)
	// Receipts outlive disposable logs and unavailable repositories.
	require.NoError(t, os.RemoveAll(f.h.logsDir))
	require.NoError(t, os.Rename(f.h.repoDir, f.h.repoDir+"-unavailable"))
	retried, err := f.d.DeploymentRelease(req)
	require.NoError(t, err)
	assert.Equal(t, receipt, retried)
	require.NoError(t, ticket.CheckBoardControl(f.h.tasksDir), ".workflow-events must not make an ordinary board managed")
}

func TestDeploymentReceiptMatchingRefusals(t *testing.T) {
	for _, change := range []string{"environment", "repository", "unrelated commit", "abbreviation", "ref", "missing commit", "disarmed", "expired", "missing worktree", "changed branch", "changed definition"} {
		t.Run(change, func(t *testing.T) {
			f := workflowEventFixture(t, "deployment", false)
			w := f.tkt.GraphState.Wait
			req := web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: w.RequiredCommit, EventID: "deploy-1"}
			accept := true
			switch change {
			case "environment":
				req.Environment = "staging"
			case "repository":
				req.Repository = initRepo(t)
				req.Commit = eventTestGit(t, req.Repository, "rev-parse", "HEAD")
			case "unrelated commit":
				tree := eventTestGit(t, f.h.repoDir, "rev-parse", "HEAD^{tree}")
				req.Commit = eventTestGit(t, f.h.repoDir, "commit-tree", tree, "-m", "unrelated")
			case "abbreviation":
				req.Commit = req.Commit[:8]
				accept = false
			case "ref":
				req.Commit = "HEAD"
				accept = false
			case "missing commit":
				req.Commit = strings.Repeat("f", 40)
				accept = false
			case "disarmed":
				w.Disarmed = true
			case "expired":
				w.Deadline = time.Now().Add(-time.Second)
			case "missing worktree":
				require.NoError(t, os.RemoveAll(w.Worktree))
			case "changed branch":
				require.NoError(t, f.tkt.SetField("branch", "different"))
			case "changed definition":
				f.tkt.GraphState.DefinitionFingerprint = "old"
			}
			require.NoError(t, f.tkt.SetField("graph_state", f.tkt.GraphState))
			f.write(t)
			_, err := f.d.DeploymentRelease(req)
			if accept {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, web.ErrInvalidState)
			}
			f.d.reconcileWorkflowEvents()
			got := f.h.readTask("tst-release.md")
			assert.Equal(t, ticket.StatusPaused, got.Status)
			assert.Empty(t, got.GraphState.WaitHistory)
		})
	}
}

func TestDeploymentReceiptMultiWaitRecovery(t *testing.T) {
	f := workflowEventFixture(t, "deployment", false)
	second := f.tkt.Clone()
	require.NoError(t, second.SetField("id", "tst-second"))
	require.NoError(t, second.SetField("branch", "kontora/second"))
	s := second.GraphState.Clone()
	s.ExecutionID = "execution-second"
	s.Wait.ID = "wait-second"
	s.Wait.ExecutionID = s.ExecutionID
	s.Wait.Branch = second.Branch
	s.Wait.Worktree = filepath.Join(f.h.wtDir, "second")
	eventTestGit(t, f.h.repoDir, "worktree", "add", "-b", second.Branch, s.Wait.Worktree, "HEAD")
	require.NoError(t, second.SetField("graph_state", s))
	data, err := second.Marshal()
	require.NoError(t, err)
	secondPath := f.h.writeTicket("tst-second.md", string(data))
	f.d.tickets[second.ID] = newTicketState(second, secondPath)
	req := web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: s.Wait.RequiredCommit, EventID: "multi-deploy"}
	receipt := &ticket.WorkflowReceipt{ID: workflowEventKey("deployment", req.EventID), RequestID: req.EventID, Repository: s.Wait.Repository, Environment: req.Environment, Commit: req.Commit, AcceptedAt: time.Now().UTC()}
	application, err := f.d.releaseApplication(f.tkt, f.path, receipt)
	require.NoError(t, err)
	event := &workflowEvent{Version: 1, Kind: "deployment", Deployment: &req, Receipt: receipt, Applications: []*workflowEventApplication{application}}
	path := f.d.workflowEventPath("deployment", req.EventID)
	require.NoError(t, saveWorkflowEvent(path, event))
	// Crash after one ticket is saved, before the applied marker and before
	// discovering the next matching ticket.
	require.NoError(t, f.d.applyWorkflowEvent(event, application))
	d := f.h.newDaemon(f.h.cfg)
	d.reconcileWorkflowEvents()
	d.reconcileWorkflowEvents()
	for _, id := range []string{f.tkt.ID, second.ID} {
		got := f.h.readTask(id + ".md")
		assert.Equal(t, ticket.StatusTodo, got.Status)
		require.Len(t, got.GraphState.WaitHistory, 1)
		assert.Equal(t, receipt, got.GraphState.WaitHistory[0].Receipt)
		assert.Len(t, got.GraphState.Transitions, 1)
	}
	event, err = readWorkflowEvent(path)
	require.NoError(t, err)
	require.Len(t, event.Applications, 2)
	assert.True(t, event.Applications[0].Applied)
	assert.True(t, event.Applications[1].Applied)
}

func TestWorkflowReleaseKeepsStageAgentAuthority(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	// A pending runner owns lifecycle authority even if the ticket's visible
	// status has already been parked by another process.
	f.d.running[f.tkt.ID] = func() {}
	_, err := f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
	require.ErrorIs(t, err, web.ErrInvalidState)
	delete(f.d.running, f.tkt.ID)
	f.d.reconcileWorkflowWaits(context.Background())
	assert.Equal(t, ticket.StatusPaused, f.h.readTask("tst-release.md").Status)
}
