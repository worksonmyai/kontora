package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/search"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// SearchOpts holds parameters for the search command.
type SearchOpts struct {
	Query      string
	Literal    bool
	IgnoreCase bool
	MatchCase  bool
	BodyOnly   bool
	// IDsOnly prints one ticket ID per line and nothing else.
	IDsOnly bool
	JSON    bool
	// MaxPerFile caps the matching lines shown per ticket. 0 shows all of them.
	MaxPerFile int

	Status   string
	Project  string
	Path     string
	Pipeline string
	Agent    string

	// Warn receives the report of tickets that could not be parsed and the
	// warning about a status no ticket can carry. It is separate from the
	// results writer so a warning cannot corrupt --json or -l output; a nil
	// writer drops the report.
	Warn io.Writer
}

// Search matches opts.Query against every ticket in cfg.TicketsDir and writes
// the results to w. It reads the tickets directory directly, so it works with
// no daemon running.
func Search(cfg *config.Config, w io.Writer, opts SearchOpts) error {
	scanOpts, err := searchScanOptions(cfg, opts)
	if err != nil {
		return err
	}
	warnUnknownStatus(cfg, opts)

	// A missing tickets directory is an empty store, not a failure: the run
	// reports no matches in whichever shape was asked for.
	results, parseErrs, err := search.Scan(config.ExpandTilde(cfg.TicketsDir), scanOpts)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	sortResults(results)

	if err := renderSearch(cfg, w, opts, results); err != nil {
		return err
	}
	reportParseErrors(opts.Warn, parseErrs)
	return nil
}

// sortResults puts the results in the board's order: by status, then most
// recently touched, with the title and the id breaking the ties.
func sortResults(results []search.Result) {
	slices.SortFunc(results, func(a, b search.Result) int {
		if ra, rb := StatusRank(a.Ticket.Status), StatusRank(b.Ticket.Status); ra != rb {
			return ra - rb
		}
		if c := ticketSortTime(b.Ticket).Compare(ticketSortTime(a.Ticket)); c != 0 {
			return c
		}
		if at, bt := a.Ticket.Title(), b.Ticket.Title(); at != bt {
			return strings.Compare(at, bt)
		}
		return strings.Compare(a.Ticket.ID, b.Ticket.ID)
	})
}

func ticketSortTime(t *ticket.Ticket) time.Time {
	if t.Status == ticket.StatusInProgress && t.StartedAt != nil {
		return *t.StartedAt
	}
	return derefTime(t.Created)
}

// searchScanOptions translates the command's flags into the engine's options:
// it checks the flags that contradict each other, resolves the case mode and
// the project filter, and hands over the configured projects so a query can
// match a project name.
func searchScanOptions(cfg *config.Config, opts SearchOpts) (search.Options, error) {
	if err := validateSearchOpts(cfg, opts); err != nil {
		return search.Options{}, err
	}
	filterPath, err := cfg.ResolveFilterPath(opts.Path, opts.Project)
	if err != nil {
		return search.Options{}, fmt.Errorf("search: %w", err)
	}

	scanOpts := search.Options{
		Query:      opts.Query,
		Literal:    opts.Literal,
		CaseMode:   searchCaseMode(opts),
		BodyOnly:   opts.BodyOnly,
		MaxPerFile: opts.MaxPerFile,
		Status:     opts.Status,
		Path:       config.NormalizeRepoPath(filterPath),
		Pipeline:   opts.Pipeline,
		Agent:      opts.Agent,
	}
	if len(cfg.Projects) > 0 {
		scanOpts.Projects = make(map[string]string, len(cfg.Projects))
		for name, project := range cfg.Projects {
			scanOpts.Projects[name] = project.Path
		}
	}
	return scanOpts, nil
}

// validateSearchOpts rejects the flag combinations that cannot be honoured and
// the filter values no ticket can carry. A pipeline or agent that is not
// configured is a typo: without this the run reports "No matches." and reads
// like the query is not in any ticket.
func validateSearchOpts(cfg *config.Config, opts SearchOpts) error {
	switch {
	case opts.IgnoreCase && opts.MatchCase:
		return errors.New("search: -i and -s cannot be combined")
	case opts.IDsOnly && opts.JSON:
		return errors.New("search: -l and --json cannot be combined")
	case opts.MaxPerFile < 0:
		return fmt.Errorf("search: -m cannot be negative, got %d (0 shows all lines)", opts.MaxPerFile)
	}
	if opts.Pipeline != "" {
		if _, ok := cfg.Pipelines[opts.Pipeline]; !ok {
			return fmt.Errorf("search: unknown pipeline %q", opts.Pipeline)
		}
	}
	if opts.Agent != "" {
		if _, ok := cfg.Agents[opts.Agent]; !ok {
			return fmt.Errorf("search: unknown agent %q", opts.Agent)
		}
	}
	return nil
}

