package prompt

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

func TestPlannotatorReview(t *testing.T) {
	data := Data{
		Ticket: TicketData{ID: "kon-001", Title: "Fix bug"},
	}

	t.Run("reads and deletes file", func(t *testing.T) {
		reviewsDir := t.TempDir()
		reviewPath := filepath.Join(reviewsDir, "kon-001.md")
		content := "line one\nline two\nline three\n"
		require.NoError(t, os.WriteFile(reviewPath, []byte(content), 0o644))

		out, err := RenderWithOptions(`{{ plannotatorReview }}`, data, "", Options{
			ReviewsDir: reviewsDir,
			Logger:     discardLogger(),
		})
		require.NoError(t, err)
		assert.Equal(t, content, out)

		_, statErr := os.Stat(reviewPath)
		assert.True(t, os.IsNotExist(statErr), "review file should be deleted after read")
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		reviewsDir := t.TempDir()
		out, err := RenderWithOptions(`X{{ plannotatorReview }}X`, data, "", Options{
			ReviewsDir: reviewsDir,
			Logger:     discardLogger(),
		})
		require.NoError(t, err)
		assert.Equal(t, "XX", out)
	})

	t.Run("missing reviews_dir returns empty", func(t *testing.T) {
		out, err := RenderWithOptions(`[{{ plannotatorReview }}]`, data, "", Options{
			Logger: discardLogger(),
		})
		require.NoError(t, err)
		assert.Equal(t, "[]", out)
	})

	t.Run("preserves multiline content in full prompt", func(t *testing.T) {
		reviewsDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(reviewsDir, "kon-001.md"), []byte("a\n\nb"), 0o644))

		tmpl := `Hello {{ .Ticket.Title }}
feedback:
{{ plannotatorReview }}
end`
		out, err := RenderWithOptions(tmpl, data, "", Options{
			ReviewsDir: reviewsDir,
			Logger:     discardLogger(),
		})
		require.NoError(t, err)
		assert.Equal(t, "Hello Fix bug\nfeedback:\na\n\nb\nend", out)
	})

	t.Run("empty ticket ID returns empty", func(t *testing.T) {
		reviewsDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(reviewsDir, ".md"), []byte("leak"), 0o644))

		out, err := RenderWithOptions(`{{ plannotatorReview }}`, Data{}, "", Options{
			ReviewsDir: reviewsDir,
			Logger:     discardLogger(),
		})
		require.NoError(t, err)
		assert.Empty(t, out)
	})
}

// TestPlannotatorAnnotations covers the annotation helper. The difference that
// matters is the one this table pins first: it must not unlink the file, so a
// failed refine run can be retried against the same feedback.
func TestPlannotatorAnnotations(t *testing.T) {
	cases := []struct {
		name string
		// id is the ticket ID the prompt renders with.
		id string
		// files are written under the reviews dir before rendering.
		files map[string]string
		// noReviewsDir renders without a reviews directory configured.
		noReviewsDir bool
		want         string
	}{
		{
			name:  "reads the annotations file",
			id:    "kon-001",
			files: map[string]string{"kon-001.annotations.md": "note one\n\nnote two\n"},
			want:  "[note one\n\nnote two\n]",
		},
		{
			name:  "code review feedback is a different file",
			id:    "kon-001",
			files: map[string]string{"kon-001.md": "review feedback"},
			want:  "[]",
		},
		{
			name:  "missing file renders empty",
			id:    "kon-001",
			want:  "[]",
			files: nil,
		},
		{
			name:         "no reviews dir renders empty",
			id:           "kon-001",
			noReviewsDir: true,
			want:         "[]",
		},
		{
			name:  "empty ticket id renders empty",
			id:    "",
			files: map[string]string{".annotations.md": "leak"},
			want:  "[]",
		},
		{
			name:  "traversal in the ticket id is refused",
			id:    "../escape",
			files: map[string]string{"escape.annotations.md": "leak"},
			want:  "[]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reviewsDir := t.TempDir()
			for name, content := range tc.files {
				require.NoError(t, os.WriteFile(filepath.Join(reviewsDir, name), []byte(content), 0o644))
			}
			opts := Options{ReviewsDir: reviewsDir, Logger: discardLogger()}
			if tc.noReviewsDir {
				opts.ReviewsDir = ""
			}
			data := Data{Ticket: TicketData{ID: tc.id}}

			// Rendering twice proves the read leaves the file in place.
			for range 2 {
				out, err := RenderWithOptions(`[{{ plannotatorAnnotations }}]`, data, "", opts)
				require.NoError(t, err)
				assert.Equal(t, tc.want, out)
			}
		})
	}
}

// TestPlannotatorFeedbackFilesAreIndependent proves an annotation and a pending
// code review can wait for the same ticket at once: each helper reads only its
// own file, and consuming the review leaves the annotations alone.
func TestPlannotatorFeedbackFilesAreIndependent(t *testing.T) {
	reviewsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(reviewsDir, "kon-001.md"), []byte("review"), 0o644))
	annotations := filepath.Join(reviewsDir, "kon-001"+AnnotationsSuffix)
	require.NoError(t, os.WriteFile(annotations, []byte("annotations"), 0o644))

	data := Data{Ticket: TicketData{ID: "kon-001"}}
	opts := Options{ReviewsDir: reviewsDir, Logger: discardLogger()}

	out, err := RenderWithOptions(`{{ plannotatorReview }}|{{ plannotatorAnnotations }}`, data, "", opts)
	require.NoError(t, err)
	assert.Equal(t, "review|annotations", out)

	assert.NoFileExists(t, filepath.Join(reviewsDir, "kon-001.md"), "the review is consumed")
	assert.FileExists(t, annotations, "the annotations survive")
}
