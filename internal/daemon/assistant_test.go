package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

// piSessionJSONL is one canned pi session file: a model banner, a tool call
// with its result, and a closing line of prose.
const piSessionJSONL = `{"type":"model_change","modelId":"sonnet","timestamp":"2026-08-22T10:00:00.000Z"}
{"type":"message","timestamp":"2026-08-22T10:00:01.000Z","message":{"role":"assistant","usage":{"input":1,"output":2,"cacheWrite":0,"cacheRead":0},"content":[{"type":"toolCall","toolCallId":"c1","name":"bash","arguments":{"command":"kontora ls"}}]}}
{"type":"message","timestamp":"2026-08-22T10:00:02.000Z","message":{"role":"toolResult","toolName":"bash","isError":false,"content":[{"type":"text","text":"kon-7d21 running\nkon-9a02 todo\n"}]}}
{"type":"message","timestamp":"2026-08-22T10:00:03.000Z","message":{"role":"assistant","usage":{"input":1,"output":2,"cacheWrite":0,"cacheRead":0},"content":[{"type":"text","text":"Two tickets are running."}]}}
`

// spawnedTurn is one call the stub spawner saw.
type spawnedTurn struct {
	args []string
	env  map[string]string
}

// stubTurns records every turn and, for a pi agent, writes the canned session
// file where the reader will look for it.
type stubTurns struct {
	turns   chan spawnedTurn
	session string
	// claudeDir stands in for CLAUDE_CONFIG_DIR. When it is set the spawner
	// writes the session file claude would have written, so the next turn finds
	// something to resume.
	claudeDir string
	// before runs inside the spawner, so a test can drive the gate while the
	// turn is still in flight.
	before func(ctx context.Context, p TurnParams)
}

func (s *stubTurns) spawn(ctx context.Context, p TurnParams) (string, error) {
	if s.before != nil {
		s.before(ctx, p)
	}
	if dir := flagValue(p.Args, "--session-dir"); dir != "" && s.session != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(s.session), 0o644); err != nil {
			return "", err
		}
	}
	if id := flagValue(p.Args, "--session-id"); id != "" && s.claudeDir != "" {
		dir := filepath.Join(s.claudeDir, "projects", "p")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			return "", err
		}
	}
	s.turns <- spawnedTurn{args: slices.Clone(p.Args), env: p.Env}
	return "", nil
}

func (s *stubTurns) next(t *testing.T) spawnedTurn {
	t.Helper()
	select {
	case turn := <-s.turns:
		return turn
	case <-time.After(5 * time.Second):
		t.Fatal("no assistant turn was spawned")
		return spawnedTurn{}
	}
}

func flagValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// assistantConfig returns a harness config whose assistant runs on the named
// agent binary.
func (h *testHarness) assistantConfig(binary string) *config.Config {
	cfg := h.defaultConfig("true", "true")
	cfg.Agents["assistant"] = config.Agent{Binary: binary}
	cfg.Assistant = config.Assistant{
		Agent:    "assistant",
		Workdir:  h.tasksDir,
		Autonomy: config.AutonomyAsk,
		Timeout:  config.Duration{Duration: time.Minute},
	}
	return cfg
}

// waitIdle waits for the thread's turn to finish, so the test reads a settled
// store rather than one the turn goroutine is still writing.
func waitIdle(t *testing.T, d *Daemon, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !d.assistant.isRunning(id) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the assistant turn never finished")
}

