package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDelete_RemovesFile(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: open
---
# Ticket
`)

	require.NoError(t, Delete(dir, "tst-001"))

	_, err := os.Stat(filepath.Join(dir, "tst-001.md"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDelete_PrefixMatch(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001.md", `---
id: tst-001
status: open
---
# Ticket
`)

	require.NoError(t, Delete(dir, "tst"))

	_, err := os.Stat(filepath.Join(dir, "tst-001.md"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDelete_NotFound(t *testing.T) {
	dir := t.TempDir()
	require.Error(t, Delete(dir, "nonexistent"))
}
