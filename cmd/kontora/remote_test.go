package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/testutil"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestRemoteListEpicMetadata(t *testing.T) {
	row := remoteListItem(web.TicketInfo{
		ID: "e1", Kind: "epic", EpicExecution: "agent", ChildOrder: []string{"c2", "c1"},
		Branch: "integration", BaseBranch: "main", Ready: true,
	})
	var out bytes.Buffer
	require.NoError(t, cli.RenderList(cli.Listing{Items: []cli.ListItem{row}}, &out, cli.ListOpts{JSON: true}))
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "epic", rows[0]["kind"])
	assert.Equal(t, "agent", rows[0]["epic_execution"])
	assert.Equal(t, []any{"c2", "c1"}, rows[0]["child_order"])
	assert.Equal(t, "integration", rows[0]["branch"])
	assert.Equal(t, "main", rows[0]["base_branch"])
	assert.Equal(t, true, rows[0]["ready"])
}

func TestPrintRemoteListPreservesServerReadiness(t *testing.T) {
	tickets := []web.TicketInfo{
		{
			ID: "tst-ready", Status: "todo", Kontora: true, Ready: true,
			Deps: []web.TicketRef{{ID: "external-done", Status: "done"}},
		},
		{
			ID: "tst-block", Status: "todo", Kontora: true, Ready: false,
			Deps:     []web.TicketRef{{ID: "external-done", Status: "done"}, {ID: "external-open", Status: "open"}},
			Blockers: []string{"external-open"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/tickets" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tickets": tickets})
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		opts cli.ListOpts
		ids  []string
	}{
		{name: "all", opts: cli.ListOpts{JSON: true}, ids: []string{"tst-block", "tst-ready"}},
		{name: "ready", opts: cli.ListOpts{JSON: true, Ready: true}, ids: []string{"tst-ready"}},
		{name: "blocked", opts: cli.ListOpts{JSON: true, Blocked: true}, ids: []string{"tst-block"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, printRemoteList(&out, remote.New(srv.URL, ""), tc.opts))
			var rows []cli.ListItem
			require.NoError(t, json.Unmarshal(out.Bytes(), &rows))
			var ids []string
			for _, row := range rows {
				ids = append(ids, row.ID)
				if row.ID == "tst-ready" {
					assert.True(t, row.Ready)
					assert.Empty(t, row.Blockers)
				} else {
					assert.False(t, row.Ready)
					assert.Equal(t, []string{"external-open"}, row.Blockers)
				}
			}
			assert.Equal(t, tc.ids, ids)
		})
	}

	var out bytes.Buffer
	require.NoError(t, printRemoteList(&out, remote.New(srv.URL, ""), cli.ListOpts{Blocked: true}))
	assert.Contains(t, out.String(), "external-open")
	assert.NotContains(t, out.String(), "external-done")
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// buildKontora compiles the CLI once for the subprocess-based remote tests.
func buildKontora(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kontora-bin")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "kontora")
		out, err := exec.Command("go", "build", "-o", builtBin, ".").CombinedOutput()
		if err != nil {
			buildErr = err
			builtBin = string(out)
		}
	})
	require.NoError(t, buildErr, builtBin)
	return builtBin
}

// runCLI runs the built binary with the given args and environment.
func runCLI(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	return runCLIWithStdin(t, env, nil, args...)
}

func runCLIWithStdin(t *testing.T, env []string, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	return runCLIInDir(t, "", env, stdin, args...)
}

func runCLIInDir(t *testing.T, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(buildKontora(t), args...)
	cmd.Dir = dir
	cmd.Env = cliTestEnv(t, env)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runCLISeparated(t *testing.T, env []string, stdin io.Reader, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(buildKontora(t), args...)
	cmd.Env = cliTestEnv(t, env)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Subprocesses must not reach a developer's store or daemon through inherited config.
func cliTestEnv(t *testing.T, env []string) []string {
	t.Helper()
	isolated := []string{
		"HOME", "XDG_CONFIG_HOME", "KONTORA_URL", "KONTORA_TOKEN", "KONTORA_OPERATOR_TOKEN",
		config.PathEnvVar, config.TicketsDirEnvVar, config.LegacyTicketsDirEnvVar,
		config.AgentEnvVar, config.TicketEnvVar, config.StageEnvVar,
	}
	base := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return slices.Contains(isolated, key)
	})
	home := t.TempDir()
	base = append(base, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	for _, key := range isolated[2:] {
		base = append(base, key+"=")
	}
	return append(base, env...)
}

// Keep the payload within the existing note grammar: shell safety does not
// change how Markdown headings or note bylines are parsed.
const handoffLiteralText = " \tLiteral `printf backticks` and $(printf substitution), ${HOME}\n" +
	"mixed 'single' and \"double\" quotes, C:\\scratch\\file, Unicode 日本語 🚀\n\nlast line \t\r\n\n"

type handoffInputCase struct {
	name   string
	verb   string
	agent  string
	author string
	reply  bool
}

func handoffInputCases() []handoffInputCase {
	return []handoffInputCase{
		{name: "note default author", verb: "note"},
		{name: "note agent author", verb: "note", agent: "agent-user"},
		{name: "note explicit author and reply", verb: "note", agent: "agent-user", author: "flag-user", reply: true},
		{name: "summary", verb: "summary"},
	}
}

type handoffFixture struct {
	env        []string
	ticketPath string
	parentID   string
}

func newHandoffFixture(t *testing.T) handoffFixture {
	t.Helper()
	scratch := t.TempDir()
	store := filepath.Join(scratch, "tickets")
	require.NoError(t, os.MkdirAll(store, 0o755))
	configPath := filepath.Join(scratch, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, fmt.Appendf(nil, `tickets_dir: %q
worktrees_dir: %q
logs_dir: %q
author: configured-user
agents:
  claude:
    binary: true
`, store, filepath.Join(scratch, "worktrees"), filepath.Join(scratch, "logs")), 0o600))

	tkt, err := ticket.ParseBytes([]byte("---\nid: kon-safe\nstatus: open\nsummary: old summary\n---\n# Scratch handoff\n"))
	require.NoError(t, err)
	parent, err := tkt.AddNote(ticket.AddNoteOptions{Text: "Existing parent", Author: "parent-user"})
	require.NoError(t, err)
	data, err := tkt.Marshal()
	require.NoError(t, err)
	path := filepath.Join(store, "kon-safe.md")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return handoffFixture{
		env: []string{
			"KONTORA_URL=",
			config.PathEnvVar + "=" + configPath,
			config.TicketsDirEnvVar + "=" + store,
			config.LegacyTicketsDirEnvVar + "=",
		},
		ticketPath: path,
		parentID:   parent.ID,
	}
}

