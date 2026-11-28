package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func TestAcceptedReleaseKeepsAuthorityPastDeadline(t *testing.T) {
	for _, kind := range []string{"human", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			f := workflowEventFixture(t, kind, false)
			clock := newWorkflowWaitTestClock()
			f.d.workflowClock = clock
			graph := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph
			graph.Nodes[0].Timeout.Duration = time.Second
			graph.Edges = append(graph.Edges, config.WorkflowEdge{From: config.WorkflowPort{Node: "gate", Port: "timeout"}, To: "stop"})
			fingerprint, err := workflowFingerprint(f.h.cfg, f.tkt, graph)
			require.NoError(t, err)
			s := f.tkt.GraphState.Clone()
			s.DefinitionFingerprint, s.Wait.Deadline = fingerprint, clock.Now().Add(time.Second)
			if kind == "human" {
				s.Wait.Repository, err = worktree.CommonDir(f.h.repoDir)
				require.NoError(t, err)
			}
			require.NoError(t, f.tkt.SetField("graph_state", s))
			f.write(t)
			f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
			receipt := &ticket.WorkflowReceipt{ID: workflowEventKey(kind, "accepted"), AcceptedAt: clock.Now().UTC(),
				Repository: s.Wait.Repository, Commit: s.Wait.RequiredCommit, Environment: s.Wait.Environment}
			application, err := f.d.releaseApplication(f.tkt, journalTicketAlias(t, f.path), receipt)
			require.NoError(t, err)
			event := &workflowEvent{Version: 1, Kind: kind, TicketID: f.tkt.ID, Receipt: receipt, Applications: []*workflowEventApplication{application}}
			if kind == "human" {
				req := eventHumanRequest(f)
				req.RequestID = "accepted"
				event.Human = &req
			} else {
				event.Deployment = &web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: receipt.Environment, Commit: receipt.Commit, EventID: "accepted"}
			}
			require.NoError(t, saveWorkflowEvent(f.d.workflowEventPath(kind, "accepted"), event))
			clock.Advance(2 * time.Second)
			// Simulate startup running timer reconciliation before event recovery.
			f.d.reconcileWorkflowWaits(context.Background())
			require.Equal(t, s, f.h.readTask("tst-release.md").GraphState)
			f.d.reconcileWorkflowEvents()
			got := f.h.readTask("tst-release.md")
			require.Equal(t, "work", got.Stage)
			require.Len(t, got.GraphState.WaitHistory, 1)
			require.Equal(t, receipt, got.GraphState.WaitHistory[0].Receipt)
		})
	}
}

