package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtures writes a tickets directory holding one ticket per behaviour the scan
// has to get right, plus the files it has to ignore.
func fixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	// Frontmatter runs to line 9; the body match lines are 10, 12 and 13, and
	// the custom "priority" key is line 8.
	write("tst-001.md", `---
id: tst-001
kontora: true
status: todo
pipeline: default
path: /repo/one
agent: claude
priority: 2
---
# Worktree cleanup

The worktree lock leaks.
worktree again
`)

	write("tst-002.md", `---
id: tst-002
kontora: true
status: done
pipeline: review
path: /repo/two
agent: codex
---
# Literal dot

a.b in one line
axb in another
worktree here
`)

	write("tst-003.md", `---
id: tst-003
kontora: true
status: archived
path: /repo/three
---
# Old work

worktree cleanup finished
`)

	// No query in this test matches tst-004's text: it is only ever reported
	// through its project name.
	write("tst-004.md", `---
id: tst-004
kontora: true
status: open
path: /repo/four
---
# Retry budget

Nothing here.
`)

	conflict := `---
id: tst-005
kontora: true
status: todo
path: /repo/five
---
# Conflict copy

worktree note
`
	write("tst-005.md", conflict)
	// A sync conflict copy: same id, non-canonical filename.
	write("tst-005 2.md", conflict)

	// CRLF line endings, and a body line the project fixture also matches so a
	// ticket can carry a project hit and file hits at once.
	write("tst-008.md", strings.ReplaceAll(`---
id: tst-008
kontora: true
status: todo
path: /repo/four
---
# Windows line endings

worktree on a crlf line
`, "\n", "\r\n"))

	write("tst-007.md", `---
id: tst-007
kontora: true
status: open
path: /repo/seven
---
# Capitalized

Worktree only here
xNEEDLE marker
`)

	// Subdirectories are not descended into.
	write(filepath.Join("sub", "tst-006.md"), `---
id: tst-006
kontora: true
status: todo
path: /repo/six
---
# Nested

worktree nested
`)

	// Unterminated frontmatter: parsing fails and the failure is reported. The
	// path cannot be read out of it, so a project run has to parse it to find out.
	write("tst-bad.md", `---
id: tst-bad
worktree
`)

	// Malformed YAML, but the frontmatter block is closed and its path reads
	// cleanly, so a project run skips it instead of reporting it.
	write("tst-bad2.md", `---
id: tst-bad2
path: /repo/nine
broken: [unclosed
---
# Broken
`)

	// Not a ticket file at all.
	write("notes.txt", "worktree in a text file\n")

	return dir
}

func ids(results []Result) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Ticket.ID)
	}
	return out
}

func find(t *testing.T, results []Result, id string) Result {
	t.Helper()
	for _, r := range results {
		if r.Ticket.ID == id {
			return r
		}
	}
	t.Fatalf("no result for %s", id)
	return Result{}
}

