package ticket

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// ErrNoteEmoji is a reaction whose emoji could not be stored: it is empty, too
// long, or holds a character that has no place in a chip label or a URL path.
var ErrNoteEmoji = errors.New("invalid reaction emoji")

// noteSidecarVersion is written on every save so a later format change can tell
// what it is reading.
const noteSidecarVersion = 1

// maxEmojiRunes bounds one chip label. A ZWJ sequence such as a flag or a
// family is several runes, so this is well above one grapheme.
const maxEmojiRunes = 16

// NoteReactions is the on-disk shape of "<tickets_dir>/<ticket-id>.notes.json".
//
// Reactions live beside the ticket rather than in its body because the body is
// interpolated into a stage prompt: a chip table in the "## Notes" section
// would be noise to the agent that reads it. The cost is that a ticket copied
// without its sidecar keeps its conversation and loses its reactions.
type NoteReactions struct {
	Version int `json:"version"`
	// Reactions maps a note id to its chips. A note with no chips left is
	// removed from the map rather than kept with an empty list.
	Reactions map[string][]NoteReaction `json:"reactions,omitempty"`
}

// NoteReaction is one emoji chip and everyone who is on it.
type NoteReaction struct {
	Emoji  string   `json:"emoji"`
	Actors []string `json:"actors"`
}

// For returns the chips on one note, or nil.
func (r NoteReactions) For(noteID string) []NoteReaction {
	return r.Reactions[noteID]
}

// NoteSidecarPath builds the sidecar path for a ticket.
func NoteSidecarPath(dir, ticketID string) (string, error) {
	if !IsSafeID(ticketID) {
		return "", fmt.Errorf("unsafe ticket id %q", ticketID)
	}
	return filepath.Join(dir, ticketID+".notes.json"), nil
}

// LoadNoteReactions reads a ticket's reactions. A missing sidecar is an empty
// set, never an error.
func LoadNoteReactions(dir, ticketID string) (NoteReactions, error) {
	path, err := NoteSidecarPath(dir, ticketID)
	if err != nil {
		return NoteReactions{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NoteReactions{Version: noteSidecarVersion}, nil
	}
	if err != nil {
		return NoteReactions{}, err
	}
	var out NoteReactions
	if err := json.Unmarshal(data, &out); err != nil {
		return NoteReactions{}, fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	return out, nil
}

// SetNoteReaction adds or removes one actor's reaction and saves the sidecar.
// A chip that drops to no actors is removed, and a note with no chips left is
// removed from the map, so an emptied sidecar is an empty object rather than a
// tree of husks.
func SetNoteReaction(dir, ticketID, noteID, emoji, actor string, on bool) error {
	if err := validNoteEmoji(emoji); err != nil {
		return err
	}
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: no actor", ErrNoteEmoji)
	}

	state, err := LoadNoteReactions(dir, ticketID)
	if err != nil {
		return err
	}
	if state.Reactions == nil {
		state.Reactions = map[string][]NoteReaction{}
	}

	chips := state.Reactions[noteID]
	at := slices.IndexFunc(chips, func(c NoteReaction) bool { return c.Emoji == emoji })
	switch {
	case on && at < 0:
		chips = append(chips, NoteReaction{Emoji: emoji, Actors: []string{actor}})
	case on:
		if !slices.Contains(chips[at].Actors, actor) {
			chips[at].Actors = append(chips[at].Actors, actor)
		}
	case at >= 0:
		chips[at].Actors = slices.DeleteFunc(chips[at].Actors, func(a string) bool { return a == actor })
		if len(chips[at].Actors) == 0 {
			chips = slices.Delete(chips, at, at+1)
		}
	}

	if len(chips) == 0 {
		delete(state.Reactions, noteID)
	} else {
		state.Reactions[noteID] = chips
	}
	return saveNoteReactions(dir, ticketID, state)
}

// RemoveNoteSidecar deletes a ticket's sidecar. A ticket that never had one is
// not an error.
func RemoveNoteSidecar(dir, ticketID string) error {
	path, err := NoteSidecarPath(dir, ticketID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func saveNoteReactions(dir, ticketID string, state NoteReactions) error {
	path, err := NoteSidecarPath(dir, ticketID)
	if err != nil {
		return err
	}
	state.Version = noteSidecarVersion
	// Sorted so a rewrite that changes nothing produces the same bytes.
	for _, chips := range state.Reactions {
		for i := range chips {
			sort.Strings(chips[i].Actors)
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeNoteFileAtomic(path, append(data, '\n'))
}

func validNoteEmoji(emoji string) error {
	if emoji == "" {
		return fmt.Errorf("%w: empty", ErrNoteEmoji)
	}
	if len([]rune(emoji)) > maxEmojiRunes {
		return fmt.Errorf("%w: too long", ErrNoteEmoji)
	}
	for _, r := range emoji {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == '/' || r == '\\' {
			return fmt.Errorf("%w: %q", ErrNoteEmoji, emoji)
		}
	}
	return nil
}

// writeNoteFileAtomic writes through a temporary file in the same directory, so
// a reader never sees a half-written sidecar.
func writeNoteFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
