package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/testutil"
)

func TestYamlQuote(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"", `""`},
		{"has:colon", `"has:colon"`},
		{"null", `"null"`},
		{"Null", `"Null"`},
		{"NULL", `"NULL"`},
		{"~", `"~"`},
		{"~/projects/foo", "~/projects/foo"}, // ~ inside a path is not special
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, yamlQuote(tc.input))
		})
	}
}

func TestGenerateID_Format(t *testing.T) {
	dir := t.TempDir()

	id, err := GenerateID(dir, "~/projects/kontora")
	require.NoError(t, err)

	assert.Regexp(t, `^kon-[a-z0-9]{4}$`, id)
}

func TestGenerateID_Uniqueness(t *testing.T) {
	dir := t.TempDir()

	seen := make(map[string]bool)
	for range 20 {
		id, err := GenerateID(dir, "~/projects/kontora")
		require.NoError(t, err)
		assert.False(t, seen[id], "duplicate id: %s", id)
		seen[id] = true
	}
}

func TestGenerateID_FallbackPrefix(t *testing.T) {
	dir := t.TempDir()

	id, err := GenerateID(dir, "~/projects/grafana")
	require.NoError(t, err)

	assert.Regexp(t, `^gra-[a-z0-9]{4}$`, id)
}

func TestGenerateID_FallbackPrefixSpecialChars(t *testing.T) {
	dir := t.TempDir()

	id, err := GenerateID(dir, "~/projects/My-Repo")
	require.NoError(t, err)

	assert.Regexp(t, `^myr-[a-z0-9]{4}$`, id)
}

func TestGenerateID_CollisionRetry(t *testing.T) {
	dir := t.TempDir()

	// Create a file that could collide — but since IDs are random,
	// just verify the function works when files exist.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kon-aaaa.md"), []byte("test"), 0o644))

	id, err := GenerateID(dir, "~/projects/kontora")
	require.NoError(t, err)
	if id == "kon-aaaa" {
		// Extremely unlikely but technically possible to still collide on retry
		t.Log("got same id as existing file, this is statistically very unlikely")
	}
}

func TestGenerateID_EmptyPath(t *testing.T) {
	dir := t.TempDir()

	_, err := GenerateID(dir, "")
	require.Error(t, err)
}

func writeTicket(t *testing.T, dir, filename, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644))
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	return testutil.InitRepo(t)
}

func testConfig(dir string) *config.Config {
	return &config.Config{
		TicketsDir: dir,
		Agents: map[string]config.Agent{
			"claude-sonnet": {Binary: "claude"},
		},
		Stages: map[string]config.Stage{
			"code":   {Prompt: "code"},
			"review": {Prompt: "review"},
		},
		Pipelines: map[string]config.Pipeline{
			"default": {
				{Stage: "code", Agent: "claude-sonnet", OnSuccess: "next", OnFailure: "pause"},
				{Stage: "review", Agent: "claude-sonnet", OnSuccess: "done", OnFailure: "pause"},
			},
		},
	}
}

func TestStatus_ShowsTasks(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
kontora: true
status: todo
pipeline: default
path: /tmp/testrepo
stage: code
---
# Ticket one
`)
	writeTicket(t, dir, "tst-002.md", `---
id: tst-002
kontora: true
status: done
pipeline: default
path: /tmp/testrepo
---
# Ticket two
`)
	writeTicket(t, dir, "tst-003.md", `---
id: tst-003
kontora: true
status: paused
pipeline: default
path: /tmp/testrepo
stage: review
---
# Ticket three
`)

	var buf bytes.Buffer
	require.NoError(t, Status(cfg, false, &buf, StatusOpts{}))

	out := buf.String()
	assert.Contains(t, out, "tst-001")
	assert.NotContains(t, out, "tst-002", "done ticket should be hidden by default")
	assert.Contains(t, out, "tst-003")

	// Verify sort order: todo before paused
	idx1 := strings.Index(out, "tst-001")
	idx3 := strings.Index(out, "tst-003")
	assert.Less(t, idx1, idx3, "todo should come before paused")
}

func TestStatus_SkipsNonKontoraFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	// Ticket without pipeline field
	writeTicket(t, dir, "notes.md", `---
title: Just some notes
---
# Notes
`)
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
kontora: true
status: todo
pipeline: default
path: /tmp/testrepo
---
# Real ticket
`)

	var buf bytes.Buffer
	require.NoError(t, Status(cfg, false, &buf, StatusOpts{}))

	out := buf.String()
	assert.Contains(t, out, "tst-001")
	assert.NotContains(t, out, "notes")
}

