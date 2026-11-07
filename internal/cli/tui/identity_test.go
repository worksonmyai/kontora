package tui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestNewSourceBoardIdentity(t *testing.T) {
	requested := t.TempDir()
	other := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(requested, alias))
	writeTicket(t, requested, "tst-001", "todo", "Requested board")
	writeTicket(t, other, "tst-001", "todo", "Other board")
	file := filepath.Join(requested, "tst-001.md")
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "branch", "child"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(root, "branch", "child"), filepath.Join(root, "link")))
	require.NoError(t, os.Symlink(requested, filepath.Join(root, "branch", "board")))
	require.NoError(t, os.Symlink(other, filepath.Join(root, "board")))
	for _, tc := range []struct {
		name       string
		advertised string
		connected  bool
		requested  string
		diagnostic string
	}{
		{name: "same directory", advertised: requested, connected: true},
		{name: "directory alias", advertised: alias, connected: true},
		{name: "different board with identical ID", advertised: other, diagnostic: "does not match requested board"},
		{name: "symlink before parent", advertised: other, requested: root + "/link/../board", diagnostic: "does not match requested board"},
		{name: "older daemon", diagnostic: "identity missing"},
		{name: "relative identity", advertised: ".", diagnostic: "identity missing"},
		{name: "missing directory", advertised: filepath.Join(other, "missing"), diagnostic: "cannot compare daemon board"},
		{name: "file is not a directory", advertised: file, diagnostic: "cannot compare daemon board"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var unexpected atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/config" && r.Method == http.MethodGet {
					assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
					_ = json.NewEncoder(w).Encode(web.ConfigInfo{TicketsDir: tc.advertised, TmuxSession: "daemon-session", TmuxSocket: "/tmp/daemon.sock"})
					return
				}
				unexpected.Add(1)
				http.Error(w, "wrong board", http.StatusTeapot)
			}))
			defer srv.Close()
			board := requested
			if tc.requested != "" {
				board = tc.requested
			}
			cfg := &config.Config{TicketsDir: board, Web: config.Web{Host: "127.0.0.1", Port: srv.Listener.Addr().(*net.TCPAddr).Port, Token: "test-token"}}
			src := newSource(cfg)
			assert.Equal(t, tc.connected, src.Connected())
			if tc.connected {
				session, socket := src.Attachment()
				assert.Equal(t, "daemon-session", session)
				assert.Equal(t, "/tmp/daemon.sock", socket)
				assert.Empty(t, src.Diagnostic())
			} else {
				tickets, _, err := src.FetchTickets()
				require.NoError(t, err)
				require.Len(t, tickets, 1)
				assert.Equal(t, "Requested board", tickets[0].Title)
				info, err := src.FetchTask("tst-001")
				require.NoError(t, err)
				assert.Equal(t, "Requested board", info.Title)
				assert.Contains(t, src.Diagnostic(), tc.diagnostic)
				assert.Error(t, src.PauseTicket("tst-001"))
				assert.Error(t, src.RetryTicket("tst-001"))
				assert.Error(t, src.SkipStage("tst-001"))
				assert.Error(t, src.SetStage("tst-001", "implement"))
				assert.Error(t, src.UpdateTicket("tst-001", web.UpdateTicketRequest{}))
				_, err = src.CreateTicket(web.CreateTicketRequest{})
				assert.Error(t, err)
				assert.Nil(t, src.Subscribe(context.Background()))
				session, socket := src.Attachment()
				assert.Empty(t, session)
				assert.Empty(t, socket)
			}
			assert.Zero(t, unexpected.Load(), "no ticket request, mutation or subscription may reach the other board")
		})
	}
}

func TestNewSourceDeadline(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	result := make(chan Source, 1)
	go func() {
		result <- newSource(&config.Config{TicketsDir: t.TempDir(), Web: config.Web{Host: "127.0.0.1", Port: srv.Listener.Addr().(*net.TCPAddr).Port}})
	}()
	<-started
	select {
	case src := <-result:
		assert.False(t, src.Connected())
		assert.Contains(t, src.Diagnostic(), "context deadline exceeded")
	case <-time.After(4 * time.Second):
		t.Fatal("source selection exceeded its two-second deadline")
	}
}

