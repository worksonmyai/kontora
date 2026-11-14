package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestReview16ActivityKeepsInvocationPlaintextAfterSameVisitContinuation(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	path := h.writeTicket("review-log.md", `---
id: review-log
status: paused
stage: step1
graph_state:
  version: 1
  execution_id: exec
  revision: 3
  definition_fingerprint: fingerprint
  visit_id: visit
  visit: 1
history:
  - stage: step1
    workflow_execution_id: exec
    node_visit_id: visit
    invocation_id: first
    run: 0
  - stage: step1
    workflow_execution_id: exec
    node_visit_id: visit
    invocation_id: second
    continuation: true
    run: 1
---
# Plaintext activity
`)
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets[tk.ID] = newTicketState(tk, path)
	require.NoError(t, os.MkdirAll(session.TicketDir(h.logsDir, tk.ID), 0700))
	exact := session.GraphLogPath(h.logsDir, tk.ID, "step1", "visit", "first")
	require.NoError(t, os.WriteFile(exact, []byte("original invocation transcript"), 0600))
	_, err = os.Stat(session.EventsPath(h.logsDir, tk.ID, "step1", 0))
	require.True(t, os.IsNotExist(err), "exercise the supported plaintext-only fallback")

	selected, stale, graph := cli.GraphRunLog(tk, h.logsDir, "step1", 0)
	require.Equal(t, exact, selected)
	require.True(t, graph)
	require.False(t, stale)
	info, err := d.GetActivity(web.ActivityQuery{ID: tk.ID, Stage: "step1", Run: 0})
	require.NoError(t, err)
	require.Equal(t, "log", info.Source)
	require.Nil(t, info.Tape)
	require.Equal(t, "original invocation transcript", info.Content)
	require.False(t, info.Live)
	require.False(t, info.Stale, "the configured logs directory contains the exact invocation log")
}
