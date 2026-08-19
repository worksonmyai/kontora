package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestViewRendersBranchFields(t *testing.T) {
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
