package daemon

import (
	"context"
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
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func (h *testHarness) agentEpicMD(id, status string, children ...string) string {
	return strings.Replace(h.epicMD(id, status, children...), "kind: epic\n", "kind: epic\nepic_execution: agent\n", 1)
}

func newEpicAgentHarness(t *testing.T) *testHarness {
	t.Helper()
	h := newHarness(t)
	// In-memory test configs do not pass through config.Load's defaults.
	h.cfg.MaxConcurrentEpicSupervisors = 4
	return h
}

// Report completion so child-validation tests cannot pass on a protocol pause.
func completeEpicInvocation(t *testing.T, p RunnerParams) {
	t.Helper()
	inv, err := session.InvocationFromEnv(func(key string) string { return p.Env[key] })
	require.NoError(t, err)
	_, err = session.PublishResult(inv.ResultPath, session.Result{Outcome: "complete", Summary: "Assigned stage work complete."})
	require.NoError(t, err)
}

func awaitEpicAgent(t *testing.T, calls <-chan RunnerParams) RunnerParams {
	t.Helper()
	select {
	case p := <-calls:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("epic agent did not start")
		return RunnerParams{}
	}
}

func epicBlockingRunner(calls chan<- RunnerParams) RunnerFunc {
	return func(ctx context.Context, p RunnerParams) (process.Result, error) {
		calls <- p
		<-ctx.Done()
		return process.Result{ExitCode: -1, StartedAt: time.Now(), ExitedAt: time.Now()}, ctx.Err()
	}
}

func waitEpicIndexed(t *testing.T, d *Daemon, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := d.GetTicket(id)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
}

func TestEpicAgentExplicitRunActivatesPassiveEpic(t *testing.T) {
	cases := []struct {
		name, childStatus string
		derived           ticket.Status
	}{
		{"open", "open", ticket.StatusOpen},
		{"in progress", "human_review", ticket.StatusInProgress},
		{"done", "done", ticket.StatusDone},
		{"cancelled", "cancelled", ticket.StatusCancelled},
		{"archived", "done", ticket.StatusArchived},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.AutoPickUp = new(false)
			calls := make(chan RunnerParams, 2)
			d := h.newDaemon(h.cfg, WithRunner(epicBlockingRunner(calls)))
			md := h.epicMD("epc-a", "open", "epc-c1")
			if tc.derived == ticket.StatusArchived {
				md = h.epicMD("epc-a", "archived", "epc-c1")
			}
			h.writeTicket("epc-a.md", md)
			childMD := h.epicChildMD("epc-c1", tc.childStatus, "epc-a", "2026-01-02T00:00:00Z")
			childPath := h.writeTicket("epc-c1.md", childMD)
			stop := startDaemon(t, d)
			defer stop()
			h.waitForStatus("epc-a.md", tc.derived, 5*time.Second)

			err := d.RunTicket("epc-a")
			if tc.derived == ticket.StatusArchived {
				require.ErrorIs(t, err, web.ErrInvalidState)
				assert.True(t, h.readTask("epc-a.md").IsPassiveEpic())
				assert.Equal(t, ticket.StatusArchived, h.readTask("epc-a.md").Status)
				assert.Empty(t, calls)
			} else {
				require.NoError(t, err)
				p := awaitEpicAgent(t, calls)
				assert.Equal(t, "epc-a", p.TicketID)
				got := h.waitForStatus("epc-a.md", ticket.StatusInProgress, 5*time.Second)
				assert.True(t, got.IsAgentEpic())
				assert.Equal(t, "agent", got.EpicExecution)
				require.NoError(t, d.PauseTicket("epc-a"))
				h.waitForStatus("epc-a.md", ticket.StatusPaused, 5*time.Second)
				waitForAgentsDone(t, d, 5*time.Second)
				paused := h.readTask("epc-a.md")
				assert.True(t, paused.IsAgentEpic(), "pause must not revert to passive mode")
				require.Len(t, paused.History, 1, "confirmed user stops retain the invocation history")
				assert.Equal(t, "default", paused.History[0].Stage)
				cost, err := d.GetTicketCost("epc-a")
				require.NoError(t, err)
				assert.Equal(t, 1, cost.TrackedRuns)
			}
			childBytes, err := os.ReadFile(childPath)
			require.NoError(t, err)
			assert.Equal(t, childMD, string(childBytes), "run and pause affect the supervisor only")
		})
	}
}

