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

			var buf bytes.Buffer
			require.NoError(t, View(testConfig(dir), "tst-001", &buf))

			for _, want := range tc.want {
				assert.Contains(t, buf.String(), want)
			}
			for _, miss := range tc.wantMiss {
				assert.NotContains(t, buf.String(), miss)
			}
		})
	}
}

func TestViewBody(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{TicketsDir: dir}
	body := "# A title\n\nSome text.\n\n## Notes\n\n- one\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tst-001.md"),
		[]byte("---\nid: tst-001\nstatus: todo\nkontora: true\n---\n"+body), 0o644))

	var buf bytes.Buffer
	require.NoError(t, ViewBody(cfg, "tst", &buf))

	assert.Equal(t, body, buf.String(), "the output must equal the file after the closing delimiter")
}
