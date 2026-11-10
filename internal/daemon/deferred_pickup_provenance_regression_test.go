package daemon

import (
	"container/heap"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestDeferredPickupAnnotationReloadProvenance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit bool
		disable  bool
	}{
		{name: "automatic disabled", disable: true},
		{name: "automatic enabled"},
		{name: "explicit disabled", explicit: true, disable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = "deferred-annotation"
			t.Setenv(config.TicketsDirEnvVar, "")
			t.Setenv(config.LegacyTicketsDirEnvVar, "")
			h := newPlannotatorHarness(t)
			rh := &reloadHarness{testHarness: h.testHarness, configPath: filepath.Join(t.TempDir(), "config.yaml")}
			content := rh.yaml(configOpts{plannotator: "plannotator:\n  reviews_dir: " + h.reviewsDir + "\n"})
			rh.writeConfig(t, content)
			var err error
			h.cfg, err = config.Load(rh.configPath)
			require.NoError(t, err)
			started := make(chan string, 4)
			d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
				started <- p.TicketID
				return process.Result{StartedAt: time.Now(), ExitedAt: time.Now(), Termination: process.TerminationProcessGroupGone}, nil
			}, WithConfigPath(rh.configPath))
			_, stop := startAnnotationDaemon(t, h, d, id, h.annotationTicketMD(id, "open", ""))
			defer stop()
			require.NoError(t, d.StartPlannotatorAnnotate(id))
			require.Eventually(t, func() bool { return h.callCount.Load() == 1 }, 3*time.Second, 10*time.Millisecond)
			d.handleFileChanged(h.writeTicket(id+".md", h.annotationTicketMD(id, "todo", "")))
			waitDeferred := func() {
				t.Helper()
				require.Eventually(t, func() bool {
					d.mu.Lock()
					defer d.mu.Unlock()
					_, deferred := d.plannotatorDeferred[id]
					return deferred && !d.queued[id]
				}, 3*time.Second, 10*time.Millisecond)
			}
			waitDeferred()
			if tc.explicit {
				require.NoError(t, d.RunTicket(id))
				waitDeferred()
				d.mu.Lock()
				d.enqueueAutomatic(d.tickets[id].ticket)
				d.mu.Unlock()
				waitDeferred()
			}
			if tc.disable {
				rh.writeConfigAtomic(t, content+"auto_pick_up: false\n")
				require.NoError(t, d.reloadConfig())
				require.False(t, *d.config().AutoPickUp)
			}
			h.stdoutCh <- annotateJSON(annotateDismissed, "")
			waitForPlannotatorDone(t, d, id)
			if tc.disable && !tc.explicit {
				select {
				case got := <-started:
					t.Fatalf("automatic pickup started after annotation/reload/release: %s", got)
				case <-time.After(200 * time.Millisecond):
				}
				require.Equal(t, ticket.StatusTodo, h.readTask(id+".md").Status)
				require.NoError(t, d.RunTicket(id))
			}
			select {
			case got := <-started:
				require.Equal(t, id, got)
			case <-time.After(5 * time.Second):
				t.Fatal("eligible pickup did not start after annotation release")
			}
			h.waitForStatus(id+".md", ticket.StatusDone, 5*time.Second)
		})
	}
}