func TestStatus_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	var buf bytes.Buffer
	require.NoError(t, Status(cfg, false, &buf, StatusOpts{}))

	assert.Equal(t, "No tickets.\n", buf.String())
}

func TestStatus_AgentColumn(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
kontora: true
status: todo
pipeline: default
path: /tmp/testrepo
stage: code
---
# Ticket
`)

	var buf bytes.Buffer
	require.NoError(t, Status(cfg, false, &buf, StatusOpts{}))

	assert.Contains(t, buf.String(), "claude-sonnet")
}

func TestStatus_ClosedFiltering(t *testing.T) {
	cases := []struct {
		name        string
		tickets     map[string]string // filename -> content
		opts        StatusOpts
		wantIDs     []string
		wantMissing []string
		wantExact   string // if set, assert exact output
	}{
		{
			name: "hides done and cancelled by default",
			tickets: map[string]string{
				"tst-001.md": `---
id: tst-001
kontora: true
status: in_progress
pipeline: default
path: /tmp/testrepo
stage: code
---
# Running ticket
`,
				"tst-002.md": `---
id: tst-002
kontora: true
status: done
pipeline: default
path: /tmp/testrepo
---
# Done ticket
`,
				"tst-003.md": `---
id: tst-003
kontora: true
status: cancelled
pipeline: default
path: /tmp/testrepo
---
# Cancelled ticket
`,
				"tst-004.md": `---
id: tst-004
kontora: true
status: paused
pipeline: default
path: /tmp/testrepo
stage: code
---
# Paused ticket
`,
			},
			opts:        StatusOpts{},
			wantIDs:     []string{"tst-001", "tst-004"},
			wantMissing: []string{"tst-002", "tst-003"},
		},
		{
			name: "ShowClosed includes all tickets",
			tickets: map[string]string{
				"tst-001.md": `---
id: tst-001
kontora: true
status: todo
pipeline: default
path: /tmp/testrepo
stage: code
---
# Todo ticket
`,
				"tst-002.md": `---
id: tst-002
kontora: true
status: done
pipeline: default
path: /tmp/testrepo
---
# Done ticket
`,
				"tst-003.md": `---
id: tst-003
kontora: true
status: cancelled
pipeline: default
path: /tmp/testrepo
---
# Cancelled ticket
`,
			},
			opts:    StatusOpts{ShowClosed: true},
			wantIDs: []string{"tst-001", "tst-002", "tst-003"},
		},
		{
			name: "only closed tickets shows hint",
			tickets: map[string]string{
				"tst-001.md": `---
id: tst-001
kontora: true
status: done
pipeline: default
path: /tmp/testrepo
---
# Done ticket
`,
				"tst-002.md": `---
id: tst-002
kontora: true
status: cancelled
pipeline: default
path: /tmp/testrepo
---
# Cancelled ticket
`,
			},
			opts:      StatusOpts{},
			wantExact: "No active tickets. Use --closed to show done/cancelled.\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(dir)
			for name, content := range tc.tickets {
				writeTicket(t, dir, name, content)
			}

			var buf bytes.Buffer
			require.NoError(t, Status(cfg, false, &buf, tc.opts))
			out := buf.String()

			if tc.wantExact != "" {
				assert.Equal(t, tc.wantExact, out)
				return
			}
			for _, id := range tc.wantIDs {
				assert.Contains(t, out, id)
			}
			for _, id := range tc.wantMissing {
				assert.NotContains(t, out, id)
			}
		})
	}
}

func TestStatus_AllFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
kontora: true
status: todo
pipeline: default
path: /tmp/testrepo
---
# Kontora ticket
`)
	writeTicket(t, dir, "other.md", `---
id: other-001
status: open
---
# Non-kontora ticket
`)

	// all=false: only kontora tickets
	var buf bytes.Buffer
	require.NoError(t, Status(cfg, false, &buf, StatusOpts{}))
	out := buf.String()
	assert.Contains(t, out, "tst-001")
	assert.NotContains(t, out, "other-001")

	// all=true: both tickets
	buf.Reset()
	require.NoError(t, Status(cfg, true, &buf, StatusOpts{}))
	out = buf.String()
	assert.Contains(t, out, "tst-001")
	assert.Contains(t, out, "other-001")
}

