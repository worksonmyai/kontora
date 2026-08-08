package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
)

// Agent kinds Kontora knows how to resume. Any other agent always starts a new
// conversation, because its resume flags are unknown.
const (
	agentKindClaude = "claude"
	agentKindPi     = "pi"
)

// resumeRecord names the agent session a stage is running. The daemon writes it
// before the agent starts and deletes it as soon as the agent returns, so a
// record left on disk means the daemon itself died mid-stage and the session can
// be continued.
//
// It stays in logs_dir rather than in ticket frontmatter because everything it
// points at is machine-local: Claude keys its session files by working
// directory, and the pi session directory lives under logs_dir too.
type resumeRecord struct {
	// SessionID is the Claude session UUID. Pi leaves it empty: pi names its
	// session file itself, so the file does not exist yet at write time and is
	// resolved from the stage session directory at resume time instead.
	SessionID string    `json:"session_id"`
	Stage     string    `json:"stage"`
	Agent     string    `json:"agent"`
	Worktree  string    `json:"worktree"`
	Instance  string    `json:"instance"`
	StartedAt time.Time `json:"started_at"`

	// sessionPath is the pi session file resumableRecord picked. It is resolved
	// per resume attempt and never stored.
	sessionPath string
}

// resumeAgentKind reports which resume flags apply to an agent, or "" when
// Kontora cannot resume it. An agent with `resume: false` is never resumable.
func resumeAgentKind(agentCfg config.Agent) string {
	if agentCfg.Resume != nil && !*agentCfg.Resume {
		return ""
	}
	switch {
	case agentCfg.IsClaude():
		return agentKindClaude
	case agentCfg.IsPi():
		return agentKindPi
	}
	return ""
}

// resumeRecordPath mirrors stageLogPath so a stage's record sits next to its
// log. Keying by (ticket, stage) is what keeps a stage from ever attaching to
// another stage's conversation.
func resumeRecordPath(cfg *config.Config, ticketID, stageName string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), ticketID, stageName+".session")
}

// piSessionDir is the per-stage session storage pi writes into. Every stage of a
// ticket materializes its log from this directory, so a shared one would give
// each stage the same session.
func piSessionDir(cfg *config.Config, ticketID, stageName string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), ticketID, "pi-sessions", stageName)
}

func (d *Daemon) writeResumeRecord(p spawnAgentParams, kind, sessionID string) {
	rec := resumeRecord{
		SessionID: sessionID,
		Stage:     p.stageName,
		Agent:     kind,
		Worktree:  p.wtPath,
		Instance:  d.instanceName,
		StartedAt: time.Now(),
	}
	path := resumeRecordPath(p.cfg, p.ticketID, p.stageName)
	data, err := json.Marshal(rec)
	if err != nil {
		p.log.Info("resume record not written", "stage", p.stageName, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.log.Info("resume record not written", "stage", p.stageName, "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		p.log.Info("resume record not written", "stage", p.stageName, "err", err)
	}
}

// readResumeRecord returns the stage's record, or nil when there is none to act
// on. A missing, unreadable, or malformed file is not an error: it only means
// the stage runs fresh.
func (d *Daemon) readResumeRecord(p spawnAgentParams) *resumeRecord {
	path := resumeRecordPath(p.cfg, p.ticketID, p.stageName)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			p.log.Info("resume skipped: record unreadable", "stage", p.stageName, "path", path, "err", err)
		}
		return nil
	}
	var rec resumeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		p.log.Info("resume skipped: record malformed", "stage", p.stageName, "path", path, "err", err)
		return nil
	}
	return &rec
}

func (d *Daemon) removeResumeRecord(cfg *config.Config, ticketID, stageName string) {
	_ = os.Remove(resumeRecordPath(cfg, ticketID, stageName))
}

// resumableRecord returns the record this stage may continue in, or nil to run
// it fresh. Every rejection is logged with its reason and never pauses the
// ticket: resume must not make a stage worse off than it was without it.
func (d *Daemon) resumableRecord(p spawnAgentParams) *resumeRecord {
	kind := resumeAgentKind(p.agentCfg)
	if kind == "" {
		return nil
	}
	rec := d.readResumeRecord(p)
	if rec == nil {
		return nil
	}

	reject := func(reason string, attrs ...any) *resumeRecord {
		p.log.Info("resume skipped: "+reason, append([]any{"stage", p.stageName}, attrs...)...)
		return nil
	}

	// The agent kind is checked as well as the stage, so switching a ticket from
	// Claude to pi cannot hand pi a Claude session ID.
	if rec.Agent != kind {
		return reject("record belongs to another agent", "recorded", rec.Agent, "agent", kind)
	}
	if rec.Stage != p.stageName {
		return reject("record belongs to another stage", "recorded", rec.Stage)
	}
	if rec.Instance != d.instanceName {
		return reject("record belongs to another daemon instance", "recorded", rec.Instance, "instance", d.instanceName)
	}
	// Claude keys its session files by working directory, so a session recorded
	// against a different worktree cannot be found from this one.
	if rec.Worktree != p.wtPath {
		return reject("record belongs to another worktree", "recorded", rec.Worktree, "worktree", p.wtPath)
	}

	switch kind {
	case agentKindClaude:
		if rec.SessionID == "" {
			return reject("record carries no session ID")
		}
		matches, pattern, err := claudeSessionFiles(agentEnv(p.cfg, p.agentCfg), rec.SessionID)
		if err != nil || len(matches) == 0 {
			return reject("session file not found", "pattern", pattern, "err", err)
		}
	case agentKindPi:
		path := piResumeSessionFile(p.cfg, p.ticketID, p.stageName, rec.StartedAt)
		if path == "" {
			return reject("session file not found", "session_dir", piSessionDir(p.cfg, p.ticketID, p.stageName))
		}
		rec.sessionPath = path
	}

	// A live window means a process may still own the session. Attaching a
	// second one to it would corrupt the conversation.
	windows, err := d.windows.list(d.tmuxSession)
	if err != nil {
		return reject("tmux windows could not be listed", "err", err)
	}
	if slices.Contains(windows, p.ticketID) {
		return reject("a tmux window for this ticket is still live")
	}

	return rec
}

// piResumeSessionFile picks the JSONL of the interrupted run from the stage's
// session directory: the newest file pi touched at or after the run started.
// Retries of the same stage leave their own files there, and the start time is
// what tells the interrupted run's file apart from theirs.
func piResumeSessionFile(cfg *config.Config, ticketID, stageName string, startedAt time.Time) string {
	matches, err := filepath.Glob(filepath.Join(piSessionDir(cfg, ticketID, stageName), "*.jsonl"))
	if err != nil {
		return ""
	}
	// A second of slack absorbs filesystem timestamp granularity.
	floor := startedAt.Add(-time.Second)
	eligible := make([]string, 0, len(matches))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || fi.ModTime().Before(floor) {
			continue
		}
		eligible = append(eligible, m)
	}
	if len(eligible) == 0 {
		return ""
	}
	return newestFile(eligible)
}
