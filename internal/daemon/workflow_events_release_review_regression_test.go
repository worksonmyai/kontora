package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func releaseReviewCaptureWorkspace(t *testing.T, f *resultFixture) {
	t.Helper()
	s := f.tkt.GraphState.Clone()
	s.Wait.Worktree = filepath.Join(f.h.wtDir, "human-release")
	eventTestGit(t, f.h.repoDir, "worktree", "add", "-b", f.tkt.Branch, s.Wait.Worktree, "HEAD")
	var err error
	s.Wait.Repository, err = worktree.CommonDir(f.h.repoDir)
	require.NoError(t, err)
	require.NoError(t, f.tkt.SetField("graph_state", s))
	f.write(t)
}

func releaseReviewAcceptHuman(t *testing.T, f *resultFixture) (*workflowEvent, string) {
	t.Helper()
	req := eventHumanRequest(f)
	receipt := &ticket.WorkflowReceipt{ID: workflowEventKey("human", req.RequestID), RequestID: req.RequestID,
		Reason: req.Reason, AcceptedAt: time.Now().UTC()}
	application, err := f.d.releaseApplication(f.tkt, f.path, receipt)
	require.NoError(t, err)
	event := &workflowEvent{Version: 1, Kind: "human", TicketID: f.tkt.ID, Human: &req, Receipt: receipt,
		Applications: []*workflowEventApplication{application}}
	path := f.d.workflowEventPath("human", req.RequestID)
	require.NoError(t, saveWorkflowEvent(path, event))
	return event, path
}

func releaseReviewChangeWorkspace(t *testing.T, f *resultFixture, change string) {
	t.Helper()
	w := f.tkt.GraphState.Wait
	switch change {
	case "missing":
		require.NoError(t, os.RemoveAll(w.Worktree))
	case "relocated":
		eventTestGit(t, f.h.repoDir, "worktree", "move", w.Worktree, w.Worktree+"-moved")
	case "changed branch":
		eventTestGit(t, w.Worktree, "checkout", "-b", "unexpected-release-branch")
	case "changed repository":
		require.NoError(t, os.Rename(f.h.repoDir, f.h.repoDir+"-removed"))
	}
}

func TestReleaseReviewHumanCapturedWorkspace(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "agent"
		if terminal {
			name = "completion"
		}
		for _, change := range []string{"unchanged", "missing", "relocated", "changed branch", "changed repository"} {
			t.Run(name+"/"+change, func(t *testing.T) {
				f := workflowEventFixture(t, "human", terminal)
				releaseReviewCaptureWorkspace(t, f)
				releaseReviewChangeWorkspace(t, f, change)
				req := eventHumanRequest(f)
				before := f.h.readTask("tst-release.md")
				_, err := f.d.WorkflowRelease(f.tkt.ID, req)
				if change == "unchanged" {
					require.NoError(t, err)
					require.Len(t, f.h.readTask("tst-release.md").GraphState.WaitHistory, 1)
					return
				}
				require.ErrorIs(t, err, web.ErrInvalidState)
				require.Equal(t, before, f.h.readTask("tst-release.md"))
				require.NoFileExists(t, f.d.workflowEventPath("human", req.RequestID))
				require.False(t, f.d.queued[f.tkt.ID])
			})
		}
	}
}

func TestReleaseReviewHumanRevalidatesWorkspaceAtApplication(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, change := range []string{"missing", "relocated", "changed branch", "changed repository"} {
			name := "agent"
			if terminal {
				name = "completion"
			}
			t.Run(name+"/"+change, func(t *testing.T) {
				f := workflowEventFixture(t, "human", terminal)
				releaseReviewCaptureWorkspace(t, f)
				event, path := releaseReviewAcceptHuman(t, f)
				releaseReviewChangeWorkspace(t, f, change)
				d := f.h.newDaemon(f.h.cfg)
				d.reconcileWorkflowEvents()
				require.Equal(t, f.tkt.GraphState, f.h.readTask("tst-release.md").GraphState)
				stored, err := readWorkflowEvent(path)
				require.NoError(t, err)
				require.False(t, stored.Applications[0].Applied)
				require.NoFileExists(t, workflowControlPath(f.h.cfg, f.tkt.ID))
				require.NoFileExists(t, controlCleanupPath(f.h.cfg, f.tkt.ID))
				retried, err := d.WorkflowRelease(f.tkt.ID, *event.Human)
				require.NoError(t, err)
				require.Equal(t, event.Receipt, retried)
				require.Equal(t, f.tkt.GraphState, f.h.readTask("tst-release.md").GraphState)
			})
		}
	}
}

