package summarizer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_Success(t *testing.T) {
	result, err := Run(context.Background(), Params{
		Binary:   "echo",
		Args:     []string{"-n"},
		Prompt:   "summarize this",
		PaneText: "some terminal output",
		TicketID: "t-001",
		Stage:    "code",
	})
	require.NoError(t, err)
	assert.Equal(t, "summarize this", result)
}

func TestRun_PipesStdin(t *testing.T) {
	result, err := Run(context.Background(), Params{
		Binary:   "cat",
		PaneText: "hello from stdin",
		TicketID: "t-001",
	})
	require.NoError(t, err)
	assert.Equal(t, "hello from stdin", result)
}

func TestRun_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, Params{
		Binary:   "sleep",
		Args:     []string{"10"},
		TicketID: "t-001",
	})
	require.Error(t, err)
}

func TestRun_NonZeroExit(t *testing.T) {
	_, err := Run(context.Background(), Params{
		Binary:   "false",
		TicketID: "t-001",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 1")
}

func TestRun_BinaryNotFound(t *testing.T) {
	_, err := Run(context.Background(), Params{
		Binary:   "nonexistent-binary-12345",
		TicketID: "t-001",
	})
	require.Error(t, err)
}

func TestRun_PromptTemplateRendering(t *testing.T) {
	result, err := Run(context.Background(), Params{
		Binary:   "echo",
		Args:     []string{"-n"},
		Prompt:   "Ticket: {{.TicketID}}, Stage: {{.Stage}}",
		PaneText: "",
		TicketID: "t-42",
		Stage:    "review",
	})
	require.NoError(t, err)
	assert.Equal(t, "Ticket: t-42, Stage: review", result)
}

func TestRun_EmptyPrompt(t *testing.T) {
	result, err := Run(context.Background(), Params{
		Binary:   "cat",
		Prompt:   "",
		PaneText: "just stdin",
		TicketID: "t-001",
	})
	require.NoError(t, err)
	assert.Equal(t, "just stdin", result)
}
