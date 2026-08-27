package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// yamlQuote wraps a string in double quotes if it contains characters
// that are special in YAML (colons, hashes, brackets, etc.).
// Simple alphanumeric values are left unquoted.
func yamlQuote(s string) string {
	if s == "" || strings.ContainsAny(s, ":#{}&*!|>'\"%@`,[]\\") {
		return fmt.Sprintf("%q", s)
	}
	switch strings.ToLower(s) {
	case "null", "~", "true", "false", "yes", "no", "on", "off":
		return fmt.Sprintf("%q", s)
	}
	return s
}

type NewOpts struct {
	ID       string // Pre-generated ID; if empty, GenerateID is called.
	Path     string
	Pipeline string
	Agent    string
	Status   string
	Title    string
	Body     string
	Branch   string
	// BaseBranch names the branch the work branch starts from. Empty means the
	// repository's default branch.
	BaseBranch string
	// ScheduledAt is an RFC 3339 instant the daemon promotes the ticket at. A
	// scheduled ticket is created open, and the whole file is written once, so a
	// watching daemon never sees it as todo before the schedule is in place.
	ScheduledAt string
	NoEdit      bool
	// Kind is "epic" for a ticket that groups others, empty for ordinary work.
	// An epic is created open with no pipeline and no agent: it is not run, and
	// its status is derived from its children from then on.
	Kind string
	// Parent files the new ticket under an epic.
	Parent string
}

// New creates a ticket file and optionally opens it in $EDITOR.
func New(cfg *config.Config, opts NewOpts) (string, error) {
	if opts.Path == "" {
		return "", fmt.Errorf("-path is required")
	}
	if opts.Title == "" {
		return "", fmt.Errorf("title is required")
	}
	if opts.ScheduledAt != "" {
		at, err := ticket.ParseSchedule(opts.ScheduledAt)
		if err != nil {
			return "", err
		}
		if ticket.SchedulePast(at, time.Now()) {
			return "", fmt.Errorf("%s is in the past: %s", ticket.FieldScheduledAt, ticket.FormatSchedule(at))
		}
		opts.ScheduledAt = ticket.FormatSchedule(at)
		if opts.Status == "" {
			opts.Status = "open"
		} else if opts.Status != "open" {
			return "", fmt.Errorf("a scheduled ticket is created open, not %q: the schedule is what moves it to todo", opts.Status)
		}
	}
	isEpic := opts.Kind == string(ticket.KindEpic)
	if err := checkEpicOpts(&opts); err != nil {
		return "", err
	}
	if err := resolveParent(cfg, &opts); err != nil {
		return "", err
	}
	if opts.Status == "" {
		opts.Status = "todo"
	}
	if opts.Status != "open" && opts.Status != "todo" {
		return "", fmt.Errorf("status must be \"open\" or \"todo\", got %q", opts.Status)
	}

	// Every creation path reaches New, so this is the one place where the
	// project and top-level defaults are stamped into the ticket. An epic skips
	// it: a stamped pipeline would give it a stage ribbon and an agent it has
	// no run for.
	if !isEpic {
		opts.Pipeline, opts.Agent = cfg.ResolveTicketDefaults(opts.Path, opts.Pipeline, opts.Agent)
	}

	// A name the config does not know would otherwise sit in the frontmatter
	// until the daemon picked the ticket up and paused it.
	if opts.Pipeline != "" {
		if _, ok := cfg.Pipelines[opts.Pipeline]; !ok {
			return "", fmt.Errorf("unknown pipeline %q", opts.Pipeline)
		}
	}
	if opts.Agent != "" {
		if _, ok := cfg.Agents[opts.Agent]; !ok {
			return "", fmt.Errorf("%w %q", app.ErrUnknownAgent, opts.Agent)
		}
	}

	// An open ticket is a draft, which is why it skips the check. A scheduled one
	// is a commitment to run unattended, so the repository has to be right now
	// rather than at a pickup nobody is watching.
	if !isEpic && (opts.Status != "open" || opts.ScheduledAt != "") {
		if err := CheckRepo(opts.Path, opts.BaseBranch); err != nil {
			return "", err
		}
	}

	id := opts.ID
	if id == "" {
		var err error
		id, err = GenerateID(cfg, opts.Path)
		if err != nil {
			return "", fmt.Errorf("generating id: %w", err)
		}
	}

	content := ticketFrontmatter(id, opts, time.Now().UTC().Format(time.RFC3339))

	dir := config.ExpandTilde(cfg.TicketsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating tickets dir: %w", err)
	}
	path := filepath.Join(dir, id+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("writing ticket file: %w", err)
	}

	if !opts.NoEdit {
		if err := openEditor(cfg.Editor, path); err != nil {
			return id, err
		}
	}

	return id, nil
}

