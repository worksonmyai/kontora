package cli

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/config"
)

const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// GenerateID creates a ticket ID in the form <prefix>-<4 random alphanumeric>.
// The prefix is derived from filepath.Base(repoPath) via fallbackPrefix.
// Retries once on collision with an existing file in tasksDir.
func GenerateID(tasksDir, repoPath string) (string, error) {
	prefix := fallbackPrefix(filepath.Base(repoPath))
	if prefix == "" {
		return "", fmt.Errorf("cannot derive prefix from path %q", repoPath)
	}

	for range 2 {
		suffix, err := randomSuffix(4)
		if err != nil {
			return "", err
		}
		id := prefix + "-" + suffix
		path := filepath.Join(config.ExpandTilde(tasksDir), id+".md")
		_, statErr := os.Stat(path)
		if os.IsNotExist(statErr) {
			return id, nil
		}
		if statErr != nil {
			return "", fmt.Errorf("checking id %s: %w", id, statErr)
		}
	}
	return "", fmt.Errorf("id collision after retry")
}

func fallbackPrefix(repoName string) string {
	var prefix []byte
	for i := range len(repoName) {
		c := repoName[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			prefix = append(prefix, c)
		} else if c >= 'A' && c <= 'Z' {
			prefix = append(prefix, c+32) // lowercase
		}
		if len(prefix) == 3 {
			break
		}
	}
	return string(prefix)
}

func randomSuffix(n int) (string, error) {
	b := make([]byte, n)
	for i := range n {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(idChars))))
		if err != nil {
			return "", err
		}
		b[i] = idChars[idx.Int64()]
	}
	return string(b), nil
}
