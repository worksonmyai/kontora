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
func ReportResult(subjectID, inputFile string, stdin io.Reader, out io.Writer, getenv func(string) string, verdictFlag ...string) error {
	inv, err := session.InvocationFromEnv(getenv)
	if err != nil {
		return err
	}
	if subjectID == "" || subjectID != inv.SubjectID() {
		if inv.AutomationID != "" {
			return fmt.Errorf("result automation must exactly match invocation automation %q", inv.AutomationID)
		}
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
	result, err := session.DecodeResult(data, inv.Version)
	if err != nil {
		return err
	}
	if len(verdictFlag) > 1 {
		return fmt.Errorf("only one --verdict value is allowed")
	}
	if len(verdictFlag) == 1 {
		if inv.Version != session.GraphResultVersion {
			return fmt.Errorf("--verdict requires result version 2")
		}
		if strings.TrimSpace(verdictFlag[0]) == "" {
			return fmt.Errorf("--verdict must be nonblank")
		}
		if result.Verdict != "" && result.Verdict != verdictFlag[0] {
			return fmt.Errorf("--verdict conflicts with the JSON verdict")
		}
		result.Verdict = verdictFlag[0]
	}
	if inv.Version == session.GraphResultVersion {
		if err := session.ValidateGraphResult(result, inv.Verdicts); err != nil {
			return err
		}
	}
	if _, err := session.PublishResult(inv.ResultPath, result, inv.Version); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Accepted     bool   `json:"accepted"`
		InvocationID string `json:"invocation_id"`
		Outcome      string `json:"outcome"`
		Verdict      string `json:"verdict,omitempty"`
	}{Accepted: true, InvocationID: inv.ID, Outcome: result.Outcome, Verdict: result.Verdict})
}
