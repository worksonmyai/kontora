package ticket

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/frontmatter"
	"gopkg.in/yaml.v3"
)

// IsSafeID reports whether id is safe to interpolate into a filesystem path
// as a single filename component. Ticket IDs come from frontmatter and aren't
// constrained by GenerateID for hand-edited tickets, so callers that build
// paths like <dir>/<id>.md must guard against traversal.
func IsSafeID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, "/\\\x00") {
		return false
	}
	if strings.Contains(id, "..") {
		return false
	}
	return true
}

// ListFiles returns the candidate ticket files in dir, in directory order.
// Subdirectories are not descended into and entries that are not ".md" files
// are skipped; whether a file holds a real ticket is only known once it is
// parsed, which is left to the caller.
func ListFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading tickets dir: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	return paths, nil
}

// IsCanonicalPath reports whether path is the canonical file for ticket id,
// i.e. its basename is exactly "<id>.md". Sync tools (iCloud, Syncthing)
// leave stale conflict copies like "<id> 2.md" or "<id>.sync-conflict-*.md"
// whose frontmatter still carries the same id; those must not be treated as
// the live ticket. This applies to all tickets, kontora or not: tickets are
// keyed by id, so a stale copy of a foreign ticket would otherwise alias the
// live one and shadow its content.
func IsCanonicalPath(path, id string) bool {
	return id != "" && filepath.Base(path) == id+".md"
}

// The two frontmatter keys holding a ticket's notification fields. They are
// named because the daemon and the application service both write them.
const (
	FieldNotify         = "notify"
	FieldNotifyChannels = "notify_channels"
)

// NotifyList reads either a single status or a sequence, so `notify: done` and
// `notify: [human_review, done]` both work. It is a type rather than a plain
// []string so a later per-channel mapping form can be added without changing
// tickets already on disk. Modelled on config.PerAgent.
//
// A form this cannot read fails the whole ticket, not just the field: SetField
// re-decodes the entire node, so every daemon write to such a ticket fails and
// the startup scan skips it. That is what a malformed deps: does today, which
// is why the error names the line.
type NotifyList struct {
	Statuses []string
}

func (n *NotifyList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var one string
		if err := value.Decode(&one); err != nil {
			return err
		}
		// `notify:` with nothing after it decodes as an empty scalar, which is
		// a ticket that named no status, not one that named "".
		if one == "" {
			n.Statuses = nil
			return nil
		}
		n.Statuses = []string{one}
		return nil
	}
	if value.Kind == yaml.SequenceNode {
		return value.Decode(&n.Statuses)
	}
	return fmt.Errorf("invalid notify on line %d: want one status or a list of statuses", value.Line)
}

// MarshalYAML keeps the field round-trippable through the typed struct. The
// ticket file itself is written from the raw node, so this only matters where a
// ticket is re-encoded from its Go value.
func (n NotifyList) MarshalYAML() (any, error) {
	if len(n.Statuses) == 0 {
		return nil, nil
	}
	return n.Statuses, nil
}

type Status string

const (
	StatusOpen        Status = "open"
	StatusTodo        Status = "todo"
	StatusInProgress  Status = "in_progress"
	StatusPaused      Status = "paused"
	StatusHumanReview Status = "human_review"
	StatusDone        Status = "done"
	StatusCancelled   Status = "cancelled"
	// StatusArchived is a terminal status for old closed tickets. Archived
	// tickets stay on disk but are hidden from the CLI, TUI, and WebUI lists.
	StatusArchived Status = "archived"
	// StatusLegacyClosed is the terminal status tickets written by the external
	// ticket CLI carry. Kontora reads it as closed: a dependency on such a
	// ticket counts as finished, and the archive sweep accepts one as input. No
	// command writes it.
	StatusLegacyClosed Status = "closed"
)

// Kind names what a ticket is, for the tickets that are not ordinary work. It
// is a named type rather than a string so it cannot be swapped with free text.
type Kind string

// KindEpic marks a ticket that groups others through their parent field. An
// epic is never run: it has no pipeline, no branch and no worktree, and its
// status is derived from its children rather than written by hand.
const KindEpic Kind = "epic"

