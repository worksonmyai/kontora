package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testUUID = "2f1e0c7a-1111-2222-3333-444455556666"

// fixture is one ticket's log directory plus a Claude config dir, with helpers
// that plant the files the resolver looks for.
type fixture struct {
	t *testing.T
	l Layout
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, l: Layout{
		LogsDir:         t.TempDir(),
		TicketID:        "kon-1",
		ClaudeConfigDir: t.TempDir(),
	}}
}

func (f *fixture) write(path, content string) string {
	f.t.Helper()
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(f.t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func (f *fixture) claude(uuid string) string {
	return f.write(filepath.Join(f.l.ClaudeConfigDir, "projects", "-wt-kon-1", uuid+".jsonl"), "{}\n")
}

func (f *fixture) pi(stage, name string) string {
	return f.write(filepath.Join(PiDir(f.l.LogsDir, f.l.TicketID, stage), name), "{}\n")
}

func (f *fixture) record(path, uuid string) {
	f.t.Helper()
	data, err := json.Marshal(Record{SessionID: uuid, Stage: "code", Agent: KindClaude, StartedAt: time.Now()})
	require.NoError(f.t, err)
	f.write(path, string(data))
}

// paths returns the emitted rows as "stage/run/artifact" keys mapped to the
// path, so an assertion reads as the table it renders.
func rows(files []File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Stage+" "+f.Artifact)
	}
	return out
}

func TestRefResolve(t *testing.T) {
	f := newFixture(t)
	want := f.claude(testUUID)
	piPath := f.pi("code", "01JC9.jsonl")

	tests := []struct {
		name       string
		ref        Ref
		wantPath   string
		wantReason string
	}{
		{
			name:     "claude hit",
			ref:      Ref{KindClaude, testUUID},
			wantPath: want,
		},
		{
			name:       "claude miss",
			ref:        Ref{KindClaude, "00000000-0000-4000-8000-000000000000"},
			wantReason: "session file missing: no ",
		},
		{
			name:       "claude ref with a separator is refused",
			ref:        Ref{KindClaude, "projects/" + testUUID},
			wantReason: "session reference refused:",
		},
		{
			name:     "pi hit",
			ref:      Ref{KindPi, "pi-sessions/code/01JC9.jsonl"},
			wantPath: piPath,
		},
		{
			name:       "pi miss",
			ref:        Ref{KindPi, "pi-sessions/code/gone.jsonl"},
			wantReason: "session file missing:",
		},
		{
			name:       "pi ref escaping the ticket directory is refused",
			ref:        Ref{KindPi, "../../../etc/passwd"},
			wantReason: "session reference refused:",
		},
		{
			name:       "unknown runtime",
			ref:        Ref{"codex", "whatever"},
			wantReason: "session reference refused:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, reason := tt.ref.Resolve(f.l)
			assert.Equal(t, tt.wantPath, path)
			if tt.wantReason == "" {
				assert.Empty(t, reason)
			} else {
				assert.Contains(t, reason, tt.wantReason)
				assert.Empty(t, path, "an unresolved reference must carry no path")
			}
		})
	}
}

