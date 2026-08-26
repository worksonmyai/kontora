package ticket

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func notesTicket(t *testing.T, body string) *Ticket {
	t.Helper()
	tkt, err := ParseBytes([]byte("---\nid: test\nstatus: open\ncreated: 2026-01-01T00:00:00Z\n---\n" + body))
	require.NoError(t, err)
	return tkt
}

func TestParseNotes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []Note
	}{
		{
			name: "no notes section",
			body: "# Title\n\nSome description.\n",
			want: nil,
		},
		{
			name: "a legacy byline carries only a timestamp",
			body: "## Notes\n\n**2026-08-08T10:00:00Z**\n\nInvestigate timeout\n",
			want: []Note{{ID: "#0", At: "2026-08-08T10:00:00Z", Text: "Investigate timeout"}},
		},
		{
			name: "an authored note",
			body: "## Notes\n\n**2026-08-26T09:26:55Z · claude · q88f**\n\ndone\n",
			want: []Note{{ID: "q88f", At: "2026-08-26T09:26:55Z", Author: "claude", Text: "done"}},
		},
		{
			name: "a reply names its parent",
			body: "## Notes\n\n**2026-08-26T09:10:00Z · alexander · a1b2 · re:q88f**\n\nack\n",
			want: []Note{{ID: "a1b2", At: "2026-08-26T09:10:00Z", Author: "alexander", ParentID: "q88f", Text: "ack"}},
		},
		{
			name: "an edited note",
			body: "## Notes\n\n**2026-08-26T08:12:40Z · alexander · n3x0 · edited**\n\nrewritten\n",
			want: []Note{{ID: "n3x0", At: "2026-08-26T08:12:40Z", Author: "alexander", Edited: true, Text: "rewritten"}},
		},
		{
			name: "a note with no author keeps the id third",
			body: "## Notes\n\n**2026-08-26T08:12:40Z ·  · n3x0**\n\nno author\n",
			want: []Note{{ID: "n3x0", At: "2026-08-26T08:12:40Z", Text: "no author"}},
		},
		{
			name: "a byline with too few fields stays free-form",
			body: "## Notes\n\n**2026-08-08T10:00:00Z · alexander**\n\nhalf a byline\n",
			want: []Note{{ID: "#0", At: "2026-08-08T10:00:00Z · alexander", Text: "half a byline"}},
		},
		{
			name: "a third field that is not an id stays free-form",
			body: "## Notes\n\n**one · two · three**\n\nhand written\n",
			want: []Note{{ID: "#0", At: "one · two · three", Text: "hand written"}},
		},
		{
			name: "an unknown trailing flag stays free-form",
			body: "## Notes\n\n**2026-08-08T10:00:00Z · alexander · a1b2 · shouty**\n\nunknown flag\n",
			want: []Note{{ID: "#0", At: "2026-08-08T10:00:00Z · alexander · a1b2 · shouty", Text: "unknown flag"}},
		},
		{
			name: "hand-written text before any byline is one note",
			body: "## Notes\n\njust a loose line\n",
			want: []Note{{ID: "#0", Text: "just a loose line"}},
		},
		{
			name: "the section ends at the next heading",
			body: "## Notes\n\n**t1**\n\nkept\n\n## Other\n\n**t2**\n\ndropped\n",
			want: []Note{{ID: "#0", At: "t1", Text: "kept"}},
		},
		{
			name: "an empty section yields no notes",
			body: "## Notes\n",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseNotes(tt.body)
			require.Len(t, got, len(tt.want))
			for i := range got {
				got[i].start, got[i].end, got[i].hasByline = 0, 0, false
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAddNote(t *testing.T) {
	ts := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		body    string
		opts    AddNoteOptions
		wantSub string
	}{
		{
			name:    "no notes section creates one",
			body:    "# Title\n\nSome content.\n",
			opts:    AddNoteOptions{Text: "first note", Author: "alexander", At: ts},
			wantSub: "## Notes\n\n**2026-03-06T12:00:00Z · alexander · ",
		},
		{
			name:    "existing notes section appends",
			body:    "# Title\n\n## Notes\n\n**2026-03-06T11:00:00Z**\n\nold note\n",
			opts:    AddNoteOptions{Text: "new note", Author: "kontora", At: ts},
			wantSub: "old note\n\n**2026-03-06T12:00:00Z · kontora · ",
		},
		{
			name:    "empty body",
			body:    "",
			opts:    AddNoteOptions{Text: "note on empty", At: ts},
			wantSub: "## Notes\n\n**2026-03-06T12:00:00Z ·  · ",
		},
		{
			name:    "multiline note text",
			body:    "# Title\n",
			opts:    AddNoteOptions{Text: "line one\nline two", At: ts},
			wantSub: "line one\nline two\n",
		},
		{
			// The section, not the body, is what the note is appended to: past
			// the next heading it would be written into someone else's section
			// and ParseNotes would never see it again.
			name:    "notes section followed by another heading",
			body:    "# Title\n\n## Notes\n\n**2026-03-06T11:00:00Z**\n\nold note\n\n## Verification\n\nran the suite\n",
			opts:    AddNoteOptions{Text: "new note", Author: "alexander", At: ts},
			wantSub: "new note\n\n## Verification\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tkt := notesTicket(t, tt.body)
			note, err := tkt.AddNote(tt.opts)
			require.NoError(t, err)
			assert.Regexp(t, `^[a-z0-9]{4}$`, note.ID)
			assert.Contains(t, tkt.Body, tt.wantSub)
			assert.Equal(t, tkt.Body, tkt.rawBody, "Marshal writes rawBody")
			assert.Equal(t, note.ID, ParseNotes(tkt.Body)[len(ParseNotes(tkt.Body))-1].ID,
				"the note just written must read back as the last one in the section")
		})
	}
}

func TestAddNoteMultiple(t *testing.T) {
	tkt := notesTicket(t, "# Title\n")
	for _, text := range []string{"first", "second"} {
		_, err := tkt.AddNote(AddNoteOptions{Text: text, Author: "alexander"})
		require.NoError(t, err)
	}

	assert.Equal(t, 1, strings.Count(tkt.Body, "## Notes"))
	notes := ParseNotes(tkt.Body)
	require.Len(t, notes, 2)
	assert.Equal(t, "first", notes[0].Text)
	assert.Equal(t, "second", notes[1].Text)
	assert.NotEqual(t, notes[0].ID, notes[1].ID)
}

func TestAddNoteRejects(t *testing.T) {
	tests := []struct {
		name string
		opts AddNoteOptions
		want error
	}{
		{name: "empty text", opts: AddNoteOptions{Text: "  "}, want: ErrNoteEmpty},
		{name: "author holds the separator", opts: AddNoteOptions{Text: "x", Author: "a · b"}, want: ErrNoteAuthor},
		{name: "author holds a newline", opts: AddNoteOptions{Text: "x", Author: "a\nb"}, want: ErrNoteAuthor},
		{name: "author holds bold marks", opts: AddNoteOptions{Text: "x", Author: "**a**"}, want: ErrNoteAuthor},
		{name: "unknown parent", opts: AddNoteOptions{Text: "x", ParentID: "zzzz"}, want: ErrNoteNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tkt := notesTicket(t, "# Title\n")
			before := tkt.Body
			_, err := tkt.AddNote(tt.opts)
			assert.ErrorIs(t, err, tt.want)
			assert.Equal(t, before, tkt.Body, "a refused note leaves the body alone")
		})
	}
}

