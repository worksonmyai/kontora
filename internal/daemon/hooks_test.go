package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// appendHook returns a hook that appends line to the file at path. The line is
// double-quoted so that a KONTORA_* reference in it expands.
func appendHook(name, path, line string) config.Hook {
	return config.Hook{Name: name, Run: fmt.Sprintf(`printf '%%s\n' "%s" >> %s`, line, path)}
}

// readLines returns the non-empty lines of the file at path, or nil when it
// does not exist, so a test can assert that nothing was recorded.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var lines []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// waitForLines waits until path holds want lines and returns them. A hook and
// the status change that follows it are separate writes, so a test that reads
// the file as soon as the status lands can catch it half-written.
func waitForLines(t *testing.T, path string, want int) []string {
	t.Helper()
	var lines []string
	require.Eventually(t, func() bool {
		lines = readLines(t, path)
		return len(lines) >= want
	}, 5*time.Second, 25*time.Millisecond, "want %d lines in %s, have %v", want, path, lines)
	return lines
}

// startDaemon runs d until the returned stop function is called.
func startDaemon(t *testing.T, d *Daemon) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	return func() {
		cancel()
		require.NoError(t, <-errCh)
	}
}

// TestHooksLifecycleOrder pins the boundaries the three events sit on: the
// worktree is prepared before the agent starts, and the post-run event comes
// after the agent has exited.
func TestHooksLifecycleOrder(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	orderLog := filepath.Join(dir, "order.log")
	cwdFile := filepath.Join(dir, "cwd.txt")

	h.cfg.Hooks = config.Hooks{
		config.HookWorktreeCreated: {config.Hook{
			Name: "setup",
			Run:  fmt.Sprintf(`printf '%%s\n' "$KONTORA_EVENT" >> %s; pwd > %s`, orderLog, cwdFile),
		}},
		config.HookStageStart: {appendHook("before", orderLog, "$KONTORA_EVENT")},
		config.HookStageEnd:   {appendHook("after", orderLog, "$KONTORA_EVENT")},
	}
	h.cfg.Agents["agent1"] = config.Agent{Binary: writeScript(t, fmt.Sprintf("echo agent >> %s\n", orderLog))}

	d := h.newDaemon(h.cfg)
	stop := startDaemon(t, d)
	defer stop()

	h.writeTicket("tst-h01.md", h.taskMD("tst-h01", "todo", "one-stage"))
	h.waitForStatus("tst-h01.md", ticket.StatusDone, 15*time.Second)

	assert.Equal(t, []string{"worktree_created", "stage_start", "agent", "stage_end"},
		waitForLines(t, orderLog, 4))

	// The worktree the hook ran in is removed once the ticket completes, so the
	// path it recorded is compared rather than stat'ed.
	h.waitForWorktreeGone("tst-h01", 5*time.Second)
	assert.Equal(t, []string{filepath.Join(evalDir(t, h.wtDir), h.repoName, "tst-h01")},
		readLines(t, cwdFile))
}

// evalDir resolves the symlinks in dir, which macOS temp directories sit behind
// and a shell's pwd reports resolved.
func evalDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return resolved
}

// TestHooksSimpleTicketLifecycle pins that a ticket without a pipeline fires
// the same three events. Its single run is a stage in every sense that matters
// here, and a stage_start without a stage_end would leave a hook pair unmatched.
func TestHooksSimpleTicketLifecycle(t *testing.T) {
	h := newHarness(t)
	orderLog := filepath.Join(t.TempDir(), "order.log")

	h.cfg.Hooks = config.Hooks{
		config.HookWorktreeCreated: {appendHook("created", orderLog, "$KONTORA_EVENT $KONTORA_STAGE")},
		config.HookStageStart:      {appendHook("before", orderLog, "$KONTORA_EVENT $KONTORA_STAGE")},
		config.HookStageEnd:        {appendHook("after", orderLog, "$KONTORA_EVENT ${KONTORA_EXIT_CODE}")},
	}

	d := h.newDaemon(h.cfg)
	stop := startDaemon(t, d)
	defer stop()

	h.writeTicket("tst-h08.md", simpleTaskMD("tst-h08", "todo", h.repoDir))
	h.waitForStatus("tst-h08.md", ticket.StatusDone, 15*time.Second)

	assert.Equal(t, []string{
		"worktree_created default",
		"stage_start default",
		"stage_end 0",
	}, waitForLines(t, orderLog, 3))
}

