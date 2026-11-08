package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/frontmatter"
	"github.com/worksonmyai/kontora/internal/ticket"
	"gopkg.in/yaml.v3"
)

const transferID = "tst-transfer"

func transferFixture(t *testing.T, fields string, reactions bool) TransferRequest {
	t.Helper()
	source, destination := t.TempDir(), t.TempDir()
	md := "---\nid: " + transferID + "\nstatus: paused\n" + fields + "---\n# Transfer\n\nDescription and notes stay.\n"
	require.NoError(t, os.WriteFile(filepath.Join(source, transferID+".md"), []byte(md), 0o644))
	if reactions {
		require.NoError(t, os.WriteFile(filepath.Join(source, transferID+".notes.json"), []byte("{\"version\":1,\"reactions\":{}}\n"), 0o644))
	}
	rev, err := Revision(source, transferID)
	require.NoError(t, err)
	return TransferRequest{SourceDir: source, DestinationDir: destination, ID: transferID, Revision: rev}
}

func TestTransferPreservesDocumentAndNormalizesRuntime(t *testing.T) {
	for _, enrollment := range []string{"", "kontora: false\n", "kontora: true\n"} {
		t.Run(strings.TrimSpace(enrollment), func(t *testing.T) {
			req := transferFixture(t, enrollment+`stage: implement
attempt: 3
started_at: 2026-01-01T00:00:00Z
completed_at: 2026-01-02T00:00:00Z
claimed_by: stale-instance
scheduled_at: 2027-01-01T00:00:00Z
archived_from: done
archived_at: 2026-01-02T00:00:00Z
archived_by: somebody
archive_note: old archive reason
last_error: old error
last_log: /foreign/log
pipeline: code
agent: pi
path: /foreign/repo
branch: work
base_branch: main
deps: [other]
parent: epic
custom: &custom
  value: keep # custom comment
copy: *custom
history:
  - stage: code
    agent: pi
    summary: imported summary
    started_at: 2026-01-01T00:00:00Z
    exit_code: 0
    future_history_field: keep # history comment
`, true)
			before, err := os.ReadFile(filepath.Join(req.SourceDir, req.ID+".notes.json"))
			require.NoError(t, err)
			got, err := Transfer(req)
			require.NoError(t, err)
			assert.Equal(t, ticket.StatusOpen, got.Status)
			assert.Equal(t, "", got.Stage)
			assert.Zero(t, got.Attempt)
			assert.Nil(t, got.StartedAt)
			assert.Nil(t, got.CompletedAt)
			assert.Empty(t, got.ClaimedBy)
			assert.Empty(t, got.ScheduledAt)
			assert.Empty(t, got.ArchivedFrom)
			assert.Nil(t, got.ArchivedAt)
			assert.Empty(t, got.ArchivedBy)
			assert.Empty(t, got.ArchiveNote)
			assert.Empty(t, got.LastLog)
			assert.Empty(t, got.LastError)
			assert.Equal(t, "code", got.Pipeline)
			assert.Equal(t, "pi", got.Agent)
			assert.Equal(t, "work", got.Branch)
			assert.Equal(t, "main", got.BaseBranch)
			assert.Equal(t, []string{"other"}, got.Deps)
			assert.Equal(t, "epic", got.Parent)
			destination, err := filepath.EvalSymlinks(req.DestinationDir)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(destination, req.ID+".md"), got.FilePath)
			out, err := os.ReadFile(got.FilePath)
			require.NoError(t, err)
			assert.Contains(t, string(out), "future_history_field: keep # history comment")
			assert.Contains(t, string(out), "value: keep # custom comment")
			assert.Contains(t, string(out), "copy: *custom")
			assert.Contains(t, string(out), "imported_history_count: 1")
			assert.Contains(t, string(out), "Description and notes stay.")
			for _, field := range []string{"archived_from:", "archived_at:", "archived_by:", "archive_note:"} {
				assert.NotContains(t, string(out), field)
			}
			if enrollment == "" {
				assert.NotContains(t, string(out), "kontora:")
			} else {
				assert.Contains(t, string(out), strings.TrimSpace(enrollment))
			}
			after, err := os.ReadFile(filepath.Join(req.DestinationDir, req.ID+".notes.json"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			for _, suffix := range []string{".md", ".notes.json"} {
				_, err := os.Lstat(filepath.Join(req.SourceDir, req.ID+suffix))
				assert.ErrorIs(t, err, os.ErrNotExist)
			}
			require.NoError(t, ticket.CheckBoardControl(req.SourceDir))
			require.NoError(t, ticket.CheckBoardControl(req.DestinationDir))
			blocked, err := RecoverTransfers(req.DestinationDir)
			require.NoError(t, err)
			assert.Empty(t, blocked)
		})
	}
}

func TestTransferNormalizesAliasesAndMerges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
		status string
		custom any
	}{
		{name: "removed scalar anchor", fields: "stage: &oldstage implement\ncustom: *oldstage\n", custom: "implement"},
		{name: "removed timestamp anchor", fields: "started_at: &when 2026-01-01T00:00:00Z\ncustom: [*when]\n", custom: []any{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{name: "replaced status anchor", status: "&oldstatus paused", fields: "custom: *oldstatus\n", custom: "paused"},
		{name: "replaced history count anchor", fields: "imported_history_count: &count 7\ncustom: {count: *count}\n", custom: map[string]any{"count": 7}},
		{name: "inline merge", fields: "<<: {stage: implement, attempt: 3, scheduled_at: '2020-01-01T00:00:00Z', last_log: /old/log, custom: keep}\n", custom: "keep"},
		{name: "aliased merge", fields: `defaults: &defaults
  stage: &stage implement
  attempt: 3
  started_at: 2020-01-01T00:00:00Z
  completed_at: 2020-01-02T00:00:00Z
  claimed_by: stale-instance
  invocation_id: ''
  scheduled_at: '2020-01-01T00:00:00Z'
  annotation_return_status: ''
  archived_from: done
  archived_at: 2020-01-02T00:00:00Z
  archived_by: somebody
  archive_note: old reason
  last_error: old error
  last_log: /old/log
  custom: *stage
<<: *defaults
`, custom: "implement"},
		{name: "merge sequence precedence", fields: "<<: [{stage: implement, custom: first}, {custom: second, scheduled_at: '2020-01-01T00:00:00Z'}]\n", custom: "first"},
		{name: "explicit overrides merge", fields: "<<: {stage: implement, custom: inherited}\ncustom: explicit\n", custom: "explicit"},
		{name: "alias into removed inline merge", fields: "<<: &defaults {stage: implement, custom: &value keep}\ncopy: *defaults\n", custom: "keep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := transferFixture(t, tc.fields, true)
			if tc.status != "" {
				path := filepath.Join(req.SourceDir, req.ID+".md")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(data), "status: paused", "status: "+tc.status, 1)), 0o644))
				var revisionErr error
				req.Revision, revisionErr = Revision(req.SourceDir, req.ID)
				require.NoError(t, revisionErr)
			}
			before, err := os.ReadFile(filepath.Join(req.SourceDir, req.ID+".md"))
			require.NoError(t, err)
			fm, body, err := frontmatter.Split(string(before))
			require.NoError(t, err)
			var expected map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(fm), &expected))
			for _, field := range []string{"stage", "attempt", "started_at", "completed_at", "claimed_by", "invocation_id",
				"scheduled_at", "annotation_return_status", "archived_from", "archived_at", "archived_by", "archive_note", "last_error", "last_log"} {
				delete(expected, field)
			}
			expected["status"] = "open"
			expected["imported_history_count"] = 0
			got, err := Transfer(req)
			require.NoError(t, err)
			out, err := os.ReadFile(got.FilePath)
			require.NoError(t, err)
			parsed, err := ticket.ParseBytes(out)
			require.NoError(t, err)
			assert.Equal(t, ticket.StatusOpen, parsed.Status)
			assert.Empty(t, parsed.ScheduledAt)
			fm, actualBody, err := frontmatter.Split(string(out))
			require.NoError(t, err)
			var actual map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(fm), &actual))
			assert.Equal(t, expected, actual)
			assert.Equal(t, tc.custom, actual["custom"])
			assert.Equal(t, body, actualBody)
			_, err = os.Stat(filepath.Join(req.SourceDir, req.ID+".md"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestTransferRefusalsLeaveSourceUntouched(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		setup  func(*testing.T, *TransferRequest)
	}{
		{name: "running", fields: "status: in_progress\n"},
		{name: "invocation", fields: "invocation_id: live\n"},
		{name: "annotation", fields: "annotation_return_status: paused\n"},
		{name: "managed", fields: "managed: {}\n"},
		{name: "null ownership", fields: "managed: null\n"},
		{name: "invalid custom alias", fields: "custom: &cycle {self: *cycle}\n"},
		{name: "serialized custom type changes", fields: "custom: [2026-01-01T00:00:00Z]\n"},
		{name: "stale markdown", setup: func(t *testing.T, req *TransferRequest) {
			require.NoError(t, os.WriteFile(filepath.Join(req.SourceDir, req.ID+".md"), []byte("---\nid: "+req.ID+"\n---\n# Changed\n"), 0o644))
		}},
		{name: "stale reactions", setup: func(t *testing.T, req *TransferRequest) {
			require.NoError(t, os.WriteFile(filepath.Join(req.SourceDir, req.ID+".notes.json"), []byte("{}"), 0o644))
		}},
		{name: "destination markdown", setup: func(t *testing.T, req *TransferRequest) {
			require.NoError(t, os.WriteFile(filepath.Join(req.DestinationDir, req.ID+".md"), []byte("destination"), 0o644))
		}},
		{name: "destination reactions", setup: func(t *testing.T, req *TransferRequest) {
			require.NoError(t, os.WriteFile(filepath.Join(req.DestinationDir, req.ID+".notes.json"), []byte("destination"), 0o644))
		}},
		{name: "dangling destination markdown", setup: func(t *testing.T, req *TransferRequest) {
			require.NoError(t, os.Symlink("missing", filepath.Join(req.DestinationDir, req.ID+".md")))
		}},
		{name: "dangling destination reactions", setup: func(t *testing.T, req *TransferRequest) {
			require.NoError(t, os.Symlink("missing", filepath.Join(req.DestinationDir, req.ID+".notes.json")))
		}},
		{name: "controlled source", setup: func(t *testing.T, req *TransferRequest) {
			require.NoError(t, os.MkdirAll(filepath.Join(req.SourceDir, ".kontora", "state"), 0o700))
		}},
		{name: "controlled destination", setup: func(t *testing.T, req *TransferRequest) {
			require.NoError(t, os.MkdirAll(filepath.Join(req.DestinationDir, ".kontora", "state"), 0o700))
		}},
		{name: "parent conflict", setup: func(_ *testing.T, req *TransferRequest) {
			req.Validate = func(*ticket.Ticket) error { return errors.New("cached execution or artifact collision") }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := tc.fields
			if strings.HasPrefix(fields, "status:") {
				fields = ""
			}
			req := transferFixture(t, fields, false)
			if tc.name == "running" {
				path := filepath.Join(req.SourceDir, req.ID+".md")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(data), "status: paused", "status: in_progress", 1)), 0o644))
				req.Revision, err = Revision(req.SourceDir, req.ID)
				require.NoError(t, err)
			}
			if tc.setup != nil {
				tc.setup(t, &req)
			}
			before, err := os.ReadFile(filepath.Join(req.SourceDir, req.ID+".md"))
			require.NoError(t, err)
			_, err = Transfer(req)
			require.Error(t, err)
			if tc.name == "serialized custom type changes" {
				require.ErrorContains(t, err, "normalized ticket changed preserved fields")
			}
			_, err = os.Stat(filepath.Join(req.SourceDir, captureDirectory))
			assert.ErrorIs(t, err, os.ErrNotExist)
			after, err := os.ReadFile(filepath.Join(req.SourceDir, req.ID+".md"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			entries, err := os.ReadDir(filepath.Join(req.DestinationDir, transferDirectory))
			if !errors.Is(err, os.ErrNotExist) {
				require.NoError(t, err)
				assert.Empty(t, entries)
			}
		})
	}
}

