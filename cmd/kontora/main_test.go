package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/frontmatter"
	"github.com/worksonmyai/kontora/internal/testutil"
)

func TestResolveConfigPath(t *testing.T) {
	cases := []struct {
		name       string
		setupLocal bool
		setupDirs  []int // indices into configDirs to create config files in
		wantIdx    int   // -1 = local, 0+ = index into configDirs
	}{
		{
			name:       "local exists",
			setupLocal: true,
			wantIdx:    -1,
		},
		{
			name:      "first config dir",
			setupDirs: []int{0},
			wantIdx:   0,
		},
		{
			name:      "falls back to second config dir",
			setupDirs: []int{1},
			wantIdx:   1,
		},
		{
			name:      "first dir wins when both exist",
			setupDirs: []int{0, 1},
			wantIdx:   0,
		},
		{
			name:    "nothing found returns first dir path",
			wantIdx: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			configDirs := []string{t.TempDir(), t.TempDir()}

			localPath := filepath.Join(workDir, ".kontora", "config.yaml")

			if tc.setupLocal {
				require.NoError(t, os.MkdirAll(filepath.Dir(localPath), 0o755))
				require.NoError(t, os.WriteFile(localPath, []byte("# local"), 0o644))
			}
			for _, idx := range tc.setupDirs {
				p := filepath.Join(configDirs[idx], "kontora", "config.yaml")
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(t, os.WriteFile(p, []byte("# dir"), 0o644))
			}

			got := config.ResolveConfigPath(workDir, configDirs)

			var expected string
			if tc.wantIdx == -1 {
				expected = localPath
			} else {
				expected = filepath.Join(configDirs[tc.wantIdx], "kontora", "config.yaml")
			}

			assert.Equal(t, expected, got)
		})
	}
}

// TestDispatchCoversCommandTable checks that every verb in cli.Commands reaches
// a handler. An unknown verb prints usage and exits 1, so a command listed in
// the table but missing from the switch shows up as that exact output.
func TestDispatchCoversCommandTable(t *testing.T) {
	for _, cmd := range cli.Commands {
		t.Run(cmd.Name, func(t *testing.T) {
			assert.Contains(t, handlers, cmd.Name,
				"%q is in cli.Commands but nothing dispatches it", cmd.Name)
		})
	}

	// And the reverse: a handler with no table entry is invisible in the usage
	// text and in the completions.
	names := make(map[string]bool, len(cli.Commands))
	for _, cmd := range cli.Commands {
		names[cmd.Name] = true
	}
	for verb := range handlers {
		assert.True(t, names[verb], "%q is dispatched but missing from cli.Commands", verb)
	}
}

func TestEstimateCompactionCommand(t *testing.T) {
	for _, cmd := range cli.Commands {
		if cmd.Name == "estimate-compaction" {
			assert.False(t, cmd.Remote, "estimate-compaction must be local-only")
			assert.True(t, cmd.Config, "estimate-compaction reads the config for logs_dir")
			assert.False(t, cmd.TicketID, "estimate-compaction does not take a ticket ID")

			flags := make(map[string]bool)
			for _, f := range cmd.Flags {
				flags[f.Name] = true
			}
			assert.True(t, flags["logs-dir"], "must have --logs-dir flag")
			assert.True(t, flags["stage"], "must have --stage flag")
			assert.True(t, flags["thresholds"], "must have --thresholds flag")
			assert.True(t, flags["top"], "must have --top flag")
			return
		}
	}
	t.Fatal("estimate-compaction not found in cli.Commands")
}