func TestEpicAgentChildChangesDoNotDeriveSupervisorStatus(t *testing.T) {
	for _, status := range []string{"todo", "in_progress", "paused", "human_review", "done", "cancelled", "archived"} {
		t.Run(status, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.AutoPickUp = new(false)
			d := h.newDaemon(h.cfg)
			stop := startDaemon(t, d)
			defer stop()
			// Seed after recovery so in_progress is an intentional live state.
			d.handleFileChanged(h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", status, "epc-c1")))
			waitEpicIndexed(t, d, "epc-a")
			for _, childStatus := range []string{"open", "done", "cancelled"} {
				d.handleFileChanged(h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", childStatus, "epc-a", "2026-01-02T00:00:00Z")))
				got := h.readTask("epc-a.md")
				assert.Equal(t, ticket.Status(status), got.Status)
				assert.True(t, got.IsAgentEpic())
			}
			assert.Empty(t, ticket.ParseNotes(h.readTask("epc-a.md").Body), "no passive auto-close note")
		})
	}
}

func TestEpicAgentSuccessfulExitChecksChildrenAndKeepsWorktree(t *testing.T) {
	cases := []struct {
		name     string
		children []string
		want     ticket.Status
		initial  string
		report   string // empty means complete
	}{
		{"done", []string{"done"}, ticket.StatusHumanReview, "open", ""},
		{"legacy closed", []string{"closed"}, ticket.StatusHumanReview, "open", ""},
		{"all done", []string{"done", "done"}, ticket.StatusHumanReview, "open", ""},
		{"empty", nil, ticket.StatusPaused, "open", ""},
		{"open", []string{"open"}, ticket.StatusPaused, "open", ""},
		{"cancelled", []string{"cancelled"}, ticket.StatusPaused, "open", ""},
		{"archived", []string{"archived"}, ticket.StatusPaused, "open", ""},
		{"done and cancelled", []string{"done", "cancelled"}, ticket.StatusPaused, "open", ""},
		{"done and archived", []string{"done", "archived"}, ticket.StatusPaused, "open", ""},
		{"waiting for review", []string{"human_review"}, ticket.StatusHumanReview, "open", ""},
		{"paused", []string{"paused"}, ticket.StatusPaused, "open", ""},
		{"restart done epic", []string{"done"}, ticket.StatusHumanReview, "done", ""},
		{"restart with unfinished child", []string{"open"}, ticket.StatusPaused, "done", ""},
		{"missing result", []string{"done"}, ticket.StatusPaused, "open", "missing"},
		{"invalid result", []string{"done"}, ticket.StatusPaused, "open", "invalid"},
		{"blocked result", []string{"done"}, ticket.StatusPaused, "open", "blocked"},
		{"restart missing result", []string{"done"}, ticket.StatusPaused, "done", "missing"},
		{"restart blocked result", []string{"done"}, ticket.StatusPaused, "done", "blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.AutoPickUp = new(false)
			var runs annotationRun
			d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				runs.record(p)
				assert.Nil(t, h.readTask("epc-a.md").CompletedAt)
				switch tc.report {
				case "missing":
				case "invalid":
					require.NoError(t, os.MkdirAll(filepath.Dir(p.Env[session.ResultFileEnvVar]), 0o755))
					require.NoError(t, os.WriteFile(p.Env[session.ResultFileEnvVar], []byte(`{"outcome":"complete"}`), 0o600))
				case "blocked":
					_, err := session.PublishResult(p.Env[session.ResultFileEnvVar], session.Result{Outcome: "blocked", Summary: "Device evidence unavailable.", NextStep: "Provide device access."})
					require.NoError(t, err)
				default:
					completeEpicInvocation(t, p)
				}
				now := time.Now()
				return process.Result{ExitCode: 0, StartedAt: now, ExitedAt: now}, nil
			}))
			var ids []string
			for i, status := range tc.children {
				id := fmt.Sprintf("epc-c%d", i+1)
				ids = append(ids, id)
				h.writeTicket(id+".md", h.epicChildMD(id, status, "epc-a", "2026-01-02T00:00:00Z"))
			}
			md := h.agentEpicMD("epc-a", tc.initial, ids...)
			if tc.initial == "done" {
				md = strings.Replace(md, "status: done\n", "status: done\ncompleted_at: 2026-01-03T00:00:00Z\n", 1)
			}
			h.writeTicket("epc-a.md", md)
			stop := startDaemon(t, d)
			defer stop()
			waitEpicIndexed(t, d, "epc-a")
			if tc.initial == "done" {
				require.NotNil(t, h.readTask("epc-a.md").CompletedAt)
				require.NoError(t, d.RetryTicket("epc-a"))
			} else {
				require.NoError(t, d.RunTicket("epc-a"))
			}
			got := h.waitForStatus("epc-a.md", tc.want, 5*time.Second)
			waitForAgentsDone(t, d, 5*time.Second)
			assert.True(t, got.IsAgentEpic())
			assert.Nil(t, got.CompletedAt)
			assert.Empty(t, got.Pipeline)
			require.Len(t, got.History, 1)
			assert.Equal(t, "default", got.History[0].Stage)
			assert.Equal(t, "agent1", got.History[0].Agent)
			assert.Zero(t, got.History[0].ExitCode)
			require.Len(t, runs.all(), 1)
			assert.DirExists(t, runs.all()[0].Dir)
			wt, err := worktree.FindWorktreeForBranch(h.repoDir, got.Branch)
			require.NoError(t, err)
			assert.NotEmpty(t, wt)
			if tc.want == ticket.StatusPaused {
				assert.NotEmpty(t, got.LastError)
				assert.NotEqual(t, "complete", string(got.History[0].RunOutcome), "child validation and protocol pauses are not completed delivery")
			} else {
				assert.Empty(t, got.LastError)
				assert.Equal(t, "complete", string(got.History[0].RunOutcome))
			}
			if tc.report == "blocked" {
				assert.Equal(t, "blocked", string(got.History[0].RunOutcome))
				assert.Contains(t, got.LastError, "Provide device access.")
			}
			if tc.report == "missing" || tc.report == "invalid" {
				assert.Equal(t, "protocol_error", string(got.History[0].RunOutcome))
			}
			assert.Zero(t, got.Attempt)
			require.NotEmpty(t, got.History[0].InvocationID)
		})
	}
}