type HistoryEntry struct {
	Stage string `yaml:"stage"`
	Agent string `yaml:"agent"`
	// Model and Effort are what Kontora passed the agent, resolved from the
	// stage override and the agent's own default, not what the agent reported
	// running as. Empty means Kontora passed no flag and the agent used its own
	// default.
	Model    string `yaml:"model,omitempty"`
	Effort   string `yaml:"effort,omitempty"`
	ExitCode int    `yaml:"exit_code"`
	// Run is the zero-based index of this run among the runs of the same stage.
	// It keys the structured activity sidecar, which is written per run while
	// <stage>.log holds only the newest one.
	Run         int        `yaml:"run,omitempty"`
	StartedAt   *time.Time `yaml:"started_at"`
	CompletedAt *time.Time `yaml:"completed_at"`
	Summary     string     `yaml:"summary,omitempty"`
	// Kind names a run that is not an ordinary stage run. Empty means a stage
	// run; KindAnnotation is a run started from Plannotator annotations.
	Kind string `yaml:"kind,omitempty"`
	// SessionReused reports whether an annotation run continued the session the
	// stage's last run left behind. Only an annotation run sets it; on a stage run
	// it is absent even when the run resumed from a crash record.
	SessionReused bool `yaml:"session_reused,omitempty"`
	// SessionKind names the agent runtime that owns SessionRef: "claude" or
	// "pi". It is recorded rather than derived from Agent because Agent is a
	// config name, and the config that gave it a kind can change or be gone by
	// the time the row is read.
	SessionKind string `yaml:"session_kind,omitempty"`
	// SessionRef locates the run's session JSONL without naming a path only one
	// machine has. For claude it is the session UUID Kontora minted, resolved
	// against $CLAUDE_CONFIG_DIR/projects/*/<uuid>.jsonl; for pi it is the
	// file's path relative to <logs_dir>/<ticket-id>, because pi names its own
	// file. Empty means the run wrote no session Kontora can point at.
	SessionRef string `yaml:"session_ref,omitempty"`
}

// KindAnnotation labels a history entry whose run was asked to rewrite the
// ticket from reviewer annotations, not to do stage work.
const KindAnnotation = "annotation"

type Ticket struct {
	ID          string     `yaml:"id"`
	Kontora     bool       `yaml:"kontora"`
	Status      Status     `yaml:"status"`
	Kind        Kind       `yaml:"kind,omitempty"`
	Pipeline    string     `yaml:"pipeline"`
	Path        string     `yaml:"path"`
	Agent       string     `yaml:"agent"`
	Stage       string     `yaml:"stage"`
	Attempt     int        `yaml:"attempt"`
	StartedAt   *time.Time `yaml:"started_at"`
	CompletedAt *time.Time `yaml:"completed_at"`
	Branch      string     `yaml:"branch"`
	// BaseBranch names the branch the work branch starts from. Empty means the
	// repository's default branch.
	BaseBranch string         `yaml:"base_branch"`
	History    []HistoryEntry `yaml:"history"`
	// Deps, Links and Parent are the ticket's relations: deps lists the tickets
	// this one is blocked by, links is a symmetric "related" list, and parent
	// names the epic or parent task. Deps and links are written by the relation
	// commands; parent is not written by kontora. The reverse of deps is not
	// stored; it is derived by scanning the store.
	Deps   []string `yaml:"deps"`
	Links  []string `yaml:"links"`
	Parent string   `yaml:"parent"`
	// Children orders an epic's sub-tickets. Membership is defined by the
	// child's parent field, never by this list: an ID here that names no child
	// is ignored, and a child missing from it sorts last by created.
	Children []string `yaml:"children,omitempty"`
	// ScheduledAt is the instant an open ticket becomes todo, as an RFC 3339
	// string. It is a string rather than a *time.Time so a hand-edited value the
	// strict parser rejects stays visible instead of making the whole ticket
	// unparsable. Absent from a ticket with no schedule, and removed by the
	// promotion. Use Schedule to read it.
	ScheduledAt string     `yaml:"scheduled_at,omitempty"`
	Created     *time.Time `yaml:"created"`
	LastError   string     `yaml:"last_error"`
	LastLog     string     `yaml:"last_log"`
	Summary     string     `yaml:"summary"`
	// FinalSummary is the ticket-level result: one text synthesized from the
	// ticket itself and every run summary the ticket recorded. It opens with
	// what the ticket set out to do, then what the runs did. It is written
	// after a terminal stage succeeds and cleared when a new run starts, so
	// unlike Summary it never describes a single run.
	FinalSummary string `yaml:"final_summary"`
	// ClaimedBy records the daemon instance that last picked the ticket up. It
	// is consulted only while Status is in_progress, to keep daemons on a shared
	// tickets_dir from stealing or killing each other's work.
	ClaimedBy string `yaml:"claimed_by"`
	// AnnotationReturnStatus parks a ticket for an annotation run. It holds the
	// status the ticket had when the annotations were submitted, and its presence
	// is what routes the next pickup to that run instead of the pipeline. The run
	// restores the status and clears this field. It is a Status rather than a
	// string so that it cannot be swapped with an error message on the way back
	// into the status field.
	AnnotationReturnStatus Status `yaml:"annotation_return_status"`

	// ArchivedFrom is the closed status the ticket held before it was archived,
	// and is what a restore writes back into status. It is a Status rather than
	// a string for the same reason AnnotationReturnStatus is: it must not be
	// swappable with free text on the way back into the status field. The other
	// three record when the archive happened, who did it ("web", "sweep") and
	// the optional reason. All four are absent from a ticket that is not
	// archived, and a restore removes them again.
	ArchivedFrom Status     `yaml:"archived_from,omitempty"`
	ArchivedAt   *time.Time `yaml:"archived_at,omitempty"`
	ArchivedBy   string     `yaml:"archived_by,omitempty"`
	ArchiveNote  string     `yaml:"archive_note,omitempty"`

	// Notify lists the statuses whose arrival this ticket asks to be told
	// about, plus the pseudo-status "waiting". A ticket without the field is
	// silent. Only a transition the daemon decided on its own sends: a person
	// moving the ticket by hand, through the CLI or the dashboard, sends
	// nothing.
	Notify NotifyList `yaml:"notify,omitempty"`
	// NotifyChannels overrides the channels this ticket's notifications go to,
	// above the project's and the global default. The sole entry "none"
	// silences the ticket while leaving its notify: list readable.
	NotifyChannels []string `yaml:"notify_channels,omitempty"`

	Body     string `yaml:"-"`
	FilePath string `yaml:"-"`

	rawNode *yaml.Node // preserved for round-trip
	rawBody string     // original body bytes
}