func assertHandoffStored(t *testing.T, fixture handoffFixture, verb, text, author, parent string) {
	t.Helper()
	tkt, err := ticket.ParseFile(fixture.ticketPath)
	require.NoError(t, err)
	notes := ticket.ParseNotes(tkt.Body)
	require.NotEmpty(t, notes)
	assert.Equal(t, "Existing parent", notes[0].Text)
	assert.Equal(t, fixture.parentID, notes[0].ID)
	assert.Equal(t, "parent-user", notes[0].Author)
	if verb == "summary" {
		// Summary's frontmatter retains whitespace; only stdin strips final LFs.
		assert.Equal(t, text, tkt.Summary)
		require.Len(t, notes, 1)
		return
	}
	assert.Equal(t, "old summary", tkt.Summary)
	require.Len(t, notes, 2)
	assert.Equal(t, strings.TrimSpace(text), notes[1].Text)
	assert.Equal(t, author, notes[1].Author)
	assert.Equal(t, parent, notes[1].ParentID)
	assert.NotEmpty(t, notes[1].ID)
}

func TestHandoffInput_LocalPersistence(t *testing.T) {
	for _, tc := range handoffInputCases() {
		for _, mode := range []string{"stdin", "positional"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				fixture := newHandoffFixture(t)
				env := append(slices.Clone(fixture.env), config.AgentEnvVar+"="+tc.agent)
				args := []string{tc.verb}
				author := "configured-user"
				if tc.agent != "" {
					author = tc.agent
				}
				if tc.author != "" {
					args = append(args, "--author", tc.author)
					author = tc.author
				}
				parent := ""
				if tc.reply {
					parent = fixture.parentID
					args = append(args, "--reply-to", parent)
				}
				args = append(args, "kon-safe")
				var stdin io.Reader
				want := handoffLiteralText
				if mode == "stdin" {
					stdin = strings.NewReader(handoffLiteralText)
					want = strings.TrimRight(want, "\n")
				} else {
					args = append(args, handoffLiteralText)
				}
				out, err := runCLIWithStdin(t, env, stdin, args...)
				require.NoError(t, err, out)
				ack := "note added"
				if tc.verb == "summary" {
					ack = "summary set"
				}
				assert.Contains(t, out, "kon-safe "+ack)
				assertHandoffStored(t, fixture, tc.verb, want, author, parent)
			})
		}
	}
}

func TestHandoffInput_RemoteRequests(t *testing.T) {
	for _, tc := range handoffInputCases() {
		for _, mode := range []string{"stdin", "positional"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				type request struct {
					method, path, contentType, auth string
					body                            map[string]string
					err                             error
				}
				requests := make(chan request, 4)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/api/tickets" {
						_ = json.NewEncoder(w).Encode(map[string]any{"tickets": []map[string]string{{"id": "abc123"}}})
						return
					}
					var body map[string]string
					err := json.NewDecoder(r.Body).Decode(&body)
					requests <- request{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), body, err}
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
				}))
				defer srv.Close()

				env := []string{"KONTORA_URL=" + srv.URL, "KONTORA_TOKEN=handoff-token", config.AgentEnvVar + "=" + tc.agent}
				args := []string{tc.verb}
				want := map[string]string{"text": handoffLiteralText}
				if tc.agent != "" {
					want["author"] = tc.agent
				}
				if tc.author != "" {
					args = append(args, "--author", tc.author)
					want["author"] = tc.author
				}
				if tc.reply {
					args = append(args, "--reply-to", "p123")
					want["parent"] = "p123"
				}
				args = append(args, "abc123")
				var stdin io.Reader
				if mode == "stdin" {
					stdin = strings.NewReader(handoffLiteralText)
					want["text"] = strings.TrimRight(handoffLiteralText, "\n")
				} else {
					args = append(args, handoffLiteralText)
				}
				out, err := runCLIWithStdin(t, env, stdin, args...)
				require.NoError(t, err, out)
				ack := "note added"
				if tc.verb == "summary" {
					ack = "summary set"
				}
				assert.Contains(t, out, "abc123 "+ack)
				require.Len(t, requests, 1, "exactly one mutation request")
				got := <-requests
				require.NoError(t, got.err)
				assert.Equal(t, http.MethodPost, got.method)
				assert.Equal(t, "/api/tickets/abc123/"+tc.verb, got.path)
				assert.Equal(t, "application/json", got.contentType)
				assert.Equal(t, "Bearer handoff-token", got.auth)
				// The remote CLI does not trim note whitespace or fill in the
				// config author: those are the receiving daemon's responsibility.
				assert.Equal(t, want, got.body)
			})
		}
	}
}

