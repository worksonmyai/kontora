package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

const (
	resumeTestSessionID = "a1b2c3d4-0000-4000-8000-000000000001"
	resumeTicketID      = "kon-123"
)

// writeRecordFile plants rec under fileStage's record path. The two stage names
// are separate arguments because the record's own stage field is what the
// wrong-stage cases change.
func writeRecordFile(t *testing.T, cfg *config.Config, ticketID, fileStage string, rec resumeRecord) {
	t.Helper()
	path := resumeRecordPath(cfg, ticketID, fileStage)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

// plantClaudeSession creates the JSONL that claudeSessionFiles globs for.
func plantClaudeSession(t *testing.T, claudeDir, sessionID string) {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", "-worktree-"+resumeTicketID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte("{}\n"), 0o644))
}

// plantPiSession creates a stage session file with the given modification time.
func plantPiSession(t *testing.T, cfg *config.Config, ticketID, stage, name string, modTime time.Time) string {
	t.Helper()
	dir := piSessionDir(cfg, ticketID, stage)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	require.NoError(t, os.Chtimes(path, modTime, modTime))
	return path
}

// resumeFixture drives resumableRecord directly, with the Claude session
// directory, pi session directory, and tmux window list all under the test's
// control.
type resumeFixture struct {
	h         *testHarness
	d         *Daemon
	p         spawnAgentParams
	claudeDir string
}

func newResumeFixture(t *testing.T, agentBinary string) *resumeFixture {
	t.Helper()
	h := newHarness(t)
	claudeDir := t.TempDir()
	cfg := h.defaultConfig(agentBinary, "true")
	cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeDir}

	d := h.newDaemon(cfg)
	d.windows = windowOps{
		list: func(string) ([]string, error) { return nil, nil },
		kill: func(string, string) error { return nil },
	}

	return &resumeFixture{
		h:         h,
		d:         d,
		claudeDir: claudeDir,
		p: spawnAgentParams{
			cfg:       cfg,
			ctx:       context.Background(),
			log:       testLogger(t),
			ticketID:  resumeTicketID,
			stageName: "code",
			wtPath:    filepath.Join(h.wtDir, resumeTicketID),
			agentCfg:  cfg.Agents["agent1"],
		},
	}
}

// validRecord passes every guard once its session file exists.
func (f *resumeFixture) validRecord(kind string) resumeRecord {
	rec := resumeRecord{
		Stage:     f.p.stageName,
		Agent:     kind,
		Worktree:  f.p.wtPath,
		Instance:  f.d.instanceName,
		StartedAt: time.Now().Add(-time.Minute),
	}
	if kind == agentKindClaude {
		rec.SessionID = resumeTestSessionID
	}
	return rec
}

func TestResumeRecordReadWriteRemove(t *testing.T) {
	f := newResumeFixture(t, "claude")

	assert.Nil(t, f.d.readResumeRecord(f.p), "no record yet")

	f.d.writeResumeRecord(f.p, agentKindClaude, resumeTestSessionID)

	got := f.d.readResumeRecord(f.p)
	require.NotNil(t, got)
	assert.Equal(t, resumeTestSessionID, got.SessionID)
	assert.Equal(t, "code", got.Stage)
	assert.Equal(t, agentKindClaude, got.Agent)
	assert.Equal(t, f.p.wtPath, got.Worktree)
	assert.Equal(t, "test-instance", got.Instance)
	assert.False(t, got.StartedAt.IsZero(), "start time recorded")

	assert.Equal(t,
		filepath.Join(f.h.logsDir, resumeTicketID, "code.session"),
		resumeRecordPath(f.p.cfg, resumeTicketID, "code"))

	f.d.removeResumeRecord(f.p.cfg, resumeTicketID, "code")
	assert.Nil(t, f.d.readResumeRecord(f.p), "record removed")

	// Removing a record that is already gone is not an error.
	f.d.removeResumeRecord(f.p.cfg, resumeTicketID, "code")
}