// ticketFrontmatter renders the whole ticket file. Every optional field is a
// line that is either written or absent: an empty value is not the same as no
// value for the round-trip parser, and a stamped empty field would show up in
// the dashboard as a field somebody set.
func ticketFrontmatter(id string, opts NewOpts, created string) string {
	line := func(key, value string) string {
		if value == "" {
			return ""
		}
		return fmt.Sprintf("%s: %s\n", key, yamlQuote(value))
	}
	// Quoted rather than passed through line(): the daemon stores the instant
	// quoted, and a round-trip has to leave the line as it found it.
	scheduled := ""
	if opts.ScheduledAt != "" {
		scheduled = fmt.Sprintf("scheduled_at: %q\n", opts.ScheduledAt)
	}
	body := "\n"
	if opts.Body != "" {
		body = "\n" + opts.Body + "\n"
	}
	return fmt.Sprintf("---\nid: %s\nkontora: true\n%sstatus: %s\n%s%s%s%s%s%spath: %s\ncreated: %s\n---\n# %s\n%s",
		id,
		line("kind", opts.Kind),
		yamlQuote(opts.Status),
		line("pipeline", opts.Pipeline),
		line("agent", opts.Agent),
		line("branch", opts.Branch),
		line("base_branch", opts.BaseBranch),
		scheduled,
		line("parent", opts.Parent),
		yamlQuote(opts.Path), created, opts.Title, body)
}

// checkEpicOpts validates the epic-only rules and settles the status an epic is
// created with. It takes a pointer because "an epic is created open" is a
// default as well as a rule. A ticket with no kind passes through untouched.
func checkEpicOpts(opts *NewOpts) error {
	if opts.Kind == "" {
		return nil
	}
	if opts.Kind != string(ticket.KindEpic) {
		return fmt.Errorf("kind must be %q, got %q", ticket.KindEpic, opts.Kind)
	}
	if opts.ScheduledAt != "" {
		return fmt.Errorf("an epic is never run, so it cannot be scheduled")
	}
	if opts.Pipeline != "" || opts.Agent != "" {
		return fmt.Errorf("an epic has no pipeline and no agent")
	}
	if opts.Parent != "" {
		return fmt.Errorf("epics do not nest")
	}
	switch opts.Status {
	case "":
		opts.Status = "open"
	case "open":
	default:
		return fmt.Errorf("an epic is created open, not %q: its status is derived from its children", opts.Status)
	}
	return nil
}

// resolveParent checks the epic a new ticket is filed under and rewrites the
// option to the full id, so -parent takes a prefix the way every other ticket
// argument does. Service.SetParent enforces the same two rules on an existing
// ticket; without this the creation path is the way around them, and a bad
// parent reaches the file as a dangling reference nothing reports.
func resolveParent(cfg *config.Config, opts *NewOpts) error {
	if opts.Parent == "" {
		return nil
	}
	repo := store.NewDiskRepo(cfg.TicketsDir)
	parentID, err := repo.Resolve(opts.Parent)
	if err != nil {
		return fmt.Errorf("parent %s: %w", opts.Parent, err)
	}
	st, err := repo.Get(parentID)
	if err != nil {
		return fmt.Errorf("parent %s: %w", parentID, err)
	}
	if st.Ticket.Kind != ticket.KindEpic {
		return fmt.Errorf("%w: %s is not an epic", app.ErrNotAnEpic, parentID)
	}
	opts.Parent = parentID
	return nil
}

// ReadDescription reads the markdown a ticket body starts with. The path "-"
// reads stdin, which is how a caller pipes a rendered description in. Trailing
// newlines are trimmed because New adds its own.
func ReadDescription(path string, stdin io.Reader) (string, error) {
	var (
		data []byte
		err  error
	)
	if path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("reading description: %w", err)
	}
	return strings.TrimRight(string(data), "\n"), nil
}
