package assistant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input map[string]any
		want  Decision
	}{
		{name: "a read tool", tool: "Read", input: map[string]any{"file_path": "/x"}, want: DecisionRead},
		{name: "pi spells its read tools lowercase", tool: "read", want: DecisionRead},
		{name: "grep", tool: "Grep", want: DecisionRead},
		{name: "edit", tool: "Edit", want: DecisionWrite},
		{name: "write", tool: "Write", want: DecisionWrite},
		{name: "notebook edit", tool: "NotebookEdit", want: DecisionWrite},
		{name: "an unrecognised tool is a write", tool: "SomeMcpTool", want: DecisionWrite},
		{name: "a tool with no name is a write", tool: "", want: DecisionWrite},

		{name: "a kontora read verb", tool: "Bash", input: cmd("kontora ls"), want: DecisionRead},
		{name: "a kontora read verb with flags", tool: "Bash", input: cmd("kontora view kon-7d21 --json"), want: DecisionRead},
		{name: "an absolute kontora path", tool: "Bash", input: cmd("/usr/local/bin/kontora activity kon-7d21"), want: DecisionRead},
		{name: "a chain of read verbs", tool: "Bash", input: cmd("kontora ls && kontora stats"), want: DecisionRead},

		{name: "a kontora write verb", tool: "Bash", input: cmd("kontora run kon-7d21"), want: DecisionWrite},
		{name: "set-stage", tool: "Bash", input: cmd("kontora set-stage kon-7d21 implement"), want: DecisionWrite},
		{name: "one write in a chain taints the line", tool: "Bash", input: cmd("kontora ls && kontora move kon-7d21 done"), want: DecisionWrite},
		{name: "delete is held apart from a write", tool: "Bash", input: cmd("kontora delete kon-7d21"), want: DecisionDelete},
		{name: "a delete anywhere in a chain wins", tool: "Bash", input: cmd("kontora ls; kontora delete kon-7d21"), want: DecisionDelete},

		{name: "a non-kontora command is a write", tool: "Bash", input: cmd("ls -la"), want: DecisionWrite},
		{name: "a bare kontora with no verb is a write", tool: "Bash", input: cmd("kontora"), want: DecisionWrite},
		{name: "an unknown kontora verb is a write", tool: "Bash", input: cmd("kontora frobnicate"), want: DecisionWrite},
		{name: "an empty command is a write", tool: "Bash", input: cmd("   "), want: DecisionWrite},
		{name: "a command substitution hides what runs", tool: "Bash", input: cmd("kontora view $(kontora ls)"), want: DecisionWrite},
		{name: "a redirection hides where the bytes go", tool: "Bash", input: cmd("kontora ls > /tmp/out"), want: DecisionWrite},
		{name: "a pipe into another command is a write", tool: "Bash", input: cmd("kontora ls | xargs rm"), want: DecisionWrite},
		{name: "a background read hides what follows it", tool: "Bash", input: cmd("kontora ls & curl example.com"), want: DecisionWrite},
		{name: "the reference topics", tool: "Bash", input: cmd("kontora skills list"), want: DecisionRead},
		{name: "one reference topic", tool: "Bash", input: cmd("kontora skills show cli"), want: DecisionRead},

		{name: "the usage of a write verb", tool: "Bash", input: cmd("kontora new -h"), want: DecisionRead},
		{name: "the long help flag", tool: "Bash", input: cmd("kontora new --help"), want: DecisionRead},
		{name: "a help flag after a positional would append a note", tool: "Bash", input: cmd("kontora note kon-1 -h"), want: DecisionWrite},
		{name: "help does not excuse what follows it in a chain", tool: "Bash", input: cmd("kontora new -h && rm -rf x"), want: DecisionWrite},

		{name: "config prints", tool: "Bash", input: cmd("kontora config"), want: DecisionRead},
		{name: "config edit opens the daemon config", tool: "Bash", input: cmd("kontora config edit"), want: DecisionWrite},
		{name: "bash with no command field is a write", tool: "Bash", want: DecisionWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Classify(tt.tool, tt.input))
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		autonomy string
		decision Decision
		want     Verdict
	}{
		{config.AutonomyRead, DecisionRead, VerdictAllow},
		{config.AutonomyRead, DecisionWrite, VerdictDeny},
		{config.AutonomyRead, DecisionDelete, VerdictDeny},
		{config.AutonomyAsk, DecisionRead, VerdictAllow},
		{config.AutonomyAsk, DecisionWrite, VerdictPark},
		{config.AutonomyAsk, DecisionDelete, VerdictPark},
		{config.AutonomyAuto, DecisionRead, VerdictAllow},
		{config.AutonomyAuto, DecisionWrite, VerdictAllow},
		{config.AutonomyAuto, DecisionDelete, VerdictPark},
		{"nonsense", DecisionWrite, VerdictPark},
	}
	for _, tt := range tests {
		t.Run(tt.autonomy+"/"+string(tt.decision), func(t *testing.T) {
			assert.Equal(t, tt.want, Resolve(tt.autonomy, tt.decision))
		})
	}
}