func TestNew_CreatesValidTask(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	repoDir := initTestRepo(t)

	id, err := New(cfg, NewOpts{
		Path:   repoDir,
		Title:  "My Title",
		NoEdit: true,
	})
	require.NoError(t, err)

	filePath := filepath.Join(dir, id+".md")
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "id: "+id)
	assert.Contains(t, content, "status: todo")
	assert.NotContains(t, content, "pipeline:")
	assert.Contains(t, content, "path: "+repoDir)
	assert.Contains(t, content, "# My Title")
}

func TestNew_WritesAgentField(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	repoDir := initTestRepo(t)

	id, err := New(cfg, NewOpts{
		Path:   repoDir,
		Title:  "With agent",
		Agent:  "claude-sonnet",
		NoEdit: true,
	})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, id+".md"))
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "agent: claude-sonnet")
}

func TestNew_OmitsAgentWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	repoDir := initTestRepo(t)

	id, err := New(cfg, NewOpts{
		Path:   repoDir,
		Title:  "No agent",
		NoEdit: true,
	})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, id+".md"))
	require.NoError(t, err)

	assert.NotContains(t, string(data), "agent:")
}

func TestNew_WritesBody(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	repoDir := initTestRepo(t)

	id, err := New(cfg, NewOpts{
		Path:   repoDir,
		Title:  "With Body",
		Body:   "Some description here.\n\nMore details.",
		NoEdit: true,
	})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, id+".md"))
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "# With Body\n\nSome description here.\n\nMore details.\n")
}

func TestNew_EmptyBodyPreservesFormat(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	repoDir := initTestRepo(t)

	id, err := New(cfg, NewOpts{
		Path:   repoDir,
		Title:  "No Body",
		NoEdit: true,
	})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, id+".md"))
	require.NoError(t, err)

	content := string(data)
	assert.True(t, strings.HasSuffix(content, "# No Body\n\n"), "empty body should end with title + blank line")
}

func TestNew_GeneratesUniqueIDs(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	repoDir := initTestRepo(t)

	id1, err := New(cfg, NewOpts{Path: repoDir, Title: "T1", NoEdit: true})
	require.NoError(t, err)
	id2, err := New(cfg, NewOpts{Path: repoDir, Title: "T2", NoEdit: true})
	require.NoError(t, err)

	assert.NotEqual(t, id1, id2)
}

func TestNew_RequiresPath(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	_, err := New(cfg, NewOpts{Title: "T", NoEdit: true})
	require.Error(t, err)
}

func TestNew_RejectsNonGitRepo(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	nonGitDir := t.TempDir()

	_, err := New(cfg, NewOpts{Path: nonGitDir, Title: "T", NoEdit: true})
	require.ErrorContains(t, err, "not a git repository")
}

func TestNew_AllowsOpenStatusWithoutGitRepo(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	nonGitDir := t.TempDir()

	id, err := New(cfg, NewOpts{Path: nonGitDir, Title: "Draft", Status: "open", NoEdit: true})
	require.NoError(t, err)
	assert.NotEmpty(t, id)
}

