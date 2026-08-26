package web

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/stats"
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
	ErrPlannotatorInFlight = errors.New("plannotator is already open for this ticket")
	ErrPlannotatorBinary   = errors.New("plannotator not installed: https://plannotator.ai")

	// ErrAssistantDisabled means no assistant agent is configured. It is the
	// state behind the pane's configure hint, so it answers 501 rather than an
	// error the UI would show as a failure.
	ErrAssistantDisabled = errors.New("assistant is not configured")
	ErrAssistantNotFound = errors.New("assistant thread not found")
	ErrAssistantBusy     = errors.New("this thread is already running a turn")
	// ErrAssistantAtCapacity is the global turn cap, which is not the same
	// refusal as a thread that is already busy: this chat can run, just not yet.
	ErrAssistantAtCapacity = errors.New("too many assistant turns are running; try again in a moment")
	// ErrAssistantStale is a thread whose agent has been repointed or removed
	// under it. Its session cannot resume on another CLI, so it needs a new chat.
	ErrAssistantStale        = errors.New("this chat's agent has changed")
	ErrAssistantGateNotFound = errors.New("no tool call is waiting on that id")
	// ErrAssistantGateDenied is a gate call whose nonce does not match the one
	// the thread's current turn was started with.
	ErrAssistantGateDenied = errors.New("assistant gate: not this turn's agent")
)

// TicketService defines the contract between the web layer and the daemon.
// The daemon implements this interface; the web package tests use mocks.
type TicketService interface {
	ListTickets(opts ListTicketsOptions) []TicketInfo
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
	ListArchivedTickets() []ArchivedTicketInfo
	ArchiveTicket(id string, note string) error
	RestoreTicket(id string) error
	AddNote(id string, text string) error
	SetSummary(id string, text string) error
	InitTicket(id string, req InitTicketRequest) error
	AddDependency(id string, dependencyID string) error
	RemoveDependency(id string, dependencyID string) error
	LinkTickets(id string, relatedIDs []string) error
	UnlinkTickets(id string, relatedIDs []string) error
	UpdateTicket(id string, req UpdateTicketRequest) error
	UploadTicket(content []byte) (TicketInfo, error)
	GetLogs(id string, stage string) (string, error)
	GetActivity(q ActivityQuery) (ActivityInfo, error)
	GetStats(q StatsQuery) (StatsInfo, error)
	GetChanges(id string) (ChangesInfo, error)
	GetChain(id string) (ChainInfo, error)
	GetRawConfig() (string, error)
	PutRawConfig(content string) error
	Subscribe() (ch <-chan TicketEvent, unsubscribe func())
	HasTerminalSession(id string) bool
	StartPlannotatorReview(id string) error
	StartPlannotatorAnnotate(id string) error
	AssistantConfig() AssistantConfigInfo
	ListAssistantThreads() ([]AssistantThreadInfo, error)
	CreateAssistantThread(req CreateAssistantThreadRequest) (AssistantThreadInfo, error)
	GetAssistantThread(id string) (AssistantThreadInfo, error)
	DeleteAssistantThread(id string) error
	AssistantActivity(q AssistantActivityQuery) (AssistantActivityInfo, error)
	AssistantPartial(id string) (AssistantPartialInfo, error)
	PostAssistantMessage(id string, req AssistantMessageRequest) error
	StopAssistantTurn(id string) error
	ResolveAssistantGate(gateID string, approve bool) error
	AskAssistantGate(req AssistantGateAskRequest) (AssistantGateAskResponse, error)
}

