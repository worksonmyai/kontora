// Package search matches a query against the ticket files in a directory.
//
// The match runs over the raw markdown, frontmatter block and body alike, so
// keys the ticket.Ticket struct does not carry (type, priority, assignee, and
// anything else a hand-edited ticket holds) are searchable, and every hit
// carries a true 1-based file line number.
package search

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// CaseMode selects how the query treats letter case.
type CaseMode int

const (
	// CaseSmart matches case-insensitively when the query holds no uppercase
	// letter, and case-sensitively when it does.
	CaseSmart CaseMode = iota
	CaseInsensitive
	CaseSensitive
)

// FieldProject marks the synthetic hit a ticket gets because its project name
// matched. A project is not written in the ticket file, so it cannot be found
// by scanning one.
const FieldProject = "project"

type Options struct {
	Query   string
	Literal bool
	// CaseMode is CaseSmart unless -i or -s overrode it.
	CaseMode CaseMode
	// BodyOnly restricts the scan to the markdown body, skipping the
	// frontmatter block. It also drops project matching: a project name is in
	// neither the frontmatter nor the body.
	BodyOnly bool
	// MaxPerFile caps the file lines kept per ticket. 0, or any negative value,
	// keeps all of them. The synthetic project hit is never capped away, since
	// it is the reason the ticket is a result at all. Result.Total always counts
	// every hit found, capped or not.
	MaxPerFile int

	// Status, Path, Pipeline and Agent narrow the result set by the ticket's own
	// frontmatter fields. An empty value matches any ticket. Agent is the
	// ticket's agent override, not the agent its current pipeline stage resolves
	// to, which only the config knows.
	Status   string
	Path     string
	Pipeline string
	Agent    string

	// Projects maps a configured project name to the repository path it owns.
	// A project name is written in the config, not in any ticket file, so this
	// is the only way a query can find one: every ticket pointing at the path of
	// a project whose name matches is a result, carrying a synthetic
	// FieldProject hit. Callers that do not search project names leave it nil.
	Projects map[string]string
}

// Hit is one matching line.
type Hit struct {
	// Line is the 1-based line in the file, or 0 for the synthetic project hit.
	Line int
	// Field is FieldProject for the synthetic hit and empty for a file line.
	Field string
	Text  string
	// Start and End are the byte offsets of the match within Text.
	Start int
	End   int
}

// Result is one matching ticket.
type Result struct {
	Ticket   *ticket.Ticket
	FilePath string
	Hits     []Hit
	// Total counts every hit found, the synthetic project hit included, so it
	// exceeds len(Hits) when MaxPerFile truncated them.
	Total int
}

// compile builds the matcher the options describe.
func compile(opts Options) (*regexp.Regexp, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("search: query is required")
	}
	pattern := opts.Query
	if opts.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	// (?m) makes ^ and $ line anchors instead of text anchors. The prefilter
	// matches the whole file at once, so without it every anchored query drops
	// each file before lineHits, which matches per line, ever runs: the search
	// would silently report nothing. Per line it is a no-op, since a single line
	// holds no newline.
	flags := "m"
	if insensitive(opts) {
		flags += "i"
	}
	re, err := regexp.Compile("(?" + flags + ")" + pattern)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return re, nil
}

// insensitive resolves the case mode against the query.
func insensitive(opts Options) bool {
	switch opts.CaseMode {
	case CaseInsensitive:
		return true
	case CaseSensitive:
		return false
	case CaseSmart:
	}
	return !hasUpper(opts.Query, opts.Literal)
}

// hasUpper reports whether the query holds an uppercase letter. In regex mode
// the letter of an escape sequence is not one: \S and \W select a character
// class, they do not ask for case-sensitive matching.
func hasUpper(query string, literal bool) bool {
	escaped := false
	for _, r := range query {
		switch {
		case escaped:
			escaped = false
		case !literal && r == '\\':
			escaped = true
		case r >= 'A' && r <= 'Z':
			return true
		}
	}
	return false
}

// Scan matches every ticket in dir and returns the results, the tickets that
// could not be parsed, and a fatal error. A parse failure is reported rather
// than dropped, so a malformed ticket does not silently disappear from a search
// that should have found it.
//
// Subdirectories are not descended into, and a file whose basename is not
// "<id>.md" is skipped: sync tools leave conflict copies carrying the same id,
// and reporting one would double the ticket.
//
// Results come back in file path order. Callers that want another order sort
// what they get; the order here only has to be the same on every run.
func Scan(dir string, opts Options) ([]Result, []error, error) {
	re, err := compile(opts)
	if err != nil {
		return nil, nil, err
	}

	paths, err := ticket.ListFiles(dir)
	if err != nil {
		return nil, nil, err
	}

	results, errs := scanAll(paths, re, matchingProjects(re, opts), opts)

	slices.SortFunc(results, func(a, b Result) int { return strings.Compare(a.FilePath, b.FilePath) })
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return results, errs, nil
}

// matchingProjects returns the repository paths owned by a project whose name
// the query matches, keyed by normalized path. A body-only run has no project
// to report, so it never takes this path.
func matchingProjects(re *regexp.Regexp, opts Options) map[string]string {
	if opts.BodyOnly {
		return nil
	}
	var owned map[string]string
	for name, path := range opts.Projects {
		if !re.MatchString(name) {
			continue
		}
		if owned == nil {
			owned = make(map[string]string, len(opts.Projects))
		}
		owned[config.NormalizeRepoPath(path)] = name
	}
	return owned
}