func TestHandoffInput_FileRedirection(t *testing.T) {
	for _, verb := range []string{"note", "summary"} {
		t.Run(verb, func(t *testing.T) {
			fixture := newHandoffFixture(t)
			scratch := t.TempDir()
			sentinel := filepath.Join(scratch, "must-not-exist")
			text := handoffLiteralText + "$(touch \"" + sentinel + "\") and `touch \"" + sentinel + "\"`\n\n"
			input := filepath.Join(scratch, "handoff input.txt")
			require.NoError(t, os.WriteFile(input, []byte(text), 0o600))
			cmd := exec.Command("sh", "-c", `exec "$1" "$2" "$3" < "$4"`,
				"handoff-redirection", buildKontora(t), verb, "kon-safe", input)
			cmd.Env = cliTestEnv(t, fixture.env)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
			assert.NoFileExists(t, sentinel, "neither substitution may execute")
			assertHandoffStored(t, fixture, verb, strings.TrimRight(text, "\n"), "configured-user", "")
		})
	}
}

func TestHandoffInput_InheritedRemoteSettings(t *testing.T) {
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Every inherited destination is scratch space or a test server. Even a
	// broken isolation helper must not send a request to a live daemon/store.
	inherited := t.TempDir()
	configPath := filepath.Join(inherited, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("invalid inherited config: [\n"), 0o600))
	for key, value := range map[string]string{
		"KONTORA_URL": srv.URL, "KONTORA_TOKEN": "inherited-secret",
		config.PathEnvVar:             configPath,
		config.TicketsDirEnvVar:       filepath.Join(inherited, "tickets"),
		config.LegacyTicketsDirEnvVar: filepath.Join(inherited, "legacy-tickets"),
		config.AgentEnvVar:            "inherited-agent", config.TicketEnvVar: "kon-safe", config.StageEnvVar: "inherited-stage",
		"XDG_CONFIG_HOME": inherited,
	} {
		t.Setenv(key, value)
	}
	for _, entry := range cliTestEnv(t, nil) {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "KONTORA_URL", "KONTORA_TOKEN", config.PathEnvVar,
			config.TicketsDirEnvVar, config.LegacyTicketsDirEnvVar,
			config.AgentEnvVar, config.TicketEnvVar, config.StageEnvVar:
			assert.Empty(t, value, key)
		case "XDG_CONFIG_HOME":
			assert.NotEqual(t, inherited, value)
		}
	}
	for _, verb := range []string{"note", "summary"} {
		t.Run(verb, func(t *testing.T) {
			fixture := newHandoffFixture(t)
			out, err := runCLIWithStdin(t, fixture.env, strings.NewReader(handoffLiteralText), verb, "kon-safe")
			require.NoError(t, err, out)
			assertHandoffStored(t, fixture, verb, strings.TrimRight(handoffLiteralText, "\n"), "configured-user", "")
		})
	}
	mu.Lock()
	assert.Zero(t, hits, "local fixtures must not use the inherited remote route")
	mu.Unlock()
	for _, dir := range []string{"tickets", "legacy-tickets"} {
		assert.NoDirExists(t, filepath.Join(inherited, dir), "inherited store must remain untouched")
	}
}

func TestRemoteDispatch_RunWithoutLocalConfig(t *testing.T) {
	var runPath, authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123/run":
			runPath = r.URL.Path
			authHeader = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL, "KONTORA_TOKEN=secret"}, "run", "abc")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123/run", runPath)
	assert.Equal(t, "Bearer secret", authHeader)
}

