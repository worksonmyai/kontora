package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// searchConfig writes the ticket store the search tests run against and returns
// a config with one project pointing at /repo/one.
func searchConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()

	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
kontora: true
status: todo
pipeline: default
path: /repo/one
created: 2026-01-02T10:00:00Z
---
# Fix worktree cleanup

The worktree lock leaks.
worktree again
`)

	writeTicket(t, dir, "tst-002.md", `---
id: tst-002
kontora: true
status: done
path: /repo/two
created: 2026-01-01T10:00:00Z
---
# Sidecar retry budget

worktree note
`)

	writeTicket(t, dir, "tst-bad.md", `---
id: tst-bad
worktree
`)

	cfg := testConfig(dir)
	cfg.Projects = map[string]config.Project{"onerepo": {Path: "/repo/one"}}
	return cfg
}

func TestSearch(t *testing.T) {
	cases := []struct {
		name string
		// cfg replaces the shared store when a case needs a different one.
		cfg      func(t *testing.T) *config.Config
		opts     SearchOpts
		wantErr  string
		wantOut  []string
		denyOut  []string
		wantWarn []string
		check    func(t *testing.T, out string)
	}{
		{
			name: "grouped output carries the header and the line numbers",
			opts: SearchOpts{Query: "worktree"},
			wantOut: []string{
				"tst-001  todo  Fix worktree cleanup  onerepo\n",
				"   9  # Fix worktree cleanup\n",
				"  11  The worktree lock leaks.\n",
				"  12  worktree again\n",
				"tst-002  done  Sidecar retry budget\n",
				"  10  worktree note\n",
			},
			// in_progress and todo sort above done.
			check: func(t *testing.T, out string) {
				assert.Less(t, strings.Index(out, "tst-001"), strings.Index(out, "tst-002"))
			},
		},
		{
			name:    "ids only prints bare ids",
			opts:    SearchOpts{Query: "worktree", IDsOnly: true},
			wantOut: []string{"tst-001\ntst-002\n"},
			check: func(t *testing.T, out string) {
				assert.Equal(t, "tst-001\ntst-002\n", out)
			},
		},
		{
			name:    "max per file reports the hits it did not show",
			opts:    SearchOpts{Query: "worktree", MaxPerFile: 2},
			wantOut: []string{"   9  # Fix worktree cleanup\n", "      ... 1 more\n"},
			denyOut: []string{"worktree again"},
		},
		{
			name:    "no matches says so",
			opts:    SearchOpts{Query: "nothingmatchesthis"},
			wantOut: []string{"No matches.\n"},
		},
		{
			name:    "a matching project name reports its tickets",
			opts:    SearchOpts{Query: "onerepo"},
			wantOut: []string{"tst-001", "  project  onerepo\n"},
			denyOut: []string{"tst-002"},
		},
		{
			name:    "status narrows the results",
			opts:    SearchOpts{Query: "worktree", Status: "done"},
			wantOut: []string{"tst-002"},
			denyOut: []string{"tst-001"},
		},
		{
			name:    "unknown project is rejected",
			opts:    SearchOpts{Query: "worktree", Project: "nosuchproject"},
			wantErr: `unknown project "nosuchproject"`,
		},
		{
			name:    "project and path cannot be combined",
			opts:    SearchOpts{Query: "worktree", Project: "onerepo", Path: "/repo/two"},
			wantErr: "path and project cannot be combined",
		},
		{
			name:    "case flags cannot be combined",
			opts:    SearchOpts{Query: "worktree", IgnoreCase: true, MatchCase: true},
			wantErr: "-i and -s cannot be combined",
		},
		{
			name:    "an invalid regex is rejected",
			opts:    SearchOpts{Query: "("},
			wantErr: "error parsing regexp",
		},
		{
			name:     "unparseable tickets are reported to the warning writer",
			opts:     SearchOpts{Query: "worktree"},
			wantWarn: []string{"1 ticket could not be parsed:", "tst-bad.md"},
		},
		{
			name:    "ids only and json cannot be combined",
			opts:    SearchOpts{Query: "worktree", IDsOnly: true, JSON: true},
			wantErr: "-l and --json cannot be combined",
		},
		{
			name:    "a negative cap is rejected",
			opts:    SearchOpts{Query: "worktree", MaxPerFile: -1},
			wantErr: "-m cannot be negative",
		},
		{
			name:    "unknown pipeline is rejected",
			opts:    SearchOpts{Query: "worktree", Pipeline: "nosuchpipeline"},
			wantErr: `unknown pipeline "nosuchpipeline"`,
		},
		{
			name:    "unknown agent is rejected",
			opts:    SearchOpts{Query: "worktree", Agent: "nosuchagent"},
			wantErr: `unknown agent "nosuchagent"`,
		},
		{
			// A status is not a closed set: a ticket another tool wrote can
			// carry any string, so this warns and still runs.
			name:     "unknown status warns and runs",
			opts:     SearchOpts{Query: "worktree", Status: "nosuchstatus"},
			wantOut:  []string{"No matches.\n"},
			wantWarn: []string{`warning: "nosuchstatus" is not a configured status`},
		},
		{
			name:    "a configured pipeline narrows the results",
			opts:    SearchOpts{Query: "worktree", Pipeline: "default"},
			wantOut: []string{"tst-001"},
			denyOut: []string{"tst-002"},
		},
		{
			name:    "an anchored query matches a frontmatter line",
			opts:    SearchOpts{Query: "^status: done"},
			wantOut: []string{"tst-002", "  4  status: done\n"},
			denyOut: []string{"tst-001"},
		},
		{
			name: "a missing tickets dir is an empty store",
			cfg: func(t *testing.T) *config.Config {
				return testConfig(filepath.Join(t.TempDir(), "missing"))
			},
			opts:    SearchOpts{Query: "worktree"},
			wantOut: []string{"No matches.\n"},
		},
		{
			name:    "json output is an array when nothing matched",
			opts:    SearchOpts{Query: "nothingmatchesthis", JSON: true},
			wantOut: []string{"[]\n"},
			check: func(t *testing.T, out string) {
				assert.Equal(t, "[]\n", out)
			},
		},
	}

	shared := searchConfig(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := shared
			if tc.cfg != nil {
				cfg = tc.cfg(t)
			}
			var out, warn bytes.Buffer
			opts := tc.opts
			opts.Warn = &warn

			err := Search(cfg, &out, opts)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, out.String())
				return
			}
			require.NoError(t, err)

			for _, want := range tc.wantOut {
				assert.Contains(t, out.String(), want)
			}
			for _, deny := range tc.denyOut {
				assert.NotContains(t, out.String(), deny)
			}
			for _, want := range tc.wantWarn {
				assert.Contains(t, warn.String(), want)
			}
			if tc.check != nil {
				tc.check(t, out.String())
			}
		})
	}
}

func TestSearchJSON(t *testing.T) {
	cfg := searchConfig(t)

	var out, warn bytes.Buffer
	require.NoError(t, Search(cfg, &out, SearchOpts{Query: "worktree", JSON: true, Warn: &warn}))

	var results []struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Status   string `json:"status"`
		Stage    string `json:"stage"`
		Pipeline string `json:"pipeline"`
		Agent    string `json:"agent"`
		Path     string `json:"path"`
		Project  string `json:"project"`
		File     string `json:"file"`
		Total    int    `json:"total"`
		Matches  []struct {
			Line  int    `json:"line"`
			Field string `json:"field"`
			Text  string `json:"text"`
			Start int    `json:"start"`
			End   int    `json:"end"`
		} `json:"matches"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &results))
	require.Len(t, results, 2)

	first := results[0]
	assert.Equal(t, "tst-001", first.ID)
	assert.Equal(t, "Fix worktree cleanup", first.Title)
	assert.Equal(t, "todo", first.Status)
	assert.Equal(t, "default", first.Pipeline)
	assert.Equal(t, "/repo/one", first.Path)
	assert.Equal(t, "onerepo", first.Project)
	assert.Contains(t, first.File, "tst-001.md")
	assert.Equal(t, 3, first.Total)
	require.Len(t, first.Matches, 3)
	assert.Equal(t, 11, first.Matches[1].Line)
	assert.Equal(t, "The worktree lock leaks.", first.Matches[1].Text)
	assert.Equal(t, 4, first.Matches[1].Start)
	assert.Equal(t, 12, first.Matches[1].End)

	// The warning never reaches the JSON, which has to stay parseable.
	assert.Contains(t, warn.String(), "could not be parsed")
}

