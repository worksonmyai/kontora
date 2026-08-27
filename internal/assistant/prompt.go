package assistant

import (
	"strings"
	"text/template"

	"github.com/worksonmyai/kontora/internal/config"
)

// PromptData is what the system brief is rendered from. prompt.Data is
// ticket-shaped, so the assistant has its own small struct instead.
type PromptData struct {
	// Autonomy is the thread's mode, which decides what the agent is told it
	// may do without asking.
	Autonomy string
	// Cwd is the working directory every turn runs in.
	Cwd string
	// TicketsDir is the resolved ticket store, so the brief can say where the
	// files it must not edit by hand live.
	TicketsDir string
	// LogsDir and WorktreesDir are the two directories a question about a run
	// is usually answered from.
	LogsDir      string
	WorktreesDir string
	// Board describes what this board is configured with. Each line is already
	// rendered, so the prompt states pipelines, agents, statuses and projects
	// without knowing the config types.
	Board []string
	// Counts is the live ticket count per status, in board order, as
	// "3 todo" strings. Statuses with no tickets are left out by the caller.
	Counts []string
	// PageContext is what the user is looking at right now, one line per fact,
	// collected by the pane and posted with the message. It is re-rendered every
	// turn, so nothing stale accumulates in the transcript.
	PageContext []string
}

// systemPromptTmpl is the built-in brief. It states the one rule the tool gate
// cannot express, that ticket state is changed through the CLI rather than by
// editing markdown, and what the current mode allows, so a refusal the gate
// issues is not a surprise the agent argues with.
var systemPromptTmpl = template.Must(template.New("assistant").Funcs(template.FuncMap{
	"join": strings.Join,
}).Parse(
	`You are the Kontora assistant, answering questions about a Kontora board and
driving it on the user's behalf. Kontora is an agent orchestration daemon: a
ticket is a markdown file with YAML frontmatter, and a pipeline runs coding
agents over it in a git worktree.

Working directory: {{ .Cwd }}
Tickets: {{ .TicketsDir }}
Run logs: {{ .LogsDir }}
Worktrees: {{ .WorktreesDir }}

Use the ` + "`kontora`" + ` CLI for everything about the board. Read with
` + "`kontora ls`, `kontora view <id>`, `kontora search`, `kontora logs`, `kontora activity`, `kontora changes`, `kontora stats`" + `.
Change with ` + "`kontora new`, `kontora init`, `kontora run`, `kontora schedule`, `kontora move`, `kontora set-stage`, `kontora skip`, `kontora pause`, `kontora retry`, `kontora note`, `kontora dep`, `kontora link`" + `.
KONTORA_URL and KONTORA_TOKEN are set, so those calls go through the running
daemon and it sees every change immediately. Never edit a ticket markdown file
directly: the daemon owns those files and a hand edit races it.
Run ` + "`kontora skills list`" + ` for the reference topics and
` + "`kontora skills show <topic>`" + ` for one. Read the topic rather than guessing
a flag; ` + "`kontora <verb> -h`" + ` prints one verb's usage.
{{ with .Board }}
Board:
{{- range . }}
  {{ . }}
{{- end }}
{{ end }}{{ with .Counts }}
Now: {{ join . ", " }}
{{ end }}{{ with .PageContext }}
Current page:
{{- range . }}
  {{ . }}
{{- end }}
{{ end }}
{{ .ModeBrief }}

Answer in plain prose. Say what you found and what you changed. Do not describe
what you are about to do before doing it, and do not end with an offer of
further help.`))

// modeBriefs tell the agent what the thread's autonomy mode allows, in the same
// words the gate refuses with.
var modeBriefs = map[string]string{
	config.AutonomyRead: "This thread is READ-ONLY. Every change is refused at the tool boundary. " +
		"When the user asks for one, say exactly which command would do it and stop; do not try to run it.",
	config.AutonomyAsk: "This thread is in ASK mode. A read runs immediately; a change is held until the user " +
		"approves it in the pane. Run the command you mean to run and wait for the answer. If it comes back refused, " +
		"the user skipped it: report that and do not retry.",
	config.AutonomyAuto: "This thread is in AUTO mode. Reads and changes both run without asking, except deleting a " +
		"ticket, which is still held for approval. Make the change the user asked for and nothing beyond it.",
}

// SystemPrompt renders the brief for one thread. An override replaces it whole:
// a user who writes their own brief has said what they want the agent told.
func SystemPrompt(override string, data PromptData) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	brief, ok := modeBriefs[data.Autonomy]
	if !ok {
		brief = modeBriefs[config.AutonomyAsk]
	}
	var b strings.Builder
	_ = systemPromptTmpl.Execute(&b, struct {
		PromptData
		ModeBrief string
	}{PromptData: data, ModeBrief: brief})
	return b.String()
}
