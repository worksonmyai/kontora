package ticket

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	// ErrNoteNotFound names a note id that no note in the ticket carries.
	ErrNoteNotFound = errors.New("note not found")
	// ErrNoteNested is a reply to a note that is itself a reply. The note
	// format allows one level, so a second has nowhere to hang.
	ErrNoteNested = errors.New("a reply cannot be replied to")
	// ErrNoteEmpty is a note with no text.
	ErrNoteEmpty = errors.New("note text is empty")
	// ErrNoteAuthor is an author that would break the byline grammar.
	ErrNoteAuthor = errors.New("invalid note author")
	// ErrNoteUnaddressable is a hand-written note whose byline cannot carry an
	// id without changing what it says.
	ErrNoteUnaddressable = errors.New("note byline cannot carry an id")
)

// SystemAuthor is the name Kontora signs its own notes with: the pause reason
// and a failed hook's error. It is not an agent name, so it can never collide
// with one the config defines.
const SystemAuthor = "kontora"

const (
	noteIDChars = "abcdefghijklmnopqrstuvwxyz0123456789"
	noteIDLen   = 4
	// noteFieldSep separates the byline's fields. It is the only thing holding
	// the grammar together, so AddNote refuses an author that contains it.
	noteFieldSep = " · "
)

// noteByline matches the bold line that opens a note.
var noteByline = regexp.MustCompile(`^\*\*(.+)\*\*$`)

var noteIDPattern = regexp.MustCompile(`^[a-z0-9]{4}$`)
var noteDatePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}`)
var noteHeading = regexp.MustCompile(`^( {0,3})(#{1,6})([\t ].*|)$`)
var noteListMarker = regexp.MustCompile(`^(?:[-+*]|[0-9]{1,9}[.)])([ \t]+)`)
var noteThematicBreak = regexp.MustCompile(`^(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$`)
var noteHTMLBlock = regexp.MustCompile(`(?i)^<(?:[a-z].*>|!--)`)

// Note is one entry in a ticket body's "## Notes" section.
//
// The byline is a "·"-separated field list: timestamp, author, id, then
// optional "re:<id>" and "edited" flags. Date-led legacy bylines keep their
// whole text in At, with no id until a mutation mints one.
type Note struct {
	// ID is the minted 4-character id, or "#<index>" for a note whose byline
	// carries none. Both forms address the note in the mutating methods.
	ID       string
	At       string
	Author   string
	ParentID string
	Edited   bool
	Text     string

	// start and end bound the note's lines in the body, end exclusive, so one
	// note can be rewritten without rebuilding the section around it.
	start, end int
	// hasByline is false for text that precedes the first byline in the
	// section, which is a note with a body and nothing else.
	hasByline bool
}

// AddNoteOptions is the input to AddNote. At defaults to the current time.
type AddNoteOptions struct {
	Text     string
	Author   string
	ParentID string
	At       time.Time
}

// ParseNotes reads the "## Notes" section using date-led or structured bylines
// outside fenced code. A column-zero "# " or "## " line outside fences ends it.
// Body content outside the section is not touched.
func ParseNotes(body string) []Note {
	lines := strings.Split(body, "\n")
	start, end := notesSection(lines)
	if start < 0 {
		return nil
	}

	var notes []Note
	cur := Note{start: start}
	var buf []string
	flush := func(stop int) {
		cur.end = stop
		if text := strings.TrimSpace(strings.Join(buf, "\n")); text != "" {
			cur.Text = text
			notes = append(notes, cur)
		}
		buf = nil
	}

	var fence noteFence
	for i := start; i < end; i++ {
		if inner, ok := recognizedNoteByline(lines[i]); !fence.consume(lines[i]) && ok {
			flush(i)
			cur = parseNoteByline(inner)
			cur.start, cur.hasByline = i, true
			continue
		}
		buf = append(buf, lines[i])
	}
	flush(end)

	for i := range notes {
		if notes[i].ID == "" {
			notes[i].ID = fmt.Sprintf("#%d", i)
		}
	}
	return notes
}

// AddNote appends a note to the "## Notes" section, creating the section when
// the body has none, and returns it with its minted id. A ParentID that names
// a legacy note mints that note's id first.
func (t *Ticket) AddNote(opts AddNoteOptions) (Note, error) {
	text := strings.TrimSpace(opts.Text)
	if text == "" {
		return Note{}, ErrNoteEmpty
	}
	text = normalizeNoteText(text)
	if err := validNoteAuthor(opts.Author); err != nil {
		return Note{}, err
	}

	parent := ""
	if opts.ParentID != "" {
		id, _, err := t.EnsureNoteID(opts.ParentID)
		if err != nil {
			return Note{}, err
		}
		for _, n := range ParseNotes(t.Body) {
			if n.ID == id && n.ParentID != "" {
				return Note{}, ErrNoteNested
			}
		}
		parent = id
	}

	id, err := mintNoteID(ParseNotes(t.Body))
	if err != nil {
		return Note{}, err
	}
	at := opts.At
	if at.IsZero() {
		at = time.Now()
	}
	note := Note{
		ID:       id,
		At:       at.UTC().Format(time.RFC3339),
		Author:   opts.Author,
		ParentID: parent,
		Text:     text,
	}

	// Appended to the end of the section, not the end of the body: a ticket
	// whose "## Notes" is followed by another heading would otherwise take the
	// note into that section, where ParseNotes cannot see it again.
	lines := strings.Split(t.Body, "\n")
	start, end := notesSection(lines)
	if start < 0 {
		if strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "## Notes", "")
		start, end = len(lines), len(lines)
	}
	var fence noteFence
	for _, line := range lines[start:end] {
		fence.consume(line)
	}
	var block []string
	// Full-body writes can leave a fence open across the append boundary.
	if fence.length > 0 {
		block = append(block, fence.closingLine(), "")
	}
	block = append(block, formatNoteByline(note), "")
	block = append(block, strings.Split(text, "\n")...)
	block = append(block, "")
	if end > start && strings.TrimSpace(lines[end-1]) != "" {
		block = append([]string{""}, block...)
	}
	t.SetBody(strings.Join(replaceLines(lines, end, end, block), "\n"))
	return note, nil
}