// TestClipLine covers what the piped tests cannot: clipping and the highlight
// span both hang off a terminal width, which a buffer never reports.
func TestClipLine(t *testing.T) {
	cases := []struct {
		name              string
		text              string
		start, end, width int
		wantText          string
		wantStart         int
		wantEnd           int
	}{
		{name: "no width leaves the line alone", text: "a long enough line", start: 2, end: 6, width: 0, wantText: "a long enough line", wantStart: 2, wantEnd: 6},
		{name: "line shorter than the width", text: "short", start: 0, end: 5, width: 20, wantText: "short", wantStart: 0, wantEnd: 5},
		{name: "tail is cut", text: "0123456789", start: 1, end: 3, width: 6, wantText: "01234…", wantStart: 1, wantEnd: 3},
		{name: "span crossing the cut is clamped", text: "0123456789", start: 3, end: 9, width: 6, wantText: "…2345…", wantStart: 4, wantEnd: 7},
		{name: "the window slides to a span past the cut", text: "0123456789", start: 7, end: 9, width: 6, wantText: "…6789", wantStart: 4, wantEnd: 6},
		{name: "a width too small for the ellipsis still keeps one column", text: "0123456789", start: 0, end: 2, width: 1, wantText: "0…", wantStart: 0, wantEnd: 1},
		{name: "multibyte runes count as one column", text: "ααααααααα", start: 0, end: 2, width: 4, wantText: "ααα…", wantStart: 0, wantEnd: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, start, end := clipLine(tc.text, tc.start, tc.end, tc.width)
			assert.Equal(t, tc.wantText, text)
			assert.Equal(t, tc.wantStart, start)
			assert.Equal(t, tc.wantEnd, end)
			assert.NotPanics(t, func() { highlightMatch(text, start, end) })
			if start < end {
				// What survived the clip is still part of the match.
				assert.Contains(t, tc.text[tc.start:tc.end], text[start:end])
			}
		})
	}
}

