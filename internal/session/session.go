// Package session locates the files one run of a stage leaves behind.
//
// Kontora's own artifacts sit under <logs_dir>/<ticket-id>: the plaintext
// stage log, the per-run activity sidecar, and the resume records. The agent's
// raw session JSONL is elsewhere: pi writes it under the same ticket directory,
// while Claude writes it under its own config directory, keyed by the worktree
// path, so nothing under logs_dir names it.
//
// The package takes primitives. Every path argument is an already expanded
// directory, because the daemon and the CLI expand `~` at different points.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The agent runtimes whose session files Kontora can locate. The values equal
// config.AgentKindClaude and config.AgentKindPi, which TestKindsMatchConfig
// pins: they are written into ticket history, so a rename here would orphan
// every row already on disk.
const (
	KindClaude = "claude"
	KindPi     = "pi"
)

// PiSessionsDir is the directory under a ticket's log directory that holds one
// session directory per stage. It is the first element of every pi session
// reference.
const PiSessionsDir = "pi-sessions"

// TicketDir is where a ticket's logs, sidecars and pi sessions live.
func TicketDir(logsDir, ticketID string) string {
	return filepath.Join(logsDir, ticketID)
}

// LogPath is the plaintext capture of a stage's agent output. It holds only the
// newest run: every run of the stage writes to the same file.
func LogPath(logsDir, ticketID, stage string) string {
	return filepath.Join(logsDir, ticketID, stage+".log")
}

// EventsPath is the structured activity sidecar for one run of a stage. It sits
// beside the stage log; the .json suffix keeps it out of every existing
// log-directory scanner, all of which filter on .log.
func EventsPath(logsDir, ticketID, stage string, run int) string {
	return filepath.Join(logsDir, ticketID, fmt.Sprintf("%s.%d.events.json", stage, run))
}

// PiDir is the per-stage session storage pi writes into. Every stage of a
// ticket materializes its log from its own directory, so a shared one would
// give each stage the same session.
func PiDir(logsDir, ticketID, stage string) string {
	return filepath.Join(logsDir, ticketID, PiSessionsDir, stage)
}

// RecordPath names the crash-recovery record of a running stage, and
// CompletedRecordPath the record its agent left behind on return.
func RecordPath(logsDir, ticketID, stage string) string {
	return filepath.Join(logsDir, ticketID, stage+".session")
}

func CompletedRecordPath(logsDir, ticketID, stage string) string {
	return filepath.Join(logsDir, ticketID, stage+".completed-session")
}

// PiFiles returns the session JSONL files in a stage session directory, oldest
// first by modification time. A missing directory yields no files and no error:
// a stage that never ran with pi simply has none.
func PiFiles(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	type entry struct {
		path string
		mod  time.Time
	}
	entries := make([]entry, 0, len(matches))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		entries = append(entries, entry{path: m, mod: fi.ModTime()})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].mod.Equal(entries[j].mod) {
			return entries[i].path < entries[j].path
		}
		return entries[i].mod.Before(entries[j].mod)
	})
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.path)
	}
	return paths, nil
}

// ClaudeFiles globs the Claude config dir for the JSONL files of the given
// session. The pattern is returned so a caller that found nothing can say what
// it looked for.
func ClaudeFiles(configDir, sessionID string) (matches []string, pattern string, err error) {
	pattern = filepath.Join(configDir, "projects", "*", sessionID+".jsonl")
	matches, err = filepath.Glob(pattern)
	return matches, pattern, err
}

// ClaudeConfigDir reports where Claude keeps its projects directory, given the
// environment Kontora passes an agent. The returned path still holds a literal
// `~` when the configured value does; callers expand it.
func ClaudeConfigDir(env map[string]string) string {
	if v, ok := env["CLAUDE_CONFIG_DIR"]; ok && v != "" {
		return v
	}
	return "~/.claude"
}

// SafeStage reports whether a stage name can key a file. History is ticket
// frontmatter, which anyone can edit, and filepath.Join would clean a planted
// "../" into a path outside the ticket's log directory.
func SafeStage(stage string) bool {
	if stage == "" || stage == "." || stage == ".." {
		return false
	}
	if strings.ContainsAny(stage, `/\`) {
		return false
	}
	return stage == filepath.Base(stage)
}

// SafeRef reports whether a session reference can be resolved to a path. A
// Claude reference is a session UUID and names a single file, so it is held to
// the same rule as a stage name, plus a ban on glob metacharacters: the
// reference is globbed, and "*" would otherwise match an unrelated project's
// session. A pi reference is a path relative to the ticket's log directory and
// so contains separators by design; it is required to stay inside that
// directory.
func SafeRef(kind, ref string) bool {
	switch kind {
	case KindClaude:
		return SafeStage(ref) && !strings.ContainsAny(ref, `*?[]`)
	case KindPi:
		if ref == "" || strings.ContainsAny(ref, `\`) {
			return false
		}
		return filepath.IsLocal(ref)
	}
	return false
}

// Record is the on-disk <stage>.session JSON: the agent session a stage run
// opened. The JSON tags are persisted and must not change.
type Record struct {
	// SessionID is the Claude session UUID. Pi leaves it empty: pi names its
	// session file itself, so the file does not exist yet at write time.
	SessionID string    `json:"session_id"`
	Stage     string    `json:"stage"`
	Agent     string    `json:"agent"`
	Worktree  string    `json:"worktree"`
	Instance  string    `json:"instance"`
	StartedAt time.Time `json:"started_at"`

	// SessionPath is the pi session file a resume attempt picked. It is
	// resolved per attempt and never stored.
	SessionPath string `json:"-"`
}

// ReadRecord parses the record at path. A missing, unreadable or malformed file
// yields a nil record and an error the caller is free to treat as "no record".
func ReadRecord(path string) (*Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parsing session record %s: %w", path, err)
	}
	return &rec, nil
}