// TestRunsClassification walks every branch the reader can take on one history
// row. Each case asserts the single session row the run produces.
func TestRunsClassification(t *testing.T) {
	tests := []struct {
		name string
		// plant sets up the ticket directory and returns the row to read.
		plant      func(f *fixture) Run
		wantPath   func(f *fixture) string
		wantNote   string
		wantExtras []string // stage names of the unattributed rows that follow
	}{
		{
			name: "claude row resolves",
			plant: func(f *fixture) Run {
				f.claude(testUUID)
				return Run{Stage: "code", Agent: "a", Kind: KindClaude, Ref: Ref{KindClaude, testUUID}}
			},
			wantPath: func(f *fixture) string { return f.claude(testUUID) },
		},
		{
			name: "claude row whose file is gone",
			plant: func(*fixture) Run {
				return Run{Stage: "code", Agent: "a", Kind: KindClaude, Ref: Ref{KindClaude, testUUID}}
			},
			wantNote: "session file missing",
		},
		{
			name: "pi row resolves",
			plant: func(f *fixture) Run {
				f.pi("code", "01JC9.jsonl")
				return Run{Stage: "code", Agent: "a", Kind: KindPi, Ref: Ref{KindPi, "pi-sessions/code/01JC9.jsonl"}}
			},
			wantPath: func(f *fixture) string { return f.pi("code", "01JC9.jsonl") },
		},
		{
			name: "pi row whose reference escapes the ticket directory",
			plant: func(*fixture) Run {
				return Run{Stage: "code", Agent: "a", Kind: KindPi, Ref: Ref{KindPi, "../../../etc/passwd"}}
			},
			wantNote: "session reference refused",
		},
		{
			name: "refless pi row falls back to discovery",
			plant: func(f *fixture) Run {
				f.pi("code", "01JC9.jsonl")
				return Run{Stage: "code", Agent: "a", Kind: KindPi}
			},
			wantNote:   noSessionRecorded,
			wantExtras: []string{"code"},
		},
		{
			name: "refless claude row recovers from the completed record",
			plant: func(f *fixture) Run {
				f.claude(testUUID)
				f.record(CompletedRecordPath(f.l.LogsDir, f.l.TicketID, "code"), testUUID)
				return Run{Stage: "code", Agent: "a", Kind: KindClaude}
			},
			wantPath: func(f *fixture) string { return f.claude(testUUID) },
			wantNote: "recovered from the stage's session record",
		},
		{
			name: "refless claude row with no record at all",
			plant: func(*fixture) Run {
				return Run{Stage: "code", Agent: "a", Kind: KindClaude}
			},
			wantNote: noSessionRecorded,
		},
		{
			name: "an agent Kontora cannot locate a session for",
			plant: func(*fixture) Run {
				return Run{Stage: "code", Agent: "shell-runner"}
			},
			wantNote: `agent "shell-runner" writes no session`,
		},
		{
			name: "a live record no row claims is reported separately",
			plant: func(f *fixture) Run {
				f.claude(testUUID)
				f.record(RecordPath(f.l.LogsDir, f.l.TicketID, "code"), testUUID)
				return Run{Stage: "code", Agent: "shell-runner"}
			},
			wantNote:   `agent "shell-runner" writes no session`,
			wantExtras: []string{"code"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			run := tt.plant(f)

			got := Runs(f.l, Artifacts{Sessions: true}, []Run{run})
			require.NotEmpty(t, got)
			assert.Equal(t, "code", got[0].Stage)
			assert.Equal(t, 0, got[0].Run)
			assert.Equal(t, ArtifactSession, got[0].Artifact)

			if tt.wantPath == nil {
				assert.Empty(t, got[0].Path)
			} else {
				assert.Equal(t, tt.wantPath(f), got[0].Path)
			}
			if tt.wantNote == "" {
				assert.Empty(t, got[0].Note)
			} else {
				assert.Contains(t, got[0].Note, tt.wantNote)
			}

			extras := got[1:]
			require.Len(t, extras, len(tt.wantExtras))
			for i, stage := range tt.wantExtras {
				assert.Equal(t, stage, extras[i].Stage)
				assert.Equal(t, UnknownRun, extras[i].Run)
				assert.NotEmpty(t, extras[i].Note)
			}
		})
	}
}

// TestRunsEveryRowIsReported is the rule the whole design exists for: a ticket
// whose runs are all unresolvable still prints one row per run.
func TestRunsEveryRowIsReported(t *testing.T) {
	f := newFixture(t)
	runs := []Run{
		{Stage: "plan", Index: 0, Agent: "a", Kind: KindClaude},
		{Stage: "code", Index: 0, Agent: "a", Kind: KindClaude, Ref: Ref{KindClaude, testUUID}},
		{Stage: "code", Index: 1, Agent: "a", Kind: KindPi},
		{Stage: "ship", Index: 0, Agent: "shell-runner"},
	}

	got := Runs(f.l, Artifacts{Sessions: true}, runs)
	require.Len(t, got, len(runs))
	for i, file := range got {
		assert.Equal(t, runs[i].Stage, file.Stage, "row %d", i)
		assert.Equal(t, runs[i].Index, file.Run, "row %d", i)
		assert.Empty(t, file.Path, "row %d", i)
		assert.NotEmpty(t, file.Note, "row %d must say why it has no path", i)
	}
}

