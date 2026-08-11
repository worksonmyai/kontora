package prompt

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/worksonmyai/kontora/internal/ticket"
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
	// ReviewsDir is the plannotator reviews directory. It is used as-is in
	// filepath.Join, so callers are expected to resolve ~ / relative paths
	// before passing it in if they want an absolute path.
	// When set, the plannotatorReview helper reads <ReviewsDir>/<ticket.ID>.md
	// and deletes the file after reading, and plannotatorAnnotations reads
	// <ReviewsDir>/<ticket.ID>.annotations.md and leaves it in place. When
	// empty, both helpers return "".
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
			path, ok := feedbackPath(opts, data, logger, "plannotatorReview", ".md")
			if !ok {
				return ""
			}
			body, ok := readFeedback(path, logger, "plannotatorReview")
			if !ok {
				return ""
			}
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				logger.Warn("plannotatorReview: unlink failed", "path", path, "err", rmErr)
			}
			return body
		},
		// Unlike plannotatorReview, this helper leaves the file in place. The
		// daemon removes it only after the annotation run succeeds, so a failed run
		// can be retried with the same annotations.
		//
		// A read failure fails the render instead of producing an empty set of
		// notes: an agent given no notes reports success, and that success is what
		// deletes the annotations it never read. A missing file still renders
		// empty, because a stage prompt may name the helper with nothing pending.
		"plannotatorAnnotations": func() (string, error) {
			path, ok := feedbackPath(opts, data, logger, "plannotatorAnnotations", AnnotationsSuffix)
			if !ok {
				return "", nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return "", nil
				}
				return "", fmt.Errorf("plannotatorAnnotations: %w", err)
			}
			return string(b), nil
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

// AnnotationsSuffix is the file name suffix under ReviewsDir that holds a
// ticket's pending Plannotator annotations. Code review feedback uses ".md", so
// the two never overwrite each other.
const AnnotationsSuffix = ".annotations.md"

// feedbackPath builds <ReviewsDir>/<ticket.ID><suffix>, or reports ok=false when
// there is no directory, no ticket, or an ID that cannot be a path component.
func feedbackPath(opts Options, data Data, logger *slog.Logger, fn, suffix string) (string, bool) {
	if opts.ReviewsDir == "" || data.Ticket.ID == "" {
		return "", false
	}
	if !ticket.IsSafeID(data.Ticket.ID) {
		logger.Warn(fn+": unsafe ticket id, refusing to build path", "id", data.Ticket.ID)
		return "", false
	}
	return filepath.Join(opts.ReviewsDir, data.Ticket.ID+suffix), true
}

// readFeedback reads path, treating a missing file as "nothing pending" rather
// than an error: a prompt that names the helper must still render when no
// feedback is waiting.
func readFeedback(path string, logger *slog.Logger, fn string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn(fn+": read failed", "path", path, "err", err)
		}
		return "", false
	}
	return string(b), true
}