func TestAssistantTurnTapeAndResume(t *testing.T) {
	h := newHarness(t)
	stub := &stubTurns{turns: make(chan spawnedTurn, 4), session: piSessionJSONL}
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(stub.spawn))

	cfgInfo := d.AssistantConfig()
	require.True(t, cfgInfo.Enabled)
	assert.Equal(t, config.AgentKindPi, cfgInfo.Kind)

	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	assert.Equal(t, config.AutonomyAsk, thread.Autonomy)

	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "what is running"}))
	first := stub.next(t)
	waitIdle(t, d, thread.ID)

	sessionID := flagValue(first.args, "--session-id")
	require.NotEmpty(t, sessionID)
	assert.Equal(t, "what is running", first.args[len(first.args)-1])
	assert.Equal(t, thread.ID, first.env[assistantThreadEnv])
	assert.NotEmpty(t, first.env[assistantNonceEnv])

	// The transcript is the agent's own session file, read as the same tape a
	// ticket run's activity tab shows.
	info, err := d.AssistantActivity(web.AssistantActivityQuery{ID: thread.ID})
	require.NoError(t, err)
	require.NotNil(t, info.Tape)
	assert.False(t, info.Running)
	assert.Equal(t, "sonnet", info.Tape.Model)
	assert.Equal(t, []string{"model", "tool", "text"}, eventKinds(info.Tape))
	require.Len(t, info.Messages, 1)
	assert.Equal(t, "what is running", info.Messages[0].Text)
	assert.NotEmpty(t, info.ETag)

	// Every row carries its result, so the whole tape is stable and a cursor
	// past its end is pulled back to that end rather than skipping rows.
	require.Equal(t, len(info.Tape.Events), info.Tape.StableCount())
	sliced, err := d.AssistantActivity(web.AssistantActivityQuery{ID: thread.ID, After: 99})
	require.NoError(t, err)
	assert.Equal(t, len(info.Tape.Events), sliced.Offset)
	assert.Empty(t, sliced.Tape.Events)

	// The same validator answers 304.
	same, err := d.AssistantActivity(web.AssistantActivityQuery{ID: thread.ID, IfNoneMatch: info.ETag})
	require.NoError(t, err)
	assert.True(t, same.NotModified)

	// Turn two runs against the session turn one opened.
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "and now"}))
	second := stub.next(t)
	waitIdle(t, d, thread.ID)
	assert.Equal(t, sessionID, flagValue(second.args, "--session-id"))
	assert.Equal(t, flagValue(first.args, "--session-dir"), flagValue(second.args, "--session-dir"))

	// The first message titles the thread; both turns are recorded.
	stored, err := d.GetAssistantThread(thread.ID)
	require.NoError(t, err)
	assert.Equal(t, "what is running", stored.Title)
	assert.Equal(t, 2, stored.Turns)
	require.Len(t, stored.Messages, 2)
}

func TestAssistantClaudeTurnResumes(t *testing.T) {
	cases := []struct {
		name string
		// wroteSession is whether the first turn got as far as creating the
		// session file.
		wroteSession bool
	}{
		{name: "the second turn resumes what the first opened", wroteSession: true},
		// A first turn that dies before claude runs, as it did when the daemon
		// could not resolve the binary, writes no session. Resuming that id
		// fails with "No conversation found" and would go on failing for every
		// later message, so the thread opens the session instead.
		{name: "a session the first turn never wrote is opened, not resumed", wroteSession: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			claudeDir := filepath.Join(t.TempDir(), "claude")
			stub := &stubTurns{turns: make(chan spawnedTurn, 4)}
			if c.wroteSession {
				stub.claudeDir = claudeDir
			}
			cfg := h.assistantConfig("claude")
			cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeDir}
			d := h.newDaemon(cfg, WithTurnSpawner(stub.spawn))

			thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
			require.NoError(t, err)

			require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "one"}))
			first := stub.next(t)
			waitIdle(t, d, thread.ID)

			sessionID := flagValue(first.args, "--session-id")
			require.NotEmpty(t, sessionID)
			assert.NotContains(t, first.args, "-r")
			assert.Contains(t, first.args, "--settings")
			joined := strings.Join(first.args, " ")
			assert.Contains(t, joined, "--print --output-format stream-json --verbose")
			assert.Contains(t, first.args, h.logsDir, "the run logs have to be readable from the tickets dir")

			require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "two"}))
			second := stub.next(t)
			waitIdle(t, d, thread.ID)

			if c.wroteSession {
				assert.Equal(t, sessionID, flagValue(second.args, "-r"))
				assert.NotContains(t, second.args, "--session-id")
				return
			}
			// The same id, so the thread keeps its identity and recovers on the
			// next message rather than staying broken.
			assert.Equal(t, sessionID, flagValue(second.args, "--session-id"))
			assert.NotContains(t, second.args, "-r")
		})
	}
}