func TestEpicAgentCompletionRefreshesChildren(t *testing.T) {
	for _, tc := range []struct {
		before, after string
		want          ticket.Status
	}{
		{"open", "done", ticket.StatusHumanReview},
		{"done", "open", ticket.StatusPaused},
	} {
		t.Run(tc.before+" to "+tc.after, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			childPath := h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", tc.before, "epc-a", "2026-01-02T00:00:00Z"))
			var d *Daemon
			d = h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				completeEpicInvocation(t, p)
				data := []byte(h.epicChildMD("epc-c1", tc.after, "epc-a", "2026-01-02T00:00:00Z"))
				// Deliberately suppress the watcher so a cached child snapshot is
				// guaranteed to be stale when the runner returns.
				d.recordSelfWrite(childPath, data)
				require.NoError(t, os.WriteFile(childPath, data, 0o644))
				now := time.Now()
				return process.Result{ExitCode: 0, StartedAt: now, ExitedAt: now}, nil
			}))
			// Children are discovered by parent links, not by trusting an epic's
			// possibly stale children list (intentionally omitted here).
			h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", "todo"))
			stop := startDaemon(t, d)
			defer stop()
			h.waitForStatus("epc-a.md", tc.want, 5*time.Second)
			assert.Equal(t, ticket.Status(tc.after), h.readTask("epc-c1.md").Status)
		})
	}
}