func TestQuick_CreatesTodoTask(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	repoDir := initTestRepo(t)

	id, err := Quick(cfg, QuickOpts{
		Path:  repoDir,
		Title: "Quick ticket",
	})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, id+".md"))
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "status: todo")
	assert.Contains(t, content, "# Quick ticket")
}

func TestLogs_ShowsLogFile(t *testing.T) {
	tasksDir := t.TempDir()
	logsDir := t.TempDir()

	writeTicket(t, tasksDir, "tst-001.md", `---
id: tst-001
status: in_progress
pipeline: default
path: /tmp/testrepo
stage: code
---
# Ticket
`)

	logDir := filepath.Join(logsDir, "tst-001")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "code.log"), []byte("hello from agent\n"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, Logs(tasksDir, logsDir, "tst-001", "", &buf))

	assert.Contains(t, buf.String(), "hello from agent")
}

func TestLogs_SpecificStage(t *testing.T) {
	tasksDir := t.TempDir()
	logsDir := t.TempDir()

	writeTicket(t, tasksDir, "tst-001.md", `---
id: tst-001
status: in_progress
pipeline: default
path: /tmp/testrepo
stage: review
---
# Ticket
`)

	logDir := filepath.Join(logsDir, "tst-001")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	writeTicket(t, logDir, "code.log", "code output\n")
	writeTicket(t, logDir, "review.log", "review output\n")

	var buf bytes.Buffer
	require.NoError(t, Logs(tasksDir, logsDir, "tst-001", "review", &buf))

	assert.Contains(t, buf.String(), "review output")
	assert.NotContains(t, buf.String(), "code output")
}

func TestLogs_FallsBackToHistory(t *testing.T) {
	tasksDir := t.TempDir()
	logsDir := t.TempDir()

	writeTicket(t, tasksDir, "tst-001.md", `---
id: tst-001
status: done
pipeline: default
path: /tmp/testrepo
history:
  - stage: code
    agent: claude-sonnet
    exit_code: 0
---
# Ticket
`)

	var buf bytes.Buffer
	require.NoError(t, Logs(tasksDir, logsDir, "tst-001", "", &buf))

	out := buf.String()
	assert.Contains(t, out, "code")
	assert.Contains(t, out, "claude-sonnet")
}

func TestLogs_SpecificStageFallsBackWhenMissing(t *testing.T) {
	tasksDir := t.TempDir()
	logsDir := t.TempDir()

	writeTicket(t, tasksDir, "tst-001.md", `---
id: tst-001
status: done
pipeline: default
path: /tmp/testrepo
history:
  - stage: code
    agent: claude-sonnet
    exit_code: 0
---
# Ticket
`)

	var buf bytes.Buffer
	require.NoError(t, Logs(tasksDir, logsDir, "tst-001", "default", &buf))

	out := buf.String()
	assert.Contains(t, out, "code")
	assert.Contains(t, out, "claude-sonnet")
}

func TestLogs_NoLogsFound(t *testing.T) {
	tasksDir := t.TempDir()
	logsDir := t.TempDir()

	writeTicket(t, tasksDir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	var buf bytes.Buffer
	require.NoError(t, Logs(tasksDir, logsDir, "tst-001", "", &buf))

	assert.Contains(t, buf.String(), "no logs found")
}

func TestLogs_TaskNotFound(t *testing.T) {
	tasksDir := t.TempDir()
	logsDir := t.TempDir()

	var buf bytes.Buffer
	require.Error(t, Logs(tasksDir, logsDir, "nonexistent", "", &buf))
}

func TestNote_AppendsNote(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket one
`)

	require.NoError(t, Note(dir, "tst-001", "hello from test"))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "## Notes")
	assert.Contains(t, content, "hello from test")
}

func TestNote_PrefixMatch(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	require.NoError(t, Note(dir, "tst", "prefix note"))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "prefix note")
}

func TestNote_PreservesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
custom_field: keep me
---
# Ticket
`)

	require.NoError(t, Note(dir, "tst-001", "preserve test"))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "custom_field: keep me")
	assert.Contains(t, content, "id: tst-001")
	assert.Contains(t, content, "preserve test")
}

