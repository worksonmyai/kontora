package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
)

const localConditionID = "tst-condition"

func localConditionFixture(t *testing.T, logsDir string) (cwd, configPath, ticketPath string) {
	t.Helper()
	cwd = t.TempDir()
	configPath = filepath.Join(t.TempDir(), "config.yaml")
	ticketsDir := t.TempDir()
	ticketPath = filepath.Join(ticketsDir, localConditionID+".md")
	conf := fmt.Sprintf(`tickets_dir: %q
logs_dir: %q
default_agent: fake
agents:
  fake: {binary: unused}
pipelines:
  graph:
    version: 2
    entry: check
    nodes:
      - {id: check, type: check, command: 'exit 0', timeout: 1m}
      - {id: choose, type: condition, command: 'exit 1'}
      - {id: yes, type: end, status: done}
      - {id: no, type: end, status: paused}
    edges:
      - {from: {node: check, port: pass}, to: choose}
      - {from: {node: check, port: fail}, to: no}
      - {from: {node: choose, port: 'true'}, to: yes}
      - {from: {node: choose, port: 'false'}, to: no}
`, ticketsDir, logsDir)
	require.NoError(t, os.WriteFile(configPath, []byte(conf), 0o600))
	require.NoError(t, os.WriteFile(ticketPath, []byte(`---
id: tst-condition
kontora: true
status: paused
pipeline: graph
stage: choose
last_error: condition termination is uncertain
graph_state:
  version: 2
  execution_id: execution
  revision: 8
  definition_fingerprint: fingerprint
---
# Condition ticket
`), 0o600))
	return cwd, configPath, ticketPath
}

func TestLocalLogsCommandCondition(t *testing.T) {
	cwd, configPath, _ := localConditionFixture(t, "logs")
	logDir := filepath.Join(cwd, "logs", localConditionID)
	conditions := filepath.Join(logDir, "conditions")
	require.NoError(t, os.MkdirAll(conditions, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "check.log"), []byte("producer output\n"), 0o600))
	for _, tc := range []struct {
		name, stage, want string
		latest            bool
	}{
		{name: "latest evaluation", stage: "choose", want: "condition output\n", latest: true},
		{name: "condition without an evaluation", stage: "choose", want: "No condition log found.\n"},
		{name: "ordinary stage", stage: "check", want: "producer output\n"},
		{name: "ordinary fallback", stage: "unknown", want: "producer output\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.latest {
				path := filepath.Join(conditions, "choose.00000000000000000001-a.log")
				require.NoError(t, os.WriteFile(path, []byte("condition output\n"), 0o600))
				t.Cleanup(func() { require.NoError(t, os.Remove(path)) })
			}
			out, err := runCLIInDir(t, cwd, nil, nil, "logs", "--config", configPath, "--stage", tc.stage, "tst-cond")
			require.NoError(t, err, out)
			assert.Equal(t, tc.want, out)
		})
	}
}

func TestLocalRetryConditionJournal(t *testing.T) {
	for _, tc := range []struct {
		name, configured, journal, wantError string
		statError                            bool
	}{
		{name: "relative stopped journal", configured: "logs", journal: `{"phase":"stopped"}`, wantError: "retry blocked by workflow condition journal"},
		{name: "tilde unreadable journal", configured: "~/logs", journal: `{`, wantError: "retry blocked by workflow condition journal"},
		{name: "cannot check journal", configured: "logs", statError: true, wantError: "cannot check workflow condition journal"},
		{name: "no journal", configured: "logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd, configPath, ticketPath := localConditionFixture(t, tc.configured)
			before, err := os.ReadFile(ticketPath)
			require.NoError(t, err)
			logDir := filepath.Join(cwd, "logs", localConditionID)
			require.NoError(t, os.MkdirAll(filepath.Dir(logDir), 0o700))
			if tc.statError {
				require.NoError(t, os.Symlink(localConditionID, logDir))
			}
			if tc.journal != "" {
				require.NoError(t, os.MkdirAll(logDir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(logDir, "workflow-condition.json"), []byte(tc.journal), 0o600))
			}

			out, err := runCLIInDir(t, cwd, []string{"HOME=" + cwd}, nil, "retry", "--config", configPath, "tst-cond")
			after, readErr := os.ReadFile(ticketPath)
			require.NoError(t, readErr)
			if tc.wantError != "" {
				require.Error(t, err, out)
				assert.Contains(t, out, tc.wantError)
				assert.NotContains(t, out, "re-queued")
				assert.Equal(t, before, after)
				return
			}
			require.NoError(t, err, out)
			assert.Contains(t, out, "re-queued")
			stored, err := ticket.ParseBytes(after)
			require.NoError(t, err)
			assert.Equal(t, ticket.StatusTodo, stored.Status)
			assert.Empty(t, stored.LastError)
			require.NotNil(t, stored.GraphState)
			assert.EqualValues(t, 9, stored.GraphState.Revision)
		})
	}
}