// TestSessionsCommand pins the local-only decision. Every path the verb prints
// names a file on the machine that runs it, so a --url that made it answer for
// another host would print paths the caller cannot open.
func TestSessionsCommand(t *testing.T) {
	for _, cmd := range cli.Commands {
		if cmd.Name != "sessions" {
			continue
		}
		assert.False(t, cmd.Remote, "sessions must be local-only")
		assert.True(t, cmd.Config, "sessions reads the config for logs_dir and tickets_dir")
		assert.True(t, cmd.TicketID, "sessions takes a ticket ID")

		flags := make(map[string]bool)
		for _, f := range cmd.Flags {
			flags[f.Name] = true
		}
		for _, name := range []string{"stage", "run", "logs", "events", "all"} {
			assert.True(t, flags[name], "must have --%s flag", name)
		}
		return
	}
	t.Fatal("sessions not found in cli.Commands")
}

// TestCLIDocsCoverEveryCommand keeps docs/cli.md honest: a command added to the
// table without a docs entry is the drift that left view, delete, skip,
// set-stage and fmt undocumented for as long as they existed.
func TestCLIDocsCoverEveryCommand(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	require.NoError(t, err)
	docs := string(data)

	for _, cmd := range cli.Commands {
		t.Run(cmd.Name, func(t *testing.T) {
			assert.Contains(t, docs, "`kontora "+cmd.Name,
				"%q has no heading in docs/cli.md", cmd.Name)
		})
	}
}

// TestCreateViewListEndToEnd drives the built binary against a scratch config,
// which is the only way to cover the flag parsing, the file that is written,
// and the JSON a script reads back, in one pass.
func TestCreateViewListEndToEnd(t *testing.T) {
	scratch := t.TempDir()
	ticketsDir := filepath.Join(scratch, "tickets")
	repo := testutil.InitRepo(t)
	configPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(
		"tickets_dir: "+ticketsDir+"\n"+
			"worktrees_dir: "+filepath.Join(scratch, "wt")+"\n"+
			"logs_dir: "+filepath.Join(scratch, "logs")+"\n"+
			"default_agent: claude\n"+
			"agents:\n  claude:\n    binary: true\n"), 0o644))

	bodyPath := filepath.Join(scratch, "body.md")
	require.NoError(t, os.WriteFile(bodyPath, []byte("## Goal\n\nSmoke test.\n"), 0o644))

	out, err := runCLI(t, nil, "new", "--config", configPath, "--status", "open", "--quiet",
		"--description-file", bodyPath, "--path", repo, "Replacement smoke test")
	require.NoError(t, err, out)
	id := strings.TrimSpace(out)
	assert.Equal(t, id+"\n", out, "--quiet prints the id and nothing else")

	// The first persisted status is open, so a daemon with auto_pick_up cannot
	// claim the ticket between creation and an edit.
	written, err := os.ReadFile(filepath.Join(ticketsDir, id+".md"))
	require.NoError(t, err)
	assert.Contains(t, string(written), "status: open")

	out, err = runCLI(t, nil, "view", "--config", configPath, "--body", id)
	require.NoError(t, err, out)
	_, body, err := frontmatter.Split(string(written))
	require.NoError(t, err)
	assert.Equal(t, body, out, "view --body prints the file after the closing delimiter")
	assert.Contains(t, out, "# Replacement smoke test")
	assert.Contains(t, out, "Smoke test.")

	out, err = runCLI(t, nil, "ls", "--config", configPath, "--json")
	require.NoError(t, err, out)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &items))
	require.Len(t, items, 1)
	assert.Equal(t, id, items[0]["id"])
	assert.Equal(t, "open", items[0]["status"])
	assert.NotContains(t, items[0], "body")

	out, err = runCLI(t, nil, "ls", "--config", configPath, "--ready", "--blocked")
	require.Error(t, err, out)
	assert.Contains(t, out, "--ready and --blocked")
}

