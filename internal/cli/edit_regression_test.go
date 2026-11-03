package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestEditReleasesMutationLockDuringHumanInteraction(t *testing.T) {
	for _, tc := range []string{"unrelated write", "same ticket changed", "authority acquired"} {
		t.Run(tc, func(t *testing.T) {
			dir := t.TempDir()
			for _, id := range []string{"tst-001", "tst-002"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), []byte("---\nid: "+id+"\nstatus: open\n---\n# Ticket\n"), 0o644))
			}
			control := t.TempDir()
			entered, release := filepath.Join(control, "entered"), filepath.Join(control, "release")
			script := filepath.Join(control, "editor")
			require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n: > \"$1\"\nwhile [ ! -e \"$2\" ]; do sleep 0.01; done\nprintf '\\nEditor change\\n' >> \"$3\"\n"), 0o755))
			done := make(chan error, 1)
			finished := false
			go func() { done <- Edit(dir, script+" "+entered+" "+release, "tst-001") }()
			t.Cleanup(func() {
				_ = os.WriteFile(release, nil, 0o644)
				if !finished {
					select {
					case err := <-done:
						t.Logf("editor cleanup result: %v", err)
					case <-time.After(5 * time.Second):
						t.Error("editor did not stop during cleanup")
					}
				}
			})
			require.Eventually(t, func() bool { _, err := os.Stat(entered); return err == nil }, 5*time.Second, 10*time.Millisecond)

			var wantErr string
			switch tc {
			case "unrelated write", "same ticket changed":
				id := "tst-002"
				if tc == "same ticket changed" {
					id = "tst-001"
					wantErr = "ticket changed while editor was open"
				}
				mutated := make(chan error, 1)
				go func() { mutated <- Summary(dir, id, "concurrent update") }()
				select {
				case err := <-mutated:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("human editor blocked an ordinary mutation")
				}
			case "authority acquired":
				board, err := controlstore.AcquireBoard(dir, controlstore.Authority{BoardID: "board", OwnerID: "owner", StateDir: t.TempDir()})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, board.Close()) })
			}
			require.NoError(t, os.WriteFile(release, nil, 0o644))
			select {
			case err := <-done:
				finished = true
				if tc == "authority acquired" {
					require.ErrorIs(t, err, ticket.ErrManaged)
				} else if wantErr != "" {
					require.ErrorContains(t, err, wantErr)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("editor did not finish")
			}
			data, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
			require.NoError(t, err)
			if tc == "unrelated write" {
				require.Contains(t, string(data), "Editor change")
			} else {
				require.NotContains(t, string(data), "Editor change")
			}
		})
	}
}
