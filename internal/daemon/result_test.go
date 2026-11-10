package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

const resultTicketID = "tst-result"

// Recovery is driven synchronously, without Run's watcher, scheduler or orphan
// cleanup. In particular, a recovered todo ticket cannot race a new pickup.
type resultFixture struct {
	h    *testHarness
	d    *Daemon
	path string
	tkt  *ticket.Ticket
	rec  *executionRecord
}

func newResultFixture(t *testing.T) *resultFixture {
	t.Helper()
	h := newHarness(t)
	h.writeTicket("epc-result.md", fmt.Sprintf("---\nid: epc-result\nkontora: true\nkind: epic\nepic_execution: agent\nstatus: paused\npath: %s\n---\n# Supervisor\n", h.repoDir))
	md := strings.Replace(h.taskMD(resultTicketID, "in_progress", "two-stage"), "status: in_progress\n",
		"status: in_progress\nparent: epc-result\nstage: step2\nattempt: 2\nbranch: kontora/result\nclaimed_by: test-instance\ninvocation_id: inv-result\nstarted_at: 2026-01-01T01:00:00Z\n", 1)
	path := h.writeTicket(resultTicketID+".md", md)
	d := h.newDaemon(h.cfg)
	d.windows = windowOps{
		list: func(string) ([]string, error) { return nil, nil },
		kill: func(string, string) error { t.Fatal("no recovery window to kill"); return nil },
	}
	tkt := h.readTask(resultTicketID + ".md")
	rec := &executionRecord{
		Invocation: session.Invocation{Version: session.ResultVersion, ID: tkt.InvocationID, TicketID: tkt.ID, Stage: tkt.Stage,
			ResultPath: filepath.Join(h.logsDir, tkt.ID, "results", tkt.InvocationID+".json")},
		EpicID: "epc-result", Instance: d.instanceName, Worktree: h.wtDir,
		Pipeline: tkt.Pipeline, TicketStage: tkt.Stage, Attempt: tkt.Attempt,
		Branch: tkt.Branch, RepoPath: tkt.Path, Phase: "running",
	}
	require.NoError(t, saveExecution(h.cfg, rec))
	d.tickets[tkt.ID] = newTicketState(tkt, path)
	return &resultFixture{h: h, d: d, path: path, tkt: tkt, rec: rec}
}

func (f *resultFixture) write(t *testing.T) {
	t.Helper()
	data, err := f.tkt.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.path, data, 0o644))
}

func (f *resultFixture) reconcile(t *testing.T) *ticket.Ticket {
	t.Helper()
	f.d.mu.Lock()
	handled := f.d.reconcileExecutionLocked(f.tkt, f.path)
	f.d.mu.Unlock()
	require.True(t, handled)
	return f.h.readTask(resultTicketID + ".md")
}

func publishDaemonResult(t *testing.T, path, outcome string) {
	t.Helper()
	result := session.Result{Outcome: outcome, Summary: "assigned stage finished"}
	if outcome == "blocked" {
		result.Summary, result.NextStep = "credentials unavailable", "operator must provide credentials"
	}
	duplicate, err := session.PublishResult(path, result)
	require.NoError(t, err)
	require.False(t, duplicate)
}

func TestResultClassification(t *testing.T) {
	for _, tc := range []struct {
		name, report, failure, want, reason string
		exit                                int
		reported                            bool
	}{
		{name: "missing zero exit", want: "protocol_error", reason: "missing current-invocation report"},
		{name: "complete", report: "complete", want: "complete", reason: "assigned stage finished", reported: true},
		{name: "blocked", report: "blocked", want: "blocked", reason: "Next step: operator must provide credentials", reported: true},
		{name: "blocked beats nonzero", report: "blocked", exit: 7, want: "blocked", reason: "credentials unavailable", reported: true},
		{name: "blocked beats runner failure", report: "blocked", failure: "transport failed", want: "blocked", reason: "credentials unavailable", reported: true},
		{name: "complete cannot hide nonzero", report: "complete", exit: 7, want: "failed", reason: "agent exited with code 7", reported: true},
		{name: "complete cannot hide runner failure", report: "complete", failure: "transport failed", want: "failed", reason: "transport failed", reported: true},
		{name: "missing nonzero", exit: 7, want: "failed", reason: "agent exited with code 7"},
		{name: "malformed", report: "malformed", want: "protocol_error", reason: "result protocol:", reported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			if tc.report == "malformed" {
				require.NoError(t, os.WriteFile(path, []byte(`{"outcome":"complete","summary":null}`), 0o600))
			} else if tc.report != "" {
				publishDaemonResult(t, path, tc.report)
			}
			rec := &executionRecord{Invocation: session.Invocation{ResultPath: path}}
			run := agentRun{Result: process.Result{ExitCode: tc.exit}}
			classifyExecution(rec, &run, tc.failure)
			assert.Same(t, rec, run.Execution)
			assert.Equal(t, tc.want, run.Outcome)
			assert.Contains(t, run.OutcomeReason, tc.reason)
			assert.Equal(t, tc.reported, run.Reported)
		})
	}
	t.Run("standalone has no protocol", func(t *testing.T) {
		run := agentRun{Result: process.Result{ExitCode: 0}}
		classifyExecution(nil, &run, "")
		assert.Empty(t, run.Outcome)
		assert.Nil(t, run.Execution)
	})
}

func TestResultRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, report, phase, outcome, reason string
		want                                 ticket.Status
		retained                             bool
	}{
		{name: "no report reruns", want: ticket.StatusTodo, retained: true},
		{name: "complete still needs validation", report: "complete", want: ticket.StatusTodo, retained: true},
		{name: "blocked pauses", report: "blocked", want: ticket.StatusPaused, outcome: "blocked", reason: "operator must provide credentials"},
		{name: "malformed pauses", report: "malformed", want: ticket.StatusPaused, outcome: "protocol_error", reason: "result protocol:"},
		{name: "finalizing never repeats hooks", phase: "finalizing", report: "complete", want: ticket.StatusPaused, reason: "hook effects are unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResultFixture(t)
			f.rec.Agent, f.rec.Run = "agent2", 4
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			if tc.phase != "" {
				f.rec.Phase = tc.phase
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
			}
			if tc.report == "malformed" {
				require.NoError(t, os.MkdirAll(filepath.Dir(f.rec.Invocation.ResultPath), 0o700))
				require.NoError(t, os.WriteFile(f.rec.Invocation.ResultPath, []byte("not json"), 0o600))
			} else if tc.report != "" {
				publishDaemonResult(t, f.rec.Invocation.ResultPath, tc.report)
			}
			got := f.reconcile(t)
			assert.Equal(t, tc.want, got.Status)
			assert.Equal(t, "step2", got.Stage)
			assert.Equal(t, 2, got.Attempt)
			assert.Equal(t, "kontora/result", got.Branch)
			assert.Nil(t, got.CompletedAt)
			assert.Contains(t, got.LastError, tc.reason)
			if tc.outcome != "" {
				require.Len(t, got.History, 1)
				assert.Equal(t, ticket.RunOutcome(tc.outcome), got.History[0].RunOutcome)
				assert.Equal(t, f.rec.Invocation.ID, got.History[0].InvocationID)
				assert.Contains(t, got.History[0].OutcomeReason, tc.reason)
				assert.Equal(t, -1, got.History[0].ExitCode, "recovery did not observe a runner exit")
				assert.Equal(t, f.rec.Agent, got.History[0].Agent)
				assert.Equal(t, f.rec.Run, got.History[0].Run)
			} else {
				assert.Empty(t, got.History, "a report is not evidence of a validated completion")
			}
			if tc.retained {
				assert.Equal(t, f.rec.Invocation.ID, got.InvocationID)
				rec, err := loadExecution(f.h.cfg, got.ID)
				require.NoError(t, err)
				assert.Equal(t, "recovered", rec.Phase)
			} else {
				assert.Empty(t, got.InvocationID)
				assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
			}
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			f.reconcile(t)
			after, err := os.ReadFile(f.path)
			require.NoError(t, err)
			assert.Equal(t, before, after, "recovery must be idempotent")
			assert.Empty(t, f.d.running)
			assert.Zero(t, f.d.sem.used)
		})
	}
}

func TestResultRecoveryTransitionReplay(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("history_already_written=%t", existing), func(t *testing.T) {
			f := newResultFixture(t)
			ended := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
			row := ticket.HistoryEntry{Stage: "step2", Agent: "agent2", Run: 3, ExitCode: 0,
				InvocationID: f.rec.Invocation.ID, RunOutcome: "complete", OutcomeReason: "validated", CompletedAt: &ended}
			f.rec.Phase = "transition"
			f.rec.Transition = &executionTransition{Status: ticket.StatusDone, Stage: "step2", Attempt: 2,
				CompletedAt: &ended, Summary: "accepted summary", History: row}
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			require.NoError(t, f.tkt.SetField("last_error", "old failure"))
			if existing {
				require.NoError(t, f.tkt.SetField("history", []ticket.HistoryEntry{row}))
			}
			f.write(t)
			got := f.reconcile(t)
			assert.Equal(t, ticket.StatusDone, got.Status)
			assert.Equal(t, "accepted summary", got.Summary)
			assert.Equal(t, &ended, got.CompletedAt)
			assert.Empty(t, got.LastError)
			assert.Empty(t, got.InvocationID)
			require.Len(t, got.History, 1)
			assert.Equal(t, row, got.History[0])
			assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
			assert.Equal(t, got.History, f.reconcile(t).History)
		})
	}
}

