package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func TestResultRecoveryOverlappingBranchReservations(t *testing.T) {
	for _, first := range []string{"ordinary", "recovered"} {
		t.Run(first+" stops first", func(t *testing.T) {
			f := newResultFixture(t)
			common, err := worktree.CommonDir(f.h.repoDir)
			require.NoError(t, err)
			key := common + "\x00" + f.rec.Branch
			ctx, cancel := context.WithCancel(context.Background())
			f.d.running["tst-live"] = cancel
			f.d.runningBranches[key] = "tst-live"
			f.d.windows.list = func(string) ([]string, error) { return nil, errors.New("inspection unavailable") }
			f.reconcile(t)
			stopOrdinary := func() {
				f.d.finishOrdinaryRun(ctx, cancel, "tst-live", key, f.path, f.d.ticketLog("tst-live"))
			}
			stopRecovered := func() {
				f.d.mu.Lock()
				f.d.releaseRecoveredExecutionLocked(f.tkt.ID)
				f.d.mu.Unlock()
			}
			if first == "ordinary" {
				stopOrdinary()
				holder, exists := f.d.branchHolderLocked(key)
				assert.True(t, exists)
				assert.Equal(t, f.tkt.ID, holder)
				md := f.h.taskMD("tst-next", "todo", "one-stage")
				path := f.h.writeTicket("tst-next.md", md)
				next := f.h.readTask("tst-next.md")
				require.NoError(t, next.SetField("branch", f.rec.Branch))
				data, err := next.Marshal()
				require.NoError(t, err)
				f.h.writeTicket("tst-next.md", string(data))
				f.d.tickets[next.ID] = newTicketState(next, path)
				_, cancelNext, _, _, _, err := f.d.claimExecution(context.Background(), f.h.cfg, next.ID, f.d.ticketLog(next.ID))
				if cancelNext != nil {
					cancelNext()
				}
				require.ErrorContains(t, err, "in use by ticket "+f.tkt.ID)
				assert.Equal(t, "paused", string(f.h.readTask("tst-next.md").Status))
				stopRecovered()
			} else {
				stopRecovered()
				holder, exists := f.d.branchHolderLocked(key)
				assert.True(t, exists)
				assert.Equal(t, "tst-live", holder)
				stopOrdinary()
			}
			_, exists := f.d.branchHolderLocked(key)
			assert.False(t, exists)
			assert.Empty(t, f.d.running)
			assert.Empty(t, f.d.sem)
		})
	}
}
