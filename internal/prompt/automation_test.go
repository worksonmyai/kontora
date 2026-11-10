package prompt

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAutomationPromptContextsAndFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes"), 0600))
	data := AutomationData{
		Automation: AutomationContext{ID: "aut-one", Key: "report", Name: "Report", Path: "~/repo"},
		Run:        AutomationRunContext{Number: 12, Branch: "auto/report-12", Date: "2024-01-02"},
		LastRun:    AutomationRunContext{StartedAt: time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC), Result: "done"},
	}
	result, err := RenderAutomation(`{{ .Automation.Name }} {{ .Automation.Path }} #{{ .Run.Number }} {{ .Run.Branch }} {{ .Run.Date }} {{ .LastRun.StartedAt.Format "2006-01-02" }} {{ .LastRun.Result }} {{ file "notes.txt" }}`, data, dir)
	require.NoError(t, err)
	require.Equal(t, "Report ~/repo #12 auto/report-12 2024-01-02 2024-01-01 done notes", result)
}

func TestCompileAutomationDoesNotExecute(t *testing.T) {
	for _, text := range []string{`{{ file "/does/not/exist" }}`, `{{ .LastRun.StartedAt.Format "2006" }}`, `{{ index (slice "x" 20) 0 }}`} {
		_, err := CompileAutomation(text)
		require.NoError(t, err)
	}
	_, err := CompileAutomation(`{{ if }}`)
	require.Error(t, err)
	_, err = CompileAutomation(`{{ plannotatorReview }}`)
	require.Error(t, err)
	_, err = RenderAutomation(`{{ file "missing" }}`, AutomationData{}, t.TempDir())
	require.ErrorContains(t, err, `file "missing"`)
	_, err = RenderAutomation(`{{ .Ticket.ID }}`, AutomationData{}, t.TempDir())
	require.Error(t, err)
	rendered, err := RenderAutomation(`{{ if .LastRun.StartedAt.IsZero }}first run{{ end }}`, AutomationData{}, t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "first run", rendered)
}