// EditNote replaces a note's text in place and flags its byline as edited.
// Every other line of the section is left as it was.
func (t *Ticket) EditNote(ref, text string) (Note, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Note{}, ErrNoteEmpty
	}
	text = normalizeNoteText(text)
	id, _, err := t.EnsureNoteID(ref)
	if err != nil {
		return Note{}, err
	}

	notes := ParseNotes(t.Body)
	i := noteIndexOf(notes, id)
	if i < 0 {
		return Note{}, ErrNoteNotFound
	}
	note := notes[i]
	note.Edited = true
	note.Text = text

	block := append([]string{formatNoteByline(note), ""}, strings.Split(text, "\n")...)
	t.SetBody(strings.Join(replaceLines(strings.Split(t.Body, "\n"), note.start, note.end, append(block, "")), "\n"))
	return note, nil
}

// DeleteNote removes a note from the section. A note that has replies takes
// them with it: the format allows one level of nesting, so an orphaned reply
// has nowhere to go.
func (t *Ticket) DeleteNote(ref string) error {
	notes := ParseNotes(t.Body)
	i := noteIndexOf(notes, ref)
	if i < 0 {
		return ErrNoteNotFound
	}

	drop := []int{i}
	if target := notes[i]; !strings.HasPrefix(target.ID, "#") {
		for j, n := range notes {
			if j != i && n.ParentID == target.ID {
				drop = append(drop, j)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(drop)))

	lines := strings.Split(t.Body, "\n")
	for _, j := range drop {
		lines = replaceLines(lines, notes[j].start, notes[j].end, nil)
	}
	t.SetBody(strings.Join(lines, "\n"))
	return nil
}

// EnsureNoteID resolves a note reference to a stable id, minting one into the
// byline when the note carries none. It reports whether the body was rewritten,
// so a caller that only reads can tell an untouched ticket from a rewritten one.
func (t *Ticket) EnsureNoteID(ref string) (string, bool, error) {
	notes := ParseNotes(t.Body)
	i := noteIndexOf(notes, ref)
	if i < 0 {
		return "", false, ErrNoteNotFound
	}
	note := notes[i]
	if !strings.HasPrefix(note.ID, "#") {
		return note.ID, false, nil
	}
	if strings.Contains(note.At, noteFieldSep) || strings.Contains(note.At, "**") {
		return "", false, fmt.Errorf("%w: %q", ErrNoteUnaddressable, note.At)
	}

	id, err := mintNoteID(notes)
	if err != nil {
		return "", false, err
	}
	note.ID = id

	lines := strings.Split(t.Body, "\n")
	if note.hasByline {
		lines[note.start] = formatNoteByline(note)
	} else {
		lines = replaceLines(lines, note.start, note.start, []string{formatNoteByline(note), ""})
	}
	t.SetBody(strings.Join(lines, "\n"))
	return id, true, nil
}

// notesSection returns the half-open line range of the "## Notes" section's
// content, or (-1, -1) when the body has no such section.
func notesSection(lines []string) (int, int) {
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Notes" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	var fence noteFence
	for i := start; i < len(lines); i++ {
		if fence.consume(lines[i]) {
			continue
		}
		if strings.HasPrefix(lines[i], "# ") || strings.HasPrefix(lines[i], "## ") {
			return start, i
		}
	}
	return start, len(lines)
}

func recognizedNoteByline(line string) (string, bool) {
	m := noteByline.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", false
	}
	first, _, _ := strings.Cut(m[1], "·")
	note := parseNoteByline(m[1])
	return m[1], noteDatePattern.MatchString(strings.TrimSpace(first)) || note.ID != ""
}

