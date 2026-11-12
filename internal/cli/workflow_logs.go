package cli

import (
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func GraphRunLog(t *ticket.Ticket, logsDir, stage string, run int) (path string, stale, graph bool) {
	var visit string
	count, selected := 0, -1
	for i, h := range t.History {
		if h.Stage != stage {
			continue
		}
		if run < 0 || count == run {
			visit, selected = h.NodeVisitID, i
		}
		count++
	}
	if t.GraphState != nil && t.Stage == stage && (run < 0 || run == count) {
		visit = t.GraphState.VisitID
		selected = -1
	}
	if visit == "" {
		return "", false, false
	}
	if !ticket.IsSafeID(t.ID) || !ticket.IsSafeID(stage) || !ticket.IsSafeID(visit) {
		return "", false, true
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