func TestRemoteDispatch_ListWithoutDirectoryIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, identity string
		useEnv         bool
	}{
		{name: "url with missing identity"},
		{name: "url with another board", identity: "/remote/board-a"},
		{name: "environment with missing identity", useEnv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boardB := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(boardB, "kon-shared.md"), []byte(
				"---\nid: kon-shared\nstatus: paused\nkontora: true\n---\n# Requested board B\n"), 0o600))
			var mu sync.Mutex
			var requests, auth []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				auth = append(auth, r.Header.Get("Authorization"))
				mu.Unlock()
				switch r.URL.Path {
				case "/api/config":
					_ = json.NewEncoder(w).Encode(web.ConfigInfo{TicketsDir: tc.identity})
				case "/api/tickets":
					_ = json.NewEncoder(w).Encode(map[string]any{"tickets": []web.TicketInfo{
						{ID: "kon-shared", Title: "Daemon board A", Status: "paused", Kontora: true, Ready: true},
					}})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			args := []string{"ls", "--json", "--tickets-dir", boardB,
				"--config", filepath.Join(t.TempDir(), "missing-config.yaml")}
			var env []string
			if tc.useEnv {
				env = []string{"KONTORA_URL=" + srv.URL, "KONTORA_TOKEN=explicit-token"}
			} else {
				args = append(args, "--url", srv.URL, "--token", "explicit-token")
			}
			out, err := runCLI(t, env, args...)
			require.NoError(t, err, out)
			var rows []map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &rows))
			require.Len(t, rows, 1)
			assert.Equal(t, "kon-shared", rows[0]["id"])
			assert.Equal(t, "Daemon board A", rows[0]["title"])
			assert.Equal(t, true, rows[0]["ready"])
			assert.NotContains(t, out, "Requested board B")
			mu.Lock()
			assert.Equal(t, []string{"GET /api/tickets"}, requests, "explicit remote mode must not probe local directory identity")
			assert.Equal(t, []string{"Bearer explicit-token"}, auth)
			mu.Unlock()
		})
	}
}

func TestRemoteDispatch_PauseSendsExactID(t *testing.T) {
	var pausePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123def"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123def/pause":
			pausePath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123def"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "pause", "abc")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123def/pause", pausePath)
}

func TestRemoteDispatch_View(t *testing.T) {
	const tail = "## Notes\n\n**2026-08-08**\n\none\n\n**2026-08-09**\n\ntwo\n\n**2026-08-10**\n\nthree\n\n## Code review\n\nfinding\n"
	const body = "# Title\n\nDescription.\n\n" + tail
	for _, tt := range []struct {
		name    string
		flags   []string
		want    string
		noNotes bool
	}{
		{name: "body", flags: []string{"--body"}, want: body},
		{name: "notes", flags: []string{"--notes"}, want: tail},
		{name: "last", flags: []string{"--last", "2"}, want: "**2026-08-09**\n\ntwo\n\n**2026-08-10**\n\nthree\n\n## Code review\n\nfinding\n"},
		{name: "above count", flags: []string{"--last", "5"}, want: tail},
		{name: "notes and last", flags: []string{"--notes", "--last", "1"}, want: "**2026-08-10**\n\nthree\n\n## Code review\n\nfinding\n"},
		{name: "no notes", flags: []string{"--notes"}, noNotes: true},
		{name: "no last notes", flags: []string{"--last", "2"}, noNotes: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/tickets":
					_ = json.NewEncoder(w).Encode(map[string]any{"tickets": []map[string]string{{"id": "abc123"}}})
				case "/api/tickets/abc123":
					info := map[string]string{"id": "abc123", "body": body, "description_tail": tail}
					if tt.noNotes {
						info["body"], info["description_tail"] = "# Title\n", ""
					}
					_ = json.NewEncoder(w).Encode(info)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			args := append([]string{"view"}, tt.flags...)
			args = append(args, "abc")
			out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL, "PAGER=false"}, args...)
			require.NoError(t, err, out)
			assert.Equal(t, tt.want, out)
		})
	}
}

func TestViewFlagErrors(t *testing.T) {
	for _, tt := range []struct {
		name  string
		flags []string
		want  []string
	}{
		{name: "body and notes", flags: []string{"--body", "--notes"}, want: []string{"usage:", "--body", "--notes"}},
		{name: "body and last", flags: []string{"--body", "--last", "2"}, want: []string{"usage:", "--body", "--last"}},
		{name: "zero", flags: []string{"--last", "0"}, want: []string{"usage:", "--last", "positive"}},
		{name: "negative", flags: []string{"--last", "-1"}, want: []string{"usage:", "--last", "positive"}},
		{name: "not integer", flags: []string{"--last", "two"}, want: []string{"invalid value", "-last"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"view"}, tt.flags...)
			args = append(args, "abc")
			out, err := runCLI(t, nil, args...)
			require.Error(t, err)
			for _, want := range tt.want {
				assert.Contains(t, out, want)
			}
		})
	}
}

func TestRemoteDispatch_NoteAppends(t *testing.T) {
	var notePath, noteText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123/note":
			notePath = r.URL.Path
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			noteText = body.Text
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "note", "abc", "blocked on review")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123/note", notePath)
	assert.Equal(t, "blocked on review", noteText)
}

func TestRemoteDispatch_ServerValidationErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		default:
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid state transition"})
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "run", "abc")
	require.Error(t, err)
	assert.Contains(t, out, "invalid state transition")
}

func TestRemoteDispatch_DeleteWithConfirmation(t *testing.T) {
	var deletePath, confirmHeader, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123":
			deletePath = r.URL.Path
			method = r.Method
			confirmHeader = r.Header.Get("X-Kontora-Confirm")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "delete", "abc", "-f")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123", deletePath)
	assert.Equal(t, http.MethodDelete, method)
	assert.Equal(t, "delete-ticket-file", confirmHeader)
}