// AssistantConfigInfo tells the pane whether it can be used. Enabled false plus
// Hint is what the configure state renders instead of a composer.
type AssistantConfigInfo struct {
	Enabled  bool   `json:"enabled"`
	Agent    string `json:"agent,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Model    string `json:"model,omitempty"`
	Autonomy string `json:"autonomy,omitempty"`
	Workdir  string `json:"workdir,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

// AssistantThreadInfo is one chat as the history list and the pane header read
// it. Messages is filled only by the single-thread reads.
type AssistantThreadInfo struct {
	ID        string             `json:"id"`
	Title     string             `json:"title"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	Agent     string             `json:"agent,omitempty"`
	Kind      string             `json:"kind,omitempty"`
	Model     string             `json:"model,omitempty"`
	Autonomy  string             `json:"autonomy"`
	Turns     int                `json:"turns"`
	Writes    int                `json:"writes"`
	Running   bool               `json:"running"`
	Messages  []AssistantMessage `json:"messages,omitempty"`
}

// AssistantMessage is one user turn. The agent's side of the conversation comes
// from the tape, not from here.
type AssistantMessage struct {
	N     int       `json:"n"`
	Text  string    `json:"text"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

type CreateAssistantThreadRequest struct {
	Autonomy string `json:"autonomy,omitempty"`
}

// AssistantMessageRequest posts one message. A non-empty Autonomy switches the
// thread's mode before the turn runs, which is how the pane's mode selector
// takes effect on the very next message. Context is what the user is looking
// at, one fact per line. It is rendered into that turn's system prompt rather
// than prefixed to the message, so the pane's own bubble stays what the user
// typed.
type AssistantMessageRequest struct {
	Text     string `json:"text"`
	Autonomy string `json:"autonomy,omitempty"`
	Context  string `json:"context,omitempty"`
}

// AssistantActivityQuery is one poll of a thread. After is the number of events
// the caller already holds.
type AssistantActivityQuery struct {
	ID          string
	After       int
	IfNoneMatch string
}

// AssistantPartialInfo is the cheap read behind the stream. It costs no file:
// the stream reads it ten times a second.
type AssistantPartialInfo struct {
	Running bool
	Gen     int
	Text    string
	Tool    string
	// Sealed stops the caret without ending the connection.
	Sealed bool
}

// AssistantActivityInfo is one poll's answer: the transcript slice, the user
// messages, and any write waiting on the person. They ride together so one
// request drives the tool rows, the prose and the proposal card.
type AssistantActivityInfo struct {
	Running  bool               `json:"running"`
	Autonomy string             `json:"autonomy,omitempty"`
	Offset   int                `json:"offset,omitempty"`
	Tape     *logfmt.Tape       `json:"tape,omitempty"`
	Gate     *assistant.Pending `json:"gate,omitempty"`
	Messages []AssistantMessage `json:"messages,omitempty"`

	// Partial is the message being written, empty once the session file carries
	// it. PartialGen is bumped by a new block, so a reader replaces rather than
	// appends; PartialTool names the call whose arguments are still coming.
	Partial     string `json:"partial,omitempty"`
	PartialGen  int    `json:"partial_gen,omitempty"`
	PartialTool string `json:"partial_tool,omitempty"`

	// ETag and NotModified drive HTTP revalidation and are not part of the
	// payload the client parses.
	ETag        string `json:"-"`
	NotModified bool   `json:"-"`
}

// AssistantGateAskRequest is the agent side of the gate: the claude PreToolUse
// hook and the pi tool_call handler both send it. Nonce is the secret the
// thread's current turn was started with.
type AssistantGateAskRequest struct {
	Thread string         `json:"thread"`
	Nonce  string         `json:"nonce"`
	Tool   string         `json:"tool"`
	Input  map[string]any `json:"input"`
}

// AssistantGateAskResponse is what the agent is told. Reason is stated on a
// refusal so the agent reports it rather than retrying the call.
type AssistantGateAskResponse struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// AssistantGateRequest is the person's answer at the pane.
type AssistantGateRequest struct {
	Decision string `json:"decision"`
}

// PlannotatorOutcome enumerates the results of a plannotator run.
// The UI uses this to render the finished-state toast.
const (
	PlannotatorOutcomeApproved  = "approved"
	PlannotatorOutcomeRework    = "rework"
	PlannotatorOutcomeCancelled = "cancelled"
	PlannotatorOutcomeError     = "error"
	// PlannotatorOutcomeAnnotated reports submitted ticket annotations: the
	// ticket is parked for a run that rewrites it.
	PlannotatorOutcomeAnnotated = "annotated"
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

// ListTicketsOptions narrows or widens a list response. IncludeHidden adds the
// tickets whose status has no board column: archived, legacy closed, and any
// foreign status. The board never asks for them; a CLI listing that filters or
// resolves an id needs the complete set.
type ListTicketsOptions struct {
	IncludeHidden bool
}

// RelationRequest is the body of the relation endpoints. A dependency call
// names exactly one related ticket; a link call may name several.
type RelationRequest struct {
	Related []string `json:"related"`
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

// StatsQuery bounds one Stats page request. Days is already resolved from the
// range chip the client sent; an empty Project or Pipeline means no filter, and
// a name no ticket carries simply matches nothing.
type StatsQuery struct {
	Days     int
	Project  string
	Pipeline string
}

// StatsInfo is the pre-aggregated Stats payload. It is the aggregator's result
// unchanged: the client does no math over it, so there is nothing for the web
// layer to reshape.
type StatsInfo = stats.Result

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
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Kontora  bool   `json:"kontora"`
	Stage    string `json:"stage"`
	Pipeline string `json:"pipeline"`
	Path     string `json:"path"`
	// Project is the configured project whose path is the ticket's, empty when
	// no project matches. It is derived from the config, not stored.
	Project       string     `json:"project,omitempty"`
	Agent         string     `json:"agent"`
	AgentOverride bool       `json:"agent_override,omitempty"`
	Attempt       int        `json:"attempt"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	// FinishedAt is when the last stage run completed. It is in the board
	// payload as well as the detail one, because the review column orders by
	// it. UpdatedAt is the file mtime and moves on every write to the markdown.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	Branch     string     `json:"branch,omitempty"`
	BaseBranch string     `json:"base_branch,omitempty"`
	AutoBranch string     `json:"auto_branch,omitempty"` // what the daemon would name the branch, set only while Branch is empty
	ClaimedBy  string     `json:"claimed_by,omitempty"`
	// CanAnnotate reports whether the ticket can be opened in Plannotator's
	// annotation UI right now: it is initialized, its status allows an edit, and
	// no annotation run is already pending. Like AutoBranch it is a read-only
	// projection, computed here so the dashboard does not keep its own copy of
	// rules only the daemon can enforce.
	CanAnnotate bool `json:"can_annotate,omitempty"`

	// The running agent is blocked on an interactive question and the stage
	// will sit there until a human answers through the read-write terminal or
	// the stage times out. Like CanAnnotate these are daemon state, not ticket
	// fields: they exist only while the agent process does.
	WaitingForInput bool       `json:"waiting_for_input,omitempty"`
	WaitingSince    *time.Time `json:"waiting_since,omitempty"`
	WaitingTool     string     `json:"waiting_tool,omitempty"`
	WaitingQuestion string     `json:"waiting_question,omitempty"`

	Stages    []string      `json:"stages,omitempty"`
	History   []HistoryInfo `json:"history,omitempty"`
	Body      string        `json:"body,omitempty"`
	LastError string        `json:"last_error,omitempty"`
	LastLog   string        `json:"last_log,omitempty"`
	Summary   string        `json:"summary,omitempty"`
	// FinalSummary is the ticket-level outcome. Like the other detail fields it
	// is absent from board list payloads, which never render it.
	FinalSummary string     `json:"final_summary,omitempty"`
	Notes        []NoteInfo `json:"notes,omitempty"`

	// The archive stamp, present only while Status is archived. ArchivedFrom is
	// the closed status the ticket held before it was archived, and is where a
	// restore puts it back.
	ArchivedFrom string     `json:"archived_from,omitempty"`
	ArchivedAt   *time.Time `json:"archived_at,omitempty"`
	ArchivedBy   string     `json:"archived_by,omitempty"`
	ArchiveNote  string     `json:"archive_note,omitempty"`

	// Ready reports that no dependency holds the ticket back, and Blockers names
	// the ones that do. Both are derived on read, not stored, and only mean
	// something for a kontora ticket in todo.
	Ready    bool     `json:"ready,omitempty"`
	Blockers []string `json:"blockers,omitempty"`

	// Relations from the frontmatter: Deps are the tickets this one waits on,
	// Links the symmetric related set, Parent the epic above it. Blocks is the
	// reverse of Deps and is stored nowhere, so it is derived and only present
	// in the detail projection.
	Deps   []TicketRef `json:"deps,omitempty"`
	Links  []TicketRef `json:"links,omitempty"`
	Parent *TicketRef  `json:"parent,omitempty"`
	Blocks []TicketRef `json:"blocks,omitempty"`
	// Children is the reverse of Parent: the tickets whose parent names this
	// one. Like Blocks it is stored nowhere and derived by scanning the store,
	// so it is detail-only. It carries more than a TicketRef because the tree
	// renders each child's stage and elapsed as well as its status.
	Children []TicketChild `json:"children,omitempty"`
}

// TicketRef is one end of a relation. Title and Status are filled in when the
// daemon finds the referenced ticket in its store, which includes tickets the
// board list hides (archived, or a status with no column). A ref with neither
// names a ticket that is not in the tickets dir, or comes from a board list
// payload, which carries ids alone.
type TicketRef struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// TicketChild is one sub-ticket in the tree at the top of the ticket tab: a
// TicketRef plus what the tree draws beside the title. StartedAt and CompletedAt
// bound the child's whole run, first pickup to last exit, the way the ticket
// page's own wall time is measured. Ticket.StartedAt alone is rewritten at every
// stage spawn and would report only the last stage. CompletedAt is absent while
// the child runs, which is what tells the page to clock it live instead of
// printing a fixed duration. StageIndex is 1-based, and 0 when the child's stage
// is not in its pipeline.
type TicketChild struct {
	ID          string     `json:"id"`
	Title       string     `json:"title,omitempty"`
	Status      string     `json:"status,omitempty"`
	Stage       string     `json:"stage,omitempty"`
	StageIndex  int        `json:"stage_index,omitempty"`
	StageCount  int        `json:"stage_count,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// NoteInfo is one entry from the ticket body's "## Notes" section. At is the
// bold line that opened the note: a UTC RFC3339 timestamp for notes written
// through AddNote, and whatever the author typed for hand-written ones.
type NoteInfo struct {
	At   string `json:"at,omitempty"`
	Text string `json:"text"`
}

type HistoryInfo struct {
	Stage string `json:"stage"`
	Agent string `json:"agent,omitempty"`
	// Model and Effort are what Kontora passed the agent, not what the agent
	// reported running as. Absent means Kontora passed no flag.
	Model       string     `json:"model,omitempty"`
	Effort      string     `json:"effort,omitempty"`
	ExitCode    int        `json:"exit_code"`
	Run         int        `json:"run"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	// Kind is empty for a stage run and "annotation" for an annotation run.
	Kind string `json:"kind,omitempty"`
	// SessionReused is set only on an annotation run: true when it continued the
	// session the stage's last run left behind.
	SessionReused bool `json:"session_reused,omitempty"`
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

// ChainInfo is the dependency chain through one ticket: everything it waits on
// transitively, everything that waits on it, and where it sits on the longest
// path through itself. Derived on every read, stored nowhere.
type ChainInfo struct {
	// ID echoes the ticket the chain was built for, so a response that arrives
	// after the user opened another ticket can be dropped.
	ID string `json:"id"`
	// Verdict is blocked, ready or cycle. It reports the deps alone, never the
	// ticket's own status: a done ticket with closed deps reads ready.
	Verdict string `json:"verdict"`
	// Position is the ticket's 1-based place on the critical path and Goal is
	// that path's last node. Both are zero on a cycle.
	Position   int    `json:"position"`
	PathLength int    `json:"path_length"`
	Goal       string `json:"goal"`
	// Total is the true node count and Done counts the nodes that no longer
	// block. Nodes is capped, so Total > len(Nodes) means the payload was
	// trimmed.
	Total int `json:"total"`
	Done  int `json:"done"`
	// Cycle names the ids on the cycle when Verdict is cycle, and Nodes is then
	// empty: a graph with a cycle has no order to draw.
	Cycle []string    `json:"cycle"`
	Nodes []ChainNode `json:"nodes"`
}

// ChainNode is one ticket in the chain, sorted roots to goal with Depth
// monotonic non-decreasing.
type ChainNode struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Depth  int    `json:"depth"`
	// Direction is upstream, self or downstream.
	Direction      string `json:"direction"`
	OnCriticalPath bool   `json:"on_critical_path"`
	// Resolved is true when the node no longer blocks what depends on it. It is
	// the same rule the daemon schedules on, so the page never has to keep its
	// own list of closed statuses.
	Resolved bool `json:"resolved"`
	// HoldsChain marks the one node the chain is waiting on: an unresolved dep
	// work can start on while something blocks the ticket, and the first
	// unresolved node from the ticket onward while nothing does. A chain whose
	// nodes are all resolved marks none.
	HoldsChain bool       `json:"holds_chain"`
	WaitsOn    ChainWaits `json:"waits_on"`
	// Missing is set for an id no ticket file answers. Title and Status are
	// then empty.
	Missing bool `json:"missing"`
}

// ChainWaits counts a node's own deps, including the ones outside the chain.
type ChainWaits struct {
	Open  int `json:"open"`
	Total int `json:"total"`
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

// ticketRefs wraps relation ids as unresolved refs. Titles and statuses are
// added by whoever holds the store; a View sees one ticket.
func ticketRefs(ids []string) []TicketRef {
	if len(ids) == 0 {
		return nil
	}
	refs := make([]TicketRef, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		refs = append(refs, TicketRef{ID: id})
	}
	return refs
}

// ArchivedTicketInfo is one row of the Archive view. It is not a TicketInfo:
// the list renders nine columns and needs none of the stage arrays, relations
// or bodies the board payload carries.
type ArchivedTicketInfo struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Project  string `json:"project,omitempty"`
	Pipeline string `json:"pipeline,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Path     string `json:"path,omitempty"`
	// Status is the ticket's archived_from, empty for one archived before that
	// field existed. It is never "archived", which every row in this list
	// already is.
	Status string `json:"status"`
	// WallSeconds is the interval from the first history entry's started_at to
	// the last completed_at, so it spans every run rather than the newest one.
	// It is derived here because history is detail-only and the wall column
	// needs it on a list row.
	WallSeconds int        `json:"wall_seconds"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
	ArchivedBy  string     `json:"archived_by,omitempty"`
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
		FinishedAt:    v.FinishedAt,
		Branch:        v.Branch,
		BaseBranch:    v.BaseBranch,
		ClaimedBy:     v.ClaimedBy,
		Stages:        v.Stages,
		Body:          v.Body,
		LastError:     v.LastError,
		LastLog:       v.LastLog,
		Summary:       v.Summary,
		FinalSummary:  v.FinalSummary,
		Notes:         ParseNotes(v.Body),
		Deps:          ticketRefs(v.Deps),
		Links:         ticketRefs(v.Links),
		ArchivedFrom:  v.ArchivedFrom,
		ArchivedAt:    v.ArchivedAt,
		ArchivedBy:    v.ArchivedBy,
		ArchiveNote:   v.ArchiveNote,
	}
	if v.Parent != "" {
		info.Parent = &TicketRef{ID: v.Parent}
	}
	if len(v.History) > 0 {
		info.History = make([]HistoryInfo, len(v.History))
		for i, h := range v.History {
			info.History[i] = HistoryInfo{
				Stage:         h.Stage,
				Agent:         h.Agent,
				Model:         h.Model,
				Effort:        h.Effort,
				ExitCode:      h.ExitCode,
				Run:           h.Run,
				StartedAt:     h.StartedAt,
				CompletedAt:   h.CompletedAt,
				Summary:       h.Summary,
				Kind:          h.Kind,
				SessionReused: h.SessionReused,
			}
		}
	}
	return info
}
