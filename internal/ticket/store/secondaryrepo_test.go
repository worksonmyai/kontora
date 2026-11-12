package store

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/frontmatter"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"gopkg.in/yaml.v3"
)

const secondaryFixture = `---
id: tst-001
kontora: true
status: %s
running_status: awaiting_data
kind: epic
epic_execution: agent
stage: review
attempt: 3
started_at: 2026-01-01T00:00:00Z
completed_at: 2026-01-02T00:00:00Z
created: 2025-01-01T00:00:00Z
claimed_by: another-daemon
invocation_id: invocation-001
annotation_return_status: paused
scheduled_at: 2030-01-01T00:00:00Z
archived_from: done
archived_at: 2026-01-03T00:00:00Z
archived_by: web
archive_note: keep
last_error: keep error
last_log: keep log
history:
  - stage: implement
    agent: old-agent
    exit_code: 0
    summary: historical summary
    started_at: 2026-01-01T00:00:00Z
    completed_at: 2026-01-02T00:00:00Z
imported_history_count: 1
custom:
  nested: [keep, 42]
summary: old summary
final_summary: old final summary
pipeline: old-pipeline
path: /old/repo
agent: old-agent
branch: old-branch
base_branch: old-base
notify: done
notify_channels: [desktop]
deps: [old-dep]
links: [old-link]
parent: old-parent
children: [old-child]
---
# Original

Description
`

func secondaryFrontmatter(t *testing.T, data []byte) map[string]any {
	t.Helper()
	fm, _, err := frontmatter.Split(string(data))
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(fm), &fields))
	return fields
}

func TestSecondaryRepoEditsEveryStatusPreserveProtectedFields(t *testing.T) {
	for _, status := range []string{"open", "todo", "in_progress", "awaiting_data", "paused", "human_review", "done", "cancelled", "archived", "closed", "foreign-status"} {
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()
			original := []byte(fmt.Sprintf(secondaryFixture, status))
			path := writeTestTicket(t, dir, "tst-001", string(original))
			repo := NewSecondaryRepo(dir)
			id, err := repo.Resolve("tst")
			require.NoError(t, err)
			require.Equal(t, "tst-001", id)
			listed, err := repo.List()
			require.NoError(t, err)
			require.Len(t, listed, 1)
			edits := map[string]any{
				"summary": "new summary", "final_summary": "new final summary",
				"pipeline": "new-pipeline", "path": "/new/repo", "agent": "new-agent",
				"branch": "new-branch", "base_branch": "new-base", "notify": []string{"done", "paused"},
				"notify_channels": []string{"other"}, "deps": []string{"new-dep"},
				"links": []string{"new-link"}, "parent": "new-parent", "children": []string{"new-child"},
			}
			require.NoError(t, repo.WithMutation(func(scoped app.MutationRepository) error {
				assertBoardLocks(t, dir, true)
				st, err := scoped.Get(id)
				require.NoError(t, err)
				for field, value := range edits {
					require.NoError(t, st.Ticket.SetField(field, value))
				}
				st.Ticket.SetBody("# Edited\n\nNew description\n\n## Notes\n\nA note\n")
				require.NoError(t, scoped.Save(st))
				fresh, err := scoped.Get(id)
				require.NoError(t, err)
				require.Equal(t, ticket.Status(status), fresh.Ticket.Status)
				require.Equal(t, "Edited", fresh.Ticket.Title())
				return nil
			}))
			out, err := os.ReadFile(path)
			require.NoError(t, err)
			before, after := secondaryFrontmatter(t, original), secondaryFrontmatter(t, out)
			for field := range edits {
				require.NotEqual(t, before[field], after[field], field)
				delete(before, field)
				delete(after, field)
			}
			require.Equal(t, before, after)
			st, err := repo.Get(id)
			require.NoError(t, err)
			st.Ticket.SetBody("# Direct Save\n")
			require.NoError(t, repo.Save(st))
			assertBoardLocks(t, dir, false)
		})
	}
}

func TestSecondaryRepoRejectsProtectedChanges(t *testing.T) {
	changes := map[string]any{
		"status": "done", "running_status": "other", "kontora": false, "kind": "", "epic_execution": "",
		"stage": "implement", "attempt": 0, "started_at": nil, "completed_at": nil,
		"created": nil, "claimed_by": "", "invocation_id": "", "annotation_return_status": "",
		"scheduled_at": "", "archived_from": "", "archived_at": nil, "archived_by": "",
		"archive_note": "", "last_error": "", "last_log": "", "history": []any{},
		"imported_history_count": 0, "custom": "changed", "new_unknown_lifecycle": true,
	}
	for field, value := range changes {
		for _, scoped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/scoped=%t", field, scoped), func(t *testing.T) {
				dir := t.TempDir()
				original := []byte(fmt.Sprintf(secondaryFixture, "in_progress"))
				path := writeTestTicket(t, dir, "tst-001", string(original))
				repo := NewSecondaryRepo(dir)
				mutate := func(r app.MutationRepository) error {
					st, err := r.Get("tst-001")
					require.NoError(t, err)
					st.Ticket.SetBody("# Allowed body edit combined with forbidden field\n")
					if field == "kind" {
						require.NoError(t, st.Ticket.DeleteField("epic_execution"))
					}
					require.NoError(t, st.Ticket.SetField(field, value))
					return r.Save(st)
				}
				var err error
				if scoped {
					err = repo.WithMutation(mutate)
				} else {
					err = mutate(repo)
				}
				require.ErrorIs(t, err, ErrSecondaryLifecycle)
				out, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, original, out)
			})
		}
	}
}