func TestReleaseReviewHumanControlJournalWorkspace(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "agent"
		if terminal {
			name = "completion"
		}
		for _, change := range []string{"unchanged", "missing", "relocated", "changed branch", "changed repository"} {
			t.Run(name+"/"+change, func(t *testing.T) {
				f := workflowEventFixture(t, "human", terminal)
				releaseReviewCaptureWorkspace(t, f)
				event, eventPath := releaseReviewAcceptHuman(t, f)
				// Interrupt after the control journal is durable but before the ticket saves.
				require.NoError(t, os.Chmod(f.path, 0444))
				require.Error(t, f.d.applyWorkflowEvent(event, event.Applications[0]))
				require.NoError(t, os.Chmod(f.path, 0644))
				require.FileExists(t, workflowControlPath(f.h.cfg, f.tkt.ID))
				require.Equal(t, f.tkt.GraphState, f.h.readTask("tst-release.md").GraphState)
				releaseReviewChangeWorkspace(t, f, change)
				d := f.h.newDaemon(f.h.cfg)
				d.reconcileWorkflowEvents()
				d.reconcileControlCleanup()
				got := f.h.readTask("tst-release.md")
				stored, err := readWorkflowEvent(eventPath)
				require.NoError(t, err)
				if change != "unchanged" {
					require.Equal(t, f.tkt.GraphState, got.GraphState)
					require.Equal(t, ticket.StatusPaused, got.Status)
					require.False(t, stored.Applications[0].Applied)
					require.False(t, d.queued[got.ID])
					require.FileExists(t, workflowControlPath(f.h.cfg, got.ID))
					if change == "changed branch" {
						require.DirExists(t, f.tkt.GraphState.Wait.Worktree)
						require.Equal(t, "unexpected-release-branch", eventTestGit(t, f.tkt.GraphState.Wait.Worktree, "branch", "--show-current"))
					}
					return
				}
				require.True(t, stored.Applications[0].Applied)
				require.NoFileExists(t, workflowControlPath(f.h.cfg, got.ID))
				require.Nil(t, got.GraphState.Wait)
				require.Len(t, got.GraphState.WaitHistory, 1)
				require.Equal(t, event.Receipt, got.GraphState.WaitHistory[0].Receipt)
				if terminal {
					require.Equal(t, ticket.StatusDone, got.Status)
				} else {
					require.True(t, d.queued[got.ID])
					require.NotNil(t, got.GraphState.Wake)
					require.Equal(t, event.Receipt, got.GraphState.Wake.Receipt)
				}
			})
		}
	}
}

func TestReleaseReviewCheckoutFreeHumanEntry(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	require.Empty(t, f.tkt.GraphState.Wait.Worktree)
	s := f.tkt.GraphState.Clone()
	var err error
	s.Wait.Repository, err = worktree.CommonDir(f.h.repoDir)
	require.NoError(t, err)
	require.NoError(t, f.tkt.SetField("graph_state", s))
	f.write(t)
	_, err = f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
	require.NoError(t, err)
	require.Equal(t, ticket.StatusTodo, f.h.readTask("tst-release.md").Status)
}

