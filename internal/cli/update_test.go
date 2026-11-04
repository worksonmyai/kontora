package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
)

func updateTestConfig(dir string) *config.Config {
	return &config.Config{
		TicketsDir: dir,
		Statuses:   []string{"blocked"},
		Pipelines: map[string]config.Pipeline{
			"one-stage": {{Stage: "s1", Agent: "a1", OnSuccess: "done", OnFailure: "pause"}},
			"two-stage": {{Stage: "s1", Agent: "a1", OnSuccess: "next", OnFailure: "pause"}, {Stage: "s2", Agent: "a1", OnSuccess: "done", OnFailure: "pause"}},
		},
		Agents: map[string]config.Agent{"a1": {Binary: "true"}},
	}
}

func TestUpdate_UpdatesSelectedFields(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: open
pipeline: one-stage
path: ~/old/path
agent: a1
---
# Original body
`)

	cfg := updateTestConfig(dir)
	newBody := "# Updated body\n\nNew content.\n"
	require.NoError(t, Update(cfg, "tst-001", web.UpdateTicketRequest{
		Pipeline: new("two-stage"),
		Body:     &newBody,
	}))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "pipeline: two-stage")
	assert.Contains(t, content, "# Updated body")
	// Untouched fields stay intact.
	assert.Contains(t, content, "path: ~/old/path")
	assert.Contains(t, content, "agent: a1")
}

func TestUpdate_ClearsAgentWithEmptyString(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-002.md", `---
id: tst-002
status: paused
pipeline: one-stage
path: ~/repo
agent: a1
---
# Ticket
`)

	cfg := updateTestConfig(dir)
	require.NoError(t, Update(cfg, "tst-002", web.UpdateTicketRequest{Agent: new("")}))

	data, err := os.ReadFile(filepath.Join(dir, "tst-002.md"))
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `agent: ""`)
	// Omitted fields are left intact.
	assert.Contains(t, content, "pipeline: one-stage")
	assert.Contains(t, content, "path: ~/repo")
}

// TestUpdate_NoneClearsFields covers the opt-out sentinel reaching update, so a
// user who learned "none" from kontora new does not get "unknown pipeline" here.
func TestUpdate_NoneClearsFields(t *testing.T) {
	cases := []struct {
		name string
		req  web.UpdateTicketRequest
		want string
	}{
		{name: "pipeline", req: web.UpdateTicketRequest{Pipeline: new(config.NoneSentinel)}, want: `pipeline: ""`},
		{name: "agent", req: web.UpdateTicketRequest{Agent: new(config.NoneSentinel)}, want: `agent: ""`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTicket(t, dir, "tst-none.md", `---
id: tst-none
status: open
pipeline: one-stage
path: ~/repo
agent: a1
---
# Body
`)

			cfg := updateTestConfig(dir)
			require.NoError(t, Update(cfg, "tst-none", tc.req))

			data, err := os.ReadFile(filepath.Join(dir, "tst-none.md"))
			require.NoError(t, err)
			assert.Contains(t, string(data), tc.want)
		})
	}
}

func TestUpdate_AgentEpicRepository(t *testing.T) {
	path := initTestRepo(t)
	other := initTestRepo(t)
	for _, id := range []string{"e1", "child"} {
		t.Run(id, func(t *testing.T) {
			dir := t.TempDir()
			writeTicket(t, dir, "e1.md", fmt.Sprintf("---\nid: e1\nkind: epic\nepic_execution: agent\nstatus: open\npath: %q\n---\n# Epic\n", path))
			writeTicket(t, dir, "child.md", fmt.Sprintf("---\nid: child\nparent: e1\nstatus: open\npath: %q\n---\n# Child\n", path))
			cfg := updateTestConfig(dir)
			require.ErrorIs(t, Update(cfg, id, web.UpdateTicketRequest{Path: &other}), app.ErrInvalidState)
			tkt, err := ticket.ParseFile(filepath.Join(dir, id+".md"))
			require.NoError(t, err)
			assert.Equal(t, path, tkt.Path)
			require.NoError(t, Update(cfg, id, web.UpdateTicketRequest{Path: &path}))
		})
	}
}

func TestUpdate_AgentEpicBranchSeparation(t *testing.T) {
	path := initTestRepo(t)
	for _, tc := range []struct {
		name, id, mode string
		req            web.UpdateTicketRequest
		wantErr        bool
	}{
		{name: "child cannot take integration branch", id: "child", mode: "agent", req: web.UpdateTicketRequest{Branch: new("integration")}, wantErr: true},
		{name: "epic cannot take child branch", id: "e1", mode: "agent", req: web.UpdateTicketRequest{Branch: new("child-work")}, wantErr: true},
		{name: "whitespace cannot bypass separation", id: "child", mode: "agent", req: web.UpdateTicketRequest{Branch: new(" integration ")}, wantErr: true},
		{name: "child keeps separate branch and epic base", id: "child", mode: "agent", req: web.UpdateTicketRequest{Branch: new("other-child"), BaseBranch: new("integration")}},
		{name: "epic keeps separate branch", id: "e1", mode: "agent", req: web.UpdateTicketRequest{Branch: new("other-integration")}},
		{name: "passive parent remains grouping only", id: "child", req: web.UpdateTicketRequest{Branch: new("integration")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTicket(t, dir, "e1.md", fmt.Sprintf("---\nid: e1\nkind: epic\nepic_execution: %q\nstatus: paused\npath: %q\nbranch: integration\n---\n# Epic\n", tc.mode, path))
			writeTicket(t, dir, "child.md", fmt.Sprintf("---\nid: child\nparent: e1\nstatus: open\npath: %q\nbranch: child-work\n---\n# Child\n", path))
			file := filepath.Join(dir, tc.id+".md")
			before, err := os.ReadFile(file)
			require.NoError(t, err)
			err = Update(updateTestConfig(dir), tc.id, tc.req)
			if tc.wantErr {
				require.ErrorIs(t, err, app.ErrInvalidState)
				after, err := os.ReadFile(file)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				return
			}
			require.NoError(t, err)
			got, err := ticket.ParseFile(file)
			require.NoError(t, err)
			assert.Equal(t, *tc.req.Branch, got.Branch)
		})
	}
}

func TestUpdate_DanglingGroupingParent(t *testing.T) {
	dir := t.TempDir()
	path := initTestRepo(t)
	writeTicket(t, dir, "child.md", "---\nid: child\nparent: deleted-epic\nstatus: open\npath: old\n---\n# Child\n")
	require.NoError(t, Update(updateTestConfig(dir), "child", web.UpdateTicketRequest{Path: &path, Branch: new("child-work")}))
	got, err := ticket.ParseFile(filepath.Join(dir, "child.md"))
	require.NoError(t, err)
	assert.Equal(t, path, got.Path)
	assert.Equal(t, "deleted-epic", got.Parent)
}

func TestUpdate_AgentEpicRestrictions(t *testing.T) {
	path := initTestRepo(t)
	for _, tc := range []struct {
		name, status string
		req          web.UpdateTicketRequest
		allowed      bool
	}{
		{name: "edit draft", status: "open", req: web.UpdateTicketRequest{Body: new("updated")}, allowed: true},
		{name: "edit paused", status: "paused", req: web.UpdateTicketRequest{Agent: new("a1")}, allowed: true},
		{name: "branch", status: "open", req: web.UpdateTicketRequest{Branch: new("integration"), BaseBranch: new("main")}, allowed: true},
		{name: "no pipeline", status: "open", req: web.UpdateTicketRequest{Pipeline: new("one-stage")}},
		{name: "clear pipeline", status: "open", req: web.UpdateTicketRequest{Pipeline: new("none")}, allowed: true},
		{name: "running", status: "in_progress", req: web.UpdateTicketRequest{Body: new("updated")}},
		{name: "done", status: "done", req: web.UpdateTicketRequest{Body: new("updated")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := fmt.Sprintf("---\nid: e1\nkind: epic\nepic_execution: agent\nstatus: %s\npath: %q\n---\n# Epic\n", tc.status, path)
			writeTicket(t, dir, "e1.md", original)
			err := Update(updateTestConfig(dir), "e1", tc.req)
			if tc.allowed {
				require.NoError(t, err)
				tkt, err := ticket.ParseFile(filepath.Join(dir, "e1.md"))
				require.NoError(t, err)
				assert.True(t, tkt.IsAgentEpic())
				return
			}
			require.ErrorIs(t, err, app.ErrInvalidState)
			out, err := os.ReadFile(filepath.Join(dir, "e1.md"))
			require.NoError(t, err)
			assert.Equal(t, original, string(out))
		})
	}
}

func TestUpdate_StateGuard(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		kind    string
		allowed bool
	}{
		{name: "in_progress", status: "in_progress"},
		{name: "done", status: "done"},
		{name: "cancelled", status: "cancelled"},
		{name: "archived", status: "archived"},
		// An epic is editable in every status it derives. The statuses above
		// are refused because an agent owns the file or the work is over, and
		// an epic has neither: the brief is why the ticket exists.
		{name: "epic in_progress", status: "in_progress", kind: "epic", allowed: true},
		{name: "epic done", status: "done", kind: "epic", allowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			kindLine := ""
			if tc.kind != "" {
				kindLine = "kind: " + tc.kind + "\n"
			}
			original := `---
id: tst-st
status: ` + tc.status + `
` + kindLine + `pipeline: one-stage
path: ~/repo
---
# Body
`
			writeTicket(t, dir, "tst-st.md", original)

			cfg := updateTestConfig(dir)
			err := Update(cfg, "tst-st", web.UpdateTicketRequest{Body: new("changed")})
			data, readErr := os.ReadFile(filepath.Join(dir, "tst-st.md"))
			require.NoError(t, readErr)

			if tc.allowed {
				require.NoError(t, err)
				assert.Contains(t, string(data), "changed")
				return
			}
			require.ErrorIs(t, err, app.ErrInvalidState)
			// File must be untouched.
			assert.Equal(t, original, string(data))
		})
	}
}

func TestUpdate_AllowsCustomStatus(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-cs.md", `---
id: tst-cs
status: blocked
pipeline: one-stage
path: ~/repo
---
# Body
`)

	cfg := updateTestConfig(dir)
	require.NoError(t, Update(cfg, "tst-cs", web.UpdateTicketRequest{Body: new("# Edited\n")}))

	data, err := os.ReadFile(filepath.Join(dir, "tst-cs.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "# Edited")
}

func TestUpdate_UnknownPipeline(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-up.md", `---
id: tst-up
status: open
path: ~/repo
---
# Body
`)

	cfg := updateTestConfig(dir)
	err := Update(cfg, "tst-up", web.UpdateTicketRequest{Pipeline: new("does-not-exist")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown pipeline")
}

func TestUpdate_UnknownAgent(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-ua.md", `---
id: tst-ua
status: open
path: ~/repo
---
# Body
`)

	cfg := updateTestConfig(dir)
	err := Update(cfg, "tst-ua", web.UpdateTicketRequest{Agent: new("ghost")})
	require.ErrorIs(t, err, app.ErrUnknownAgent)
}