func TestEpicAgentIntegratesChildBranches(t *testing.T) {
	h := newEpicAgentHarness(t)
	for _, branch := range []string{"first", "second"} {
		mustGit(t, h.repoDir, "checkout", "-b", branch, "main")
		commitFile(t, h.repoDir, branch+".txt", branch+" result\n", "Child "+branch)
		h.writeTicket("epc-"+branch+".md", h.epicChildMD("epc-"+branch, "done", "epc-a", "2026-01-02T00:00:00Z"))
	}
	mustGit(t, h.repoDir, "checkout", "main")
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		completeEpicInvocation(t, p)
		assert.Contains(t, strings.Join(p.Args, " "), "Serialize integration-branch writes")
		for _, branch := range []string{"first", "second"} {
			mustGit(t, p.Dir, "merge", "--ff", "--no-edit", branch)
		}
		for _, branch := range []string{"first", "second"} {
			data, err := os.ReadFile(filepath.Join(p.Dir, branch+".txt"))
			require.NoError(t, err)
			assert.Equal(t, branch+" result\n", string(data))
			mustGit(t, p.Dir, "merge-base", "--is-ancestor", branch, "HEAD")
		}
		now := time.Now()
		return process.Result{ExitCode: 0, StartedAt: now, ExitedAt: now}, nil
	}))
	h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", "todo"))
	stop := startDaemon(t, d)
	defer stop()
	got := h.waitForStatus("epc-a.md", ticket.StatusHumanReview, 5*time.Second)
	waitForAgentsDone(t, d, 5*time.Second)
	wt, err := worktree.FindWorktreeForBranch(h.repoDir, got.Branch)
	require.NoError(t, err)
	require.NotEmpty(t, wt)
	require.Len(t, got.History, 1)
	mustGit(t, wt, "merge-base", "--is-ancestor", "first", "HEAD")
	mustGit(t, wt, "merge-base", "--is-ancestor", "second", "HEAD")
}

