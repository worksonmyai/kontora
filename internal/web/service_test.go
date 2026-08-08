package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func TestTicketInfoFromView_HistoryRun(t *testing.T) {
	info := TicketInfoFromView(app.View{
		ID: "t1",
		History: []app.HistoryView{
			{Stage: "review", Run: 0},
			{Stage: "review", Run: 1},
		},
	})
	require.Len(t, info.History, 2)
	assert.Equal(t, 0, info.History[0].Run)
	assert.Equal(t, 1, info.History[1].Run)
}

func TestTicketInfoFromView_ClaimedBy(t *testing.T) {
	info := TicketInfoFromView(app.View{ID: "t1", ClaimedBy: "alpha"})
	assert.Equal(t, "alpha", info.ClaimedBy)

	// An empty claim stays empty (and is omitted from JSON via omitempty).
	assert.Empty(t, TicketInfoFromView(app.View{ID: "t2"}).ClaimedBy)
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
			name: "notes written through AddNote",
			body: "# Title\n\nDescription stays.\n\n## Notes\n\n**2026-08-08T10:00:00Z**\n\nInvestigate timeout\n\n**2026-08-08T10:05:00Z**\n\nRetry with debug logging\n",
			want: []NoteInfo{
				{At: "2026-08-08T10:00:00Z", Text: "Investigate timeout"},
				{At: "2026-08-08T10:05:00Z", Text: "Retry with debug logging"},
			},
		},
		{
			name: "a multiline note keeps its blank lines",
			body: "## Notes\n\n**t1**\n\nfirst line\n\nsecond paragraph\n",
			want: []NoteInfo{{At: "t1", Text: "first line\n\nsecond paragraph"}},
		},
		{
			name: "hand-written text before any byline is one note",
			body: "## Notes\n\njust a loose line\n",
			want: []NoteInfo{{Text: "just a loose line"}},
		},
		{
			name: "the section ends at the next heading",
			body: "## Notes\n\n**t1**\n\nkept\n\n## Other\n\n**t2**\n\ndropped\n",
			want: []NoteInfo{{At: "t1", Text: "kept"}},
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
