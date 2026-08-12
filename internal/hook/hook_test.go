package hook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A case's expected file contents cannot name the per-case temp directory up
// front, so they stand for it: dirPlaceholder for the worktree as configured,
// cwdPlaceholder for the path the shell reports, which on macOS resolves the
// symlink the temp directory sits behind.
const (
	dirPlaceholder = "{{dir}}"
	cwdPlaceholder = "{{cwd}}"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name string
		// specs run against a fresh temp directory used as the worktree.
		specs []Spec
		// exitCode, when set, is the KONTORA_EXIT_CODE a post-run event carries.
		exitCode *int
		// cancelAfter cancels the context that long after Run starts; zero
		// leaves it live. A negative value cancels before Run is called.
		cancelAfter time.Duration
		wantErr     string
		wantWarns   []string
		wantOutput  string
		// wantFiles maps a path under the worktree to its exact content.
		wantFiles  map[string]string
		wantAbsent []string
	}{
		{
			name: "a successful hook writes its output and runs in the worktree",
			specs: []Spec{{
				Name: "bootstrap",
				Run:  `pwd > ctx.txt && echo hook-output`,
			}},
			wantOutput: "hook-output\n",
			wantFiles:  map[string]string{"ctx.txt": cwdPlaceholder + "\n"},
		},
		{
			name: "the context and the inherited environment reach the command",
			specs: []Spec{{
				Run: `printf '%s\n' "$KONTORA_EVENT" "$KONTORA_TICKET_ID" "$KONTORA_TICKET_FILE" ` +
					`"$KONTORA_WORKTREE" "$KONTORA_REPO_PATH" "$KONTORA_BRANCH" "$KONTORA_STAGE" ` +
					`"$KONTORA_AGENT" "$KONTORA_PROJECT" "$PARENT_SENTINEL" > ctx.txt`,
			}},
			wantFiles: map[string]string{"ctx.txt": strings.Join([]string{
				"worktree_created", "tst-001", "/tickets/tst-001.md", dirPlaceholder,
				"/repos/kontora", "kontora/tst-001", "step1", "agent1", "kontora", "present",
			}, "\n") + "\n"},
		},
		{
			name:     "a post-run event carries the exit code",
			specs:    []Spec{{Run: `echo "${KONTORA_EXIT_CODE-unset}" > code.txt`}},
			exitCode: new(3),
			wantFiles: map[string]string{
				"code.txt": "3\n",
			},
		},
		{
			name:      "an event without an exit code leaves the variable unset",
			specs:     []Spec{{Run: `echo "${KONTORA_EXIT_CODE-unset}" > code.txt`}},
			wantFiles: map[string]string{"code.txt": "unset\n"},
		},
		{
			name:    "a fatal hook that exits non-zero reports its name and code",
			specs:   []Spec{{Name: "bootstrap", Run: "exit 3", Fatal: true}},
			wantErr: "hook bootstrap: exited with code 3",
		},
		{
			name: "an unnamed hook is identified by event and index",
			specs: []Spec{
				{Name: "first", Run: "true"},
				{Run: "exit 1", Fatal: true},
			},
			wantErr: "hook worktree_created[1]: exited with code 1",
		},
		{
			name:    "a hook that exceeds its timeout is killed",
			specs:   []Spec{{Name: "slow", Run: "sleep 30", Timeout: 300 * time.Millisecond, Fatal: true}},
			wantErr: "hook slow: timed out after 300ms",
		},
		{
			name:        "a cancelled context stops the sequence before the first hook",
			specs:       []Spec{{Run: "touch ran.txt"}},
			cancelAfter: -1,
			wantErr:     context.Canceled.Error(),
			wantAbsent:  []string{"ran.txt"},
		},
		{
			name:        "a cancelled context stops a running non-fatal hook",
			specs:       []Spec{{Name: "slow", Run: "sleep 30"}, {Run: "touch later.txt"}},
			cancelAfter: 200 * time.Millisecond,
			wantErr:     "hook slow:",
			wantAbsent:  []string{"later.txt"},
		},
		{
			name: "a fatal failure stops the hooks after it",
			specs: []Spec{
				{Name: "setup", Run: "exit 1", Fatal: true},
				{Name: "later", Run: "touch later.txt"},
			},
			wantErr:    "hook setup: exited with code 1",
			wantAbsent: []string{"later.txt"},
		},
		{
			name: "a warning failure is reported and the sequence continues",
			specs: []Spec{
				{Name: "optional", Run: "echo broken >&2; exit 2"},
				{Name: "later", Run: "touch later.txt", Fatal: true},
			},
			wantWarns:  []string{"hook optional: exited with code 2"},
			wantOutput: "broken\n",
			wantFiles:  map[string]string{"later.txt": ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PARENT_SENTINEL", "present")
			dir := t.TempDir()
			resolved, err := filepath.EvalSymlinks(dir)
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch {
			case tt.cancelAfter < 0:
				cancel()
			case tt.cancelAfter > 0:
				time.AfterFunc(tt.cancelAfter, cancel)
			}

			var out bytes.Buffer
			var warns []string
			err = Run(ctx, tt.specs, Context{
				Event:      "worktree_created",
				TicketID:   "tst-001",
				TicketFile: "/tickets/tst-001.md",
				Worktree:   dir,
				RepoPath:   "/repos/kontora",
				Branch:     "kontora/tst-001",
				Stage:      "step1",
				Agent:      "agent1",
				Project:    "kontora",
				ExitCode:   tt.exitCode,
			}, &out, func(_ Spec, err error) { warns = append(warns, err.Error()) })

			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
			require.Len(t, warns, len(tt.wantWarns))
			for i, want := range tt.wantWarns {
				assert.Contains(t, warns[i], want)
			}
			if tt.wantOutput != "" {
				assert.Contains(t, out.String(), tt.wantOutput)
			}
			for name, want := range tt.wantFiles {
				got, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err, "reading %s", name)
				want = strings.ReplaceAll(want, dirPlaceholder, dir)
				want = strings.ReplaceAll(want, cwdPlaceholder, resolved)
				assert.Equal(t, want, string(got), "content of %s", name)
			}
			for _, name := range tt.wantAbsent {
				assert.NoFileExists(t, filepath.Join(dir, name))
			}
		})
	}
}
