package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registry and the embedded directory have to agree in both directions: a
// topic with no file cannot be shown, and a file with no topic is unreachable.
func TestSkillTopicsMatchFiles(t *testing.T) {
	entries, err := skillDocs.ReadDir("skills")
	require.NoError(t, err)

	files := map[string]bool{}
	for _, e := range entries {
		files["skills/"+e.Name()] = true
	}

	for _, topic := range skillTopics {
		t.Run(topic.Name, func(t *testing.T) {
			assert.True(t, files[topic.File], "no embedded file for topic")
			assert.NotEmpty(t, topic.Desc)
			doc, err := skillDoc(topic.Name)
			require.NoError(t, err)
			assert.NotEmpty(t, skillSections(doc), "topic has no ## sections")
			delete(files, topic.File)
		})
	}
	assert.Empty(t, files, "embedded files no topic names")
}

func TestSkills(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    []string
		notWant []string
		wantErr []string
	}{
		{
			name: "no arguments lists the topics",
			want: []string{"cli", "tickets", "pipelines", "config"},
		},
		{
			name: "list names every topic",
			args: []string{"list"},
			want: []string{"cli", "tickets", "pipelines", "config"},
		},
		{
			name:    "list of one topic prints its headings only",
			args:    []string{"list", "cli"},
			want:    []string{"kontora new", "kontora skills"},
			notWant: []string{"--description-file"},
		},
		{
			name:    "show prints the whole topic",
			args:    []string{"show", "tickets"},
			want:    []string{"# Tickets", "## Status lifecycle", "## Relations", "## Epics"},
			notWant: []string{"No command writes it", "`parent` names an epic and is read-only"},
		},
		{
			name: "epic reference is available as a section",
			args: []string{"show", "tickets", "Epics"},
			want: []string{"## Epics", "kind: epic", "--kind epic", "--parent", "human_review", "children", "display order", "every status", "kontora view --body EPIC_ID", "not inlined",
				"epic_execution: agent", "kontora run EPIC_ID", "--status todo", "same Git repository",
				"stops only its", "supervisor", "Children already running continue", "rework", "history", "session",
				"base_branch", "does not rebase", "without\nper-operation approval", "`main`, `master`", "default branch",
				"detached HEAD", "published or shared history", "investigate questions", "safe, reversible defaults",
				"must not publish or make off-machine writes", "children, scripts, or tools", "Read-only external lookups", "mixed versions",
				"max_concurrent_epic_supervisors", "four additional agent processes", "Cancelled and archived",
				"epic_child_pipeline", "passive or agent", "standalone, without a pipeline",
				"Do not change, clear or replace a child's pipeline", "pipeline definitions, stage definitions or project pipeline defaults",
				"without `--pipeline`", "`init`, stage skipping, direct file edits or pause/retry sequences",
				"do not start it", "Report the conflict and request an operator decision", "Do not substitute another pipeline",
				"prompt instructions, not runtime enforcement", "`commit-no-push` still contains a commit stage",
				"Existing assignments are not migrated", "detail-path edits", "ephemeral preview snapshot", "older strict-YAML loaders"},
			notWant: []string{"## Body", "Run, retry, pause, skip, init", "ls --json` omits"},
		},
		{
			name: "show of a section prints that section only",
			args: []string{"show", "cli", "kontora new"},
			want: []string{
				"## kontora new", "--description-file", "--kind epic", "--parent", "--status open",
				"`--pipeline` wins", "epic_child_pipeline", "project `pipeline`, global `default_pipeline`",
				"Passive and agent epics both qualify", "ordinary-ticket parent does not qualify",
				"independent agent resolution", "lexical matching",
			},
			notWant: []string{"## kontora view"},
		},
		{
			name: "init reference preserves stored assignments and request scoped optouts",
			args: []string{"show", "cli", "kontora init"},
			want: []string{
				"explicit request, stored nonempty", "epic_child_pipeline", "global `default_pipeline`",
				"local `init` opens the pipeline picker", "errors if no pipelines are configured",
				"Pass `--pipeline none` for standalone execution",
				"resolves actual membership", "both passive and agent epics", "not durable opt-out provenance",
				"later initialization can fill it", "Agent", "resolution stays independent",
				"Existing assignments are not migrated", "detail-path changes", "retarget them explicitly",
			},
		},
		{
			name: "stage reference uses shared handoffs without changing lifecycle restrictions",
			args: []string{"show", "cli", "If you are a stage agent"},
			want: []string{
				"KONTORA_TICKET_ID", "Handoff submission", "exit 0", "non-zero",
				"`done`, `cancel`, `move`", "`pause`, `retry`, `skip`, `set-stage`", "`archive`",
			},
			notWant: []string{"## Handoff submission"},
		},
		{
			name: "handoff reference prefers native tools with safe stdin fallback",
			args: []string{"show", "cli", "Handoff submission"},
			want: []string{
				"kontora_note({text: string})", "kontora_summary({text: string})",
				"current ticket", "checkpoint", "32,768 UTF-8 bytes", "blank text", "NUL",
				"process arguments", "large or sensitive text", "stdin",
				"file-writing tool", "unique scratch files outside the repository",
				"kontora note TICKET_ID < /absolute/path/note.md",
				"kontora summary TICKET_ID < /absolute/path/summary.txt",
				"payload-bearing shell commands", "echo", "printf",
				"read the ticket before resubmitting", "Do not retry automatically",
			},
			notWant: []string{"## Common flags"},
		},
		{
			name: "note reference preserves input and flag rules",
			args: []string{"show", "cli", "kontora note"},
			want: []string{
				"[TEXT]", "Positional text remains supported", "trailing LF", "surrounding whitespace",
				"before the ticket ID", "--author NAME", "--reply-to NOTE_ID", "$KONTORA_AGENT",
				"kontora note --reply-to NOTE_ID TICKET_ID < /absolute/path/note.md",
				"Handoff submission",
			},
			notWant: []string{"before the free-form text", "echo TEXT", "## kontora summary"},
		},
		{
			name: "summary reference preserves normalization and stage capture",
			args: []string{"show", "cli", "kontora summary"},
			want: []string{
				"[TEXT]", "Positional text remains supported", "trailing LF",
				"spaces and internal newlines", "4,000 bytes", "[truncated]", "history",
				"kontora summary TICKET_ID < /absolute/path/summary.txt", "Handoff submission",
			},
			notWant: []string{"echo TEXT", "## kontora dep"},
		},
		{
			name: "cli reference puts handoff flags before the ticket ID",
			args: []string{"show", "cli"},
			want: []string{"For `note` and `summary`, put all flags before the ticket ID"},
			notWant: []string{
				"before the free-form text", "echo TEXT |", "kontora note <id> \"message\"",
			},
		},
		{
			name: "stage reference explains epic context",
			args: []string{"show", "pipelines", "Stages"},
			want: []string{"epic id, title", "not inlined", "kontora view --body EPIC_ID"},
		},
		{
			name: "update reference explains epic brief editing",
			args: []string{"show", "cli", "kontora update"},
			want: []string{"epic", "every status", "--body-file"},
		},
		{
			name:    "list reference exposes epic execution and Git fields",
			args:    []string{"show", "cli", "kontora ls"},
			want:    []string{"kind", "epic_execution", "child_order", "branch", "base_branch", "not membership", "parent"},
			notWant: []string{"omits `kind`"},
		},
		{
			name: "run reference explains explicit epic activation",
			args: []string{"show", "cli", "kontora run"},
			want: []string{
				"epic_execution: agent", "same Git repository", "human_review", "Creation and draft edits never activate",
				"without a pipeline", "changing, clearing or replacing child pipelines",
				"pipeline definitions, stage definitions or project pipeline defaults", "without `--pipeline`",
				"`init`,\nstage skipping, direct file edits or pause/retry sequences", "report the conflict",
				"leave that work unstarted", "Do not substitute another pipeline",
				"prompt instructions, not runtime enforcement", "operator commands remain available",
			},
		},
		{
			name: "pause reference limits epic cancellation to the supervisor",
			args: []string{"show", "cli", "kontora pause"},
			want: []string{"only the supervisor", "Running children continue"},
		},
		{
			name: "config reference exposes separate supervisor capacity",
			args: []string{"show", "config", "Top-level fields"},
			want: []string{"max_concurrent_epic_supervisors", "`4`", "negative values are rejected", "Requires restart", "four agent processes", "oldest eligible", "Unconfirmed", "Upgrade every daemon"},
		},
		{
			name: "project reference exposes epic child defaults and request precedence",
			args: []string{"show", "config", "projects"},
			want: []string{
				"epic_child_pipeline", "passive or agent", "1. Explicit request", "2. Stored nonempty pipeline",
				"3. Project `epic_child_pipeline`, when the resolved parent is an actual epic",
				"4. Project `pipeline`", "5. Global `default_pipeline`", "6. Standalone execution",
				"Missing or ordinary-ticket parents", "supervisors run standalone", "resolution\nare independent",
				"Existing\nassignments are not migrated", "detail-path edits", "Raw uploads and daemon startup",
				"not durable provenance", "later\ninitialization can fill the blank", "neither accepts\n`none`",
				"inheritance omits `pipeline`; standalone sends `none`", "lexical",
			},
		},
		{
			name: "config reference requires compatible strict YAML readers",
			args: []string{"show", "config"},
			want: []string{
				"Upgrade all clients and daemon binaries", "before adding\n`epic_child_pipeline`",
				"Older strict-YAML loaders reject the key", "`default_pipeline` |",
			},
		},
		{
			name: "pipeline defaults reference is available as a section",
			args: []string{"show", "pipelines", "Default selection"},
			want: []string{
				"1. Explicit request", "2. Stored nonempty pipeline",
				"3. Project `epic_child_pipeline`, for a child of any actual epic, passive or agent",
				"4. Project `pipeline`", "5. Global `default_pipeline`", "6. Standalone execution",
				"Existing assignments are not migrated", "not durable provenance", "Agent resolution stays independent",
				"before adding", "Older strict-YAML loaders reject the key", "ticket-level opt-out",
			},
			notWant: []string{"## Stages"},
		},
		{
			name: "pipeline reference preserves supervisor workflows without restricting operators",
			args: []string{"show", "pipelines", "Moving a ticket by hand"},
			want: []string{
				"changing, clearing or replacing child pipelines", "pipeline definitions, stage definitions or project pipeline defaults",
				"without `--pipeline`", "`init`, stage skipping, direct file edits or pause/retry sequences",
				"report the conflict", "operator decision and leave that work unstarted", "must not substitute",
				"prompt instructions, not runtime enforcement", "ordinary operators", "commit stage",
			},
		},
		{
			name: "reload reference pins both capacities",
			args: []string{"show", "config", "Reloading"},
			want: []string{
				"max_concurrent_agents", "max_concurrent_epic_supervisors", "restart",
				"default_pipeline", "Run settings apply to the next run", "later creation and initialization",
				"not existing assignments", "preserves stored nonempty pipelines", "invalid child-pipeline reference",
				"keeps the running config",
			},
		},
		{
			name: "pipeline reference separates worker and supervisor runs",
			args: []string{"show", "pipelines", "What a run is"},
			want: []string{"max_concurrent_epic_supervisors", "four additional agent processes", "oldest", "pool has capacity", "human_review", "rework", "history"},
		},
		{
			name:    "a section matches on a unique substring",
			args:    []string{"show", "cli", "new"},
			want:    []string{"## kontora new"},
			notWant: []string{"## kontora view"},
		},
		{
			name: "a section matches case-insensitively",
			args: []string{"show", "cli", "KONTORA NEW"},
			want: []string{"## kontora new"},
		},
		{
			name:    "the section arguments are joined",
			args:    []string{"show", "cli", "kontora", "set-stage"},
			want:    []string{"## kontora set-stage"},
			notWant: []string{"## kontora skip"},
		},
		{
			name:    "an ambiguous section names the candidates",
			args:    []string{"show", "cli", "kontora s"},
			wantErr: []string{"matches", "kontora search", "kontora skip"},
		},
		{
			name:    "a section that matches nothing says so",
			args:    []string{"show", "cli", "nowhere"},
			wantErr: []string{`no section matching "nowhere"`},
		},
		{
			name:    "an unknown topic lists the topics",
			args:    []string{"show", "nope"},
			wantErr: []string{`unknown topic "nope"`, "cli", "tickets", "pipelines", "config"},
		},
		{
			name:    "show with no topic lists the topics",
			args:    []string{"show"},
			wantErr: []string{"needs a topic", "pipelines"},
		},
		{
			name:    "an unknown subcommand is rejected",
			args:    []string{"install"},
			wantErr: []string{`unknown subcommand "install"`},
		},
		{
			name: "-h lists the topics, like every other verb prints usage",
			args: []string{"-h"},
			want: []string{"cli", "tickets", "pipelines", "config"},
		},
		{
			name: "--help lists the topics",
			args: []string{"--help"},
			want: []string{"cli", "tickets", "pipelines", "config"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := Skills(&buf, tc.args)
			if len(tc.wantErr) > 0 {
				require.Error(t, err)
				for _, want := range tc.wantErr {
					assert.Contains(t, err.Error(), want)
				}
				assert.Empty(t, buf.String())
				return
			}
			require.NoError(t, err)
			for _, want := range tc.want {
				assert.Contains(t, buf.String(), want)
			}
			for _, notWant := range tc.notWant {
				assert.NotContains(t, buf.String(), notWant)
			}
		})
	}
}

// A heading inside a fenced block is text: the topics quote YAML whose comments
// start with a hash, and a config example holding one must not split the
// section it is in.
func TestSkillSections(t *testing.T) {
	doc := strings.Join([]string{
		"# Title",
		"intro",
		"## first",
		"one",
		"```yaml",
		"## not a heading",
		"```",
		"two",
		"### nested",
		"three",
	}, "\n")

	sections := skillSections(doc)
	require.Len(t, sections, 2)
	assert.Equal(t, "first", sections[0].title)
	assert.Contains(t, sections[0].body, "## not a heading")
	assert.NotContains(t, sections[0].body, "three")
	assert.Equal(t, "nested", sections[1].title)
	assert.Contains(t, sections[1].body, "three")
}
