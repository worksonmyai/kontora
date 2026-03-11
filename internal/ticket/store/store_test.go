package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func writeTestTicket(t *testing.T, dir, id, content string) string {
	t.Helper()
	path := filepath.Join(dir, id+".md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestDiskRepo_Resolve_ExactMatch(t *testing.T) {
	dir := t.TempDir()
	writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Test\n")

	repo := NewDiskRepo(dir)
	id, err := repo.Resolve("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "tst-001", id)
}

func TestDiskRepo_Resolve_PrefixMatch(t *testing.T) {
	dir := t.TempDir()
	writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Test\n")

	repo := NewDiskRepo(dir)
	id, err := repo.Resolve("tst")
	require.NoError(t, err)
	assert.Equal(t, "tst-001", id)
}

func TestDiskRepo_Resolve_NotFound(t *testing.T) {
	dir := t.TempDir()
	repo := NewDiskRepo(dir)

	_, err := repo.Resolve("nonexistent")
	require.Error(t, err)
}

func TestDiskRepo_Get(t *testing.T) {
	dir := t.TempDir()
	writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\n---\n# Hello\n")

	repo := NewDiskRepo(dir)
	st, err := repo.Get("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "tst-001", st.Ticket.ID)
	assert.Equal(t, ticket.StatusTodo, st.Ticket.Status)
	assert.Equal(t, filepath.Join(dir, "tst-001.md"), st.FilePath)
}

func TestDiskRepo_Get_NotFound(t *testing.T) {
	dir := t.TempDir()
	repo := NewDiskRepo(dir)

	_, err := repo.Get("nonexistent")
	require.Error(t, err)
}

func TestDiskRepo_List(t *testing.T) {
	dir := t.TempDir()
	writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\n---\n# One\n")
	writeTestTicket(t, dir, "tst-002", "---\nid: tst-002\nstatus: open\n---\n# Two\n")

	repo := NewDiskRepo(dir)
	tickets, err := repo.List()
	require.NoError(t, err)
	assert.Len(t, tickets, 2)
}

func TestDiskRepo_List_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	repo := NewDiskRepo(dir)

	tickets, err := repo.List()
	require.NoError(t, err)
	assert.Empty(t, tickets)
}

func TestDiskRepo_List_NonexistentDir(t *testing.T) {
	repo := NewDiskRepo("/nonexistent/dir")
	tickets, err := repo.List()
	require.NoError(t, err)
	assert.Empty(t, tickets)
}

func TestDiskRepo_Save_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\ncustom: keep\n---\n# Hello\n")

	repo := NewDiskRepo(dir)
	st, err := repo.Get("tst-001")
	require.NoError(t, err)

	require.NoError(t, st.Ticket.SetField("status", "done"))
	require.NoError(t, repo.Save(st))

	// Re-read and verify.
	st2, err := repo.Get("tst-001")
	require.NoError(t, err)
	assert.Equal(t, ticket.StatusDone, st2.Ticket.Status)

	// Verify custom fields survived.
	data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "custom: keep")
}

func TestDaemonRepo_Get_ReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Test\n")

	repo := NewDaemonRepo(DaemonRepoCallbacks{
		PathLookup: func(id string) (string, error) {
			if id == "tst-001" {
				return path, nil
			}
			return "", fmt.Errorf("not found")
		},
	})

	st, err := repo.Get("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "tst-001", st.Ticket.ID)
}

func TestDaemonRepo_Resolve(t *testing.T) {
	repo := NewDaemonRepo(DaemonRepoCallbacks{
		PathLookup: func(id string) (string, error) {
			if id == "tst-001" {
				return "/tmp/tst-001.md", nil
			}
			return "", fmt.Errorf("not found")
		},
	})

	id, err := repo.Resolve("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "tst-001", id)

	_, err = repo.Resolve("missing")
	require.Error(t, err)
}

func TestDaemonRepo_Save_CallsWriteAndAfterSave(t *testing.T) {
	var writtenPath string
	var savedID string

	dir := t.TempDir()
	path := writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Test\n")

	repo := NewDaemonRepo(DaemonRepoCallbacks{
		PathLookup: func(id string) (string, error) {
			return path, nil
		},
		WriteTicket: func(t *ticket.Ticket, p string) error {
			writtenPath = p
			data, err := t.Marshal()
			if err != nil {
				return err
			}
			return os.WriteFile(p, data, 0o644)
		},
		AfterSave: func(id string, st *app.StoredTicket) {
			savedID = id
		},
	})

	st, err := repo.Get("tst-001")
	require.NoError(t, err)
	require.NoError(t, st.Ticket.SetField("status", "done"))
	require.NoError(t, repo.Save(st))

	assert.Equal(t, path, writtenPath)
	assert.Equal(t, "tst-001", savedID)
}

func TestDaemonRepo_List(t *testing.T) {
	want := []*app.StoredTicket{
		{Ticket: &ticket.Ticket{ID: "tst-001"}, FilePath: "/tmp/tst-001.md"},
	}

	repo := NewDaemonRepo(DaemonRepoCallbacks{
		ListTickets: func() []*app.StoredTicket { return want },
	})

	got, err := repo.List()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