func TestDeploymentReconciliationSharesOnlyPassLocalProof(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(fmt.Sprint(matching), func(t *testing.T) {
			f := workflowEventFixture(t, "deployment", false)
			var deployed string
			if matching {
				eventTestGit(t, f.h.repoDir, "commit", "--allow-empty", "-m", "descendant")
				deployed = eventTestGit(t, f.h.repoDir, "rev-parse", "HEAD")
			} else {
				deployed = eventTestGit(t, f.h.repoDir, "commit-tree", eventTestGit(t, f.h.repoDir, "rev-parse", "HEAD^{tree}"), "-m", "unrelated")
			}
			second := f.tkt.Clone()
			require.NoError(t, second.SetField("id", "tst-second"))
			require.NoError(t, second.SetField("branch", "kontora/second"))
			s := second.GraphState.Clone()
			s.ExecutionID, s.Wait.ExecutionID, s.Wait.ID = "second", "second", "second-wait"
			s.Wait.Branch, s.Wait.Worktree = second.Branch, filepath.Join(f.h.wtDir, "second")
			eventTestGit(t, f.h.repoDir, "worktree", "add", "-b", second.Branch, s.Wait.Worktree, "HEAD")
			require.NoError(t, second.SetField("graph_state", s))
			data, err := second.Marshal()
			require.NoError(t, err)
			secondPath := f.h.writeTicket("tst-second.md", string(data))
			f.d.tickets[second.ID] = newTicketState(second, secondPath)
			for i := range 3 {
				req := web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: deployed, EventID: fmt.Sprintf("historical-%d", i)}
				event := &workflowEvent{Version: 1, Kind: "deployment", Deployment: &req, Receipt: &ticket.WorkflowReceipt{
					ID: workflowEventKey("deployment", req.EventID), Repository: s.Wait.Repository, Commit: deployed, Environment: req.Environment}}
				require.NoError(t, saveWorkflowEvent(f.d.workflowEventPath("deployment", req.EventID), event))
			}
			realGit, err := exec.LookPath("git")
			require.NoError(t, err)
			bin := t.TempDir()
			log := filepath.Join(bin, "walks")
			script := fmt.Sprintf("#!/bin/sh\nfor arg do\n if [ \"$arg\" = rev-list ]; then printf 'walk\\n' >> %q; fi\ndone\nexec %q \"$@\"\n", log, realGit)
			require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700))
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			f.d.reconcileWorkflowEvents()
			walks, err := os.ReadFile(log)
			require.NoError(t, err)
			wantWalks := 1
			if matching {
				wantWalks = 2 // Each positive application revalidates local history.
			}
			require.Equal(t, wantWalks, strings.Count(string(walks), "walk"), "duplicate non-matches share one traversal across waits and receipts")
			for _, id := range []string{f.tkt.ID, second.ID} {
				got := f.h.readTask(id + ".md")
				require.Equal(t, matching, got.GraphState.Wait == nil)
			}
			if !matching {
				// A new required commit must not inherit an old non-match.
				s := f.tkt.GraphState.Clone()
				s.Revision++
				s.Wait.RequiredCommit = deployed
				require.NoError(t, f.tkt.SetField("graph_state", s))
				f.write(t)
				f.d.reconcileWorkflowEvents()
				require.Nil(t, f.h.readTask("tst-release.md").GraphState.Wait)
				return
			}
			// A later wait must revalidate local objects, even for a proven pair.
			s = f.tkt.GraphState.Clone()
			s.Revision += 10
			s.Wait.ID = "later-wait"
			require.NoError(t, f.tkt.SetField("graph_state", s))
			f.write(t)
			object := filepath.Join(s.Wait.Repository, "objects", deployed[:2], deployed[2:])
			objectData, err := os.ReadFile(object)
			require.NoError(t, err)
			require.NoError(t, os.Remove(object))
			f.d.reconcileWorkflowEvents()
			require.NotNil(t, f.h.readTask("tst-release.md").GraphState.Wait)
			require.NoError(t, os.WriteFile(object, objectData, 0444))
			f.d.reconcileWorkflowEvents()
			require.Nil(t, f.h.readTask("tst-release.md").GraphState.Wait, "errors must not poison repaired evidence")
		})
	}
}

func TestDeploymentApplicationRejectsChangedSnapshot(t *testing.T) {
	for _, change := range []string{"revision", "commit", "environment", "repository", "wait", "branch", "disarmed"} {
		t.Run(change, func(t *testing.T) {
			f := workflowEventFixture(t, "deployment", false)
			req := web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "stale-proof"}
			event := &workflowEvent{Version: 1, Kind: "deployment", Deployment: &req, Receipt: &ticket.WorkflowReceipt{
				ID: workflowEventKey("deployment", req.EventID), Repository: f.tkt.GraphState.Wait.Repository, Commit: req.Commit, Environment: req.Environment}}
			path := f.d.workflowEventPath("deployment", req.EventID)
			require.NoError(t, saveWorkflowEvent(path, event))
			snapshot := &app.StoredTicket{Ticket: f.tkt.Clone(), FilePath: f.path}
			s := f.tkt.GraphState.Clone()
			switch change {
			case "revision":
				s.Revision++
			case "commit":
				s.Wait.RequiredCommit = strings.Repeat("a", 40)
			case "environment":
				s.Wait.Environment = "staging"
			case "repository":
				s.Wait.Repository = "different"
			case "wait":
				s.Wait.ID = "new"
			case "branch":
				require.NoError(t, f.tkt.SetField("branch", "other"))
			case "disarmed":
				s.Wait.Disarmed = true
			}
			require.NoError(t, f.tkt.SetField("graph_state", s))
			f.write(t)
			f.d.acceptDeploymentApplication(path, event.Receipt, snapshot)
			stored, err := readWorkflowEvent(path)
			require.NoError(t, err)
			require.Empty(t, stored.Applications)
			require.Equal(t, s, f.h.readTask("tst-release.md").GraphState)
		})
	}
}

