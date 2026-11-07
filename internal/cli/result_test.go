package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/session"
)

func TestReportResult(t *testing.T) {
	for _, stage := range []string{"implement", "code review", "проверка кода", "review [security]*?"} {
		t.Run(stage, func(t *testing.T) {
			for _, file := range []bool{false, true} {
				t.Run(map[bool]string{false: "stdin", true: "file"}[file], func(t *testing.T) {
					inv := session.Invocation{Version: 1, ID: "inv-1", TicketID: "kon-test", Stage: stage, ResultPath: filepath.Join(t.TempDir(), "result.json")}
					env := inv.Env()
					getenv := func(k string) string { return env[k] }
					payload := `{"outcome":"blocked","summary":"No device.","next_step":"Provide device."}`
					inputFile := ""
					var input io.Reader = strings.NewReader(payload)
					if file {
						inputFile = filepath.Join(t.TempDir(), "input.json")
						require.NoError(t, os.WriteFile(inputFile, []byte(payload), 0o600))
						input = nil
					}
					for range 2 {
						if !file {
							input = strings.NewReader(payload)
						}
						var out bytes.Buffer
						require.NoError(t, ReportResult("kon-test", inputFile, input, &out, getenv))
						var ack map[string]any
						require.NoError(t, json.Unmarshal(out.Bytes(), &ack))
						assert.Equal(t, true, ack["accepted"])
						assert.Equal(t, "inv-1", ack["invocation_id"])
						assert.Equal(t, "blocked", ack["outcome"])
					}
					got, err := session.ReadResult(inv.ResultPath)
					require.NoError(t, err)
					assert.Equal(t, "blocked", got.Outcome)
					var out bytes.Buffer
					err = ReportResult("kon-test", "", strings.NewReader(`{"outcome":"complete","summary":"Done."}`), &out, getenv)
					require.Error(t, err)
					assert.Empty(t, out.String())
				})
			}
		})
	}
}

func TestReportResultInvocationIsolation(t *testing.T) {
	dir := t.TempDir()
	old := session.Invocation{Version: 1, ID: "old", TicketID: "kon-test", Stage: "implement", ResultPath: filepath.Join(dir, "old.json")}
	current := session.Invocation{Version: 1, ID: "current", TicketID: "kon-test", Stage: "implement", ResultPath: filepath.Join(dir, "current.json")}
	var out bytes.Buffer
	currentEnv := current.Env()
	require.NoError(t, ReportResult("kon-test", "", strings.NewReader(`{"outcome":"blocked","summary":"No device.","next_step":"Provide device."}`), &out, func(k string) string { return currentEnv[k] }))
	before, err := os.ReadFile(current.ResultPath)
	require.NoError(t, err)
	out.Reset()
	oldEnv := old.Env()
	require.NoError(t, ReportResult("kon-test", "", strings.NewReader(`{"outcome":"complete","summary":"Old work."}`), &out, func(k string) string { return oldEnv[k] }))
	assert.Contains(t, out.String(), `"invocation_id":"old"`)
	after, err := os.ReadFile(current.ResultPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestReportResultRejects(t *testing.T) {
	for _, tt := range []struct {
		name, ticket, file, payload string
		noEnv, readError            bool
	}{
		{name: "missing context", ticket: "kon-test", noEnv: true},
		{name: "missing ticket"},
		{name: "different ticket", ticket: "kon-other"},
		{name: "prefix not accepted", ticket: "kon-t"},
		{name: "relative file", ticket: "kon-test", file: "result.json"},
		{name: "dash is not a file", ticket: "kon-test", file: "-"},
		{name: "missing file", ticket: "kon-test", file: "/nonexistent/kontora/result.json"},
		{name: "invalid json", ticket: "kon-test", payload: `{}`},
		{name: "oversized", ticket: "kon-test", payload: strings.Repeat("x", session.MaxResultBytes+1)},
		{name: "read failure", ticket: "kon-test", readError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			env := (session.Invocation{Version: 1, ID: "inv-1", TicketID: "kon-test", Stage: "implement", ResultPath: path}).Env()
			var input io.Reader = strings.NewReader(tt.payload)
			if tt.readError {
				input = resultErrorReader{}
			}
			var out bytes.Buffer
			err := ReportResult(tt.ticket, tt.file, input, &out, func(k string) string {
				if tt.noEnv {
					return ""
				}
				return env[k]
			})
			require.Error(t, err)
			assert.Empty(t, out.String())
			assert.NoFileExists(t, path)
		})
	}
}

func TestReportResultBoundedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	env := (session.Invocation{Version: 1, ID: "inv-1", TicketID: "kon-test", Stage: "implement", ResultPath: path}).Env()
	input := &resultCountingReader{}
	var out bytes.Buffer
	err := ReportResult("kon-test", "", input, &out, func(k string) string { return env[k] })
	require.Error(t, err)
	assert.Equal(t, session.MaxResultBytes+1, input.read)
	assert.Empty(t, out.String())
	assert.NoFileExists(t, path)
}

func TestReportResultAcknowledgementFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	env := (session.Invocation{Version: 1, ID: "inv-1", TicketID: "kon-test", Stage: "implement", ResultPath: path}).Env()
	getenv := func(k string) string { return env[k] }
	payload := `{"outcome":"complete","summary":"Done."}`
	err := ReportResult("kon-test", "", strings.NewReader(payload), resultErrorWriter{}, getenv)
	require.ErrorContains(t, err, "write failed")
	// A failed acknowledgement cannot roll back a published terminal result.
	got, err := session.ReadResult(path)
	require.NoError(t, err)
	assert.Equal(t, "complete", got.Outcome)
	var out bytes.Buffer
	require.NoError(t, ReportResult("kon-test", "", strings.NewReader(payload), &out, getenv))
	assert.Contains(t, out.String(), `"accepted":true`)
}

func TestResultCommandMetadata(t *testing.T) {
	for _, command := range Commands {
		if command.Name != "result" {
			continue
		}
		assert.True(t, command.TicketID)
		assert.False(t, command.Config)
		assert.False(t, command.Store)
		assert.False(t, command.Remote)
		require.Len(t, command.Flags, 1)
		assert.Equal(t, "input-file", command.Flags[0].Name)
		assert.Equal(t, "path", command.Flags[0].Value)
		return
	}
	t.Fatal("result command missing from metadata")
}

type resultCountingReader struct{ read int }

func (r *resultCountingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.read += len(p)
	return len(p), nil
}

type resultErrorWriter struct{}

func (resultErrorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

type resultErrorReader struct{}

func (resultErrorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