func ParseFile(path string) (*Ticket, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := ParseBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	t.FilePath = path
	return t, nil
}

func Parse(r io.Reader) (*Ticket, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return ParseBytes(data)
}

func ParseBytes(data []byte) (*Ticket, error) {
	yamlStr, body, err := frontmatter.Split(string(data))
	if err != nil {
		return nil, err
	}

	// Parse into yaml.Node for round-trip preservation
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		return nil, fmt.Errorf("parsing frontmatter YAML: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("expected YAML document, got kind %d", doc.Kind)
	}

	// Decode known fields directly from the parsed node (single parse)
	var t Ticket
	if err := doc.Content[0].Decode(&t); err != nil {
		return nil, fmt.Errorf("decoding fields: %w", err)
	}
	t.Body = body
	t.rawNode = doc.Content[0]
	t.rawBody = body

	return &t, nil
}

// Marshal re-encodes the ticket as a frontmatter markdown file.
// The body is preserved byte-for-byte.
func (t *Ticket) Marshal() ([]byte, error) {
	var yamlBuf bytes.Buffer
	enc := yaml.NewEncoder(&yamlBuf)
	enc.SetIndent(2)
	if err := enc.Encode(t.rawNode); err != nil {
		return nil, fmt.Errorf("encoding frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("closing encoder: %w", err)
	}

	yamlOut := yamlBuf.String()
	yamlOut = strings.TrimSuffix(yamlOut, "...\n")
	yamlOut = strings.TrimSuffix(yamlOut, "\n")

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.WriteString(yamlOut)
	buf.WriteString("\n---\n")
	buf.WriteString(t.rawBody)

	return buf.Bytes(), nil
}

// SetField updates or adds a field in the raw YAML node and syncs typed fields.
func (t *Ticket) SetField(key string, value any) error {
	if t.rawNode == nil || t.rawNode.Kind != yaml.MappingNode {
		return fmt.Errorf("raw node is not a mapping")
	}

	var valNode yaml.Node
	if err := valNode.Encode(value); err != nil {
		return fmt.Errorf("encoding value: %w", err)
	}

	// Search for existing key
	content := t.rawNode.Content
	for i := 0; i < len(content)-1; i += 2 {
		if content[i].Value == key {
			*content[i+1] = valNode
			return t.syncFromNode()
		}
	}

	// Append new key-value pair
	keyNode := &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: key,
	}
	t.rawNode.Content = append(t.rawNode.Content, keyNode, &valNode)
	return t.syncFromNode()
}

// DeleteField removes a field from the raw YAML node and syncs typed fields.
// Unlike SetField with a zero value, which leaves the key behind holding an
// empty value, this leaves no trace of the field in the frontmatter. Removing
// a key that is not there is not an error.
func (t *Ticket) DeleteField(key string) error {
	if t.rawNode == nil || t.rawNode.Kind != yaml.MappingNode {
		return fmt.Errorf("raw node is not a mapping")
	}
	content := t.rawNode.Content
	for i := 0; i < len(content)-1; i += 2 {
		if content[i].Value == key {
			t.rawNode.Content = slices.Delete(content, i, i+2)
			return t.syncFromNode()
		}
	}
	return nil
}

// syncFromNode re-decodes the typed fields from the raw node. It decodes into a
// fresh Ticket rather than over this one, because Decode leaves a field alone
// when its key is absent: decoding in place would keep the Go-side value of a
// key DeleteField just removed. The non-YAML fields are carried across.
func (t *Ticket) syncFromNode() error {
	if t.rawNode == nil {
		return nil
	}
	var decoded Ticket
	if err := t.rawNode.Decode(&decoded); err != nil {
		return err
	}
	decoded.Body = t.Body
	decoded.FilePath = t.FilePath
	decoded.rawNode = t.rawNode
	decoded.rawBody = t.rawBody
	*t = decoded
	return nil
}

// SetBody replaces the ticket body content.
func (t *Ticket) SetBody(body string) {
	t.Body = body
	t.rawBody = body
}

// Title returns the first # heading from the body.
func (t *Ticket) Title() string {
	for line := range strings.SplitSeq(t.Body, "\n") {
		line = strings.TrimSpace(line)
		if title, ok := strings.CutPrefix(line, "# "); ok {
			return title
		}
	}
	return ""
}