func TestRemoteDispatch_DeleteWithoutConfirmationFails(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "delete", "abc")
	require.Error(t, err)
	assert.Contains(t, out, "confirmation")
	assert.False(t, hit, "no request should be sent without confirmation")
}

func TestRemoteDispatch_ExtraPositionalRejected(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A stray second positional must fail clearly rather than silently
	// swallowing the trailing flag and dying with a misleading error.
	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "delete", "abc", "def", "-f")
	require.Error(t, err)
	assert.Contains(t, out, "unexpected argument")
	assert.False(t, hit, "no request should be sent for malformed input")
}

func TestRemoteDispatch_UpdateSelectedFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "pipeline and empty agent omit notify fields",
			args: []string{"--pipeline", "two-stage", "--agent", ""},
			want: `{"pipeline":"two-stage","agent":""}`,
		},
		{
			name: "notify lists",
			args: []string{"--notify", " human_review, ,done,", "--notify-channels", " tg, hook "},
			want: `{"notify":["human_review","done"],"notify_channels":["tg","hook"]}`,
		},
		{
			name: "notify only",
			args: []string{"--notify", "waiting,done"},
			want: `{"notify":["waiting","done"]}`,
		},
		{
			name: "channels only",
			args: []string{"--notify-channels", "tg,hook"},
			want: `{"notify_channels":["tg","hook"]}`,
		},
		{
			name: "empty notify clears",
			args: []string{"--notify", ""},
			want: `{"notify":[]}`,
		},
		{
			name: "none notify clears",
			args: []string{"--notify", " , none, "},
			want: `{"notify":[]}`,
		},
		{
			name: "empty channels clears",
			args: []string{"--notify-channels", ""},
			want: `{"notify_channels":[]}`,
		},
		{
			name: "none channel retained",
			args: []string{"--notify-channels", " none "},
			want: `{"notify_channels":["none"]}`,
		},
		{
			name: "both empty",
			args: []string{"--notify", "", "--notify-channels", ""},
			want: `{"notify":[],"notify_channels":[]}`,
		},
		{
			name: "only singleton none is converted",
			args: []string{"--notify", "none,done", "--notify-channels", "none,tg"},
			want: `{"notify":["none","done"],"notify_channels":["none","tg"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]any{
						"tickets":        []map[string]string{{"id": "abc123"}},
						"running_agents": 0,
					})
				case r.URL.Path == "/api/tickets/abc123" && r.Method == http.MethodPut:
					gotBody, _ = io.ReadAll(r.Body)
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			// Flags after the ID must work, including explicit empty values.
			args := append([]string{"update", "abc", "--url", srv.URL}, tc.args...)
			out, err := runCLI(t, nil, args...)
			require.NoError(t, err, out)
			// Match the full wire body: omitted, null and [] are distinct.
			assert.JSONEq(t, tc.want, string(gotBody))
		})
	}
}

func TestRemoteDispatch_InitWithFlags(t *testing.T) {
	for _, tc := range []struct {
		name, kind, pipeline, path, status string
		flags                              []string
	}{
		{"ordinary", "", "two-stage", "/repo", "open", []string{"--pipeline", "two-stage", "--path", "/repo", "--status", "open"}},
		{"epic", "epic", "", "", "todo", []string{"--status", "todo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var initPath string
			var gotReq map[string]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]any{
						"tickets": []map[string]string{{"id": "abc123", "kind": tc.kind}}, "running_agents": 0,
					})
				case r.URL.Path == "/api/tickets/abc123" && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "abc123", Kind: tc.kind})
				case r.URL.Path == "/api/tickets/abc123/init":
					initPath = r.URL.Path
					_ = json.NewDecoder(r.Body).Decode(&gotReq)
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			args := append([]string{"init", "abc"}, tc.flags...)
			out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, args...)
			require.NoError(t, err, out)
			assert.Equal(t, "/api/tickets/abc123/init", initPath)
			assert.Equal(t, tc.pipeline, gotReq["pipeline"])
			assert.Equal(t, tc.path, gotReq["path"])
			assert.Equal(t, tc.status, gotReq["status"])
		})
	}
}

func TestRemoteDispatch_InitMissingFlagsFails(t *testing.T) {
	var initHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/init") {
			initHit = true
		}
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "init", "abc")
	require.Error(t, err)
	assert.Contains(t, out, "requires --pipeline and --path")
	assert.False(t, initHit, "daemon init endpoint must not be called")
}

func TestRemoteDispatch_ConfigEditSavesValid(t *testing.T) {
	var putContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/config/raw" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content": "max_concurrent_agents: 1\nagents:\n  claude:\n    binary: claude\n",
			})
		case r.URL.Path == "/api/config/raw" && r.Method == http.MethodPut:
			var body struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			putContent = body.Content
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	editor := writeFakeEditor(t, "max_concurrent_agents: 9\nagents:\n  claude:\n    binary: claude\n")
	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL, "EDITOR=" + editor}, "config", "edit")
	require.NoError(t, err, out)
	assert.Contains(t, putContent, "max_concurrent_agents: 9")
	assert.Contains(t, out, "Config saved and reloaded.")
}

func TestRemoteDispatch_ConfigEditInvalidNotSaved(t *testing.T) {
	var putHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/config/raw" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content": "agents:\n  claude:\n    binary: claude\n",
			})
		case r.URL.Path == "/api/config/raw" && r.Method == http.MethodPut:
			putHit = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Editor writes a config with an unknown field, which fails local validation.
	editor := writeFakeEditor(t, "totally_unknown_field: 1\n")
	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL, "EDITOR=" + editor}, "config", "edit")
	require.Error(t, err)
	assert.Contains(t, out, "invalid")
	assert.False(t, putHit, "invalid config must not be uploaded")
}

// writeFakeEditor creates an executable script that overwrites the file passed
// as its first argument with content, simulating a user editing in $EDITOR.
func writeFakeEditor(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-editor.sh")
	script := "#!/bin/sh\ncat > \"$1\" <<'KONTORA_EOF'\n" + content + "KONTORA_EOF\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestRemoteDispatch_LocalOnlyVerbsRejected(t *testing.T) {
	cases := []struct {
		verb string
		args []string
		// want is a fragment of the expected output. An empty want means the
		// verb has to succeed despite remote mode being on.
		want string
	}{
		{verb: "edit", args: []string{"edit", "abc"}, want: "not available in remote mode"},
		{verb: "archive", args: []string{"archive"}, want: "not available in remote mode"},
		{verb: "doctor", args: []string{"doctor"}, want: "not available in remote mode"},
		{verb: "start", args: []string{"start"}, want: "not available in remote mode"},
		{verb: "setup", args: []string{"setup"}, want: "not available in remote mode"},
		// fmt and completion touch neither the daemon nor a config file, so an
		// exported KONTORA_URL must leave them working.
		{verb: "completion", args: []string{"completion", "fish"}},
		{verb: "fmt", args: []string{"fmt"}},
	}

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			out, err := runCLI(t, []string{"KONTORA_URL=http://127.0.0.1:1"}, tc.args...)
			if tc.want == "" {
				require.NoError(t, err, out)
				return
			}
			require.Error(t, err, out)
			assert.Contains(t, out, tc.want)
		})
	}
}

func TestNewSendsAgentToDaemon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		fields string
	}{
		{name: "absent notify fields"},
		{
			name:   "notify lists",
			args:   []string{"--notify", " human_review, ,done,", "--notify-channels", " tg, hook "},
			fields: `,"notify":["human_review","done"],"notify_channels":["tg","hook"]`,
		},
		{
			name:   "notify only",
			args:   []string{"--notify", "waiting,done"},
			fields: `,"notify":["waiting","done"]`,
		},
		{
			name:   "channels only",
			args:   []string{"--notify-channels", "tg,hook"},
			fields: `,"notify_channels":["tg","hook"]`,
		},
		{
			name:   "empty notify skips default",
			args:   []string{"--notify", ""},
			fields: `,"notify":[]`,
		},
		{
			name:   "none notify skips default",
			args:   []string{"--notify", " , none, "},
			fields: `,"notify":[]`,
		},
		{
			name:   "empty channels",
			args:   []string{"--notify-channels", ""},
			fields: `,"notify_channels":[]`,
		},
		{
			name:   "none channel retained",
			args:   []string{"--notify-channels", " none "},
			fields: `,"notify_channels":["none"]`,
		},
		{
			name:   "both empty",
			args:   []string{"--notify", "", "--notify-channels", ""},
			fields: `,"notify":[],"notify_channels":[]`,
		},
		{
			name:   "only singleton none is converted",
			args:   []string{"--notify", "none,done", "--notify-channels", "none,tg"},
			fields: `,"notify":["none","done"],"notify_channels":["none","tg"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/tickets" && r.Method == http.MethodPost {
					gotBody, _ = io.ReadAll(r.Body)
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			args := append([]string{"new", "--url", srv.URL, "--path", "/repo", "--agent", "codex", "--pipeline", "none"}, tc.args...)
			args = append(args, "Remote ticket")
			out, err := runCLI(t, nil, args...)
			require.NoError(t, err, out)

			// Pipeline's sentinel is resolved by the daemon, but notify's
			// singleton "none" must already be an explicit empty JSON array.
			want := `{"title":"Remote ticket","path":"/repo","agent":"codex","pipeline":"none"` + tc.fields + `}`
			assert.JSONEq(t, want, string(gotBody))
		})
	}
}