func TestVerifiedSourcePreservesOwnershipGuard(t *testing.T) {
	dir := t.TempDir()
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		_ = json.NewEncoder(w).Encode(web.ConfigInfo{TicketsDir: dir})
	}))
	defer srv.Close()
	src := newSource(&config.Config{TicketsDir: dir, Web: config.Web{Host: "127.0.0.1", Port: srv.Listener.Addr().(*net.TCPAddr).Port}})
	require.True(t, src.Connected())
	require.NoError(t, src.PauseTicket("tst-001"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora", "authority.json"), []byte("persisted ownership"), 0o600))
	require.ErrorIs(t, src.PauseTicket("tst-001"), ticket.ErrManaged)
	assert.EqualValues(t, 1, writes.Load())
}

func TestNewSourceRejectsRedirect(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		_ = json.NewEncoder(w).Encode(web.ConfigInfo{TicketsDir: t.TempDir()})
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	src := newSource(&config.Config{TicketsDir: t.TempDir(), Web: config.Web{Host: "127.0.0.1", Port: srv.Listener.Addr().(*net.TCPAddr).Port}})
	assert.False(t, src.Connected())
	assert.False(t, followed.Load())
	assert.Contains(t, src.Diagnostic(), "redirects disabled")
}

func TestModelIdentityActions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		connected bool
		session   string
		socket    string
	}{
		{"file mode", false, "", ""},
		{"missing socket", true, "daemon", ""},
		{"missing session", true, "", "/tmp/daemon.sock"},
		{"verified attachment", true, "daemon", "/tmp/daemon.sock"},
	} {
		for _, view := range []viewState{viewList, viewDetail} {
			for _, key := range []string{"a", "p", "r", "s", "n", "g"} {
				t.Run(tc.name+"/"+string(rune('0'+view))+"/"+key, func(t *testing.T) {
					src := &mockSource{connected: tc.connected, session: tc.session, socket: tc.socket, diagnostic: "identity diagnostic"}
					m := newModel(src)
					m.width, m.height = 160, 40
					m.list.width, m.list.height = 160, 40
					m.list.setTickets([]web.TicketInfo{
						{ID: "tst-001", Title: "First", Status: "todo", Kontora: true},
						{ID: "tst-002", Title: "Second", Status: "todo", Kontora: true},
					}, 0)
					m.list.rowIdx = 1
					if view == viewDetail {
						m, _ = m.openDetail(web.TicketInfo{ID: "tst-001", Status: "todo", Kontora: true})
					}
					result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
					got := result.(model)
					if key == "a" && tc.session != "" && tc.socket != "" {
						assert.NotEmpty(t, got.attachTarget)
						require.NotNil(t, cmd)
					} else if key == "g" && view == viewList {
						assert.Zero(t, got.list.rowIdx)
						assert.NoError(t, got.list.err)
						assert.Nil(t, cmd)
					} else if !tc.connected || key == "a" {
						assert.Empty(t, got.attachTarget)
						assert.Nil(t, cmd)
						assert.Empty(t, src.actions)
						if view == viewList {
							assert.Error(t, got.list.err)
						} else {
							assert.Error(t, got.detail.err)
						}
					} else if key == "n" && view == viewDetail {
						assert.Nil(t, cmd)
						assert.Empty(t, src.actions)
					} else {
						require.NotNil(t, cmd)
						msg := cmd()
						id := "tst-001"
						if view == viewList {
							id = "tst-002"
						}
						switch key {
						case "p", "r", "s":
							assert.NoError(t, msg.(actionResultMsg).err)
							action := map[string]string{"p": "pause:", "r": "retry:", "s": "skip:"}[key]
							assert.Equal(t, []string{action + id}, src.actions)
						case "n":
							assert.NoError(t, msg.(configMsg).err)
						case "g":
							assert.NoError(t, msg.(agentConfigMsg).err)
							_, update := got.Update(msg)
							require.NotNil(t, update)
							assert.NoError(t, update().(agentUpdatedMsg).err)
							assert.Equal(t, []string{"update:" + id}, src.actions)
						}
					}
					rendered := got.View()
					assert.Contains(t, rendered, "identity diagnostic")
					if !tc.connected {
						for _, hint := range []string{"p pause", "r retry", "s skip", "n new", "g agent", "a attach"} {
							assert.NotContains(t, rendered, hint)
						}
					}
				})
			}
		}
	}
}

func TestDoAttachVerifiedSocket(t *testing.T) {
	if os.Getenv("KONTORA_TEST_ATTACH_HELPER") == "1" {
		require.NoError(t, doAttach("daemon-session", "/tmp/daemon.sock", "tst-001"))
		t.Fatal("attachment did not replace the process")
	}
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755))
	t.Setenv("PATH", binDir)
	t.Setenv("TMUX", "/tmp/conflicting.sock,123,0")
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(executable, "-test.run=^TestDoAttachVerifiedSocket$")
	cmd.Env = append(os.Environ(), "KONTORA_TEST_ATTACH_HELPER=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Equal(t, []string{"-S", "/tmp/daemon.sock", "attach-session", "-t", "=daemon-session:=tst-001", "-r"}, strings.Split(strings.TrimSpace(string(out)), "\n"))
}

func TestAttachArgsIdentity(t *testing.T) {
	t.Setenv("TMUX", "/tmp/conflicting.sock,123,0")
	for _, tc := range []struct {
		name    string
		session string
		socket  string
		fail    bool
	}{
		{"verified daemon", "daemon-session", "/tmp/daemon.sock", false},
		{"no socket", "daemon-session", "", true},
		{"relative socket", "daemon-session", "relative", true},
		{"no session", "", "/tmp/daemon.sock", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := attachArgs(tc.session, tc.socket, "tst-001")
			if tc.fail {
				require.ErrorIs(t, err, errAttachmentUnavailable)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{"tmux", "-S", "/tmp/daemon.sock", "attach-session", "-t", "=daemon-session:=tst-001", "-r"}, args)
		})
	}
}
