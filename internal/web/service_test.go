package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func TestTicketInfoFromView_HistoryRun(t *testing.T) {
	info := TicketInfoFromView(app.View{
		ID: "t1",
		History: []app.HistoryView{
			{Stage: "review", Run: 0},
			{Stage: "review", Run: 1, Model: "haiku", Effort: "low"},
		},
	})
	require.Len(t, info.History, 2)
	assert.Equal(t, 0, info.History[0].Run)
	assert.Equal(t, 1, info.History[1].Run)
	assert.Equal(t, "haiku", info.History[1].Model)
	assert.Equal(t, "low", info.History[1].Effort)

	// A run that passed no flag sends no key, so the browser can tell "none
	// passed" from "passed an empty value".
	encoded, err := json.Marshal(info.History[0])
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "model")
	assert.NotContains(t, string(encoded), "effort")
}

func TestTicketInfoFromView_ClaimedBy(t *testing.T) {
	info := TicketInfoFromView(app.View{ID: "t1", ClaimedBy: "alpha"})
	assert.Equal(t, "alpha", info.ClaimedBy)

	// An empty claim stays empty (and is omitted from JSON via omitempty).
	assert.Empty(t, TicketInfoFromView(app.View{ID: "t2"}).ClaimedBy)
}

// TestTicketInfoFromView_FinalSummary: the ticket-level summary rides the
// detailed payload the detail endpoint and SSE ticket updates share, and is
// omitted from JSON when a view carries none.
func TestTicketInfoFromView_FinalSummary(t *testing.T) {
	info := TicketInfoFromView(app.View{ID: "t1", Summary: "the last run", FinalSummary: "the whole ticket"})
	assert.Equal(t, "the whole ticket", info.FinalSummary)
	assert.Equal(t, "the last run", info.Summary)

	encoded, err := json.Marshal(info)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"final_summary":"the whole ticket"`)

	// A board list view carries no detail fields, so the key is absent.
	list, err := json.Marshal(TicketInfoFromView(app.View{ID: "t2"}))
	require.NoError(t, err)
	assert.NotContains(t, string(list), "final_summary")
}

// TestTicketInfoFromView_FinishedAt: the finish time crosses into the payload
// the board reads, and a ticket that never finished sends no key at all rather
// than a null the sort would have to handle.
func TestTicketInfoFromView_FinishedAt(t *testing.T) {
	finished := time.Date(2026, 4, 20, 11, 30, 0, 0, time.UTC)
	info := TicketInfoFromView(app.View{ID: "t1", FinishedAt: &finished})
	require.NotNil(t, info.FinishedAt)
	assert.Equal(t, finished, *info.FinishedAt)

	encoded, err := json.Marshal(info)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"finished_at":"2026-04-20T11:30:00Z"`)

	never, err := json.Marshal(TicketInfoFromView(app.View{ID: "t2"}))
	require.NoError(t, err)
	assert.NotContains(t, string(never), "finished_at")
}

// TestTicketInfoFromView_Relations: a View sees one ticket, so it hands over
// relation ids and nothing else. Titles and statuses are added by the daemon,
// which holds the store.
func TestTicketInfoFromView_Relations(t *testing.T) {
	info := TicketInfoFromView(app.View{
		ID:     "t1",
		Deps:   []string{"t2", "", "t3"},
		Links:  []string{"t4"},
		Parent: "t5",
	})
	// The empty id is dropped: a "deps: [t2, ]" line must not render a chip with
	// no ticket behind it.
	assert.Equal(t, []TicketRef{{ID: "t2"}, {ID: "t3"}}, info.Deps)
	assert.Equal(t, []TicketRef{{ID: "t4"}}, info.Links)
	require.NotNil(t, info.Parent)
	assert.Equal(t, TicketRef{ID: "t5"}, *info.Parent)
	// Both reverse edges are derived from the whole store, so neither is set here.
	assert.Nil(t, info.Blocks)
	assert.Nil(t, info.Children)

	// A ticket with no relations carries none of the keys.
	encoded, err := json.Marshal(TicketInfoFromView(app.View{ID: "t6"}))
	require.NoError(t, err)
	for _, key := range []string{"deps", "links", "parent", "blocks", "children"} {
		assert.NotContains(t, string(encoded), `"`+key+`"`)
	}
}

func TestParseNotes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []NoteInfo
	}{
		{
			name: "no notes section",
			body: "# Title\n\nSome description.\n",
			want: nil,
		},
		{
			name: "a legacy byline carries only a timestamp",
			body: "# Title\n\nDescription stays.\n\n## Notes\n\n**2026-08-08T10:00:00Z**\n\nInvestigate timeout\n\n**2026-08-08T10:05:00Z**\n\nRetry with debug logging\n",
			want: []NoteInfo{
				{ID: "#0", At: "2026-08-08T10:00:00Z", Text: "Investigate timeout"},
				{ID: "#1", At: "2026-08-08T10:05:00Z", Text: "Retry with debug logging"},
			},
		},
		{
			name: "an authored note carries its id and author",
			body: "## Notes\n\n**2026-08-26T09:26:55Z · claude · q88f**\n\ndone\n",
			want: []NoteInfo{{ID: "q88f", At: "2026-08-26T09:26:55Z", Author: "claude", Text: "done"}},
		},
		{
			name: "a reply and an edit carry their flags",
			body: "## Notes\n\n**2026-08-26T09:10:00Z · alexander · a1b2 · re:q88f · edited**\n\nack\n",
			want: []NoteInfo{{ID: "a1b2", At: "2026-08-26T09:10:00Z", Author: "alexander", ParentID: "q88f", Edited: true, Text: "ack"}},
		},
		{
			name: "a multiline note keeps its blank lines",
			body: "## Notes\n\n**t1**\n\nfirst line\n\nsecond paragraph\n",
			want: []NoteInfo{{ID: "#0", At: "t1", Text: "first line\n\nsecond paragraph"}},
		},
		{
			name: "hand-written text before any byline is one note",
			body: "## Notes\n\njust a loose line\n",
			want: []NoteInfo{{ID: "#0", Text: "just a loose line"}},
		},
		{
			name: "the section ends at the next heading",
			body: "## Notes\n\n**t1**\n\nkept\n\n## Other\n\n**t2**\n\ndropped\n",
			want: []NoteInfo{{ID: "#0", At: "t1", Text: "kept"}},
		},
		{
			name: "an empty section yields no notes",
			body: "## Notes\n",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ParseNotes(tc.body))
		})
	}
}

func TestTicketInfoFromView_Notes(t *testing.T) {
	body := "# Title\n\nDescription stays.\n\n## Notes\n\n**2026-08-08T10:00:00Z**\n\nInvestigate timeout\n"
	info := TicketInfoFromView(app.View{ID: "t1", Body: body})

	require.Len(t, info.Notes, 1)
	assert.Equal(t, "Investigate timeout", info.Notes[0].Text)
	assert.Equal(t, body, info.Body, "parsing notes must not rewrite the body")
}