func TestReleaseReviewFreshHumanReceiptAfterRetry(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	old, oldPath := releaseReviewAcceptHuman(t, f)
	fresh := *old.Human
	fresh.RequestID = "freshly-inspected-approval"
	_, err := f.d.WorkflowRelease(f.tkt.ID, fresh)
	require.ErrorIs(t, err, web.ErrInvalidState)
	require.NoError(t, f.d.RetryTicket(f.tkt.ID))
	parked := f.h.readTask("tst-release.md")
	require.Greater(t, parked.GraphState.Revision, old.Human.Revision)
	require.Equal(t, old.Human.WaitID, parked.GraphState.Wait.ID)
	require.ErrorIs(t, f.d.applyWorkflowEvent(old, old.Applications[0]), errExecutionChanged)
	retried, err := f.d.WorkflowRelease(f.tkt.ID, *old.Human)
	require.NoError(t, err)
	require.Equal(t, old.Receipt, retried)
	require.Equal(t, parked.GraphState, f.h.readTask("tst-release.md").GraphState)
	_, err = f.d.WorkflowRelease(f.tkt.ID, fresh)
	require.ErrorIs(t, err, web.ErrInvalidState, "a new request still needs the inspected revision")
	fresh.Revision = parked.GraphState.Revision
	receipt, err := f.d.WorkflowRelease(f.tkt.ID, fresh)
	require.NoError(t, err)
	require.NotEqual(t, old.Receipt.ID, receipt.ID)
	applied := f.h.readTask("tst-release.md")
	require.Len(t, applied.GraphState.WaitHistory, 1)
	require.Equal(t, receipt, applied.GraphState.WaitHistory[0].Receipt)
	require.Len(t, applied.GraphState.Transitions, 1)
	retried, err = f.d.WorkflowRelease(f.tkt.ID, *old.Human)
	require.NoError(t, err)
	require.Equal(t, old.Receipt, retried)
	f.d.reconcileWorkflowEvents()
	require.Equal(t, applied.GraphState, f.h.readTask("tst-release.md").GraphState)
	stored, err := readWorkflowEvent(oldPath)
	require.NoError(t, err)
	require.False(t, stored.Applications[0].Applied)
	conflict := fresh
	conflict.Reason = "changed payload"
	_, err = f.d.WorkflowRelease(f.tkt.ID, conflict)
	require.ErrorIs(t, err, web.ErrInvalidState)
}

func TestReleaseReviewFreshHumanReceiptAfterFailedSaveRetry(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	old, oldPath := releaseReviewAcceptHuman(t, f)
	require.NoError(t, os.Chmod(f.path, 0444))
	require.Error(t, f.d.applyWorkflowEvent(old, old.Applications[0]))
	require.NoError(t, os.Chmod(f.path, 0644))
	require.FileExists(t, workflowControlPath(f.h.cfg, f.tkt.ID))
	require.NoError(t, f.d.RetryTicket(f.tkt.ID))
	parked := f.h.readTask("tst-release.md")
	d := f.h.newDaemon(f.h.cfg)
	d.reconcileWorkflowEvents()
	require.NoFileExists(t, workflowControlPath(f.h.cfg, f.tkt.ID))
	require.Equal(t, parked.GraphState, f.h.readTask("tst-release.md").GraphState)
	fresh := *old.Human
	fresh.RequestID = "approval-after-failed-save"
	fresh.Revision = parked.GraphState.Revision
	receipt, err := d.WorkflowRelease(f.tkt.ID, fresh)
	require.NoError(t, err)
	applied := f.h.readTask("tst-release.md")
	require.Len(t, applied.GraphState.WaitHistory, 1)
	require.Equal(t, receipt, applied.GraphState.WaitHistory[0].Receipt)
	require.NotNil(t, applied.GraphState.Wake)
	require.Equal(t, receipt, applied.GraphState.Wake.Receipt)
	retried, err := d.WorkflowRelease(f.tkt.ID, *old.Human)
	require.NoError(t, err)
	require.Equal(t, old.Receipt, retried)
	require.Equal(t, applied.GraphState, f.h.readTask("tst-release.md").GraphState)
	stored, err := readWorkflowEvent(oldPath)
	require.NoError(t, err)
	require.False(t, stored.Applications[0].Applied)
}