func TestResumeRecordMalformedReadsAsAbsent(t *testing.T) {
	f := newResumeFixture(t, "claude")
	path := resumeRecordPath(f.p.cfg, f.p.ticketID, f.p.stageName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	assert.Nil(t, f.d.readResumeRecord(f.p), "malformed record reads as absent")
	assert.Nil(t, f.d.resumableRecord(f.p), "malformed record does not resume")
}

func TestResumeAgentKind(t *testing.T) {
	cases := []struct {
		name  string
		agent config.Agent
		want  string
	}{
		{name: "claude resumes by default", agent: config.Agent{Binary: "claude"}, want: agentKindClaude},
		{name: "pi resumes by default", agent: config.Agent{Binary: "pi"}, want: agentKindPi},
		{name: "unknown agent never resumes", agent: config.Agent{Binary: "aider"}, want: ""},
		{name: "claude with resume true", agent: config.Agent{Binary: "claude", Resume: new(true)}, want: agentKindClaude},
		{name: "claude with resume false", agent: config.Agent{Binary: "claude", Resume: new(false)}, want: ""},
		{name: "pi with resume false", agent: config.Agent{Binary: "pi", Resume: new(false)}, want: ""},
		{name: "resume true does not enable an unknown agent", agent: config.Agent{Binary: "aider", Resume: new(true)}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resumeAgentKind(tc.agent))
		})
	}
}

func TestResumableRecordGuards(t *testing.T) {
	cases := []struct {
		name string
		// binary selects the agent the stage is about to run.
		binary string
		// resume is the agent's configured resume setting.
		resume *bool
		// mutate adjusts an otherwise valid record to break one guard.
		mutate func(*resumeRecord)
		// noRecord skips writing the record entirely.
		noRecord bool
		// noSessionFile skips planting the agent's session file.
		noSessionFile bool
		// windows is what the tmux window list reports.
		windows    []string
		wantResume bool
	}{
		{name: "claude record passes every guard", binary: "claude", wantResume: true},
		{name: "pi record passes every guard", binary: "pi", wantResume: true},
		{name: "no record", binary: "claude", noRecord: true},
		{
			name:   "record names another stage",
			binary: "claude",
			mutate: func(r *resumeRecord) { r.Stage = "plan" },
		},
		{
			name:   "record names another agent",
			binary: "pi",
			mutate: func(r *resumeRecord) { r.Agent = agentKindClaude },
		},
		{
			name:   "record names another daemon instance",
			binary: "claude",
			mutate: func(r *resumeRecord) { r.Instance = "desktop" },
		},
		{
			name:   "record names another worktree",
			binary: "claude",
			mutate: func(r *resumeRecord) { r.Worktree += "-new" },
		},
		{name: "claude session file is missing", binary: "claude", noSessionFile: true},
		{
			name:   "claude record carries no session ID",
			binary: "claude",
			mutate: func(r *resumeRecord) { r.SessionID = "" },
		},
		{name: "pi session file is missing", binary: "pi", noSessionFile: true},
		{name: "a live tmux window owns the ticket", binary: "claude", windows: []string{resumeTicketID}},
		{name: "another ticket's window does not block resume", binary: "claude", windows: []string{"kon-999"}, wantResume: true},
		{name: "resume disabled for the agent", binary: "claude", resume: new(false)},
		{name: "agent kontora cannot resume", binary: "aider"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResumeFixture(t, tc.binary)
			if tc.resume != nil {
				agentCfg := f.p.agentCfg
				agentCfg.Resume = tc.resume
				f.p.agentCfg = agentCfg
			}
			f.d.windows.list = func(string) ([]string, error) { return tc.windows, nil }

			kind := agentKindClaude
			if tc.binary == "pi" {
				kind = agentKindPi
			}
			rec := f.validRecord(kind)
			if tc.mutate != nil {
				tc.mutate(&rec)
			}
			if !tc.noRecord {
				writeRecordFile(t, f.p.cfg, f.p.ticketID, f.p.stageName, rec)
			}
			if !tc.noSessionFile {
				plantClaudeSession(t, f.claudeDir, resumeTestSessionID)
				plantPiSession(t, f.p.cfg, f.p.ticketID, f.p.stageName, "session-code.jsonl", time.Now())
			}

			got := f.d.resumableRecord(f.p)
			if !tc.wantResume {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			if tc.binary == "pi" {
				assert.Equal(t,
					filepath.Join(f.h.logsDir, resumeTicketID, "pi-sessions", "code", "session-code.jsonl"),
					got.sessionPath)
			} else {
				assert.Equal(t, resumeTestSessionID, got.SessionID)
			}
		})
	}
}

func TestResumableRecordSkipsWhenWindowsCannotBeListed(t *testing.T) {
	f := newResumeFixture(t, "claude")
	f.d.windows.list = func(string) ([]string, error) { return nil, assert.AnError }
	writeRecordFile(t, f.p.cfg, f.p.ticketID, f.p.stageName, f.validRecord(agentKindClaude))
	plantClaudeSession(t, f.claudeDir, resumeTestSessionID)

	assert.Nil(t, f.d.resumableRecord(f.p), "an unreadable window list must not resume")
}

