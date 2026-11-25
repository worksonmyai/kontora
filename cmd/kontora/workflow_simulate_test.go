package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestWorkflowSimulateRecipe(t *testing.T) {
	const (
		implement = `{"node":"implement"}`
		pass      = `{"node":"tests","exit_code":0}`
		fail      = `{"node":"tests","exit_code":1}`
		exitTwo   = `{"node":"tests","exit_code":2}`
		repair    = `{"node":"repair"}`
		approve   = `{"node":"review","verdict":"approved"}`
		findings  = `{"node":"review","verdict":"changes_requested"}`
		release   = `{"node":"release","wait":"released"}`
		timeout   = `{"node":"release","wait":"timeout"}`

		handoff    = "Final: handoff (end, status=human_review)"
		inspection = "Final: inspection (end, status=paused)"
	)
	for _, tc := range []struct {
		name        string
		events      []string
		final       string
		portsInPath []string
		repairs     int
		rejected    bool
	}{
		{name: "clean success", events: []string{implement, pass, approve, release}, final: handoff,
			portsInPath: []string{"implement --complete--> tests", "tests --pass--> review", "review --complete--> approved", "approved --true--> release", "release --released--> handoff"}},
		{name: "repair then release", events: []string{implement, fail, repair, pass, approve, release}, final: handoff, repairs: 1,
			portsInPath: []string{"tests --fail--> repairable", "repairable --true--> repair-budget", "repair-budget --repeat--> repair (loop count 1)", "repair --complete--> tests", "tests --pass--> review", "approved --true--> release", "release --released--> handoff"}},
		{name: "two repairs can succeed", events: []string{implement, fail, repair, fail, repair, pass, approve, release}, final: handoff, repairs: 2},
		{name: "two repairs then exhaustion", events: []string{implement, fail, repair, fail, repair, fail}, final: inspection + ": loop repair-budget exhausted", repairs: 2,
			portsInPath: []string{"repair-budget --repeat--> repair (loop count 1)", "repair-budget --repeat--> repair (loop count 2)", "repair-budget --exhausted--> inspection (loop count 2)"}},
		{name: "exit two goes straight to inspection", events: []string{implement, exitTwo}, final: inspection,
			portsInPath: []string{"tests --fail--> repairable", "repairable --false--> inspection"}},
		{name: "findings are not approval", events: []string{implement, pass, findings}, final: inspection, portsInPath: []string{"approved --false--> inspection"}},
		{name: "wait timeout", events: []string{implement, pass, approve, timeout}, final: inspection, portsInPath: []string{"release --timeout--> inspection"}},
		{name: "implement failure", events: []string{`{"node":"implement","outcome":"failed"}`}, final: inspection, portsInPath: []string{"implement --failed--> inspection"}},
		{name: "repair failure", events: []string{implement, fail, `{"node":"repair","outcome":"failed"}`}, final: inspection, repairs: 1, portsInPath: []string{"repair --failed--> inspection"}},
		{name: "review failure", events: []string{implement, pass, `{"node":"review","outcome":"failed"}`}, final: inspection, portsInPath: []string{"review --failed--> inspection"}},
		{name: "waiting", events: []string{implement, pass, approve}, final: "Final: release (waiting, status=paused)"},
		{name: "missing input", events: []string{implement}, final: "Final: tests (missing input, status=todo)"},
		{name: "wrong node", events: []string{implement, approve}, final: `Final: tests (rejected, status=paused): event 2: expected node "tests", got "review"`, rejected: true},
		{name: "undeclared verdict", events: []string{implement, pass, `{"node":"review","verdict":"other"}`}, final: "event 3: undeclared agent verdict", rejected: true},
		{name: "invalid wait outcome", events: []string{implement, pass, approve, `{"node":"release","wait":"approved"}`}, final: "event 4: invalid declared wait release port", rejected: true},
		{name: "wait with null outcome", events: []string{implement, pass, approve, `{"node":"release","wait":"released","outcome":null}`}, final: "event 4: expected a wait outcome (released or timeout) without completion fields", rejected: true},
		{name: "wait with empty outcome", events: []string{implement, pass, approve, `{"node":"release","wait":"released","outcome":""}`}, final: "event 4: expected a wait outcome (released or timeout) without completion fields", rejected: true},
		{name: "wait with null verdict", events: []string{implement, pass, approve, `{"node":"release","wait":"released","verdict":null}`}, final: "event 4: expected a wait outcome (released or timeout) without completion fields", rejected: true},
		{name: "wait with empty verdict", events: []string{implement, pass, approve, `{"node":"release","wait":"released","verdict":""}`}, final: "event 4: expected a wait outcome (released or timeout) without completion fields", rejected: true},
		{name: "wait with null exit code", events: []string{implement, pass, approve, `{"node":"release","wait":"released","exit_code":null}`}, final: "event 4: expected a wait outcome (released or timeout) without completion fields", rejected: true},
		{name: "check with wait instead of exit code", events: []string{implement, `{"node":"tests","wait":"released"}`}, final: "event 2: wait is only valid at a declared wait", rejected: true},
		{name: "check without exit code", events: []string{implement, `{"node":"tests"}`}, final: "event 2: check completion requires exit_code", rejected: true},
		{name: "agent with empty wait", events: []string{`{"node":"implement","wait":""}`}, final: "event 1: wait is only valid at a declared wait", rejected: true},
		{name: "agent with null wait", events: []string{`{"node":"implement","wait":null}`}, final: "event 1: wait is only valid at a declared wait", rejected: true},
		{name: "extra event", events: []string{implement, exitTwo, release}, final: `event 3: no input expected after end node "inspection"`, rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scenario := filepath.Join(dir, "scenario.json")
			data := "[" + strings.Join(tc.events, ",") + "]"
			require.NoError(t, os.WriteFile(scenario, []byte(data), 0o644))
			env := []string{
				"HOME=" + dir, "XDG_CONFIG_HOME=" + dir,
				config.PathEnvVar + "=" + filepath.Join(dir, "not-created.yaml"),
				config.TicketsDirEnvVar + "=" + filepath.Join(dir, "not-created-store"),
				"PATH=" + dir,
			}
			stdout, stderr, err := runCLISeparated(t, env, nil, "workflow-simulate", "--recipe", "implement-test-review", "--scenario", scenario)
			if tc.rejected {
				require.Error(t, err)
				assert.Contains(t, stderr, "scenario transition rejected")
			} else {
				require.NoError(t, err, stderr)
				assert.Empty(t, stderr)
			}
			assert.Contains(t, stdout, tc.final)
			assert.Equal(t, tc.repairs, strings.Count(stdout, "repair-budget --repeat--> repair"))
			rest := stdout
			for _, edge := range tc.portsInPath {
				_, remaining, found := strings.Cut(rest, edge)
				require.True(t, found, "expected edge %q in order in %s", edge, stdout)
				rest = remaining
			}
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Len(t, entries, 1, "simulation must not create config, tickets, worktrees, or logs")
			assert.Equal(t, "scenario.json", entries[0].Name())
		})
	}
	stdout, stderr, err := runCLISeparated(t, nil, nil, "workflow-simulate", "--recipe", "implement-test-review", "--preflight")
	require.NoError(t, err, stderr)
	assert.Contains(t, stdout, "Preflight found no rejected paths")
}