func TestDeploymentHistoryDoesNotBlockWaitDeadline(t *testing.T) {
	f := workflowEventFixture(t, "deployment", false)
	clock := newWorkflowWaitTestClock()
	f.d.workflowClock = clock
	graph := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph
	graph.Nodes[1].Wake = &config.WorkflowWake{Max: 3, CheckTimeout: config.Duration{Duration: time.Second}}
	graph.Nodes[0].Timeout.Duration = time.Second
	graph.Edges = append(graph.Edges, config.WorkflowEdge{From: config.WorkflowPort{Node: "gate", Port: "timeout"}, To: "stop"})
	fingerprint, err := workflowFingerprint(f.h.cfg, f.tkt, graph)
	require.NoError(t, err)
	s := f.tkt.GraphState.Clone()
	s.DefinitionFingerprint = fingerprint
	s.Wait.Deadline = clock.Now().Add(time.Second)
	require.NoError(t, f.tkt.SetField("graph_state", s))
	f.write(t)
	f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	for _, kind := range []string{"timer", "check"} {
		other := f.tkt.Clone()
		require.NoError(t, other.SetField("id", "tst-"+kind))
		require.NoError(t, other.SetField("branch", "kontora/"+kind))
		require.NoError(t, other.SetField("stage", "work"))
		state := s.Clone()
		state.ExecutionID = "execution-" + kind
		state.Wait = &ticket.WorkflowWait{ID: "wait-" + kind, Kind: kind, Phase: "pending", ExecutionID: state.ExecutionID,
			NodeID: "work", Worktree: filepath.Join(f.h.wtDir, kind), Repository: s.Wait.Repository, Branch: other.Branch,
			RequestedAt: clock.Now(), Next: "Resume after waiting"}
		if kind == "timer" {
			state.Wait.Until = clock.Now().Add(time.Second)
		} else {
			state.Wait.Command, state.Wait.Poll = "true", time.Second
			state.Wait.NextPoll = clock.Now().Add(time.Second)
		}
		eventTestGit(t, f.h.repoDir, "worktree", "add", "-b", other.Branch, state.Wait.Worktree, "HEAD")
		require.NoError(t, other.SetField("graph_state", state))
		data, err := other.Marshal()
		require.NoError(t, err)
		otherPath := f.h.writeTicket(other.ID+".md", string(data))
		f.d.tickets[other.ID] = newTicketState(other, otherPath)
	}
	eventTestGit(t, f.h.repoDir, "commit", "--allow-empty", "-m", "descendant")
	deployed := eventTestGit(t, f.h.repoDir, "rev-parse", "HEAD")
	req := web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: deployed, EventID: "slow-history"}
	event := &workflowEvent{Version: 1, Kind: "deployment", Deployment: &req, Receipt: &ticket.WorkflowReceipt{
		ID: workflowEventKey("deployment", req.EventID), Repository: s.Wait.Repository, Commit: deployed, Environment: req.Environment}}
	require.NoError(t, saveWorkflowEvent(f.d.workflowEventPath("deployment", req.EventID), event))
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	started, release := filepath.Join(bin, "started"), filepath.Join(bin, "release")
	script := fmt.Sprintf("#!/bin/sh\nfor arg do\n if [ \"$arg\" = rev-list ]; then\n  touch %q\n  while [ ! -f %q ]; do sleep 0.01; done\n fi\ndone\nexec %q \"$@\"\n", started, release, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.d.workflowWaitLoop(ctx) }()
	defer func() {
		_ = os.WriteFile(release, nil, 0600)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("wait loop did not stop")
		}
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 5*time.Second, 10*time.Millisecond)
	clock.Advance(time.Second)
	f.d.signalWorkflowWait()
	require.Eventually(t, func() bool {
		got, err := ticket.ParseFile(f.path)
		return err == nil && got.GraphState.Wait == nil && got.Stage == "stop"
	}, 2*time.Second, 10*time.Millisecond, "a blocked history scan must not delay a wait deadline")
	require.Eventually(t, func() bool {
		got, err := ticket.ParseFile(filepath.Join(f.h.tasksDir, "tst-timer.md"))
		return err == nil && got.Status == ticket.StatusTodo && got.GraphState.Wake != nil && got.GraphState.Wake.Reason == "timer"
	}, 2*time.Second, 10*time.Millisecond, "an unrelated timer must still fire")
	f.d.mu.Lock()
	probeQueued := f.d.workflowProbeQueuedLocked("tst-check")
	f.d.mu.Unlock()
	require.True(t, probeQueued, "an unrelated due probe must still reach the probe queue")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel the blocked deployment worker")
	}
	stored, err := readWorkflowEvent(f.d.workflowEventPath("deployment", req.EventID))
	require.NoError(t, err)
	require.Empty(t, stored.Applications, "a late or canceled proof cannot override the timeout")
}
