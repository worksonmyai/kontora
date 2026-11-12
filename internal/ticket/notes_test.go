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
			body: "## Notes\n\n**2026-08-08T10:00:00Z · two · three**\n\nhand written\n",
			want: []Note{{ID: "#0", At: "2026-08-08T10:00:00Z · two · three", Text: "hand written"}},
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
			body: "## Notes\n\n**2026-08-08**\n\nkept\n\n## Other\n\n**2026-08-09**\n\ndropped\n",
			want: []Note{{ID: "#0", At: "2026-08-08", Text: "kept"}},
		},
		{
			name: "fenced shell comment",
			body: "## Notes\n\n**2026-08-08**\n\n```sh\n# run the suite\n```\n",
			want: []Note{{ID: "#0", At: "2026-08-08", Text: "```sh\n# run the suite\n```"}},
		},
		{
			name: "fenced byline",
			body: "## Notes\n\n**2026-08-08**\n\n~~~\n**2026-08-09**\n## heading\n~~~\n",
			want: []Note{{ID: "#0", At: "2026-08-08", Text: "~~~\n**2026-08-09**\n## heading\n~~~"}},
		},
		{
			name: "bold subheading",
			body: "## Notes\n\n**2026-11-07T20:00:00Z · pi · ab12**\n\n**Verification:**\n- go test passed\n",
			want: []Note{{ID: "ab12", At: "2026-11-07T20:00:00Z", Author: "pi", Text: "**Verification:**\n- go test passed"}},
		},
		{
			name: "non-date text with invalid grammar stays in the note",
			body: "## Notes\n\n**2026-08-08**\n\n**one · two · three**\n\ntext\n",
			want: []Note{{ID: "#0", At: "2026-08-08", Text: "**one · two · three**\n\ntext"}},
		},
		{
			name: "non-date full grammar",
			body: "## Notes\n\n**hand-written · author · ab12**\n\ntext\n",
			want: []Note{{ID: "ab12", At: "hand-written", Author: "author", Text: "text"}},
		},
		{
			name: "minted empty timestamp",
			body: "## Notes\n\n**2026-08-08T10:00:00Z**\n\nlegacy\n\n** ·  · cd34**\n\nminted\n",
			want: []Note{{ID: "#0", At: "2026-08-08T10:00:00Z", Text: "legacy"}, {ID: "cd34", Text: "minted"}},
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
		name     string
		body     string
		opts     AddNoteOptions
		wantSub  string
		wantText string
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
			name:    "existing open backtick fence",
			body:    "## Notes\n\n**2026-08-08**\n\n```sh\necho prior\n",
			opts:    AddNoteOptions{Text: "new note", At: ts},
			wantSub: "echo prior\n\n```\n\n**2026-03-06T12:00:00Z",
		},
		{
			name:    "existing open list fence",
			body:    "## Notes\n\n**2026-08-08**\n\n- Example\n\n  ~~~sh\n  echo prior\n",
			opts:    AddNoteOptions{Text: "new note", At: ts},
			wantSub: "echo prior\n\n  ~~~\n\n**2026-03-06T12:00:00Z",
		},
		{
			name:    "list fence after lazy paragraph continuation",
			body:    "## Notes\n\n**2026-08-08**\n\n- Example\ncontinued text\n\n  ~~~md\n  literal\n\n## Other\noutside\n",
			opts:    AddNoteOptions{Text: "new note", At: ts},
			wantSub: "new note\n\n## Other\noutside\n",
		},
		{
			name:    "list fence after overpadded marker",
			body:    "## Notes\n\n**2026-08-08**\n\n-     indented code\n\n  ~~~md\n  literal\n\n## Other\noutside\n",
			opts:    AddNoteOptions{Text: "new note", At: ts},
			wantSub: "new note\n\n## Other\noutside\n",
		},
		{
			name:    "empty notes section followed by another heading",
			body:    "# Title\n\n## Notes\n## Verification\n\nran the suite\n",
			opts:    AddNoteOptions{Text: "line one\nline two", At: ts},
			wantSub: "line one\nline two\n\n## Verification\n\nran the suite\n",
		},
		{
			name:    "empty body",
			body:    "",
			opts:    AddNoteOptions{Text: "note on empty", At: ts},
			wantSub: "## Notes\n\n**2026-03-06T12:00:00Z ·  · ",
		},
		{
			name:     "h2 review report",
			opts:     AddNoteOptions{Text: "review: 1 issue\n\n## Code review\n\n### Finding\n\ntext"},
			wantText: "review: 1 issue\n\n### Code review\n\n#### Finding\n\ntext",
		},
		{
			name:     "fenced shell comment unchanged",
			opts:     AddNoteOptions{Text: "```sh\n# run the suite\n```"},
			wantText: "```sh\n# run the suite\n```",
		},
		{
			name:     "fenced bytes preserved with heading shift",
			opts:     AddNoteOptions{Text: "## Report\n\n  ````sh\r\n# run the suite\r\n**2026-08-08**\r\n~~~\r\n```\r\n  `````  \r\n\n## Result"},
			wantText: "### Report\n\n  ````sh\r\n# run the suite\r\n**2026-08-08**\r\n~~~\r\n```\r\n  `````  \r\n\n### Result",
		},
		{
			name:     "list-prefixed fence unchanged",
			opts:     AddNoteOptions{Text: "Example:\n\n- ```md\n  ## Literal heading\n  **2026-08-08**\n  ```"},
			wantText: "Example:\n\n- ```md\n  ## Literal heading\n  **2026-08-08**\n  ```",
		},
		{
			name:     "numbered list indented fence unchanged",
			opts:     AddNoteOptions{Text: "1. Example\n\n    ```md\n    ## Literal heading\n    **2026-08-08**\n    ```"},
			wantText: "1. Example\n\n    ```md\n    ## Literal heading\n    **2026-08-08**\n    ```",
		},
		{
			name:     "close list-contained fence with indentation",
			opts:     AddNoteOptions{Text: "- Example\n\n  ```sh\n  echo hi"},
			wantText: "- Example\n\n  ```sh\n  echo hi\n  ```",
		},
		{
			name:     "close list-prefixed fence with indentation",
			opts:     AddNoteOptions{Text: "- ~~~~sh\n  echo hi"},
			wantText: "- ~~~~sh\n  echo hi\n  ~~~~",
		},
		{
			name:     "nested list fence unchanged",
			opts:     AddNoteOptions{Text: "- Example\n  1. ~~~md\n     ## Literal heading\n     **2026-08-08**\n     ~~~"},
			wantText: "- Example\n  1. ~~~md\n     ## Literal heading\n     **2026-08-08**\n     ~~~",
		},
		{
			name:     "list fence after lazy continuation unchanged",
			opts:     AddNoteOptions{Text: "1. Example\ncontinued text\n\n    ```md\n    **2026-08-08**\n    ```"},
			wantText: "1. Example\ncontinued text\n\n    ```md\n    **2026-08-08**\n    ```",
		},
		{
			name:     "list fence after indented code unchanged",
			opts:     AddNoteOptions{Text: "1.     indented code\n\n    ```md\n    **2026-08-08**\n    ```"},
			wantText: "1.     indented code\n\n    ```md\n    **2026-08-08**\n    ```",
		},
		{
			name:     "tab-indented list fence unchanged",
			opts:     AddNoteOptions{Text: "1. Example\n\n\t```md\n\t**2026-08-08**\n\t```"},
			wantText: "1. Example\n\n\t```md\n\t**2026-08-08**\n\t```",
		},
		{
			name:     "dedent ends list fence",
			opts:     AddNoteOptions{Text: "- ```md\n  ## Literal heading\n\n## Result"},
			wantText: "- ```md\n  ## Literal heading\n\n### Result",
		},
		{
			name:     "html block ends list before top-level fence",
			opts:     AddNoteOptions{Text: "- Example\n<div>\n\n  ```md\n## Literal heading\n**2026-08-08**\n  ```"},
			wantText: "- Example\n<div>\n\n  ```md\n## Literal heading\n**2026-08-08**\n  ```",
		},
		{
			name:     "four-space marker padding prevents lazy continuation",
			opts:     AddNoteOptions{Text: "-    Example\ncontinued text\n\n     ```md\n     literal"},
			wantText: "-    Example\ncontinued text\n\n     ```md\n     literal",
		},
		{
			name:     "four spaces outside list do not open fence",
			opts:     AddNoteOptions{Text: "Example:\n\n    ```md\n## Result"},
			wantText: "Example:\n\n    ```md\n### Result",
		},
		{
			name:     "h3 unchanged",
			opts:     AddNoteOptions{Text: "  ### Verification\n\ntext  "},
			wantText: "### Verification\n\ntext",
		},
		{
			name:     "h4 unchanged",
			opts:     AddNoteOptions{Text: "#### Verification\n\ntext"},
			wantText: "#### Verification\n\ntext",
		},
		{
			name:     "heading depth capped",
			opts:     AddNoteOptions{Text: "# Review\n###### Finding"},
			wantText: "### Review\n###### Finding",
		},
		{
			name:     "close open backtick fence",
			opts:     AddNoteOptions{Text: "````sh\n# run the suite\n```"},
			wantText: "````sh\n# run the suite\n```\n````",
		},
		{
			name:     "close open tilde fence",
			opts:     AddNoteOptions{Text: "~~~sh\n# run the suite"},
			wantText: "~~~sh\n# run the suite\n~~~",
		},
		{
			name:     "escape byline text only outside fence",
			opts:     AddNoteOptions{Text: "**2026-08-08**\n** ·  · cd34**\n**Verification:**\n```\n**2026-08-08**\n```"},
			wantText: "__2026-08-08__\n__ ·  · cd34__\n**Verification:**\n```\n**2026-08-08**\n```",
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
			oldCount := len(ParseNotes(tkt.Body))
			note, err := tkt.AddNote(tt.opts)
			require.NoError(t, err)
			assert.Regexp(t, `^[a-z0-9]{4}$`, note.ID)
			assert.Contains(t, tkt.Body, tt.wantSub)
			assert.Equal(t, tkt.Body, tkt.rawBody, "Marshal writes rawBody")
			before := ParseNotes(tkt.Body)
			require.Len(t, before, oldCount+1)
			assert.Equal(t, note.ID, before[len(before)-1].ID)
			assert.True(t, strings.HasPrefix(NotesTail(tkt.Body, 1), formatNoteByline(note)))
			id, changed, err := tkt.EnsureNoteID(note.ID)
			require.NoError(t, err)
			assert.Equal(t, note.ID, id)
			assert.False(t, changed)
			assert.Equal(t, note.Text, before[len(before)-1].Text)
			if tt.wantText != "" {
				assert.Equal(t, tt.wantText, note.Text)
			}
			_, err = tkt.AddNote(AddNoteOptions{Text: "next note"})
			require.NoError(t, err)
			after := ParseNotes(tkt.Body)
			require.Len(t, after, len(before)+1)
			assert.Equal(t, note.Text, after[len(before)-1].Text)
			assert.Equal(t, "next note", after[len(before)].Text)
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
	for _, tt := range []struct {
		name, text, want string
	}{
		{name: "plain", text: "one, revised", want: "one, revised"},
		{name: "headings", text: "## Review\n### Finding", want: "### Review\n#### Finding"},
		{name: "h3", text: "### Review", want: "### Review"},
		{name: "open fence", text: "~~~\n## Code", want: "~~~\n## Code\n~~~"},
		{name: "byline", text: "**2026-08-08**", want: "__2026-08-08__"},
		{name: "list fence", text: "- ```md\n  ## Literal heading\n  **2026-08-08**\n  ```", want: "- ```md\n  ## Literal heading\n  **2026-08-08**\n  ```"},
		{name: "open list fence", text: "- Example\n\n  ```sh\n  echo hi", want: "- Example\n\n  ```sh\n  echo hi\n  ```"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tkt := notesTicket(t, "# Title\n")
			first, err := tkt.AddNote(AddNoteOptions{Text: "one", Author: "alexander"})
			require.NoError(t, err)
			_, err = tkt.AddNote(AddNoteOptions{Text: "two", Author: "claude"})
			require.NoError(t, err)
			untouched := strings.Split(tkt.Body, "\n")

			edited, err := tkt.EditNote(first.ID, tt.text)
			require.NoError(t, err)
			assert.True(t, edited.Edited)

			notes := ParseNotes(tkt.Body)
			require.Len(t, notes, 2)
			assert.Equal(t, tt.want, notes[0].Text)
			assert.Equal(t, tt.want, edited.Text)
			assert.True(t, notes[0].Edited)
			assert.Equal(t, "two", notes[1].Text)
			assert.Equal(t, untouched[len(untouched)-4:], strings.Split(tkt.Body, "\n")[len(strings.Split(tkt.Body, "\n"))-4:],
				"the second note's lines are untouched")

			_, err = tkt.EditNote("zzzz", "nope")
			assert.ErrorIs(t, err, ErrNoteNotFound)
		})
	}
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
	tkt := notesTicket(t, "## Notes\n\n**2026-08-08T10:00:00Z · a · b · c**\n\nhand written\n")
	_, _, err := tkt.EnsureNoteID("#0")
	assert.ErrorIs(t, err, ErrNoteUnaddressable)
}

