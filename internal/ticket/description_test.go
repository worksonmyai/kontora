package ticket

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitDescription(t *testing.T) {
	for _, tc := range []struct {
		name, body, description, tail string
	}{
		{name: "empty"},
		{name: "no notes", body: "# Title\n\nText  \n", description: "# Title\n\nText  \n"},
		{name: "other headings", body: "## Other\n## notes\n### Notes\n## Notes extra\n", description: "## Other\n## notes\n### Notes\n## Notes extra\n"},
		{name: "whole tail", body: "# Title\n\n## Notes\nold\n## Later\nkeep\n## Notes\nmore", description: "# Title\n\n", tail: "## Notes\nold\n## Later\nkeep\n## Notes\nmore"},
		{name: "heading only", body: "## Notes", tail: "## Notes"},
		{name: "whitespace and CRLF", body: "前文  \r\n\r\n \t## Notes \t\r\nraw\r\n## Later\r\n", description: "前文  \r\n\r\n", tail: " \t## Notes \t\r\nraw\r\n## Later\r\n"},
		{name: "fence matches notes parser", body: "```\n## Notes\n```", description: "```\n", tail: "## Notes\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			description, tail := SplitDescription(tc.body)
			assert.Equal(t, tc.description, description)
			assert.Equal(t, tc.tail, tail)
			assert.Equal(t, tc.body, description+tail)
			linesStart, _ := notesSection(strings.Split(tc.body, "\n"))
			assert.Equal(t, linesStart >= 0, tail != "")
			tkt, err := ParseBytes([]byte("---\nid: tst-desc\nstatus: open\n---\n" + tc.body))
			require.NoError(t, err)
			before, err := tkt.Marshal()
			require.NoError(t, err)
			require.NoError(t, tkt.SetDescription(description))
			after, err := tkt.Marshal()
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestSetDescription(t *testing.T) {
	const tail = " \t## Notes\r\n\n**legacy**\n\nraw  \n## Later\nkeep\n## Notes\nsecond"
	for _, tc := range []struct {
		name, body, description, want string
		invalid                       bool
	}{
		{name: "add separator", body: "old\n\n" + tail, description: "new", want: "new\n" + tail},
		{name: "keep separators", body: "old\n" + tail, description: "new\r\n\r\n", want: "new\r\n\r\n" + tail},
		{name: "clear prefix", body: "old\n" + tail, want: tail},
		{name: "replace without tail", body: "old", description: "new", want: "new"},
		{name: "clear without tail", body: "old"},
		{name: "heading at start", body: tail, description: "new", want: "new\n" + tail},
		{name: "invalid heading", body: "old\n" + tail, description: "new\n## Notes\nattack", invalid: true},
		{name: "invalid padded heading", body: "old", description: " \t## Notes \r\n", invalid: true},
		{name: "invalid at EOF", body: "old", description: "## Notes", invalid: true},
		{name: "other heading allowed", body: "old\n" + tail, description: "## Other\n## notes", want: "## Other\n## notes\n" + tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tkt, err := ParseBytes([]byte("---\nid: tst-desc\nstatus: open\n---\n" + tc.body))
			require.NoError(t, err)
			err = tkt.SetDescription(tc.description)
			if tc.invalid {
				require.Error(t, ValidateDescription(tc.description))
				require.Error(t, err)
				assert.Equal(t, tc.body, tkt.Body)
				return
			}
			require.NoError(t, ValidateDescription(tc.description))
			require.NoError(t, err)
			assert.Equal(t, tc.want, tkt.Body)
			data, err := tkt.Marshal()
			require.NoError(t, err)
			roundTrip, err := ParseBytes(data)
			require.NoError(t, err)
			assert.Equal(t, tc.want, roundTrip.Body)
		})
	}
}
