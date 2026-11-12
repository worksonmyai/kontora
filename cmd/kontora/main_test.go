package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/frontmatter"
	"github.com/worksonmyai/kontora/internal/testutil"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
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

func TestRenderUsage(t *testing.T) {
	usage := renderUsage()

	_, groups, ok := strings.Cut(usage, "\n")
	require.True(t, ok, "usage has no line after the Usage: line")

	lines := strings.Split(groups, "\n")
	group := ""
	seen := map[string]string{}
	descColumn := map[string]int{}
	for _, line := range lines {
		switch {
		case line == "":
		case !strings.HasPrefix(line, "  "):
			group = strings.TrimSuffix(line, ":")
			assert.Contains(t, cli.CommandGroups, group, "unknown heading %q", line)
		default:
			fields := strings.Fields(line)
			require.NotEmpty(t, fields)
			verb := fields[0]
			assert.Empty(t, seen[verb], "%q is listed under %q and %q", verb, seen[verb], group)
			seen[verb] = group
			descColumn[verb] = strings.Index(line, fields[1])
		}
	}

	for _, cmd := range cli.Commands {
		assert.Equal(t, cmd.Group, seen[cmd.Name], "%q is not listed under its own group", cmd.Name)
	}
	assert.Len(t, seen, len(cli.Commands))

	// estimate-compaction is the longest verb, and must not push its own
	// description past where every other one starts.
	assert.Equal(t, descColumn["ls"], descColumn["estimate-compaction"])
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

// TestTicketsDirPrecedence runs the built binary for each rung of the ladder:
// --tickets-dir, then KONTORA_TICKETS_DIR, then TICKETS_DIR, then the file. Each
// created ticket must exist only in the selected store.
func TestTicketsDirPrecedence(t *testing.T) {
	repo := testutil.InitRepo(t)
	cases := []struct {
		name    string
		legacy  bool
		kontora string
		flag    bool
		want    string
	}{
		{name: "config only", want: "file"},
		{name: "legacy var beats the file", legacy: true, want: "legacy"},
		{name: "kontora var beats the legacy var", legacy: true, kontora: "kontora", want: "kontora"},
		{name: "flag beats both vars", legacy: true, kontora: "kontora", flag: true, want: "flag"},
		{name: "a blank var falls back to the file", kontora: "   ", want: "file"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scratch := t.TempDir()
			stores := map[string]string{}
			for _, name := range []string{"file", "kontora", "legacy", "flag"} {
				dir := filepath.Join(scratch, name)
				require.NoError(t, os.MkdirAll(dir, 0o755))
				stores[name] = dir
			}
			configPath := filepath.Join(scratch, "config.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte(
				"tickets_dir: "+stores["file"]+"\n"+
					"secondary_tickets_dirs:\n  - "+stores["file"]+"\n  - "+stores["kontora"]+"\n  - "+stores["legacy"]+"\n  - "+stores["flag"]+"\n"+
					"worktrees_dir: "+filepath.Join(scratch, "wt")+"\n"+
					"logs_dir: "+filepath.Join(scratch, "logs")+"\n"+
					"default_agent: claude\n"+
					"agents:\n  claude:\n    binary: true\n"), 0o644))
			var env, overrides []string
			if tc.legacy {
				env = append(env, config.LegacyTicketsDirEnvVar+"="+stores["legacy"])
			}
			if tc.kontora != "" {
				value := tc.kontora
				if dir, ok := stores[value]; ok {
					value = dir
				}
				env = append(env, config.TicketsDirEnvVar+"="+value)
			}
			if tc.flag {
				overrides = []string{"--tickets-dir", stores["flag"]}
			}
			args := append([]string{"new", "--config", configPath, "--status", "open", "--quiet", "--path", repo}, overrides...)
			args = append(args, "ticket in "+tc.want)
			out, err := runCLI(t, env, args...)
			require.NoError(t, err, out)
			id := strings.TrimSpace(out)
			for name, dir := range stores {
				if name == tc.want {
					assert.FileExists(t, filepath.Join(dir, id+".md"))
				} else {
					entries, err := os.ReadDir(dir)
					require.NoError(t, err)
					assert.Empty(t, entries, "unselected store %s must remain empty", name)
				}
			}
			args = append([]string{"view", "--config", configPath, "--body", id}, overrides...)
			out, err = runCLI(t, env, args...)
			require.NoError(t, err, out)
			assert.Contains(t, out, "# ticket in "+tc.want)

			args = append([]string{"ls", "--config", configPath, "--json"}, overrides...)
			out, err = runCLI(t, env, args...)
			require.NoError(t, err, out)
			var items []map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &items))
			require.Len(t, items, 1)
			assert.Equal(t, id, items[0]["id"])
			assert.Equal(t, "ticket in "+tc.want, items[0]["title"])
			assert.Equal(t, "open", items[0]["status"])

			// Retry needs the resolved execution config as well as the selected
			// store, even for an ordinary ticket without an explicit branch.
			for _, action := range []string{"pause", "retry"} {
				args := append([]string{action, "--config", configPath, id}, overrides...)
				out, err := runCLI(t, env, args...)
				require.NoError(t, err, out)
			}
			got, err := ticket.ParseFile(filepath.Join(stores[tc.want], id+".md"))
			require.NoError(t, err)
			assert.Equal(t, ticket.StatusTodo, got.Status)
			assert.Empty(t, got.Branch)
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

	const tail = "\n## Notes\n\n**2026-08-08**\n\none\n\n**2026-08-09**\n\ntwo\n\n## Later\n\nkeep\n"
	for _, tt := range []struct {
		name, suffix, want string
		flags              []string
	}{
		{name: "no notes", flags: []string{"--notes"}},
		{name: "no last notes", flags: []string{"--last", "2"}},
		{name: "notes", suffix: tail, flags: []string{"--notes"}, want: strings.TrimPrefix(tail, "\n")},
		{name: "last", suffix: tail, flags: []string{"--last", "1"}, want: "**2026-08-09**\n\ntwo\n\n## Later\n\nkeep\n"},
		{name: "above count", suffix: tail, flags: []string{"--last", "5"}, want: strings.TrimPrefix(tail, "\n")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(ticketsDir, id+".md"), append(append([]byte(nil), written...), []byte(tt.suffix)...), 0o644))
			args := append([]string{"view", "--config", configPath}, tt.flags...)
			args = append(args, id)
			out, err := runCLI(t, pagerEnv, args...)
			require.NoError(t, err, out)
			assert.Equal(t, tt.want, out)
		})
	}
}