// gateHarness stands in for the agent's tool boundary: the stub spawner asks
// the gate about one command, exactly as the claude hook and the pi extension
// do, and the test reads the answer off the channel.
type gateHarness struct {
	d       *Daemon
	stub    *stubTurns
	answers chan web.AssistantGateAskResponse
	// command is what the next turn's agent asks about.
	command string
}

func newGateHarness(t *testing.T, h *testHarness) *gateHarness {
	t.Helper()
	g := &gateHarness{
		stub:    &stubTurns{turns: make(chan spawnedTurn, 4), session: piSessionJSONL},
		answers: make(chan web.AssistantGateAskResponse, 4),
	}
	g.stub.before = func(_ context.Context, p TurnParams) {
		resp, err := g.d.AskAssistantGate(web.AssistantGateAskRequest{
			Thread: p.ThreadID,
			Nonce:  p.Env[assistantNonceEnv],
			Tool:   "Bash",
			Input:  map[string]any{"command": g.command},
		})
		assert.NoError(t, err)
		g.answers <- resp
	}
	g.d = h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(g.stub.spawn))
	return g
}

// ask starts a turn whose agent asks about command, and waits for the turn to
// finish. The verdict is on the answers channel.
func (g *gateHarness) ask(t *testing.T, threadID, command string) {
	t.Helper()
	g.command = command
	require.NoError(t, g.d.PostAssistantMessage(threadID, web.AssistantMessageRequest{Text: command}))
}

func (g *gateHarness) finish(t *testing.T, threadID string) {
	t.Helper()
	g.stub.next(t)
	waitIdle(t, g.d, threadID)
}

func TestAssistantGate(t *testing.T) {
	h := newHarness(t)
	g := newGateHarness(t, h)

	thread, err := g.d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)

	t.Run("a read runs without asking", func(t *testing.T) {
		g.ask(t, thread.ID, "kontora ls")
		assert.True(t, (<-g.answers).Allow)
		g.finish(t, thread.ID)
	})

	t.Run("a write in ask parks until it is approved", func(t *testing.T) {
		g.ask(t, thread.ID, "kontora set-stage kon-7d21 implement")

		gate := waitForGate(t, g.d, thread.ID)
		assert.Equal(t, "Bash", gate.Tool)
		assert.Equal(t, "kontora set-stage kon-7d21 implement", gate.Arg)
		assert.Equal(t, assistant.DecisionWrite, gate.Kind)

		require.NoError(t, g.d.ResolveAssistantGate(gate.ID, true))
		assert.True(t, (<-g.answers).Allow)
		g.finish(t, thread.ID)

		stored, err := g.d.GetAssistantThread(thread.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, stored.Writes)
	})

	t.Run("a skipped write is refused with a reason", func(t *testing.T) {
		g.ask(t, thread.ID, "kontora move kon-7d21 done")
		gate := waitForGate(t, g.d, thread.ID)
		require.NoError(t, g.d.ResolveAssistantGate(gate.ID, false))

		resp := <-g.answers
		assert.False(t, resp.Allow)
		assert.Contains(t, resp.Reason, "Do not retry")
		g.finish(t, thread.ID)
	})

	t.Run("an unrecognised call is parked rather than allowed", func(t *testing.T) {
		g.ask(t, thread.ID, "make deploy")
		gate := waitForGate(t, g.d, thread.ID)
		assert.Equal(t, assistant.DecisionWrite, gate.Kind)
		require.NoError(t, g.d.ResolveAssistantGate(gate.ID, false))
		assert.False(t, (<-g.answers).Allow)
		g.finish(t, thread.ID)
	})

	t.Run("a parked write changes the validator so the poll carrying it is not a 304", func(t *testing.T) {
		idle, err := g.d.AssistantActivity(web.AssistantActivityQuery{ID: thread.ID})
		require.NoError(t, err)
		require.NotEmpty(t, idle.ETag)

		g.ask(t, thread.ID, "kontora pause kon-7d21")
		waitForGate(t, g.d, thread.ID)

		parked, err := g.d.AssistantActivity(web.AssistantActivityQuery{ID: thread.ID, IfNoneMatch: idle.ETag})
		require.NoError(t, err)
		assert.False(t, parked.NotModified)
		require.NotNil(t, parked.Gate)

		gate := waitForGate(t, g.d, thread.ID)
		require.NoError(t, g.d.ResolveAssistantGate(gate.ID, false))
		<-g.answers
		g.finish(t, thread.ID)
	})

	t.Run("a gate call with the wrong nonce is refused", func(t *testing.T) {
		_, err := g.d.AskAssistantGate(web.AssistantGateAskRequest{Thread: thread.ID, Nonce: "wrong", Tool: "Read"})
		assert.ErrorIs(t, err, web.ErrAssistantGateDenied)
	})
}

