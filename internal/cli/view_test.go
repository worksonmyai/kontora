package cli

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
