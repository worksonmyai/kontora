package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/worksonmyai/kontora/internal/session"
)

// ReportResult submits terminal stage output only to the environment-bound
// invocation. inputFile, when present, must be absolute; otherwise stdin is read.
// It needs neither ticket-store access nor daemon connectivity. An acknowledgement
// is emitted only after immutable publication (including identical resubmission).
func ReportResult(ticketID, inputFile string, stdin io.Reader, out io.Writer, getenv func(string) string) error {
	inv, err := session.InvocationFromEnv(getenv)
	if err != nil {
		return err
	}
	if ticketID == "" || ticketID != inv.TicketID {
		return fmt.Errorf("result ticket must exactly match invocation ticket %q", inv.TicketID)
	}
	input := stdin
	if inputFile != "" {
		if !utf8.ValidString(inputFile) || strings.ContainsRune(inputFile, '\x00') || !filepath.IsAbs(inputFile) {
			return fmt.Errorf("--input-file must be an absolute path")
		}
		info, err := os.Stat(inputFile)
		if err != nil {
			return fmt.Errorf("reading result input: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("result input must be a regular file")
		}
		f, err := os.Open(inputFile)
		if err != nil {
			return fmt.Errorf("opening result input: %w", err)
		}
		defer f.Close()
		input = f
	}
	if input == nil {
		return fmt.Errorf("result input is unavailable")
	}
	if out == nil {
		return fmt.Errorf("result acknowledgement output is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(input, session.MaxResultBytes+1))
	if err != nil {
		return fmt.Errorf("reading result input: %w", err)
	}
	result, err := session.DecodeResult(data)
	if err != nil {
		return err
	}
	if _, err := session.PublishResult(inv.ResultPath, result); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Accepted     bool   `json:"accepted"`
		InvocationID string `json:"invocation_id"`
		Outcome      string `json:"outcome"`
	}{Accepted: true, InvocationID: inv.ID, Outcome: result.Outcome})
}
