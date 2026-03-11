package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRender(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "PLAN.md"), []byte("the plan"), 0o644))

	data := Data{
		Ticket: TicketData{
			ID:          "abc-123",
			Title:       "Fix the bug",
			Description: "Something is broken",
			FilePath:    "/tmp/tickets/abc-123.md",
		},
	}

	tests := []struct {
		name    string
		tmpl    string
		want    string
		wantErr bool
	}{
		{
			name: "plain text passthrough",
			tmpl: "Just do the thing.",
			want: "Just do the thing.",
		},
		{
			name: "ticket title",
			tmpl: "Work on: {{ .Ticket.Title }}",
			want: "Work on: Fix the bug",
		},
		{
			name: "ticket id and description",
			tmpl: "[{{ .Ticket.ID }}] {{ .Ticket.Description }}",
			want: "[abc-123] Something is broken",
		},
		{
			name: "file existing",
			tmpl: `Read: {{ file "PLAN.md" }}`,
			want: "Read: the plan",
		},
		{
			name:    "file missing returns error",
			tmpl:    `Read: {{ file "MISSING.md" }}`,
			wantErr: true,
		},
		{
			name: "ticket file path",
			tmpl: "Ticket file: {{ .Ticket.FilePath }}",
			want: "Ticket file: /tmp/tickets/abc-123.md",
		},
		{
			name: "combined",
			tmpl: `Ticket {{ .Ticket.ID }}: {{ .Ticket.Title }}
{{ file "PLAN.md" }}`,
			want: `Ticket abc-123: Fix the bug
the plan`,
		},
		{
			name:    "invalid syntax",
			tmpl:    "{{ invalid }",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.tmpl, data, tmpDir)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
