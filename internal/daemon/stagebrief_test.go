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
)

func TestStageBrief(t *testing.T) {
	t.Run("built-in names the ticket, the stage and the refused verbs", func(t *testing.T) {
		got := stageBrief("", "kon-q88f", "commit")

		assert.Contains(t, got, "kon-q88f")
		assert.Contains(t, got, `stage "commit"`)
		assert.Contains(t, got, "Exit 0")
		for _, verb := range []string{"done", "cancel", "move", "pause", "retry", "skip", "set-stage", "schedule"} {
			assert.Contains(t, got, verb, "the brief must name every verb rejectSelfMove refuses")
		}
		assert.Contains(t, got, "kontora note kon-q88f")
	})

	t.Run("an override replaces it whole and is not a template", func(t *testing.T) {
		got := stageBrief("mine, with {{ .Ticket }} left alone", "kon-q88f", "commit")

		assert.Equal(t, "mine, with {{ .Ticket }} left alone", got)
	})

	t.Run("a blank override falls back to the built-in", func(t *testing.T) {
		assert.Equal(t, stageBrief("", "kon-1", "step1"), stageBrief("   \n", "kon-1", "step1"))
	})
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
		})
	}
}
