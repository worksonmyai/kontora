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
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func TestCleanupWorktreePreservesReservedBranch(t *testing.T) {
	for _, role := range []string{"worker", "supervisor"} {
		for _, alias := range []string{"same repository", "symlink", "linked worktree"} {
			for _, termination := range []string{"running", "uncertain"} {
				t.Run(role+"/"+alias+"/"+termination, func(t *testing.T) {
					h := newEpicAgentHarness(t)
					cleanupRepo := h.repoDir
					if alias != "same repository" {
						cleanupRepo = filepath.Join(t.TempDir(), "repo-alias")
						if alias == "symlink" {
							require.NoError(t, os.Symlink(h.repoDir, cleanupRepo))
						} else {
							mustGit(t, h.repoDir, "worktree", "add", "-b", "alias-checkout", cleanupRepo, "main")
						}
					}
					calls := make(chan RunnerParams, 1)
					d := h.newDaemon(h.cfg, WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
						calls <- p
						if termination == "uncertain" {
							return process.Result{ExitCode: -1}, process.ErrTerminationUncertain
						}
						<-ctx.Done()
						return process.Result{Termination: process.TerminationProcessGroupGone}, ctx.Err()
					}))
					md := h.taskMD("tst-active", "todo", "one-stage")
					if role == "supervisor" {
						md = h.agentEpicMD("tst-active", "todo")
					}
					md = strings.Replace(md, "status: todo\n", "status: todo\nbranch: shared-cleanup\n", 1)
					h.writeTicket("tst-active.md", md)
					stop := sync.OnceFunc(startDaemon(t, d))
					t.Cleanup(stop)
					active := awaitEpicAgent(t, calls)
					h.waitForStatus("tst-active.md", ticket.StatusInProgress, 5*time.Second)

					// A different ticket ID bypasses the running-ticket guard, so only
					// the branch reservation can protect the active checkout.
					closed, err := ticket.Parse(strings.NewReader(strings.Replace(
						simpleTaskMD("tst-closed", "done", cleanupRepo),
						"status: done\n", "status: done\nbranch: shared-cleanup\n", 1)))
					require.NoError(t, err)
					d.cleanupWorktree(testLogger(t), closed)
					require.DirExists(t, active.Dir)
					wt, err := worktree.FindWorktreeForBranch(cleanupRepo, closed.Branch)
					require.NoError(t, err)
					assert.Equal(t, active.Dir, wt)

					stop()
					d.cleanupWorktree(testLogger(t), closed)
					if termination == "uncertain" {
						assert.DirExists(t, active.Dir, "uncertain termination must retain branch ownership")
					} else {
						assert.NoDirExists(t, active.Dir, "confirmed shutdown permits cleanup")
					}
				})
			}
		}
	}
}
