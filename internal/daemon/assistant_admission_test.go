package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestAssistantRejectedCompanionPostPreservesStreamedAnswer(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("pi")
	cfg.Assistant.Stream = new(true)
	spawned := make(chan struct{}, 1)
	d := h.newDaemon(cfg, WithTurnSpawner(func(_ context.Context, p TurnParams) (string, error) {
		p.Stream.Text("The captured explanation.")
		spawned <- struct{}{}
		return "", nil
	}))
	thread, err := d.createCompanionThread()
	require.NoError(t, err)
	require.NoError(t, d.postAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "explain"}, true))
	<-spawned
	waitIdle(t, d, thread.ID)
	before := d.assistant.live.Snapshot(thread.ID)
	require.NotEmpty(t, before.Text)
	assert.ErrorIs(t, d.PostAssistantMessage(thread.ID, web.AssistantMessageRequest{Text: "ordinary route"}), web.ErrAssistantGateDenied)
	after := d.assistant.live.Snapshot(thread.ID)
	assert.Equal(t, before.Text, after.Text)
	assert.Equal(t, before.Gen, after.Gen)
	assert.False(t, d.assistant.isRunning(thread.ID))
}

func TestAssistantHistorySurvivesUnavailableCompanionStore(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			h := newHarness(t)
			cfg := h.assistantConfig("pi")
			cfg.Assistant.Companion.Enabled = enabled
			d := h.newDaemon(cfg)
			thread, err := d.CreateAssistantThread(web.CreateAssistantThreadRequest{})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(h.logsDir, "companion"), []byte("unavailable"), 0o600))
			threads, err := d.ListAssistantThreads()
			require.NoError(t, err)
			require.Len(t, threads, 1)
			assert.Equal(t, thread.ID, threads[0].ID)
			_, err = d.GetAssistantThread(thread.ID)
			require.NoError(t, err)
		})
	}
}

func TestCompanionRelativeLogsUseAbsoluteSessionRoot(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("claude")
	cfg.LogsDir = "relative-logs"
	d := h.newDaemon(cfg)
	id := assistant.NewCompanionID()
	root := d.assistantThreadStore(cfg, id).Root()
	assert.True(t, filepath.IsAbs(root))
	assert.True(t, filepath.IsAbs(d.companionClaudeDir(cfg, id)))
	piDir, err := d.assistantThreadStore(cfg, id).PiSessionDir(id)
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(piDir))
	assert.Equal(t, filepath.Join("relative-logs", "assistant"), d.assistantStore(cfg).Root())
}