func TestAssistantAutonomyModes(t *testing.T) {
	tests := []struct {
		name      string
		autonomy  string
		command   string
		wantPark  bool
		wantAllow bool
		wantWhy   string
	}{
		{name: "read refuses a write", autonomy: config.AutonomyRead, command: "kontora run kon-7d21", wantWhy: "read-only"},
		{name: "read refuses a delete", autonomy: config.AutonomyRead, command: "kontora delete kon-7d21", wantWhy: "read-only"},
		{name: "read allows a read", autonomy: config.AutonomyRead, command: "kontora view kon-7d21", wantAllow: true},
		{name: "auto allows a write", autonomy: config.AutonomyAuto, command: "kontora run kon-7d21", wantAllow: true},
		{name: "auto still holds a delete", autonomy: config.AutonomyAuto, command: "kontora delete kon-7d21", wantPark: true, wantAllow: true},
		{name: "ask holds a write", autonomy: config.AutonomyAsk, command: "kontora note kon-7d21 hi", wantPark: true, wantAllow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			g := newGateHarness(t, h)
			thread, err := g.d.CreateAssistantThread(web.CreateAssistantThreadRequest{Autonomy: tt.autonomy})
			require.NoError(t, err)
			require.Equal(t, tt.autonomy, thread.Autonomy)

			g.ask(t, thread.ID, tt.command)
			if tt.wantPark {
				gate := waitForGate(t, g.d, thread.ID)
				require.NoError(t, g.d.ResolveAssistantGate(gate.ID, tt.wantAllow))
			}
			resp := <-g.answers
			assert.Equal(t, tt.wantAllow, resp.Allow)
			if tt.wantWhy != "" {
				assert.Contains(t, resp.Reason, tt.wantWhy)
			}
			g.finish(t, thread.ID)
		})
	}
}

func TestAssistantOneTurnPerThread(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	stub := &stubTurns{turns: make(chan spawnedTurn, 4)}
	stub.before = func(context.Context, TurnParams) { <-release }
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(stub.spawn))

	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)

	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "one"}))
	assert.ErrorIs(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "two"}), web.ErrAssistantBusy)

	close(release)
	stub.next(t)
	waitIdle(t, d, thread.ID)

	// The slot is given back, so the next message runs.
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "three"}))
	stub.next(t)
	waitIdle(t, d, thread.ID)
}

func TestAssistantDisabled(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.defaultConfig("true", "true"))

	info := d.AssistantConfig()
	assert.False(t, info.Enabled)
	assert.Contains(t, info.Hint, "assistant.agent")

	_, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	assert.ErrorIs(t, err, web.ErrAssistantDisabled)

	threads, err := d.ListAssistantThreads()
	require.NoError(t, err)
	assert.Empty(t, threads, "a disabled assistant still has an empty history rather than an error")

	_, err = d.GetAssistantThread("nope")
	assert.ErrorIs(t, err, web.ErrAssistantNotFound)
}

func TestAssistantThreadLifecycle(t *testing.T) {
	h := newHarness(t)
	stub := &stubTurns{turns: make(chan spawnedTurn, 4)}
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(stub.spawn))

	first, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	second, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{Autonomy: config.AutonomyAuto})
	require.NoError(t, err)
	assert.Equal(t, config.AutonomyAuto, second.Autonomy)

	threads, err := d.ListAssistantThreads()
	require.NoError(t, err)
	assert.Len(t, threads, 2)

	require.NoError(t, d.DeleteAssistantThread(first.ID))
	assert.ErrorIs(t, d.DeleteAssistantThread(first.ID), web.ErrAssistantNotFound)

	threads, err = d.ListAssistantThreads()
	require.NoError(t, err)
	require.Len(t, threads, 1)
	assert.Equal(t, second.ID, threads[0].ID)

	assert.ErrorIs(t, d.StopAssistantTurn("nope"), web.ErrAssistantNotFound)
	require.NoError(t, d.StopAssistantTurn(second.ID), "stopping an idle thread is not an error")
}

