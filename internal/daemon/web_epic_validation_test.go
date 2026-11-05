package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestDaemon_UpdateEpicExecutionSettings(t *testing.T) {
	for _, tc := range []struct {
		name, id, branch    string
		wantErr, pathUpdate bool
	}{
		{name: "child follows fresh parent repository", id: "epc-child", branch: "child-work", pathUpdate: true},
		{name: "child takes integration branch", id: "epc-child", branch: "integration", wantErr: true},
		{name: "epic takes child branch", id: "epc-a", branch: "child-work", wantErr: true},
		{name: "child keeps independent branch", id: "epc-child", branch: "other-child"},
		{name: "epic keeps independent branch", id: "epc-a", branch: "other-integration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.AutoPickUp = new(false)
			d := h.newDaemon(h.cfg)
			for id, md := range map[string]string{
				"epc-a":     h.agentEpicMD("epc-a", "paused"),
				"epc-child": h.epicChildMD("epc-child", "open", "epc-a", "2026-01-02T00:00:00Z"),
			} {
				path := h.writeTicket(id+".md", md)
				tkt, err := ticket.ParseFile(path)
				require.NoError(t, err)
				branch := "integration"
				if id == "epc-child" {
					branch = "child-work"
				}
				require.NoError(t, tkt.SetField("branch", branch))
				data, err := tkt.Marshal()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o644))
				d.tickets[id] = newTicketState(tkt, path)
			}
			file := filepath.Join(h.tasksDir, tc.id+".md")
			before, err := os.ReadFile(file)
			require.NoError(t, err)
			req := web.UpdateTicketRequest{Branch: &tc.branch, BaseBranch: new("integration")}
			if tc.pathUpdate {
				other := initRepo(t)
				parentPath := filepath.Join(h.tasksDir, "epc-a.md")
				parent, err := ticket.ParseFile(parentPath)
				require.NoError(t, err)
				require.NoError(t, parent.SetField("path", other))
				data, err := parent.Marshal()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(parentPath, data, 0o644))
				req.Path = &other
			}
			err = d.UpdateTicket(tc.id, req)
			if tc.wantErr {
				require.ErrorIs(t, err, web.ErrInvalidState)
				after, err := os.ReadFile(file)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.branch, h.readTask(tc.id+".md").Branch)
		})
	}
}

func TestDaemon_EpicActivationReadsCurrentFiles(t *testing.T) {
	for _, entry := range []string{"run", "init"} {
		for _, tc := range []struct {
			name, childEdit, unrelatedMarkdown string
			wantErr                            bool
		}{
			{name: "child repository changed", childEdit: "repository", wantErr: true},
			{name: "new child", childEdit: "new", wantErr: true},
			{name: "malformed known child", childEdit: "malformed", wantErr: true},
			{name: "clean store"},
			{name: "unrelated notes", unrelatedMarkdown: "# Unrelated notes\n"},
			{name: "unrelated invalid YAML", unrelatedMarkdown: "---\nid: unrelated\nstatus: [\n---\n# Invalid\n"},
		} {
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				h := newEpicAgentHarness(t)
				h.cfg.AutoPickUp = new(false)
				d := h.newDaemon(h.cfg)
				for id, md := range map[string]string{
					"epc-a":     h.epicMD("epc-a", "open"),
					"epc-child": h.epicChildMD("epc-child", "open", "epc-a", "2026-01-02T00:00:00Z"),
				} {
					path := h.writeTicket(id+".md", md)
					tkt, err := ticket.ParseFile(path)
					require.NoError(t, err)
					d.tickets[id] = newTicketState(tkt, path)
				}
				if tc.unrelatedMarkdown != "" {
					h.writeTicket("unrelated-notes.md", tc.unrelatedMarkdown)
				}
				childID := "epc-child"
				switch tc.childEdit {
				case "new":
					childID = "epc-new"
					h.writeTicket(childID+".md", fmt.Sprintf("---\nid: %s\nparent: epc-a\nstatus: open\npath: %q\n---\n# New child\n", childID, initRepo(t)))
					assert.NotContains(t, d.tickets, childID)
				case "repository":
					other := initRepo(t)
					require.NoError(t, cli.Update(h.cfg, childID, web.UpdateTicketRequest{Path: &other}))
					assert.Equal(t, h.repoDir, d.tickets[childID].ticket.Path, "the watcher has not adopted the local edit")
				case "malformed":
					h.writeTicket(childID+".md", "# Broken child\n")
				}
				var err error
				if entry == "run" {
					err = d.RunTicket("epc-a")
				} else {
					err = d.InitTicket("epc-a", web.InitTicketRequest{Status: "todo"})
				}
				if tc.wantErr {
					if tc.childEdit == "malformed" {
						require.ErrorContains(t, err, "no frontmatter found")
					} else {
						require.ErrorIs(t, err, web.ErrInvalidState)
						assert.NotEqual(t, h.repoDir, h.readTask(childID+".md").Path)
					}
					assert.True(t, h.readTask("epc-a.md").IsPassiveEpic())
					assert.Empty(t, d.queue)
					return
				}
				require.NoError(t, err)
				assert.True(t, h.readTask("epc-a.md").IsAgentEpic())
				assert.Len(t, d.queue, 1)
				assert.Equal(t, ticket.StatusOpen, h.readTask("epc-child.md").Status)
			})
		}
	}
}