func TestSecondaryRepoRejectsProtectedFieldDeletion(t *testing.T) {
	dir := t.TempDir()
	original := fmt.Sprintf(secondaryFixture, "todo")
	path := writeTestTicket(t, dir, "tst-001", original)
	repo := NewSecondaryRepo(dir)
	for field := range secondaryFrontmatter(t, []byte(original)) {
		if slices.Contains([]string{"summary", "final_summary", "pipeline", "path", "agent", "branch", "base_branch", "notify", "notify_channels", "deps", "links", "parent", "children"}, field) {
			continue
		}
		t.Run(field, func(t *testing.T) {
			st, err := repo.Get("tst-001")
			require.NoError(t, err)
			if field == "kind" {
				require.NoError(t, st.Ticket.DeleteField("epic_execution"))
			}
			require.NoError(t, st.Ticket.DeleteField(field))
			require.Error(t, repo.Save(st))
			out, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, original, string(out))
		})
	}
}

func TestSecondaryRepoFreshProtectedSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: open\n---\n# Before\n")
	repo := NewSecondaryRepo(dir)
	stale, err := repo.Get("tst-001")
	require.NoError(t, err)
	fresh := []byte("---\nid: tst-001\nstatus: in_progress\ninvocation_id: running\n---\n# Fresh\n")
	require.NoError(t, os.WriteFile(path, fresh, 0o644))
	stale.Ticket.SetBody("# Stale edit\n")
	require.ErrorIs(t, repo.Save(stale), ErrSecondaryLifecycle)
	out, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, fresh, out)
}

func TestSecondaryRepoRetainsDiskProtections(t *testing.T) {
	for _, name := range []string{"nil", "nil ticket", "missing", "outside", "conflict filename", "unsafe id", "id mismatch", "incoming managed", "current managed", "other managed", "board control"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Before\n")
			repo := NewSecondaryRepo(dir)
			st, err := repo.Get("tst-001")
			require.NoError(t, err)
			st.Ticket.SetBody("# Edited\n")
			switch name {
			case "nil":
				st = nil
			case "nil ticket":
				st.Ticket = nil
			case "missing":
				require.NoError(t, os.Remove(path))
			case "outside":
				st.FilePath = filepath.Join(t.TempDir(), "tst-001.md")
			case "conflict filename":
				st.FilePath = filepath.Join(dir, "tst-001 2.md")
			case "unsafe id":
				st.Ticket.ID = "../escape"
			case "id mismatch":
				require.NoError(t, os.WriteFile(path, []byte("---\nid: other\nstatus: todo\n---\n# Wrong\n"), 0o644))
			case "incoming managed":
				require.NoError(t, st.Ticket.SetField("managed", nil))
			case "current managed", "other managed":
				id := "tst-001"
				if name == "other managed" {
					id = "other"
				}
				writeTestTicket(t, dir, id, "---\nid: "+id+"\nmanaged: null\n---\n# Managed\n")
			case "board control":
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".kontora"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora", "authority.json"), []byte("{}"), 0o600))
			}
			before, beforeErr := os.ReadFile(path)
			err = repo.Save(st)
			require.Error(t, err)
			if name == "incoming managed" || name == "current managed" || name == "other managed" || name == "board control" {
				require.ErrorIs(t, err, ticket.ErrManaged)
			}
			after, afterErr := os.ReadFile(path)
			if beforeErr != nil {
				require.ErrorIs(t, afterErr, os.ErrNotExist)
			} else {
				require.NoError(t, afterErr)
				require.Equal(t, before, after)
			}
		})
	}
}

func TestSecondaryRepoComparesSerializedFields(t *testing.T) {
	for _, mode := range []string{"typed lifecycle only", "protected YAML rewrite", "remove editable fields"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			original := "---\nid: tst-001\nstatus: \"todo\"\npipeline: pipeline\nsummary: summary\n---\n# Test\n"
			path := writeTestTicket(t, dir, "tst-001", original)
			repo := NewSecondaryRepo(dir)
			st, err := repo.Get("tst-001")
			require.NoError(t, err)
			switch mode {
			case "typed lifecycle only":
				st.Ticket.Status = ticket.StatusDone
				st.Ticket.Kontora = true
				st.Ticket.Stage = "review"
				st.Ticket.InvocationID = "fake-run"
			case "protected YAML rewrite":
				require.NoError(t, st.Ticket.SetField("status", "todo"))
			case "remove editable fields":
				require.NoError(t, st.Ticket.DeleteField("pipeline"))
				require.NoError(t, st.Ticket.DeleteField("summary"))
			}
			err = repo.Save(st)
			if mode == "protected YAML rewrite" {
				require.ErrorIs(t, err, ErrSecondaryLifecycle)
			} else {
				require.NoError(t, err)
			}
			out, err := os.ReadFile(path)
			require.NoError(t, err)
			if mode == "protected YAML rewrite" {
				require.Equal(t, original, string(out))
			}
			fresh, err := ticket.ParseBytes(out)
			require.NoError(t, err)
			require.Equal(t, ticket.StatusTodo, fresh.Status)
			require.False(t, fresh.Kontora)
			require.Empty(t, fresh.Stage)
			require.Empty(t, fresh.InvocationID)
			if mode == "remove editable fields" {
				require.Empty(t, fresh.Pipeline)
				require.Empty(t, fresh.Summary)
			}
		})
	}
}

func TestSecondaryRepoProtectsAliasesIntoEditableFields(t *testing.T) {
	dir := t.TempDir()
	path := writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nagent: &shared original\ncustom: *shared\n---\n# Test\n")
	repo := NewSecondaryRepo(dir)
	st, err := repo.Get("tst-001")
	require.NoError(t, err)
	require.NoError(t, st.Ticket.SetField("agent", "changed"))
	err = repo.Save(st)
	require.Error(t, err)
	out, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(out), "original")
}
