package store

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingTransferRejectsUnsafeRoot(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, transferDirectory)
			if kind == "file" {
				require.NoError(t, os.WriteFile(root, []byte("unsafe"), 0o600))
			} else {
				require.NoError(t, os.Symlink(t.TempDir(), root))
			}
			pending, err := HasPendingTransfer(dir, "held")
			require.Error(t, err)
			assert.False(t, pending)
		})
	}
}

func TestPendingTransferAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		record  bool
		id      string
		refused bool
		token   string
	}{
		{name: "unrecorded staging", id: "held"},
		{name: "corrupt record", record: true, id: "held", refused: true},
		{name: "unrelated corrupt transfer", record: true, id: "other"},
		{name: "unidentified corrupt transfer", record: true, id: "other", token: "unidentified", refused: true},
		{name: "unidentified staging", id: "other", token: "unidentified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			token := tc.token
			if token == "" {
				token = "transfer-" + hex.EncodeToString([]byte("held")) + "-test"
			}
			txn := filepath.Join(dir, transferDirectory, token)
			require.NoError(t, os.MkdirAll(txn, 0o700))
			if tc.record {
				require.NoError(t, os.WriteFile(filepath.Join(txn, "record.json"), []byte("invalid"), 0o600))
			}
			pending, err := HasPendingTransfer(dir, tc.id)
			assert.False(t, pending)
			if tc.refused {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
