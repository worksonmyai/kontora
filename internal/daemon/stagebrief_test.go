package daemon

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestStageBrief(t *testing.T) {
	t.Run("built-in names the ticket, the stage and the refused verbs", func(t *testing.T) {
		got := stageBrief("", "kon-q88f", "commit")

		assert.Contains(t, got, "kon-q88f")
		assert.Contains(t, got, `stage "commit"`)
		assert.Contains(t, got, "Submit a terminal complete or blocked result")
		assert.Contains(t, got, "A clean exit alone does not complete the stage")
		assert.Contains(t, got, "Notes and summaries do not replace the required terminal result")
		assert.NotContains(t, got, "Exit 0")
		for _, verb := range []string{"done", "cancel", "move", "pause", "retry", "skip", "set-stage", "schedule"} {
			assert.Contains(t, got, verb, "the brief must name every verb rejectSelfMove refuses")
		}
		assert.Contains(t, got, "shared handoff instructions in the run prompt")
		assert.NotContains(t, got, "kontora note")
		assert.NotContains(t, got, "kontora summary")
	})

	t.Run("an override replaces it whole and is not a template", func(t *testing.T) {
		got := stageBrief("mine, with {{ .Ticket }} left alone", "kon-q88f", "commit")

		assert.Equal(t, "mine, with {{ .Ticket }} left alone", got)
	})

	t.Run("a blank override falls back to the built-in", func(t *testing.T) {
		assert.Equal(t, stageBrief("", "kon-1", "step1"), stageBrief("   \n", "kon-1", "step1"))
	})
}

func TestStageResultInstructions(t *testing.T) {
	got := stageResultInstructions()
	for _, clause := range []string{
		"## Mandatory stage result",
		"kontora_result({outcome, summary, next_step?})",
		"kontora result --input-file /absolute/path/result.json TICKET_ID",
		"Use the actual current ticket ID", "Do not supply or override invocation identity",
		"unique scratch file outside the repository", "stdin",
		`outcome "complete" or "blocked"`, "summary is required", "blocked also requires next_step",
		"current stage's assigned work, not the whole ticket",
		"review stage can be complete with findings",
		"assigned implementation or required acceptance evidence remains unmet",
		"Fixture work alone is not complete", "production foundations",
		"Report blocked when requirements or authority prevent further work",
		"Missing or invalid results pause the same stage", "clean runner exit alone cannot advance it",
		"Notes, summaries, final prose, questions, and checkpoints are not terminal results",
		"Check the acknowledgement", "Identical resubmission is idempotent",
		"conflicting result cannot replace", "Do not use notes as a fallback",
		"does not override execution failures, timeouts, provider errors, hooks, child checks, or human acceptance",
		"Do not change your own lifecycle status",
		"## Prerequisites and escalation", "Read saved decisions", "investigate the missing prerequisite",
		"Continue independent authorized work", "concrete question", "options, their costs",
		"recommended default", "Save the question and answer together immediately",
		"Do not weaken acceptance requirements", "Do not substitute emulation",
		"Retry requires explicit operator action", "Saving an answer", "dismissing attention",
		"children, branches, worktrees, dirty files, and interrupted Git operations",
	} {
		assert.Contains(t, got, clause)
	}
	for _, authority := range []string{"## Supervised", "no per-commit approval", "may merge", "may rebase", "may install"} {
		assert.NotContains(t, got, authority, "reporting must not grant supervision authority")
	}
}

// The flag exists on claude and pi only. A third-kind agent is given no brief,
// because passing it one it does not know fails the run outright.
func TestStageBriefReachesTheAgent(t *testing.T) {
	cases := []struct {
		name   string
		agent  config.Agent
		brief  string
		wantIn bool
	}{
		{"claude", config.Agent{Binary: "claude"}, "be careful", true},
		{"pi", config.Agent{Binary: "pi"}, "be careful", true},
		{"another agent takes no flag", config.Agent{Binary: "programmator"}, "be careful", false},
		{"an empty brief adds nothing", config.Agent{Binary: "claude"}, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, settingsFile, _, err := buildAgentArgs(tc.agent, "the prompt", tc.brief, "chan", "", "", "", "", nil, false)
			require.NoError(t, err)
			if settingsFile != "" {
				t.Cleanup(func() { os.Remove(settingsFile) })
			}

			idx := slices.Index(args, "--append-system-prompt")
			if !tc.wantIn {
				assert.Equal(t, -1, idx)
				return
			}
			require.NotEqual(t, -1, idx)
			assert.Equal(t, tc.brief, args[idx+1])
			// The prompt is positional and appended last, so the flag and its
			// value must land ahead of it or the prompt is read as the value.
			assert.Less(t, idx+1, slices.Index(args, "the prompt"))
		})
	}
}

