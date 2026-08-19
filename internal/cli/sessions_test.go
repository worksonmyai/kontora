package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
)

const (
	sessionsUUID       = "2f1e0c7a-1111-2222-3333-444455556666"
	sessionsReviewUUID = "9a2c1b3d-5555-6666-7777-888899990000"
)

// sessionsFixture is a ticket whose logs dir and Claude config dir are both
// under the test's control, so every path the renderer prints is one the test
// planted.
type sessionsFixture struct {
	t         *testing.T
	cfg       *config.Config
	logsDir   string
	claudeDir string
}

func newSessionsFixture(t *testing.T, history string) *sessionsFixture {
	t.Helper()
	dir := t.TempDir()
	f := &sessionsFixture{t: t, cfg: testConfig(dir), logsDir: t.TempDir(), claudeDir: t.TempDir()}
	f.cfg.LogsDir = f.logsDir
	f.cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": f.claudeDir}
	f.cfg.Agents["pi-agent"] = config.Agent{Binary: "pi"}
	f.cfg.Agents["shell-runner"] = config.Agent{Binary: "make"}

	writeTicket(t, dir, "tst-s01.md", `---
id: tst-s01
kontora: true
status: done
pipeline: default
path: /tmp/testrepo
`+history+`---
# Sessions fixture
`)
	return f
}