// Pi keeps every attempt's session file in the stage directory, so the file the
// interrupted run wrote is the one modified after that run began. Older files
// belong to attempts that already ended.
func TestPiResumeSessionFileIgnoresEarlierAttempts(t *testing.T) {
	f := newResumeFixture(t, "pi")
	started := time.Now()
	plantPiSession(t, f.p.cfg, f.p.ticketID, f.p.stageName, "old-attempt.jsonl", started.Add(-time.Hour))

	rec := f.validRecord(agentKindPi)
	rec.StartedAt = started
	writeRecordFile(t, f.p.cfg, f.p.ticketID, f.p.stageName, rec)
	assert.Nil(t, f.d.resumableRecord(f.p), "only a file from an earlier attempt")

	plantPiSession(t, f.p.cfg, f.p.ticketID, f.p.stageName, "interrupted.jsonl", started.Add(time.Minute))
	got := f.d.resumableRecord(f.p)
	require.NotNil(t, got)
	assert.Equal(t, "interrupted.jsonl", filepath.Base(got.sessionPath))
}

// resumeDaemon runs a real daemon over the one-stage pipeline and records every
// agent invocation, so tests can assert on the arguments and prompt a resumed
// stage is spawned with.
type resumeDaemon struct {
	h         *testHarness
	d         *Daemon
	cfg       *config.Config
	claudeDir string
	// wtPath is the worktree the daemon will reuse for the ticket, created up
	// front so a record can name it.
	wtPath string

	mu     sync.Mutex
	spawns []RunnerParams
}

// The one-stage pipeline in the default test config.
const resumeStage = "step1"

// newResumeDaemon wires a daemon whose runner calls run with the 1-based
// invocation count. A nil run exits cleanly.
func newResumeDaemon(t *testing.T, agentBinary string, run func(ctx context.Context, n int, p RunnerParams) (process.Result, error)) *resumeDaemon {
	t.Helper()
	h := newHarness(t)
	claudeDir := t.TempDir()
	cfg := h.defaultConfig(agentBinary, agentBinary)
	cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeDir}

	rd := &resumeDaemon{h: h, cfg: cfg, claudeDir: claudeDir}
	runner := func(ctx context.Context, p RunnerParams) (process.Result, error) {
		rd.mu.Lock()
		rd.spawns = append(rd.spawns, p)
		n := len(rd.spawns)
		rd.mu.Unlock()
		if run != nil {
			return run(ctx, n, p)
		}
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	rd.d = New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)
	rd.d.windows = windowOps{
		list: func(string) ([]string, error) { return nil, nil },
		kill: func(string, string) error { return nil },
	}

	// Create the worktree ahead of the daemon so a record can name its path;
	// worktree creation is idempotent, so the daemon reuses this one.
	wtPath, _, err := rd.d.worktrees.Create(worktree.CreateOpts{
		RepoPath: h.repoDir,
		RepoName: h.repoName,
		TaskID:   resumeTicketID,
		Branch:   "kontora/" + resumeTicketID,
	})
	require.NoError(t, err)
	rd.wtPath = wtPath
	return rd
}

// plantRecord writes a record for the stage the daemon is about to run.
func (rd *resumeDaemon) plantRecord(t *testing.T, kind string, mutate func(*resumeRecord)) {
	t.Helper()
	rec := resumeRecord{
		Stage:     resumeStage,
		Agent:     kind,
		Worktree:  rd.wtPath,
		Instance:  rd.d.instanceName,
		StartedAt: time.Now().Add(-time.Minute),
	}
	switch kind {
	case agentKindClaude:
		rec.SessionID = resumeTestSessionID
		plantClaudeSession(t, rd.claudeDir, resumeTestSessionID)
	case agentKindPi:
		plantPiSession(t, rd.cfg, resumeTicketID, resumeStage, "interrupted.jsonl", time.Now())
	}
	if mutate != nil {
		mutate(&rec)
	}
	writeRecordFile(t, rd.cfg, resumeTicketID, resumeStage, rec)
}

// run starts the daemon, submits the one-stage pipeline ticket, and waits for it
// to reach want.
func (rd *resumeDaemon) run(t *testing.T, want ticket.Status) {
	t.Helper()
	rd.runWith(t, rd.h.taskMD(resumeTicketID, "todo", "one-stage"), want)
}

