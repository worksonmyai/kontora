package assistant

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
)

// SessionPath locates the JSONL a thread's agent is writing its transcript to.
// It returns "" when the agent has not written anything yet, which is what a
// poll sees between starting the first turn and the agent's first token.
//
// The two kinds keep their transcripts in different places. Claude writes into
// its own config directory, under a folder keyed by the working directory, and
// names the file after the session UUID. pi writes into the session directory
// Kontora hands it.
func SessionPath(kind, sessionID, claudeConfigDir, piSessionDir string) string {
	switch kind {
	case config.AgentKindClaude:
		if sessionID == "" {
			return ""
		}
		matches, _, err := session.ClaudeFiles(config.ExpandTilde(claudeConfigDir), sessionID)
		if err != nil || len(matches) == 0 {
			return ""
		}
		return matches[0]
	case config.AgentKindPi:
		return newestJSONL(piSessionDir)
	}
	return ""
}

// newestJSONL is the most recently modified session file in dir, or "" when
// there is none. pi names its files itself, so the directory, not a path
// Kontora chose, is what identifies the thread's transcript.
func newestJSONL(dir string) string {
	if dir == "" {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	type entry struct {
		path string
		mod  int64
	}
	entries := make([]entry, 0, len(matches))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		entries = append(entries, entry{path: m, mod: fi.ModTime().UnixNano()})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].mod == entries[j].mod {
			return entries[i].path < entries[j].path
		}
		return entries[i].mod > entries[j].mod
	})
	return entries[0].path
}