func TestBuildArgs(t *testing.T) {
	claude := config.Agent{Binary: "claude"}
	pi := config.Agent{Binary: "pi"}

	tests := []struct {
		name        string
		agent       config.Agent
		model       string
		effort      string
		spec        TurnSpec
		wantErr     string
		wantContain [][]string
		wantAbsent  []string
	}{
		{
			name:  "a first claude turn opens a session",
			agent: claude,
			spec:  TurnSpec{Prompt: "what is running", SessionID: "S", GateFile: "/tmp/s.json", AddDirs: []string{"/logs", "/wt"}},
			wantContain: [][]string{
				{"--session-id", "S"},
				{"--settings", "/tmp/s.json"},
				{"--add-dir", "/logs", "/wt"},
				{"--print", "--output-format", "stream-json", "--verbose"},
			},
			wantAbsent: []string{"-r"},
		},
		{
			name:        "a claude turn asks for partial messages",
			agent:       claude,
			spec:        TurnSpec{Prompt: "long one", SessionID: "S", Stream: true},
			wantContain: [][]string{{"--verbose", "--include-partial-messages"}},
		},
		{
			name:       "a claude turn with streaming off does not",
			agent:      claude,
			spec:       TurnSpec{Prompt: "long one", SessionID: "S"},
			wantAbsent: []string{"--include-partial-messages"},
		},
		{
			name:       "pi has no flag for it",
			agent:      pi,
			spec:       TurnSpec{Prompt: "hi", SessionID: "S", SessionDir: "/d", Stream: true},
			wantAbsent: []string{"--include-partial-messages"},
		},
		{
			name:        "a later claude turn resumes it",
			agent:       claude,
			spec:        TurnSpec{Prompt: "and now", SessionID: "S", Resume: true},
			wantContain: [][]string{{"-r", "S"}},
			wantAbsent:  []string{"--session-id"},
		},
		{
			name:        "the prompt is the last argument",
			agent:       claude,
			spec:        TurnSpec{Prompt: "hello", SessionID: "S"},
			wantContain: [][]string{{"hello"}},
		},
		{
			name:  "a pi turn names the session and its directory",
			agent: pi,
			spec:  TurnSpec{Prompt: "hi", SessionID: "S", SessionDir: "/d", GateFile: "/tmp/e.js", SystemPrompt: "brief"},
			wantContain: [][]string{
				{"-e", "/tmp/e.js"},
				{"--append-system-prompt", "brief"},
				{"--print", "--session-id", "S", "--session-dir", "/d"},
			},
			wantAbsent: []string{"--mode", "-r"},
		},
		{
			name:        "a pi turn resumes by naming the same session id",
			agent:       pi,
			spec:        TurnSpec{Prompt: "again", SessionID: "S", SessionDir: "/d", Resume: true},
			wantContain: [][]string{{"--session-id", "S"}},
			wantAbsent:  []string{"-r"},
		},
		{
			name:        "the model reaches the agent's own flag",
			agent:       claude,
			model:       "sonnet",
			spec:        TurnSpec{SessionID: "S"},
			wantContain: [][]string{{"--model", "sonnet"}},
		},
		{
			name:    "an agent of no known kind is rejected",
			agent:   config.Agent{Binary: "aider"},
			spec:    TurnSpec{SessionID: "S"},
			wantErr: "only supports claude and pi",
		},
		{
			name:    "a pi effort against a thinking-level model is rejected",
			agent:   pi,
			model:   "anthropic/sonnet:high",
			effort:  "low",
			spec:    TurnSpec{SessionID: "S"},
			wantErr: "both set pi's thinking level",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, err := BuildArgs(tt.agent, tt.model, tt.effort, tt.spec)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			joined := " " + strings.Join(args, " ") + " "
			for _, run := range tt.wantContain {
				assert.Contains(t, joined, " "+strings.Join(run, " ")+" ", "args: %v", args)
			}
			for _, absent := range tt.wantAbsent {
				assert.NotContains(t, args, absent, "args: %v", args)
			}
			if tt.spec.Prompt != "" {
				assert.Equal(t, tt.spec.Prompt, args[len(args)-1])
			}
		})
	}
}

func TestStoreRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())

	thread := Thread{
		ID: NewID(), Title: "what is running", CreatedAt: time.Now().UTC().Truncate(time.Second),
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
		Agent:     "cl", Kind: config.AgentKindClaude, Cwd: "/tickets", Autonomy: config.AutonomyAsk,
	}
	require.NoError(t, store.Save(thread))

	got, err := store.Load(thread.ID)
	require.NoError(t, err)
	assert.Equal(t, thread, got)

	require.NoError(t, store.AppendTurn(thread.ID, Turn{N: 1, Text: "hi"}))
	require.NoError(t, store.AppendTurn(thread.ID, Turn{N: 2, Text: "again", ExitCode: 1}))
	turns, err := store.Turns(thread.ID)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	assert.Equal(t, "again", turns[1].Text)
	assert.Equal(t, 1, turns[1].ExitCode)

	// A newer thread sorts first, and a directory with no thread.json is
	// skipped rather than failing the listing.
	older := Thread{ID: NewID(), UpdatedAt: thread.UpdatedAt.Add(-time.Hour)}
	require.NoError(t, store.Save(older))
	require.NoError(t, os.MkdirAll(filepath.Join(store.Root(), "junkdir"), 0o755))
	list, err := store.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, thread.ID, list[0].ID)

	require.NoError(t, store.Delete(thread.ID))
	_, err = store.Load(thread.ID)
	assert.ErrorIs(t, err, ErrThreadNotFound)
	assert.ErrorIs(t, store.Delete(thread.ID), ErrThreadNotFound)
}

func TestStoreRejectsUnsafeID(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, id := range []string{"../escape", "a/b", "", "UPPER", strings.Repeat("a", 33)} {
		_, err := store.Dir(id)
		assert.ErrorIs(t, err, ErrThreadNotFound, "id %q", id)
	}
}

func TestListEmptyRoot(t *testing.T) {
	list, err := NewStore(filepath.Join(t.TempDir(), "nothing-here")).List()
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestTitle(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "New chat"},
		{"  \n ", "New chat"},
		{"move kon-7d21 to done", "move kon-7d21 to done"},
		{"first line\nsecond line", "first line"},
		{strings.Repeat("x", 60), strings.Repeat("x", 47) + "…"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, Title(tt.in))
	}
}

func TestSliceTapeCursor(t *testing.T) {
	// A tool row with no result yet cannot be frozen, so the cursor stops there
	// however far past it the client asks for.
	tape := logfmt.Tape{Events: []logfmt.Event{
		{Kind: "text", Text: "a"},
		{Kind: "text", Text: "b"},
		{Kind: "tool", Tool: "Bash"},
		{Kind: "text", Text: "c"},
	}}
	require.Equal(t, 2, tape.StableCount())

	tests := []struct {
		name       string
		after      int
		wantOffset int
		wantFirst  string
	}{
		{name: "a fresh reader takes the whole tape", after: 0, wantOffset: 0, wantFirst: "a"},
		{name: "a cursor inside the stable prefix is honoured", after: 1, wantOffset: 1, wantFirst: "b"},
		{name: "a cursor past the stable count is pulled back", after: 99, wantOffset: 2, wantFirst: ""},
		{name: "a negative cursor reads as zero", after: -5, wantOffset: 0, wantFirst: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cut := tape
			cut.Events = append([]logfmt.Event(nil), tape.Events...)
			off := cut.SliceAt(tt.after)
			assert.Equal(t, tt.wantOffset, off)
			require.NotEmpty(t, cut.Events)
			assert.Equal(t, tt.wantFirst, cut.Events[0].Text)
		})
	}
}