func TestResultRecoveryFinalizingRecordsUnvalidatedRun(t *testing.T) {
	f := newResultFixture(t)
	f.rec.Agent, f.rec.Run = "agent2", 3
	started := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	run := agentRun{Result: process.Result{StartedAt: started, ExitedAt: ended},
		Outcome: "complete", OutcomeReason: "assigned stage finished", FinalMessage: "implementation notes",
		Model: "test-model", Effort: "high", SessionKind: "pi", SessionRef: "step2/session.jsonl"}
	publishDaemonResult(t, f.rec.Invocation.ResultPath, "complete")
	require.NoError(t, f.d.beginFinalization(f.h.cfg, f.rec, run))
	got := f.reconcile(t)
	assert.Equal(t, ticket.StatusPaused, got.Status)
	assert.Nil(t, got.CompletedAt)
	assert.Contains(t, got.LastError, "hook effects are unknown")
	require.Len(t, got.History, 1)
	h := got.History[0]
	assert.Equal(t, f.rec.Invocation.ID, h.InvocationID)
	assert.Equal(t, ticket.RunOutcomeFailed, h.RunOutcome)
	assert.Equal(t, got.LastError, h.OutcomeReason)
	assert.Equal(t, "agent2", h.Agent)
	assert.Equal(t, "step2", h.Stage)
	assert.Equal(t, 3, h.Run)
	assert.Equal(t, "test-model", h.Model)
	assert.Equal(t, "high", h.Effort)
	assert.Equal(t, "implementation notes", h.Summary)
	assert.Equal(t, "pi", h.SessionKind)
	assert.Equal(t, "step2/session.jsonl", h.SessionRef)
	assert.Equal(t, &started, h.StartedAt)
	assert.Equal(t, &ended, h.CompletedAt)
	assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
	assert.Equal(t, got.History, f.reconcile(t).History)
}

func TestResultRecoveryInvalidExecution(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "wrong artifact path", "wrong ticket", "wrong version"} {
		t.Run(kind, func(t *testing.T) {
			f := newResultFixture(t)
			switch kind {
			case "missing":
				require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
			case "malformed":
				require.NoError(t, os.WriteFile(executionPath(f.h.cfg, f.tkt.ID), []byte("{"), 0o600))
			case "wrong artifact path":
				f.rec.Invocation.ResultPath = filepath.Join(f.h.logsDir, "other.json")
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
			case "wrong ticket":
				data, err := os.ReadFile(executionPath(f.h.cfg, f.tkt.ID))
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(executionPath(f.h.cfg, f.tkt.ID), []byte(strings.Replace(string(data), `"ticket_id":"tst-result"`, `"ticket_id":"tst-other"`, 1)), 0o600))
			case "wrong version":
				f.rec.Invocation.Version++
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
			}
			got := f.reconcile(t)
			assert.Equal(t, ticket.StatusPaused, got.Status)
			assert.Contains(t, got.LastError, "invocation recovery:")
			assert.Empty(t, got.History)
			assert.Empty(t, f.d.running)
		})
	}
}

func TestResultRecoveryTerminationConfirmation(t *testing.T) {
	for _, mode := range []string{"absent", "killed", "list fails", "kill fails", "still present", "verification fails"} {
		t.Run(mode, func(t *testing.T) {
			f := newResultFixture(t)
			lists, kills := 0, 0
			f.d.windows = windowOps{
				list: func(name string) ([]string, error) {
					assert.Equal(t, f.d.tmuxSession, name)
					lists++
					if mode == "list fails" || mode == "verification fails" && lists > 1 {
						return nil, errors.New("tmux unavailable")
					}
					if mode == "absent" || mode == "killed" && lists > 1 {
						return []string{"unrelated-window"}, nil
					}
					return []string{"unrelated-window", f.tkt.ID}, nil
				},
				kill: func(name, id string) error {
					assert.Equal(t, f.d.tmuxSession, name)
					assert.Equal(t, f.tkt.ID, id)
					kills++
					if mode == "kill fails" {
						return errors.New("kill refused")
					}
					return nil
				},
			}
			got := f.reconcile(t)
			if mode == "absent" || mode == "killed" {
				assert.Equal(t, ticket.StatusTodo, got.Status)
				assert.Empty(t, f.d.running)
				assert.Zero(t, f.d.sem.used)
				if mode == "absent" {
					assert.Zero(t, kills)
					assert.Equal(t, 1, lists)
				} else {
					assert.Equal(t, 1, kills)
					assert.Equal(t, 2, lists)
				}
				return
			}
			assert.Equal(t, ticket.StatusInProgress, got.Status)
			assert.Equal(t, f.rec.Invocation.ID, got.InvocationID)
			assert.Empty(t, got.History)
			assert.FileExists(t, executionPath(f.h.cfg, got.ID))
			require.Contains(t, f.d.running, got.ID)
			assert.Equal(t, 1, f.d.sem.used)
			common, err := worktree.CommonDir(f.h.repoDir)
			require.NoError(t, err)
			assert.Equal(t, got.ID, f.d.runningBranches[common+"\x00"+got.Branch])
			f.reconcile(t)
			assert.Len(t, f.d.running, 1)
			assert.Equal(t, 1, f.d.sem.used, "repeated recovery must not reserve another slot")
			assert.Len(t, f.d.runningBranches, 1)
		})
	}
}

