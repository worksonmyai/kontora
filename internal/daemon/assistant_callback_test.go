package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestAssistantDelayedCallbacksCannotAffectLaterTurn(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("pi")
	cfg.Assistant.Stream = new(true)
	calls := make(chan TurnParams, 2)
	finish := make(chan struct{}, 2)
	d := h.newDaemon(cfg, WithTurnSpawner(func(_ context.Context, p TurnParams) (string, error) {
		calls <- p
		<-finish
		return "", nil
	}))
	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{Autonomy: config.AutonomyAuto})
	require.NoError(t, err)
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "first"}))
	first := <-calls
	first.Stream.Text("old answer")
	finish <- struct{}{}
	waitIdle(t, d, thread.ID)

	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "second"}))
	second := <-calls
	second.Stream.Text("current answer")
	before := d.assistant.live.Snapshot(thread.ID)
	first.Stream.Block()
	first.Stream.Text("late old answer")
	first.Stream.Tool("late tool")
	first.Stream.Seal()
	_, err = d.AskAssistantGate(web.AssistantGateAskRequest{
		Thread: thread.ID, Nonce: first.Env[assistantNonceEnv], Tool: "Bash", Input: map[string]any{"command": "kontora run kon-1234"},
	})
	assert.ErrorIs(t, err, web.ErrAssistantGateDenied)
	assert.Equal(t, before, d.assistant.live.Snapshot(thread.ID))
	assert.True(t, d.assistant.nonceMatches(thread.ID, second.Env[assistantNonceEnv]))
	assert.False(t, d.assistant.nonceMatches(thread.ID, first.Env[assistantNonceEnv]))
	current, err := d.assistantStore(cfg).Load(thread.ID)
	require.NoError(t, err)
	assert.Zero(t, current.Writes)
	_, pending := d.assistant.gate.Pending(thread.ID)
	assert.False(t, pending)
	finish <- struct{}{}
	waitIdle(t, d, thread.ID)
	turns, err := d.assistantStore(cfg).Turns(thread.ID)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	assert.Empty(t, turns[1].Gates)
}

func TestAssistantOldParkedHookCannotWriteIntoLaterTurn(t *testing.T) {
	h := newHarness(t)
	calls := make(chan TurnParams, 2)
	finish := make(chan struct{}, 2)
	d := h.newDaemon(h.assistantConfig("pi"), WithTurnSpawner(func(_ context.Context, p TurnParams) (string, error) {
		calls <- p
		<-finish
		return "", nil
	}))
	thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{Autonomy: config.AutonomyAsk})
	require.NoError(t, err)
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "first"}))
	first := <-calls
	answer := make(chan web.AssistantGateAskResponse, 1)
	go func() {
		response, _ := d.AskAssistantGate(web.AssistantGateAskRequest{Thread: thread.ID, Nonce: first.Env[assistantNonceEnv], Tool: "Bash", Input: map[string]any{"command": "kontora run kon-1234"}})
		answer <- response
	}()
	require.Eventually(t, func() bool { _, pending := d.assistant.gate.Pending(thread.ID); return pending }, time.Second, time.Millisecond)
	finish <- struct{}{} // Agent exits without waiting for its parked hook.
	waitIdle(t, d, thread.ID)
	require.NoError(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "second"}))
	<-calls
	select {
	case response := <-answer:
		assert.False(t, response.Allow)
	case <-time.After(time.Second):
		t.Fatal("old hook was not cleared")
	}
	current, err := d.assistantStore(d.config()).Load(thread.ID)
	require.NoError(t, err)
	assert.Zero(t, current.Writes)
	finish <- struct{}{}
	waitIdle(t, d, thread.ID)
	turns, err := d.assistantStore(d.config()).Turns(thread.ID)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	assert.Empty(t, turns[1].Gates)
}

func TestCompanionStoreScopeDoesNotExposeOtherDaemons(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("pi")
	first := h.newDaemon(cfg)
	second := h.newDaemon(cfg, WithLockPath(filepath.Join(t.TempDir(), "another.lock")))
	thread, err := first.createCompanionThread()
	require.NoError(t, err)
	general, err := first.CreateAssistantThread(web.CreateAssistantThreadRequest{})
	require.NoError(t, err)
	_, err = second.GetAssistantThread(thread.ID)
	assert.ErrorIs(t, err, web.ErrAssistantNotFound)
	_, err = second.AssistantPartial(thread.ID)
	assert.ErrorIs(t, err, web.ErrAssistantNotFound)
	assert.ErrorIs(t, second.StopAssistantTurn(thread.ID), web.ErrAssistantNotFound)
	assert.ErrorIs(t, second.DeleteAssistantThread(thread.ID), web.ErrAssistantNotFound)
	list, err := second.ListAssistantThreads()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, general.ID, list[0].ID, "ordinary shared logs_dir history must remain unchanged")
}

func TestCompanionClaudeCredentialHomeIsPreserved(t *testing.T) {
	for _, tc := range []struct {
		name           string
		original       string
		secure         string
		explicitSecure bool
	}{
		{name: "default keychain uses explicit empty secure storage home"},
		{name: "custom config keeps exact keychain hash input", original: "/custom/claude-profile"},
		{name: "separate credentials home is not overwritten", original: "/custom/claude-profile", secure: "/credentials", explicitSecure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.assistantConfig("claude")
			d := h.newDaemon(cfg)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			saved, set := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
			require.NoError(t, os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR"))
			t.Cleanup(func() {
				if set {
					_ = os.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", saved)
				} else {
					_ = os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
				}
			})
			thread := assistant.Thread{ID: assistant.NewCompanionID(), Purpose: assistant.PurposeCompanion, Kind: config.AgentKindClaude}
			env := map[string]string{"CLAUDE_CONFIG_DIR": tc.original}
			if tc.explicitSecure {
				env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = tc.secure
			}
			require.NoError(t, d.prepareCompanionClaude(cfg, thread, env))
			want := tc.original
			if tc.explicitSecure {
				want = tc.secure
			}
			got, exists := env["CLAUDE_SECURESTORAGE_CONFIG_DIR"]
			assert.True(t, exists, "empty is significant: it selects the default keychain identity")
			assert.Equal(t, want, got)
			assert.Equal(t, d.companionClaudeDir(cfg, thread.ID), env["CLAUDE_CONFIG_DIR"])
			assert.NotEqual(t, tc.original, env["CLAUDE_CONFIG_DIR"])
		})
	}
}