func TestAddNoteReply(t *testing.T) {
	tkt := notesTicket(t, "# Title\n")
	parent, err := tkt.AddNote(AddNoteOptions{Text: "question", Author: "alexander"})
	require.NoError(t, err)

	reply, err := tkt.AddNote(AddNoteOptions{Text: "ack", Author: "claude", ParentID: parent.ID})
	require.NoError(t, err)
	assert.Equal(t, parent.ID, reply.ParentID)
	assert.Contains(t, tkt.Body, "re:"+parent.ID)

	_, err = tkt.AddNote(AddNoteOptions{Text: "deeper", ParentID: reply.ID})
	assert.ErrorIs(t, err, ErrNoteNested)
}

func TestEditNote(t *testing.T) {
	tkt := notesTicket(t, "# Title\n")
	first, err := tkt.AddNote(AddNoteOptions{Text: "one", Author: "alexander"})
	require.NoError(t, err)
	_, err = tkt.AddNote(AddNoteOptions{Text: "two", Author: "claude"})
	require.NoError(t, err)
	untouched := strings.Split(tkt.Body, "\n")

	edited, err := tkt.EditNote(first.ID, "one, revised")
	require.NoError(t, err)
	assert.True(t, edited.Edited)

	notes := ParseNotes(tkt.Body)
	require.Len(t, notes, 2)
	assert.Equal(t, "one, revised", notes[0].Text)
	assert.True(t, notes[0].Edited)
	assert.Equal(t, "two", notes[1].Text)
	assert.Equal(t, untouched[len(untouched)-4:], strings.Split(tkt.Body, "\n")[len(strings.Split(tkt.Body, "\n"))-4:],
		"the second note's lines are untouched")

	_, err = tkt.EditNote("zzzz", "nope")
	assert.ErrorIs(t, err, ErrNoteNotFound)
}

