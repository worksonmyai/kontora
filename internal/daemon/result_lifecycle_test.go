package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestSupervisedResultLifecycle(t *testing.T) {
	cases := []struct {
		name, report, pipeline string
		exit                   int
		standalone             bool
		want                   ticket.Status
		outcome                string
	}{
		{name: "invalid", report: `{"outcome":"complete","summary":"done","extra":true}`, pipeline: "retry-stage", want: ticket.StatusPaused, outcome: "protocol_error"},
		{name: "blocked runner failure", report: `{"outcome":"blocked","summary":"Need evidence","next_step":"Provide device"}`, pipeline: "retry-stage", exit: 1, want: ticket.StatusPaused, outcome: "blocked"},
		{name: "complete review stage", report: `{"outcome":"complete","summary":"Review finished"}`, pipeline: "two-stage", want: ticket.StatusTodo, outcome: "complete"},
		{name: "standalone blocked keeps files", report: `{"outcome":"blocked","summary":"Need evidence","next_step":"Provide device"}`, standalone: true, want: ticket.StatusPaused, outcome: "blocked"},
		{name: "standalone missing keeps files", standalone: true, want: ticket.StatusPaused, outcome: "protocol_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.Environment = map[string]string{"KONTORA_RESULT_VERSION": "spoof", "KONTORA_RESULT_FILE": "/tmp/spoof", "KONTORA_INVOCATION_ID": "spoof"}
			var worktree string
			calls := 0
			d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls++
				worktree = p.Dir
				require.True(t, p.RequireTermination)
				inv, err := session.InvocationFromEnv(func(k string) string { return p.Env[k] })
				require.NoError(t, err)
				assert.NotEqual(t, "spoof", inv.ID)
				assert.Equal(t, "tst-child", inv.TicketID)
				if tc.report != "" {
					require.NoError(t, os.MkdirAll(filepath.Dir(inv.ResultPath), 0o700))
					require.NoError(t, os.WriteFile(inv.ResultPath, []byte(tc.report), 0o600))
				}
				if tc.standalone {
					require.NoError(t, os.WriteFile(filepath.Join(p.Dir, "untracked-evidence"), []byte("keep"), 0o600))
				}
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, ExitCode: tc.exit, Termination: process.TerminationProcessGroupGone}, nil
			}), WithFinalSummarySpawner(func(context.Context, FinalSummaryParams) (string, error) { return "Summary", nil }))
			h.writeTicket("epc-parent.md", h.agentEpicMD("epc-parent", "paused"))
			md := h.taskMD("tst-child", "todo", tc.pipeline)
			if tc.standalone {
				md = simpleTaskMD("tst-child", "todo", h.repoDir)
			}
			md = strings.Replace(md, "kontora: true\n", "kontora: true\nparent: epc-parent\n", 1)
			h.writeTicket("tst-child.md", md)
			require.NoError(t, d.initialScan(h.tasksDir))
			d.runTicket(context.Background(), "tst-child")
			d.background.Wait()
			got := h.readTask("tst-child.md")
			assert.Equal(t, tc.want, got.Status)
			require.Len(t, got.History, 1)
			assert.Equal(t, tc.outcome, string(got.History[0].RunOutcome))
			assert.NotEmpty(t, got.History[0].InvocationID)
			assert.Equal(t, tc.exit, got.History[0].ExitCode)
			assert.Equal(t, 0, got.Attempt)
			assert.Equal(t, 1, calls)
			if tc.want == ticket.StatusPaused && !tc.standalone {
				assert.Equal(t, "step1", got.Stage)
			}
			if tc.outcome == "blocked" {
				assert.Contains(t, got.LastError, "Provide device")
			}
			if tc.standalone {
				assert.FileExists(t, filepath.Join(worktree, "untracked-evidence"))
			}
			_, err := os.Stat(executionPath(h.cfg, "tst-child"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestSupervisedQuickBlockedResumeDoesNotFallback(t *testing.T) {
	rd := newResumeDaemon(t, "claude", func(_ context.Context, n int, p RunnerParams) (process.Result, error) {
		assert.Equal(t, 1, n)
		complete := session.Result{Outcome: "blocked", Summary: "Need device", NextStep: "Approve device access"}
		inv, err := session.InvocationFromEnv(func(k string) string { return p.Env[k] })
		require.NoError(t, err)
		_, err = session.PublishResult(inv.ResultPath, complete)
		require.NoError(t, err)
		now := time.Now()
		return process.Result{ExitCode: -1, StartedAt: now, ExitedAt: now}, assert.AnError
	})
	rd.plantRecord(t, agentKindClaude, nil)
	rd.h.writeTicket("epc-parent.md", rd.h.agentEpicMD("epc-parent", "paused"))
	md := strings.Replace(rd.h.taskMD(resumeTicketID, "todo", "one-stage"), "kontora: true\n", "kontora: true\nparent: epc-parent\n", 1)
	rd.runWith(t, md, ticket.StatusPaused)
	require.Len(t, rd.invocations(t), 1)
	got := rd.h.readTask(resumeTicketID + ".md")
	require.Len(t, got.History, 1)
	assert.Equal(t, ticket.RunOutcomeBlocked, got.History[0].RunOutcome)
	assert.Equal(t, 0, got.Attempt)
}

func TestSupervisedStandaloneChildScopeExclusions(t *testing.T) {
	for _, scope := range []string{"unrelated", "passive", "annotation"} {
		t.Run(scope, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			d := h.newDaemon(h.cfg)
			tk, err := ticket.Parse(strings.NewReader(simpleTaskMD("tst-child", "todo", h.repoDir)))
			require.NoError(t, err)
			if scope != "unrelated" {
				require.NoError(t, tk.SetField("parent", "epc-parent"))
				h.writeTicket("epc-parent.md", h.epicMD("epc-parent", "open"))
			}
			if scope == "annotation" {
				h.writeTicket("epc-parent.md", h.agentEpicMD("epc-parent", "paused"))
			}
			id, err := d.supervision(tk, scope == "annotation")
			require.NoError(t, err)
			assert.Empty(t, id)
		})
	}
}