// TestHooksEnvironment pins the KONTORA_* set, the inherited environment, and
// the log every hook's output is appended to.
func TestHooksEnvironment(t *testing.T) {
	t.Setenv("PARENT_SENTINEL", "present")
	h := newHarness(t)
	dir := t.TempDir()
	ctxFile := filepath.Join(dir, "ctx.txt")

	h.cfg.Projects = map[string]config.Project{"repo": {Path: h.repoDir}}
	h.cfg.Hooks = config.Hooks{
		config.HookWorktreeCreated: {config.Hook{
			Name: "context",
			Run: fmt.Sprintf(`pwd > %s && printf '%%s\n' "$KONTORA_EVENT" "$KONTORA_TICKET_ID" `+
				`"$KONTORA_TICKET_FILE" "$KONTORA_WORKTREE" "$KONTORA_REPO_PATH" "$KONTORA_BRANCH" `+
				`"$KONTORA_STAGE" "$KONTORA_AGENT" "$KONTORA_PROJECT" "$PARENT_SENTINEL" >> %s && echo hook-output`,
				ctxFile, ctxFile),
		}},
	}

	d := h.newDaemon(h.cfg)
	stop := startDaemon(t, d)
	defer stop()

	h.writeTicket("tst-h02.md", h.taskMD("tst-h02", "todo", "one-stage"))
	got := h.waitForStatus("tst-h02.md", ticket.StatusDone, 15*time.Second)

	// The worktree path a hook is given is the one git reports, with the
	// symlink macOS temp directories sit behind already resolved.
	wtPath := filepath.Join(evalDir(t, h.wtDir), h.repoName, "tst-h02")
	assert.Equal(t, []string{
		wtPath,
		"worktree_created",
		"tst-h02",
		filepath.Join(h.tasksDir, "tst-h02.md"),
		wtPath,
		h.repoDir,
		got.Branch,
		"step1",
		"agent1",
		"repo",
		"present",
	}, waitForLines(t, ctxFile, 11))
	assert.NotEmpty(t, got.Branch)

	hookLog := filepath.Join(h.logsDir, "tst-h02", "hooks", "hooks.log")
	logged := strings.Join(waitForLines(t, hookLog, 2), "\n")
	assert.Contains(t, logged, "hook-output")
	assert.Contains(t, logged, "worktree_created context",
		"each hook's output should be announced, so one hook's lines can be told from the next one's")
}

// TestHooksFailurePolicy covers what a failing hook does to the ticket at each
// event, under the default policy and under an explicit one.
func TestHooksFailurePolicy(t *testing.T) {
	tests := []struct {
		name string
		// event and onFailure configure the one failing hook.
		event      string
		onFailure  string
		wantStatus ticket.Status
		// wantAgentRan is whether the stage's agent was spawned at all.
		wantAgentRan bool
		wantError    string
	}{
		{
			name:       "worktree_created pauses by default",
			event:      config.HookWorktreeCreated,
			wantStatus: ticket.StatusPaused,
			wantError:  "hook bootstrap: exited with code 1",
		},
		{
			name:         "worktree_created set to warn lets the stage run",
			event:        config.HookWorktreeCreated,
			onFailure:    config.HookOnFailureWarn,
			wantStatus:   ticket.StatusDone,
			wantAgentRan: true,
		},
		{
			name:       "stage_start pauses by default",
			event:      config.HookStageStart,
			wantStatus: ticket.StatusPaused,
			wantError:  "hook bootstrap: exited with code 1",
		},
		{
			name:         "stage_end warns by default",
			event:        config.HookStageEnd,
			wantStatus:   ticket.StatusDone,
			wantAgentRan: true,
		},
		{
			name:         "stage_end set to pause stops the ticket",
			event:        config.HookStageEnd,
			onFailure:    config.HookOnFailurePause,
			wantStatus:   ticket.StatusPaused,
			wantAgentRan: true,
			wantError:    "hook bootstrap: exited with code 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ranFile := filepath.Join(t.TempDir(), "agent.log")

			h.cfg.Hooks = config.Hooks{tt.event: {config.Hook{
				Name:      "bootstrap",
				Run:       "exit 1",
				OnFailure: tt.onFailure,
			}}}
			h.cfg.Agents["agent1"] = config.Agent{Binary: writeScript(t, fmt.Sprintf("echo agent >> %s\n", ranFile))}

			d := h.newDaemon(h.cfg)
			stop := startDaemon(t, d)
			defer stop()

			h.writeTicket("tst-h03.md", h.taskMD("tst-h03", "todo", "one-stage"))
			got := h.waitForStatus("tst-h03.md", tt.wantStatus, 15*time.Second)

			if tt.wantError == "" {
				assert.Empty(t, got.LastError)
			} else {
				assert.Contains(t, got.LastError, tt.wantError)
				assert.Contains(t, got.Body, tt.wantError, "the failure should be recorded as a note")
			}
			if tt.wantAgentRan {
				assert.FileExists(t, ranFile, "the agent should have run")
			} else {
				assert.NoFileExists(t, ranFile, "the agent must not be spawned")
			}
			if tt.wantStatus == ticket.StatusPaused {
				assert.Nil(t, got.CompletedAt,
					"a hook that stops the ticket must not leave it carrying a completion time")
			}
		})
	}
}