// A remote schedule sends an absolute instant: --after is resolved on the
// caller's clock, so the delay is measured from when the command was typed.
func TestRemoteScheduleSendsAnInstant(t *testing.T) {
	cases := []struct {
		name string
		args []string
		// want is checked against the decoded body.
		want func(t *testing.T, body map[string]any)
	}{
		{
			name: "--at is normalized to UTC",
			args: []string{"schedule", "abc", "--at", "2099-09-01T11:00:00+02:00"},
			want: func(t *testing.T, body map[string]any) {
				assert.Equal(t, "2099-09-01T09:00:00Z", body["scheduled_at"])
				assert.Nil(t, body["clear"])
			},
		},
		{
			name: "--after becomes an instant",
			args: []string{"schedule", "abc", "--after", "24h"},
			want: func(t *testing.T, body map[string]any) {
				at, err := time.Parse(time.RFC3339, body["scheduled_at"].(string))
				require.NoError(t, err)
				assert.WithinDuration(t, time.Now().Add(24*time.Hour), at, time.Minute)
			},
		},
		{
			name: "--clear names no instant",
			args: []string{"schedule", "abc", "--clear"},
			want: func(t *testing.T, body map[string]any) {
				assert.Equal(t, true, body["clear"])
				assert.Nil(t, body["scheduled_at"])
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/tickets/abc/schedule" && r.Method == http.MethodPost {
					_ = json.NewDecoder(r.Body).Decode(&gotBody)
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc"})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc"})
			}))
			defer srv.Close()

			out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, tc.args...)
			require.NoError(t, err, out)
			require.NotNil(t, gotBody)
			tc.want(t, gotBody)
		})
	}
}

