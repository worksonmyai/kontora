package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/web"
)

const evidenceTicketID = "tst-evidence"

type evidenceService struct {
	web.TicketService
	activity func(web.ActivityQuery) (web.ActivityInfo, error)
	changes  func(string) (web.ChangesInfo, error)
}

func (evidenceService) ListTickets(web.ListTicketsOptions) []web.TicketInfo {
	return []web.TicketInfo{{ID: evidenceTicketID}}
}

func (evidenceService) RunningAgents() int { return 0 }

func (evidenceService) GetTicket(string) (web.TicketInfo, error) {
	return web.TicketInfo{}, web.ErrTicketNotFound
}

func (s evidenceService) GetActivity(q web.ActivityQuery) (web.ActivityInfo, error) {
	return s.activity(q)
}

func (s evidenceService) GetChanges(id string) (web.ChangesInfo, error) { return s.changes(id) }

func startEvidenceServer(t *testing.T, svc evidenceService) []string {
	t.Helper()
	const token = "evidence-token"
	srv, err := web.New(svc, nil, "127.0.0.1", 0, token, nil, "", slog.Default())
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})
	return []string{"KONTORA_URL=http://" + srv.Addr(), "KONTORA_TOKEN=" + token, "KONTORA_PAGER=", "CLICOLOR_FORCE=0", "NO_COLOR=1"}
}

func TestCLIActivity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		stage string
		run   int
		info  web.ActivityInfo
		err   error
		want  string
	}{
		{
			name: "selected run with structured events", flags: []string{"--stage", "review", "--run", "2"}, stage: "review", run: 2,
			info: web.ActivityInfo{Source: "events", Stage: "review", Run: 2, Live: true, Tape: &logfmt.Tape{Events: []logfmt.Event{
				{Kind: "text", Text: "Review started."},
				{Kind: "tool", Tool: "Read", Arg: "notes.md", Summary: "Loaded notes"},
			}}},
			want: "review run 2 (live)\ntext Review started.\ntool Read notes.md Loaded notes\n",
		},
		{
			name: "latest run falls back to a stale log",
			info: web.ActivityInfo{Source: "log", Stage: "review", Run: 3, Stale: true, Content: "first line\n  indented line\n"},
			want: "review run 3 (may include another run of this stage)\nfirst line\n  indented line\n",
		},
		{
			name: "no activity",
			want: "No activity recorded for this ticket yet.\n",
		},
		{
			name: "selected run unavailable", flags: []string{"--stage", "review", "--run", "9"}, stage: "review", run: 9,
			err: web.ErrLogNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startEvidenceServer(t, evidenceService{activity: func(q web.ActivityQuery) (web.ActivityInfo, error) {
				assert.Equal(t, web.ActivityQuery{ID: evidenceTicketID, Stage: tc.stage, Run: tc.run}, q)
				return tc.info, tc.err
			}})
			out, stderr, err := runCLISeparated(t, env, nil, append([]string{"activity", "tst-evid"}, tc.flags...)...)
			if tc.err != nil {
				require.Error(t, err)
				assert.Contains(t, stderr, tc.err.Error())
				assert.Empty(t, out)
				return
			}
			require.NoError(t, err, stderr)
			assert.Equal(t, tc.want, out)
		})
	}
}

func TestCLIChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		info web.ChangesInfo
		err  error
		want []string
	}{
		{
			name: "commits and changed files",
			info: web.ChangesInfo{
				Branch: "work/evidence", Base: "main",
				Commits: []web.CommitInfo{{SHA: "445566778899", Subject: "Record review"}},
				Files: []web.FileChangeInfo{
					{Path: "notes.md", Added: 12, Deleted: 2},
					{Path: "archive.md", Added: 3, Deleted: 5},
				},
			},
			want: []string{"Branch work/evidence → main", "Commits", "44556677 Record review", "Files", "notes.md +12 -2", "archive.md +3 -5", "2 file(s), +15 -7"},
		},
		{
			name: "branch without changes",
			info: web.ChangesInfo{Branch: "work/evidence", Base: "main"},
			want: []string{"Branch work/evidence → main", "No changes."},
		},
		{
			name: "no branch",
			want: []string{"No branch for this ticket yet."},
		},
		{
			name: "ticket removed after lookup",
			err:  fmt.Errorf("%w: %s was removed", web.ErrTicketNotFound, evidenceTicketID),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startEvidenceServer(t, evidenceService{changes: func(id string) (web.ChangesInfo, error) {
				assert.Equal(t, evidenceTicketID, id)
				return tc.info, tc.err
			}})
			out, stderr, err := runCLISeparated(t, env, nil, "changes", "tst-evid")
			if tc.err != nil {
				require.Error(t, err)
				assert.Contains(t, stderr, tc.err.Error())
				assert.Empty(t, out)
				return
			}
			require.NoError(t, err, stderr)
			var lines []string
			for line := range strings.SplitSeq(out, "\n") {
				if fields := strings.Fields(line); len(fields) > 0 {
					lines = append(lines, strings.Join(fields, " "))
				}
			}
			assert.Equal(t, tc.want, lines)
		})
	}
}
