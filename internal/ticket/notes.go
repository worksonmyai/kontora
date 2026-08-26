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

// Note is one entry in a ticket body's "## Notes" section.
//
// The byline is a "·"-separated field list: timestamp, author, id, then
// optional "re:<id>" and "edited" flags. A byline that does not match that
// grammar is a note written before this format, or by hand: its whole text
// lands in At and the note carries no id until a mutation mints one.
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

// ParseNotes reads the "## Notes" section of a ticket body. A bold line opens
// a note and everything up to the next one is its body. The section ends at
// the next heading. Body content outside the section is not touched.
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

	for i := start; i < end; i++ {
		if m := noteByline.FindStringSubmatch(strings.TrimSpace(lines[i])); m != nil {
			flush(i)
			cur = Note{start: i, hasByline: true}
			cur.At, cur.Author, cur.ID, cur.ParentID, cur.Edited = parseNoteByline(m[1])
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
	block := append([]string{formatNoteByline(note), ""}, strings.Split(text, "\n")...)
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
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "# ") || strings.HasPrefix(lines[i], "## ") {
			return start, i
		}
	}
	return start, len(lines)
}

// parseNoteByline splits a byline's inner text into fields. Anything that does
// not match the grammar exactly is returned whole as At, so a hand-written
// byline survives a round trip unchanged.
func parseNoteByline(inner string) (at, author, id, parent string, edited bool) {
	fields := strings.Split(inner, noteFieldSep)
	if len(fields) < 3 || !noteIDPattern.MatchString(fields[2]) {
		return inner, "", "", "", false
	}
	for _, f := range fields[3:] {
		switch {
		case f == "edited":
			edited = true
		case strings.HasPrefix(f, "re:") && noteIDPattern.MatchString(f[3:]):
			parent = f[3:]
		default:
			return inner, "", "", "", false
		}
	}
	return fields[0], fields[1], fields[2], parent, edited
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
	out := make([]string, 0, len(lines)-(end-start)+len(with))
	out = append(out, lines[:start]...)
	out = append(out, with...)
	return append(out, lines[end:]...)
}