func TestRevisionAndTransferRejectUnsafeFiles(t *testing.T) {
	for _, suffix := range []string{".md", ".notes.json"} {
		for _, kind := range []string{"symlink", "dangling", "directory", "fifo"} {
			t.Run(suffix+kind, func(t *testing.T) {
				req := transferFixture(t, "", true)
				path := filepath.Join(req.SourceDir, req.ID+suffix)
				require.NoError(t, os.Remove(path))
				switch kind {
				case "symlink":
					target := filepath.Join(t.TempDir(), "external")
					require.NoError(t, os.WriteFile(target, []byte("external"), 0o644))
					require.NoError(t, os.Symlink(target, path))
				case "dangling":
					require.NoError(t, os.Symlink("missing", path))
				case "directory":
					require.NoError(t, os.Mkdir(path, 0o700))
				case "fifo":
					require.NoError(t, syscall.Mkfifo(path, 0o600))
				}
				_, err := Revision(req.SourceDir, req.ID)
				require.Error(t, err)
				_, err = Transfer(req)
				require.Error(t, err)
			})
		}
	}
}

func TestTransferRejectsPendingIncomingSource(t *testing.T) {
	for _, checkpoint := range []string{"published-markdown", "committed"} {
		t.Run(checkpoint, func(t *testing.T) {
			req := transferFixture(t, "", true)
			_, err := transfer(req, func(step string) error {
				if step == checkpoint {
					return errors.New("stop")
				}
				return nil
			})
			require.Error(t, err)
			onward := TransferRequest{SourceDir: req.DestinationDir, DestinationDir: t.TempDir(), ID: req.ID}
			onward.Revision, err = Revision(onward.SourceDir, onward.ID)
			require.NoError(t, err)
			preserved := make(map[string][]byte)
			for _, suffix := range []string{".md", ".notes.json"} {
				path := filepath.Join(onward.SourceDir, onward.ID+suffix)
				preserved[path], err = os.ReadFile(path)
				require.NoError(t, err)
			}
			_, err = Transfer(onward)
			require.ErrorContains(t, err, "pending transfer")
			for path, before := range preserved {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after)
			}
			_, err = os.Stat(filepath.Join(onward.DestinationDir, onward.ID+".md"))
			assert.ErrorIs(t, err, os.ErrNotExist)
			blocked, err := RecoverTransfers(req.DestinationDir)
			require.NoError(t, err)
			assert.Empty(t, blocked)
			_, err = Transfer(onward)
			require.NoError(t, err)
			blocked, err = RecoverTransfers(req.DestinationDir)
			require.NoError(t, err)
			assert.Empty(t, blocked)
			for _, suffix := range []string{".md", ".notes.json"} {
				_, err := os.Stat(filepath.Join(onward.SourceDir, onward.ID+suffix))
				assert.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestTransferIgnoresUnrelatedUnavailableRecords(t *testing.T) {
	for _, board := range []string{"source", "destination"} {
		t.Run(board, func(t *testing.T) {
			req := transferFixture(t, "", false)
			destination := req.SourceDir
			if board == "destination" {
				destination = req.DestinationDir
			}
			incoming := TransferRequest{SourceDir: t.TempDir(), DestinationDir: destination, ID: "tst-pending"}
			require.NoError(t, os.WriteFile(filepath.Join(incoming.SourceDir, incoming.ID+".md"), []byte("---\nid: "+incoming.ID+"\nstatus: open\n---\n# Pending\n"), 0o644))
			var err error
			incoming.Revision, err = Revision(incoming.SourceDir, incoming.ID)
			require.NoError(t, err)
			_, err = transfer(incoming, func(step string) error {
				if step == "prepared" {
					return errors.New("stop")
				}
				return nil
			})
			require.ErrorContains(t, err, "stop")
			require.NoError(t, os.Rename(incoming.SourceDir, incoming.SourceDir+".unavailable"))
			_, err = HasPendingTransfer(destination, incoming.ID)
			require.Error(t, err)
			pending, err := HasPendingTransfer(destination, req.ID)
			require.NoError(t, err)
			assert.False(t, pending)
			_, err = Transfer(req)
			require.NoError(t, err)
			require.NoError(t, os.Rename(incoming.SourceDir+".unavailable", incoming.SourceDir))
			blocked, err := RecoverTransfers(destination)
			require.NoError(t, err)
			assert.Empty(t, blocked)
		})
	}
}

func TestPendingTransferRecognizesDestinationAlias(t *testing.T) {
	req := transferFixture(t, "", false)
	_, err := transfer(req, func(step string) error {
		if step == "published-markdown" {
			return errors.New("stop before commit")
		}
		return nil
	})
	require.Error(t, err)
	alias := filepath.Join(t.TempDir(), "destination-alias")
	require.NoError(t, os.Symlink(req.DestinationDir, alias))
	pending, err := HasPendingTransfer(alias, req.ID)
	require.NoError(t, err)
	assert.True(t, pending)
	blocked, err := RecoverTransfers(alias)
	require.NoError(t, err)
	assert.Empty(t, blocked)
}

func TestCommittedRecoveryNeverDeletesRecreatedSource(t *testing.T) {
	req := transferFixture(t, "", false)
	_, err := transfer(req, func(step string) error {
		if step == "committed" {
			return errors.New("stop before cleanup")
		}
		return nil
	})
	require.Error(t, err)
	path := filepath.Join(req.SourceDir, req.ID+".md")
	require.NoError(t, os.WriteFile(path, []byte("recreated source"), 0o644))
	blocked, err := RecoverTransfers(req.DestinationDir)
	require.NoError(t, err)
	assert.Empty(t, blocked)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "recreated source", string(got))
}

func TestTransferRecoveryAtCheckpoints(t *testing.T) {
	for _, checkpoint := range []string{"prepared", "captured-markdown", "captured-reactions", "published-reactions", "published-markdown", "committed"} {
		t.Run(checkpoint, func(t *testing.T) {
			req := transferFixture(t, "", true)
			interrupted := errors.New("simulated interruption")
			_, err := transfer(req, func(step string) error {
				if step == checkpoint {
					return interrupted
				}
				return nil
			})
			require.ErrorIs(t, err, interrupted)
			blocked, err := RecoverTransfers(req.DestinationDir)
			require.NoError(t, err)
			assert.Empty(t, blocked)
			got, err := ticket.ParseFile(filepath.Join(req.DestinationDir, req.ID+".md"))
			require.NoError(t, err)
			assert.Equal(t, ticket.StatusOpen, got.Status)
			assert.Equal(t, transferID, got.ID)
			_, err = os.Stat(filepath.Join(req.SourceDir, req.ID+".md"))
			assert.ErrorIs(t, err, os.ErrNotExist)
			_, err = RecoverTransfers(req.DestinationDir)
			require.NoError(t, err)
		})
	}
}

func TestTransferRecoveryRetainsConflictingVersions(t *testing.T) {
	for _, collision := range []string{"source", "destination", "destination reactions", "captured contents", "corrupt record"} {
		t.Run(collision, func(t *testing.T) {
			req := transferFixture(t, "", true)
			_, err := transfer(req, func(step string) error {
				if step == "captured-reactions" {
					return errors.New("stop")
				}
				return nil
			})
			require.Error(t, err)
			entries, err := os.ReadDir(filepath.Join(req.DestinationDir, transferDirectory))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			txn := filepath.Join(req.DestinationDir, transferDirectory, entries[0].Name())
			capture := filepath.Join(req.SourceDir, captureDirectory, entries[0].Name())
			var conflictPath string
			switch collision {
			case "source":
				conflictPath = filepath.Join(req.SourceDir, req.ID+".md")
			case "destination":
				conflictPath = filepath.Join(req.DestinationDir, req.ID+".md")
			case "destination reactions":
				conflictPath = filepath.Join(req.DestinationDir, req.ID+".notes.json")
			case "captured contents":
				conflictPath = filepath.Join(capture, "markdown")
			case "corrupt record":
				conflictPath = filepath.Join(txn, "record.json")
			}
			require.NoError(t, os.WriteFile(conflictPath, []byte("new version"), 0o644))
			blocked, err := RecoverTransfers(req.DestinationDir)
			require.Error(t, err)
			assert.True(t, blocked[req.ID])
			data, err := os.ReadFile(conflictPath)
			require.NoError(t, err)
			assert.Equal(t, "new version", string(data))
			_, err = os.Stat(filepath.Join(capture, "markdown"))
			require.NoError(t, err)
			_, err = os.Stat(filepath.Join(txn, "markdown"))
			require.NoError(t, err)
		})
	}
}

func TestTransferRecoveryBlocksMissingPrivateState(t *testing.T) {
	for _, missing := range []string{"capture directory", "capture root", "staged markdown", "staged reactions"} {
		t.Run(missing, func(t *testing.T) {
			req := transferFixture(t, "", true)
			interrupted := errors.New("stop before commit")
			_, err := transfer(req, func(step string) error {
				if step == "published-markdown" {
					return interrupted
				}
				return nil
			})
			require.ErrorIs(t, err, interrupted)
			entries, err := os.ReadDir(filepath.Join(req.DestinationDir, transferDirectory))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			txn := filepath.Join(req.DestinationDir, transferDirectory, entries[0].Name())
			capture := filepath.Join(req.SourceDir, captureDirectory, entries[0].Name())
			var path string
			switch missing {
			case "capture directory":
				path = capture
			case "capture root":
				path = filepath.Dir(capture)
			case "staged markdown":
				path = filepath.Join(txn, "markdown")
			case "staged reactions":
				path = filepath.Join(txn, "reactions")
			}
			require.NoError(t, os.Rename(path, path+".saved"))
			preserved := make(map[string][]byte)
			for _, path := range []string{
				filepath.Join(req.DestinationDir, req.ID+".md"),
				filepath.Join(req.DestinationDir, req.ID+".notes.json"),
				filepath.Join(txn, "record.json"),
				filepath.Join(txn, "markdown"),
				filepath.Join(txn, "reactions"),
			} {
				data, err := os.ReadFile(path)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				require.NoError(t, err)
				preserved[path] = data
			}
			pending, err := HasPendingTransfer(req.DestinationDir, req.ID)
			if strings.HasPrefix(missing, "capture") {
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NoError(t, err)
				assert.True(t, pending)
			}
			blocked, err := RecoverTransfers(req.DestinationDir)
			require.ErrorIs(t, err, os.ErrNotExist)
			assert.True(t, blocked[req.ID])
			for path, before := range preserved {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after, path)
			}
			require.NoError(t, os.Rename(path+".saved", path))
			blocked, err = RecoverTransfers(req.DestinationDir)
			require.NoError(t, err)
			assert.Empty(t, blocked)
		})
	}
}

func TestTransferRecoveryAggregatesBlockedIDs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		successful bool
		failedLast bool
	}{
		{name: "recordless after failure"},
		{name: "recordless before failure", failedLast: true},
		{name: "success after failure", successful: true},
		{name: "success before failure", successful: true, failedLast: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := transferFixture(t, "", false)
			root := filepath.Join(req.DestinationDir, transferDirectory)
			prefix := "transfer-" + fmt.Sprintf("%x", req.ID) + "-"
			failed, orphan := prefix+"!failed", prefix+"~unrecorded"
			if tc.failedLast {
				failed, orphan = prefix+"~failed", prefix+"!unrecorded"
			}
			require.NoError(t, os.MkdirAll(filepath.Join(root, failed), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, failed, "record.json"), []byte("invalid"), 0o600))
			if tc.successful {
				require.NoError(t, os.Remove(filepath.Join(root, failed, "record.json")))
				_, err := transfer(req, func(step string) error {
					if step == "prepared" {
						return errors.New("stop before capture")
					}
					return nil
				})
				require.ErrorContains(t, err, "stop before capture")
				require.NoError(t, os.WriteFile(filepath.Join(root, failed, "record.json"), []byte("invalid"), 0o600))
			} else {
				require.NoError(t, os.Mkdir(filepath.Join(root, orphan), 0o700))
			}
			blocked, err := RecoverTransfers(req.DestinationDir)
			require.Error(t, err)
			assert.Equal(t, map[string]bool{req.ID: true}, blocked)
			if tc.successful {
				_, err := ticket.ParseFile(filepath.Join(req.DestinationDir, req.ID+".md"))
				require.NoError(t, err)
			}
		})
	}
}

