package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestReworkFinalizationReplay(t *testing.T) {
	cases := []struct {
		name, report, outcome string
		supervised, failWrite bool
		exit                  int
		want                  ticket.Status
	}{
		{name: "supervised complete", supervised: true, report: "complete", outcome: "success", want: ticket.StatusHumanReview},
		{name: "supervised complete replay", supervised: true, report: "complete", failWrite: true, outcome: "success", want: ticket.StatusHumanReview},
		{name: "supervised blocked", supervised: true, report: "blocked", outcome: "blocked", want: ticket.StatusPaused},
		{name: "supervised blocked replay", supervised: true, report: "blocked", failWrite: true, outcome: "blocked", want: ticket.StatusPaused},
		{name: "unsupervised complete", outcome: "success", want: ticket.StatusHumanReview},
		{name: "unsupervised failure", exit: 1, outcome: "failure", want: ticket.StatusPaused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.ReworkIsBuiltin = true
			h.cfg.Stages[config.ReworkStageName] = config.Stage{Prompt: "Address feedback for {{ .Ticket.ID }}"}
			h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			if tc.failWrite {
				h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "fail-ticket-write", Run: `chmod 444 "$KONTORA_TICKET_FILE"`}}}
			}
			md := strings.Replace(h.taskMD("tst-rework-replay", "todo", "one-stage"), "kontora: true\n", "kontora: true\nstage: rework\n", 1)
			if tc.supervised {
				h.writeTicket("epc-parent.md", h.agentEpicMD("epc-parent", "paused"))
				md = strings.Replace(md, "kontora: true\n", "kontora: true\nparent: epc-parent\n", 1)
			}
			path := h.writeTicket("tst-rework-replay.md", md)
			tkt := h.readTask("tst-rework-replay.md")
			require.NoError(t, tkt.SetField("history", []ticket.HistoryEntry{{Stage: "step1", Agent: "agent1", Summary: "Initial implementation"}}))
			data, err := tkt.Marshal()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o644))
			summaryStarted, release := make(chan struct{}, 2), make(chan struct{})
			releaseSummary := sync.OnceFunc(func() { close(release) })
			calls := 0
			d, collect := h.newMetricsDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls++
				assert.Equal(t, tc.supervised, p.RequireTermination)
				if tc.supervised {
					_, err := session.PublishResult(p.Env["KONTORA_RESULT_FILE"], session.Result{Outcome: tc.report, Summary: "Feedback inspected", NextStep: "Provide evidence"})
					require.NoError(t, err)
				} else {
					assert.Empty(t, p.Env["KONTORA_RESULT_FILE"])
				}
				fresh := h.readTask("tst-rework-replay.md")
				require.NoError(t, fresh.SetField("summary", "Feedback inspected"))
				data, err := fresh.Marshal()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o644))
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, ExitCode: tc.exit, Termination: process.TerminationWindowGone}, nil
			}), WithFinalSummarySpawner(func(_ context.Context, p FinalSummaryParams) (string, error) {
				assert.Contains(t, strings.Join(p.Args, " "), "Initial implementation")
				assert.Contains(t, strings.Join(p.Args, " "), "Feedback inspected")
				summaryStarted <- struct{}{}
				<-release
				return "Implementation and feedback addressed", nil
			}))
			t.Cleanup(releaseSummary)
			d.windows.list = func(string) ([]string, error) { return nil, nil }
			require.NoError(t, d.initialScan(h.tasksDir))
			runDone := make(chan struct{})
			go func() {
				defer close(runDone)
				d.runTicket(context.Background(), tkt.ID)
			}()
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
				t.Fatal("rework retained its worker while the final summary was blocked")
			}
			if tc.failWrite {
				require.Equal(t, ticket.StatusInProgress, h.readTask("tst-rework-replay.md").Status)
				rec, err := loadExecution(h.cfg, tkt.ID)
				require.NoError(t, err)
				require.Equal(t, "transition", rec.Phase)
				assert.Contains(t, d.pendingFinalizations, rec.Invocation.ID)
				assert.Contains(t, d.running, tkt.ID, "failed final write retains the reservation")
				assert.Empty(t, summaryStarted, "a failed final write must not start the summary")
				d.handleFileChanged(path)
				require.Equal(t, ticket.StatusInProgress, h.readTask("tst-rework-replay.md").Status)
				require.Contains(t, d.running, tkt.ID, "failed replay retains the reservation")
				require.NoError(t, os.Chmod(path, 0o644))
				d.handleFileChanged(path)
			}
			if tc.want == ticket.StatusHumanReview {
				assert.Eventually(t, func() bool { return len(summaryStarted) == 1 }, 5*time.Second, 10*time.Millisecond,
					"final summary must start after the transition is written, including replay")
			}
			require.Eventually(t, func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return len(d.running) == 0 && len(d.sem) == 0 && len(d.runningBranches) == 0 && len(d.recoveredReservations) == 0 && len(d.pendingFinalizations) == 0
			}, 5*time.Second, 10*time.Millisecond, "a blocked summary must not retain execution resources")
			releaseSummary()
			d.background.Wait()
			for range 2 {
				d.handleFileChanged(path)
				d.background.Wait()
			}
			got := h.readTask("tst-rework-replay.md")
			assert.Equal(t, tc.want, got.Status)
			assert.Equal(t, config.ReworkStageName, got.Stage)
			require.Len(t, got.History, 2)
			assert.Equal(t, tc.exit, got.History[1].ExitCode)
			if tc.supervised {
				assert.Equal(t, tc.report, string(got.History[1].RunOutcome))
				assert.NotEmpty(t, got.History[1].InvocationID)
			} else {
				assert.Empty(t, got.History[1].RunOutcome)
				assert.Empty(t, got.History[1].InvocationID)
			}
			assert.Empty(t, got.InvocationID)
			assert.NoFileExists(t, executionPath(h.cfg, got.ID))
			if tc.want == ticket.StatusHumanReview {
				assert.Equal(t, "Implementation and feedback addressed", got.FinalSummary)
				assert.Len(t, summaryStarted, 1)
			} else {
				assert.Empty(t, got.FinalSummary)
				assert.Empty(t, summaryStarted)
			}
			assert.Equal(t, 1, calls)
			metric, ok := collect()["kontora.stage.runs"]
			require.True(t, ok, "replayed rework must record its validated run")
			assert.Equal(t, map[string]int64{tc.outcome: 1}, sumByAttr(t, metric, "outcome"))
			assert.Equal(t, map[string]int64{config.ReworkStageName: 1}, sumByAttr(t, metric, "stage"))
		})
	}
}

