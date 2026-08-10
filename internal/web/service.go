package web

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

var (
	ErrTicketNotFound      = errors.New("ticket not found")
	ErrInvalidState        = errors.New("invalid state transition")
	ErrLogNotFound         = errors.New("log not found")
	ErrUnknownAgent        = errors.New("unknown agent")
	ErrUnknownPipeline     = errors.New("unknown pipeline")
	ErrDeleteRejected      = errors.New("delete rejected")
	ErrInvalidConfig       = errors.New("invalid config")
	ErrConfigPathNotSet    = errors.New("config path not configured")
	ErrPlannotatorInFlight = errors.New("plannotator review already in progress")
	ErrPlannotatorBinary   = errors.New("plannotator not installed: https://plannotator.ai")
)

// TicketService defines the contract between the web layer and the daemon.
// The daemon implements this interface; the web package tests use mocks.
type TicketService interface {
	ListTickets() []TicketInfo
	RunningAgents() int
	GetTicket(id string) (TicketInfo, error)
	CreateTicket(req CreateTicketRequest) (TicketInfo, error)
	GetConfig() ConfigInfo
	DeleteTicket(id string) error
	PauseTicket(id string) error
	RetryTicket(id string) error
	RunTicket(id string) error
	SkipStage(id string) error
	SetStage(id string, stage string) error
	MoveTicket(id string, newStatus string) error
	AddNote(id string, text string) error
	SetSummary(id string, text string) error
	InitTicket(id string, req InitTicketRequest) error
	UpdateTicket(id string, req UpdateTicketRequest) error
	UploadTicket(content []byte) (TicketInfo, error)
	GetLogs(id string, stage string) (string, error)
	GetActivity(q ActivityQuery) (ActivityInfo, error)
	GetChanges(id string) (ChangesInfo, error)
	GetRawConfig() (string, error)
	PutRawConfig(content string) error
	Subscribe() (ch <-chan TicketEvent, unsubscribe func())
	HasTerminalSession(id string) bool
	StartPlannotatorReview(id string) error
}

// PlannotatorOutcome enumerates the results of a plannotator review run.
// The UI uses this to render the finished-state toast.
const (
	PlannotatorOutcomeApproved  = "approved"
	PlannotatorOutcomeRework    = "rework"
	PlannotatorOutcomeCancelled = "cancelled"
	PlannotatorOutcomeError     = "error"
)

type CreateTicketRequest struct {
	Title    string `json:"title"`
	Path     string `json:"path"`
	Pipeline string `json:"pipeline,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Status   string `json:"status,omitempty"`
	Body     string `json:"body,omitempty"`
	Branch   string `json:"branch,omitempty"`
	// BaseBranch names the branch the work branch starts from. Empty means the
	// repository's default branch.
	BaseBranch string `json:"base_branch,omitempty"`
}

type InitTicketRequest struct {
	Pipeline string `json:"pipeline"`
	Path     string `json:"path"`
	Agent    string `json:"agent,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

type UpdateTicketRequest struct {
	Body       *string `json:"body,omitempty"`
	Pipeline   *string `json:"pipeline,omitempty"`
	Path       *string `json:"path,omitempty"`
	Agent      *string `json:"agent,omitempty"`
	Branch     *string `json:"branch,omitempty"`
	BaseBranch *string `json:"base_branch,omitempty"`
}

type PipelineInfo struct {
	Name   string   `json:"name"`
	Stages []string `json:"stages"`
	// MaxRetries is the per-stage retry allowance, positionally aligned with
	// Stages. It is the denominator of the "attempt 2 / 2" meter.
	MaxRetries   []int  `json:"max_retries,omitempty"`
	DefaultAgent string `json:"default_agent,omitempty"`
}

// ActivityQuery addresses one stage run. After is the number of events the
// caller already holds, and only a live response honours it. IfNoneMatch is the
// validator the caller last saw.
type ActivityQuery struct {
	ID          string
	Stage       string
	Run         int
	After       int
	IfNoneMatch string
}

// ActivityInfo is one stage run's transcript. Source is "events" when the tape
// was read from the run's structured record and "log" when the response fell
// back to the shared plaintext log.
//
// Live and Stale are orthogonal. Live means the run is in flight, so the tape
// will grow. Stale means a plaintext fallback whose bytes may describe another
// run of the same stage; a live run reading that shared file is both.
type ActivityInfo struct {
	Source  string       `json:"source"`
	Stage   string       `json:"stage"`
	Run     int          `json:"run"`
	Stale   bool         `json:"stale,omitempty"`
	Live    bool         `json:"live,omitempty"`
	Offset  int          `json:"offset,omitempty"` // index of the first event in Tape.Events
	Content string       `json:"content,omitempty"`
	Tape    *logfmt.Tape `json:"tape,omitempty"`

	// ETag and NotModified drive HTTP revalidation and are not part of the
	// payload the client parses.
	ETag        string `json:"-"`
	NotModified bool   `json:"-"`
}

// ProjectInfo describes one configured project. Path is the value as written in
// the config file; ResolvedPath is the same path tilde-expanded and cleaned, so
// a browser can match it against an absolute path a user typed without knowing
// the daemon host's home directory.
type ProjectInfo struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	ResolvedPath string `json:"resolved_path"`
	Pipeline     string `json:"pipeline"`
	Agent        string `json:"agent"`
	BranchPrefix string `json:"branch_prefix,omitempty"`
}