func TestResultRecoveryRespectsLifecycleAndOwnership(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"invocation_id", "inv-newer"}, {"status", "paused"}, {"status", "done"},
		{"stage", "step1"}, {"pipeline", "one-stage"}, {"attempt", "3"},
		{"branch", "operator-branch"}, {"path", "/another/repository"},
		{"claimed_by", "another-instance"}, {"managed", ""},
	} {
		t.Run(tc.field+"="+tc.value, func(t *testing.T) {
			f := newResultFixture(t)
			// Raw insertion also exercises the empty imported managed marker.
			data, err := os.ReadFile(f.path)
			require.NoError(t, err)
			lines := strings.Split(string(data), "\n")
			replaced := false
			for i, line := range lines {
				if strings.HasPrefix(line, tc.field+":") {
					lines[i] = tc.field + ": " + tc.value
					replaced = true
				}
			}
			if !replaced {
				lines = append(lines[:1], append([]string{tc.field + ": " + tc.value}, lines[1:]...)...)
			}
			before := []byte(strings.Join(lines, "\n"))
			require.NoError(t, os.WriteFile(f.path, before, 0o644))
			f.d.windows.list = func(string) ([]string, error) {
				if tc.field == "claimed_by" || tc.field == "managed" || tc.field == "invocation_id" {
					t.Error("must not touch another execution's windows")
				}
				return nil, errors.New("termination remains unknown")
			}
			f.reconcile(t)
			after, err := os.ReadFile(f.path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.FileExists(t, executionPath(f.h.cfg, f.tkt.ID))
			if tc.field == "claimed_by" || tc.field == "managed" {
				assert.Empty(t, f.d.running)
				assert.Zero(t, f.d.sem.used)
			} else {
				// Operator edits do not prove that the original execution stopped.
				assert.Contains(t, f.d.running, f.tkt.ID)
				assert.Equal(t, 1, f.d.sem.used)
			}
		})
	}
}

