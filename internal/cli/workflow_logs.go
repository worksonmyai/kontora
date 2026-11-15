package cli

import (
	"os"

	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func GraphRunLog(t *ticket.Ticket, logsDir, stage string, run int) (path string, stale, graph bool) {
	var visit, invocation string
	count, selected := 0, -1
	for i, h := range t.History {
		if h.Stage != stage {
			continue
		}
		if run < 0 || count == run {
			visit, invocation, selected = h.NodeVisitID, h.InvocationID, i
		}
		count++
	}
	if t.GraphState != nil && t.Stage == stage &&
		(run == count || run < 0 && (t.IsRunning() || selected < 0 || visit != t.GraphState.VisitID)) {
		visit, invocation = t.GraphState.VisitID, t.InvocationID
		selected = -1
	}
	if visit == "" {
		return "", false, false
	}
	if !ticket.IsSafeID(t.ID) || !ticket.IsSafeID(stage) || !ticket.IsSafeID(visit) {
		return "", false, true
	}
	if invocation != "" {
		if !ticket.IsSafeID(invocation) {
			return "", false, true
		}
		exact := session.GraphLogPath(logsDir, t.ID, stage, visit, invocation)
		if _, err := os.Stat(exact); err == nil || !os.IsNotExist(err) {
			return exact, false, true
		}
	}
	if selected >= 0 {
		for _, h := range t.History[selected+1:] {
			if h.Stage == stage && h.NodeVisitID == visit {
				stale = true
			}
		}
		if t.IsRunning() && t.GraphState != nil && t.Stage == stage && t.GraphState.VisitID == visit {
			stale = true
		}
	}
	return session.GraphLogPath(logsDir, t.ID, stage, visit), stale, true
}
