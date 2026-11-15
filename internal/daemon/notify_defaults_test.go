package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestDaemonExecutionNotifyDefaults(t *testing.T) {
	for _, boundary := range []struct {
		name, status, stage string
		apply               bool
		run                 func(*Daemon, string) error
	}{
		{"run open", "open", "step1", true, (*Daemon).RunTicket},
		{"run already todo", "todo", "step1", true, (*Daemon).RunTicket},
		{"retry", "paused", "step1", true, (*Daemon).RetryTicket},
		{"move todo", "open", "step1", true, func(d *Daemon, id string) error { return d.MoveTicket(id, "todo") }},
		{"init", "open", "step1", true, func(d *Daemon, id string) error { return d.InitTicket(id, web.InitTicketRequest{}) }},
		{"advancing skip", "paused", "step1", true, (*Daemon).SkipStage},
		{"final skip", "paused", "step2", false, (*Daemon).SkipStage},
		{"rework", "human_review", "step2", true, (*Daemon).transitionToRework},
		{"annotation park", "open", "step1", true, (*Daemon).parkForAnnotation},
	} {
		for _, choice := range []struct {
			name, field string
			want        []string
		}{
			{"unset", "", []string{"paused", "human_review", "waiting"}},
			{"anchored null", "notify: &choice null\ncustom: *choice\n", []string{"paused", "human_review", "waiting"}},
			{"Off", "notify: []\n", []string{}},
			{"saved", "notify: [done]\n", []string{"done"}},
		} {
			t.Run(boundary.name+"/"+choice.name, func(t *testing.T) {
				h := newHarness(t)
				h.cfg.Notifications.DefaultStatuses = []string{"paused", "human_review", "waiting"}
				d := h.newDaemon(h.cfg)
				id := "tst-default"
				path := filepath.Join(h.tasksDir, id+".md")
				h.writeTicket(id+".md", fmt.Sprintf("---\nid: %s\nkontora: true\npath: %q\nstatus: %s\npipeline: two-stage\nstage: %s\nlast_error: retained\n%s---\n# Test\n", id, h.repoDir, boundary.status, boundary.stage, choice.field))
				d.tickets[id] = newTicketState(h.readTask(id+".md"), path)
				require.NoError(t, boundary.run(d, id))
				want := choice.want
				if !boundary.apply && (choice.name == "unset" || choice.name == "anchored null") {
					want = nil
				}
				got := h.readTask(id + ".md")
				assert.Equal(t, want, got.Notify.Statuses)
				assert.Equal(t, want, d.tickets[id].ticket.Notify.Statuses)
				if boundary.name == "run already todo" {
					assert.Equal(t, "retained", got.LastError)
				}
			})
		}
	}
}

func TestWorkflowRestartNotifyDefaults(t *testing.T) {
	for _, tc := range []struct {
		name        string
		saved, want []string
		interrupted bool
	}{
		{"unset", nil, []string{"paused"}, false},
		{"Off", []string{}, []string{}, false},
		{"saved", []string{"done"}, []string{"done"}, false},
		{"refused", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := workflowActionFixture(t)
			f.h.cfg.Notifications.DefaultStatuses = []string{"paused"}
			if tc.saved != nil {
				require.NoError(t, f.tkt.SetField("notify", tc.saved))
			}
			if !tc.interrupted {
				require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
				require.NoError(t, f.tkt.SetField("invocation_id", ""))
			}
			f.write(t)
			err := f.d.WorkflowRestart(f.tkt.ID)
			if tc.interrupted {
				require.ErrorIs(t, err, web.ErrInvalidState)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, f.h.readTask(f.tkt.ID+".md").Notify.Statuses)
		})
	}
}

