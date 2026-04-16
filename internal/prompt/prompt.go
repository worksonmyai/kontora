package prompt

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

type TicketData struct {
	ID          string
	Title       string
	Description string
	FilePath    string
}

type Data struct {
	Ticket TicketData
}

// Options controls optional data plumbed into template helpers.
type Options struct {
	// ReviewsDir is the absolute path to the plannotator reviews directory.
	// When set, the plannotatorReview helper reads <ReviewsDir>/<ticket.ID>.md
	// and deletes the file after reading. When empty, the helper returns "".
	ReviewsDir string

	// Logger is used for non-fatal warnings (e.g. review file unlink failures).
	// Optional; defaults to slog.Default().
	Logger *slog.Logger
}

// Render is a thin wrapper around RenderWithOptions for the common case
// (no optional helpers configured).
func Render(tmpl string, data Data, workDir string) (string, error) {
	return RenderWithOptions(tmpl, data, workDir, Options{})
}

// RenderWithOptions renders tmpl with the given data, making additional helpers
// available when opts provides the necessary inputs.
func RenderWithOptions(tmpl string, data Data, workDir string, opts Options) (string, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	funcMap := template.FuncMap{
		"file": func(name string) (string, error) {
			b, err := os.ReadFile(filepath.Join(workDir, name))
			if err != nil {
				return "", fmt.Errorf("file %q: %w", name, err)
			}
			return string(b), nil
		},
		"plannotatorReview": func() string {
			if opts.ReviewsDir == "" || data.Ticket.ID == "" {
				return ""
			}
			path := filepath.Join(opts.ReviewsDir, data.Ticket.ID+".md")
			b, err := os.ReadFile(path)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					logger.Warn("plannotatorReview: read failed", "path", path, "err", err)
				}
				return ""
			}
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				logger.Warn("plannotatorReview: unlink failed", "path", path, "err", rmErr)
			}
			return string(b)
		},
	}

	t, err := template.New("prompt").Funcs(funcMap).Parse(tmpl)
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