// TestHooksWorktreeCreatedFailureRemovesWorktree pins the retry contract: a
// half-prepared worktree is removed, so the next pickup runs the hook again on
// a worktree the daemon just created rather than reusing the broken one.
func TestHooksWorktreeCreatedFailureRemovesWorktree(t *testing.T) {
	h := newHarness(t)
	runLog := filepath.Join(t.TempDir(), "runs.log")
	ignoreInRepo(t, h.repoDir, ".env")

	h.cfg.Hooks = config.Hooks{config.HookWorktreeCreated: {config.Hook{
		Name: "bootstrap",
		// The hook writes only a gitignored file, so the worktree it leaves
		// behind is clean and removable.
		Run: fmt.Sprintf("echo SECRET=1 > .env; echo ran >> %s; exit 1", runLog),
	}}}

	d := h.newDaemon(h.cfg)
	stop := startDaemon(t, d)
	defer stop()

	path := h.writeTicket("tst-h04.md", h.taskMD("tst-h04", "todo", "one-stage"))
	got := h.waitForStatus("tst-h04.md", ticket.StatusPaused, 15*time.Second)
	assert.Contains(t, got.LastError, "hook bootstrap: exited with code 1")
	assert.Len(t, waitForLines(t, runLog, 1), 1)
	h.waitForWorktreeGone("tst-h04", 5*time.Second)

	// Retrying recreates the worktree, so the hook runs a second time.
	require.NoError(t, os.WriteFile(path,
		[]byte(h.taskMD("tst-h04", "todo", "one-stage")), 0o644))
	assert.Len(t, waitForLines(t, runLog, 2), 2)
	h.waitForStatus("tst-h04.md", ticket.StatusPaused, 15*time.Second)
	h.waitForWorktreeGone("tst-h04", 5*time.Second)
}

// TestHooksScopeOrder pins the merge order of the two scopes and what an
// unconfigured repository resolves to.
func TestHooksScopeOrder(t *testing.T) {
	tests := []struct {
		name string
		// projectPath is the path the project entry claims. Only a path equal to
		// the ticket's repository matches it.
		projectPath string
		want        []string
	}{
		{name: "both scopes run, global first", projectPath: "", want: []string{"global", "project"}},
		{name: "unmatched repository runs global only", projectPath: "/repos/elsewhere", want: []string{"global"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			orderLog := filepath.Join(t.TempDir(), "scope.log")

			projectPath := tt.projectPath
			if projectPath == "" {
				projectPath = h.repoDir
			}
			h.cfg.Hooks = config.Hooks{
				config.HookWorktreeCreated: {appendHook("global", orderLog, "global")},
			}
			h.cfg.Projects = map[string]config.Project{"repo": {
				Path: projectPath,
				Hooks: config.Hooks{
					config.HookWorktreeCreated: {appendHook("project", orderLog, "project")},
				},
			}}

			d := h.newDaemon(h.cfg)
			stop := startDaemon(t, d)
			defer stop()

			h.writeTicket("tst-h05.md", h.taskMD("tst-h05", "todo", "one-stage"))
			h.waitForStatus("tst-h05.md", ticket.StatusDone, 15*time.Second)

			assert.Equal(t, tt.want, waitForLines(t, orderLog, len(tt.want)))
		})
	}
}