// The daemon runs unattended against whatever store it resolved, so it says
// when that came from the environment rather than the config file it was given.
func TestTicketsDirEnvNote(t *testing.T) {
	scratch := t.TempDir()
	fromFile := filepath.Join(scratch, "file")
	fromEnv := filepath.Join(scratch, "env")

	configPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(
		"tickets_dir: "+fromFile+"\n"+
			"default_agent: claude\n"+
			"agents:\n  claude:\n    binary: true\n"), 0o644))

	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "no variable set", want: ""},
		{
			name: "the variable names the directory the file already names",
			env:  map[string]string{config.TicketsDirEnvVar: fromFile},
			want: "",
		},
		{
			name: "the variable moves the store",
			env:  map[string]string{config.TicketsDirEnvVar: fromEnv},
			want: config.TicketsDirEnvVar + " sets tickets_dir to " + fromEnv + "; " + configPath + " resolves to " + fromFile,
		},
		{
			name: "the legacy variable is named as itself",
			env:  map[string]string{config.LegacyTicketsDirEnvVar: fromEnv},
			want: config.LegacyTicketsDirEnvVar + " sets tickets_dir to " + fromEnv + "; " + configPath + " resolves to " + fromFile,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.TicketsDirEnvVar, "")
			t.Setenv(config.LegacyTicketsDirEnvVar, "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := config.Load(configPath)
			require.NoError(t, err)

			assert.Equal(t, tc.want, foldEnvOverConfig(cfg, configPath))
		})
	}
}