// TestSearchHeader covers the width budget: the header has to fit the terminal
// whichever columns it ends up printing.
func TestSearchHeader(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		project string
		width   int
		wantOut string
		// fits says the header can be made to fit the width at all: it cannot
		// when the columns that are always printed are wider on their own.
		fits bool
	}{
		{name: "no width prints everything", body: "# A short title", project: "onerepo", width: 0, wantOut: "tst-001  todo  A short title  onerepo"},
		{name: "a wide terminal prints everything", body: "# A short title", project: "onerepo", width: 80, wantOut: "tst-001  todo  A short title  onerepo", fits: true},
		{name: "the title absorbs the clipping", body: "# A title far too long for this terminal", project: "onerepo", width: 40, wantOut: "tst-001  todo  A title far too…  onerepo", fits: true},
		{name: "no project leaves the title more room", body: "# A title far too long for this terminal", width: 40, wantOut: "tst-001  todo  A title far too long for…", fits: true},
		{name: "a non-ascii title is measured in runes", body: "# ααααααααααααααααααααααααα", project: "onerepo", width: 40, wantOut: "tst-001  todo  ααααααααααααααα…  onerepo", fits: true},
		{name: "a terminal too narrow to fit the columns drops the title", body: "# A short title", project: "onerepo", width: 10, wantOut: "tst-001  todo  onerepo"},
		{name: "a ticket with no title prints the other columns", body: "no heading", project: "onerepo", width: 40, wantOut: "tst-001  todo  onerepo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := searchHeader(&ticket.Ticket{ID: "tst-001", Status: "todo", Body: tc.body}, tc.project, tc.width)
			assert.Equal(t, tc.wantOut, header)
			if tc.fits {
				assert.LessOrEqual(t, lipgloss.Width(header), tc.width)
			}
		})
	}
}