func TestEpicAgentRoleSurvivesFreshAndCustomResumePrompts(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", resume), func(t *testing.T) {
			rd := newResumeDaemon(t, "claude", func(_ context.Context, _ int, p RunnerParams) (process.Result, error) {
				completeEpicInvocation(t, p)
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now}, nil
			})
			rd.cfg.ResumePrompt = "Custom continuation for {{ .Ticket.ID }}"
			rd.h.writeTicket("epc-c1.md", rd.h.epicChildMD("epc-c1", "done", resumeTicketID, "2026-01-02T00:00:00Z"))
			if resume {
				plantClaudeSession(t, rd.claudeDir, resumeTestSessionID)
				writeRecordFile(t, rd.cfg, resumeTicketID, simpleStageName, resumeRecord{
					SessionID: resumeTestSessionID, Stage: simpleStageName, Agent: agentKindClaude,
					Worktree: rd.wtPath, Instance: rd.d.instanceName, StartedAt: time.Now().Add(-time.Minute),
				})
			}
			rd.runWith(t, rd.h.agentEpicMD(resumeTicketID, "todo", "epc-c1"), ticket.StatusHumanReview)
			spawns := rd.invocations(t)
			require.Len(t, spawns, 1)
			got := renderedPrompt(spawns[0])
			assert.Contains(t, got, "## Epic supervisor")
			assert.Contains(t, got, "kontora skills show")
			assert.Contains(t, got, "## Supervised local delivery")
			assert.Contains(t, got, "## Mandatory supervised result")
			assert.Contains(t, got, "## Prerequisites and escalation")
			assert.NotContains(t, got, "rebase local branches owned by this epic")
			for _, clause := range []string{
				"Create children with --parent and omit --path to inherit the epic's repository path and its project defaults.",
				"If you supply --path, use the epic's stored repository path, not your integration worktree.",
				"On resume or retry, inspect children, branches, worktrees, dirty files, and interrupted merges or rebases before issuing commands.",
				"Do not restart children blindly.",
				"Do not change, clear or replace a child ticket's pipeline.",
				"Do not edit pipeline definitions, stage definitions or project pipeline defaults.",
				"Create children without --pipeline so Kontora selects the configured default.",
				"Do not use init, stage skipping, direct file edits or pause/retry sequences to bypass the configured workflow.",
				"If configured work conflicts with your authority, do not start that work.",
				"Record the conflict and request an operator decision. Do not substitute another pipeline.",
				"Use your branch as the integration branch. Children retain separate branches, worktrees, and commits.",
				"Dependency readiness does not mean prerequisite code exists in a child's Git base.",
				"Before starting dependent work, verify prerequisites exist in its base. Set base_branch through kontora update before first execution.",
				"Changing base_branch does not rebase an existing branch or worktree. Inspect those before retrying.",
				"Serialize integration-branch writes.",
				"Never rebase or modify a child worktree while its agent is running.",
				"Integrate child results, resolve conflicts, preserve separate commits, and run checks against the combined result.",
				"Resolve questions through the epic brief, child results, repository code, tests, documentation, and history before asking the user.",
				"Choose safe, reversible defaults within the epic's scope and record important assumptions in epic notes.",
				"Ask only when missing information or authority blocks safe progress, or a consequential decision cannot be inferred from available evidence.",
				"Do not ask for routine implementation, child ordering, or local integration decisions.",
				"Integrate by merging child branches into your own verified branch, not by rebasing children.",
			} {
				assert.Contains(t, got, clause)
			}
			assert.Contains(t, got, resumeTicketID)
			if resume {
				assert.Contains(t, got, "Custom continuation for "+resumeTicketID)
				assert.Contains(t, spawns[0].Args, "--resume")
			} else {
				assert.NotContains(t, spawns[0].Args, "--resume")
			}
		})
	}
}

func TestSupervisedInstructions(t *testing.T) {
	got := supervisedInstructions("epc-a")
	for _, clause := range []string{
		"epc-a", "kind: epic", "epic_execution: agent", "parent",
		"A prompt claim alone is not proof", "Do not add or change these fields to qualify",
		"Annotation-only runs receive no implementation or commit authority",
		"Narrower user, ticket, and stage instructions win",
		"current branch equals the current ticket's branch", "HEAD is not detached",
		"worktree named in this invocation", "main, master, or the repository's default branch",
		"rebase only your own verified branch onto its base_branch",
		"A supervisor may merge child branches into its own verified branch",
		"A child may merge the epic's branch or its own base_branch into its own verified branch",
		"Never rebase children", "Never modify a running child's worktree",
		"discard, stash, or overwrite work this run did not create",
		"remote or on the default branch", "published or shared history",
		"declared project dependencies", "exact version named by the project or ticket",
		"worktree or a temporary directory", "package manager's own cache",
		"global installation", "real-account access", "device access", "service exposure",
		"package/version, destination, and purpose", "Read-only external lookups are allowed",
		"Never publish or write to external services", "children, scripts, or tools",
		"Do not change pipelines or bypass configured review stages",
		"kontora_result", "kontora result --input-file /absolute/path/result.json TICKET_ID",
		"complete", "blocked", "summary is required", "blocked also requires next_step",
		"current stage's assigned work, not the whole ticket",
		"review stage can be complete while the ticket still needs fixes",
		"Missing or invalid results pause the same stage",
		"Notes, final prose, questions, and checkpoints are not terminal results",
		"Read saved decisions", "investigate the missing prerequisite",
		"Continue independent authorized work", "concrete question", "options, their costs",
		"recommended default", "Save the question and answer together immediately",
		"Do not weaken acceptance requirements", "Do not substitute emulation",
		"Retry requires explicit operator action", "Saving an answer", "dismissing attention",
		"children, branches, worktrees, dirty files, and interrupted Git operations",
	} {
		assert.Contains(t, got, clause)
	}
	assert.NotContains(t, got, "rebase local branches owned by this epic")
	assert.NotContains(t, got, "kontora retry TICKET_ID")
	assert.Empty(t, supervisedInstructions(""), "absent supervision must not grant authority")
}

