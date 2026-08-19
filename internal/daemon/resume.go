package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
)

// Agent kinds Kontora knows how to resume. Any other agent always starts a new
// conversation, because its resume flags are unknown.
// The values are the config package's kind names because they are persisted in
// resume records: reusing them keeps an old record readable.
const (
	agentKindClaude = config.AgentKindClaude
	agentKindPi     = config.AgentKindPi
)

// resumeRecord names the agent session a stage is running. The daemon writes it
// before the stage's agent starts and retires it as soon as that agent returns,
// so a record left on disk means the daemon itself died mid-stage and the session
// can be continued.
//
// It stays in logs_dir rather than in ticket frontmatter because everything it
// points at is machine-local: Claude keys its session files by working
// directory, and the pi session directory lives under logs_dir too. The CLI
// reads the same file, so the shape lives in internal/session.
type resumeRecord = session.Record

// resumeAgentKind reports which resume flags apply to an agent, or "" when
// Kontora cannot resume it. An agent with `resume: false` is never resumable.
func resumeAgentKind(agentCfg config.Agent) string {
	if agentCfg.Resume != nil && !*agentCfg.Resume {
		return ""
	}
	return agentCfg.Kind()
}

// resumeRecordPath mirrors stageLogPath so a stage's record sits next to its
// log. Keying by (ticket, stage) is what keeps a stage from ever attaching to
// another stage's conversation.
func resumeRecordPath(cfg *config.Config, ticketID, stageName string) string {
	return session.RecordPath(expandTilde(cfg.LogsDir), ticketID, stageName)
}

// completedRecordPath names the session a stage left behind when its agent
// returned. It is a different file from resumeRecordPath on purpose: a
// surviving crash-recovery record means the daemon died mid-stage and the stage
// must continue that conversation, while this record exists only so an
// annotation run can keep talking to the agent that did the work. Sharing one
// file would make every ordinary retry resume, which the crash-recovery contract
// forbids.
func completedRecordPath(cfg *config.Config, ticketID, stageName string) string {
	return session.CompletedRecordPath(expandTilde(cfg.LogsDir), ticketID, stageName)
}

// piSessionDir is the per-stage session storage pi writes into. Every stage of a
// ticket materializes its log from this directory, so a shared one would give
// each stage the same session.
func piSessionDir(cfg *config.Config, ticketID, stageName string) string {
	return session.PiDir(expandTilde(cfg.LogsDir), ticketID, stageName)
}

// runSessionRef names the session JSONL a finished run wrote, in the
// machine-independent form ticket history stores: the Claude session UUID
// Kontora minted, or the pi session file's path relative to the ticket's log
// directory. It returns two empty strings for an agent that writes neither.
//
// A Claude run records its ID whether or not the glob finds the file. A run
// that lost its session file still says which session it was, and a reader that
// cannot find it reports a missing file rather than an unrecorded run.
func runSessionRef(cfg *config.Config, ticketID string, params RunnerParams, since time.Time) (kind, ref string) {
	switch {
	case params.SessionID != "":
		return agentKindClaude, params.SessionID
	case params.SessionDir != "":
		path := newestSessionFileSince(params.SessionDir, since)
		if path == "" {
			return "", ""
		}
		// An annotation run is given a session directory of its own, but it is
		// still under the ticket, so one base covers both. A directory that is
		// not, which only a hand-edited logs_dir can produce, records nothing:
		// history must not carry a reference the reader would resolve against
		// the wrong ticket.
		rel, err := filepath.Rel(session.TicketDir(expandTilde(cfg.LogsDir), ticketID), path)
		if err != nil {
			return "", ""
		}
		rel = filepath.ToSlash(rel)
		if !session.SafeRef(agentKindPi, rel) {
			return "", ""
		}
		return agentKindPi, rel
	}
	return "", ""
}

