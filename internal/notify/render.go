package notify

import "strings"

// maxMessage caps the rendered text in UTF-16 code units, which is the unit
// Telegram counts: it rejects a sendMessage body over 4096 of them, and an
// emoji is two. A run summary has no length limit of its own.
const maxMessage = 3500

// render is the shared plain-text body every channel sends. Telegram escapes
// the result for HTML; the other two send it as it is.
func render(e Event) string {
	f := e.Fields
	head := e.TicketID + ": " + e.To
	if e.From != "" {
		head += " (was " + e.From + ")"
	}
	lines := []string{head}
	if f.Title != "" {
		lines = append(lines, f.Title)
	}
	var meta []string
	for _, kv := range []struct{ k, v string }{
		{"project", f.Project},
		{"stage", f.Stage},
		{"branch", f.Branch},
	} {
		if kv.v != "" {
			meta = append(meta, kv.k+": "+kv.v)
		}
	}
	if len(meta) > 0 {
		lines = append(lines, strings.Join(meta, " | "))
	}
	for _, s := range []string{f.Question, f.LastError, f.Summary} {
		if s != "" {
			lines = append(lines, s)
		}
	}
	return truncate(strings.Join(lines, "\n"))
}

func truncate(s string) string {
	units := 0
	for i, r := range s {
		n := 1
		if r > 0xFFFF {
			n = 2
		}
		if units+n > maxMessage {
			return s[:i] + "\n(truncated)"
		}
		units += n
	}
	return s
}
