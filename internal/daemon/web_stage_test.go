package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestStageActionsHTTPPauseFirst(t *testing.T) {
	for _, operation := range []string{"skip", "set-stage"} {
		for _, tc := range []struct {
			name, status          string
			running, live, review bool
		}{
			{name: "running status", status: "in_progress"},
			{name: "starting", status: "todo", running: true},
			{name: "stopping", status: "paused", running: true, live: true},
			{name: "uncertain termination", status: "human_review", running: true, live: true},
			{name: "live invocation", status: "paused", live: true},
			{name: "plannotator", status: "human_review", review: true},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t)
				d := h.newDaemon(h.cfg)
				path := h.writeTicket("tst-stage.md", strings.Replace(h.taskMD("tst-stage", tc.status, "two-stage"), "pipeline:", "stage: step1\npipeline:", 1))
				cached := h.readTask("tst-stage.md")
				require.NoError(t, cached.SetField("status", "paused"))
				d.tickets["tst-stage"] = newTicketState(cached, path)
				cancelled := false
				if tc.running {
					d.running["tst-stage"] = func() { cancelled = true }
				}
				if tc.live {
					d.liveRuns["tst-stage"] = liveRun{stage: "step1"}
				}
				if tc.review {
					d.plannotator["tst-stage"] = func() { cancelled = true }
				}
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				srv, err := web.New(d, d.broker, "127.0.0.1", 0, "", nil, d.tmuxSession, testLogger(t))
				require.NoError(t, err)
				require.NoError(t, srv.Start())
				defer func() { require.NoError(t, srv.Shutdown(context.Background())) }()

				resp, err := http.Post("http://"+srv.Addr()+"/api/tickets/tst-stage/"+operation, "application/json", strings.NewReader(`{"stage":"step2"}`))
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, resp.Body.Close())
				require.NoError(t, err)
				assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
				assert.Contains(t, string(body), "pause first")
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				assert.Equal(t, "step1", d.tickets["tst-stage"].ticket.Stage)
				assert.Equal(t, ticket.StatusPaused, d.tickets["tst-stage"].ticket.Status)
				assert.Empty(t, d.queued)
				assert.False(t, cancelled)
			})
		}
	}
}

func TestImportedDraftStageSelectionHTTP(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	imported, err := d.UploadTicket([]byte("---\nid: imported\nstatus: open\n---\n# Imported draft\n"))
	require.NoError(t, err)
	pipeline := "two-stage"
	require.NoError(t, d.UpdateTicket(imported.ID, web.UpdateTicketRequest{Pipeline: &pipeline}))
	before, err := d.GetTicket(imported.ID)
	require.NoError(t, err)
	require.False(t, before.Kontora)
	require.Empty(t, before.Stage)
	require.Equal(t, []string{"step1", "step2"}, before.Stages)
	srv, err := web.New(d, d.broker, "127.0.0.1", 0, "", nil, d.tmuxSession, testLogger(t))
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	defer func() { require.NoError(t, srv.Shutdown(context.Background())) }()

	resp, err := http.Post("http://"+srv.Addr()+"/api/tickets/"+imported.ID+"/set-stage", "application/json", strings.NewReader(`{"stage":"step2"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var result web.TicketInfo
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "step2", result.Stage)
	assert.Equal(t, "open", result.Status)
	assert.False(t, result.Kontora)
	stored := h.readTask(imported.ID + ".md")
	assert.Equal(t, "step2", stored.Stage)
	assert.Equal(t, ticket.StatusOpen, stored.Status)
	assert.False(t, stored.Kontora)
	assert.Empty(t, d.queued)
	assert.Empty(t, d.running)
}

func TestStoppedStageActionsHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, operation, stage, wantStage, wantStatus string
		queued                                        bool
	}{
		{"intermediate skip queues", "skip", "step1", "step2", "todo", true},
		{"final skip completes without a run", "skip", "step2", "step2", "done", false},
		{"set-stage does not retry", "set-stage", "step1", "step2", "paused", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("true", "true")
			cfg.AutoPickUp = new(false)
			d := h.newDaemon(cfg)
			path := h.writeTicket("tst-stage.md", fmt.Sprintf("---\nid: tst-stage\nkontora: true\nstatus: paused\npipeline: two-stage\nstage: %s\nattempt: 3\nlast_error: old error\npath: %s\n---\n# Stage test\n", tc.stage, h.repoDir))
			d.tickets["tst-stage"] = newTicketState(h.readTask("tst-stage.md"), path)
			srv, err := web.New(d, d.broker, "127.0.0.1", 0, "", nil, d.tmuxSession, testLogger(t))
			require.NoError(t, err)
			require.NoError(t, srv.Start())
			defer func() { require.NoError(t, srv.Shutdown(context.Background())) }()
			resp, err := http.Post("http://"+srv.Addr()+"/api/tickets/tst-stage/"+tc.operation, "application/json", strings.NewReader(`{"stage":"step2"}`))
			require.NoError(t, err)
			defer resp.Body.Close()
			var result web.TicketInfo
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tc.wantStage, result.Stage)
			assert.Equal(t, tc.wantStatus, result.Status)
			stored := h.readTask("tst-stage.md")
			assert.Equal(t, tc.wantStage, stored.Stage)
			assert.Equal(t, ticket.Status(tc.wantStatus), stored.Status)
			assert.Empty(t, stored.History)
			assert.Equal(t, tc.queued, d.queued["tst-stage"])
			assert.Empty(t, d.running)
			if tc.operation == "set-stage" {
				assert.Equal(t, 3, stored.Attempt)
				assert.Equal(t, "old error", stored.LastError)
			} else {
				assert.Empty(t, stored.LastError)
			}
			if tc.wantStatus == "done" {
				assert.NotNil(t, stored.CompletedAt)
			}
		})
	}
}