// writeResumeRecord plants the crash-recovery record for a run about to start. A
// failure is logged and otherwise ignored: a run with no record simply cannot be
// continued.
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

// promoteResumeRecord renames the crash-recovery record of a run that returned
// to the completed-session record. Renaming rather than rewriting keeps the two
// exclusive: a record is either an unseen death or a finished conversation, never
// both. It also carries over the start time, which is how a pi session file is
// identified and which the moment of exit is too late to find.
func (d *Daemon) promoteResumeRecord(p spawnAgentParams) {
	from := resumeRecordPath(p.cfg, p.ticketID, p.stageName)
	err := os.Rename(from, completedRecordPath(p.cfg, p.ticketID, p.stageName))
	if err == nil || os.IsNotExist(err) {
		// A missing record is the normal case for an agent Kontora cannot resume.
		return
	}
	// Only the annotation run reads the completed-session record, so losing it
	// costs a session reuse and nothing else.
	p.log.Warn("completed session not recorded", "stage", p.stageName, "err", err)
	if rmErr := os.Remove(from); rmErr != nil && !os.IsNotExist(rmErr) {
		// This one does matter: a crash-recovery record that outlives a run the
		// daemon saw end makes the next ordinary retry resume, and a resumed run is
		// told to finish work that is already finished.
		p.log.Error("resume record left behind, the next retry of this stage will resume it",
			"stage", p.stageName, "path", from, "err", rmErr)
	}
}

// readRecordAt returns the record at path, or nil when there is none to act on.
// A missing, unreadable, or malformed file is not an error: it only means the run
// starts fresh.
func (d *Daemon) readRecordAt(p spawnAgentParams, path string) *resumeRecord {
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

// resumableRecord returns the record this run may continue in, or nil to run it
// fresh. Every rejection is logged with its reason and never pauses the ticket:
// resume must not make a run worse off than it was without it.
//
// An annotation run reads the completed-session record instead, so that it
// continues the conversation the stage's last run ended in. Every guard below
// applies to both records.
func (d *Daemon) resumableRecord(p spawnAgentParams) *resumeRecord {
	kind := resumeAgentKind(p.agentCfg)
	if kind == "" {
		return nil
	}
	path := resumeRecordPath(p.cfg, p.ticketID, p.stageName)
	if p.annotation {
		// A stage run the daemon never saw end still has to recover into its own
		// conversation. Continuing the completed session here would append to its pi
		// session file, making that file the newest in the stage's session
		// directory, which is how piResumeSessionFile tells the interrupted run's
		// file from the rest; the stage would then recover into the wrong
		// conversation.
		if _, err := os.Stat(path); err == nil {
			p.log.Info("resume skipped: the stage has an interrupted run to recover",
				"stage", p.stageName, "path", path)
			return nil
		}
		path = completedRecordPath(p.cfg, p.ticketID, p.stageName)
	}
	rec := d.readRecordAt(p, path)
	if rec == nil {
		if p.annotation {
			// A stage run is missing this record on every first run, so it is not
			// logged. For an annotation run it is why the run did not continue the
			// stage's conversation.
			p.log.Info("resume skipped: no completed session recorded", "stage", p.stageName, "path", path)
		}
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
		matches, pattern, err := claudeSessionFiles(agentEnv(p.cfg, p.agentCfg, d.configPath), rec.SessionID)
		if err != nil || len(matches) == 0 {
			return reject("session file not found", "pattern", pattern, "err", err)
		}
	case agentKindPi:
		path := piResumeSessionFile(p.cfg, p.ticketID, p.stageName, rec.StartedAt)
		if path == "" {
			return reject("session file not found", "session_dir", piSessionDir(p.cfg, p.ticketID, p.stageName))
		}
		rec.SessionPath = path
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
	return newestSessionFileSince(piSessionDir(cfg, ticketID, stageName), startedAt)
}

func newestSessionFileSince(sessionDir string, startedAt time.Time) string {
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
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
