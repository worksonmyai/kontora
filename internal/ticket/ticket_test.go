package ticket

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBasic(t *testing.T) {
	tkt, err := ParseFile("testdata/basic.md")
	require.NoError(t, err)

	assert.Equal(t, "kon-q88f", tkt.ID)
	assert.Equal(t, StatusOpen, tkt.Status)
	assert.Equal(t, "default", tkt.Pipeline)
	assert.Equal(t, "~/projects/kontora", tkt.Path)
	assert.Equal(t, "code", tkt.Stage)
	assert.Equal(t, 1, tkt.Attempt)
	require.NotNil(t, tkt.StartedAt)
	assert.Equal(t, "kon-q88f-work", tkt.Branch)
	assert.Equal(t, "testdata/basic.md", tkt.FilePath)
}

func TestParseMinimal(t *testing.T) {
	tkt, err := ParseFile("testdata/minimal.md")
	require.NoError(t, err)

	assert.Equal(t, "min-001", tkt.ID)
	assert.Equal(t, "", tkt.Pipeline)
	assert.Equal(t, "", tkt.Path)
	assert.Nil(t, tkt.StartedAt)
}

func TestUnknownFieldRoundTrip(t *testing.T) {
	tkt, err := ParseFile("testdata/unknown_fields.md")
	require.NoError(t, err)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	// Re-parse and verify unknown fields survive
	tkt2, err := ParseBytes(out)
	require.NoError(t, err)

	assert.Equal(t, "unk-001", tkt2.ID)

	// Verify unknown fields are in the output
	outStr := string(out)
	assert.Contains(t, outStr, "custom_field: hello world")
	assert.Contains(t, outStr, "another_custom: 42")
	assert.Contains(t, outStr, "nested_custom")
}

func TestFieldOrderPreservation(t *testing.T) {
	tkt, err := ParseFile("testdata/basic.md")
	require.NoError(t, err)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	// Verify id comes before status in output
	outStr := string(out)
	idIdx := strings.Index(outStr, "id:")
	statusIdx := strings.Index(outStr, "status:")
	assert.Less(t, idIdx, statusIdx, "field order not preserved")
}

func TestBodyByteIdentity(t *testing.T) {
	original, err := ParseFile("testdata/basic.md")
	require.NoError(t, err)

	out, err := original.Marshal()
	require.NoError(t, err)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)

	assert.Equal(t, original.Body, reparsed.Body)
}

func TestTimestampUTC(t *testing.T) {
	tkt, err := ParseFile("testdata/basic.md")
	require.NoError(t, err)

	expected := time.Date(2026, 2, 25, 19, 39, 45, 0, time.UTC)
	require.NotNil(t, tkt.Created)
	assert.True(t, tkt.Created.Equal(expected), "Created = %v, want %v", tkt.Created, expected)
}

func TestTimestampOffset(t *testing.T) {
	tkt, err := ParseFile("testdata/timestamp_offset.md")
	require.NoError(t, err)

	require.NotNil(t, tkt.Created)
	// 2026-03-01T10:00:00.123456+01:00
	loc := time.FixedZone("+01:00", 3600)
	expected := time.Date(2026, 3, 1, 10, 0, 0, 123456000, loc)
	assert.True(t, tkt.Created.Equal(expected), "Created = %v, want %v", tkt.Created, expected)

	require.NotNil(t, tkt.StartedAt)
}

func TestStatuses(t *testing.T) {
	tests := []struct {
		status Status
	}{
		{StatusOpen},
		{StatusTodo},
		{StatusInProgress},
		{StatusPaused},
		{StatusDone},
		{StatusCancelled},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			input := "---\nid: test\nstatus: " + string(tt.status) + "\ncreated: 2026-01-01T00:00:00Z\n---\n# Test\n"
			tkt, err := ParseBytes([]byte(input))
			require.NoError(t, err)
			assert.Equal(t, tt.status, tkt.Status)
		})
	}
}

func TestStatusAllowsUnknownValues(t *testing.T) {
	for _, status := range []string{"running", "closed", "failed", "custom"} {
		t.Run(status, func(t *testing.T) {
			input := "---\nid: test\nstatus: " + status + "\ncreated: 2026-01-01T00:00:00Z\n---\n# Test\n"
			task, err := ParseBytes([]byte(input))
			require.NoError(t, err)
			assert.Equal(t, Status(status), task.Status)
		})
	}
}

func TestNoFrontmatter(t *testing.T) {
	_, err := ParseBytes([]byte("# Just a heading\n\nNo frontmatter here.\n"))
	require.Error(t, err)
}

func TestEmptyBody(t *testing.T) {
	tkt, err := ParseFile("testdata/empty_body.md")
	require.NoError(t, err)
	assert.Equal(t, "", tkt.Body)
}

func TestDashesInBody(t *testing.T) {
	tkt, err := ParseFile("testdata/dashes_in_body.md")
	require.NoError(t, err)

	assert.Equal(t, "dash-001", tkt.ID)
	assert.Contains(t, tkt.Body, "horizontal rule")
	assert.Equal(t, 2, strings.Count(tkt.Body, "---"))
}

func TestSetFieldExisting(t *testing.T) {
	tkt, err := ParseFile("testdata/basic.md")
	require.NoError(t, err)

	require.NoError(t, tkt.SetField("status", "done"))

	// Typed field should be updated immediately without re-parsing
	assert.Equal(t, StatusDone, tkt.Status)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	assert.Equal(t, StatusDone, reparsed.Status)
}

func TestSetFieldNew(t *testing.T) {
	tkt, err := ParseFile("testdata/minimal.md")
	require.NoError(t, err)

	require.NoError(t, tkt.SetField("pipeline", "quick"))

	out, err := tkt.Marshal()
	require.NoError(t, err)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	assert.Equal(t, "quick", reparsed.Pipeline)
}