// TestRunsReportsTheRunningStage covers the stage in flight. It has no history
// row until its agent exits, so its record is only reachable by sweeping the
// ticket's log directory, and it is the transcript a reader most often wants.
func TestRunsReportsTheRunningStage(t *testing.T) {
	f := newFixture(t)
	want := f.claude(testUUID)
	f.record(RecordPath(f.l.LogsDir, f.l.TicketID, "review"), testUUID)
	piNow := f.pi("review", "01JC9.jsonl")

	got := Runs(f.l, Artifacts{Sessions: true}, []Run{{Stage: "code", Agent: "a", Kind: KindClaude}})
	require.Len(t, got, 3)
	assert.Equal(t, "code", got[0].Stage)

	assert.Equal(t, File{
		Stage: "review", Run: UnknownRun, Artifact: ArtifactSession, Path: piNow,
		Note: "discovered in pi-sessions/review; not attributable to a run",
	}, got[1])
	assert.Equal(t, File{
		Stage: "review", Run: UnknownRun, Artifact: ArtifactSession, Path: want,
		Note: "running now, or interrupted",
	}, got[2])
}

// TestRunsSharedFile covers a resumed run: it continues the earlier
// conversation and appends to that run's file, so one file backs two rows.
func TestRunsSharedFile(t *testing.T) {
	f := newFixture(t)
	want := f.claude(testUUID)
	runs := []Run{
		{Stage: "code", Index: 0, Kind: KindClaude, Ref: Ref{KindClaude, testUUID}},
		{Stage: "code", Index: 1, Kind: KindClaude, Ref: Ref{KindClaude, testUUID}},
	}

	got := Runs(f.l, Artifacts{Sessions: true}, runs)
	require.Len(t, got, 2)
	assert.Equal(t, want, got[0].Path)
	assert.Empty(t, got[0].Note)
	assert.Equal(t, want, got[1].Path)
	assert.Equal(t, "resumed: same file as run 0", got[1].Note)
}

// TestRunsArtifactSelection covers the two Kontora artifacts. The stage log is
// per stage: a stage run twice yields one log row, and it carries no run.
func TestRunsArtifactSelection(t *testing.T) {
	f := newFixture(t)
	logPath := f.write(LogPath(f.l.LogsDir, f.l.TicketID, "code"), "output\n")
	events0 := f.write(EventsPath(f.l.LogsDir, f.l.TicketID, "code", 0), "{}")
	runs := []Run{
		{Stage: "code", Index: 0, Kind: KindClaude},
		{Stage: "code", Index: 1, Kind: KindClaude},
	}

	t.Run("logs only", func(t *testing.T) {
		got := Runs(f.l, Artifacts{Logs: true}, runs)
		require.Len(t, got, 1)
		assert.Equal(t, ArtifactLog, got[0].Artifact)
		assert.Equal(t, UnknownRun, got[0].Run)
		assert.Equal(t, logPath, got[0].Path)
	})

	t.Run("logs and events", func(t *testing.T) {
		got := Runs(f.l, Artifacts{Logs: true, Events: true}, runs)
		assert.Equal(t, []string{"code log", "code events", "code events"}, rows(got))
		assert.Equal(t, events0, got[1].Path)
		assert.Empty(t, got[2].Path, "run 1 wrote no sidecar")
		assert.Contains(t, got[2].Note, "activity sidecar missing")
	})

	t.Run("empty history", func(t *testing.T) {
		assert.Empty(t, Runs(f.l, Artifacts{Sessions: true, Logs: true, Events: true}, nil))
	})
}

// TestRunsRefusesAnUnsafeStage keeps a hand-edited stage name from reaching the
// filesystem: every artifact of the row is reported, and none of them names a
// path outside the ticket's log directory.
func TestRunsRefusesAnUnsafeStage(t *testing.T) {
	f := newFixture(t)
	got := Runs(f.l, Artifacts{Sessions: true, Logs: true, Events: true},
		[]Run{{Stage: "../../etc", Agent: "shell-runner"}})

	require.Len(t, got, 3)
	for _, file := range got {
		assert.Empty(t, file.Path, file.Artifact)
		assert.NotEmpty(t, file.Note, file.Artifact)
	}
}
