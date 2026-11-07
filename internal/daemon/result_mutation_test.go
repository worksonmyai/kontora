package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestResultBindingWaitsForOfflineMutation(t *testing.T) {
	for _, change := range []string{"cancel", "summary"} {
		t.Run(change, func(t *testing.T) {
			if runtime.GOOS != "linux" {
				if _, err := exec.LookPath("lsof"); err != nil {
					t.Skip("lock descriptor observation requires /proc or lsof")
				}
			}
			f := newResultFixture(t)
			snapshot := f.tkt.Clone()
			params := RunnerParams{Env: map[string]string{}}
			p := spawnAgentParams{cfg: f.h.cfg, ticketID: snapshot.ID, filePath: f.path,
				stageName: snapshot.Stage, wtPath: f.h.wtDir}
			errs := make(chan error, 1)
			require.NoError(t, ticket.WithBoardMutation(f.h.tasksDir, func() error {
				go func() {
					_, err := f.d.bindExecution(snapshot, p, &params, f.rec.EpicID)
					errs <- err
				}()
				lockPath, err := filepath.EvalSymlinks(filepath.Join(f.h.tasksDir, ".kontora", "mutation.lock"))
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					opened, err := mutationLockDescriptors(lockPath)
					if err != nil {
						return err
					}
					if opened == 2 {
						break
					}
					select {
					case err := <-errs:
						return fmt.Errorf("invocation binding returned before acquiring the board lock: %v", err)
					case <-ctx.Done():
						return ctx.Err()
					case <-ticker.C:
					}
				}
				fresh := f.h.readTask(resultTicketID + ".md")
				if change == "cancel" {
					err = fresh.SetField("status", "cancelled")
				} else {
					err = fresh.SetField("summary", "operator summary")
				}
				if err != nil {
					return err
				}
				data, err := fresh.Marshal()
				if err != nil {
					return err
				}
				return os.WriteFile(f.path, data, 0o644)
			}))
			err := <-errs
			got := f.h.readTask(resultTicketID + ".md")
			if change == "cancel" {
				require.Error(t, err)
				assert.Equal(t, ticket.StatusCancelled, got.Status)
				assert.Equal(t, f.rec.Invocation.ID, got.InvocationID)
				assert.Empty(t, params.Env["KONTORA_INVOCATION_ID"])
				assert.False(t, params.RequireTermination)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "operator summary", got.Summary)
				assert.Equal(t, params.Env["KONTORA_INVOCATION_ID"], got.InvocationID)
				assert.True(t, params.RequireTermination)
			}
		})
	}
}
