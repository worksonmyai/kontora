package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/companion"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestCompanionAssistantToolsRoutesAndScopedSessions(t *testing.T) {
	for _, kind := range []string{"pi", "claude"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.assistantConfig(kind)
			cfg.Assistant.Prompt = "GENERAL CUSTOM EXECUTION PROMPT"
			cfg.Assistant.Autonomy = config.AutonomyAuto
			cfg.Assistant.Companion.Enabled = true
			originalClaude := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(originalClaude, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"test@example.invalid"}}`), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(originalClaude, "settings.json"), []byte(`{"apiKeyHelper":"configured-auth-helper"}`), 0o600))
			cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": originalClaude}
			calls := make(chan TurnParams, 4)
			var d *Daemon
			spawn := func(_ context.Context, p TurnParams) (string, error) {
				for _, call := range []struct {
					tool  string
					input map[string]any
				}{
					{"Read", map[string]any{"file_path": "/tickets/one.md"}},
					{"Bash", map[string]any{"command": "kontora ls"}},
					{"Bash", map[string]any{"command": "kontora run kon-1234"}},
					{"Bash", map[string]any{"command": "kontora rm kon-1234"}},
					{"WebFetch", map[string]any{"url": "https://example.invalid"}},
					{"Agent", nil}, {"AskUserQuestion", nil}, {"unknown_tool", nil},
				} {
					answer, err := d.AskAssistantGate(web.AssistantGateAskRequest{Thread: p.ThreadID, Nonce: p.Env[assistantNonceEnv], Tool: call.tool, Input: call.input})
					assert.NoError(t, err)
					assert.False(t, answer.Allow, call.tool)
					assert.NotEmpty(t, answer.Reason)
					_, pending := d.assistant.gate.Pending(p.ThreadID)
					assert.False(t, pending, "companion must never create an approval")
				}
				if kind == "claude" {
					id := flagValue(p.Args, "--session-id")
					if id != "" {
						dir := filepath.Join(p.Env["CLAUDE_CONFIG_DIR"], "projects", "p")
						require.NoError(t, os.MkdirAll(dir, 0o700))
						require.NoError(t, os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o600))
					}
				} else {
					dir := flagValue(p.Args, "--session-dir")
					require.NoError(t, os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(piSessionJSONL), 0o600))
				}
				calls <- p
				return "", nil
			}
			d = h.newDaemon(cfg, WithTurnSpawner(spawn))
			general, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{Autonomy: config.AutonomyAuto})
			require.NoError(t, err)
			thread, err := d.createCompanionThread()
			require.NoError(t, err)
			assert.Equal(t, assistant.PurposeCompanion, thread.Purpose)
			assert.Equal(t, config.AutonomyRead, thread.Autonomy)
			assert.True(t, strings.HasPrefix(thread.ID, "k"))
			store := d.assistantThreadStore(cfg, thread.ID)
			assert.Equal(t, filepath.Join(h.logsDir, "companion", companion.Scope(d.lockPath, cfg.TicketsDir), "assistant"), store.Root())
			assert.Equal(t, filepath.Join(h.logsDir, "assistant"), d.assistantThreadStore(cfg, general.ID).Root())
			_, err = d.assistantStore(cfg).Load(thread.ID)
			assert.ErrorIs(t, err, assistant.ErrThreadNotFound)

			assert.ErrorIs(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "client bypass", Context: "forged evidence"}), web.ErrAssistantGateDenied)
			for _, autonomy := range []string{config.AutonomyAsk, config.AutonomyAuto, "invalid"} {
				assert.ErrorIs(t, d.postAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "escalate", Autonomy: autonomy}, true), web.ErrAssistantGateDenied)
			}
			assert.ErrorIs(t, d.postAssistantMessage(general.ID, web.AssistantMessageRequest{Text: "wrong purpose"}, true), web.ErrAssistantGateDenied)
			untouched, err := store.Load(thread.ID)
			require.NoError(t, err)
			assert.Zero(t, untouched.Turns)

			for i := range 2 {
				require.NoError(t, d.postAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "Explain captured work", Context: "SERVER CAPTURE", Autonomy: config.AutonomyRead}, true))
				var call TurnParams
				select {
				case call = <-calls:
				case <-time.After(5 * time.Second):
					t.Fatal("no companion turn")
				}
				waitIdle(t, d, thread.ID)
				prompt := flagValue(call.Args, "--append-system-prompt")
				assert.Contains(t, prompt, "ALL tools are forbidden")
				assert.Contains(t, prompt, "SERVER CAPTURE")
				assert.NotContains(t, prompt, cfg.Assistant.Prompt)
				assert.True(t, strings.HasPrefix(call.LogFile, store.Root()+string(os.PathSeparator)))
				if kind == "claude" {
					assert.Equal(t, originalClaude, call.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"])
					assert.True(t, strings.HasPrefix(call.Env["CLAUDE_CONFIG_DIR"], store.Root()+string(os.PathSeparator)))
					settings, err := os.ReadFile(filepath.Join(call.Env["CLAUDE_CONFIG_DIR"], "settings.json"))
					require.NoError(t, err)
					assert.Contains(t, string(settings), "configured-auth-helper")
					if i == 1 {
						assert.NotEmpty(t, flagValue(call.Args, "-r"))
					}
				} else {
					assert.True(t, strings.HasPrefix(flagValue(call.Args, "--session-dir"), store.Root()+string(os.PathSeparator)))
				}
			}
			turns, err := store.Turns(thread.ID)
			require.NoError(t, err)
			require.Len(t, turns, 2)
			for _, turn := range turns {
				require.Len(t, turn.Gates, 8)
				for _, gate := range turn.Gates {
					assert.Equal(t, assistant.VerdictDeny, gate.Verdict)
					assert.False(t, gate.Approved)
				}
			}
			info, err := d.GetAssistantThread(thread.ID)
			require.NoError(t, err)
			assert.Zero(t, info.Writes)
			assert.Equal(t, 2, info.Turns)
			activity, err := d.AssistantActivity(web.AssistantActivityQuery{ID: thread.ID})
			require.NoError(t, err)
			assert.Len(t, activity.Messages, 2)
			_, err = d.AssistantPartial(thread.ID)
			require.NoError(t, err)
			require.NoError(t, d.StopAssistantTurn(thread.ID))
			list, err := d.ListAssistantThreads()
			require.NoError(t, err)
			require.Len(t, list, 2)
			assert.Equal(t, thread.ID, list[0].ID)
			require.NoError(t, d.DeleteAssistantThread(thread.ID))
			_, err = d.GetAssistantThread(thread.ID)
			assert.ErrorIs(t, err, web.ErrAssistantNotFound)
			_, err = d.GetAssistantThread(general.ID)
			assert.NoError(t, err)
		})
	}
}

func TestAssistantPreparationSerializesDeletionAndAdmission(t *testing.T) {
	h := newHarness(t)
	started, proceed := make(chan struct{}), make(chan struct{})
	stub := &stubTurns{turns: make(chan spawnedTurn, 2)}
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(stub.spawn))
	thread, err := d.createCompanionThread()
	require.NoError(t, err)
	postDone := make(chan error, 1)
	go func() {
		postDone <- d.postAssistantMessagePrepared(thread.ID, web.AssistantMessageRequest{Text: "prepared"}, true, func(store *assistant.Store, current assistant.Thread, _ *web.AssistantMessageRequest) error {
			assert.True(t, d.assistant.isRunning(thread.ID), "claim must precede preparation")
			assert.Zero(t, current.Turns)
			close(started)
			<-proceed
			dir, err := store.Dir(thread.ID)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "evidence.json"), []byte("captured"), 0o600)
		})
	}()
	<-started
	deleted := make(chan error, 1)
	go func() { deleted <- d.DeleteAssistantThread(thread.ID) }()
	select {
	case err := <-deleted:
		t.Fatalf("deletion overlapped preparation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(proceed)
	require.NoError(t, <-postDone)
	require.NoError(t, <-deleted)
	dir, err := d.assistantThreadStore(d.config(), thread.ID).Dir(thread.ID)
	require.NoError(t, err)
	_, err = os.Stat(dir)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.ErrorIs(t, d.postAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "late"}, true), web.ErrAssistantNotFound)
}

func TestAssistantConcurrentPostsKeepCountersAndSession(t *testing.T) {
	h := newHarness(t)
	started, finish := make(chan TurnParams, 2), make(chan struct{})
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(func(_ context.Context, p TurnParams) (string, error) {
		started <- p
		<-finish
		return "", nil
	}))
	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	const posts = 16
	var wg sync.WaitGroup
	results := make(chan error, posts)
	for range posts {
		wg.Go(func() {
			results <- d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "simultaneous"})
		})
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			assert.ErrorIs(t, err, web.ErrAssistantBusy)
		}
	}
	assert.Equal(t, 1, accepted)
	call := <-started
	metadata, err := d.assistantStore(d.config()).Load(thread.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, metadata.Turns)
	assert.Equal(t, metadata.SessionID, flagValue(call.Args, "--session-id"))
	close(finish)
	waitIdle(t, d, thread.ID)
}

func TestAssistantDeleteTimeoutPreservesBusyThread(t *testing.T) {
	h := newHarness(t)
	started, finish := make(chan struct{}), make(chan struct{})
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(func(_ context.Context, _ TurnParams) (string, error) {
		close(started)
		<-finish // Deliberately ignore cancellation.
		return "", nil
	}))
	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "hold"}))
	<-started
	assert.ErrorIs(t, d.deleteAssistantThread(thread.ID, time.Millisecond), web.ErrAssistantBusy)
	_, err = d.GetAssistantThread(thread.ID)
	require.NoError(t, err)
	assert.ErrorIs(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "overlap"}), web.ErrAssistantBusy)
	close(finish)
	waitIdle(t, d, thread.ID)
	require.NoError(t, d.DeleteAssistantThread(thread.ID))
	assert.ErrorIs(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "resurrect"}), web.ErrAssistantNotFound)
}

func TestAssistantShutdownClosesAdmissionBeforeJoiningPreparation(t *testing.T) {
	h := newHarness(t)
	prepared, proceed := make(chan struct{}), make(chan struct{})
	started := make(chan struct{}, 1)
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(func(ctx context.Context, _ TurnParams) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}))
	thread, err := d.createCompanionThread()
	require.NoError(t, err)
	posted := make(chan error, 1)
	go func() {
		posted <- d.postAssistantMessagePrepared(thread.ID, web.AssistantMessageRequest{Text: "prepare"}, true, func(_ *assistant.Store, _ assistant.Thread, _ *web.AssistantMessageRequest) error {
			close(prepared)
			<-proceed
			return nil
		})
	}()
	<-prepared
	stopped := make(chan struct{})
	go func() { d.assistant.stopAll(); d.background.Wait(); close(stopped) }()
	close(proceed)
	require.NoError(t, <-posted)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join the admitted turn")
	}
	assert.ErrorIs(t, d.postAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "too late"}, true), web.ErrAssistantBusy)
	_, err = d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	assert.ErrorIs(t, err, web.ErrAssistantBusy)
	_, err = d.createCompanionThread()
	assert.ErrorIs(t, err, web.ErrAssistantBusy)
}

func TestAssistantPreparationFailureReleasesSlot(t *testing.T) {
	h := newHarness(t)
	stub := &stubTurns{turns: make(chan spawnedTurn, 2)}
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(stub.spawn))
	thread, err := d.createCompanionThread()
	require.NoError(t, err)
	failure := errors.New("sidecar write failed")
	err = d.postAssistantMessagePrepared(thread.ID, web.AssistantMessageRequest{Text: "not admitted"}, true, func(_ *assistant.Store, _ assistant.Thread, _ *web.AssistantMessageRequest) error { return failure })
	assert.ErrorIs(t, err, failure)
	assert.False(t, d.assistant.isRunning(thread.ID))
	metadata, err := d.assistantThreadStore(d.config(), thread.ID).Load(thread.ID)
	require.NoError(t, err)
	assert.Zero(t, metadata.Turns)
	assert.Empty(t, stub.turns)
	require.NoError(t, d.postAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "retry"}, true))
	stub.next(t)
	waitIdle(t, d, thread.ID)
}