func releaseReviewRefreshFingerprint(t *testing.T, f *resultFixture) {
	t.Helper()
	s := f.tkt.GraphState.Clone()
	var err error
	s.DefinitionFingerprint, err = workflowFingerprint(f.h.cfg, f.tkt, f.h.cfg.Pipelines[f.tkt.Pipeline].Graph)
	require.NoError(t, err)
	require.NoError(t, f.tkt.SetField("graph_state", s))
	f.write(t)
}

func TestReleaseReviewExhaustedLoopDestinations(t *testing.T) {
	for _, kind := range []string{"human", "deployment"} {
		for _, target := range []string{"work", "done", "stop", "review", "second-gate", "invalid"} {
			t.Run(kind+"/"+target, func(t *testing.T) {
				f := workflowEventFixture(t, kind, false)
				graph := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph
				graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "limit", Type: "loop", Max: 0},
					config.WorkflowNode{ID: "review", Type: "end", Status: "human_review"},
					config.WorkflowNode{ID: "second-gate", Type: "wait", Mode: "human"},
					config.WorkflowNode{ID: "invalid", Type: "condition"})
				graph.Edges[0].To = "limit"
				graph.Edges = append(graph.Edges,
					config.WorkflowEdge{From: config.WorkflowPort{Node: "limit", Port: "repeat"}, To: "gate"},
					config.WorkflowEdge{From: config.WorkflowPort{Node: "limit", Port: "exhausted"}, To: target},
					config.WorkflowEdge{From: config.WorkflowPort{Node: "second-gate", Port: "released"}, To: "done"})
				releaseReviewRefreshFingerprint(t, f)
				var err error
				if kind == "human" {
					_, err = f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
				} else {
					_, err = f.d.DeploymentRelease(web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production",
						Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "exhausted-release"})
				}
				if target == "invalid" {
					if kind == "human" {
						require.ErrorIs(t, err, web.ErrInvalidState)
					} else {
						require.NoError(t, err)
					}
					require.Equal(t, f.tkt.GraphState, f.h.readTask("tst-release.md").GraphState)
					return
				}
				require.NoError(t, err)
				got := f.h.readTask("tst-release.md")
				require.Equal(t, target, got.Stage)
				require.Len(t, got.GraphState.WaitHistory, 1)
				require.Len(t, got.GraphState.Transitions, 2)
				require.Equal(t, "exhausted", got.GraphState.Transitions[1].Port)
				require.Equal(t, "loop limit exhausted", got.LastError)
				if target == "work" {
					require.True(t, got.GraphState.Continuation)
					require.Equal(t, ticket.StatusTodo, got.Status)
				} else {
					require.False(t, got.GraphState.Continuation)
					require.Nil(t, got.GraphState.Wake)
				}
			})
		}
	}
}