func TestResultSupervisionScope(t *testing.T) {
	for _, tc := range []struct {
		name, childFields, parentFields, want string
		annotation, omitParent, wantError     bool
	}{
		{name: "standalone"},
		{name: "agent epic itself", childFields: "kind: epic\nepic_execution: agent\n", want: resultTicketID},
		{name: "direct supervised child", childFields: "parent: epc-parent\n", parentFields: "kind: epic\nepic_execution: agent\n", want: "epc-parent"},
		{name: "passive parent", childFields: "parent: epc-parent\n", parentFields: "kind: epic\n"},
		{name: "ordinary parent", childFields: "parent: epc-parent\n"},
		{name: "missing parent", childFields: "parent: epc-parent\n", omitParent: true},
		{name: "unsafe parent", childFields: "parent: ../epc-parent\n", omitParent: true, wantError: true},
		{name: "no ancestor inference", childFields: "parent: epc-parent\n", parentFields: "parent: epc-result\n"},
		{name: "annotation bypasses epic", childFields: "kind: epic\nepic_execution: agent\n", annotation: true},
		{name: "annotation bypasses child", childFields: "parent: epc-parent\n", parentFields: "kind: epic\nepic_execution: agent\n", annotation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResultFixture(t)
			if !tc.omitParent {
				f.h.writeTicket("epc-parent.md", "---\nid: epc-parent\nstatus: paused\n"+tc.parentFields+"---\n# Parent\n")
			}
			path := f.h.writeTicket("scope.md", "---\nid: "+resultTicketID+"\nstatus: in_progress\n"+tc.childFields+"---\n# Scope\n")
			tkt, err := ticket.ParseFile(path)
			require.NoError(t, err)
			got, err := f.d.supervision(tkt, tc.annotation)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	t.Run("malformed parent fails closed", func(t *testing.T) {
		f := newResultFixture(t)
		f.h.writeTicket("epc-result.md", "---\nstatus: [\n---\n")
		_, err := f.d.supervision(f.tkt, false)
		require.ErrorContains(t, err, "read supervision scope")
	})
}

func TestResultRecoveryLegacyScope(t *testing.T) {
	for _, supervised := range []bool{false, true} {
		t.Run(fmt.Sprintf("supervised=%t", supervised), func(t *testing.T) {
			f := newResultFixture(t)
			require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
			require.NoError(t, f.tkt.DeleteField("invocation_id"))
			if !supervised {
				require.NoError(t, f.tkt.DeleteField("parent"))
			}
			f.write(t)
			f.d.mu.Lock()
			handled := f.d.reconcileExecutionLocked(f.tkt, f.path)
			f.d.mu.Unlock()
			assert.Equal(t, supervised, handled, "standalone legacy tickets stay with the existing recovery path")
			got := f.h.readTask(resultTicketID + ".md")
			if supervised {
				assert.Equal(t, ticket.StatusTodo, got.Status)
			} else {
				assert.Equal(t, ticket.StatusInProgress, got.Status)
			}
			assert.Equal(t, 2, got.Attempt)
			assert.Equal(t, "step2", got.Stage)
			assert.Empty(t, got.History)
		})
	}
}

func TestResultRecoveryPinsSupervisionAcrossParentChanges(t *testing.T) {
	f := newResultFixture(t)
	got := f.reconcile(t)
	require.Equal(t, ticket.StatusTodo, got.Status)
	require.NoError(t, os.Remove(filepath.Join(f.h.tasksDir, "epc-result.md")))
	ep, err := f.d.invocationScope(got, spawnAgentParams{cfg: f.h.cfg})
	require.NoError(t, err)
	assert.Equal(t, "epc-result", ep, "removing supervision cannot turn a recovered run into standalone work")
	ep, err = f.d.invocationScope(got, spawnAgentParams{cfg: f.h.cfg, annotation: true})
	require.NoError(t, err)
	assert.Empty(t, ep, "annotation must not inherit recovered execution scope")
}

func TestResultBindingIsolatesInvocationsAndOverridesEnvironment(t *testing.T) {
	f := newResultFixture(t)
	p := spawnAgentParams{cfg: f.h.cfg, ticketID: f.tkt.ID, stageName: "step2", filePath: f.path, wtPath: f.h.wtDir}
	params := RunnerParams{Env: map[string]string{"KEEP": "configured"}}
	for key := range f.rec.Invocation.Env() {
		params.Env[key] = "configured-spoof"
	}
	first, err := f.d.bindExecution(f.tkt, p, &params, "epc-result")
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, first.Invocation.ID, f.h.readTask(resultTicketID+".md").InvocationID)
	assert.Equal(t, "configured", params.Env["KEEP"])
	for key, value := range first.Invocation.Env() {
		assert.Equal(t, value, params.Env[key])
	}
	assert.True(t, params.RequireTermination)
	require.NotNil(t, params.TerminalReported)
	assert.False(t, params.TerminalReported())
	publishDaemonResult(t, first.Invocation.ResultPath, "complete")
	assert.True(t, params.TerminalReported())

	secondParams := RunnerParams{Env: map[string]string{}}
	second, err := f.d.bindExecution(f.tkt, p, &secondParams, "epc-result")
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.NotEqual(t, first.Invocation.ID, second.Invocation.ID)
	assert.NotEqual(t, first.Invocation.ResultPath, second.Invocation.ResultPath)
	assert.False(t, secondParams.TerminalReported())
	run := agentRun{Result: process.Result{ExitCode: 0}}
	classifyExecution(second, &run, "")
	assert.Equal(t, "protocol_error", run.Outcome, "old completion cannot satisfy the new invocation")
	assert.FileExists(t, first.Invocation.ResultPath)

	standalone := RunnerParams{Env: first.Invocation.Env()}
	rec, err := f.d.bindExecution(f.tkt, p, &standalone, "")
	require.NoError(t, err)
	assert.Nil(t, rec)
	for key := range first.Invocation.Env() {
		assert.Empty(t, standalone.Env[key], "standalone must clear inherited %s", key)
	}
	assert.False(t, standalone.RequireTermination)
	assert.Nil(t, standalone.TerminalReported)
}

func TestResultSupervisedPipelineOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, report, failure, onFailure, wantStage, wantOutcome string
		exit, attempt, wantAttempt                               int
		want                                                     ticket.Status
		hookReject, providerReject                               bool
	}{
		{name: "missing pauses rather than retries", onFailure: "retry", attempt: 1, wantAttempt: 1, want: ticket.StatusPaused, wantStage: "step2", wantOutcome: "protocol_error"},
		{name: "blocked preserves retry count", report: "blocked", onFailure: "retry", attempt: 1, wantAttempt: 1, want: ticket.StatusPaused, wantStage: "step2", wantOutcome: "blocked"},
		{name: "blocked does not go back", report: "blocked", onFailure: "back", attempt: 1, wantAttempt: 1, want: ticket.StatusPaused, wantStage: "step2", wantOutcome: "blocked"},
		{name: "complete", report: "complete", want: ticket.StatusDone, wantStage: "step2", wantOutcome: "complete"},
		{name: "failed retains retry behavior", report: "complete", exit: 4, onFailure: "retry", attempt: 1, wantAttempt: 2, want: ticket.StatusTodo, wantStage: "step2", wantOutcome: "failed"},
		{name: "failed retains back behavior", report: "complete", exit: 4, onFailure: "back", attempt: 1, want: ticket.StatusTodo, wantStage: "step1", wantOutcome: "failed"},
		{name: "runner rejects complete", report: "complete", failure: "runner transport failed", onFailure: "retry", attempt: 1, wantAttempt: 1, want: ticket.StatusPaused, wantStage: "step2", wantOutcome: "failed"},
		{name: "provider rejects complete", report: "complete", providerReject: true, onFailure: "retry", attempt: 1, wantAttempt: 1, want: ticket.StatusPaused, wantStage: "step2", wantOutcome: "failed"},
		{name: "hook rejects complete", report: "complete", hookReject: true, onFailure: "back", attempt: 1, wantAttempt: 1, want: ticket.StatusPaused, wantStage: "step2", wantOutcome: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.cfg
			onFailure := tc.onFailure
			if onFailure == "" {
				onFailure = "pause"
			}
			cfg.Pipelines["two-stage"][1].OnFailure = onFailure
			cfg.Pipelines["two-stage"][1].MaxRetries = 3
			cfg.Agents["agent2"] = config.Agent{Binary: "true", FailurePatterns: []string{"provider unavailable"}}
			if tc.hookReject {
				cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "reject", Run: "exit 1", OnFailure: "pause"}}}
			}
			h.writeTicket("epc-result.md", "---\nid: epc-result\nkind: epic\nepic_execution: agent\nstatus: paused\n---\n# Supervisor\n")
			md := strings.Replace(h.taskMD(resultTicketID, "todo", "two-stage"), "status: todo\n",
				fmt.Sprintf("status: todo\nparent: epc-result\nstage: step2\nattempt: %d\n", tc.attempt), 1)
			path := h.writeTicket(resultTicketID+".md", md)
			calls := 0
			var invocation session.Invocation
			var wtPath string
			runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls++
				var err error
				invocation, err = session.InvocationFromEnv(func(key string) string { return p.Env[key] })
				require.NoError(t, err)
				assert.Equal(t, resultTicketID, invocation.TicketID)
				assert.Equal(t, "step2", invocation.Stage)
				assert.True(t, p.RequireTermination)
				if tc.report != "" {
					publishDaemonResult(t, invocation.ResultPath, tc.report)
				}
				wtPath = p.Dir
				require.NoError(t, os.WriteFile(filepath.Join(p.Dir, "agent-work.txt"), []byte("keep this work\n"), 0o644))
				if tc.providerReject {
					require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
					require.NoError(t, os.WriteFile(p.LogFile, []byte("provider unavailable\n"), 0o644))
				}
				now := time.Now()
				var runnerErr error
				if tc.failure != "" {
					runnerErr = errors.New(tc.failure)
				}
				return process.Result{ExitCode: tc.exit, StartedAt: now, ExitedAt: now}, runnerErr
			}
			d, collect := h.newMetricsDaemon(cfg, WithRunner(runner))
			d.tickets[resultTicketID] = newTicketState(h.readTask(resultTicketID+".md"), path)
			d.runTicket(context.Background(), resultTicketID)
			d.background.Wait()
			metricOutcome := tc.wantOutcome
			if metricOutcome == "complete" {
				metricOutcome = "success"
			}
			if metricOutcome == "failed" {
				metricOutcome = "failure"
			}
			assert.Equal(t, map[string]int64{metricOutcome: 1}, sumByAttr(t, collect()["kontora.stage.runs"], "outcome"))
			assert.Equal(t, 1, calls)
			got := h.readTask(resultTicketID + ".md")
			assert.Equal(t, tc.want, got.Status)
			assert.Equal(t, tc.wantStage, got.Stage)
			assert.Equal(t, tc.wantAttempt, got.Attempt)
			assert.Empty(t, got.InvocationID)
			require.Len(t, got.History, 1)
			assert.Equal(t, invocation.ID, got.History[0].InvocationID)
			assert.Equal(t, ticket.RunOutcome(tc.wantOutcome), got.History[0].RunOutcome)
			assert.Equal(t, tc.exit, got.History[0].ExitCode)
			assert.Equal(t, "agent2", got.History[0].Agent)
			if tc.want == ticket.StatusPaused {
				assert.NotEmpty(t, got.LastError)
				assert.NotEmpty(t, got.History[0].OutcomeReason)
				assert.Nil(t, got.CompletedAt)
			}
			assert.FileExists(t, filepath.Join(wtPath, "agent-work.txt"), "even accepted dirty work must be retained")
			assert.NoFileExists(t, executionPath(cfg, resultTicketID))
			assert.Empty(t, d.running)
		})
	}
}

