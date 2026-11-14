package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestGraphRunLogPrefersInvocationAndFallsBackToLegacy(t *testing.T) {
	dir := t.TempDir()
	tk := &ticket.Ticket{ID: "ticket", Stage: "work", Status: ticket.StatusPaused,
		GraphState: &ticket.GraphState{VisitID: "visit"},
		History: []ticket.HistoryEntry{
			{Stage: "work", NodeVisitID: "visit", InvocationID: "first", RunOutcome: ticket.RunOutcomeWaiting},
			{Stage: "work", NodeVisitID: "visit", InvocationID: "continuation", Continuation: true},
		},
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, tk.ID), 0700))
	legacy := session.GraphLogPath(dir, tk.ID, "work", "visit")
	require.NoError(t, os.WriteFile(legacy, []byte("latest"), 0600))
	path, stale, graph := GraphRunLog(tk, dir, "work", 0)
	require.True(t, graph)
	require.True(t, stale)
	require.Equal(t, legacy, path)
	exact := session.GraphLogPath(dir, tk.ID, "work", "visit", "first")
	require.NoError(t, os.WriteFile(exact, []byte("suspension"), 0600))
	path, stale, graph = GraphRunLog(tk, dir, "work", 0)
	require.Equal(t, exact, path)
	require.False(t, stale)
	require.True(t, graph)
	latest := session.GraphLogPath(dir, tk.ID, "work", "visit", "continuation")
	require.NoError(t, os.WriteFile(latest, []byte("continued"), 0600))
	path, stale, graph = GraphRunLog(tk, dir, "work", -1)
	require.Equal(t, latest, path)
	require.False(t, stale)
	require.True(t, graph)
	tk.Status = ticket.StatusInProgress
	tk.InvocationID = "active"
	active := session.GraphLogPath(dir, tk.ID, "work", "visit", "active")
	require.NoError(t, os.WriteFile(active, []byte("active"), 0600))
	path, stale, graph = GraphRunLog(tk, dir, "work", -1)
	require.Equal(t, active, path)
	require.False(t, stale)
	require.True(t, graph)
	tk.History[0].InvocationID = "../unsafe"
	path, _, graph = GraphRunLog(tk, dir, "work", 0)
	require.Empty(t, path)
	require.True(t, graph)
}