// warnUnknownStatus reports a --status value that is neither built in nor
// configured. It is a warning and not an error, unlike the pipeline and agent
// filters: a ticket written by another tool may carry any status string, and
// searching for one has to stay possible.
func warnUnknownStatus(cfg *config.Config, opts SearchOpts) {
	if opts.Warn == nil || opts.Status == "" || cfg.IsKnownStatus(opts.Status) {
		return
	}
	fmt.Fprintf(opts.Warn, "warning: %q is not a configured status\n", opts.Status)
}

func searchCaseMode(opts SearchOpts) search.CaseMode {
	switch {
	case opts.IgnoreCase:
		return search.CaseInsensitive
	case opts.MatchCase:
		return search.CaseSensitive
	default:
		return search.CaseSmart
	}
}

func renderSearch(cfg *config.Config, w io.Writer, opts SearchOpts, results []search.Result) error {
	switch {
	case opts.JSON:
		return writeSearchJSON(cfg, w, results)
	case opts.IDsOnly:
		for _, r := range results {
			if _, err := fmt.Fprintln(w, r.Ticket.ID); err != nil {
				return err
			}
		}
		return nil
	}

	if len(results) == 0 {
		_, err := fmt.Fprintln(w, styleFaint.Render("No matches."))
		return err
	}

	width := outputWidth(w)
	for i, r := range results {
		if i > 0 {
			fmt.Fprintln(w)
		}
		writeSearchResult(cfg, w, r, width)
	}
	return nil
}

func writeSearchResult(cfg *config.Config, w io.Writer, r search.Result, width int) {
	project := searchProject(cfg, r.Ticket)
	fmt.Fprintln(w, searchHeader(r.Ticket, project, width))

	gutter := searchGutter(r.Hits)
	for _, hit := range r.Hits {
		// The row is two spaces, the gutter, and two more before the text.
		text, start, end := clipLine(hit.Text, hit.Start, hit.End, width-gutter-4)
		if width > 0 {
			text = highlightMatch(text, start, end)
		}
		fmt.Fprintf(w, "  %*s  %s\n", gutter, searchHitLabel(hit), text)
	}
	if r.Total > len(r.Hits) {
		fmt.Fprintf(w, "  %*s  %s\n", gutter, "", styleFaint.Render(fmt.Sprintf("... %d more", r.Total-len(r.Hits))))
	}
}

// searchHeader is the per-ticket line: id, status, title and project. The other
// columns are short and fixed, so the title is the one that absorbs the
// clipping when the line does not fit.
func searchHeader(t *ticket.Ticket, project string, width int) string {
	title := t.Title()
	if width > 0 && title != "" {
		room := headerTitleRoom(t, project, width)
		if room <= 0 {
			// Nothing left to print a title in. The id, the status and the
			// project are the columns worth keeping.
			title = ""
		} else {
			title, _, _ = clipLine(title, 0, 0, room)
		}
	}

	parts := []string{styleCyan.Render(t.ID), searchStatus(t.Status)}
	if title != "" {
		parts = append(parts, styleBold.Render(title))
	}
	if project != "" {
		parts = append(parts, styleFaint.Render(project))
	}
	return strings.Join(parts, "  ")
}

// headerTitleRoom is the width left for the title once the columns that are
// always printed have taken theirs, separators included. It goes to zero or
// below on a terminal too narrow to hold those columns, which is the caller's
// signal to drop the title.
func headerTitleRoom(t *ticket.Ticket, project string, width int) int {
	// The title itself and the two columns before it, plus the project when it
	// is printed. Every gap between them is two columns wide.
	parts := 3
	room := width - utf8.RuneCountInString(t.ID) - utf8.RuneCountInString(string(t.Status))
	if project != "" {
		parts++
		room -= utf8.RuneCountInString(project)
	}
	return room - 2*(parts-1)
}

func searchStatus(status ticket.Status) string {
	if c, ok := StatusColor[status]; ok {
		return lipgloss.NewStyle().Foreground(c).Render(string(status))
	}
	return styleFaint.Render(string(status))
}

