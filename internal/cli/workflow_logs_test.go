package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestLogPathIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, stage             string
		graph, sidecar, allowed bool
	}{
		{name: "legacy traversal", stage: "../outside"},
		{name: "legacy external symlink", stage: "review"},
		{name: "sidecar external symlink", stage: "review", sidecar: true},
		{name: "graph external symlink", stage: "review", graph: true},
		{name: "legacy internal symlink", stage: "review", allowed: true},
		{name: "sidecar internal symlink", stage: "review", sidecar: true, allowed: true},
		{name: "graph internal symlink", stage: "review", graph: true, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticketsDir, logsDir := t.TempDir(), t.TempDir()
			tkt, err := ticket.ParseBytes([]byte("---\nid: tst-logs\nstatus: done\n---\n# Logs\n"))
			require.NoError(t, err)
			if tc.graph {
				require.NoError(t, tkt.SetField("history", []ticket.HistoryEntry{{Stage: "review", NodeVisitID: "execution.1"}}))
			}
			data, err := tkt.Marshal()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(ticketsDir, tkt.ID+".md"), data, 0o600))
			logDir := session.TicketDir(logsDir, tkt.ID)
			require.NoError(t, os.MkdirAll(logDir, 0o755))
			path := session.LogPath(logsDir, tkt.ID, "review")
			target := "../outside.log"
			if tc.allowed {
				target = "output.log"
			}
			content := []byte("log contents")
			if tc.sidecar {
				path = session.EventsPath(logsDir, tkt.ID, "review", 0)
				content = []byte(`{"version":1,"events":[{"kind":"text","text":"log contents"}]}`)
			}
			if tc.graph {
				path = session.GraphLogPath(logsDir, tkt.ID, "review", "execution.1")
			}
			require.NoError(t, os.WriteFile(filepath.Join(logDir, target), content, 0o600))
			if tc.stage == "review" {
				require.NoError(t, os.Symlink(target, path))
			}

			tape, text, err := StageActivity(ticketsDir, logsDir, tkt.ID, tc.stage, 0)
			if tc.allowed {
				require.NoError(t, err)
				if tc.sidecar {
					require.NotNil(t, tape)
					assert.Equal(t, "log contents", tape.Events[0].Text)
				} else {
					assert.Equal(t, "log contents", text)
				}
			} else {
				require.Error(t, err)
				assert.Nil(t, tape)
				assert.Empty(t, text)
			}
			if !tc.sidecar {
				var out bytes.Buffer
				err := Logs(ticketsDir, logsDir, tkt.ID, tc.stage, &out)
				if tc.allowed {
					require.NoError(t, err)
					assert.Equal(t, "log contents", out.String())
				} else {
					require.Error(t, err)
					assert.Empty(t, out.String())
				}
			}
		})
	}
}

func TestGraphVisitLogs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		run   int
		visit string
		stale bool
	}{
		{name: "earlier visit remains readable", run: 0, visit: "execution.1"},
		{name: "retried visit has shared output", run: 1, visit: "execution.2", stale: true},
		{name: "latest retry", run: 2, visit: "execution.2"},
		{name: "workflow restart retains old logs", run: 3, visit: "restart.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticketsDir, logsDir := t.TempDir(), t.TempDir()
			tkt, err := ticket.ParseBytes([]byte("---\nid: tst-logs\nstatus: human_review\n---\n# Logs\n"))
			require.NoError(t, err)
			history := []ticket.HistoryEntry{
				{Stage: "check", WorkflowExecutionID: "execution", NodeVisitID: "execution.1"},
				{Stage: "check", Run: 1, WorkflowExecutionID: "execution", NodeVisitID: "execution.2"},
				{Stage: "check", Run: 2, WorkflowExecutionID: "execution", NodeVisitID: "execution.2"},
				{Stage: "check", Run: 3, WorkflowExecutionID: "restart", NodeVisitID: "restart.1"},
			}
			require.NoError(t, tkt.SetField("history", history))
			data, err := tkt.Marshal()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(ticketsDir, tkt.ID+".md"), data, 0o600))
			path := session.GraphLogPath(logsDir, tkt.ID, "check", tc.visit)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(tc.visit), 0o600))
			gotPath, stale, graph := GraphRunLog(tkt, logsDir, "check", tc.run)
			assert.Equal(t, path, gotPath)
			assert.Equal(t, tc.stale, stale)
			assert.True(t, graph)
			tape, content, err := StageActivity(ticketsDir, logsDir, tkt.ID, "check", tc.run)
			require.NoError(t, err)
			assert.Nil(t, tape)
			assert.Equal(t, tc.visit, content)
		})
	}
}