func TestAssistantThreadKeepsItsOwnAgent(t *testing.T) {
	h := newHarness(t)
	stub := &stubTurns{turns: make(chan spawnedTurn, 4), session: piSessionJSONL}
	cfg := h.assistantConfig("pi")
	d := h.newDaemon(cfg, WithTurnSpawner(stub.spawn))

	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "one"}))
	stub.next(t)
	waitIdle(t, d, thread.ID)

	// The assistant is repointed at a claude agent. The pi session this thread
	// resumes cannot move with it, so the turn is refused rather than started
	// on the other CLI, which would silently lose the chat's history.
	moved := h.assistantConfig("claude")
	d.cfg.Store(moved)
	err = d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "two"})
	assert.ErrorIs(t, err, web.ErrAssistantStale)

	// A new chat runs on the new agent.
	fresh, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	require.NoError(t, d.PostAssistantMessage(fresh.ID, web.AssistantMessageRequest{Text: "one"}))
	turn := stub.next(t)
	waitIdle(t, d, fresh.ID)
	assert.Contains(t, turn.args, "--settings", "the fresh thread runs claude")
}

func TestAssistantTurnRecordsWhatTheGateAnswered(t *testing.T) {
	h := newHarness(t)
	g := newGateHarness(t, h)

	thread, err := g.d.CreateAssistantThread(web.CreateAssistantThreadRequest{Autonomy: config.AutonomyRead})
	require.NoError(t, err)

	g.ask(t, thread.ID, "kontora run kon-7d21")
	assert.False(t, (<-g.answers).Allow)
	g.finish(t, thread.ID)

	turns, err := g.d.assistantStore(g.d.config()).Turns(thread.ID)
	require.NoError(t, err)
	require.Len(t, turns, 1)
	require.Len(t, turns[0].Gates, 1)
	assert.Equal(t, assistant.GateRecord{
		Tool:    "Bash",
		Arg:     "kontora run kon-7d21",
		Kind:    assistant.DecisionWrite,
		Verdict: assistant.VerdictDeny,
	}, turns[0].Gates[0])
}

func TestAssistantDeleteDuringTurnLeavesNothingBehind(t *testing.T) {
	h := newHarness(t)
	started := make(chan struct{})
	stub := &stubTurns{turns: make(chan spawnedTurn, 4)}
	stub.before = func(ctx context.Context, _ TurnParams) {
		close(started)
		<-ctx.Done()
	}
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(stub.spawn))

	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "one"}))
	<-started

	// Deleting waits for the turn to let go, so nothing it writes on its way
	// out recreates the directory as an orphan with no thread.json in it.
	require.NoError(t, d.DeleteAssistantThread(thread.ID))
	stub.next(t)

	dir, err := d.assistantStore(d.config()).Dir(thread.ID)
	require.NoError(t, err)
	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr), "the thread directory came back: %v", statErr)
}

func TestDefaultTurnSpawnerOnlyLogsIntoALiveThreadDir(t *testing.T) {
	binary, err := exec.LookPath("true")
	require.NoError(t, err)
	root := t.TempDir()
	live := filepath.Join(root, "live")
	require.NoError(t, os.MkdirAll(live, 0o755))

	tests := []struct {
		name    string
		dir     string
		wantLog bool
	}{
		{name: "a thread that is still there gets its log", dir: live, wantLog: true},
		{name: "a thread deleted mid-turn is not recreated", dir: filepath.Join(root, "gone")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logFile := filepath.Join(tt.dir, "turn.1.log")
			_, err := defaultTurnSpawner(t.Context(), TurnParams{Binary: binary, LogFile: logFile})
			require.NoError(t, err)
			_, statErr := os.Stat(logFile)
			assert.Equal(t, tt.wantLog, statErr == nil, "stat: %v", statErr)
		})
	}
}

func TestAssistantLoopbackAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{name: "loopback is left alone", addr: "127.0.0.1:9090", want: "127.0.0.1:9090"},
		{name: "a named interface is left alone", addr: "192.168.1.4:9090", want: "192.168.1.4:9090"},
		// A wildcard bind is what web.host: 0.0.0.0 gives, and a Host header of
		// 0.0.0.0 is refused by the origin check, so every call the agent makes
		// would answer 403.
		{name: "a v4 wildcard becomes loopback", addr: "0.0.0.0:9090", want: "127.0.0.1:9090"},
		{name: "a v6 wildcard becomes loopback", addr: "[::]:9090", want: "[::1]:9090"},
		{name: "an address with no port is left alone", addr: "nonsense", want: "nonsense"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, assistantLoopbackAddr(tt.addr))
		})
	}
}

// waitForGate waits for the thread's parked write to show up in the poll, which
// is where the pane reads it from.
func waitForGate(t *testing.T, d *Daemon, id string) assistant.Pending {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := d.AssistantActivity(web.AssistantActivityQuery{ID: id})
		require.NoError(t, err)
		if info.Gate != nil {
			return *info.Gate
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no tool call was ever parked")
	return assistant.Pending{}
}

func TestAssistantETag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(piSessionJSONL), 0o644))
	gate := &assistant.Pending{ID: "g1"}
	parked := web.AssistantActivityInfo{Gate: gate}

	base := assistantETag(path, 1, web.AssistantActivityInfo{})
	require.NotEmpty(t, base)

	// Everything the response carries beside the session file has to move the
	// validator, or the poll that first reports it answers 304. The running
	// flag and the turn record matter most: a turn that fails before the agent
	// writes anything changes nothing else, and the pane sticks on "working".
	assert.NotEqual(t, base, assistantETag(path, 1, parked))
	assert.NotEqual(t, base, assistantETag(path, 2, web.AssistantActivityInfo{}))
	assert.NotEqual(t, base, assistantETag(path, 1, web.AssistantActivityInfo{Running: true}))
	assert.NotEqual(t, base, assistantETag(path, 1, web.AssistantActivityInfo{
		Messages: []web.AssistantMessage{{N: 1, Text: "hi"}},
	}))
	assert.NotEqual(t,
		assistantETag(path, 1, web.AssistantActivityInfo{Messages: []web.AssistantMessage{{N: 1}}}),
		assistantETag(path, 1, web.AssistantActivityInfo{Messages: []web.AssistantMessage{{N: 1, Error: "boom"}}}),
		"a turn that ended with an error is a different response")
	assert.Equal(t, base, assistantETag(path, 1, web.AssistantActivityInfo{}), "the same state is the same validator")

	// An unreadable file yields no validator, which never matches a client's.
	assert.Empty(t, assistantETag(filepath.Join(dir, "missing.jsonl"), 1, parked))
}

func TestAssistantGlobalCap(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	stub := &stubTurns{turns: make(chan spawnedTurn, 8)}
	stub.before = func(context.Context, TurnParams) { <-release }
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(stub.spawn))

	// One more chat than the cap allows, so the last one is refused even though
	// its own thread is idle.
	threads := make([]string, 0, assistantMaxConcurrent+1)
	for range assistantMaxConcurrent + 1 {
		thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
		require.NoError(t, err)
		threads = append(threads, thread.ID)
	}
	for _, id := range threads[:assistantMaxConcurrent] {
		require.NoError(t, d.PostAssistantMessage(id, web.AssistantMessageRequest{Text: "go"}))
	}
	// Refused for the cap, not as a busy thread: this chat has never run, and
	// telling the user it is already running a turn is unactionable.
	assert.ErrorIs(t, d.PostAssistantMessage(threads[assistantMaxConcurrent], web.AssistantMessageRequest{Text: "go"}), web.ErrAssistantAtCapacity)

	close(release)
	for _, id := range threads[:assistantMaxConcurrent] {
		stub.next(t)
		waitIdle(t, d, id)
	}
	// The slots come back, so the chat that was turned away now runs.
	require.NoError(t, d.PostAssistantMessage(threads[assistantMaxConcurrent], web.AssistantMessageRequest{Text: "go"}))
	stub.next(t)
	waitIdle(t, d, threads[assistantMaxConcurrent])
}