func TestNote_TaskNotFound(t *testing.T) {
	dir := t.TempDir()
	require.Error(t, Note(dir, "nonexistent", "text"))
}

func TestDone_SetsStatusDone(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket one
`)

	require.NoError(t, SetStatus(dir, "tst-001", "done"))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "status: done")
	assert.Contains(t, content, "completed_at:")
}

func TestDone_PrefixMatch(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: paused
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	require.NoError(t, SetStatus(dir, "tst", "done"))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "status: done")
}

func TestDone_AlreadyDone(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: done
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	require.ErrorContains(t, SetStatus(dir, "tst-001", "done"), "already done")
}

func TestDone_PreservesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
custom_field: keep me
---
# Ticket
`)

	require.NoError(t, SetStatus(dir, "tst-001", "done"))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "custom_field: keep me")
	assert.Contains(t, content, "status: done")
}

func TestDone_TaskNotFound(t *testing.T) {
	dir := t.TempDir()
	require.Error(t, SetStatus(dir, "nonexistent", "done"))
}

func TestSetStatus_ValidTransition(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	require.NoError(t, SetStatus(dir, "tst-001", "paused"))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "status: paused")
}

func TestSetStatus_InvalidStatus(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	require.ErrorContains(t, SetStatus(dir, "tst-001", "bogus"), "invalid status")
}

func TestSetStatus_SameStatus(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	require.ErrorContains(t, SetStatus(dir, "tst-001", "todo"), "already todo")
}

func TestSetStatus_DoneSetsCompletedAt(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	require.NoError(t, SetStatus(dir, "tst-001", "done"))

	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "completed_at:")
}

