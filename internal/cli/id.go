package cli

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
)

const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// GenerateID creates a ticket ID in the form <prefix>-<4 random alphanumeric>.
// The prefix is the one the project configured for repoPath pins, or, failing
// that, one derived from filepath.Base(repoPath) via derivePrefix.
// Retries once on collision with an existing file in the tickets dir.
func GenerateID(cfg *config.Config, repoPath string) (string, error) {
	prefix := cfg.TicketPrefixFor(repoPath)
	if prefix == "" {
		prefix = derivePrefix(filepath.Base(repoPath))
	}
	if prefix == "" {
		return "", fmt.Errorf("cannot derive prefix from path %q", repoPath)
	}

	for range 2 {
		suffix, err := randomSuffix(4)
		if err != nil {
			return "", err
		}
		id := prefix + "-" + suffix
		path := filepath.Join(config.ExpandTilde(cfg.TicketsDir), id+".md")
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

// derivePrefix builds a ticket-ID prefix from a repository directory name: the
// first alphanumeric of each "-" or "_" separated segment, lowercased and
// concatenated, with no length cap. A name that yields fewer than two of them
// falls back to firstAlnum, so a single-segment name like "kontora" gives
// "kon" rather than "k".
//
// This reproduces the scheme the tickets in the store were minted under, so an
// ID still says which repository it belongs to. It diverges in three places,
// all because the prefix becomes a filename component: the output is
// lowercased and restricted to [a-z0-9], a segment starting with punctuation
// contributes its first alphanumeric instead of that punctuation, and the
// fallback counts alphanumerics rather than raw characters.
func derivePrefix(repoName string) string {
	var prefix []byte
	for _, segment := range strings.FieldsFunc(repoName, func(r rune) bool {
		return r == '-' || r == '_'
	}) {
		if c := firstAlnum(segment); c != 0 {
			prefix = append(prefix, c)
		}
	}
	if len(prefix) < 2 {
		return firstAlnumN(repoName, 3)
	}
	return string(prefix)
}

// firstAlnum returns the first ASCII alphanumeric in s, lowercased, or 0.
func firstAlnum(s string) byte {
	if p := firstAlnumN(s, 1); p != "" {
		return p[0]
	}
	return 0
}

// firstAlnumN returns the first n ASCII alphanumerics in s, lowercased. Fewer
// than n, including none at all, is what the string holds.
func firstAlnumN(s string, n int) string {
	var out []byte
	for i := range len(s) {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32) // lowercase
		}
		if len(out) == n {
			break
		}
	}
	return string(out)
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