func TestTransferRecoveryIgnoresAbsentRecord(t *testing.T) {
	req := transferFixture(t, "", false)
	txn := filepath.Join(req.DestinationDir, transferDirectory, "transfer-"+fmt.Sprintf("%x", req.ID)+"-unrecorded")
	require.NoError(t, os.MkdirAll(txn, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(txn, "markdown"), []byte("staged"), 0o600))
	pending, err := HasPendingTransfer(req.DestinationDir, req.ID)
	require.NoError(t, err)
	assert.False(t, pending)
	blocked, err := RecoverTransfers(req.DestinationDir)
	require.NoError(t, err)
	assert.Empty(t, blocked)
	data, err := os.ReadFile(filepath.Join(txn, "markdown"))
	require.NoError(t, err)
	assert.Equal(t, "staged", string(data))
}

func TestTransferCaptureRechecksSnapshot(t *testing.T) {
	for _, replacement := range []string{"captured inode", "new source"} {
		t.Run(replacement, func(t *testing.T) {
			req := transferFixture(t, "", true)
			path := filepath.Join(req.SourceDir, req.ID+".md")
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			require.NoError(t, err)
			defer f.Close()
			_, err = transfer(req, func(step string) error {
				if step != "captured-markdown" {
					return nil
				}
				if replacement == "captured inode" {
					_, err := f.WriteAt([]byte("changed"), 0)
					return err
				}
				return os.WriteFile(path, []byte("new source"), 0o644)
			})
			require.Error(t, err)
			_, err = os.Stat(filepath.Join(req.DestinationDir, req.ID+".md"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestTransferAdmissionAndValidation(t *testing.T) {
	t.Run("source scheduling admission is nonblocking", func(t *testing.T) {
		req := transferFixture(t, "", false)
		release, err := ticket.LockOrdinaryRouting(req.SourceDir)
		require.NoError(t, err)
		defer release()
		done := make(chan error, 1)
		go func() { _, err := Transfer(req); done <- err }()
		select {
		case err := <-done:
			require.ErrorIs(t, err, ticket.ErrManaged)
		case <-time.After(3 * time.Second):
			t.Fatal("transfer waited for active source routing")
		}
	})
	t.Run("validation holds both admissions and sees fresh source", func(t *testing.T) {
		req := transferFixture(t, "", false)
		called := false
		req.Validate = func(got *ticket.Ticket) error {
			called = true
			assert.Equal(t, ticket.StatusPaused, got.Status)
			_, err := ticket.LockOrdinaryRouting(req.SourceDir)
			assert.ErrorIs(t, err, ticket.ErrManaged)
			_, err = ticket.LockTransferSource(req.DestinationDir)
			assert.ErrorIs(t, err, ticket.ErrManaged)
			got.SetBody("# Callback must not mutate publication\n")
			return nil
		}
		got, err := Transfer(req)
		require.NoError(t, err)
		assert.True(t, called)
		assert.Equal(t, "Transfer", got.Title())
	})
	t.Run("same physical directory", func(t *testing.T) {
		req := transferFixture(t, "", false)
		alias := filepath.Join(t.TempDir(), "alias")
		require.NoError(t, os.Symlink(req.SourceDir, alias))
		req.DestinationDir = alias
		_, err := Transfer(req)
		require.Error(t, err)
	})
}

func TestTransferConcurrentDestinationCreation(t *testing.T) {
	for range 15 {
		req := transferFixture(t, "", false)
		data := []byte("---\nid: " + req.ID + "\nstatus: open\n---\n# Creator\n")
		var transferErr, createErr error
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() { defer wg.Done(); <-start; _, transferErr = Transfer(req) }()
		go func() {
			defer wg.Done()
			<-start
			createErr = ticket.CreateOffline(filepath.Join(req.DestinationDir, req.ID+".md"), data)
		}()
		close(start)
		wg.Wait()
		require.NotEqual(t, transferErr == nil, createErr == nil, "exactly one publication must win")
		got, err := ticket.ParseFile(filepath.Join(req.DestinationDir, req.ID+".md"))
		require.NoError(t, err)
		if createErr == nil {
			assert.Equal(t, "Creator", got.Title())
			_, err := os.Stat(filepath.Join(req.SourceDir, req.ID+".md"))
			require.NoError(t, err)
		} else {
			assert.Equal(t, "Transfer", got.Title())
		}
	}
}

func TestTransferConcurrentSourceEditAndReactions(t *testing.T) {
	for range 12 {
		req := transferFixture(t, "", true)
		var transferErr, editErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, transferErr = Transfer(req) }()
		go func() {
			defer wg.Done()
			<-start
			editErr = ticket.WithOfflineWrite(req.SourceDir, func() error {
				path := filepath.Join(req.SourceDir, req.ID+".md")
				tk, err := ticket.ParseFile(path)
				if err != nil {
					return err
				}
				tk.SetBody("# Saved editor draft\n")
				data, err := tk.Marshal()
				if err != nil {
					return err
				}
				if err := os.WriteFile(path, data, 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(req.SourceDir, req.ID+".notes.json"), []byte("{\"version\":1,\"reactions\":{\"note\":[]}}\n"), 0o644)
			})
		}()
		close(start)
		wg.Wait()
		require.NotEqual(t, transferErr == nil, editErr == nil)
		if transferErr == nil {
			for _, suffix := range []string{".md", ".notes.json"} {
				_, err := os.Lstat(filepath.Join(req.SourceDir, req.ID+suffix))
				assert.ErrorIs(t, err, os.ErrNotExist)
			}
		} else {
			got, err := ticket.ParseFile(filepath.Join(req.SourceDir, req.ID+".md"))
			require.NoError(t, err)
			assert.Equal(t, "Saved editor draft", got.Title())
			_, err = os.Stat(filepath.Join(req.DestinationDir, req.ID+".md"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		}
	}
}

func TestTransferOppositeDirectionsDoNotDeadlock(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	requests := make([]TransferRequest, 2)
	for i, pair := range [][2]string{{a, b}, {b, a}} {
		id := fmt.Sprintf("tst-%d", i)
		require.NoError(t, os.WriteFile(filepath.Join(pair[0], id+".md"), []byte("---\nid: "+id+"\nstatus: open\n---\n# Ticket\n"), 0o644))
		rev, err := Revision(pair[0], id)
		require.NoError(t, err)
		requests[i] = TransferRequest{SourceDir: pair[0], DestinationDir: pair[1], ID: id, Revision: rev}
	}
	done := make(chan error, 2)
	for _, req := range requests {
		go func() { _, err := Transfer(req); done <- err }()
	}
	for range requests {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("opposite transfers deadlocked")
		}
	}
}

func TestTransferDelayedSourceSaveCannotRecreateTicket(t *testing.T) {
	req := transferFixture(t, "", false)
	repo := NewDiskRepo(req.SourceDir)
	stale, err := repo.Get(req.ID)
	require.NoError(t, err)
	_, err = Transfer(req)
	require.NoError(t, err)
	stale.Ticket.SetBody("# Delayed edit\n")
	require.Error(t, repo.Save(stale))
	_, err = os.Lstat(filepath.Join(req.SourceDir, req.ID+".md"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