func TestSupervisedReworkOutcomes(t *testing.T) {
	cases := []struct {
		name, report, provider, hook, outcome string
		exit                                  int
		want                                  ticket.Status
	}{
		{name: "missing", outcome: "protocol_error", want: ticket.StatusPaused},
		{name: "blocked", report: "blocked", outcome: "blocked", want: ticket.StatusPaused},
		{name: "complete", report: "complete", outcome: "complete", want: ticket.StatusHumanReview},
		{name: "complete provider failure", report: "complete", provider: "PROVIDER FAILED", outcome: "failed", want: ticket.StatusPaused},
		{name: "blocked provider failure", report: "blocked", provider: "PROVIDER FAILED", outcome: "failed", want: ticket.StatusPaused},
		{name: "complete hook failure", report: "complete", hook: "exit 1", outcome: "failed", want: ticket.StatusPaused},
		{name: "complete runner failure", report: "complete", exit: 1, outcome: "failed", want: ticket.StatusPaused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.ReworkIsBuiltin = true
			h.cfg.Stages[config.ReworkStageName] = config.Stage{Prompt: "Address feedback for {{ .Ticket.ID }}"}
			if tc.hook != "" {
				h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "check", Run: tc.hook, OnFailure: "pause"}}}
			}
			if tc.provider != "" {
				h.cfg.Agents["agent1"] = config.Agent{Binary: "true", FailurePatterns: []string{tc.provider}}
			}
			calls := 0
			d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls++
				assert.True(t, p.RequireTermination)
				assert.Contains(t, renderedPrompt(p), "## Mandatory supervised result")
				inv, err := session.InvocationFromEnv(func(k string) string { return p.Env[k] })
				require.NoError(t, err)
				assert.Equal(t, config.ReworkStageName, inv.Stage)
				if tc.report != "" {
					_, err = session.PublishResult(inv.ResultPath, session.Result{Outcome: tc.report, Summary: "Feedback inspected", NextStep: "Provide evidence"})
					require.NoError(t, err)
				}
				if tc.provider != "" {
					require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
					require.NoError(t, os.WriteFile(p.LogFile, []byte(tc.provider), 0o600))
				}
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, ExitCode: tc.exit}, nil
			}), WithFinalSummarySpawner(func(context.Context, FinalSummaryParams) (string, error) { return "Summary", nil }))
			h.writeTicket("epc-parent.md", h.agentEpicMD("epc-parent", "paused"))
			md := strings.Replace(h.taskMD("tst-child", "todo", "one-stage"), "kontora: true\n", "kontora: true\nparent: epc-parent\nstage: rework\n", 1)
			h.writeTicket("tst-child.md", md)
			require.NoError(t, d.initialScan(h.tasksDir))
			d.runTicket(context.Background(), "tst-child")
			d.background.Wait()
			got := h.readTask("tst-child.md")
			assert.Equal(t, tc.want, got.Status)
			assert.Equal(t, config.ReworkStageName, got.Stage)
			require.Len(t, got.History, 1)
			assert.Equal(t, tc.outcome, string(got.History[0].RunOutcome))
			assert.NotEmpty(t, got.History[0].InvocationID)
			if tc.report == "blocked" && tc.provider != "" {
				assert.Contains(t, got.LastError, "PROVIDER FAILED")
				assert.Contains(t, got.LastError, "Next step: Provide evidence")
				assert.Contains(t, got.History[0].OutcomeReason, "Next step: Provide evidence")
			}
			assert.Equal(t, tc.exit, got.History[0].ExitCode)
			assert.Equal(t, 1, calls)
		})
	}
}