type ConfigInfo struct {
	Pipelines      []string       `json:"pipelines"`
	PipelineInfos  []PipelineInfo `json:"pipeline_infos"`
	Agents         []string       `json:"agents"`
	Projects       []ProjectInfo  `json:"projects,omitempty"`
	DefaultAgent   string         `json:"default_agent,omitempty"`
	BranchPrefix   string         `json:"branch_prefix"`
	CustomStatuses []string       `json:"custom_statuses,omitempty"`
}

type TicketInfo struct {
	ID            string        `json:"id"`
	Title         string        `json:"title"`
	Status        string        `json:"status"`
	Kontora       bool          `json:"kontora"`
	Stage         string        `json:"stage"`
	Pipeline      string        `json:"pipeline"`
	Path          string        `json:"path"`
	Agent         string        `json:"agent"`
	AgentOverride bool          `json:"agent_override,omitempty"`
	Attempt       int           `json:"attempt"`
	CreatedAt     *time.Time    `json:"created_at,omitempty"`
	StartedAt     *time.Time    `json:"started_at,omitempty"`
	UpdatedAt     *time.Time    `json:"updated_at,omitempty"`
	Branch        string        `json:"branch,omitempty"`
	BaseBranch    string        `json:"base_branch,omitempty"`
	AutoBranch    string        `json:"auto_branch,omitempty"` // what the daemon would name the branch, set only while Branch is empty
	ClaimedBy     string        `json:"claimed_by,omitempty"`
	Stages        []string      `json:"stages,omitempty"`
	History       []HistoryInfo `json:"history,omitempty"`
	Body          string        `json:"body,omitempty"`
	LastError     string        `json:"last_error,omitempty"`
	LastLog       string        `json:"last_log,omitempty"`
	Summary       string        `json:"summary,omitempty"`
	Notes         []NoteInfo    `json:"notes,omitempty"`
}

// NoteInfo is one entry from the ticket body's "## Notes" section. At is the
// bold line that opened the note: a UTC RFC3339 timestamp for notes written
// through AddNote, and whatever the author typed for hand-written ones.
type NoteInfo struct {
	At   string `json:"at,omitempty"`
	Text string `json:"text"`
}

type HistoryInfo struct {
	Stage       string     `json:"stage"`
	Agent       string     `json:"agent,omitempty"`
	ExitCode    int        `json:"exit_code"`
	Run         int        `json:"run"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Summary     string     `json:"summary,omitempty"`
}

// ChangesInfo lists the commits and changed files on a ticket's branch
// relative to the repository's default branch.
type ChangesInfo struct {
	Base    string           `json:"base"`
	Branch  string           `json:"branch"`
	Commits []CommitInfo     `json:"commits"`
	Files   []FileChangeInfo `json:"files"`
	// Remote is the repository's origin as a browsable https URL, with no
	// trailing slash or .git suffix: the web UI turns a "#511" in a summary
	// into a link under it. Empty when the project has no origin, when the
	// origin is not an https or ssh git URL, or when the ticket names no
	// repository.
	Remote string `json:"remote,omitempty"`
}

type CommitInfo struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

type FileChangeInfo struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

type TicketEvent struct {
	Type   string     `json:"type"`
	Ticket TicketInfo `json:"ticket"`
	// Outcome is only populated for plannotator_finished events.
	Outcome string `json:"outcome,omitempty"`
	// Message carries additional context (e.g. spawn failure detail) on
	// plannotator events. Optional.
	Message string `json:"message,omitempty"`
}

// noteByline matches the bold line AddNote writes above each note body.
var noteByline = regexp.MustCompile(`^\*\*(.+)\*\*$`)

// ParseNotes reads the "## Notes" section of a ticket body. AddNote writes
// each entry as a bold timestamp line, a blank line, and the text, so a bold
// line opens a note and everything up to the next one is its body. The section
// ends at the next heading. Body content outside the section is not touched.
func ParseNotes(body string) []NoteInfo {
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Notes" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil
	}

	var notes []NoteInfo
	cur := NoteInfo{}
	var buf []string
	flush := func() {
		if text := strings.TrimSpace(strings.Join(buf, "\n")); text != "" {
			cur.Text = text
			notes = append(notes, cur)
		}
		cur, buf = NoteInfo{}, nil
	}

	for _, line := range lines[start:] {
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") {
			break
		}
		if m := noteByline.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			cur.At = m[1]
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return notes
}

// TicketInfoFromView converts an app.View to a TicketInfo.
func TicketInfoFromView(v app.View) TicketInfo {
	info := TicketInfo{
		ID:            v.ID,
		Title:         v.Title,
		Status:        v.Status,
		Kontora:       v.Kontora,
		Stage:         v.Stage,
		Pipeline:      v.Pipeline,
		Path:          v.Path,
		Agent:         v.Agent,
		AgentOverride: v.AgentOverride,
		Attempt:       v.Attempt,
		CreatedAt:     v.CreatedAt,
		StartedAt:     v.StartedAt,
		Branch:        v.Branch,
		BaseBranch:    v.BaseBranch,
		ClaimedBy:     v.ClaimedBy,
		Stages:        v.Stages,
		Body:          v.Body,
		LastError:     v.LastError,
		LastLog:       v.LastLog,
		Summary:       v.Summary,
		Notes:         ParseNotes(v.Body),
	}
	if len(v.History) > 0 {
		info.History = make([]HistoryInfo, len(v.History))
		for i, h := range v.History {
			info.History[i] = HistoryInfo{
				Stage:       h.Stage,
				Agent:       h.Agent,
				ExitCode:    h.ExitCode,
				Run:         h.Run,
				StartedAt:   h.StartedAt,
				CompletedAt: h.CompletedAt,
				Summary:     h.Summary,
			}
		}
	}
	return info
}
