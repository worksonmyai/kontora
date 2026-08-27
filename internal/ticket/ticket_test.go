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
		{StatusArchived},
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

func TestClaimedByDecodeAndRoundTrip(t *testing.T) {
	src := "---\nid: clm-001\nkontora: true\nstatus: in_progress\nclaimed_by: alpha\n---\n# Claimed ticket\n"

	tkt, err := ParseBytes([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "alpha", tkt.ClaimedBy)

	// A rewrite that touches an unrelated field must preserve claimed_by.
	require.NoError(t, tkt.SetField("status", "done"))
	out, err := tkt.Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(out), "claimed_by: alpha")

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	assert.Equal(t, "alpha", reparsed.ClaimedBy)

	// Setting the claim on a ticket that had none appends it and decodes back.
	fresh, err := ParseBytes([]byte("---\nid: clm-002\nkontora: true\nstatus: in_progress\n---\n# body\n"))
	require.NoError(t, err)
	assert.Empty(t, fresh.ClaimedBy)
	require.NoError(t, fresh.SetField("claimed_by", "beta"))
	assert.Equal(t, "beta", fresh.ClaimedBy)
	out2, err := fresh.Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(out2), "claimed_by: beta")
}

func TestFinalSummaryDecodeAndRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "legacy ticket without the field",
			src:  "---\nid: fin-001\nkontora: true\nstatus: done\nsummary: last run\n---\n# body\n",
			want: "",
		},
		{
			name: "empty value",
			src:  "---\nid: fin-002\nkontora: true\nstatus: done\nfinal_summary: \"\"\n---\n# body\n",
			want: "",
		},
		{
			name: "multi-line value",
			src:  "---\nid: fin-003\nkontora: true\nstatus: done\nfinal_summary: |-\n  first line\n  second line\n---\n# body\n",
			want: "first line\nsecond line",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tkt, err := ParseBytes([]byte(tc.src))
			require.NoError(t, err)
			assert.Equal(t, tc.want, tkt.FinalSummary)

			// The per-run summary contract is untouched by the new field.
			summary := tkt.Summary
			require.NoError(t, tkt.SetField("final_summary", "the whole ticket"))
			assert.Equal(t, "the whole ticket", tkt.FinalSummary)
			assert.Equal(t, summary, tkt.Summary)

			out, err := tkt.Marshal()
			require.NoError(t, err)
			reparsed, err := ParseBytes(out)
			require.NoError(t, err)
			assert.Equal(t, "the whole ticket", reparsed.FinalSummary)
			assert.Equal(t, summary, reparsed.Summary)
		})
	}
}

func TestRelationsDecode(t *testing.T) {
	cases := []struct {
		name       string
		fm         string
		wantDeps   []string
		wantLinks  []string
		wantParent string
	}{
		{
			name: "kontora ticket without the fields",
			fm:   "id: rel-001\nstatus: todo\n",
		},
		{
			// What the ticket CLI writes for a ticket with no relations, and it
			// is not the same value as the absent field above.
			name:      "empty lists",
			fm:        "id: rel-002\nstatus: todo\ndeps: []\nlinks: []\n",
			wantDeps:  []string{},
			wantLinks: []string{},
		},
		{
			name:       "flow lists and a parent",
			fm:         "id: rel-003\nstatus: open\ndeps: [kon-aaaa, kon-bbbb]\nlinks: [kon-cccc]\nparent: kon-epic\n",
			wantDeps:   []string{"kon-aaaa", "kon-bbbb"},
			wantLinks:  []string{"kon-cccc"},
			wantParent: "kon-epic",
		},
		{
			// The external ticket CLI writes both shapes.
			name:      "block sequences",
			fm:        "id: rel-004\nstatus: open\ndeps:\n  - kon-aaaa\nlinks:\n  - kon-cccc\n  - kon-dddd\n",
			wantDeps:  []string{"kon-aaaa"},
			wantLinks: []string{"kon-cccc", "kon-dddd"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tkt, err := ParseBytes([]byte("---\n" + tc.fm + "---\n# body\n"))
			require.NoError(t, err)
			assert.Equal(t, tc.wantDeps, tkt.Deps)
			assert.Equal(t, tc.wantLinks, tkt.Links)
			assert.Equal(t, tc.wantParent, tkt.Parent)

			// Relations belong to the external tool: a daemon write to an
			// unrelated field must leave them exactly as they were.
			require.NoError(t, tkt.SetField("status", "in_progress"))
			out, err := tkt.Marshal()
			require.NoError(t, err)
			reparsed, err := ParseBytes(out)
			require.NoError(t, err)
			assert.Equal(t, tkt.Deps, reparsed.Deps)
			assert.Equal(t, tkt.Links, reparsed.Links)
			assert.Equal(t, tkt.Parent, reparsed.Parent)
		})
	}
}