// End to end: what the daemon hands a real stage run.
func TestStageBriefOnASpawnedRun(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		want       func(t *testing.T, brief string)
	}{
		{
			name: "built-in",
			want: func(t *testing.T, brief string) {
				assert.Contains(t, brief, "tst-brief")
				assert.Contains(t, brief, `stage "step1"`)
			},
		},
		{
			name:       "override",
			configured: "the only thing you are told",
			want: func(t *testing.T, brief string) {
				assert.Equal(t, "the only thing you are told", brief)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("claude", "claude")
			cfg.SystemPrompt = tc.configured

			var captured RunnerParams
			runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
				captured = p
				completeEpicInvocation(t, p)
				return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
			}

			d := h.newDaemon(cfg, WithRunner(runner))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()

			time.Sleep(200 * time.Millisecond)
			h.writeTicket("tst-brief.md", h.taskMD("tst-brief", "todo", "one-stage"))
			h.waitForStatus("tst-brief.md", "done", 10*time.Second)

			cancel()
			require.NoError(t, <-errCh)

			idx := slices.Index(captured.Args, "--append-system-prompt")
			require.NotEqual(t, -1, idx, "args: %s", strings.Join(captured.Args, " "))
			tc.want(t, captured.Args[idx+1])
			got := renderedPrompt(captured)
			assert.Contains(t, got, stageResultInstructions())
			assert.NotContains(t, got, "## Supervised local delivery")
			assert.NotContains(t, got, "## Epic supervisor")
		})
	}
}

func TestStageResultInstructionsSurviveCustomPrompts(t *testing.T) {
	for _, scope := range []string{"ordinary", "passive epic child", "activated epic child"} {
		for _, resume := range []bool{false, true} {
			name := scope + "/fresh"
			if resume {
				name = scope + "/resume"
			}
			t.Run(name, func(t *testing.T) {
				rd := newResumeDaemon(t, "claude", func(_ context.Context, _ int, p RunnerParams) (process.Result, error) {
					completeEpicInvocation(t, p)
					now := time.Now()
					return process.Result{StartedAt: now, ExitedAt: now}, nil
				})
				rd.cfg.SystemPrompt = "Custom brief: do not commit."
				rd.cfg.ResumePrompt = "Custom continuation for {{ .Ticket.ID }}"
				stage := rd.cfg.Stages[resumeStage]
				stage.Prompt = ""
				rd.cfg.Stages[resumeStage] = stage
				md := rd.h.taskMD(resumeTicketID, "todo", "one-stage")
				if scope != "ordinary" {
					parent := rd.h.epicMD("epc-parent", "open")
					if scope == "activated epic child" {
						parent = rd.h.agentEpicMD("epc-parent", "paused")
					}
					rd.h.writeTicket("epc-parent.md", parent)
					md = strings.Replace(md, "kontora: true\n", "kontora: true\nparent: epc-parent\n", 1)
				}
				if resume {
					rd.plantRecord(t, agentKindClaude, nil)
				}
				rd.runWith(t, md, ticket.StatusDone)
				spawns := rd.invocations(t)
				require.Len(t, spawns, 1)
				got := renderedPrompt(spawns[0])
				assert.Contains(t, got, stageResultInstructions())
				assert.Equal(t, 1, strings.Count(got, "## Mandatory stage result"))
				assert.Contains(t, spawns[0].Args, "Custom brief: do not commit.")
				assert.NotContains(t, got, "## Epic supervisor")
				if scope == "activated epic child" {
					assert.Contains(t, got, supervisedInstructions("epc-parent"))
				} else {
					assert.NotContains(t, got, "## Supervised local delivery")
					assert.NotContains(t, got, "no per-commit approval")
				}
				if resume {
					assert.Contains(t, got, "Custom continuation for "+resumeTicketID)
					assert.Contains(t, spawns[0].Args, "--resume")
				}
			})
		}
	}
}

func TestReworkStageResultInstructionsSurviveCustomPrompts(t *testing.T) {
	for _, scope := range []string{"ordinary", "passive epic child", "activated epic child"} {
		t.Run(scope, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.ReworkIsBuiltin = true
			h.cfg.SystemPrompt = "Custom brief: do not commit."
			h.cfg.Stages[config.ReworkStageName] = config.Stage{Prompt: ""}
			h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			var captured RunnerParams
			d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				captured = p
				completeEpicInvocation(t, p)
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now}, nil
			}), WithFinalSummarySpawner(func(context.Context, FinalSummaryParams) (string, error) { return "Summary", nil }))
			fields := "kontora: true\nstage: rework\n"
			if scope != "ordinary" {
				parent := h.epicMD("epc-parent", "open")
				if scope == "activated epic child" {
					parent = h.agentEpicMD("epc-parent", "paused")
				}
				h.writeTicket("epc-parent.md", parent)
				fields += "parent: epc-parent\n"
			}
			md := strings.Replace(h.taskMD("tst-rework", "todo", "one-stage"), "kontora: true\n", fields, 1)
			h.writeTicket("tst-rework.md", md)
			require.NoError(t, d.initialScan(h.tasksDir))
			d.runTicket(context.Background(), "tst-rework")
			assert.Equal(t, ticket.StatusHumanReview, h.readTask("tst-rework.md").Status)
			got := renderedPrompt(captured)
			assert.Contains(t, got, stageResultInstructions())
			assert.Equal(t, 1, strings.Count(got, "## Mandatory stage result"))
			assert.Contains(t, captured.Args, "Custom brief: do not commit.")
			assert.NotContains(t, got, "## Epic supervisor")
			if scope == "activated epic child" {
				assert.Contains(t, got, supervisedInstructions("epc-parent"))
			} else {
				assert.NotContains(t, got, "## Supervised local delivery")
				assert.NotContains(t, got, "no per-commit approval")
			}
		})
	}
}
