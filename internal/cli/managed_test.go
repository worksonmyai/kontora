package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestEditorRejectsImportedOwnership(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tst-001.md")
	before := []byte("---\nid: tst-001\n---\n# Original\n")
	require.NoError(t, os.WriteFile(path, before, 0o644))
	editor := filepath.Join(t.TempDir(), "editor")
	require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s\\n' '---' 'id: tst-001' 'managed: {}' '---' '# Imported' > \"$1\"\n"), 0o755))
	require.ErrorIs(t, Edit(dir, editor, "tst-001"), ticket.ErrManaged)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestLocalManagedRoutingRefusesAnotherDaemon(t *testing.T) {
	dir := t.TempDir()
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			if err := ticket.WithOfflineWrite(dir, func() error { return nil }); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer server.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	portNumber, err := strconv.Atoi(port)
	require.NoError(t, err)
	cfg := &config.Config{TicketsDir: dir}
	cfg.Web.Host, cfg.Web.Port = host, portNumber
	client := LocalClient(cfg)
	require.NoError(t, client.Summary("same-id", "ordinary"))
	require.Equal(t, int32(1), writes.Load())
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora", "authority.json"), []byte("persisted ownership"), 0o600))
	require.ErrorIs(t, client.Summary("same-id", "must not reach another board"), ticket.ErrManaged)
	require.Equal(t, int32(1), writes.Load())
}

func TestManagedOfflineWritersRefusePersistedReservation(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{TicketsDir: dir, Pipelines: map[string]config.Pipeline{"default": {{Stage: "code"}, {Stage: "review"}}}}
	for _, id := range []string{"tst-001", "tst-002"} {
		data := []byte("---\nid: " + id + "\nstatus: open\nkontora: true\npipeline: default\nstage: code\n---\n# Ticket\n")
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), data, 0o644))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".kontora"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora", "authority.json"), []byte("corrupt or unavailable authority"), 0o600))
	path := filepath.Join(dir, "tst-001.md")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	body := "replacement"
	for name, mutate := range map[string]func() error{
		"stage":   func() error { return SetStage(cfg, "tst-001", "review") },
		"init":    func() error { return Enable(cfg, "tst-001", EnableOpts{}, io.Discard) },
		"note":    func() error { return Note(dir, "tst-001", "note", NoteOptions{}) },
		"summary": func() error { return Summary(dir, "tst-001", "summary") },
		"delete":  func() error { return Delete(dir, "tst-001") },
		"editor":  func() error { return Edit(dir, "false", "tst-001") },
		"update":  func() error { return Update(cfg, "tst-001", web.UpdateTicketRequest{Body: &body}) },
		"status":  func() error { return SetStatus(cfg, "tst-001", "done") },
		"retry":   func() error { return Retry(dir, "tst-001") },
		"skip":    func() error { return Skip(cfg, "tst-001") },
		"link":    func() error { _, err := relationService(cfg).Link("tst-001", "tst-002"); return err },
		"archive": func() error {
			_, err := app.New(app.Static(cfg), store.NewDiskRepo(dir), app.NoopRuntime{}).ArchiveTicket("tst-001", "", "test")
			return err
		},
		"creation": func() error {
			_, err := New(cfg, NewOpts{ID: "tst-new", Path: "/unused", Title: "new", Status: "open", NoEdit: true})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, mutate())
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, before, actual)
		})
	}
	require.ErrorIs(t, Summary(dir, "tst-001", "summary"), ticket.ErrManaged)
}