// TestAnnotationReturnStatusRoundTrip pins the field the daemon reads to route a
// pickup to a refine run: it decodes, it survives a rewrite of other fields, and
// setting it on a ticket that never had it does not disturb custom fields or
// field order.
func TestAnnotationReturnStatusRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want Status
	}{
		{
			name: "legacy ticket without the field",
			src:  "---\nid: ann-001\nkontora: true\nstatus: todo\nmy_field: keep me\n---\n# body\n",
			want: "",
		},
		{
			name: "parked ticket carrying the field",
			src:  "---\nid: ann-002\nkontora: true\nstatus: todo\nannotation_return_status: human_review\nmy_field: keep me\n---\n# body\n",
			want: "human_review",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tkt, err := ParseBytes([]byte(tc.src))
			require.NoError(t, err)
			assert.Equal(t, tc.want, tkt.AnnotationReturnStatus)

			require.NoError(t, tkt.SetField("annotation_return_status", "paused"))
			assert.Equal(t, StatusPaused, tkt.AnnotationReturnStatus)

			out, err := tkt.Marshal()
			require.NoError(t, err)
			assert.Contains(t, string(out), "my_field: keep me", "custom fields survive")
			assert.Less(t, strings.Index(string(out), "id: ann-"), strings.Index(string(out), "status:"),
				"field order is preserved")

			reparsed, err := ParseBytes(out)
			require.NoError(t, err)
			assert.Equal(t, StatusPaused, reparsed.AnnotationReturnStatus)

			// Clearing the marker is how a finished refine run releases the ticket.
			require.NoError(t, reparsed.SetField("annotation_return_status", ""))
			assert.Empty(t, reparsed.AnnotationReturnStatus)
		})
	}
}

// TestHistoryEntryKindRoundTrip pins the optional fields a run adds to a history
// entry: model, effort, kind, session_reused, session_kind and session_ref all
// carry omitempty, so a write leaves an entry that has none of them alone. started_at and completed_at do
// not, and every write adds them as nulls to an entry that never carried them.
func TestHistoryEntryKindRoundTrip(t *testing.T) {
	src := "---\nid: hk-001\nkontora: true\nstatus: todo\nhistory:\n  - stage: code\n    agent: claude\n    exit_code: 0\n---\n# body\n"
	tkt, err := ParseBytes([]byte(src))
	require.NoError(t, err)
	require.Len(t, tkt.History, 1)
	assert.Empty(t, tkt.History[0].Kind)
	assert.False(t, tkt.History[0].SessionReused, "a stage run says nothing about session reuse")
	assert.Empty(t, tkt.History[0].Model, "an entry written before the field existed has no model")
	assert.Empty(t, tkt.History[0].Effort)
	assert.Empty(t, tkt.History[0].SessionKind, "an entry written before the field existed names no runtime")
	assert.Empty(t, tkt.History[0].SessionRef)

	require.NoError(t, tkt.SetField("history", append(tkt.History, HistoryEntry{
		Stage:         "code",
		Agent:         "claude",
		Model:         "haiku",
		Effort:        "low",
		Kind:          KindAnnotation,
		SessionReused: true,
		SessionKind:   "pi",
		SessionRef:    "pi-sessions/code/01JC9.jsonl",
	})))

	out, err := tkt.Marshal()
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(out), "kind: annotation"))
	assert.Equal(t, 1, strings.Count(string(out), "session_reused: true"))
	assert.Equal(t, 1, strings.Count(string(out), "model: haiku"))
	assert.Equal(t, 1, strings.Count(string(out), "effort: low"))
	assert.Equal(t, 1, strings.Count(string(out), "session_kind: pi"))
	assert.Equal(t, 1, strings.Count(string(out), "session_ref: pi-sessions/code/01JC9.jsonl"))

	// The entry that came in keeps the three keys it had, plus the two the type
	// writes unconditionally. An optional field that lost its omitempty would
	// show up here as a fourth key on an entry that never set it.
	first, _, ok := strings.Cut(strings.SplitN(string(out), "history:\n", 2)[1], "  - stage: code\n    agent: claude\n    model")
	require.True(t, ok, "the appended entry follows the one that came in")
	assert.Equal(t, "  - stage: code\n    agent: claude\n    exit_code: 0\n    started_at: null\n    completed_at: null\n", first)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	require.Len(t, reparsed.History, 2)
	assert.Empty(t, reparsed.History[0].Kind)
	assert.False(t, reparsed.History[0].SessionReused)
	assert.Empty(t, reparsed.History[0].Model)
	assert.Empty(t, reparsed.History[0].Effort)
	assert.Empty(t, reparsed.History[0].SessionKind)
	assert.Empty(t, reparsed.History[0].SessionRef)
	assert.Equal(t, KindAnnotation, reparsed.History[1].Kind)
	assert.True(t, reparsed.History[1].SessionReused)
	assert.Equal(t, "haiku", reparsed.History[1].Model)
	assert.Equal(t, "low", reparsed.History[1].Effort)
	assert.Equal(t, "pi", reparsed.History[1].SessionKind)
	assert.Equal(t, "pi-sessions/code/01JC9.jsonl", reparsed.History[1].SessionRef)
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

	const logPath = "/var/log/kontora/unk-001/default.log"
	require.NoError(t, tkt.SetField("last_log", logPath))
	assert.Equal(t, logPath, tkt.LastLog)

	out, err := tkt.Marshal()
	require.NoError(t, err)

	outStr := string(out)
	assert.Contains(t, outStr, "custom_field: hello world")
	assert.Contains(t, outStr, "another_custom: 42")
	assert.Contains(t, outStr, "last_log: "+logPath)

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	assert.Equal(t, logPath, reparsed.LastLog)
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

