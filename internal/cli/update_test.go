package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
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

func TestUpdate_RejectedByState(t *testing.T) {
	cases := []string{"in_progress", "done", "cancelled", "archived"}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()
			original := `---
id: tst-st
status: ` + status + `
pipeline: one-stage
path: ~/repo
---
# Body
`
			writeTicket(t, dir, "tst-st.md", original)

			cfg := updateTestConfig(dir)
			err := Update(cfg, "tst-st", web.UpdateTicketRequest{Body: new("changed")})
			require.ErrorIs(t, err, app.ErrInvalidState)

			// File must be untouched.
			data, err := os.ReadFile(filepath.Join(dir, "tst-st.md"))
			require.NoError(t, err)
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
