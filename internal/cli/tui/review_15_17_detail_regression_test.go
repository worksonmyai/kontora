package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestReview15DeploymentDetailUsesCheckout(t *testing.T) {
	for _, value := range []string{"release candidate", "release'$HOME`exit 91`$(exit 92)\\雪\nnext"} {
		t.Run(value, func(t *testing.T) {
			info := tuiGraphTicket()
			w := &ticket.WorkflowWait{ID: "deployment", Kind: "deployment", Phase: "pending",
				Repository: "/repos/project/.git", Worktree: "/checkouts/" + value, Environment: value}
			info.GraphExecution.Wait = w
			content := newDetailModel(info, 160, 70).ticketContent()
			require.Contains(t, content, "--rev <actual-full-deployed-commit> --event-id <unique-id>")
			_, args, ok := strings.Cut(content, "release: kontora release ")
			require.True(t, ok)
			args, _, ok = strings.Cut(args, " --rev ")
			require.True(t, ok)
			require.NotContains(t, args, w.Repository)
			out, err := exec.Command("sh", "-c", "printf '%s\\0' "+args).Output()
			require.NoError(t, err)
			require.Equal(t, []string{"--repo", w.Worktree, "--environment", w.Environment, ""}, strings.Split(string(out), "\x00"))
		})
	}
}

func TestReview16ConnectedTUIKeepsExactPlaintextTranscript(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale-%t", stale), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/tickets/review-log/activity" || r.URL.Query().Get("stage") != "work" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(web.ActivityInfo{
					Source: "log", Stage: "work", Run: 0, Stale: stale,
					Content: "original invocation transcript",
				})
			}))
			defer srv.Close()
			src := &apiSource{client: remote.NewWithClient(srv.URL, "", srv.Client())}
			content, err := src.FetchRunLogs("review-log", "work", 0)
			require.NoError(t, err)
			if stale {
				require.Contains(t, content, "No exact transcript")
				require.NotContains(t, content, "original invocation transcript")
			} else {
				require.Equal(t, "original invocation transcript", content)
			}
		})
	}
}

func TestReview17ReleasedVisitRetryKeepsCompletedTranscript(t *testing.T) {
	cfg := &config.Config{}
	tk := &ticket.Ticket{ID: "released-retry", Stage: "work", Status: ticket.StatusInProgress,
		GraphState: &ticket.GraphState{ExecutionID: "exec", VisitID: "visit", Wake: &ticket.WakeContext{Reason: "released", WaitID: "approval"}},
		History:    []ticket.HistoryEntry{{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "visit", Run: 0, Continuation: true, RunOutcome: ticket.RunOutcomeBlocked}},
	}
	info := web.TicketInfoFromView(app.BuildView(cfg, tk, true))
	m := newDetailModel(info, 160, 60)
	require.Len(t, m.logTargets, 2)
	m.logStage = 1
	m.setLogs("retry transcript")
	require.Equal(t, 2, m.selectedLogTarget().attempt)

	tk.Status = ticket.StatusPaused
	tk.History = append(tk.History, ticket.HistoryEntry{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "visit", Run: 1, RunOutcome: ticket.RunOutcomeBlocked})
	m.setTicket(web.TicketInfoFromView(app.BuildView(cfg, tk, true)))
	require.Equal(t, 1, m.logStage)
	require.Equal(t, 2, m.selectedLogTarget().attempt)
	require.Equal(t, "retry transcript", m.logContent)
}
