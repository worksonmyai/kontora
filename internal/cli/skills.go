package cli

import (
	"embed"
	"fmt"
	"io"
	"strings"
)

// The topics under skills/ are agent-facing reference docs, written flags and
// arguments first. They deliberately restate what docs/ already says: docs/ is
// the mkdocs site written for a person, and an assistant thread runs with its
// working directory set to the ticket store, so nothing in the repository is
// reachable from a turn. A change to a verb's flags belongs in both.

//go:embed skills/*.md
var skillDocs embed.FS

// skillTopic is one reference document.
type skillTopic struct {
	Name string
	Desc string
	File string
}

// skillTopics is the registry. TestSkillTopicsMatchFiles holds it and the
// embedded directory together, so a new file cannot be orphaned.
var skillTopics = []skillTopic{
	{"cli", "Every kontora verb: arguments, flags and what it prints", "skills/cli.md"},
	{"tickets", "The ticket file: frontmatter fields, statuses and relations", "skills/tickets.md"},
	{"pipelines", "Stages, pipelines, runs, and where logs and worktrees live", "skills/pipelines.md"},
	{"config", "The config fields a question about a board is answered from", "skills/config.md"},
}

// Skills prints the built-in reference topics. It reads neither the config file
// nor the ticket store, so it answers in remote mode and before `kontora setup`.
func Skills(w io.Writer, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	// The brief and skills/cli.md both tell the agent that `kontora <verb> -h`
	// always works, and this verb has no flag set to print usage from.
	case "list", "-h", "--help":
		if len(args) == 0 {
			return skillList(w)
		}
		return skillHeadingList(w, args[0])
	case "show":
		if len(args) == 0 {
			return fmt.Errorf("kontora skills show needs a topic\n\n%s", skillTopicLines())
		}
		return skillShow(w, args[0], strings.Join(args[1:], " "))
	default:
		return fmt.Errorf("unknown subcommand %q (want list or show)", sub)
	}
}

// skillList prints the topics, one line each.
func skillList(w io.Writer) error {
	if _, err := fmt.Fprint(w, skillTopicLines()); err != nil {
		return err
	}
	_, err := fmt.Fprint(w, "\nRun `kontora skills show <topic>` for one, or `kontora skills show <topic> SECTION` for one section of it.\n")
	return err
}

// skillHeadingList prints one topic's section headings.
func skillHeadingList(w io.Writer, topic string) error {
	doc, err := skillDoc(topic)
	if err != nil {
		return err
	}
	for _, s := range skillSections(doc) {
		if _, err := fmt.Fprintln(w, s.title); err != nil {
			return err
		}
	}
	return nil
}

// skillShow prints a whole topic, or the one section named.
func skillShow(w io.Writer, topic, section string) error {
	doc, err := skillDoc(topic)
	if err != nil {
		return err
	}
	if section == "" {
		_, err := io.WriteString(w, doc)
		return err
	}
	body, err := skillMatchSection(doc, section)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, body)
	return err
}

// skillDoc reads one topic's markdown. An unknown name lists the topics rather
// than only saying it is unknown: the caller has to guess otherwise.
func skillDoc(topic string) (string, error) {
	for _, t := range skillTopics {
		if t.Name == topic {
			b, err := skillDocs.ReadFile(t.File)
			if err != nil {
				return "", fmt.Errorf("reading topic %q: %w", topic, err)
			}
			return string(b), nil
		}
	}
	return "", fmt.Errorf("unknown topic %q\n\n%s", topic, skillTopicLines())
}

// skillTopicLines renders the registry as the listing both `list` and the
// unknown-topic error print.
func skillTopicLines() string {
	var b strings.Builder
	b.WriteString("Topics:\n")
	width := 0
	for _, t := range skillTopics {
		width = max(width, len(t.Name))
	}
	for _, t := range skillTopics {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, t.Name, t.Desc)
	}
	return b.String()
}

// skillSection is one `##` or `###` block of a topic.
type skillSection struct {
	title string
	body  string
}

// skillSections splits a topic on its `##` and `###` headings. A heading inside
// a fenced code block is text, not a heading: the topics quote YAML whose
// comments start with a hash.
func skillSections(doc string) []skillSection {
	var out []skillSection
	var body strings.Builder
	fenced := false
	flush := func() {
		if len(out) > 0 {
			out[len(out)-1].body = strings.TrimRight(body.String(), "\n") + "\n"
		}
		body.Reset()
	}
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if !fenced && (strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ")) {
			flush()
			out = append(out, skillSection{title: strings.TrimSpace(strings.TrimLeft(line, "#"))})
		}
		if len(out) > 0 {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	flush()
	return out
}

// skillMatchSection resolves a section name: an exact heading first, then a
// unique case-insensitive substring. Ambiguity is an error naming every
// candidate, because printing one of them would look like the only answer.
func skillMatchSection(doc, want string) (string, error) {
	sections := skillSections(doc)
	for _, s := range sections {
		if strings.EqualFold(s.title, want) {
			return s.body, nil
		}
	}
	var hits []skillSection
	lower := strings.ToLower(want)
	for _, s := range sections {
		if strings.Contains(strings.ToLower(s.title), lower) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].body, nil
	case 0:
		return "", fmt.Errorf("no section matching %q", want)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d sections:\n", want, len(hits))
		for _, s := range hits {
			fmt.Fprintf(&b, "  %s\n", s.title)
		}
		return "", fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
	}
}