func TestReleaseReviewTimeoutExhaustedLoopScheduler(t *testing.T) {
	for _, kind := range []string{"human", "deployment"} {
		for _, target := range []string{"work", "done", "stop", "review", "second-gate", "invalid"} {
			t.Run(kind+"/"+target, func(t *testing.T) {
				f := workflowEventFixture(t, kind, false)
				clock := newWorkflowWaitTestClock()
				graph := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph
				graph.Nodes[0].Timeout = config.Duration{Duration: time.Hour}
				graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "limit", Type: "loop"},
					config.WorkflowNode{ID: "review", Type: "end", Status: "human_review"},
					config.WorkflowNode{ID: "second-gate", Type: "wait", Mode: "human"},
					config.WorkflowNode{ID: "invalid", Type: "condition"})
				graph.Edges = append(graph.Edges,
					config.WorkflowEdge{From: config.WorkflowPort{Node: "gate", Port: "timeout"}, To: "limit"},
					config.WorkflowEdge{From: config.WorkflowPort{Node: "limit", Port: "exhausted"}, To: target},
					config.WorkflowEdge{From: config.WorkflowPort{Node: "second-gate", Port: "released"}, To: "done"})
				s := f.tkt.GraphState.Clone()
				s.Wait.Deadline = clock.Now().Add(time.Hour)
				if kind == "human" {
					var err error
					s.Wait.Repository, err = worktree.CommonDir(f.h.repoDir)
					require.NoError(t, err)
				}
				require.NoError(t, f.tkt.SetField("graph_state", s))
				releaseReviewRefreshFingerprint(t, f)
				d := f.h.newDaemon(f.h.cfg, WithWorkflowWaitClock(clock))
				d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
				clock.Advance(time.Hour)
				d.reconcileWorkflowWaits(context.Background())
				got := f.h.readTask("tst-release.md")
				if target == "invalid" {
					require.Equal(t, f.tkt.GraphState, got.GraphState)
					require.False(t, d.queued[got.ID])
					return
				}
				require.Equal(t, target, got.Stage)
				require.Len(t, got.GraphState.WaitHistory, 1)
				require.Nil(t, got.GraphState.WaitHistory[0].Receipt)
				require.Len(t, got.GraphState.Transitions, 2)
				require.Equal(t, "timeout", got.GraphState.Transitions[0].Port)
				require.Equal(t, "exhausted", got.GraphState.Transitions[1].Port)
				require.Equal(t, "loop limit exhausted", got.LastError)
				require.Empty(t, got.History)
				switch target {
				case "work":
					require.Equal(t, ticket.StatusTodo, got.Status)
					require.True(t, got.GraphState.Continuation)
					require.True(t, d.queued[got.ID])
				case "second-gate":
					require.Equal(t, ticket.StatusPaused, got.Status)
					require.NotNil(t, got.GraphState.Wait)
					require.NotEqual(t, f.tkt.GraphState.Wait.ID, got.GraphState.Wait.ID)
				default:
					require.Nil(t, got.GraphState.Wait)
					require.False(t, got.GraphState.Continuation)
					require.Equal(t, map[string]ticket.Status{"done": ticket.StatusDone, "stop": ticket.StatusPaused, "review": ticket.StatusHumanReview}[target], got.Status)
				}
			})
		}
	}
}

func TestReleaseReviewReceiptWakeReplayAndRetry(t *testing.T) {
	for _, kind := range []string{"human", "deployment"} {
		for _, boundary := range []string{"receipt", "control journal"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				f := workflowEventFixture(t, kind, false)
				if kind == "human" {
					releaseReviewCaptureWorkspace(t, f)
				}
				f.h.cfg.Stages["step1"] = config.Stage{Prompt: "Release receipt: {{ .Wake.Receipt.ID }}"}
				graph := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph
				graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "review", Type: "end", Status: "human_review"})
				graph.Edges[1].To = "review"
				releaseReviewRefreshFingerprint(t, f)
				require.NoError(t, os.Chmod(f.path, 0444))
				var receipt *ticket.WorkflowReceipt
				var err error
				if kind == "human" {
					receipt, err = f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
				} else {
					receipt, err = f.d.DeploymentRelease(web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production",
						Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "replayed-handoff-release"})
				}
				require.NoError(t, err, "acceptance remains acknowledged if the ticket save fails")
				require.NoError(t, os.Chmod(f.path, 0644))
				require.Equal(t, f.tkt.GraphState, f.h.readTask("tst-release.md").GraphState)
				data, err := os.ReadFile(workflowControlPath(f.h.cfg, f.tkt.ID))
				require.NoError(t, err)
				var journal workflowControlTransition
				require.NoError(t, json.Unmarshal(data, &journal))
				require.NotNil(t, journal.After.Wake)
				require.Equal(t, receipt, journal.After.Wake.Receipt)
				if boundary == "receipt" {
					// Replay acceptance alone, as if interrupted before the control journal write.
					require.NoError(t, os.Remove(workflowControlPath(f.h.cfg, f.tkt.ID)))
				}
				clock := newWorkflowWaitTestClock()
				var prompts []string
				d := f.h.newDaemon(f.h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
					prompts = append(prompts, renderedPrompt(p))
					outcome := "blocked"
					if len(prompts) > 1 {
						outcome = "complete"
					}
					return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: outcome, Summary: "Released work"})
				}))
				d.reconcileWorkflowEvents()
				d.reconcileWorkflowEvents()
				ready := f.h.readTask("tst-release.md")
				require.Equal(t, journal.After.Wake, ready.GraphState.Wake)
				require.Equal(t, receipt, ready.GraphState.Wake.Receipt)
				require.Len(t, ready.GraphState.Transitions, 1)
				require.True(t, d.queued[ready.ID])
				d.runTicket(context.Background(), ready.ID)
				blocked := f.h.readTask("tst-release.md")
				require.Equal(t, ticket.StatusPaused, blocked.Status, blocked.LastError)
				require.NoError(t, d.RetryTicket(ready.ID))
				d.runTicket(context.Background(), ready.ID)
				got := f.h.readTask("tst-release.md")
				require.Equal(t, ticket.StatusHumanReview, got.Status, got.LastError)
				require.Len(t, prompts, 2)
				require.Len(t, got.History, 2)
				for i, prompt := range prompts {
					require.Contains(t, prompt, "Release receipt: "+receipt.ID)
					require.Contains(t, prompt, "## Workflow wake handoff")
					require.Contains(t, prompt, "Reason: released")
					require.NotContains(t, prompt, "Wake count:")
					require.NotContains(t, prompt, "Continuation instructions:")
					require.Equal(t, ready.GraphState.Wake, got.History[i].Wake)
				}
				require.False(t, got.History[1].Continuation, "failure Retry is not a same-visit wake")
				require.Zero(t, got.GraphState.WakeCount)
			})
		}
	}
}