// scanAll runs the files through a worker pool. The pool is sized to the CPU
// count: the scan is dominated by reading and matching whole files, and the
// store is thousands of them.
func scanAll(paths []string, re *regexp.Regexp, projects map[string]string, opts Options) ([]Result, []error) {
	type outcome struct {
		result *Result
		err    error
	}

	workers := min(runtime.NumCPU(), len(paths))
	if workers < 1 {
		return nil, nil
	}

	in := make(chan string)
	out := make(chan outcome)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for path := range in {
				result, err := scanFile(path, re, projects, opts)
				if result == nil && err == nil {
					continue
				}
				out <- outcome{result: result, err: err}
			}
		})
	}
	go func() {
		for _, path := range paths {
			in <- path
		}
		close(in)
		wg.Wait()
		close(out)
	}()

	var results []Result
	var errs []error
	for o := range out {
		if o.err != nil {
			errs = append(errs, o.err)
			continue
		}
		results = append(results, *o.result)
	}
	return results, errs
}

// scanFile matches one ticket file. It returns a nil result and a nil error for
// a file that does not match or is not a ticket.
func scanFile(path string, re *regexp.Regexp, projects map[string]string, opts Options) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// The prefilter: a file the query cannot match anywhere is dropped before
	// its YAML is decoded, which is what keeps a full-store scan fast. A run
	// searching project names also needs the files a matching project owns, and
	// reads their path out of the raw frontmatter rather than parsing every
	// ticket in the store to find them.
	if !re.Match(data) && !mayOwnProject(data, projects) {
		return nil, nil
	}

	// The file is parsed from the bytes already read: a second read would let
	// the line numbers and the frontmatter fields come from two versions of a
	// file the daemon is writing.
	t, err := ticket.ParseBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	t.FilePath = path
	if !ticket.IsCanonicalPath(path, t.ID) {
		return nil, nil
	}
	if !matchesFilters(t, opts) {
		return nil, nil
	}

	hits := lineHits(string(data), re, opts.BodyOnly)
	total := len(hits)
	if opts.MaxPerFile > 0 && total > opts.MaxPerFile {
		hits = hits[:opts.MaxPerFile]
	}
	if name := projects[config.NormalizeRepoPath(t.Path)]; name != "" {
		hits = append([]Hit{projectHit(name, re)}, hits...)
		total++
	}
	if len(hits) == 0 {
		return nil, nil
	}
	return &Result{Ticket: t, FilePath: path, Hits: hits, Total: total}, nil
}

// mayOwnProject reports whether the file could belong to one of the projects
// whose name matched. It reads the path out of the raw frontmatter instead of
// parsing the YAML, since this only decides whether the parse is worth doing:
// a file it cannot read a path from is let through and settled by the parse.
func mayOwnProject(data []byte, projects map[string]string) bool {
	if len(projects) == 0 {
		return false
	}
	path, ok := frontmatterPath(data)
	if !ok {
		return true
	}
	return projects[config.NormalizeRepoPath(path)] != ""
}

// frontmatterPath returns the value of the top-level "path" key of the
// frontmatter block. A file with no frontmatter block reads as having no path:
// it holds no ticket fields at all, so it cannot belong to a project. ok is
// false only for a block that never closes, where the path may still be
// somewhere the parser can find and this scan cannot.
func frontmatterPath(data []byte) (string, bool) {
	line, rest, found := bytes.Cut(data, []byte("\n"))
	if !found || string(trimCR(line)) != "---" {
		return "", true
	}
	for len(rest) > 0 {
		line, rest, _ = bytes.Cut(rest, []byte("\n"))
		line = trimCR(line)
		if string(line) == "---" {
			return "", true
		}
		if value, ok := bytes.CutPrefix(line, []byte("path:")); ok {
			return strings.Trim(strings.TrimSpace(string(value)), `"'`), true
		}
	}
	return "", false
}

func trimCR(line []byte) []byte {
	return bytes.TrimSuffix(line, []byte("\r"))
}

func projectHit(name string, re *regexp.Regexp) Hit {
	hit := Hit{Field: FieldProject, Text: name}
	if loc := re.FindStringIndex(name); loc != nil {
		hit.Start, hit.End = loc[0], loc[1]
	}
	return hit
}

func matchesFilters(t *ticket.Ticket, opts Options) bool {
	if opts.Status != "" && string(t.Status) != opts.Status {
		return false
	}
	if opts.Pipeline != "" && t.Pipeline != opts.Pipeline {
		return false
	}
	if opts.Agent != "" && t.Agent != opts.Agent {
		return false
	}
	if opts.Path != "" && config.NormalizeRepoPath(t.Path) != config.NormalizeRepoPath(opts.Path) {
		return false
	}
	return true
}

// lineHits returns one hit per matching line, holding the first match on it.
func lineHits(content string, re *regexp.Regexp, bodyOnly bool) []Hit {
	lines := strings.Split(content, "\n")
	start := 0
	if bodyOnly {
		start = bodyStart(lines)
	}

	var hits []Hit
	for i := start; i < len(lines); i++ {
		text := strings.TrimSuffix(lines[i], "\r")
		loc := re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		hits = append(hits, Hit{Line: i + 1, Text: text, Start: loc[0], End: loc[1]})
	}
	return hits
}

// bodyStart returns the index of the first body line, skipping the frontmatter
// block. A file with no frontmatter is all body.
func bodyStart(lines []string) int {
	if len(lines) == 0 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			return i + 1
		}
	}
	return 0
}
