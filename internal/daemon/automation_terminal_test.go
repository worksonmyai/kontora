package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/tmux"
)

func terminalInspectionStub(t *testing.T, present, blocked bool) (tmux.WindowIdentity, string, string) {
	t.Helper()
	dir := t.TempDir()
	id := tmux.WindowIdentity{SocketPath: "/tmp/terminal-test-socket", ServerPID: os.Getpid(), ServerStartedAt: 123, SessionID: "$1", WindowID: "@1"}
	queried, release := filepath.Join(dir, "queried"), filepath.Join(dir, "release")
	script := "#!/bin/sh\n"
	if blocked {
		script += fmt.Sprintf(": > '%s'\nwhile [ ! -f '%s' ]; do /bin/sleep 0.01; done\n", queried, release)
	}
	script += fmt.Sprintf("printf '%%s\\n' '%d\t123'\n", id.ServerPID)
	if present {
		script += "printf '@1\\n'\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return id, queried, release
}

func TestAutoTerminalRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, phase, artifact string
		present               bool
		want                  error
	}{
		{"queued", "queued", "valid", true, automation.ErrNotFound},
		{"preparing", "preparing", "valid", true, automation.ErrNotFound},
		{"recovery required", "recovery_required", "valid", true, automation.ErrNotFound},
		{"missing artifact", "running", "missing", true, automation.ErrUnavailable},
		{"mismatched artifact", "running", "mismatch", true, automation.ErrUnavailable},
		{"symlink artifact", "running", "symlink", true, automation.ErrUnavailable},
		{"gone", "running", "valid", false, automation.ErrNotFound},
		{"running", "running", "valid", true, nil},
		{"stopping", "stopping", "valid", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d, def, _ := automationFixture(t)
			run := recoveryAllocate(t, d, def.ID, "terminal", tc.phase)
			id, _, _ := terminalInspectionStub(t, tc.present, false)
			run = recoveryArtifact(t, d, run, id)
			path := filepath.Join(automationRunDir(d.config(), run), "terminal.json")
			switch tc.artifact {
			case "missing":
				require.NoError(t, os.Remove(path))
			case "mismatch":
				other := id
				other.WindowID = "@2"
				b, err := json.Marshal(other)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, b, 0o600))
			case "symlink":
				other := filepath.Join(t.TempDir(), "identity.json")
				require.NoError(t, os.Rename(path, other))
				require.NoError(t, os.Symlink(other, path))
			}
			target, err := d.AutomationTerminal(def.ID, run.N)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, id, target.Identity)
		})
	}
}

func TestAutoTerminalRace(t *testing.T) {
	for _, mutation := range []string{"terminal phase", "new window", "changed artifact"} {
		t.Run(mutation, func(t *testing.T) {
			_, d, def, _ := automationFixture(t)
			id, queried, release := terminalInspectionStub(t, true, true)
			run := recoveryArtifact(t, d, recoveryAllocate(t, d, def.ID, "terminal", "running"), id)
			done := make(chan error, 1)
			go func() { _, err := d.AutomationTerminal(def.ID, run.N); done <- err }()
			require.Eventually(t, func() bool { _, err := os.Stat(queried); return err == nil }, time.Second, 10*time.Millisecond)
			want := automation.ErrNotFound
			if mutation == "changed artifact" {
				id.ServerStartedAt++
				b, err := json.Marshal(id)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(automationRunDir(d.config(), run), "terminal.json"), b, 0o600))
				want = automation.ErrUnavailable
			} else {
				_, err := d.automationStore.UpdateRun(def.ID, run.N, func(r *automation.Run) error {
					if mutation == "terminal phase" {
						r.Phase, r.Outcome, r.CompletedAt = "terminal", "failed", time.Now()
					} else {
						r.WindowID = "@2"
					}
					return nil
				})
				require.NoError(t, err)
			}
			require.NoError(t, os.WriteFile(release, nil, 0o600))
			select {
			case err := <-done:
				require.ErrorIs(t, err, want)
			case <-time.After(5 * time.Second):
				t.Fatal("terminal lookup did not finish")
			}
		})
	}
}
