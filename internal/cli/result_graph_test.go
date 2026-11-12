package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/session"
)

func TestReportGraphResult(t *testing.T) {
	for _, tt := range []struct {
		name, outcome, jsonVerdict string
		flag, declarations         []string
		valid                      bool
	}{
		{"JSON verdict", "complete", "changes", nil, []string{"approve", "changes"}, true},
		{"flag verdict", "complete", "", []string{"changes"}, []string{"approve", "changes"}, true},
		{"matching flag", "complete", "changes", []string{"changes"}, []string{"changes"}, true},
		{"conflicting flag", "complete", "changes", []string{"approve"}, []string{"approve", "changes"}, false},
		{"missing verdict", "complete", "", nil, []string{"approve"}, false},
		{"undeclared verdict", "complete", "changes", nil, []string{"approve"}, false},
		{"undeclared flag", "complete", "", []string{"changes"}, []string{"approve"}, false},
		{"blank flag", "complete", "", []string{" "}, []string{"approve"}, false},
		{"multiple flags", "complete", "", []string{"approve", "changes"}, []string{"approve"}, false},
		{"no declarations", "complete", "", nil, nil, true},
		{"no declarations with verdict", "complete", "changes", nil, nil, false},
		{"blocked without verdict", "blocked", "", nil, []string{"approve"}, true},
		{"blocked with verdict", "blocked", "changes", nil, []string{"changes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inv := session.Invocation{Version: session.GraphResultVersion, ID: "inv-2", TicketID: "kon-test", Stage: "review",
				ResultPath: filepath.Join(t.TempDir(), "result.json"), Verdicts: tt.declarations}
			env := inv.Env()
			payload := `{"outcome":"` + tt.outcome + `","summary":"Review finished","next_step":"Provide access"`
			if tt.jsonVerdict != "" {
				payload += `,"verdict":"` + tt.jsonVerdict + `"`
			}
			payload += `}`
			var out bytes.Buffer
			for range 2 {
				out.Reset()
				err := ReportResult(inv.TicketID, "", strings.NewReader(payload), &out, func(k string) string { return env[k] }, tt.flag...)
				if !tt.valid {
					require.Error(t, err)
					assert.Empty(t, out.String())
					assert.NoFileExists(t, inv.ResultPath)
					return
				}
				require.NoError(t, err)
			}
			result, err := session.ReadResult(inv.ResultPath, inv.Version)
			require.NoError(t, err)
			verdict := tt.jsonVerdict
			if len(tt.flag) != 0 {
				verdict = tt.flag[0]
			}
			assert.Equal(t, verdict, result.Verdict)
			assert.Contains(t, out.String(), `"invocation_id":"inv-2"`)
			if verdict != "" {
				assert.Contains(t, out.String(), `"verdict":"`+verdict+`"`)
			}
		})
	}
}

func TestReportGraphResultFileAndConflict(t *testing.T) {
	inv := session.Invocation{Version: session.GraphResultVersion, ID: "inv-2", TicketID: "kon-test", Stage: "review",
		ResultPath: filepath.Join(t.TempDir(), "result.json"), Verdicts: []string{"approve", "changes"}}
	env := inv.Env()
	getenv := func(k string) string { return env[k] }
	inputFile := filepath.Join(t.TempDir(), "input.json")
	require.NoError(t, os.WriteFile(inputFile, []byte(`{"outcome":"complete","summary":"Review done","verdict":"changes"}`), 0o600))
	var out bytes.Buffer
	require.NoError(t, ReportResult(inv.TicketID, inputFile, nil, &out, getenv))
	assert.JSONEq(t, `{"accepted":true,"invocation_id":"inv-2","outcome":"complete","verdict":"changes"}`, out.String())
	before, err := os.ReadFile(inv.ResultPath)
	require.NoError(t, err)
	out.Reset()
	err = ReportResult(inv.TicketID, "", strings.NewReader(`{"outcome":"complete","summary":"Review done","verdict":"approve"}`), &out, getenv)
	require.ErrorContains(t, err, "conflicting")
	assert.Empty(t, out.String())
	after, err := os.ReadFile(inv.ResultPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestReportLegacyResultRejectsVerdict(t *testing.T) {
	for _, flag := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "flag"}[flag], func(t *testing.T) {
			inv := session.Invocation{Version: session.ResultVersion, ID: "inv-1", TicketID: "kon-test", Stage: "review",
				ResultPath: filepath.Join(t.TempDir(), "result.json")}
			env := inv.Env()
			payload := `{"outcome":"complete","summary":"Done","verdict":"approve"}`
			var flags []string
			if flag {
				payload = `{"outcome":"complete","summary":"Done"}`
				flags = []string{"approve"}
			}
			var out bytes.Buffer
			err := ReportResult(inv.TicketID, "", strings.NewReader(payload), &out, func(k string) string { return env[k] }, flags...)
			require.Error(t, err)
			assert.Empty(t, out.String())
			assert.NoFileExists(t, inv.ResultPath)
		})
	}
}