type noteFence struct {
	marker      byte
	length      int
	indent      int
	listIndents []int
	paragraph   bool
}

// consume includes both delimiter lines in the fenced region. List indentation
// is relative to the item's content, not the start of the source line.
func (f *noteFence) consume(line string) bool {
	trimmed, indent := noteIndent(line, 0)
	if strings.TrimSpace(trimmed) == "" {
		f.paragraph = false
		return f.length > 0
	}
	lazy := f.paragraph && f.length == 0 && !noteParagraphBreak(trimmed)
	f.paragraph = false
	for !lazy && len(f.listIndents) > 0 && indent < f.listIndents[len(f.listIndents)-1] {
		f.listIndents = f.listIndents[:len(f.listIndents)-1]
		f.length = 0
	}
	base := 0
	if len(f.listIndents) > 0 {
		base = f.listIndents[len(f.listIndents)-1]
	}
	lazyAllowed := true
	if f.length == 0 {
		for indent-base <= 3 {
			m := noteListMarker.FindStringSubmatch(trimmed)
			if m == nil {
				break
			}
			width := len(m[0]) - len(m[1])
			_, contentIndent := noteIndent(m[1], indent+width)
			base = contentIndent
			// Match marked's refusal of lazy continuation after four-space padding.
			lazyAllowed = contentIndent-indent-width < 4
			// Overpadded markers use one space for the list's content indent.
			if contentIndent-indent-width > 4 {
				base = indent + width + 1
			}
			indent = contentIndent
			f.listIndents = append(f.listIndents, base)
			trimmed = trimmed[len(m[0]):]
		}
	}
	if indent-base > 3 || trimmed == "" {
		return f.length > 0
	}
	marker := trimmed[0]
	n := 0
	for n < len(trimmed) && trimmed[n] == marker {
		n++
	}
	if f.length > 0 {
		if marker == f.marker && n >= f.length && strings.TrimSpace(trimmed[n:]) == "" {
			f.length = 0
		}
		return true
	}
	if (marker == '`' || marker == '~') && n >= 3 && (marker != '`' || !strings.Contains(trimmed[n:], "`")) {
		f.marker, f.length, f.indent = marker, n, indent
		return true
	}
	f.paragraph = lazyAllowed && !noteParagraphBreak(trimmed)
	return false
}