// searchGutter is the width of the line-number column, wide enough for the
// synthetic project hit's label as well.
func searchGutter(hits []search.Hit) int {
	gutter := 0
	for _, hit := range hits {
		gutter = max(gutter, len(searchHitLabel(hit)))
	}
	return gutter
}

func searchHitLabel(hit search.Hit) string {
	if hit.Field != "" {
		return hit.Field
	}
	return strconv.Itoa(hit.Line)
}

func searchProject(cfg *config.Config, t *ticket.Ticket) string {
	name, _, ok := cfg.ProjectFor(t.Path)
	if !ok {
		return ""
	}
	return name
}

type searchMatchJSON struct {
	Line  int    `json:"line"`
	Field string `json:"field"`
	Text  string `json:"text"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

type searchResultJSON struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Stage    string `json:"stage"`
	Pipeline string `json:"pipeline"`
	Agent    string `json:"agent"`
	Path     string `json:"path"`
	Project  string `json:"project"`
	File     string `json:"file"`
	// Total counts every match found, the project match included, so a consumer
	// can tell that -m truncated Matches.
	Total   int               `json:"total"`
	Matches []searchMatchJSON `json:"matches"`
}

func writeSearchJSON(cfg *config.Config, w io.Writer, results []search.Result) error {
	out := make([]searchResultJSON, 0, len(results))
	for _, r := range results {
		matches := make([]searchMatchJSON, 0, len(r.Hits))
		for _, hit := range r.Hits {
			matches = append(matches, searchMatchJSON{
				Line:  hit.Line,
				Field: hit.Field,
				Text:  hit.Text,
				Start: hit.Start,
				End:   hit.End,
			})
		}
		out = append(out, searchResultJSON{
			ID:       r.Ticket.ID,
			Title:    r.Ticket.Title(),
			Status:   string(r.Ticket.Status),
			Stage:    r.Ticket.Stage,
			Pipeline: r.Ticket.Pipeline,
			Agent:    r.Ticket.Agent,
			Path:     r.Ticket.Path,
			Project:  searchProject(cfg, r.Ticket),
			File:     r.FilePath,
			Total:    r.Total,
			Matches:  matches,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func reportParseErrors(w io.Writer, errs []error) {
	if w == nil || len(errs) == 0 {
		return
	}
	noun := "tickets"
	if len(errs) == 1 {
		noun = "ticket"
	}
	fmt.Fprintf(w, "%d %s could not be parsed:\n", len(errs), noun)
	for _, err := range errs {
		fmt.Fprintf(w, "  %v\n", err)
	}
}

// outputWidth is the terminal width of w, or 0 when w is not a terminal.
// Highlighting and clipping both hang off it: piped output stays plain and
// uncut so it can be read by another program.
func outputWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return 0
	}
	width, _, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

// styleMatch marks the matched span inside a line, the way ripgrep does.
var styleMatch = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))

func highlightMatch(text string, start, end int) string {
	if start >= end || end > len(text) {
		return text
	}
	return text[:start] + styleMatch.Render(text[start:end]) + text[end:]
}

// clipLine cuts text to at most width columns and returns the match span
// clamped to what survived. The window slides right when the match sits past
// the width, the way ripgrep does, so a hit far into a long run summary is
// still the part that gets shown. A cut end is marked with an ellipsis, which
// costs one of the columns.
func clipLine(text string, start, end, width int) (string, int, int) {
	runes := []rune(text)
	if width <= 0 || len(runes) <= width {
		return text, start, end
	}

	// Leave a quarter of the window in front of the match for context.
	head := 0
	if end > 0 && utf8.RuneCountInString(text[:min(end, len(text))]) > width {
		head = max(utf8.RuneCountInString(text[:min(start, len(text))])-width/4, 0)
	}

	room := width
	if head > 0 {
		room--
	}
	if head+room < len(runes) {
		room--
	}
	room = max(room, 1)

	tail := min(head+room, len(runes))
	clipped := string(runes[head:tail])
	prefix, suffix := "", ""
	if head > 0 {
		prefix = "…"
	}
	if tail < len(runes) {
		suffix = "…"
	}

	// The span is in byte offsets, so it moves by the bytes cut off the front
	// and by the bytes the leading ellipsis adds back.
	start -= len(string(runes[:head]))
	end -= len(string(runes[:head]))
	switch {
	case start < 0 || start >= len(clipped):
		start, end = 0, 0
	case end > len(clipped):
		end = len(clipped)
	}
	if start < end {
		start += len(prefix)
		end += len(prefix)
	}
	return prefix + clipped + suffix, start, end
}
