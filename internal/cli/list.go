package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// ListItem is one row of `kontora ls`, in local and in remote mode. It is also
// the `--json` object, so it carries the relations and the derived readiness
// but never the ticket body: a listing is for choosing a ticket, not reading it.
type ListItem struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Status      string     `json:"status"`
	Kontora     bool       `json:"kontora"`
	Path        string     `json:"path,omitempty"`
	Project     string     `json:"project,omitempty"`
	Pipeline    string     `json:"pipeline,omitempty"`
	Stage       string     `json:"stage,omitempty"`
	Agent       string     `json:"agent,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Deps        []string   `json:"deps"`
	Links       []string   `json:"links"`
	Parent      string     `json:"parent,omitempty"`
	// Ready and Blockers are derived from the whole set, not stored. Ready is
	// meaningful only for a kontora ticket in todo; Blockers names the
	// dependency ids that are not closed.
	Ready    bool     `json:"ready"`
	Blockers []string `json:"blockers"`
}

// ListOpts are the filters and the output mode `kontora ls` takes.
type ListOpts struct {
	Status  string
	Project string
	Path    string
	// Ready and Blocked select kontora tickets in todo, and are mutually
	// exclusive.
	Ready   bool
	Blocked bool
	// ShowClosed adds done, cancelled and legacy closed tickets; ShowArchived
	// adds archived ones.
	ShowClosed   bool
	ShowArchived bool
	Limit        int
	JSON         bool
}

// Filtering reports whether any filter or output flag was given. Any of them
// makes the listing non-interactive: a filtered board is not the board.
func (o ListOpts) Filtering() bool {
	return o.Status != "" || o.Project != "" || o.Path != "" || o.Ready || o.Blocked ||
		o.Limit > 0 || o.JSON
}

// Validate rejects the flag combinations that contradict each other.
func (o ListOpts) Validate() error {
	if o.Ready && o.Blocked {
		return fmt.Errorf("--ready and --blocked select opposite sets and cannot be combined")
	}
	if o.Limit < 0 {
		return fmt.Errorf("--limit must not be negative")
	}
	return nil
}

// hiddenByDefault are the statuses a plain listing leaves out. Naming one with
// --status is asking to see it, so that filter overrides the default: --status
// done needs no --closed, and --status archived no --archived.
func hiddenByDefault(status ticket.Status, opts ListOpts) bool {
	if opts.Status == string(status) {
		return false
	}
	switch status { //nolint:exhaustive // only the hidden statuses matter here
	case ticket.StatusArchived:
		return !opts.ShowArchived
	case ticket.StatusDone, ticket.StatusCancelled, ticket.StatusLegacyClosed:
		return !opts.ShowClosed
	default:
		return false
	}
}

// ClassifyList fills Ready and Blockers on every item, from the statuses of the
// whole set. It must see every ticket, including the ones a filter will drop:
// a dependency on an archived ticket is resolved, and dropping archived tickets
// first would make it look missing.
func ClassifyList(items []ListItem) {
	status := make(map[string]ticket.Status, len(items))
	for _, it := range items {
		status[it.ID] = ticket.Status(it.Status)
	}
	for i := range items {
		items[i].Ready = true
		items[i].Blockers = nil
		for _, dep := range items[i].Deps {
			if dep == "" {
				continue
			}
			if s, ok := status[dep]; ok && ticket.IsDependencyResolved(s) {
				continue
			}
			items[i].Ready = false
			items[i].Blockers = append(items[i].Blockers, dep)
		}
	}
}

// Listing is what a set of tickets reduces to under ListOpts.
type Listing struct {
	Items []ListItem
	// HiddenClosed counts the closed tickets a default listing left out. It is
	// what tells "there are no tickets" apart from "they are all closed".
	HiddenClosed int
}

// FilterList applies the filters, orders the result and cuts it to the limit.
// Ready and blocked listings follow scheduler order, oldest first; every other
// listing keeps the status-and-recency order the board uses.
func FilterList(items []ListItem, opts ListOpts) Listing {
	wantPath := config.NormalizeRepoPath(opts.Path)

	var out []ListItem
	hiddenClosed := 0
	for _, it := range items {
		if hiddenByDefault(ticket.Status(it.Status), opts) {
			if ticket.Status(it.Status) != ticket.StatusArchived {
				hiddenClosed++
			}
			continue
		}
		if opts.Status != "" && it.Status != opts.Status {
			continue
		}
		if opts.Project != "" && it.Project != opts.Project {
			continue
		}
		if wantPath != "" && config.NormalizeRepoPath(it.Path) != wantPath {
			continue
		}
		if opts.Ready || opts.Blocked {
			if !it.Kontora || it.Status != string(ticket.StatusTodo) {
				continue
			}
			if it.Ready != opts.Ready {
				continue
			}
		}
		out = append(out, it)
	}

	if opts.Ready || opts.Blocked {
		slices.SortFunc(out, func(a, b ListItem) int {
			if c := derefTime(a.CreatedAt).Compare(derefTime(b.CreatedAt)); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
	} else {
		slices.SortFunc(out, compareListItems)
	}

	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return Listing{Items: out, HiddenClosed: hiddenClosed}
}

// compareListItems is the board's order: by status rank, then newest first,
// then title and id so equal timestamps do not shuffle between runs.
func compareListItems(a, b ListItem) int {
	if ra, rb := StatusRank(ticket.Status(a.Status)), StatusRank(ticket.Status(b.Status)); ra != rb {
		return ra - rb
	}
	ta, tb := listSortTime(a), listSortTime(b)
	if c := tb.Compare(ta); c != 0 {
		return c
	}
	if a.Title != b.Title {
		return strings.Compare(a.Title, b.Title)
	}
	return strings.Compare(a.ID, b.ID)
}

func listSortTime(it ListItem) time.Time {
	if it.Status == string(ticket.StatusInProgress) && it.StartedAt != nil {
		return *it.StartedAt
	}
	return derefTime(it.CreatedAt)
}

// RenderList writes the listing as JSON or as the static table.
func RenderList(l Listing, w io.Writer, opts ListOpts) error {
	items := l.Items
	if opts.JSON {
		// An empty result is an empty array rather than null, so a consumer can
		// iterate it without a nil check.
		if items == nil {
			items = []ListItem{}
		}
		for i := range items {
			if items[i].Deps == nil {
				items[i].Deps = []string{}
			}
			if items[i].Links == nil {
				items[i].Links = []string{}
			}
			if items[i].Blockers == nil {
				items[i].Blockers = []string{}
			}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	if len(items) == 0 {
		fmt.Fprintln(w, emptyListMessage(opts, l.HiddenClosed > 0))
		return nil
	}
	fmt.Fprintln(w, renderListTable(items))
	return nil
}

// emptyListMessage says why the listing is empty, so a filtered run does not
// read as "there are no tickets at all".
func emptyListMessage(opts ListOpts, hiddenClosed bool) string {
	switch {
	case opts.Ready:
		return "No ready tickets."
	case opts.Blocked:
		return "No blocked tickets."
	case opts.Filtering():
		return "No tickets match."
	case hiddenClosed:
		return "No active tickets. Use --closed to show done/cancelled."
	default:
		return "No tickets."
	}
}

func renderListTable(items []ListItem) string {
	headers := []string{"ID", "STATUS", "TITLE", "PROJECT", "STAGE", "AGENT", "BLOCKED BY", "MARKER"}
	pad := lipgloss.NewStyle().PaddingRight(3)

	tbl := table.New().
		Headers(headers...).
		Border(lipgloss.HiddenBorder()).
		StyleFunc(func(row, col int) lipgloss.Style {
			base := pad
			if row == table.HeaderRow {
				return base.Bold(true).Faint(true)
			}
			if row < 0 || row >= len(items) {
				return base
			}
			it := items[row]
			status := ticket.Status(it.Status)
			if !it.Kontora || status == ticket.StatusDone || status == ticket.StatusCancelled {
				return base.Faint(true)
			}
			switch col {
			case 0: // ID
				return base.Faint(true)
			case 1: // STATUS
				if c, ok := StatusColor[status]; ok {
					return base.Foreground(c)
				}
			case 4: // STAGE
				return base.Foreground(lipgloss.Color("6")) // cyan
			}
			return base
		})

	for _, it := range items {
		marker := ""
		if !it.Kontora {
			marker = "not a kontora ticket"
		}
		tbl.Row(it.ID, it.Status, dashIfEmpty(it.Title), dashIfEmpty(it.Project),
			dashIfEmpty(it.Stage), dashIfEmpty(it.Agent), dashIfEmpty(strings.Join(it.Blockers, ", ")), marker)
	}
	return tbl.Render()
}

// LocalItems projects every canonical ticket in the tickets directory into a
// ListItem, with readiness derived from the whole set.
func LocalItems(cfg *config.Config) ([]ListItem, error) {
	stored, err := store.NewDiskRepo(cfg.TicketsDir).List()
	if err != nil {
		return nil, err
	}
	items := make([]ListItem, 0, len(stored))
	for _, st := range stored {
		items = append(items, listItem(cfg, st.Ticket))
	}
	ClassifyList(items)
	return items, nil
}

func listItem(cfg *config.Config, t *ticket.Ticket) ListItem {
	project, _, _ := cfg.ProjectFor(t.Path)
	return ListItem{
		ID:          t.ID,
		Title:       t.Title(),
		Status:      string(t.Status),
		Kontora:     t.Kontora,
		Path:        t.Path,
		Project:     project,
		Pipeline:    t.Pipeline,
		Stage:       displayStage(t),
		Agent:       displayAgent(cfg, t),
		CreatedAt:   t.Created,
		StartedAt:   t.StartedAt,
		CompletedAt: t.CompletedAt,
		Deps:        t.Deps,
		Links:       t.Links,
		Parent:      t.Parent,
	}
}

// displayStage names the stage column. A kontora ticket with no pipeline runs
// standalone rather than having no stage.
func displayStage(t *ticket.Ticket) string {
	if t.Stage == "" && t.Pipeline == "" && t.Kontora {
		return "standalone"
	}
	return t.Stage
}

// displayAgent names the agent that would run the ticket next: the stage's, or
// the default for a standalone kontora ticket.
func displayAgent(cfg *config.Config, t *ticket.Ticket) string {
	if agent := app.AgentForStage(cfg, t.Pipeline, t.Stage); agent != "" {
		return agent
	}
	if t.Agent != "" {
		return t.Agent
	}
	if t.Kontora {
		return cfg.DefaultAgent
	}
	return ""
}

// List prints the ticket listing for `kontora ls` in local mode.
func List(cfg *config.Config, w io.Writer, opts ListOpts) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	items, err := LocalItems(cfg)
	if err != nil {
		return err
	}
	return RenderList(FilterList(items, opts), w, opts)
}