// A schedule and a status the schedule contradicts must not create anything.
func TestRemoteNewRejectsScheduledTodo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the CLI must reject the flags before it calls the daemon")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL},
		"new", "--path", "/repo", "--status", "todo", "--after", "24h", "Scheduled ticket")
	require.Error(t, err, out)
	assert.Contains(t, out, "contradicts")
}

func TestNewWritesAgentAndProjectDefaults(t *testing.T) {
	repoDir := testutil.InitRepo(t)
	wtDir := filepath.Join(t.TempDir(), "supervisor")
	out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "--detach", wtDir, "HEAD").CombinedOutput()
	require.NoError(t, err, string(out))
	wtDir, err = filepath.EvalSymlinks(wtDir)
	require.NoError(t, err)
	ticketsDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, fmt.Appendf(nil, `
tickets_dir: %s
default_agent: claude
agents:
  claude:
    binary: claude
  codex:
    binary: codex
stages:
  code:
    prompt: Write code.
pipelines:
  implement:
    - stage: code
      agent: claude
      on_success: done
      on_failure: pause
  child:
    - stage: code
      agent: claude
      on_success: done
      on_failure: pause
projects:
  repo:
    path: %s
    prefix: prj
    pipeline: implement
    epic_child_pipeline: child
    agent: claude
`, ticketsDir, repoDir), 0o644))

	cfg, err := config.Load(configPath)
	require.NoError(t, err)
	_, err = cli.New(cfg, cli.NewOpts{
		ID: "epc-100", Path: repoDir, Title: "Epic", Kind: "epic", EpicExecution: ticket.EpicExecutionAgent,
		Agent: "codex", Branch: "integration", BaseBranch: "epic-base", NoEdit: true,
	})
	require.NoError(t, err)

	cases := []struct {
		name       string
		path       string
		parent     string
		dir        string
		args       []string
		wantPath   string
		wantPrefix string
		contains   []string
		absent     []string
	}{
		{
			name: "project defaults fill blank fields", path: repoDir,
			args: []string{"Inherited"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"pipeline: implement", "agent: claude"},
		},
		{
			name: "explicit agent overrides the project default", path: repoDir,
			args: []string{"--agent", "codex", "Explicit agent"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"pipeline: implement", "agent: codex"},
		},
		{
			name: "none opts the pipeline out", path: repoDir,
			args: []string{"--pipeline", "none", "Standalone"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"agent: claude"}, absent: []string{"pipeline:"},
		},
		{
			name: "a supervisor child inherits the epic repository", parent: "epc-100",
			args: []string{"Child"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"pipeline: child", "agent: claude"},
		},
		{
			name: "a child resolves a parent prefix", parent: "epc-1",
			args: []string{"Child by prefix"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"pipeline: child", "agent: claude"},
		},
		{
			name: "an inherited path keeps an explicit agent", parent: "epc-100",
			args: []string{"--agent", "codex", "Explicit child agent"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"pipeline: child", "agent: codex"},
		},
		{
			name: "a child can name the ordinary pipeline explicitly", parent: "epc-100",
			args: []string{"--pipeline", "implement", "Named child"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"pipeline: implement", "agent: claude"},
		},
		{
			name: "a child can opt out of the pipeline", parent: "epc-100",
			args: []string{"--pipeline", "none", "Standalone child"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"agent: claude"}, absent: []string{"pipeline:"},
		},
		{
			name: "a child can opt out of the agent", parent: "epc-100",
			args: []string{"--agent", "none", "Stage agent child"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"pipeline: child"}, absent: []string{"agent:"},
		},
		{
			name: "a child can be created outside a repository", parent: "epc-100", dir: t.TempDir(),
			args: []string{"Child outside Git"}, wantPath: repoDir, wantPrefix: "prj-",
			contains: []string{"pipeline: child", "agent: claude"},
		},
		{
			name: "an explicit child path remains authoritative", path: wtDir, parent: "epc-100",
			args: []string{"Explicit worktree"}, wantPath: wtDir, wantPrefix: "sup-",
			absent: []string{"pipeline:", "agent:"},
		},
		{
			name: "an independent ticket still uses the current Git root",
			args: []string{"Independent"}, wantPath: wtDir, wantPrefix: "sup-",
			absent: []string{"pipeline:", "agent:"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"new", "--status", "open", "--quiet"}
			if tc.path != "" {
				args = append(args, "--path", tc.path)
			}
			if tc.parent != "" {
				args = append(args, "--parent", tc.parent)
			}
			args = append(args, tc.args...)
			dir := tc.dir
			if dir == "" {
				dir = wtDir
			}
			out, err := runCLIInDir(t, dir, []string{config.PathEnvVar + "=" + configPath}, nil, args...)
			require.NoError(t, err, out)

			id := strings.TrimSpace(out)
			assert.True(t, strings.HasPrefix(id, tc.wantPrefix), "ticket id %s", id)
			path := filepath.Join(ticketsDir, id+".md")
			tkt, err := ticket.ParseFile(path)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPath, tkt.Path)
			assert.Equal(t, ticket.StatusOpen, tkt.Status)
			assert.Empty(t, tkt.Branch)
			assert.Empty(t, tkt.BaseBranch)
			if tc.parent != "" {
				assert.Equal(t, "epc-100", tkt.Parent)
			} else {
				assert.Empty(t, tkt.Parent)
			}
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			content := string(data)
			for _, want := range tc.contains {
				assert.Contains(t, content, want)
			}
			for _, unwanted := range tc.absent {
				assert.NotContains(t, content, unwanted)
			}
		})
	}
}