func TestAssistantTurnDoesNotHoldUpShutdown(t *testing.T) {
	h := newHarness(t)
	started := make(chan struct{})
	stub := &stubTurns{turns: make(chan spawnedTurn, 4)}
	// The turn blocks on its own context, the way a real agent subprocess does.
	stub.before = func(ctx context.Context, _ TurnParams) {
		close(started)
		<-ctx.Done()
	}
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(stub.spawn))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "go"}))
	<-started

	// A turn runs on a context of its own, so shutdown has to end it: without
	// that the wait for background work sits out the whole assistant timeout.
	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not shut down while an assistant turn was running")
	}
}

func TestAssistantGateFile(t *testing.T) {
	env := map[string]string{
		assistantURLEnv:    "http://127.0.0.1:8080",
		assistantTokenEnv:  `tok"en`,
		assistantThreadEnv: "abc123",
		assistantNonceEnv:  "deadbeef",
	}

	t.Run("the pi extension carries the values rather than reading the environment", func(t *testing.T) {
		// The agent's own tools can read the environment, and the nonce is what
		// stops something else approving this turn's writes.
		js := renderAssistantExtension(env)
		assert.NotContains(t, js, "process.env")
		assert.NotContains(t, js, "__KONTORA_")
		assert.Contains(t, js, `"http://127.0.0.1:8080"`)
		assert.Contains(t, js, `"abc123"`)
		assert.Contains(t, js, `"deadbeef"`)
		assert.Contains(t, js, `tok\"en`, "a quote in the token must not end the literal")
		assert.Contains(t, js, `pi.on("tool_call"`)
	})

	t.Run("a missing value renders as an empty literal, and the gate refuses", func(t *testing.T) {
		js := renderAssistantExtension(nil)
		assert.NotContains(t, js, "__KONTORA_")
		assert.Contains(t, js, `const NONCE = "";`)
	})

	t.Run("the claude settings run the hidden gate verb through a quoted path", func(t *testing.T) {
		d := newHarness(t).newDaemon(newHarness(t).assistantConfig("claude"))
		path, err := d.writeAssistantGateFile(config.AgentKindClaude, env)
		require.NoError(t, err)
		defer os.Remove(path)

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var settings struct {
			Hooks struct {
				PreToolUse []struct {
					Matcher string `json:"matcher"`
					Hooks   []struct {
						Type    string `json:"type"`
						Command string `json:"command"`
						Timeout int    `json:"timeout"`
					} `json:"hooks"`
				} `json:"PreToolUse"`
			} `json:"hooks"`
		}
		require.NoError(t, json.Unmarshal(data, &settings))
		require.Len(t, settings.Hooks.PreToolUse, 1)
		require.Len(t, settings.Hooks.PreToolUse[0].Hooks, 1)

		hook := settings.Hooks.PreToolUse[0].Hooks[0]
		assert.Equal(t, "command", hook.Type)
		assert.True(t, strings.HasSuffix(hook.Command, "' assistant-gate"), "the path is quoted: %s", hook.Command)
		// Longer than a parked write waits, or claude kills the hook while the
		// person is still deciding.
		assert.Greater(t, hook.Timeout, int(assistantGateTimeout.Seconds()))
	})

	t.Run("an agent kind with no gate is refused", func(t *testing.T) {
		d := newHarness(t).newDaemon(newHarness(t).assistantConfig("pi"))
		_, err := d.writeAssistantGateFile("aider", env)
		assert.ErrorContains(t, err, "no tool gate")
	})
}

func TestShellQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/usr/local/bin/kontora", "'/usr/local/bin/kontora'"},
		{"/Users/a b/kontora", "'/Users/a b/kontora'"},
		{"/it's/kontora", `'/it'\''s/kontora'`},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, shellQuote(tt.in))
	}
}