// TestHooksWorktreeReuseAndAnnotation pins the two runs that must not produce a
// worktree_created event: the second stage of a pipeline, which reuses the
// worktree the first stage created, and an annotation run, which is not a stage.
func TestHooksWorktreeReuseAndAnnotation(t *testing.T) {
	h := newPlannotatorHarness(t)
	dir := t.TempDir()
	eventLog := filepath.Join(dir, "events.log")
	codeFile := filepath.Join(dir, "codes.log")

	h.cfg.Hooks = config.Hooks{
		config.HookWorktreeCreated: {appendHook("created", eventLog, "$KONTORA_EVENT")},
		config.HookStageStart: {
			appendHook("before", eventLog, "$KONTORA_EVENT $KONTORA_STAGE"),
			appendHook("code", codeFile, "start=${KONTORA_EXIT_CODE-unset}"),
		},
		config.HookStageEnd: {
			appendHook("after", eventLog, "$KONTORA_EVENT $KONTORA_STAGE"),
			appendHook("code", codeFile, "end=${KONTORA_EXIT_CODE-unset}"),
		},
	}

	d := h.newAnnotationDaemon(DirectRunner)
	_, stop := startAnnotationDaemon(t, h, d, "tst-h06",
		h.ticketMDWithPipeline("tst-h06", "todo", "pipeline: two-stage\n", ""))
	defer stop()

	h.waitForStatus("tst-h06.md", ticket.StatusDone, 20*time.Second)
	assert.Equal(t, []string{
		"worktree_created",
		"stage_start step1",
		"stage_end step1",
		"stage_start step2",
		"stage_end step2",
	}, waitForLines(t, eventLog, 5))
	assert.Equal(t, []string{"start=unset", "end=0", "start=unset", "end=0"},
		waitForLines(t, codeFile, 4))

	// An annotation run reuses the worktree and is not a stage, so it adds no
	// events at all.
	require.NoError(t, os.WriteFile(filepath.Join(h.tasksDir, "tst-h06.md"),
		[]byte(h.annotationTicketMD("tst-h06", "open", "")), 0o644))
	require.Eventually(t, func() bool {
		tk, err := d.GetTicket("tst-h06")
		return err == nil && tk.Status == string(ticket.StatusOpen)
	}, 5*time.Second, 25*time.Millisecond, "the daemon should index the reset ticket")
	// Moving the ticket to open kills the stage agent, and the file says open
	// before the kill has unregistered it. An annotate claim taken in that window
	// is refused because the ticket is still running.
	waitForAgentsDone(t, d, 5*time.Second)

	require.NoError(t, d.StartPlannotatorAnnotate("tst-h06"))
	select {
	case <-h.spawnParams:
	case <-time.After(5 * time.Second):
		t.Fatal("the annotate spawner should be invoked")
	}
	h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")
	h.waitForAnnotationRuns("tst-h06", 1, ticket.StatusOpen)

	assert.Len(t, readLines(t, eventLog), 5, "an annotation run fires no stage or worktree hooks")
}

// TestHooksStageEndExitCode pins that the post-run event carries the agent's
// exit code and that a stage_end failure leaves the pipeline's own outcome in
// last_error.
func TestHooksStageEndExitCode(t *testing.T) {
	h := newHarness(t)
	codeFile := filepath.Join(t.TempDir(), "code.txt")

	h.cfg.Hooks = config.Hooks{config.HookStageEnd: {
		appendHook("record", codeFile, "$KONTORA_EXIT_CODE"),
		{Name: "bootstrap", Run: "exit 1", OnFailure: config.HookOnFailurePause},
	}}
	h.cfg.Agents["agent1"] = config.Agent{Binary: writeScript(t, "exit 3\n")}

	d := h.newDaemon(h.cfg)
	stop := startDaemon(t, d)
	defer stop()

	h.writeTicket("tst-h07.md", h.taskMD("tst-h07", "todo", "one-stage"))
	got := h.waitForStatus("tst-h07.md", ticket.StatusPaused, 15*time.Second)

	assert.Equal(t, []string{"3"}, waitForLines(t, codeFile, 1))
	assert.Contains(t, got.LastError, "agent exited with code 3",
		"the pipeline's reason must survive the hook failure")
	assert.Contains(t, got.Body, "hook bootstrap: exited with code 1")
}