// TestTicketsDirPrecedence runs the built binary once per rung of the ladder:
// --tickets-dir, then KONTORA_TICKETS_DIR, then TICKETS_DIR, then the file. Each
// store holds one ticket named after it, so the store that answered is the one
// the ID names.
func TestTicketsDirPrecedence(t *testing.T) {
	scratch := t.TempDir()
	repo := testutil.InitRepo(t)

	stores := map[string]string{}
	for _, name := range []string{"file", "kontora", "legacy", "flag"} {
		dir := filepath.Join(scratch, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		stores[name] = dir
	}

	configPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(
		"tickets_dir: "+stores["file"]+"\n"+
			"worktrees_dir: "+filepath.Join(scratch, "wt")+"\n"+
			"logs_dir: "+filepath.Join(scratch, "logs")+"\n"+
			"default_agent: claude\n"+
			"agents:\n  claude:\n    binary: true\n"), 0o644))

	// One ticket per store, titled with the store's name.
	for name, dir := range stores {
		out, err := runCLI(t, []string{config.TicketsDirEnvVar + "=" + dir},
			"new", "--config", configPath, "--status", "open", "--quiet",
			"--path", repo, "ticket in "+name)
		require.NoError(t, err, out)
	}

	cases := []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{name: "config only", want: "file"},
		{
			name: "legacy var beats the file",
			env:  []string{config.LegacyTicketsDirEnvVar + "=" + stores["legacy"]},
			want: "legacy",
		},
		{
			name: "kontora var beats the legacy var",
			env: []string{
				config.LegacyTicketsDirEnvVar + "=" + stores["legacy"],
				config.TicketsDirEnvVar + "=" + stores["kontora"],
			},
			want: "kontora",
		},
		{
			name: "flag beats both vars",
			env: []string{
				config.LegacyTicketsDirEnvVar + "=" + stores["legacy"],
				config.TicketsDirEnvVar + "=" + stores["kontora"],
			},
			args: []string{"--tickets-dir", stores["flag"]},
			want: "flag",
		},
		{
			name: "a blank var falls back to the file",
			env:  []string{config.TicketsDirEnvVar + "=   "},
			want: "file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"ls", "--config", configPath, "--json"}, tc.args...)
			out, err := runCLI(t, tc.env, args...)
			require.NoError(t, err, out)

			var items []map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &items))
			require.Len(t, items, 1)
			assert.Equal(t, "ticket in "+tc.want, items[0]["title"])
		})
	}
}

// A pager must never engage when stdout is not a terminal. runCLI pipes stdout,
// so /bin/false as the pager would swallow the output if it ever ran.
func TestViewIsNotPagedWhenPiped(t *testing.T) {
	scratch := t.TempDir()
	ticketsDir := filepath.Join(scratch, "tickets")
	repo := testutil.InitRepo(t)
	configPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(
		"tickets_dir: "+ticketsDir+"\n"+
			"worktrees_dir: "+filepath.Join(scratch, "wt")+"\n"+
			"logs_dir: "+filepath.Join(scratch, "logs")+"\n"+
			"default_agent: claude\n"+
			"agents:\n  claude:\n    binary: true\n"), 0o644))

	out, err := runCLI(t, nil, "new", "--config", configPath, "--status", "open", "--quiet",
		"--path", repo, "Pager smoke test")
	require.NoError(t, err, out)
	id := strings.TrimSpace(out)

	pagerEnv := []string{cli.PagerEnvVar + "=/bin/false"}

	paged, err := runCLI(t, pagerEnv, "view", "--config", configPath, id)
	require.NoError(t, err, paged)
	plain, err := runCLI(t, nil, "view", "--config", configPath, id)
	require.NoError(t, err, plain)
	assert.Equal(t, plain, paged, "a piped view is identical with and without a pager set")
	assert.Contains(t, paged, "Pager smoke test")

	written, err := os.ReadFile(filepath.Join(ticketsDir, id+".md"))
	require.NoError(t, err)
	_, body, err := frontmatter.Split(string(written))
	require.NoError(t, err)
	bodyOut, err := runCLI(t, pagerEnv, "view", "--config", configPath, "--body", id)
	require.NoError(t, err, bodyOut)
	assert.Equal(t, body, bodyOut, "--body stays byte-stable with a pager set")
}