func (rd *resumeDaemon) runWith(t *testing.T, ticketMD string, want ticket.Status) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- rd.d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	rd.h.writeTicket(resumeTicketID+".md", ticketMD)
	rd.h.waitForStatus(resumeTicketID+".md", want, 15*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

func (rd *resumeDaemon) invocations(t *testing.T) []RunnerParams {
	t.Helper()
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return slices.Clone(rd.spawns)
}

func (rd *resumeDaemon) recordPath() string {
	return resumeRecordPath(rd.cfg, resumeTicketID, resumeStage)
}

// prompt returns the rendered prompt of an invocation. buildAgentArgs appends it
// after the agent's flags, and only pi's --session-dir follows it.
func renderedPrompt(p RunnerParams) string {
	args := p.Args
	if i := slices.Index(args, "--session-dir"); i >= 0 {
		args = args[:i]
	}
	return args[len(args)-1]
}

// The normal step1 prompt, which a resumed run must not receive: it would tell
// the agent to begin the stage it is already halfway through.
const resumeStagePrompt = "do step1 for " + resumeTicketID

func TestResumeClaudeContinuesRecordedSession(t *testing.T) {
	rd := newResumeDaemon(t, "claude", nil)
	rd.plantRecord(t, agentKindClaude, nil)

	rd.run(t, ticket.StatusDone)

	spawns := rd.invocations(t)
	require.Len(t, spawns, 1)
	assert.Equal(t, []string{"--resume", resumeTestSessionID},
		spawns[0].Args[slices.Index(spawns[0].Args, "--resume"):][:2])
	assert.NotContains(t, spawns[0].Args, "--session-id", "a resumed run must not open a new session")
	assert.Equal(t, resumeTestSessionID, spawns[0].SessionID,
		"log materialization must keep reading the recorded session")

	got := renderedPrompt(spawns[0])
	assert.Contains(t, got, "was interrupted when the daemon restarted")
	assert.Contains(t, got, "## Operational Context")
	assert.Contains(t, got, "Ticket ID: "+resumeTicketID)
	assert.NotContains(t, got, resumeStagePrompt)
}

func TestResumePiContinuesStageSessionFile(t *testing.T) {
	rd := newResumeDaemon(t, "pi", nil)
	rd.plantRecord(t, agentKindPi, nil)

	rd.run(t, ticket.StatusDone)

	spawns := rd.invocations(t)
	require.Len(t, spawns, 1)
	i := slices.Index(spawns[0].Args, "--session")
	require.GreaterOrEqual(t, i, 0, "args missing --session: %v", spawns[0].Args)
	assert.Equal(t,
		filepath.Join(rd.h.logsDir, resumeTicketID, "pi-sessions", resumeStage, "interrupted.jsonl"),
		spawns[0].Args[i+1])
	assert.Contains(t, renderedPrompt(spawns[0]), "was interrupted when the daemon restarted")
}

// A record that does not name this exact stage and agent must be ignored: a
// stage resuming another stage's conversation would hand the agent the wrong
// work entirely.
func TestResumeRejectedRecordRunsStageFresh(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*resumeRecord)
	}{
		{name: "record names another stage", mutate: func(r *resumeRecord) { r.Stage = "plan" }},
		{name: "record names another agent", mutate: func(r *resumeRecord) { r.Agent = agentKindPi }},
		{name: "record names another daemon instance", mutate: func(r *resumeRecord) { r.Instance = "desktop" }},
		{name: "record names another worktree", mutate: func(r *resumeRecord) { r.Worktree += "-new" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := newResumeDaemon(t, "claude", nil)
			rd.plantRecord(t, agentKindClaude, tc.mutate)

			rd.run(t, ticket.StatusDone)

			spawns := rd.invocations(t)
			require.Len(t, spawns, 1)
			assert.Contains(t, spawns[0].Args, "--session-id")
			assert.NotContains(t, spawns[0].Args, "--resume")
			assert.NotEqual(t, resumeTestSessionID, spawns[0].SessionID)
			assert.Contains(t, renderedPrompt(spawns[0]), resumeStagePrompt)
		})
	}
}

func TestResumeDisabledForAgentRunsStageFresh(t *testing.T) {
	rd := newResumeDaemon(t, "claude", nil)
	agentCfg := rd.cfg.Agents["agent1"]
	agentCfg.Resume = new(false)
	rd.cfg.Agents["agent1"] = agentCfg
	rd.plantRecord(t, agentKindClaude, nil)

	rd.run(t, ticket.StatusDone)

	spawns := rd.invocations(t)
	require.Len(t, spawns, 1)
	assert.Contains(t, spawns[0].Args, "--session-id")
	assert.NotContains(t, spawns[0].Args, "--resume")
	assert.Contains(t, renderedPrompt(spawns[0]), resumeStagePrompt)
}

