package ticket

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteReactionsRoundTrip(t *testing.T) {
	dir := t.TempDir()

	empty, err := LoadNoteReactions(dir, "tst-001")
	require.NoError(t, err, "a missing sidecar is an empty set, not an error")
	assert.Empty(t, empty.For("q88f"))

	require.NoError(t, SetNoteReaction(dir, "tst-001", "q88f", "👍", "alexander", true))
	require.NoError(t, SetNoteReaction(dir, "tst-001", "q88f", "👍", "claude", true))
	require.NoError(t, SetNoteReaction(dir, "tst-001", "q88f", "🎉", "claude", true))

	state, err := LoadNoteReactions(dir, "tst-001")
	require.NoError(t, err)
	assert.Equal(t, []NoteReaction{
		{Emoji: "👍", Actors: []string{"alexander", "claude"}},
		{Emoji: "🎉", Actors: []string{"claude"}},
	}, state.For("q88f"))

	// Re-adding the same actor is not a second vote.
	require.NoError(t, SetNoteReaction(dir, "tst-001", "q88f", "👍", "claude", true))
	state, err = LoadNoteReactions(dir, "tst-001")
	require.NoError(t, err)
	assert.Equal(t, []string{"alexander", "claude"}, state.For("q88f")[0].Actors)

	// A chip that drops to no actors goes, and a note with no chips left goes
	// with it.
	require.NoError(t, SetNoteReaction(dir, "tst-001", "q88f", "👍", "alexander", false))
	require.NoError(t, SetNoteReaction(dir, "tst-001", "q88f", "👍", "claude", false))
	require.NoError(t, SetNoteReaction(dir, "tst-001", "q88f", "🎉", "claude", false))
	state, err = LoadNoteReactions(dir, "tst-001")
	require.NoError(t, err)
	assert.NotContains(t, state.Reactions, "q88f")

	require.NoError(t, RemoveNoteSidecar(dir, "tst-001"))
	assert.NoFileExists(t, filepath.Join(dir, "tst-001.notes.json"))
	require.NoError(t, RemoveNoteSidecar(dir, "tst-001"), "removing a sidecar that never existed is not an error")
}

func TestNoteSidecarIsInvisibleToTheStore(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tst-001.md"), []byte("---\nid: tst-001\n---\n# T\n"), 0o644))
	require.NoError(t, SetNoteReaction(dir, "tst-001", "q88f", "👍", "alexander", true))

	files, err := ListFiles(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(dir, "tst-001.md")}, files)
}

func TestSetNoteReactionRejects(t *testing.T) {
	tests := []struct {
		name     string
		ticketID string
		emoji    string
		actor    string
	}{
		{name: "traversal in the ticket id", ticketID: "../evil", emoji: "👍", actor: "a"},
		{name: "empty emoji", ticketID: "tst-001", emoji: "", actor: "a"},
		{name: "emoji with a slash", ticketID: "tst-001", emoji: "a/b", actor: "a"},
		{name: "emoji with a space", ticketID: "tst-001", emoji: "a b", actor: "a"},
		{name: "no actor", ticketID: "tst-001", emoji: "👍", actor: " "},
	}

	dir := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, SetNoteReaction(dir, tt.ticketID, "q88f", tt.emoji, tt.actor, true))
		})
	}
}