func TestScan(t *testing.T) {
	cases := []struct {
		name     string
		opts     Options
		wantIDs  []string
		wantErrs int
		check    func(t *testing.T, results []Result)
	}{
		{
			name:     "smart case lowercase query matches any case",
			opts:     Options{Query: "worktree"},
			wantIDs:  []string{"tst-001", "tst-002", "tst-003", "tst-005", "tst-007", "tst-008"},
			wantErrs: 1,
			check: func(t *testing.T, results []Result) {
				r := find(t, results, "tst-001")
				assert.Equal(t, 3, r.Total)
				assert.Equal(t, []int{10, 12, 13}, []int{r.Hits[0].Line, r.Hits[1].Line, r.Hits[2].Line})
				assert.Equal(t, "The worktree lock leaks.", r.Hits[1].Text)
				assert.Equal(t, 4, r.Hits[1].Start)
				assert.Equal(t, 12, r.Hits[1].End)
			},
		},
		{
			name:     "smart case uppercase query is case sensitive",
			opts:     Options{Query: "Worktree"},
			wantIDs:  []string{"tst-001", "tst-007"},
			wantErrs: 0,
		},
		{
			name:     "explicit insensitive overrides smart case",
			opts:     Options{Query: "Worktree", CaseMode: CaseInsensitive},
			wantIDs:  []string{"tst-001", "tst-002", "tst-003", "tst-005", "tst-007", "tst-008"},
			wantErrs: 1,
		},
		{
			name:     "explicit sensitive overrides smart case",
			opts:     Options{Query: "worktree", CaseMode: CaseSensitive},
			wantIDs:  []string{"tst-001", "tst-002", "tst-003", "tst-005", "tst-008"},
			wantErrs: 1,
		},
		{
			name:    "regex metacharacters are live by default",
			opts:    Options{Query: "a.b"},
			wantIDs: []string{"tst-002"},
			check: func(t *testing.T, results []Result) {
				assert.Equal(t, 2, find(t, results, "tst-002").Total)
			},
		},
		{
			name:    "literal disables regex metacharacters",
			opts:    Options{Query: "a.b", Literal: true},
			wantIDs: []string{"tst-002"},
			check: func(t *testing.T, results []Result) {
				r := find(t, results, "tst-002")
				require.Len(t, r.Hits, 1)
				assert.Equal(t, "a.b in one line", r.Hits[0].Text)
			},
		},
		{
			name:    "custom frontmatter fields are searchable",
			opts:    Options{Query: "priority: 2"},
			wantIDs: []string{"tst-001"},
			check: func(t *testing.T, results []Result) {
				assert.Equal(t, 8, find(t, results, "tst-001").Hits[0].Line)
			},
		},
		{
			name:    "body only skips the frontmatter block",
			opts:    Options{Query: "priority: 2", BodyOnly: true},
			wantIDs: []string{},
		},
		{
			name:     "max per file truncates the hits but not the total",
			opts:     Options{Query: "worktree", MaxPerFile: 2},
			wantIDs:  []string{"tst-001", "tst-002", "tst-003", "tst-005", "tst-007", "tst-008"},
			wantErrs: 1,
			check: func(t *testing.T, results []Result) {
				r := find(t, results, "tst-001")
				assert.Len(t, r.Hits, 2)
				assert.Equal(t, 3, r.Total)
			},
		},
		{
			name:     "status filter narrows the results",
			opts:     Options{Query: "worktree", Status: "done"},
			wantIDs:  []string{"tst-002"},
			wantErrs: 1,
		},
		{
			name:     "pipeline filter narrows the results",
			opts:     Options{Query: "worktree", Pipeline: "review"},
			wantIDs:  []string{"tst-002"},
			wantErrs: 1,
		},
		{
			name:     "agent filter narrows the results",
			opts:     Options{Query: "worktree", Agent: "claude"},
			wantIDs:  []string{"tst-001"},
			wantErrs: 1,
		},
		{
			name:     "path filter narrows the results",
			opts:     Options{Query: "worktree", Path: "/repo/two/"},
			wantIDs:  []string{"tst-002"},
			wantErrs: 1,
		},
		{
			// The query is in no file, so both tickets are reported through the
			// project alone. Only the ticket whose frontmatter path cannot be
			// read (tst-bad, unterminated) is parsed and reported: the run does
			// not fall back to parsing the whole store.
			name:     "a matching project name reports its tickets",
			opts:     Options{Query: "sigil", Projects: map[string]string{"sigil": "/repo/four"}},
			wantIDs:  []string{"tst-004", "tst-008"},
			wantErrs: 1,
			check: func(t *testing.T, results []Result) {
				r := find(t, results, "tst-004")
				require.Len(t, r.Hits, 1)
				assert.Equal(t, FieldProject, r.Hits[0].Field)
				assert.Equal(t, "sigil", r.Hits[0].Text)
				assert.Equal(t, 0, r.Hits[0].Line)
			},
		},
		{
			name:     "the project hit comes first and survives the cap",
			opts:     Options{Query: "crlf", Projects: map[string]string{"crlf": "/repo/four"}, MaxPerFile: 1},
			wantIDs:  []string{"tst-004", "tst-008"},
			wantErrs: 1,
			check: func(t *testing.T, results []Result) {
				r := find(t, results, "tst-008")
				require.Len(t, r.Hits, 2)
				assert.Equal(t, FieldProject, r.Hits[0].Field)
				assert.Equal(t, 9, r.Hits[1].Line)
				// The CR of a CRLF file is not part of the line.
				assert.Equal(t, "worktree on a crlf line", r.Hits[1].Text)
				assert.Equal(t, 2, r.Total)
			},
		},
		{
			name:     "body only drops the project match",
			opts:     Options{Query: "crlf", Projects: map[string]string{"crlf": "/repo/four"}, BodyOnly: true},
			wantIDs:  []string{"tst-008"},
			wantErrs: 0,
			check: func(t *testing.T, results []Result) {
				r := find(t, results, "tst-008")
				require.Len(t, r.Hits, 1)
				assert.Empty(t, r.Hits[0].Field)
			},
		},
		{
			name:    "a start anchor matches the start of a line",
			opts:    Options{Query: "^status: archived"},
			wantIDs: []string{"tst-003"},
			check: func(t *testing.T, results []Result) {
				assert.Equal(t, 4, find(t, results, "tst-003").Hits[0].Line)
			},
		},
		{
			name:    "an end anchor matches the end of a line",
			opts:    Options{Query: "cleanup$"},
			wantIDs: []string{"tst-001"},
		},
		{
			name:    "an explicit multiline flag still works",
			opts:    Options{Query: "(?m)^id: tst-002$"},
			wantIDs: []string{"tst-002"},
		},
		{
			// The S of \S selects a character class; it is not the uppercase
			// letter that would turn smart case into a case-sensitive run.
			name:    "an escape does not make the query case sensitive",
			opts:    Options{Query: `\Sneedle`},
			wantIDs: []string{"tst-007"},
		},
	}

	dir := fixtures(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results, errs, err := Scan(dir, tc.opts)
			require.NoError(t, err)
			assert.Equal(t, tc.wantIDs, ids(results))
			assert.Len(t, errs, tc.wantErrs)
			if tc.check != nil {
				tc.check(t, results)
			}
		})
	}
}