func TestSessionPath(t *testing.T) {
	dir := t.TempDir()

	// claude: keyed by session id under its own config dir.
	claudeCfg := filepath.Join(dir, "claude")
	projects := filepath.Join(claudeCfg, "projects", "-tickets")
	require.NoError(t, os.MkdirAll(projects, 0o755))
	claudeFile := filepath.Join(projects, "abc.jsonl")
	require.NoError(t, os.WriteFile(claudeFile, []byte("{}\n"), 0o644))

	assert.Equal(t, claudeFile, SessionPath(config.AgentKindClaude, "abc", claudeCfg, ""))
	assert.Empty(t, SessionPath(config.AgentKindClaude, "missing", claudeCfg, ""))
	assert.Empty(t, SessionPath(config.AgentKindClaude, "", claudeCfg, ""))

	// pi: the newest file in the session directory it was given.
	piDir := filepath.Join(dir, "pi")
	require.NoError(t, os.MkdirAll(piDir, 0o755))
	assert.Empty(t, SessionPath(config.AgentKindPi, "s", "", piDir), "an empty directory has no transcript")
	old := filepath.Join(piDir, "old.jsonl")
	newer := filepath.Join(piDir, "new.jsonl")
	require.NoError(t, os.WriteFile(old, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(newer, []byte("{}\n"), 0o644))
	require.NoError(t, os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	assert.Equal(t, newer, SessionPath(config.AgentKindPi, "s", "", piDir))

	assert.Empty(t, SessionPath("aider", "s", claudeCfg, piDir))
}

func TestGate(t *testing.T) {
	t.Run("approve releases the call", func(t *testing.T) {
		g := NewGate(time.Minute)
		id, done := g.Park(Call{ThreadID: "t1", Tool: "Bash", Kind: DecisionWrite})

		pending, ok := g.Pending("t1")
		require.True(t, ok)
		assert.Equal(t, id, pending.ID)

		assert.True(t, g.Resolve(id, true))
		assert.True(t, <-done)

		_, ok = g.Pending("t1")
		assert.False(t, ok, "an answered call is no longer pending")
		assert.False(t, g.Resolve(id, true), "a second answer names nothing")
	})

	t.Run("skip refuses it", func(t *testing.T) {
		g := NewGate(time.Minute)
		id, done := g.Park(Call{ThreadID: "t1"})
		g.Resolve(id, false)
		assert.False(t, <-done)
	})

	t.Run("a timeout denies", func(t *testing.T) {
		g := NewGate(time.Millisecond)
		_, done := g.Park(Call{ThreadID: "t1"})
		select {
		case approved := <-done:
			assert.False(t, approved)
		case <-time.After(2 * time.Second):
			t.Fatal("the parked call was never answered")
		}
	})

	t.Run("clearing a thread refuses what it left parked", func(t *testing.T) {
		g := NewGate(time.Minute)
		_, a := g.Park(Call{ThreadID: "t1"})
		_, b := g.Park(Call{ThreadID: "t2"})
		g.Clear("t1")
		assert.False(t, <-a)
		_, ok := g.Pending("t2")
		assert.True(t, ok, "another thread's call is untouched")
		g.Clear("t2")
		assert.False(t, <-b)
	})
}

func TestSystemPrompt(t *testing.T) {
	base := PromptData{Autonomy: config.AutonomyRead, Cwd: "/tickets", TicketsDir: "/tickets", LogsDir: "/logs", WorktreesDir: "/wt"}
	board := []string{"Pipelines: default (plan -> code -> review)", "Agents: claude (default), pi"}
	page := []string{"Open ticket: kon-12 (in_progress, stage review)"}

	tests := []struct {
		name     string
		override string
		data     func(PromptData) PromptData
		want     []string
		notWant  []string
	}{
		{
			name: "the built-in brief",
			data: func(d PromptData) PromptData { return d },
			want: []string{"READ-ONLY", "/tickets", "kontora view", "kontora skills list"},
		},
		{
			name:    "auto mode",
			data:    func(d PromptData) PromptData { d.Autonomy = config.AutonomyAuto; return d },
			want:    []string{"AUTO mode"},
			notWant: []string{"READ-ONLY"},
		},
		{
			name: "an unknown mode falls back to the careful one",
			data: func(d PromptData) PromptData { d.Autonomy = "nonsense"; return d },
			want: []string{"ASK mode"},
		},
		{
			name: "a configured board",
			data: func(d PromptData) PromptData { d.Board = board; return d },
			want: []string{"Board:", "default (plan -> code -> review)", "Agents: claude (default), pi"},
		},
		{
			name:    "an empty board omits the section",
			data:    func(d PromptData) PromptData { return d },
			notWant: []string{"Board:", "Now:", "Current page:"},
		},
		{
			name: "the counts render on one line",
			data: func(d PromptData) PromptData {
				d.Counts = []string{"3 todo", "1 in_progress", "41 done"}
				return d
			},
			want: []string{"Now: 3 todo, 1 in_progress, 41 done"},
		},
		{
			name:    "no tickets omits the counts",
			data:    func(d PromptData) PromptData { d.Board = board; return d },
			want:    []string{"Board:"},
			notWant: []string{"Now:"},
		},
		{
			name: "the page the user is on",
			data: func(d PromptData) PromptData { d.PageContext = page; return d },
			want: []string{"Current page:", "Open ticket: kon-12 (in_progress, stage review)"},
		},
		{
			name:     "an override replaces the whole brief, page context included",
			override: "my own brief",
			data: func(d PromptData) PromptData {
				d.Board, d.Counts, d.PageContext = board, []string{"3 todo"}, page
				return d
			},
			want:    []string{"my own brief"},
			notWant: []string{"READ-ONLY", "Board:", "Current page:", "kontora view"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SystemPrompt(tt.override, tt.data(base))
			for _, want := range tt.want {
				assert.Contains(t, got, want)
			}
			for _, notWant := range tt.notWant {
				assert.NotContains(t, got, notWant)
			}
		})
	}
}

func cmd(command string) map[string]any { return map[string]any{"command": command} }
