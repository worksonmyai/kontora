package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// ReadConditionLog reads the latest evaluation in the ticket's log directory.
// For a command condition with no log, it returns condition=true and an error matching os.ErrNotExist.
func ReadConditionLog(logDir string, graph *config.WorkflowGraph, stage string) (data []byte, condition bool, err error) {
	if graph == nil {
		return nil, false, nil
	}
	node, ok := graph.Node(stage)
	if !ok || !node.CommandCondition() {
		return nil, false, nil
	}
	data, err = readLatestConditionLog(logDir, node.ID)
	return data, true, err
}

func readLatestConditionLog(logDir, node string) ([]byte, error) {
	if !ticket.IsSafeID(node) {
		return nil, fmt.Errorf("invalid condition node %q", node)
	}
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	conditions, err := root.OpenRoot("conditions")
	if err != nil {
		return nil, err
	}
	defer conditions.Close()
	dir, err := conditions.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var latest string
	var latestTime uint64
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() {
			continue
		}
		id, match := strings.CutPrefix(name, node+".")
		id, log := strings.CutSuffix(id, ".log")
		stamp, suffix, found := strings.Cut(id, "-")
		if !match || !log || !found || suffix == "" || !ticket.IsSafeID(id) {
			continue
		}
		// Use the evaluation ID, because a copied or edited log can have a newer modification time.
		nanos, err := strconv.ParseUint(stamp, 10, 64)
		if err == nil && (latest == "" || nanos > latestTime || nanos == latestTime && name > latest) {
			latest, latestTime = name, nanos
		}
	}
	if latest == "" {
		return nil, fmt.Errorf("no evaluation log for %q: %w", node, os.ErrNotExist)
	}
	return conditions.ReadFile(latest)
}

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