func TestEpicAgentAnnotationExcludesSupervisorRole(t *testing.T) {
	h := newPlannotatorHarness(t)
	h.cfg.MaxConcurrentEpicSupervisors = 4
	h.cfg.AnnotationPrompt = "Apply annotations: {{ plannotatorAnnotations }}"
	var runs annotationRun
	d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		now := time.Now()
		return process.Result{ExitCode: 0, StartedAt: now, ExitedAt: now}, nil
	})
	require.NoError(t, os.WriteFile(annotationsPath(h.reviewsDir, "epc-a"), []byte("Clarify the epic scope."), 0o644))
	md := strings.Replace(h.agentEpicMD("epc-a", "todo"), "epic_execution: agent\n", "epic_execution: agent\nannotation_return_status: open\n", 1)
	_, stop := startAnnotationDaemon(t, h, d, "epc-a", md)
	defer stop()
	got := h.waitForAnnotationRuns("epc-a", 1, ticket.StatusOpen)
	require.Len(t, runs.all(), 1)
	p := renderedPrompt(runs.all()[0])
	assert.Contains(t, p, "Clarify the epic scope.")
	assert.NotContains(t, strings.ToLower(p), "epic supervisor")
	assert.NotContains(t, p, "## Supervised local delivery")
	assert.True(t, got.IsAgentEpic())
	assert.Empty(t, got.AnnotationReturnStatus)
}

func TestEpicAgentFailuresPause(t *testing.T) {
	cases := []struct {
		name, hook, wantError string
		exit                  int
		runnerErr             error
		providerError         bool
		wantRuns              int
	}{
		{name: "nonzero exit", exit: 7, wantError: "code 7", wantRuns: 1},
		{name: "runner failure", runnerErr: assert.AnError, wantError: "runner failed", wantRuns: 1},
		{name: "provider failure", providerError: true, wantError: "authentication token is expired", wantRuns: 1},
		{name: "worktree hook", hook: config.HookWorktreeCreated, wantError: "hook bootstrap", wantRuns: 0},
		{name: "start hook", hook: config.HookStageStart, wantError: "hook bootstrap", wantRuns: 0},
		{name: "end hook", hook: config.HookStageEnd, wantError: "hook bootstrap", wantRuns: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.Agents["agent1"] = config.Agent{Binary: "pi", FailurePatterns: []string{}}
			if tc.hook != "" {
				h.cfg.Hooks = config.Hooks{tc.hook: {{Name: "bootstrap", Run: "exit 1", OnFailure: config.HookOnFailurePause}}}
			}
			var runs annotationRun
			d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				runs.record(p)
				completeEpicInvocation(t, p)
				if tc.providerError {
					require.NoError(t, os.MkdirAll(p.SessionDir, 0o755))
					line := fmt.Sprintf(`{"type":"message","timestamp":%q,"message":{"role":"assistant","stopReason":"error","errorMessage":"Provided authentication token is expired."}}`, time.Now().UTC().Format(time.RFC3339Nano))
					require.NoError(t, os.WriteFile(filepath.Join(p.SessionDir, "session.jsonl"), []byte(line+"\n"), 0o644))
				}
				now := time.Now()
				return process.Result{ExitCode: tc.exit, StartedAt: now, ExitedAt: now}, tc.runnerErr
			}))
			h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "done", "epc-a", "2026-01-02T00:00:00Z"))
			h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", "todo", "epc-c1"))
			stop := startDaemon(t, d)
			defer stop()
			got := h.waitForStatus("epc-a.md", ticket.StatusPaused, 5*time.Second)
			waitForAgentsDone(t, d, 5*time.Second)
			assert.Contains(t, got.LastError, tc.wantError)
			assert.True(t, got.IsAgentEpic())
			assert.Len(t, runs.all(), tc.wantRuns)
			assert.Equal(t, ticket.StatusDone, h.readTask("epc-c1.md").Status)
		})
	}
}