func TestWorkflowSimulateExplicitConfigOffline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("offline command contacted daemon: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	command := "touch " + filepath.Join(dir, "runner-or-hook-ran")
	data := fmt.Sprintf(`default_agent: fake
agents:
  fake: {binary: does-not-exist}
tickets_dir: %q
worktrees_dir: %q
logs_dir: %q
hooks:
  stage_start:
    - run: %q
pipelines:
  mixed:
    version: 2
    entry: tests
    nodes:
      - {id: tests, type: check, command: %q, timeout: 1m}
      - {id: decision, type: condition, when: {field: verdict, equals: approved}}
      - {id: ok, type: end, status: done}
      - {id: no, type: end, status: paused}
    edges:
      - {from: {node: tests, port: pass}, to: decision}
      - {from: {node: tests, port: fail}, to: no}
      - {from: {node: decision, port: 'true'}, to: ok}
      - {from: {node: decision, port: 'false'}, to: no}
`, filepath.Join(dir, "tickets"), filepath.Join(dir, "worktrees"), filepath.Join(dir, "logs"), command, command)
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))
	// Ambient config and the configured daemon must not override explicit offline inputs.
	env := []string{"KONTORA_URL=" + server.URL, config.PathEnvVar + "=" + filepath.Join(dir, "missing.yaml")}
	stdout, stderr, err := runCLISeparated(t, env, nil, "workflow-simulate", "--config", path, "--pipeline", "mixed", "--preflight")
	require.Error(t, err)
	assert.Contains(t, stderr, "preflight found rejected paths")
	assert.Contains(t, stdout, `Rejected at "decision": condition requires a successful declared agent verdict`)
	assert.Contains(t, stdout, `Producer "tests": complete, exit_code=0`)
	assert.Contains(t, stdout, "tests --pass--> decision")

	scenario := filepath.Join(dir, "scenario.json")
	require.NoError(t, os.WriteFile(scenario, []byte(`[{"node":"tests","exit_code":2}]`), 0o644))
	stdout, stderr, err = runCLISeparated(t, env, nil, "workflow-simulate", "--config", path, "--pipeline", "mixed", "--scenario", scenario)
	require.NoError(t, err, stderr)
	assert.Contains(t, stdout, "Final: no (end, status=paused)")
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, string(contents))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "no runtime artifacts or command side effects")
}

func TestWorkflowSimulateInvalidInput(t *testing.T) {
	dir := t.TempDir()
	scenario := filepath.Join(dir, "scenario.json")
	const recipe = "--recipe=implement-test-review"
	for _, tc := range []struct {
		name, input, want string
		args              []string
	}{
		{name: "no source", args: []string{"--preflight"}, want: "select either --recipe"},
		{name: "no mode", args: []string{recipe}, want: "select exactly one"},
		{name: "two modes", args: []string{recipe, "--preflight", "--scenario", scenario}, want: "select exactly one"},
		{name: "two sources", args: []string{recipe, "--preflight", "--config", "missing.yaml"}, want: "--recipe cannot be combined"},
		{name: "no explicit config", args: []string{"--pipeline=mixed", "--preflight"}, want: "both --config FILE and --pipeline NAME"},
		{name: "no explicit pipeline", args: []string{"--config=missing.yaml", "--preflight"}, want: "both --config FILE and --pipeline NAME"},
		{name: "legacy recipe", args: []string{"--recipe=plan-only", "--preflight"}, want: "unknown graph recipe"},
		{name: "unknown field", input: `[{"node":"implement","command":"exit 2"}]`, want: `unknown field "command"`},
		{name: "trailing input", input: `[] []`, want: "unexpected data after scenario array"},
		{name: "null input", input: `null`, want: "expected a JSON array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if tc.input != "" {
				require.NoError(t, os.WriteFile(scenario, []byte(tc.input), 0o644))
				args = []string{recipe, "--scenario", scenario}
			}
			stdout, stderr, err := runCLISeparated(t, nil, nil, append([]string{"workflow-simulate"}, args...)...)
			require.Error(t, err)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, tc.want)
		})
	}
}
