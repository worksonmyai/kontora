package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
)

type concurrentCLIResult struct {
	output string
	err    error
}

// An open descriptor shows the subprocess started lock acquisition while the parent holds the lock.
func processHasMutationLock(pid int, path string) (bool, error) {
	if runtime.GOOS == "linux" {
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			return false, err
		}
		for _, fd := range fds {
			target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
			if err == nil && target == path {
				return true, nil
			}
		}
		return false, nil
	}
	out, err := exec.Command("lsof", "-a", "-p", fmt.Sprint(pid), "-Fn").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("observing CLI lock descriptor: %w: %s", err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "n"+path {
			return true, nil
		}
	}
	return false, nil
}

// Wait for every subprocess at the lock boundary to force overlapping mutations.
func runConcurrentCLI(t *testing.T, fixture handoffFixture, commands ...[]string) []concurrentCLIResult {
	t.Helper()
	return runConcurrentCLIWithMutation(t, fixture, nil, commands...)
}

// mutate runs in the parent after all subprocesses reach the lock boundary,
// while it still holds the board lock, to exercise fresh status/input checks.
func runConcurrentCLIWithMutation(t *testing.T, fixture handoffFixture, mutate func() error, commands ...[]string) []concurrentCLIResult {
	t.Helper()
	if runtime.GOOS != "linux" {
		if _, err := exec.LookPath("lsof"); err != nil {
			t.Skip("lock descriptor observation requires /proc or lsof")
		}
	}
	bin := buildKontora(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := filepath.Dir(fixture.ticketPath)
	// macOS temporary directories can be symlinks; lsof prints the real path.
	realDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	lockPath := filepath.Join(realDir, ".kontora", "mutation.lock")
	results := make([]concurrentCLIResult, len(commands))
	done := make([]chan concurrentCLIResult, len(commands))
	err = ticket.WithBoardMutation(dir, func() error {
		cmds := make([]*exec.Cmd, len(commands))
		for i, args := range commands {
			cmd := exec.CommandContext(ctx, bin, args...)
			cmd.Env = cliTestEnv(t, fixture.env)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				return err
			}
			cmds[i] = cmd
			done[i] = make(chan concurrentCLIResult, 1)
			go func() {
				err := cmd.Wait()
				done[i] <- concurrentCLIResult{output: out.String(), err: err}
			}()
		}
		for i, cmd := range cmds {
			ticker := time.NewTicker(10 * time.Millisecond)
			for {
				ready, err := processHasMutationLock(cmd.Process.Pid, lockPath)
				if err != nil {
					ticker.Stop()
					return err
				}
				if ready {
					break
				}
				select {
				case result := <-done[i]:
					ticker.Stop()
					return fmt.Errorf("CLI %v exited before reaching mutation lock: %v: %s", commands[i], result.err, result.output)
				case <-ctx.Done():
					ticker.Stop()
					return fmt.Errorf("CLI %v did not reach mutation lock: %w", commands[i], ctx.Err())
				case <-ticker.C:
				}
			}
			ticker.Stop()
		}
		if mutate != nil {
			return mutate()
		}
		return nil
	})
	require.NoError(t, err)
	for i := range results {
		results[i] = <-done[i]
	}
	return results
}

func TestConcurrentCLIDependencies(t *testing.T) {
	for _, opposite := range []bool{false, true} {
		name := "different prerequisites"
		if opposite {
			name = "opposite edges"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newHandoffFixture(t)
			dir := filepath.Dir(fixture.ticketPath)
			for _, id := range []string{"kon-a", "kon-b", "kon-c"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), fmt.Appendf(nil, "---\nid: %s\nstatus: open\n---\n# %s\n", id, id), 0o600))
			}
			second := []string{"dep", "kon-a", "kon-c"}
			if opposite {
				second = []string{"dep", "kon-b", "kon-a"}
			}
			results := runConcurrentCLI(t, fixture, []string{"dep", "kon-a", "kon-b"}, second)
			a, err := ticket.ParseFile(filepath.Join(dir, "kon-a.md"))
			require.NoError(t, err)
			b, err := ticket.ParseFile(filepath.Join(dir, "kon-b.md"))
			require.NoError(t, err)
			if !opposite {
				for _, result := range results {
					require.NoError(t, result.err, result.output)
				}
				assert.ElementsMatch(t, []string{"kon-b", "kon-c"}, a.Deps)
				assert.Empty(t, b.Deps)
				return
			}
			successes := 0
			for _, result := range results {
				if result.err == nil {
					successes++
				} else {
					assert.Contains(t, result.output, "dependency cycle")
				}
			}
			assert.Equal(t, 1, successes, "exactly one opposite edge must commit")
			if results[0].err == nil {
				assert.Equal(t, []string{"kon-b"}, a.Deps)
				assert.Empty(t, b.Deps)
			} else {
				assert.Empty(t, a.Deps)
				assert.Equal(t, []string{"kon-a"}, b.Deps)
			}
		})
	}
}

func TestConcurrentCLINoteAndSummary(t *testing.T) {
	for _, second := range [][]string{{"note", "kon-safe", "second note"}, {"summary", "kon-safe", "fresh summary"}, {"update", "kon-safe", "--agent", "claude"}} {
		t.Run(second[0], func(t *testing.T) {
			fixture := newHandoffFixture(t)
			results := runConcurrentCLI(t, fixture, []string{"note", "kon-safe", "first note"}, second)
			for _, result := range results {
				require.NoError(t, result.err, result.output)
			}
			got, err := ticket.ParseFile(fixture.ticketPath)
			require.NoError(t, err)
			notes := ticket.ParseNotes(got.Body)
			texts := make([]string, len(notes))
			for i, note := range notes {
				texts[i] = note.Text
			}
			if second[0] == "note" {
				assert.ElementsMatch(t, []string{"Existing parent", "first note", "second note"}, texts)
			} else {
				assert.ElementsMatch(t, []string{"Existing parent", "first note"}, texts)
				if second[0] == "summary" {
					assert.Equal(t, "fresh summary", got.Summary)
				} else {
					assert.Equal(t, "claude", got.Agent)
					assert.Equal(t, "old summary", got.Summary)
				}
			}
		})
	}
}

func TestCLIUpdateValidatesFreshState(t *testing.T) {
	for _, tc := range []struct {
		name, field, value, wantError string
		args                          []string
	}{
		{name: "status without preparation", field: "status", value: "in_progress", args: []string{"--agent", "claude"}, wantError: "cannot update ticket in status in_progress"},
		{name: "status after preparation", field: "status", value: "in_progress", args: []string{"--branch", "proposed-branch"}, wantError: "cannot update ticket in status in_progress"},
		{name: "execution input after preparation", field: "branch", value: "fresh-branch", args: []string{"--path", "proposed-path"}, wantError: "execution settings changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHandoffFixture(t)
			var fresh []byte
			results := runConcurrentCLIWithMutation(t, fixture, func() error {
				got, err := ticket.ParseFile(fixture.ticketPath)
				if err != nil {
					return err
				}
				if err := got.SetField(tc.field, tc.value); err != nil {
					return err
				}
				fresh, err = got.Marshal()
				if err != nil {
					return err
				}
				return os.WriteFile(fixture.ticketPath, fresh, 0o600)
			}, append([]string{"update", "kon-safe"}, tc.args...))
			require.Error(t, results[0].err, results[0].output)
			assert.Contains(t, results[0].output, tc.wantError)
			after, err := os.ReadFile(fixture.ticketPath)
			require.NoError(t, err)
			assert.Equal(t, string(fresh), string(after), "rejected update must preserve the concurrent mutation byte-for-byte")
		})
	}
}