// ignoreInRepo commits a .gitignore for name, so what a hook writes decides
// whether the worktree is dirty rather than the developer's global gitignore.
func ignoreInRepo(t *testing.T, repoDir, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(name+"\n"), 0o644))
	for _, args := range [][]string{{"add", ".gitignore"}, {"commit", "-m", "ignore " + name}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

// TestHooksWorktreeCreatedFailureKeepsUnpreparedWorktree covers the other half
// of the retry contract: the hook left a tracked file behind, so the worktree
// cannot be removed and stays. The next pickup must run its hooks again rather
// than starting the agent on a worktree that was never prepared.
func TestHooksWorktreeCreatedFailureKeepsUnpreparedWorktree(t *testing.T) {
	h := newHarness(t)
	runLog := filepath.Join(t.TempDir(), "runs.log")
	ranFile := filepath.Join(t.TempDir(), "agent.log")

	h.cfg.Hooks = config.Hooks{config.HookWorktreeCreated: {config.Hook{
		Name: "bootstrap",
		Run:  fmt.Sprintf("echo work > leftover.txt; echo ran >> %s; exit 1", runLog),
	}}}
	h.cfg.Agents["agent1"] = config.Agent{Binary: writeScript(t, fmt.Sprintf("echo agent >> %s\n", ranFile))}

	d := h.newDaemon(h.cfg)
	stop := startDaemon(t, d)
	defer stop()

	path := h.writeTicket("tst-h09.md", h.taskMD("tst-h09", "todo", "one-stage"))
	got := h.waitForStatus("tst-h09.md", ticket.StatusPaused, 15*time.Second)
	assert.Contains(t, got.LastError, "hook bootstrap: exited with code 1")
	assert.Contains(t, got.LastError, "worktree kept at",
		"a worktree that could not be removed should be named in the pause reason")
	assert.Len(t, waitForLines(t, runLog, 1), 1)
	assert.DirExists(t, filepath.Join(h.wtDir, h.repoName, "tst-h09"))

	require.NoError(t, os.WriteFile(path,
		[]byte(h.taskMD("tst-h09", "todo", "one-stage")), 0o644))
	assert.Len(t, waitForLines(t, runLog, 2), 2)
	h.waitForStatus("tst-h09.md", ticket.StatusPaused, 15*time.Second)
	assert.NoFileExists(t, ranFile, "the agent must not run on an unprepared worktree")
}

// TestHooksStageEndPairsAFailedRun covers a run that ends before the pipeline
// evaluates it: the agent hides a failure behind a clean exit, so the ticket is
// paused without reaching handleAgentExit. The stage_start hooks have already
// run, and what they started has to be stopped on this path too.
func TestHooksStageEndPairsAFailedRun(t *testing.T) {
	h := newHarness(t)
	eventLog := filepath.Join(t.TempDir(), "events.log")

	h.cfg.Hooks = config.Hooks{
		config.HookStageStart: {appendHook("before", eventLog, "$KONTORA_EVENT")},
		config.HookStageEnd:   {appendHook("after", eventLog, "$KONTORA_EVENT ${KONTORA_EXIT_CODE}")},
	}
	h.cfg.Agents["agent1"] = config.Agent{
		Binary:          writeScript(t, "echo 'Error: quota exceeded for today'\n"),
		FailurePatterns: []string{"(?i)quota exceeded"},
	}

	d := h.newDaemon(h.cfg)
	stop := startDaemon(t, d)
	defer stop()

	h.writeTicket("tst-h10.md", h.taskMD("tst-h10", "todo", "one-stage"))
	got := h.waitForStatus("tst-h10.md", ticket.StatusPaused, 15*time.Second)
	assert.Contains(t, got.LastError, "agent error")
	assert.Equal(t, []string{"stage_start", "stage_end 0"}, waitForLines(t, eventLog, 2))
}

// TestHooksStageEndTicketEdit covers the window the post-run hooks open: one of
// them holds KONTORA_TICKET_FILE and writes the file the daemon is about to
// write back, so the daemon reads it again after the hooks have finished.
func TestHooksStageEndTicketEdit(t *testing.T) {
	h := newHarness(t)

	h.cfg.Hooks = config.Hooks{config.HookStageEnd: {config.Hook{
		Name: "annotate",
		Run:  `printf '\nhook-edit\n' >> "$KONTORA_TICKET_FILE"`,
	}}}

	d := h.newDaemon(h.cfg)
	stop := startDaemon(t, d)
	defer stop()

	h.writeTicket("tst-h11.md", h.taskMD("tst-h11", "todo", "one-stage"))
	got := h.waitForStatus("tst-h11.md", ticket.StatusDone, 15*time.Second)
	assert.Contains(t, got.Body, "hook-edit", "the exit write must not discard a hook's ticket edit")
}

// TestHooksSimpleTicketStageEndPauseKeepsSummary: a hook that stops a ticket
// without a pipeline must not also throw away the record of what its agent did.
func TestHooksSimpleTicketStageEndPauseKeepsSummary(t *testing.T) {
	h := newHarness(t)

	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		require.NoError(t, os.MkdirAll(p.SessionDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(p.SessionDir, "session.jsonl"),
			[]byte(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Copied the config."}]}}`),
			0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("pi", "pi")
	cfg.Hooks = config.Hooks{config.HookStageEnd: {config.Hook{
		Name: "teardown", Run: "exit 1", OnFailure: config.HookOnFailurePause,
	}}}

	d := h.newDaemon(cfg, WithRunner(runner))
	stop := startDaemon(t, d)
	defer stop()

	h.writeTicket("tst-h13.md", simpleTaskMD("tst-h13", "todo", h.repoDir))
	got := h.waitForStatus("tst-h13.md", ticket.StatusPaused, 15*time.Second)

	assert.Contains(t, got.LastError, "hook teardown: exited with code 1")
	assert.Equal(t, "Copied the config.", got.Summary,
		"a hook pause must keep the agent's summary")
}

// TestHooksReworkStage covers the built-in rework stage, which creates its
// worktree and spawns its agent itself rather than through the pipeline path,
// so it fires the stage events itself too. It reuses the worktree the review
// left behind, which is why no worktree_created event belongs here.
func TestHooksReworkStage(t *testing.T) {
	const id = "tst-h12"
	h := newPlannotatorHarness(t)
	eventLog := filepath.Join(t.TempDir(), "events.log")

	h.cfg.Hooks = config.Hooks{
		config.HookWorktreeCreated: {appendHook("created", eventLog, "$KONTORA_EVENT")},
		config.HookStageStart:      {appendHook("before", eventLog, "$KONTORA_EVENT $KONTORA_STAGE")},
		config.HookStageEnd:        {appendHook("after", eventLog, "$KONTORA_EVENT $KONTORA_STAGE ${KONTORA_EXIT_CODE}")},
	}

	d := h.newDaemonWithSpawner()
	stop := startDaemon(t, d)
	defer stop()

	filePath := h.seedReviewTicket(id)
	require.Eventually(t, func() bool {
		_, err := d.GetTicket(id)
		return err == nil
	}, 3*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview(id))
	require.Eventually(t, func() bool {
		return h.callCount.Load() == 1
	}, 3*time.Second, 20*time.Millisecond, "the review spawner should be invoked")
	h.stdoutCh <- "please tweak"

	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		return err == nil && tk.Status == ticket.StatusHumanReview && tk.Stage == config.ReworkStageName
	}, 10*time.Second, 50*time.Millisecond, "the rework stage should finish and route back to human_review")

	assert.Equal(t, []string{"stage_start rework", "stage_end rework 0"}, waitForLines(t, eventLog, 2))
}