// A ticket with no pipeline runs under stage name "default", so it resumes on
// the same code path. Its appendix leaves out the note about a next stage.
func TestResumeSimpleTicket(t *testing.T) {
	rd := newResumeDaemon(t, "claude", nil)
	plantClaudeSession(t, rd.claudeDir, resumeTestSessionID)
	writeRecordFile(t, rd.cfg, resumeTicketID, "default", resumeRecord{
		SessionID: resumeTestSessionID,
		Stage:     "default",
		Agent:     agentKindClaude,
		Worktree:  rd.wtPath,
		Instance:  rd.d.instanceName,
		StartedAt: time.Now().Add(-time.Minute),
	})

	rd.runWith(t, simpleTaskMD(resumeTicketID, "todo", rd.h.repoDir), ticket.StatusDone)

	spawns := rd.invocations(t)
	require.Len(t, spawns, 1)
	assert.Contains(t, spawns[0].Args, "--resume")
	got := renderedPrompt(spawns[0])
	assert.Contains(t, got, "was interrupted when the daemon restarted")
	assert.NotContains(t, got, "the next stage of the pipeline")
	assert.NoFileExists(t, resumeRecordPath(rd.cfg, resumeTicketID, "default"))
}

func TestResumePromptConfigured(t *testing.T) {
	rd := newResumeDaemon(t, "claude", nil)
	rd.cfg.ResumePrompt = "Continue the interrupted work on {{ .Ticket.ID }}"
	rd.plantRecord(t, agentKindClaude, nil)

	rd.run(t, ticket.StatusDone)

	spawns := rd.invocations(t)
	require.Len(t, spawns, 1)
	got := renderedPrompt(spawns[0])
	assert.Contains(t, got, "Continue the interrupted work on "+resumeTicketID)
	assert.Contains(t, got, "## Operational Context")
	assert.Contains(t, got, "Worktree: "+rd.wtPath)
	assert.NotContains(t, got, "was interrupted when the daemon restarted")
	assert.NotContains(t, got, resumeStagePrompt)
}

func TestResumeRecordLifecycle(t *testing.T) {
	var recordDuringRun []byte
	var recordPath string
	rd := newResumeDaemon(t, "claude", func(_ context.Context, _ int, _ RunnerParams) (process.Result, error) {
		recordDuringRun, _ = os.ReadFile(recordPath)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	})
	recordPath = rd.recordPath()

	rd.run(t, ticket.StatusDone)

	var rec resumeRecord
	require.NoError(t, json.Unmarshal(recordDuringRun, &rec), "record must exist while the agent runs")
	assert.NotEmpty(t, rec.SessionID)
	assert.Equal(t, resumeStage, rec.Stage)
	assert.Equal(t, agentKindClaude, rec.Agent)
	assert.Equal(t, rd.wtPath, rec.Worktree)
	assert.Equal(t, "test-instance", rec.Instance)
	assert.False(t, rec.StartedAt.IsZero())

	assert.NoFileExists(t, recordPath, "a run the daemon saw end leaves no record")
}