func TestResultStandaloneKeepsDirtyFilesWithoutReport(t *testing.T) {
	h := newHarness(t)
	path := h.writeTicket(resultTicketID+".md", simpleTaskMD(resultTicketID, "todo", h.repoDir))
	calls := 0
	var wtPath string
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls++
		for key := range (session.Invocation{}).Env() {
			assert.Empty(t, p.Env[key])
		}
		assert.False(t, p.RequireTermination)
		assert.Nil(t, p.TerminalReported)
		wtPath = p.Dir
		require.NoError(t, os.WriteFile(filepath.Join(p.Dir, "unfinished.txt"), []byte("not committed\n"), 0o644))
		now := time.Now()
		return process.Result{StartedAt: now, ExitedAt: now}, nil
	}))
	d.tickets[resultTicketID] = newTicketState(h.readTask(resultTicketID+".md"), path)
	d.runTicket(context.Background(), resultTicketID)
	assert.Equal(t, 1, calls)
	got := h.readTask(resultTicketID + ".md")
	assert.Equal(t, ticket.StatusDone, got.Status)
	assert.Empty(t, got.InvocationID)
	assert.Empty(t, got.LastError)
	assert.FileExists(t, filepath.Join(wtPath, "unfinished.txt"))
	assert.NoFileExists(t, executionPath(h.cfg, resultTicketID))
}