func TestEpicAgentCrashRecoveryExcludesPassiveEpics(t *testing.T) {
	h := newEpicAgentHarness(t)
	calls := make(chan RunnerParams, 3)
	d := h.newDaemon(h.cfg, WithRunner(epicBlockingRunner(calls)))
	h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", "in_progress"))
	h.writeTicket("epc-passive.md", h.epicMD("epc-passive", "in_progress"))
	foreign := strings.Replace(h.agentEpicMD("epc-foreign", "in_progress"), "epic_execution: agent\n", "epic_execution: agent\nclaimed_by: other-host\n", 1)
	h.writeTicket("epc-foreign.md", foreign)
	stop := startDaemon(t, d)
	defer stop()
	assert.Equal(t, "epc-a", awaitEpicAgent(t, calls).TicketID)
	h.waitForStatus("epc-a.md", ticket.StatusInProgress, 5*time.Second)
	h.waitForStatus("epc-passive.md", ticket.StatusOpen, 5*time.Second)
	assert.Equal(t, ticket.StatusInProgress, h.readTask("epc-foreign.md").Status)
	assert.Equal(t, "other-host", h.readTask("epc-foreign.md").ClaimedBy)
	select {
	case p := <-calls:
		t.Fatalf("unexpected recovered agent for %s", p.TicketID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestEpicAgentRepositoryAliasesReserveSameBranch(t *testing.T) {
	for _, alias := range []string{"symlink", "linked worktree"} {
		t.Run(alias, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			aliasPath := filepath.Join(t.TempDir(), "repo-alias")
			if alias == "symlink" {
				require.NoError(t, os.Symlink(h.repoDir, aliasPath))
			} else {
				mustGit(t, h.repoDir, "worktree", "add", "-b", "alias-checkout", aliasPath, "main")
			}
			calls := make(chan RunnerParams, 3)
			d := h.newDaemon(h.cfg, WithRunner(epicBlockingRunner(calls)))
			md := strings.Replace(h.agentEpicMD("epc-a", "todo"), "epic_execution: agent\n", "epic_execution: agent\nbranch: shared-supervisor\n", 1)
			h.writeTicket("epc-a.md", md)
			stop := startDaemon(t, d)
			defer stop()
			first := awaitEpicAgent(t, calls)
			assert.Equal(t, "epc-a", first.TicketID)
			other := strings.Replace(simpleTaskMD("tst-other", "todo", aliasPath), "status: todo\n", "status: todo\nbranch: shared-supervisor\n", 1)
			d.handleFileChanged(h.writeTicket("tst-other.md", other))
			waitEpicIndexed(t, d, "tst-other")
			require.Eventually(t, func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return d.queued["tst-other"] || d.tickets["tst-other"].ticket.Status == ticket.StatusPaused
			}, 5*time.Second, 20*time.Millisecond)
			select {
			case p := <-calls:
				t.Fatalf("alias bypassed branch reservation: %s", p.TicketID)
			case <-time.After(200 * time.Millisecond):
			}
			assert.DirExists(t, first.Dir)
		})
	}
}

func TestEpicAgentCapacityAndPauseAreSupervisorOnly(t *testing.T) {
	for _, supervisors := range []int{1, 4} {
		t.Run(fmt.Sprint(supervisors), func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.MaxConcurrentAgents = 1
			h.cfg.MaxConcurrentEpicSupervisors = supervisors
			calls := make(chan RunnerParams, supervisors+3)
			d := h.newDaemon(h.cfg, WithRunner(epicBlockingRunner(calls)))
			h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", "todo", "epc-c1"))
			stop := startDaemon(t, d)
			defer stop()
			assert.Equal(t, "epc-a", awaitEpicAgent(t, calls).TicketID)
			for i := 1; i < supervisors; i++ {
				id := fmt.Sprintf("epc-fill%d", i)
				d.handleFileChanged(h.writeTicket(id+".md", h.agentEpicMD(id, "todo")))
				assert.Equal(t, id, awaitEpicAgent(t, calls).TicketID)
			}
			d.handleFileChanged(h.writeTicket("epc-b.md", h.agentEpicMD("epc-b", "todo")))
			d.handleFileChanged(h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "todo", "epc-a", "2026-01-02T00:00:00Z")))
			child := awaitEpicAgent(t, calls)
			assert.Equal(t, "epc-c1", child.TicketID, "a supervisor must not consume worker capacity")
			h.waitForStatus("epc-c1.md", ticket.StatusInProgress, 5*time.Second)
			require.Eventually(t, func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return d.queued["epc-b"]
			}, 5*time.Second, 20*time.Millisecond)
			select {
			case p := <-calls:
				t.Fatalf("supervisor limit was exceeded: %s", p.TicketID)
			case <-time.After(100 * time.Millisecond):
			}
			require.NoError(t, d.PauseTicket("epc-a"))
			assert.Equal(t, "epc-b", awaitEpicAgent(t, calls).TicketID, "pause must release supervisor capacity")
			assert.Equal(t, ticket.StatusPaused, h.readTask("epc-a.md").Status)
			assert.Equal(t, ticket.StatusInProgress, h.readTask("epc-c1.md").Status)
			d.mu.Lock()
			_, childRunning := d.running["epc-c1"]
			d.mu.Unlock()
			assert.True(t, childRunning, "pausing an epic must leave its child agent running")
			assert.DirExists(t, child.Dir)
		})
	}
}