// Only a daemon that goes away mid-stage leaves a record behind: that is the
// one case where the next process can pick the conversation back up.
func TestResumeRecordSurvivesDaemonShutdown(t *testing.T) {
	h := newHarness(t)
	claudeDir := t.TempDir()
	cfg := h.defaultConfig("claude", "claude")
	cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeDir}

	running := make(chan struct{})
	var once sync.Once
	runner := func(ctx context.Context, _ RunnerParams) (process.Result, error) {
		once.Do(func() { close(running) })
		<-ctx.Done()
		return process.Result{ExitCode: -1, StartedAt: time.Now(), ExitedAt: time.Now()}, ctx.Err()
	}

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)
	d.windows = windowOps{
		list: func(string) ([]string, error) { return nil, nil },
		kill: func(string, string) error { return nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	h.writeTicket(resumeTicketID+".md", h.taskMD(resumeTicketID, "todo", "one-stage"))

	select {
	case <-running:
	case <-time.After(15 * time.Second):
		t.Fatal("agent never started")
	}

	cancel()
	require.NoError(t, <-errCh)

	assert.FileExists(t, resumeRecordPath(cfg, resumeTicketID, resumeStage),
		"a stage the daemon shut down on stays resumable")
}

// A resumed agent whose work is already finished can exit inside the tmux
// startup guard, which surfaces as a refused invocation. Pausing the ticket on
// that would make resume worse than not resuming at all.
func TestResumeThatDoesNotStartFallsBackToFreshRun(t *testing.T) {
	cases := []struct {
		name string
		err  string
	}{
		{name: "runner rejects the invocation", err: "spawn refused"},
		{name: "agent exits inside the startup guard", err: "interactive agent exited too quickly (300ms < 2s)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := newResumeDaemon(t, "claude", func(_ context.Context, n int, _ RunnerParams) (process.Result, error) {
				if n == 1 {
					return process.Result{ExitCode: 1, StartedAt: time.Now(), ExitedAt: time.Now()}, errors.New(tc.err)
				}
				return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
			})
			rd.plantRecord(t, agentKindClaude, nil)

			rd.run(t, ticket.StatusDone)

			spawns := rd.invocations(t)
			require.Len(t, spawns, 2, "exactly one resumed attempt and one fresh run")
			assert.Contains(t, spawns[0].Args, "--resume")
			assert.Contains(t, spawns[1].Args, "--session-id")
			assert.NotContains(t, spawns[1].Args, "--resume")
			assert.NotEqual(t, resumeTestSessionID, spawns[1].SessionID)
			assert.Contains(t, renderedPrompt(spawns[1]), resumeStagePrompt)
			assert.NoFileExists(t, rd.recordPath())
		})
	}
}

// Pausing is a deliberate restart. The daemon is still up when the agent dies,
// so nothing is left to resume and the stage starts over on the next pickup.
func TestResumeRecordRemovedOnUserPause(t *testing.T) {
	rd := newResumeDaemon(t, "claude", func(ctx context.Context, _ int, _ RunnerParams) (process.Result, error) {
		<-ctx.Done()
		return process.Result{ExitCode: -1, StartedAt: time.Now(), ExitedAt: time.Now()}, ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- rd.d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	rd.h.writeTicket(resumeTicketID+".md", rd.h.taskMD(resumeTicketID, "todo", "one-stage"))
	rd.h.waitForStatus(resumeTicketID+".md", ticket.StatusInProgress, 10*time.Second)

	time.Sleep(100 * time.Millisecond)
	paused := strings.Replace(rd.h.taskMD(resumeTicketID, "todo", "one-stage"), "status: todo", "status: paused", 1)
	rd.d.handleFileChanged(rd.h.writeTicket(resumeTicketID+".md", paused))

	waitForAgentsDone(t, rd.d, 10*time.Second)
	assert.NoFileExists(t, rd.recordPath())

	cancel()
	require.NoError(t, <-errCh)
}

// The runner can also fail after the agent completed a turn: tmux cannot send
// /exit once the Stop hook has fired. Rerunning the stage would repeat work the
// worktree already holds, so a failure that late pauses like any other.
func TestResumeFailureAfterAgentWorkedPausesWithoutRerunning(t *testing.T) {
	rd := newResumeDaemon(t, "claude", func(_ context.Context, _ int, _ RunnerParams) (process.Result, error) {
		started := time.Now().Add(-time.Minute)
		return process.Result{ExitCode: -1, StartedAt: started, ExitedAt: time.Now()},
			errors.New("sending /exit after hook: no such window")
	})
	rd.plantRecord(t, agentKindClaude, nil)

	rd.run(t, ticket.StatusPaused)

	spawns := rd.invocations(t)
	require.Len(t, spawns, 1, "a resume that reached real work must not run again")
	assert.Contains(t, spawns[0].Args, "--resume")
	tk := rd.h.readTask(resumeTicketID + ".md")
	assert.Contains(t, tk.LastError, "runner failed")
}

func TestResumeFallbackFailurePausesWithoutRetrying(t *testing.T) {
	rd := newResumeDaemon(t, "claude", func(_ context.Context, _ int, _ RunnerParams) (process.Result, error) {
		return process.Result{ExitCode: 1, StartedAt: time.Now(), ExitedAt: time.Now()}, assert.AnError
	})
	rd.plantRecord(t, agentKindClaude, nil)

	rd.run(t, ticket.StatusPaused)

	assert.Len(t, rd.invocations(t), 2, "a failed fallback is not retried again")
	tk := rd.h.readTask(resumeTicketID + ".md")
	assert.Contains(t, tk.LastError, "runner failed")
}
