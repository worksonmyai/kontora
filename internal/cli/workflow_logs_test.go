package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

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