func TestDaemon_LocalEpicInitQueuesWithoutAutoPickup(t *testing.T) {
	h := newEpicAgentHarness(t)
	h.cfg.AutoPickUp = new(false)
	calls := make(chan RunnerParams, 1)
	d := h.newDaemon(h.cfg, WithRunner(epicBlockingRunner(calls)))
	h.writeTicket("epc-a.md", h.epicMD("epc-a", "open"))
	child := h.epicChildMD("epc-child", "open", "epc-a", "2026-01-02T00:00:00Z")
	childPath := h.writeTicket("epc-child.md", child)
	stop := startDaemon(t, d)
	defer stop()
	waitEpicIndexed(t, d, "epc-a")

	srv, err := web.New(d, d.broker, "127.0.0.1", 0, "", nil, d.tmuxSession, testLogger(t))
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	defer func() { require.NoError(t, srv.Shutdown(context.Background())) }()
	cfg := *h.cfg
	host, port, err := net.SplitHostPort(srv.Addr())
	require.NoError(t, err)
	cfg.Web.Host = host
	cfg.Web.Port, err = strconv.Atoi(port)
	require.NoError(t, err)
	require.NoError(t, cli.Enable(&cfg, "epc-a", cli.EnableOpts{Status: "todo"}, io.Discard))
	p := awaitEpicAgent(t, calls)
	assert.Equal(t, "epc-a", p.TicketID)
	got := h.waitForStatus("epc-a.md", ticket.StatusInProgress, 5*time.Second)
	assert.True(t, got.IsAgentEpic())
	after, err := os.ReadFile(childPath)
	require.NoError(t, err)
	assert.Equal(t, child, string(after))
	require.NoError(t, d.PauseTicket("epc-a"))
	waitForAgentsDone(t, d, 5*time.Second)
}

func TestDaemon_InitDanglingGroupingChild(t *testing.T) {
	for _, cachedParent := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached-parent=%t", cachedParent), func(t *testing.T) {
			h := newHarness(t)
			h.cfg.AutoPickUp = new(false)
			d := h.newDaemon(h.cfg)
			if cachedParent {
				path := h.writeTicket("deleted-epic.md", h.epicMD("deleted-epic", "open"))
				parent, err := ticket.ParseFile(path)
				require.NoError(t, err)
				d.tickets[parent.ID] = newTicketState(parent, path)
				require.NoError(t, os.Remove(path))
			}
			path := h.writeTicket("child.md", fmt.Sprintf("---\nid: child\nparent: deleted-epic\nstatus: open\npath: %q\n---\n# Child\n", h.repoDir))
			tkt, err := ticket.ParseFile(path)
			require.NoError(t, err)
			d.tickets["child"] = newTicketState(tkt, path)
			require.NoError(t, d.InitTicket("child", web.InitTicketRequest{Status: "todo"}))
			assert.Equal(t, ticket.StatusTodo, h.readTask("child.md").Status)
			assert.Len(t, d.queue, 1)
		})
	}
}
