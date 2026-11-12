package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

// scheduledLocal is the schedule case's instant as the reader's zone renders
// it, so the assertion holds wherever the suite runs.
var scheduledLocal = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Local().Format("Jan 02 15:04")

func TestViewRendersFrontmatterFields(t *testing.T) {
	cases := []struct {
		name     string
		fields   string
		want     []string
		wantMiss []string
	}{
		{
			name:   "pipeline agent",
			fields: "pipeline: default\nstage: code\n",
			want:   []string{"agent:     claude-sonnet"},
		},
		{
			name:     "ticket agent overrides pipeline",
			fields:   "pipeline: default\nstage: code\nagent: pi-personal-sol\n",
			want:     []string{"agent:     pi-personal-sol"},
			wantMiss: []string{"agent:     claude-sonnet"},
		},
		{
			name:   "standalone agent override without stage",
			fields: "agent: pi-personal-sol\n",
			want:   []string{"agent:     pi-personal-sol"},
		},
		{
			name: "standalone default agent",
			want: []string{"agent:     fallback"},
		},
		{
			name:     "pipeline without stage has no agent",
			fields:   "pipeline: default\n",
			wantMiss: []string{"agent:"},
		},
		{
			name:   "pipeline override without stage",
			fields: "pipeline: default\nagent: pi-personal-sol\n",
			want:   []string{"agent:     pi-personal-sol"},
		},
		{
			name:     "passive epic has no agent",
			fields:   "kind: epic\n",
			wantMiss: []string{"agent:"},
		},
		{
			name:   "built-in rework history agent",
			fields: "pipeline: default\nstage: rework\nhistory:\n  - stage: code\n    agent: previous\n",
			want:   []string{"agent:     previous"},
		},
		{
			name:   "built-in rework default agent",
			fields: "pipeline: default\nstage: rework\n",
			want:   []string{"agent:     fallback"},
		},
		{
			name:     "no branch fields",
			want:     []string{"tst-001"},
			wantMiss: []string{"branch:", "base:"},
		},
		{
			name:     "branch only",
			fields:   "branch: custom/tst-001\n",
			want:     []string{"branch:    custom/tst-001"},
			wantMiss: []string{"base:"},
		},
		{
			name:   "branch and base branch",
			fields: "branch: custom/tst-001\nbase_branch: develop\n",
			want:   []string{"branch:    custom/tst-001", "base:      develop"},
		},
		{
			name:   "base branch only",
			fields: "base_branch: origin/develop\n",
			want:   []string{"base:      origin/develop"},
		},
		{
			name:   "provider error",
			fields: "last_error: 'agent error: Provided authentication token is expired.'\n",
			want:   []string{"Last error: agent error: Provided authentication token is expired."},
		},
		{
			name:   "a schedule prints in the reader's own zone",
			fields: "scheduled_at: \"2026-09-01T09:00:00Z\"\n",
			want:   []string{"starts:    " + scheduledLocal},
		},
		{
			// A wrong time would hide the typo instead of pointing at it.
			name:   "a schedule the parser rejects prints as it stands",
			fields: "scheduled_at: \"next tuesday\"\n",
			want:   []string{"starts:    next tuesday"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTicket(t, dir, "tst-001.md", fmt.Sprintf(`---
id: tst-001
kontora: true
status: todo
path: /repo
%s---
# Title
`, tc.fields))

			cfg := testConfig(dir)
			cfg.DefaultAgent = "fallback"
			cfg.ReworkIsBuiltin = true
			var buf bytes.Buffer
			require.NoError(t, View(cfg, "tst-001", &buf))

			for _, want := range tc.want {
				assert.Contains(t, buf.String(), want)
			}
			for _, miss := range tc.wantMiss {
				assert.NotContains(t, buf.String(), miss)
			}
		})
	}
}

func TestViewHistorySeparatesOutcomeFromRunnerExit(t *testing.T) {
	for _, outcome := range []string{"", "complete", "blocked", "protocol_error", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			fields := ""
			if outcome != "" {
				fields = "    run_outcome: " + outcome + "\n"
			}
			writeTicket(t, dir, "tst-001.md", "---\nid: tst-001\nstatus: todo\ninvocation_id: active\nhistory:\n  - stage: default\n    agent: claude\n    exit_code: 0\n    outcome_reason: needs input\n    invocation_id: previous\n"+fields+"---\n# Title\n")
			var buf bytes.Buffer
			require.NoError(t, View(testConfig(dir), "tst-001", &buf))
			want := outcome
			if want == "" {
				want = "unknown/unverified"
			}
			assert.Contains(t, buf.String(), "outcome: "+want+" · runner exit 0 · needs input · invocation previous")
			assert.Contains(t, buf.String(), "invocation: active")
			assert.NotContains(t, buf.String(), "✓")
		})
	}
}

func TestViewBody(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{TicketsDir: dir}
	const tail = "## Notes\n\n**2026-08-08**\n\none\n\n**2026-08-09**\n\ntwo\n\n**2026-08-10**\n\nthree\n\n## Code review\n\nfinding\n"
	body := "# A title\n\nSome text.\n\n" + tail
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tst-001.md"),
		[]byte("---\nid: tst-001\nstatus: todo\nkontora: true\n---\n"+body), 0o644))

	for _, tt := range []struct {
		name     string
		bodyOnly bool
		last     int
		want     string
	}{
		{name: "body", bodyOnly: true, want: body},
		{name: "notes", want: tail},
		{name: "last two", last: 2, want: "**2026-08-09**\n\ntwo\n\n**2026-08-10**\n\nthree\n\n## Code review\n\nfinding\n"},
		{name: "above count", last: 5, want: tail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if tt.bodyOnly {
				require.NoError(t, ViewBody(cfg, "tst", &buf))
			} else {
				require.NoError(t, ViewNotes(cfg, "tst", tt.last, &buf))
			}
			assert.Equal(t, tt.want, buf.String())
		})
	}
}