func TestDeleteNoteRemovesReplies(t *testing.T) {
	tkt := notesTicket(t, "# Title\n")
	keep, err := tkt.AddNote(AddNoteOptions{Text: "keep me", Author: "alexander"})
	require.NoError(t, err)
	parent, err := tkt.AddNote(AddNoteOptions{Text: "parent", Author: "alexander"})
	require.NoError(t, err)
	for _, text := range []string{"reply one", "reply two"} {
		_, err := tkt.AddNote(AddNoteOptions{Text: text, Author: "claude", ParentID: parent.ID})
		require.NoError(t, err)
	}

	require.NoError(t, tkt.DeleteNote(parent.ID))
	notes := ParseNotes(tkt.Body)
	require.Len(t, notes, 1)
	assert.Equal(t, keep.ID, notes[0].ID)
	assert.NotContains(t, tkt.Body, "reply one")
	assert.NotContains(t, tkt.Body, "reply two")

	assert.ErrorIs(t, tkt.DeleteNote("zzzz"), ErrNoteNotFound)
}

func TestEnsureNoteIDMintsLazily(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "a legacy byline", body: "## Notes\n\n**2026-08-08T10:00:00Z**\n\nlegacy\n"},
		{name: "no byline at all", body: "## Notes\n\nlegacy\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tkt := notesTicket(t, tt.body)
			id, changed, err := tkt.EnsureNoteID("#0")
			require.NoError(t, err)
			assert.True(t, changed)
			assert.Regexp(t, `^[a-z0-9]{4}$`, id)

			notes := ParseNotes(tkt.Body)
			require.Len(t, notes, 1)
			assert.Equal(t, id, notes[0].ID)
			assert.Equal(t, "legacy", notes[0].Text)

			again, changed, err := tkt.EnsureNoteID(id)
			require.NoError(t, err)
			assert.False(t, changed, "a note that has an id is not rewritten")
			assert.Equal(t, id, again)
		})
	}
}

func TestEnsureNoteIDRefusesUnaddressableByline(t *testing.T) {
	tkt := notesTicket(t, "## Notes\n\n**one · two · three**\n\nhand written\n")
	_, _, err := tkt.EnsureNoteID("#0")
	assert.ErrorIs(t, err, ErrNoteUnaddressable)
}

func TestAddNoteRoundTrip(t *testing.T) {
	tkt := notesTicket(t, "# Title\n\nBody text.\n")
	note, err := tkt.AddNote(AddNoteOptions{Text: "round trip test", Author: "alexander"})
	require.NoError(t, err)

	out, err := tkt.Marshal()
	require.NoError(t, err)
	reparsed, err := ParseBytes(out)
	require.NoError(t, err)

	assert.Contains(t, reparsed.Body, "Body text.")
	notes := ParseNotes(reparsed.Body)
	require.Len(t, notes, 1)
	assert.Equal(t, note.ID, notes[0].ID)
	assert.Equal(t, "alexander", notes[0].Author)
	assert.Equal(t, "round trip test", notes[0].Text)
}