func TestReleaseReviewInvocationReceivesReceipt(t *testing.T) {
	for _, kind := range []string{"human", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			f := workflowEventFixture(t, kind, false)
			if kind == "human" {
				releaseReviewCaptureWorkspace(t, f)
			}
			f.h.cfg.Stages["step1"] = config.Stage{Prompt: "Release receipt: {{ .Wake.Receipt.ID }}"}
			graph := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph
			graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "review", Type: "end", Status: "human_review"})
			graph.Edges[1].To = "review"
			releaseReviewRefreshFingerprint(t, f)
			clock := newWorkflowWaitTestClock()
			var prompt string
			calls := 0
			f.d = f.h.newDaemon(f.h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls++
				prompt = renderedPrompt(p)
				return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "complete", Summary: "Released work complete"})
			}))
			f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
			var receipt *ticket.WorkflowReceipt
			var err error
			if kind == "human" {
				receipt, err = f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
			} else {
				receipt, err = f.d.DeploymentRelease(web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production",
					Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "handoff-release"})
			}
			require.NoError(t, err)
			ready := f.h.readTask("tst-release.md")
			require.NotNil(t, ready.GraphState.Wake)
			require.Equal(t, "released", ready.GraphState.Wake.Reason)
			require.Equal(t, f.tkt.GraphState.Wait.ID, ready.GraphState.Wake.WaitID)
			require.Equal(t, receipt, ready.GraphState.Wake.Receipt)
			require.Zero(t, ready.GraphState.WakeCount)
			require.Zero(t, ready.GraphState.Wake.Count)
			require.Empty(t, ready.GraphState.Wake.Next)
			require.Empty(t, ready.History)
			f.d.runTicket(context.Background(), f.tkt.ID)
			got := f.h.readTask("tst-release.md")
			require.Equal(t, ticket.StatusHumanReview, got.Status, got.LastError)
			require.Equal(t, 1, calls)
			require.Contains(t, prompt, "Release receipt: "+receipt.ID)
			require.Contains(t, prompt, "## Workflow wake handoff")
			require.Contains(t, prompt, "Reason: released")
			require.Len(t, got.History, 1)
			require.Equal(t, ready.GraphState.Wake, got.History[0].Wake)
			require.Zero(t, got.GraphState.WakeCount)
		})
	}
}