// TestScanErrors runs against a directory that does not exist, so it also
// covers the order of the two failures: an invalid regex is rejected by the
// compile step before any file is read, and the missing directory is only
// reported once the query is good.
func TestScanErrors(t *testing.T) {
	cases := []struct {
		name  string
		opts  Options
		wants string
		is    error
	}{
		{name: "invalid regex", opts: Options{Query: "("}, wants: "error parsing regexp"},
		{name: "empty query", opts: Options{}, wants: "query is required"},
		{name: "missing tickets dir", opts: Options{Query: "worktree"}, wants: "reading tickets dir", is: os.ErrNotExist},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Scan(filepath.Join(t.TempDir(), "missing"), tc.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wants)
			if tc.is != nil {
				assert.ErrorIs(t, err, tc.is)
			}
		})
	}
}

// TestBodyStart covers the frontmatter block shapes Scan cannot reach: a file
// whose block never closes fails to parse long before lineHits runs.
func TestBodyStart(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{name: "no frontmatter is all body", content: "# Title\nbody\n", want: 0},
		{name: "body starts after the closing marker", content: "---\nid: a\n---\n# Title\n", want: 3},
		{name: "unterminated frontmatter falls back to the whole file", content: "---\nid: a\nbody\n", want: 0},
		{name: "crlf line endings", content: "---\r\nid: a\r\n---\r\n# Title\r\n", want: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bodyStart(strings.Split(tc.content, "\n")))
		})
	}
}
