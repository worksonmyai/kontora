package cli

import (
	"io"

	"github.com/worksonmyai/kontora/internal/logfmt"
)

// Fmt reads Claude stream-json or session JSONL from r and writes
// human-readable formatted output to w.
func Fmt(r io.Reader, w io.Writer) error {
	return logfmt.Fmt(r, w)
}