func TestLocalTUIRejectsUnverifiedDaemonBoard(t *testing.T) {
	bin := buildKontora(t)
	for _, tc := range []struct {
		name, identity, diagnostic string
	}{
		{name: "different directory", identity: "board-a", diagnostic: "does not match requested board"},
		{name: "missing identity", diagnostic: "directory identity missing"},
		{name: "relative identity", identity: "relative/tickets", diagnostic: "directory identity missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := t.TempDir()
			boardA, boardB := filepath.Join(scratch, "board-a"), filepath.Join(scratch, "board-b")
			originals := map[string]string{}
			for dir, title := range map[string]string{boardA: "Daemon board A", boardB: "Requested board B"} {
				require.NoError(t, os.MkdirAll(dir, 0o755))
				path := filepath.Join(dir, "kon-shared.md")
				originals[path] = "---\nid: kon-shared\nstatus: paused\nkontora: true\n---\n# " + title + "\n\n" + title + " detail body\n"
				require.NoError(t, os.WriteFile(path, []byte(originals[path]), 0o600))
			}
			identity := tc.identity
			if identity == "board-a" {
				identity = boardA
			}
			var mu sync.Mutex
			var requests []string
			probed := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				mu.Unlock()
				switch r.URL.Path {
				case "/api/config":
					_ = json.NewEncoder(w).Encode(web.ConfigInfo{
						TicketsDir: identity, TmuxSession: "board-a", TmuxSocket: filepath.Join(scratch, "a.sock"),
						Agents: []string{"claude", "codex"},
					})
					select {
					case probed <- struct{}{}:
					default:
					}
				case "/api/tickets":
					_ = json.NewEncoder(w).Encode(map[string]any{"tickets": []web.TicketInfo{
						{ID: "kon-shared", Title: "Daemon board A", Status: "paused", Kontora: true},
					}})
				case "/api/events":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: ticket_updated\ndata: {\"ticket\":{\"id\":\"kon-shared\",\"title\":\"Daemon board A\"}}\n\n")
				default:
					_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "kon-shared", Title: "Daemon board A", Status: "paused", Kontora: true})
				}
			}))
			defer srv.Close()
			host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
			require.NoError(t, err)
			portNumber, err := strconv.Atoi(port)
			require.NoError(t, err)
			configPath := filepath.Join(scratch, "config.yaml")
			require.NoError(t, os.WriteFile(configPath, fmt.Appendf(nil, `tickets_dir: %q
worktrees_dir: %q
logs_dir: %q
agents:
  claude:
    binary: true
web:
  host: %q
  port: %d
`, boardA, filepath.Join(scratch, "wt"), filepath.Join(scratch, "logs"), host, portNumber), 0o600))
			// A broken attachment guard must not execute the developer's tmux.
			tools := filepath.Join(scratch, "bin")
			require.NoError(t, os.MkdirAll(tools, 0o755))
			attached := filepath.Join(scratch, "attached")
			require.NoError(t, os.WriteFile(filepath.Join(tools, "tmux"), fmt.Appendf(nil, "#!/bin/sh\nprintf attached > %q\nexit 91\n", attached), 0o755))

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "ls", "--config", configPath, "--tickets-dir", boardB)
			cmd.Dir = scratch
			cmd.Env = cliTestEnv(t, []string{"TERM=xterm-256color", "TMUX=", "PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH")})
			terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 400})
			require.NoError(t, err)
			exited := make(chan struct{})
			var processErr error
			go func() {
				processErr = cmd.Wait()
				close(exited)
			}()
			output := make(chan string, 64)
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				defer close(output)
				buf := make([]byte, 32*1024)
				pending := ""
				for {
					n, err := terminal.Read(buf)
					if n > 0 {
						chunk := string(buf[:n])
						// Termenv queries the cursor while detecting terminal colors.
						// A cursor-only reply reports that OSC colors are unsupported.
						pending += chunk
						for strings.Contains(pending, "\x1b[6n") {
							_, pending, _ = strings.Cut(pending, "\x1b[6n")
							_, _ = io.WriteString(terminal, "\x1b[1;1R")
						}
						if len(pending) > 3 {
							pending = pending[len(pending)-3:]
						}
						select {
						case output <- chunk:
						case <-ctx.Done():
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
			t.Cleanup(func() {
				cancel()
				_ = terminal.Close()
				<-exited
				<-readDone
			})
			var transcript strings.Builder
			waitFor := func(wants ...string) string {
				t.Helper()
				var fresh strings.Builder
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				for {
					plain := ansi.Strip(fresh.String())
					if slices.ContainsFunc(wants, func(want string) bool { return !strings.Contains(plain, want) }) {
						select {
						case chunk, ok := <-output:
							require.True(t, ok, "PTY closed before %q:\n%s", wants, ansi.Strip(transcript.String()))
							transcript.WriteString(chunk)
							fresh.WriteString(chunk)
						case <-timer.C:
							t.Fatalf("waiting for %q:\n%s", wants, ansi.Strip(transcript.String()))
						}
						continue
					}
					return plain
				}
			}
			send := func(keys string) {
				t.Helper()
				_, err := io.WriteString(terminal, keys)
				require.NoError(t, err)
			}
			select {
			case <-probed:
			case <-ctx.Done():
				t.Fatal("CLI did not probe /api/config")
			}
			initial := waitFor("Requested board B", "file mode", tc.diagnostic, "actions and attachment disabled")
			for _, hint := range []string{"a attach", "p pause", "r retry", "s skip", "n new"} {
				assert.NotContains(t, initial, hint)
			}
			for i, key := range []string{"a", "p", "r", "s", "n", "g"} {
				// A unique filter proves the process handled each action and stayed on the board.
				// NUL separates keys so Bubble Tea cannot combine them into one rune message.
				marker := strconv.Itoa(i + 1)
				send(key + "\x00/\x00" + marker)
				wants := []string{"/" + marker + "▌"}
				if key == "a" {
					wants = append(wants, "verified daemon socket and session required")
				}
				waitFor(wants...)
				send("\x1b")
				waitFor("file mode")
			}
			send("\r")
			waitFor("Requested board B detail body")
			send("g")
			waitFor("daemon not verified; actions and attachment unavailable")
			send("q")
			waitFor("file mode")
			send("q")
			select {
			case <-exited:
				require.NoError(t, processErr, ansi.Strip(transcript.String()))
			case <-ctx.Done():
				t.Fatal("CLI did not exit after q")
			}
			for chunk := range output {
				transcript.WriteString(chunk)
			}
			assert.NotContains(t, ansi.Strip(transcript.String()), "Daemon board A")
			assert.NoFileExists(t, attached)
			mu.Lock()
			assert.Equal(t, []string{"GET /api/config"}, requests, "no board A reads, mutations, or subscriptions")
			mu.Unlock()
			for path, original := range originals {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, original, string(data), path)
			}
		})
	}
}