func TestResultRecoveryMissingTransitionRetainsEvidence(t *testing.T) {
	f := newResultFixture(t)
	f.rec.Phase = "transition"
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	got := f.reconcile(t)
	assert.Equal(t, ticket.StatusPaused, got.Status)
	assert.Contains(t, got.LastError, "missing transition")
	assert.Empty(t, got.History)
	assert.Equal(t, f.rec.Invocation.ID, got.InvocationID)
	assert.FileExists(t, executionPath(f.h.cfg, got.ID), "keep the broken transition for operator inspection")
	assert.Empty(t, f.d.running)
}

func TestResultExitGuardsBeforeAndAfterHooks(t *testing.T) {
	for _, timing := range []string{"before hooks", "during hooks"} {
		for _, tc := range []struct{ field, value string }{
			{"invocation_id", "inv-newer"}, {"claimed_by", "other-instance"}, {"managed", ""},
			{"stage", "step1"}, {"attempt", "3"}, {"status", "paused"},
		} {
			t.Run(timing+"/"+tc.field, func(t *testing.T) {
				f := newResultFixture(t)
				publishDaemonResult(t, f.rec.Invocation.ResultPath, "complete")
				run := agentRun{Result: process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}}
				classifyExecution(f.rec, &run, "")
				original, err := os.ReadFile(f.path)
				require.NoError(t, err)
				// Include null imported ownership without ordinary write guards.
				lines := strings.Split(string(original), "\n")
				out := []string{lines[0], tc.field + ": " + tc.value}
				inFrontmatter := true
				for _, line := range lines[1:] {
					if line == "---" {
						inFrontmatter = false
					}
					if inFrontmatter && strings.HasPrefix(line, tc.field+":") {
						continue
					}
					out = append(out, line)
				}
				changed := []byte(strings.Join(out, "\n"))
				marker := filepath.Join(t.TempDir(), "hook-ran")
				hookCmd := fmt.Sprintf("printf ran > %q", marker)
				if timing == "before hooks" {
					require.NoError(t, os.WriteFile(f.path, changed, 0o644))
				} else {
					replacement := filepath.Join(t.TempDir(), "replacement.md")
					require.NoError(t, os.WriteFile(replacement, changed, 0o644))
					hookCmd += fmt.Sprintf("; cp %q \"$KONTORA_TICKET_FILE\"", replacement)
				}
				f.h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "change-owner", Run: hookCmd}}}
				f.d.handleAgentExit(context.Background(), context.Background(), handleExitParams{
					cfg: f.h.cfg, log: testLogger(t), ticketID: f.tkt.ID, filePath: f.path,
					stageName: "step2", agentName: "agent2", run: 1, runData: run, result: run.Result,
					pipelineCfg: f.h.cfg.Pipelines["two-stage"], repoPath: f.h.repoDir, wtPath: f.h.wtDir,
				})
				got, err := os.ReadFile(f.path)
				require.NoError(t, err)
				assert.Equal(t, changed, got, "late completion must not overwrite a newer lifecycle or owner")
				assert.Empty(t, f.h.readTask(resultTicketID+".md").History)
				assert.FileExists(t, executionPath(f.h.cfg, f.tkt.ID), "a discarded result must not retire another execution")
				assert.DirExists(t, f.h.wtDir)
				if timing == "before hooks" {
					assert.NoFileExists(t, marker, "hooks may not run for an already superseded invocation")
				} else {
					assert.FileExists(t, marker)
				}
			})
		}
	}
}

