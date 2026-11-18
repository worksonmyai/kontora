package assistant

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

// SessionPathStrict reports inaccessible directories and ambiguous transcripts.
// An empty result means discovery succeeded without finding a session file.
func SessionPathStrict(kind, sessionID, claudeConfigDir, piSessionDir string) (string, error) {
	var matches []string
	switch kind {
	case config.AgentKindClaude:
		if sessionID == "" {
			return "", nil
		}
		if !session.SafeRef(session.KindClaude, sessionID) {
			return "", fmt.Errorf("invalid claude session reference")
		}
		root := filepath.Join(config.ExpandTilde(claudeConfigDir), "projects")
		dirs, err := os.ReadDir(root)
		if err != nil {
			return "", err
		}
		for _, dir := range dirs {
			path := filepath.Join(root, dir.Name())
			st, err := os.Stat(path)
			if err != nil {
				return "", err
			}
			if !st.IsDir() {
				continue
			}
			files, err := strictSessionFiles(path, sessionID+".jsonl")
			if err != nil {
				return "", err
			}
			matches = append(matches, files...)
		}
	case config.AgentKindPi:
		var err error
		matches, err = strictSessionFiles(piSessionDir, "")
		if err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported assistant agent kind %q", kind)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous assistant transcript: found %d session files", len(matches))
	}
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

func strictSessionFiles(dir, name string) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("assistant session directory is empty")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if (name != "" && entry.Name() != name) || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("assistant transcript is not a regular file: %s", path)
		}
		paths = append(paths, path)
	}
	return paths, nil
}