func TestDeferredPickupPoolChangeProvenance(t *testing.T) {
	for _, epic := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			name := "worker to epic"
			if epic {
				name = "epic to worker"
			}
			if explicit {
				name += " explicit"
			} else {
				name += " automatic"
			}
			t.Run(name, func(t *testing.T) {
				const id = "deferred-pool"
				h := newReloadHarness(t)
				content := h.yaml(configOpts{})
				d := h.newDaemonWithConfig(t, content)
				workerMD := h.taskMD(id, "todo", "one-stage")
				epicMD := strings.Replace(workerMD, "kontora: true\n", "kontora: true\nkind: epic\nepic_execution: agent\n", 1)
				before, after := workerMD, epicMD
				if epic {
					before, after = after, before
				}
				d.handleFileChanged(h.writeTicket(id+".md", before))
				d.mu.Lock()
				if explicit {
					d.enqueue(d.tickets[id].ticket)
				}
				require.GreaterOrEqual(t, d.readyOrdinaryJobLocked(), 0)
				item := heap.Pop(&d.queue).(*queueItem)
				delete(d.queued, id)
				d.syncQueueDepthLocked()
				d.mu.Unlock()
				h.writeTicket(id+".md", after)
				ctx := context.WithValue(context.Background(), dispatchPoolKey{}, epic)
				ctx = context.WithValue(ctx, dispatchExplicitKey{}, item.explicit)
				_, _, _, _, _, err := d.claimExecution(ctx, d.config(), id, d.ticketLog(id))
				require.ErrorIs(t, err, errExecutionChanged)
				h.writeConfig(t, content+"auto_pick_up: false\n")
				require.NoError(t, d.reloadConfig())
				d.mu.Lock()
				defer d.mu.Unlock()
				require.Equal(t, !epic, d.tickets[id].ticket.IsAgentEpic())
				index := d.readyOrdinaryJobLocked()
				if explicit {
					require.GreaterOrEqual(t, index, 0, "explicit pickup survives the pool change and reload")
				} else {
					require.Equal(t, -1, index, "automatic pickup must not become explicit on a pool change")
					require.Empty(t, d.queue)
					d.enqueue(d.tickets[id].ticket)
					require.GreaterOrEqual(t, d.readyOrdinaryJobLocked(), 0, "explicit run remains eligible")
				}
				require.Equal(t, ticket.StatusTodo, h.readTask(id+".md").Status)
			})
		}
	}
}

type deferredPickupClaimBarrier struct {
	context.Context
	once    sync.Once
	reached chan struct{}
	resume  chan struct{}
}

func (c *deferredPickupClaimBarrier) Done() <-chan struct{} {
	c.once.Do(func() {
		close(c.reached)
		<-c.resume
	})
	return c.Context.Done()
}

func TestDeferredPickupClaimAndDelayProvenance(t *testing.T) {
	for _, path := range []string{"initial claim", "transaction claim", "delayed requeue"} {
		for _, origin := range []string{"automatic", "explicit", "direct"} {
			t.Run(path+"/"+origin, func(t *testing.T) {
				const id = "deferred-claim"
				h := newReloadHarness(t)
				content := h.yaml(configOpts{})
				d := h.newDaemonWithConfig(t, content)
				filePath := h.writeTicket(id+".md", h.taskMD(id, "todo", "one-stage"))
				d.setTicketState(id, h.readTask(id+".md"), filePath)
				ctx := context.Background()
				if origin != "direct" {
					ctx = context.WithValue(ctx, dispatchExplicitKey{}, origin == "explicit")
				}
				switch path {
				case "initial claim":
					d.plannotator[id] = func() {}
					_, _, _, _, _, err := d.claimExecution(ctx, d.config(), id, d.ticketLog(id))
					require.ErrorIs(t, err, errExecutionChanged)
				case "transaction claim":
					barrier := &deferredPickupClaimBarrier{Context: ctx, reached: make(chan struct{}), resume: make(chan struct{})}
					t.Cleanup(func() {
						select {
						case <-barrier.resume:
						default:
							close(barrier.resume)
						}
					})
					result := make(chan error, 1)
					go func() {
						_, _, _, _, _, err := d.claimExecution(barrier, d.config(), id, d.ticketLog(id))
						result <- err
					}()
					select {
					case <-barrier.reached:
					case <-time.After(3 * time.Second):
						t.Fatal("claim did not reach context creation before the mutation")
					}
					d.mu.Lock()
					d.plannotator[id] = func() {}
					d.mu.Unlock()
					close(barrier.resume)
					select {
					case err := <-result:
						require.ErrorIs(t, err, errExecutionChanged)
					case <-time.After(3 * time.Second):
						t.Fatal("claim did not finish")
					}
				case "delayed requeue":
					d.requeueAfter(ctx, id, 0)
				}
				h.writeConfig(t, content+"auto_pick_up: false\n")
				require.NoError(t, d.reloadConfig())
				d.releasePlannotator(id)
				d.mu.Lock()
				defer d.mu.Unlock()
				index := d.readyOrdinaryJobLocked()
				if origin == "automatic" {
					require.Equal(t, -1, index)
					require.Empty(t, d.queue)
				} else {
					require.GreaterOrEqual(t, index, 0)
				}
				require.Empty(t, d.plannotatorDeferred)
				require.Empty(t, d.running)
				require.Equal(t, ticket.StatusTodo, h.readTask(id+".md").Status)
			})
		}
	}
}