func TestIsSafeID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"kon-q88f", true},
		{"gra-1234", true},
		{"custom_id", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../escape", false},
		{"foo/bar", false},
		{"foo\\bar", false},
		{"foo..bar", false},
		{"nul\x00byte", false},
		{"a/../b", false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			assert.Equal(t, tt.want, IsSafeID(tt.id))
		})
	}
}

func TestIsCanonicalPath(t *testing.T) {
	tests := []struct {
		path string
		id   string
		want bool
	}{
		{"/tickets/kon-q88f.md", "kon-q88f", true},
		{"kon-q88f.md", "kon-q88f", true},
		{"/tickets/kon-q88f 2.md", "kon-q88f", false},
		{"/tickets/kon-q88f.sync-conflict-20260610-070128-IDDACTZ.md", "kon-q88f", false},
		{"/tickets/kon-q88f.md", "kon-q88g", false},
		{"/tickets/notes.md", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, IsCanonicalPath(tt.path, tt.id))
		})
	}
}

func TestParseReader(t *testing.T) {
	input := "---\nid: reader-001\nstatus: open\ncreated: 2026-01-01T00:00:00Z\n---\n# From reader\n"
	tkt, err := Parse(bytes.NewReader([]byte(input)))
	require.NoError(t, err)
	assert.Equal(t, "reader-001", tkt.ID)
}

func TestNotifyDecode(t *testing.T) {
	tests := []struct {
		name           string
		frontmatter    string
		want           []string
		wantChannels   []string
		wantParseError string
	}{
		{
			name:        "absent",
			frontmatter: "id: n-1\nstatus: todo\n",
		},
		{
			name:        "one status as a scalar",
			frontmatter: "id: n-1\nstatus: todo\nnotify: done\n",
			want:        []string{"done"},
		},
		{
			name:        "a sequence",
			frontmatter: "id: n-1\nstatus: todo\nnotify: [human_review, done]\n",
			want:        []string{"human_review", "done"},
		},
		{
			name:        "a block sequence",
			frontmatter: "id: n-1\nstatus: todo\nnotify:\n  - waiting\n  - done\n",
			want:        []string{"waiting", "done"},
		},
		{
			name:        "an empty value names no status",
			frontmatter: "id: n-1\nstatus: todo\nnotify:\n",
		},
		{
			name:         "channels alongside",
			frontmatter:  "id: n-1\nstatus: todo\nnotify: [done]\nnotify_channels: [mm]\n",
			want:         []string{"done"},
			wantChannels: []string{"mm"},
		},
		{
			name:           "a mapping is rejected with its line",
			frontmatter:    "id: n-1\nstatus: todo\nnotify:\n  mm: done\n",
			wantParseError: "line 4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tkt, err := ParseBytes([]byte("---\n" + tt.frontmatter + "---\n# body\n"))
			if tt.wantParseError != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, "notify")
				assert.ErrorContains(t, err, tt.wantParseError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, tkt.Notify.Statuses)
			assert.Equal(t, tt.wantChannels, tkt.NotifyChannels)
		})
	}
}

func TestNotifyRoundTrip(t *testing.T) {
	src := "---\nid: n-2\nkontora: true\nstatus: todo\nnotify: [human_review, done]\nnotify_channels: [mm]\ncustom_key: kept\n---\n# body\n"

	tkt, err := ParseBytes([]byte(src))
	require.NoError(t, err)
	require.NoError(t, tkt.SetField("status", "done"))

	out, err := tkt.Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(out), "notify: [human_review, done]")
	assert.Contains(t, string(out), "notify_channels: [mm]")
	assert.Contains(t, string(out), "custom_key: kept")

	reparsed, err := ParseBytes(out)
	require.NoError(t, err)
	assert.Equal(t, []string{"human_review", "done"}, reparsed.Notify.Statuses)
	assert.Equal(t, []string{"mm"}, reparsed.NotifyChannels)
}

func TestNotifyIsNotAddedToATicketWithoutIt(t *testing.T) {
	tkt, err := ParseBytes([]byte("---\nid: n-3\nkontora: true\nstatus: todo\n---\n# body\n"))
	require.NoError(t, err)
	require.NoError(t, tkt.SetField("status", "done"))

	out, err := tkt.Marshal()
	require.NoError(t, err)
	assert.NotContains(t, string(out), "notify")
}
