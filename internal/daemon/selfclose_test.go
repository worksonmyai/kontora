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

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// A terminal status written while a stage runs is a human override: the daemon
// kills the run and discards its exit, so the step's on_success never applies
// and the run leaves no history entry behind.
//
// This is why the CLI refuses a lifecycle verb aimed at the ticket the calling
// process is a stage of (rejectSelfMove). Four tickets reached done instead of
// human_review this way before that guard existed, each because the stage agent
// ran `kontora done` on itself after committing.
//
// The daemon behaviour is deliberately unchanged: an override from a person is
// meant to win. The test pins the cost of one, so a change that starts recording
// the run anyway is a decision rather than an accident.
func TestSelfCloseDuringStageDiscardsTheRun(t *testing.T) {
	cases := []struct {
		name       string
		writeDone  bool
		wantStatus ticket.Status
		wantRuns   int
	}{
		{"the stage leaves the status alone", false, ticket.StatusHumanReview, 1},
		{"the status is closed mid-run", true, ticket.StatusDone, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("true", "true")
			cfg.Pipelines["park-stage"] = config.Pipeline{
				{Stage: "step1", Agent: "agent1", OnSuccess: "human_review", OnFailure: "pause"},
			}
			path := filepath.Join(h.tasksDir, "tst-self.md")

			runner := func(ctx context.Context, p RunnerParams) (process.Result, error) {
				if tc.writeDone {
					// What `kontora done` writes: SetStatus goes straight to the
					// file, so the watcher is what notices and cancels the run.
					b, err := os.ReadFile(path)
					require.NoError(t, err)
					out := strings.Replace(string(b), "status: in_progress", "status: done", 1)
					require.NoError(t, os.WriteFile(path, []byte(out), 0o644))
					time.Sleep(200 * time.Millisecond)
				}
				return DirectRunner(ctx, p)
			}

			d := h.newDaemon(cfg, WithRunner(runner))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()

			time.Sleep(200 * time.Millisecond)
			h.writeTicket("tst-self.md", h.taskMD("tst-self", "todo", "park-stage"))
			got := h.waitForStatus("tst-self.md", tc.wantStatus, 10*time.Second)

			cancel()
			require.NoError(t, <-errCh)

			assert.Len(t, got.History, tc.wantRuns)
		})
	}
}