func noteParagraphBreak(line string) bool {
	return noteHeading.MatchString(line) || noteListMarker.MatchString(line) || noteThematicBreak.MatchString(line) || noteHTMLBlock.MatchString(line) ||
		strings.HasPrefix(line, ">") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

func noteIndent(line string, column int) (string, int) {
	i := 0
	for i < len(line) {
		switch line[i] {
		case ' ':
			column++
		case '\t':
			column += 4 - column%4
		default:
			return line[i:], column
		}
		i++
	}
	return "", column
}

func (f *noteFence) closingLine() string {
	return strings.Repeat(" ", f.indent) + strings.Repeat(string(f.marker), f.length)
}

func normalizeNoteText(text string) string {
	lines := strings.Split(text, "\n")
	shallowest := 3
	var fence noteFence
	for _, line := range lines {
		if fence.consume(line) {
			continue
		}
		if m := noteHeading.FindStringSubmatch(line); m != nil {
			shallowest = min(shallowest, len(m[2]))
		}
	}
	fence = noteFence{}
	for i, line := range lines {
		if fence.consume(line) {
			continue
		}
		if inner, ok := recognizedNoteByline(line); ok {
			lines[i] = strings.Replace(line, "**"+inner+"**", "__"+inner+"__", 1)
		} else if m := noteHeading.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + strings.Repeat("#", min(6, len(m[2])+3-shallowest)) + m[3]
		}
	}
	if fence.length > 0 {
		lines = append(lines, fence.closingLine())
	}
	return strings.Join(lines, "\n")
}

// NotesTail returns the raw Notes section, or the last n notes and all later
// sections. A nonpositive n or a count above the note count includes the heading.
func NotesTail(body string, n int) string {
	if n > 0 {
		notes := ParseNotes(body)
		if n <= len(notes) {
			return strings.Join(strings.Split(body, "\n")[notes[len(notes)-n].start:], "\n")
		}
	}
	_, tail := SplitDescription(body)
	return tail
}

// parseNoteByline splits a byline's inner text into fields. Anything that does
// not match the grammar exactly is returned whole as At, so a hand-written
// byline survives a round trip unchanged.
func parseNoteByline(inner string) Note {
	fields := strings.Split(inner, noteFieldSep)
	if len(fields) < 3 || !noteIDPattern.MatchString(fields[2]) {
		return Note{At: inner}
	}
	note := Note{At: fields[0], Author: fields[1], ID: fields[2]}
	for _, f := range fields[3:] {
		switch {
		case f == "edited":
			note.Edited = true
		case strings.HasPrefix(f, "re:") && noteIDPattern.MatchString(f[3:]):
			note.ParentID = f[3:]
		default:
			return Note{At: inner}
		}
	}
	return note
}

// formatNoteByline writes the byline for a note that carries an id. The author
// field keeps its place when it is empty, so the id stays third: a legacy note
// that gains an id has no author to name.
func formatNoteByline(n Note) string {
	b := n.At + noteFieldSep + n.Author + noteFieldSep + n.ID
	if n.ParentID != "" {
		b += noteFieldSep + "re:" + n.ParentID
	}
	if n.Edited {
		b += noteFieldSep + "edited"
	}
	return "**" + b + "**"
}

func validNoteAuthor(author string) error {
	if strings.ContainsAny(author, "·\n\r") || strings.Contains(author, "**") {
		return fmt.Errorf("%w: %q", ErrNoteAuthor, author)
	}
	return nil
}

func noteIndexOf(notes []Note, ref string) int {
	for i, n := range notes {
		if n.ID == ref {
			return i
		}
	}
	return -1
}

// mintNoteID draws a 4-character id that no note in the section already uses.
func mintNoteID(notes []Note) (string, error) {
	taken := make(map[string]bool, len(notes))
	for _, n := range notes {
		taken[n.ID] = true
	}
	for range 16 {
		b := make([]byte, noteIDLen)
		for i := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(noteIDChars))))
			if err != nil {
				return "", err
			}
			b[i] = noteIDChars[n.Int64()]
		}
		if id := string(b); !taken[id] {
			return id, nil
		}
	}
	return "", errors.New("note id collision after retry")
}

// replaceLines swaps lines[start:end] for with, leaving the input untouched.
func replaceLines(lines []string, start, end int, with []string) []string {
	out := make([]string, 0)
	out = append(out, lines[:start]...)
	out = append(out, with...)
	return append(out, lines[end:]...)
}