func TestSetFieldPreservesOthers(t *testing.T) {
	tkt, err := ParseFile("testdata/unknown_fields.md")
	require.NoError(t, err)

	require.NoError(t, tkt.SetField("status", "paused"))

	out, err := tkt.Marshal()
	require.NoError(t, err)

	outStr := string(out)
	assert.Contains(t, outStr, "custom_field: hello world")
	assert.Contains(t, outStr, "another_custom: 42")
}

func TestMarshalWithHistory(t *testing.T) {
	tkt, err := ParseFile("testdata/history.md")
	require.NoError(t, err)

	require.Len(t, tkt.History, 2)
	assert.Equal(t, "plan", tkt.History[0].Stage)
	assert.Equal(t, 1, tkt.History[1].ExitCode)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	assert.Contains(t, string(out), "history:")
}

func TestTitle(t *testing.T) {
	tkt, err := ParseFile("testdata/basic.md")
	require.NoError(t, err)

	assert.Equal(t, "Fix the search index", tkt.Title())
}

func TestTitleEmpty(t *testing.T) {
	tkt, err := ParseFile("testdata/empty_body.md")
	require.NoError(t, err)
	assert.Equal(t, "", tkt.Title())
}

func TestEmptyDepsRoundTrip(t *testing.T) {
	tkt, err := ParseFile("testdata/empty_deps.md")
	require.NoError(t, err)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	assert.Contains(t, string(out), "deps: []")
}

func TestAgentFieldRoundTrip(t *testing.T) {
	input := "---\nid: test\nstatus: open\nagent: opus\ncreated: 2026-01-01T00:00:00Z\n---\n# Test\n"
	tkt, err := ParseBytes([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "opus", tkt.Agent)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	assert.Equal(t, "opus", reparsed.Agent)
}

func TestSetFieldAgent(t *testing.T) {
	input := "---\nid: test\nstatus: open\ncreated: 2026-01-01T00:00:00Z\n---\n# Test\n"
	tkt, err := ParseBytes([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "", tkt.Agent)

	require.NoError(t, tkt.SetField("agent", "sonnet"))
	assert.Equal(t, "sonnet", tkt.Agent)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	assert.Equal(t, "sonnet", reparsed.Agent)
}

func TestAppendNote(t *testing.T) {
	ts := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		body    string
		text    string
		wantSub string
	}{
		{
			name:    "no notes section creates one",
			body:    "# Title\n\nSome content.\n",
			text:    "first note",
			wantSub: "## Notes\n\n**2026-03-06T12:00:00Z**\n\nfirst note\n",
		},
		{
			name:    "existing notes section appends",
			body:    "# Title\n\n## Notes\n\n**2026-03-06T11:00:00Z**\n\nold note\n",
			text:    "new note",
			wantSub: "old note\n\n**2026-03-06T12:00:00Z**\n\nnew note\n",
		},
		{
			name:    "empty body",
			body:    "",
			text:    "note on empty",
			wantSub: "## Notes\n\n**2026-03-06T12:00:00Z**\n\nnote on empty\n",
		},
		{
			name:    "multiline note text",
			body:    "# Title\n",
			text:    "line one\nline two",
			wantSub: "line one\nline two\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "---\nid: test\nstatus: open\ncreated: 2026-01-01T00:00:00Z\n---\n" + tt.body
			tkt, err := ParseBytes([]byte(input))
			require.NoError(t, err)

			tkt.AppendNote(tt.text, ts)

			assert.Contains(t, tkt.Body, tt.wantSub)
		})
	}
}

func TestAppendNoteMultiple(t *testing.T) {
	input := "---\nid: test\nstatus: open\ncreated: 2026-01-01T00:00:00Z\n---\n# Title\n"
	tkt, err := ParseBytes([]byte(input))
	require.NoError(t, err)

	ts1 := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 3, 6, 13, 0, 0, 0, time.UTC)

	tkt.AppendNote("first", ts1)
	tkt.AppendNote("second", ts2)

	assert.Equal(t, 1, strings.Count(tkt.Body, "## Notes"))
	assert.Contains(t, tkt.Body, "first")
	assert.Contains(t, tkt.Body, "second")
}

func TestAppendNoteRoundTrip(t *testing.T) {
	input := "---\nid: test\nstatus: open\ncreated: 2026-01-01T00:00:00Z\n---\n# Title\n\nBody text.\n"
	tkt, err := ParseBytes([]byte(input))
	require.NoError(t, err)

	ts := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	tkt.AppendNote("round trip test", ts)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)

	assert.Contains(t, reparsed.Body, "round trip test")
	assert.Contains(t, reparsed.Body, "Body text.")
	assert.Equal(t, "test", reparsed.ID)
}

func TestSetBody(t *testing.T) {
	input := "---\nid: test\nstatus: open\ncreated: 2026-01-01T00:00:00Z\n---\n# Original body\n"
	tkt, err := ParseBytes([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "# Original body\n", tkt.Body)

	tkt.SetBody("# New body\n\nWith more content.\n")

	out, err := tkt.Marshal()
	require.NoError(t, err)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	assert.Equal(t, "# New body\n\nWith more content.\n", reparsed.Body)
	assert.Equal(t, "test", reparsed.ID)
}

func TestParseReader(t *testing.T) {
	input := "---\nid: reader-001\nstatus: open\ncreated: 2026-01-01T00:00:00Z\n---\n# From reader\n"
	tkt, err := Parse(bytes.NewReader([]byte(input)))
	require.NoError(t, err)
	assert.Equal(t, "reader-001", tkt.ID)
}