func TestResultRecoveryReservesCorrectPool(t *testing.T) {
	for _, epic := range []bool{false, true} {
		t.Run(fmt.Sprintf("epic=%t", epic), func(t *testing.T) {
			f := newResultFixture(t)
			f.h.cfg.MaxConcurrentAgents = 1
			f.h.cfg.MaxConcurrentEpicSupervisors = 1
			f.d = f.h.newDaemon(f.h.cfg)
			if epic {
				require.NoError(t, f.tkt.SetField("kind", "epic"))
				require.NoError(t, f.tkt.SetField("epic_execution", "agent"))
				f.write(t)
				f.rec.EpicID = f.tkt.ID
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
			}
			f.d.windows = windowOps{
				list: func(string) ([]string, error) { return nil, errors.New("server unreachable") },
				kill: func(string, string) error { t.Fatal("list failed"); return nil },
			}
			for range 2 {
				got := f.reconcile(t)
				assert.Equal(t, ticket.StatusInProgress, got.Status)
			}
			assert.Len(t, f.d.running, 1)
			if epic {
				assert.Equal(t, 1, f.d.epicSem.used)
				assert.Zero(t, f.d.sem.used, "a supervisor cannot consume an ordinary worker slot")
			} else {
				assert.Equal(t, 1, f.d.sem.used)
				assert.Zero(t, f.d.epicSem.used)
			}
		})
	}
}

func TestResultUncertainRunnerRetainsExecutionAndWork(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("epc-result.md", "---\nid: epc-result\nkind: epic\nepic_execution: agent\nstatus: paused\n---\n# Supervisor\n")
	md := strings.Replace(h.taskMD(resultTicketID, "todo", "one-stage"), "status: todo\n", "status: todo\nparent: epc-result\n", 1)
	path := h.writeTicket(resultTicketID+".md", md)
	calls := 0
	var inv session.Invocation
	var wtPath string
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls++
		var err error
		inv, err = session.InvocationFromEnv(func(key string) string { return p.Env[key] })
		require.NoError(t, err)
		publishDaemonResult(t, inv.ResultPath, "complete")
		wtPath = p.Dir
		require.NoError(t, os.WriteFile(filepath.Join(p.Dir, "unconfirmed.txt"), []byte("work\n"), 0o644))
		now := time.Now()
		return process.Result{StartedAt: now, ExitedAt: now, ExitCode: -1}, process.ErrTerminationUncertain
	}))
	d.tickets[resultTicketID] = newTicketState(h.readTask(resultTicketID+".md"), path)
	d.runTicket(context.Background(), resultTicketID)
	assert.Equal(t, 1, calls)
	got := h.readTask(resultTicketID + ".md")
	assert.Equal(t, ticket.StatusInProgress, got.Status)
	assert.Equal(t, inv.ID, got.InvocationID)
	assert.Empty(t, got.History, "even a complete artifact cannot certify an unconfirmed shutdown")
	assert.Contains(t, d.running, got.ID)
	assert.Len(t, d.runningBranches, 1)
	assert.Contains(t, d.liveRuns, got.ID)
	assert.FileExists(t, filepath.Join(wtPath, "unconfirmed.txt"))
	rec, err := loadExecution(h.cfg, got.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", rec.Phase)
	assert.Equal(t, inv.ID, rec.Invocation.ID)
}