func (f *sessionsFixture) write(path, content string) string {
	f.t.Helper()
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(f.t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func (f *sessionsFixture) claude(uuid string) string {
	return f.write(filepath.Join(f.claudeDir, "projects", "-wt", uuid+".jsonl"), "{}\n")
}

func (f *sessionsFixture) pi(stage, name string) string {
	return f.write(filepath.Join(session.PiDir(f.logsDir, "tst-s01", stage), name), "{}\n")
}

func (f *sessionsFixture) record(stage, uuid string) {
	f.t.Helper()
	data, err := json.Marshal(session.Record{SessionID: uuid, Stage: stage, Agent: session.KindClaude})
	require.NoError(f.t, err)
	f.write(session.CompletedRecordPath(f.logsDir, "tst-s01", stage), string(data))
}

func (f *sessionsFixture) run(opts SessionsOptions) []string {
	f.t.Helper()
	var buf bytes.Buffer
	require.NoError(f.t, Sessions(f.cfg, "tst-s01", opts, &buf))
	out := strings.TrimRight(buf.String(), "\n")
	if out == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	return lines
}

// twoStageHistory is one claude run of code, two of review, the second of which
// resumed the first.
const twoStageHistory = `history:
  - stage: code
    agent: claude-sonnet
    exit_code: 0
    run: 0
    session_kind: claude
    session_ref: ` + sessionsUUID + `
  - stage: review
    agent: claude-sonnet
    exit_code: 0
    run: 0
    session_kind: claude
    session_ref: ` + sessionsReviewUUID + `
  - stage: review
    agent: claude-sonnet
    exit_code: 1
    run: 1
    session_kind: claude
    session_ref: ` + sessionsReviewUUID + `
`

func TestSessionsArtifactSelection(t *testing.T) {
	tests := []struct {
		name string
		opts SessionsOptions
		want []string
	}{
		{
			name: "no artifact flag prints sessions only",
			want: []string{
				"code 0 session CODESESSION",
				"review 0 session REVIEWSESSION",
				"review 1 session REVIEWSESSION resumed: same file as run 0",
			},
		},
		{
			name: "logs only",
			opts: SessionsOptions{Logs: true},
			want: []string{
				"code - log CODELOG",
				"review - log REVIEWLOG",
			},
		},
		{
			name: "logs and events, and no session row",
			opts: SessionsOptions{Logs: true, Events: true},
			want: []string{
				"code - log CODELOG",
				"code 0 events CODEEVENTS",
				"review - log REVIEWLOG",
				"review 0 events - activity sidecar missing: " +
					"REVIEWEVENTS0",
				"review 1 events - activity sidecar missing: " +
					"REVIEWEVENTS1",
			},
		},
		{
			name: "all",
			opts: SessionsOptions{All: true},
			want: []string{
				"code - log CODELOG",
				"code 0 session CODESESSION",
				"code 0 events CODEEVENTS",
				"review - log REVIEWLOG",
				"review 0 session REVIEWSESSION",
				"review 0 events - activity sidecar missing: REVIEWEVENTS0",
				"review 1 session REVIEWSESSION resumed: same file as run 0",
				"review 1 events - activity sidecar missing: REVIEWEVENTS1",
			},
		},
		{
			name: "one stage",
			opts: SessionsOptions{Stage: "review"},
			want: []string{
				"review 0 session REVIEWSESSION",
				"review 1 session REVIEWSESSION resumed: same file as run 0",
			},
		},
		{
			name: "one run",
			opts: SessionsOptions{Run: new(1)},
			want: []string{"review 1 session REVIEWSESSION resumed: same file as run 0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSessionsFixture(t, twoStageHistory)
			expand := strings.NewReplacer(
				"CODESESSION", f.claude(sessionsUUID),
				"REVIEWSESSION", f.claude(sessionsReviewUUID),
				"CODELOG", f.write(session.LogPath(f.logsDir, "tst-s01", "code"), "out\n"),
				"REVIEWLOG", f.write(session.LogPath(f.logsDir, "tst-s01", "review"), "out\n"),
				"CODEEVENTS", f.write(session.EventsPath(f.logsDir, "tst-s01", "code", 0), "{}"),
				"REVIEWEVENTS0", session.EventsPath(f.logsDir, "tst-s01", "review", 0),
				"REVIEWEVENTS1", session.EventsPath(f.logsDir, "tst-s01", "review", 1),
			)
			want := make([]string, len(tt.want))
			for i, line := range tt.want {
				want[i] = expand.Replace(line)
			}
			assert.Equal(t, want, f.run(tt.opts))
		})
	}
}

// TestSessionsPrintsEveryReason is the regression the design exists to prevent.
// A ticket whose every run is unresolvable must still print one row per run,
// each saying why, rather than a shorter list that reads as fewer runs.
func TestSessionsPrintsEveryReason(t *testing.T) {
	f := newSessionsFixture(t, `history:
  - stage: code
    agent: claude-sonnet
    exit_code: 0
    run: 0
    session_kind: claude
    session_ref: `+sessionsUUID+`
  - stage: review
    agent: claude-sonnet
    exit_code: 0
    run: 0
  - stage: review
    agent: claude-sonnet
    exit_code: 0
    run: 1
    session_kind: pi
    session_ref: ../../../etc/passwd
`)

	got := f.run(SessionsOptions{})
	require.Len(t, got, 3, "one row per run, whatever happened to the files")
	assert.Contains(t, got[0], "code 0 session - session file missing")
	assert.Contains(t, got[1], "review 0 session - no session recorded (run predates session_ref)")
	assert.Contains(t, got[2], `review 1 session - session reference refused: "../../../etc/passwd"`)
	for _, line := range got {
		assert.NotContains(t, line, "/etc/passwd\t", "a refused reference must never yield a path")
	}
}

// TestSessionsRecoversPreSchemaRuns covers the fallback for a ticket written
// before the reference fields existed: a Claude stage's last run comes back
// from its session record, its earlier runs do not, and a pi stage's files are
// listed without being attributed to any run.
func TestSessionsRecoversPreSchemaRuns(t *testing.T) {
	f := newSessionsFixture(t, `history:
  - stage: code
    agent: claude-sonnet
    exit_code: 1
    run: 0
  - stage: code
    agent: claude-sonnet
    exit_code: 0
    run: 1
  - stage: review
    agent: pi-agent
    exit_code: 0
    run: 0
`)
	f.cfg.Pipelines["default"] = config.Pipeline{
		{Stage: "code", Agent: "claude-sonnet", OnSuccess: "next", OnFailure: "retry"},
		{Stage: "review", Agent: "pi-agent", OnSuccess: "done", OnFailure: "pause"},
	}
	wantSession := f.claude(sessionsUUID)
	f.record("code", sessionsUUID)
	wantPi := f.pi("review", "01JC9.jsonl")

	got := f.run(SessionsOptions{})
	require.Len(t, got, 4, "three runs plus the pi file no run claims")
	assert.Equal(t, "code 0 session - no session recorded (run predates session_ref)", got[0])
	assert.Equal(t, "code 1 session "+wantSession+" recovered from the stage's session record", got[1])
	assert.Equal(t, "review 0 session - no session recorded (run predates session_ref)", got[2])
	assert.Equal(t, "review ? session "+wantPi+" discovered in pi-sessions/review; not attributable to a run", got[3])
}

func TestSessionsNoRuns(t *testing.T) {
	f := newSessionsFixture(t, "")
	assert.Equal(t, []string{"tst-s01: no runs recorded"}, f.run(SessionsOptions{}))
}

func TestSessionsNoMatch(t *testing.T) {
	f := newSessionsFixture(t, twoStageHistory)
	assert.Equal(t, []string{"tst-s01: nothing matches the given filters"},
		f.run(SessionsOptions{Stage: "nope"}))
}

// TestSessionsAgentWithNoSession covers an agent whose CLI Kontora does not
// know the flags of: it writes no session anyone can point at, and the row says
// so instead of leaving the reader to guess the file went missing.
func TestSessionsAgentWithNoSession(t *testing.T) {
	f := newSessionsFixture(t, `history:
  - stage: ship
    agent: shell-runner
    exit_code: 0
    run: 0
`)
	assert.Equal(t, []string{`ship 0 session - agent "shell-runner" writes no session`},
		f.run(SessionsOptions{}))
}

// TestSessionsSweepsWithoutHistory covers the two cases that leave a ticket
// with files but no history row: a pipeline-less ticket, which appends no row
// at all, and a first stage whose agent has not exited yet.
func TestSessionsSweepsWithoutHistory(t *testing.T) {
	f := newSessionsFixture(t, "")
	wantSession := f.claude(sessionsUUID)
	data, err := json.Marshal(session.Record{SessionID: sessionsUUID, Stage: "code", Agent: session.KindClaude})
	require.NoError(t, err)
	f.write(session.RecordPath(f.logsDir, "tst-s01", "code"), string(data))
	wantPi := f.pi("implement", "01JC9.jsonl")

	assert.Equal(t, []string{
		"code ? session " + wantSession + " running now, or interrupted",
		"implement ? session " + wantPi + " discovered in pi-sessions/implement; not attributable to a run",
	}, f.run(SessionsOptions{}))
}

// TestSessionsUsesTheRecordedAgent covers a stage that was repointed at another
// agent after the run: the row's own agent decides which runtime wrote the
// files, so the pre-schema recovery keeps working.
func TestSessionsUsesTheRecordedAgent(t *testing.T) {
	f := newSessionsFixture(t, `history:
  - stage: code
    agent: claude-sonnet
    exit_code: 0
    run: 0
`)
	f.cfg.Pipelines["default"] = config.Pipeline{
		{Stage: "code", Agent: "pi-agent", OnSuccess: "done", OnFailure: "pause"},
	}
	wantSession := f.claude(sessionsUUID)
	f.record("code", sessionsUUID)

	assert.Equal(t,
		[]string{"code 0 session " + wantSession + " recovered from the stage's session record"},
		f.run(SessionsOptions{}))
}