func TestDaemonAutomaticExecutionDoesNotApplyNotifyDefaults(t *testing.T) {
	for _, mode := range []string{"startup pickup and stage advance", "startup recovery", "import pickup", "annotation no feedback", "annotation completion"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Notifications.DefaultStatuses = []string{"done", "paused"}
			h.cfg.Plannotator.ReviewsDir = t.TempDir()
			d := h.newDaemon(h.cfg)
			id := "tst-auto"
			md := h.taskMD(id, "todo", "two-stage")
			if mode == "startup recovery" {
				md = h.taskMD(id, "in_progress", "two-stage")
			}
			if mode == "annotation completion" || mode == "annotation no feedback" {
				tk, err := ticket.ParseBytes([]byte(md))
				require.NoError(t, err)
				require.NoError(t, tk.SetField("annotation_return_status", "open"))
				data, err := tk.Marshal()
				require.NoError(t, err)
				md = string(data)
				if mode == "annotation completion" {
					require.NoError(t, os.WriteFile(annotationsPath(h.cfg.Plannotator.ReviewsDir, id), []byte("Clarify the acceptance criteria."), 0o644))
				}
			}
			if mode != "import pickup" {
				h.writeTicket(id+".md", md)
			} else {
				h.writeTicket("tst-ready.md", h.taskMD("tst-ready", "open", "one-stage"))
			}
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
			if mode == "import pickup" {
				require.Eventually(t, func() bool { _, err := d.GetTicket("tst-ready"); return err == nil }, 5*time.Second, 10*time.Millisecond)
				h.writeTicket(id+".md", md)
			}
			status := ticket.StatusDone
			if mode == "annotation completion" || mode == "annotation no feedback" {
				status = ticket.StatusOpen
			}
			got := h.waitForStatus(id+".md", status, 10*time.Second)
			assert.Nil(t, got.Notify.Statuses)
			if mode == "annotation completion" {
				require.Len(t, got.History, 1)
				assert.Equal(t, ticket.KindAnnotation, got.History[0].Kind)
			}
		})
	}
}

func TestReloadDoesNotApplyNotifyDefaults(t *testing.T) {
	h := newReloadHarness(t)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))
	id := "tst-reload"
	h.writeTicket(id+".md", h.taskMD(id, "open", "one-stage"))
	d.tickets[id] = newTicketState(h.readTask(id+".md"), filepath.Join(h.tasksDir, id+".md"))
	before, err := os.ReadFile(filepath.Join(h.tasksDir, id+".md"))
	require.NoError(t, err)
	h.writeConfig(t, h.yaml(configOpts{extra: "notifications:\n  default_statuses: [paused, waiting]\n"}))
	require.NoError(t, d.reloadConfig())
	assert.Equal(t, []string{"paused", "waiting"}, d.config().NotifyDefaultFor(h.repoDir))
	after, err := os.ReadFile(filepath.Join(h.tasksDir, id+".md"))
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Nil(t, d.tickets[id].ticket.Notify.Statuses)
}

func TestExecutionNotifyProjectOff(t *testing.T) {
	h := newHarness(t)
	h.cfg.Notifications.DefaultStatuses = []string{"done"}
	h.cfg.Projects = map[string]config.Project{"repo": {Path: h.repoDir, Notify: []string{}}}
	d := h.newDaemon(h.cfg)
	h.writeTicket("tst-off.md", h.taskMD("tst-off", "todo", "one-stage"))
	d.tickets["tst-off"] = newTicketState(h.readTask("tst-off.md"), filepath.Join(h.tasksDir, "tst-off.md"))
	require.NoError(t, d.RunTicket("tst-off"))
	assert.Equal(t, []string{}, h.readTask("tst-off.md").Notify.Statuses)
}

func TestWorkflowWaitRetryNotifyDefaults(t *testing.T) {
	for _, choice := range []struct {
		name        string
		saved, want []string
	}{
		{"unset", nil, []string{"paused"}},
		{"Off", []string{}, []string{}},
		{"saved", []string{"done"}, []string{"done"}},
	} {
		t.Run(choice.name, func(t *testing.T) {
			f := workflowWaitActionFixture(t)
			f.h.cfg.Notifications.DefaultStatuses = []string{"paused"}
			if choice.saved != nil {
				require.NoError(t, f.tkt.SetField("notify", choice.saved))
				f.write(t)
			}
			require.NoError(t, f.d.PauseTicket(f.tkt.ID))
			assert.Equal(t, choice.saved, f.h.readTask(f.tkt.ID+".md").Notify.Statuses)
			require.NoError(t, f.d.RetryTicket(f.tkt.ID))
			got := f.h.readTask(f.tkt.ID + ".md")
			assert.Equal(t, ticket.StatusPaused, got.Status)
			assert.Equal(t, choice.want, got.Notify.Statuses)
			assert.False(t, f.d.queued[f.tkt.ID])
		})
	}
}