func TestPrintRemoteTickets(t *testing.T) {
	tickets := []web.TicketInfo{
		{ID: "a-done", Status: "done", Kontora: true, Ready: true},
		{ID: "b-run", Status: "in_progress", Kontora: true, Ready: true},
		{ID: "c-arch", Status: "archived", Kontora: true, Ready: true},
		{ID: "d-foreign", Status: "todo", Ready: true},
	}

	cases := []struct {
		name        string
		tickets     []web.TicketInfo
		showClosed  bool
		wantIDs     []string
		wantMissing []string
		wantText    string
	}{
		{
			name:        "closed and archived are hidden by default",
			tickets:     tickets,
			wantIDs:     []string{"b-run", "d-foreign"},
			wantMissing: []string{"a-done", "c-arch"},
			wantText:    "not a kontora ticket",
		},
		{
			name:        "closed shows done but never archived",
			tickets:     tickets,
			showClosed:  true,
			wantIDs:     []string{"a-done", "b-run", "d-foreign"},
			wantMissing: []string{"c-arch"},
		},
		{
			name:     "hidden closed tickets say so",
			tickets:  []web.TicketInfo{{ID: "a-done", Status: "done", Kontora: true, Ready: true}},
			wantText: "Use --closed",
		},
		{
			name:     "no tickets at all",
			tickets:  nil,
			wantText: "No tickets.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			opts := cli.ListOpts{ShowClosed: tc.showClosed}
			require.NoError(t, cli.RenderList(remoteListing(tc.tickets, opts), &buf, opts))
			out := buf.String()

			for _, id := range tc.wantIDs {
				assert.Contains(t, out, id)
			}
			for _, id := range tc.wantMissing {
				assert.NotContains(t, out, id)
			}
			if tc.wantText != "" {
				assert.Contains(t, out, tc.wantText)
			}
			// in_progress outranks todo, which outranks done.
			if len(tc.wantIDs) > 1 {
				prev := -1
				for _, id := range []string{"b-run", "d-foreign", "a-done"} {
					if idx := strings.Index(out, id); idx >= 0 {
						assert.Greater(t, idx, prev, "%s is out of order", id)
						prev = idx
					}
				}
			}
		})
	}
}

func TestHelpGoesToStdoutAndExitsZero(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			out, err := runCLI(t, nil, arg)
			require.NoError(t, err, out)
			assert.Contains(t, out, "Usage: kontora")
		})
	}
}

func TestRemoteDispatch_FlagsAfterPositionals(t *testing.T) {
	// The handler runs on the server's goroutine, so hits needs a lock: each
	// subtest resets it from the test goroutine.
	var (
		mu   sync.Mutex
		hits []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tickets" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
			return
		}
		// Only the full ID exists, so ResolveID has to reach it through the
		// prefix rather than the direct lookup.
		if r.URL.Path == "/api/tickets/abc" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
	}))
	defer srv.Close()

	// A flag written after the positionals has to be parsed, not swallowed. The
	// token is the flag under test: the request only carries it when --token was
	// read from the tail of the command line.
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "pause", args: []string{"pause", "abc", "--token", "tail"}, want: "/api/tickets/abc123/pause"},
		{name: "move", args: []string{"move", "abc", "done", "--token", "tail"}, want: "/api/tickets/abc123/move"},
		{name: "set-stage", args: []string{"set-stage", "abc", "code", "--token", "tail"}, want: "/api/tickets/abc123/set-stage"},
		{name: "run", args: []string{"run", "abc", "--token", "tail"}, want: "/api/tickets/abc123/run"},
		{name: "schedule", args: []string{"schedule", "abc", "--after", "24h", "--token", "tail"}, want: "/api/tickets/abc123/schedule"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			hits = nil
			mu.Unlock()

			out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, tc.args...)
			require.NoError(t, err, out)

			mu.Lock()
			got := slices.Clone(hits)
			mu.Unlock()
			assert.Contains(t, got, tc.want)
		})
	}
}
