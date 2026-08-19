package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafeStage(t *testing.T) {
	tests := []struct {
		stage string
		want  bool
	}{
		{"implement", true},
		{"step2-annotation", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{`a\b`, false},
		{"../../etc/passwd", false},
		{"/absolute", false},
	}
	for _, tt := range tests {
		t.Run(tt.stage, func(t *testing.T) {
			assert.Equal(t, tt.want, SafeStage(tt.stage))
		})
	}
}

func TestSafeRef(t *testing.T) {
	tests := []struct {
		name string
		kind string
		ref  string
		want bool
	}{
		{"claude uuid", KindClaude, "2f1e0c7a-1111-2222-3333-444455556666", true},
		{"claude with separator", KindClaude, "projects/2f1e0c7a", false},
		{"claude empty", KindClaude, "", false},
		{"claude dotdot", KindClaude, "..", false},
		{"claude star", KindClaude, "*", false},
		{"claude question mark", KindClaude, "2f1e0c7?", false},
		{"claude character class", KindClaude, "[a-z]*", false},
		{"pi relative", KindPi, "pi-sessions/implement/01JC9.jsonl", true},
		{"pi escaping", KindPi, "../../../etc/passwd", false},
		{"pi absolute", KindPi, "/etc/passwd", false},
		{"pi empty", KindPi, "", false},
		{"pi backslash", KindPi, `pi-sessions\implement\x.jsonl`, false},
		{"unknown kind", "", "anything", false},
		{"unknown kind named", "codex", "anything", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SafeRef(tt.kind, tt.ref))
		})
	}
}

func TestClaudeConfigDir(t *testing.T) {
	assert.Equal(t, "~/.claude", ClaudeConfigDir(nil))
	assert.Equal(t, "~/.claude", ClaudeConfigDir(map[string]string{"CLAUDE_CONFIG_DIR": ""}))
	assert.Equal(t, "/tmp/cc", ClaudeConfigDir(map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/cc"}))
}

func TestClaudeFiles(t *testing.T) {
	const uuid = "2f1e0c7a-1111-2222-3333-444455556666"
	dir := t.TempDir()
	plant := func(project string) string {
		p := filepath.Join(dir, "projects", project, uuid+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("{}\n"), 0o644))
		return p
	}

	t.Run("no match", func(t *testing.T) {
		matches, pattern, err := ClaudeFiles(dir, uuid)
		require.NoError(t, err)
		assert.Empty(t, matches)
		assert.Contains(t, pattern, uuid+".jsonl")
	})

	t.Run("one project", func(t *testing.T) {
		want := plant("-Users-a-projects-kontora")
		matches, _, err := ClaudeFiles(dir, uuid)
		require.NoError(t, err)
		assert.Equal(t, []string{want}, matches)
	})

	t.Run("same uuid in two projects", func(t *testing.T) {
		plant("-Users-a-projects-other")
		matches, _, err := ClaudeFiles(dir, uuid)
		require.NoError(t, err)
		assert.Len(t, matches, 2)
	})
}

func TestPiFiles(t *testing.T) {
	t.Run("missing dir", func(t *testing.T) {
		files, err := PiFiles(filepath.Join(t.TempDir(), "nope"))
		require.NoError(t, err)
		assert.Empty(t, files)
	})

	t.Run("empty dir", func(t *testing.T) {
		files, err := PiFiles(t.TempDir())
		require.NoError(t, err)
		assert.Empty(t, files)
	})

	t.Run("oldest first", func(t *testing.T) {
		dir := t.TempDir()
		base := time.Now().Add(-time.Hour)
		// Created in an order the result must not preserve, so that a passing
		// assertion can only come from the modification times.
		for name, age := range map[string]time.Duration{
			"third.jsonl":  2 * time.Minute,
			"first.jsonl":  0,
			"second.jsonl": time.Minute,
		} {
			p := filepath.Join(dir, name)
			require.NoError(t, os.WriteFile(p, []byte("{}\n"), 0o644))
			require.NoError(t, os.Chtimes(p, base, base.Add(age)))
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
		want := []string{
			filepath.Join(dir, "first.jsonl"),
			filepath.Join(dir, "second.jsonl"),
			filepath.Join(dir, "third.jsonl"),
		}
		files, err := PiFiles(dir)
		require.NoError(t, err)
		assert.Equal(t, want, files)
	})
}

func TestPaths(t *testing.T) {
	const logs, id = "/logs", "kon-1"
	assert.Equal(t, "/logs/kon-1", TicketDir(logs, id))
	assert.Equal(t, "/logs/kon-1/implement.log", LogPath(logs, id, "implement"))
	assert.Equal(t, "/logs/kon-1/implement.2.events.json", EventsPath(logs, id, "implement", 2))
	assert.Equal(t, "/logs/kon-1/pi-sessions/implement", PiDir(logs, id, "implement"))
	assert.Equal(t, "/logs/kon-1/implement.session", RecordPath(logs, id, "implement"))
	assert.Equal(t, "/logs/kon-1/implement.completed-session", CompletedRecordPath(logs, id, "implement"))
}

func TestReadRecord(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing", func(t *testing.T) {
		rec, err := ReadRecord(filepath.Join(dir, "none.session"))
		require.Error(t, err)
		assert.True(t, os.IsNotExist(err))
		assert.Nil(t, rec)
	})

	t.Run("malformed", func(t *testing.T) {
		p := filepath.Join(dir, "bad.session")
		require.NoError(t, os.WriteFile(p, []byte("not json"), 0o644))
		_, err := ReadRecord(p)
		require.Error(t, err)
	})

	t.Run("round trip", func(t *testing.T) {
		p := filepath.Join(dir, "implement.session")
		require.NoError(t, os.WriteFile(p, []byte(
			`{"session_id":"abc","stage":"implement","agent":"claude","worktree":"/wt","instance":"host","started_at":"2026-01-02T03:04:05Z"}`,
		), 0o644))
		rec, err := ReadRecord(p)
		require.NoError(t, err)
		assert.Equal(t, "abc", rec.SessionID)
		assert.Equal(t, "implement", rec.Stage)
		assert.Equal(t, "claude", rec.Agent)
		assert.Equal(t, "/wt", rec.Worktree)
		assert.Equal(t, "host", rec.Instance)
		assert.Equal(t, 2026, rec.StartedAt.Year())
	})
}