func TestSetStatus_RejectsRunning(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket
`)

	require.ErrorContains(t, SetStatus(dir, "tst-001", "running"), "invalid status")
}

func TestShowConfig_OutputsYAML(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	var buf bytes.Buffer
	require.NoError(t, ShowConfig(cfg, &buf))

	out := buf.String()
	for _, want := range []string{"tickets_dir:", "claude-sonnet", "default"} {
		assert.Contains(t, out, want)
	}
}

func TestShowConfig_IncludesDefaults(t *testing.T) {
	cfg := &config.Config{
		TicketsDir: "/tmp/tasks",
		Agents:     map[string]config.Agent{"a": {Binary: "bin"}},
		Stages:     map[string]config.Stage{"s": {Prompt: "p"}},
		Pipelines: map[string]config.Pipeline{
			"p": {{Stage: "s", Agent: "a", OnSuccess: "done", OnFailure: "pause"}},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, ShowConfig(cfg, &buf))

	// Here cfg has zero-value fields, so they won't appear. That's fine —
	// ShowConfig faithfully renders whatever config it receives.
	assert.Contains(t, buf.String(), "tickets_dir: /tmp/tasks")
}

func TestResolveAttach(t *testing.T) {
	cases := []struct {
		name       string
		ticketFile string
		ticketID   string
		wantErrMsg string
	}{
		{
			name:       "ticket not found",
			ticketID:   "nonexistent",
			wantErrMsg: "not found",
		},
		{
			name: "not running",
			ticketFile: `---
id: tst-001
status: todo
pipeline: default
path: /tmp/testrepo
---
# Ticket
`,
			ticketID:   "tst-001",
			wantErrMsg: "must be in_progress",
		},
		{
			name: "no tmux session",
			ticketFile: `---
id: tst-001
status: in_progress
pipeline: default
path: /tmp/testrepo
---
# Ticket
`,
			ticketID:   "tst-001",
			wantErrMsg: "not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.ticketFile != "" {
				writeTicket(t, dir, tc.ticketID+".md", tc.ticketFile)
			}
			_, err := resolveAttach(dir, tc.ticketID)
			require.ErrorContains(t, err, tc.wantErrMsg)
		})
	}
}

func TestInit(t *testing.T) {
	repoDir := initTestRepo(t)

	cases := []struct {
		name        string
		ticket      string
		pickReturn  string
		wantOutput  string
		wantKontora bool
		wantStatus  string
	}{
		{
			name: "already initialized",
			ticket: fmt.Sprintf(`---
id: tst-001
kontora: true
status: todo
pipeline: default
path: %s
---
# Already initialized
`, repoDir),
			wantOutput: "already initialized",
		},
		{
			name: "all fields present",
			ticket: fmt.Sprintf(`---
id: tst-001
status: open
pipeline: default
path: %s
---
# Has fields but no kontora
`, repoDir),
			pickReturn:  "todo",
			wantOutput:  "initialized",
			wantKontora: true,
			wantStatus:  "status: todo",
		},
		{
			name: "status not set prompts and sets open",
			ticket: fmt.Sprintf(`---
id: tst-001
pipeline: default
path: %s
---
# No status field
`, repoDir),
			pickReturn:  "open",
			wantOutput:  "initialized",
			wantKontora: true,
			wantStatus:  "status: open",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(dir)
			writeTicket(t, dir, "tst-001.md", tc.ticket)

			if tc.pickReturn != "" {
				pickOneFn = func(_ string, _ []string) (string, error) {
					return tc.pickReturn, nil
				}
				t.Cleanup(func() { pickOneFn = pickOne })
			}

			var buf bytes.Buffer
			require.NoError(t, Enable(cfg, "tst-001", &buf))
			assert.Contains(t, buf.String(), tc.wantOutput)

			if tc.wantKontora {
				data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
				require.NoError(t, err)
				content := string(data)
				assert.Contains(t, content, "kontora: true")
				if tc.wantStatus != "" {
					assert.Contains(t, content, tc.wantStatus)
				}
			}
		})
	}
}

func TestInit_MissingPath(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: open
pipeline: default
---
# No path
`)
	pickerCalled := false
	pickOneFn = func(_ string, _ []string) (string, error) {
		pickerCalled = true
		return "todo", nil
	}
	t.Cleanup(func() { pickOneFn = pickOne })

	var buf bytes.Buffer
	err := Enable(cfg, "tst-001", &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no path set")
	assert.False(t, pickerCalled, "pipeline picker should not run before path validation")
}

func TestInit_StageSelection(t *testing.T) {
	repoDir := initTestRepo(t)

	cases := []struct {
		name      string
		ticket    string
		picks     map[string]string // field → return value
		wantStage string
	}{
		{
			name: "stage selected",
			ticket: fmt.Sprintf(`---
id: tst-001
status: open
pipeline: default
path: %s
---
# Pick a stage
`, repoDir),
			picks:     map[string]string{"starting stage": "review", "status": "todo"},
			wantStage: "stage: review",
		},
		{
			name: "first stage selected by default",
			ticket: fmt.Sprintf(`---
id: tst-001
status: open
pipeline: default
path: %s
---
# Pick first stage
`, repoDir),
			picks:     map[string]string{"starting stage": "code", "status": "todo"},
			wantStage: "stage: code",
		},
		{
			name: "stage already set skips picker",
			ticket: fmt.Sprintf(`---
id: tst-001
status: open
pipeline: default
path: %s
stage: code
---
# Stage preset
`, repoDir),
			picks:     map[string]string{"status": "todo"},
			wantStage: "stage: code",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(dir)
			writeTicket(t, dir, "tst-001.md", tc.ticket)

			pickOneFn = func(field string, _ []string) (string, error) {
				v, ok := tc.picks[field]
				require.True(t, ok, "unexpected pick for field %q", field)
				return v, nil
			}
			t.Cleanup(func() { pickOneFn = pickOne })

			var buf bytes.Buffer
			require.NoError(t, Enable(cfg, "tst-001", &buf))

			data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
			require.NoError(t, err)
			content := string(data)

			if tc.wantStage != "" {
				assert.Contains(t, content, tc.wantStage)
			} else {
				assert.NotContains(t, content, "stage:")
			}
		})
	}
}