func TestEpicAgentUncertainTerminationRetainsReservation(t *testing.T) {
	h := newEpicAgentHarness(t)
	h.cfg.MaxConcurrentEpicSupervisors = 1
	calls := make(chan RunnerParams, 3)
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls <- p
		now := time.Now()
		return process.Result{StartedAt: now, ExitedAt: now, ExitCode: -1}, process.ErrTerminationUncertain
	}))
	h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", "todo"))
	stop := startDaemon(t, d)
	defer stop()
	first := awaitEpicAgent(t, calls)
	d.handleFileChanged(h.writeTicket("epc-b.md", h.agentEpicMD("epc-b", "todo")))
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, retained := d.running["epc-a"]
		return retained && d.queued["epc-b"] && len(d.runningBranches) == 1 && len(d.liveRuns) == 1
	}, 5*time.Second, 20*time.Millisecond)
	select {
	case p := <-calls:
		t.Fatalf("replacement supervisor started after uncertain termination: %s", p.TicketID)
	case <-time.After(200 * time.Millisecond):
	}
	// A worker has its own free capacity, but still cannot reuse the uncertain
	// supervisor's branch. This distinguishes branch retention from slot retention.
	branch := h.readTask("epc-a.md").Branch
	other := strings.Replace(simpleTaskMD("tst-other", "todo", h.repoDir), "status: todo\n", "status: todo\nbranch: "+branch+"\n", 1)
	d.handleFileChanged(h.writeTicket("tst-other.md", other))
	waitEpicIndexed(t, d, "tst-other")
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.queued["tst-other"] || d.tickets["tst-other"].ticket.Status == ticket.StatusPaused
	}, 5*time.Second, 20*time.Millisecond)
	select {
	case p := <-calls:
		t.Fatalf("worker reused uncertain supervisor branch: %s", p.TicketID)
	case <-time.After(200 * time.Millisecond):
	}
	d.cleanupWorktree(testLogger(t), h.readTask("epc-a.md"))
	assert.DirExists(t, first.Dir)
	assert.Equal(t, ticket.StatusInProgress, h.readTask("epc-a.md").Status)
}