func TestNotesTail(t *testing.T) {
	const section = "## Notes\n\n**2026-08-08**\n\none\n\n**2026-08-09**\n\ntwo\n\n**2026-08-10**\n\nthree\n"
	const lastTwo = "**2026-08-09**\n\ntwo\n\n**2026-08-10**\n\nthree\n"
	const later = "\n## Code review\n\nfinding\n"
	for _, tt := range []struct {
		name, body string
		n          int
		want       string
	}{
		{name: "no section", body: "# Title\n", n: 2},
		{name: "whole section", body: "# Title\n" + section, want: section},
		{name: "last two", body: section, n: 2, want: lastTwo},
		{name: "exact count", body: section, n: 3, want: strings.TrimPrefix(section, "## Notes\n\n")},
		{name: "above count", body: section, n: 4, want: section},
		{name: "later section", body: section + later, n: 2, want: lastTwo + later},
		{name: "empty section", body: "## Notes\n", n: 2, want: "## Notes\n"},
		{name: "loose note", body: "## Notes\n\nloose\n", n: 1, want: "\nloose\n"},
		{name: "raw bytes", body: "# Title\r\n## Notes\r\n\r\n**2026-08-08**\r\n\r\none\r\n", n: 1, want: "**2026-08-08**\r\n\r\none\r\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NotesTail(tt.body, tt.n))
		})
	}
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