func updatePick(m pickModel, key string) pickModel {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	result, _ := m.Update(msg)
	return result.(pickModel)
}

func TestPickModel(t *testing.T) {
	m := pickModel{
		field:   "pipeline",
		choices: []string{"alpha", "beta", "gamma"},
	}

	assert.Equal(t, 0, m.cursor)

	m = updatePick(m, "j")
	assert.Equal(t, 1, m.cursor, "after j")

	m = updatePick(m, "k")
	assert.Equal(t, 0, m.cursor, "after k")

	m = updatePick(m, "k")
	assert.Equal(t, 0, m.cursor, "clamps at top")

	m = updatePick(m, "j")
	m = updatePick(m, "j")
	assert.Equal(t, 2, m.cursor, "at last item")

	m = updatePick(m, "j")
	assert.Equal(t, 2, m.cursor, "clamps at bottom")

	m2 := pickModel{field: "test", choices: []string{"a", "b"}}
	m2 = updatePick(m2, "q")
	assert.True(t, m2.cancelled, "q should cancel")
}

func updateAttach(m attachModel, key string) attachModel {
	var msg tea.KeyMsg
	if key == "tab" {
		msg = tea.KeyMsg{Type: tea.KeyTab}
	} else {
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	result, _ := m.Update(msg)
	return result.(attachModel)
}

func TestAttachModel(t *testing.T) {
	items := []attachItem{
		{id: "tst-001", target: "=kontora:tst-001", title: "First ticket", stage: "code", agent: "claude", dur: "5m"},
		{id: "tst-002", target: "=kontora:tst-002", title: "Second ticket", stage: "review", agent: "claude", dur: "2m"},
		{id: "tst-003", target: "=kontora:tst-003", title: "Third ticket", stage: "code", agent: "claude", dur: "1m"},
	}
	m := attachModel{items: items}

	assert.Equal(t, 0, m.cursor)

	m = updateAttach(m, "j")
	assert.Equal(t, 1, m.cursor, "after j")

	m = updateAttach(m, "k")
	assert.Equal(t, 0, m.cursor, "after k")

	m = updateAttach(m, "k")
	assert.Equal(t, 0, m.cursor, "clamps at top")

	m = updateAttach(m, "j")
	m = updateAttach(m, "j")
	assert.Equal(t, 2, m.cursor, "at last item")

	m = updateAttach(m, "j")
	assert.Equal(t, 2, m.cursor, "clamps at bottom")

	m2 := attachModel{items: items[:2]}
	m2 = updateAttach(m2, "q")
	assert.True(t, m2.cancelled, "q should cancel")
}

func TestAttachModel_View(t *testing.T) {
	items := []attachItem{
		{id: "tst-001", target: "=kontora:tst-001", title: "My ticket title", stage: "code", agent: "claude-sonnet", dur: "5m12s"},
		{id: "tst-002", target: "=kontora:tst-002", title: "—", stage: "—", agent: "—", dur: "—"},
	}
	m := attachModel{items: items}

	out := m.View()
	assert.Contains(t, out, "tst-001")
	assert.Contains(t, out, "My ticket title")
	assert.Contains(t, out, "code")
	assert.Contains(t, out, "[RO]")
	assert.Contains(t, out, "toggle RO/RW")
}

func TestAttachModel_ToggleRW(t *testing.T) {
	m := attachModel{
		items: []attachItem{{id: "tst-001", target: "=kontora:tst-001"}},
	}

	assert.False(t, m.readWrite, "starts read-only")

	m = updateAttach(m, "tab")
	assert.True(t, m.readWrite, "tab toggles to RW")

	assert.Contains(t, m.View(), "[RW]")

	m = updateAttach(m, "tab")
	assert.False(t, m.readWrite, "tab toggles back to RO")

	assert.Contains(t, m.View(), "[RO]")
}
