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
			sections := skillSections(doc)
			require.NotEmpty(t, sections, "topic has no ## sections")
			compatibilityCount := 0
			for _, section := range sections {
				if strings.EqualFold(section.title, "Compatibility") {
					compatibilityCount++
				}
			}
			assert.Equal(t, 1, compatibilityCount, "topic must have exactly one Compatibility section")
			assert.True(t, strings.HasPrefix(sections[len(sections)-1].body, "## Compatibility\n"), "the final section must be ## Compatibility")
			delete(files, topic.File)
		})
	}
	assert.Empty(t, files, "embedded files no topic names")
}

// Every `-h` names its verb's section, so the section must exist and name the
// verb's own flags. Automation children are covered below.
func TestCLISkillCoversCommandTable(t *testing.T) {
	for _, cmd := range Commands {
		if cmd.Name == "automation" {
			continue
		}
		section := "kontora " + cmd.Name
		t.Run(cmd.Name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, Skills(&out, []string{"show", "cli", section}))
			assert.True(t, strings.HasPrefix(out.String(), "## "+section+"\n"))
			for _, f := range cmd.Flags {
				assert.Contains(t, out.String(), dashed(f.Name), "%s does not document %s", section, dashed(f.Name))
			}
		})
	}
}

func TestAutomationSkillReference(t *testing.T) {
	var parent Command
	for _, cmd := range Commands {
		if cmd.Name == "automation" {
			parent = cmd
		}
	}
	require.NotEmpty(t, parent.Subcommands)
	var headings bytes.Buffer
	require.NoError(t, Skills(&headings, []string{"list", "cli"}))
	for _, cmd := range append([]Command{{Name: ""}}, parent.Subcommands...) {
		section := strings.TrimSpace("kontora automation " + cmd.Name)
		t.Run(section, func(t *testing.T) {
			assert.Contains(t, headings.String(), section+"\n")
			var out bytes.Buffer
			require.NoError(t, Skills(&out, []string{"show", "cli", section}))
			assert.True(t, strings.HasPrefix(out.String(), "## "+section+"\n"))
			assert.NotContains(t, out.String(), "\n## ")
			for _, f := range cmd.Flags {
				if f.Name != "operator-token" && f.Name != "json" {
					assert.Contains(t, out.String(), dashed(f.Name), section)
				}
			}
		})
	}
	checks := map[string][]string{
		"kontora automation":           {"daemon API", "no offline fallback", "exact configuration keys only", "Display names and abbreviated IDs", "original full ID", "--operator-token", "KONTORA_OPERATOR_TOKEN", "independently", "--json", "pagination envelopes", "`[]`", "`null`", "exits nonzero", "`ls --json` prints the availability envelope when HTTP 200 reports unavailable storage", "HTTP errors produce diagnostics and a nonzero exit without JSON stdout"},
		"kontora automation new":       {"disabled", "`-` reads stdin", "request ID to stderr", "do not retry automatically"},
		"kontora automation update":    {"preserves omitted", "--enabled=false", "--model ''", "positive expected revision", "without an automatic retry"},
		"kontora automation duplicate": {"disabled", "unchanged source inputs", "conflict"},
		"kontora automation delete":    {"--yes", "-f", "Historical runs and logs remain", "`id`", "`deleted`"},
		"kontora automation pause":     {"without\ncancelling accepted runs", "Manual runs remain available"},
		"kontora automation run":       {"original identity pair", "original full automation ID, never a key", "not proof of completion"},
		"kontora automation stop":      {"exact\npositive run number", "never switches", "does not prove termination"},
		"kontora automation runs":      {"`0`", "`20`", "`1` through `200`", "does not promise a snapshot"},
		"kontora automation logs":      {"--raw", "--json", "mutually exclusive", "not original terminal capture", "after deletion"},
		"kontora automation preview":   {"four future occurrences", "daemon's\nclock", "no offline mode"},
		"kontora automation terminal":  {"server-resolved", "does not attach", "local tmux target"},
		"kontora automation attach":    {"read-only", "exact positive run number", "live definition", "no `--json`", "never run attachment from an agent"},
	}
	for section, want := range checks {
		t.Run(section+" semantics", func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, Skills(&out, []string{"show", "cli", section}))
			for _, text := range want {
				assert.Contains(t, out.String(), text)
			}
		})
	}
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
			name:    "list omits the disabled orchestrator topic",
			args:    []string{"list"},
			want:    []string{"cli", "tickets", "pipelines", "config"},
			notWant: []string{"orchestrator"},
		},
		{
			name:    "list of one topic prints its headings only",
			args:    []string{"list", "cli"},
			want:    []string{"kontora new", "kontora skills", "Polling for ticket attention"},
			notWant: []string{"--description-file"},
		},
		{
			name: "show prints the whole topic",
			args: []string{"show", "tickets"},
			want: []string{"# Tickets", "## Status lifecycle", "## Relations", "## Epics"},
			notWant: []string{
				"No command writes it", "`parent` names an epic and is read-only",
				"Run, retry, pause, skip, init", "ls --json` omits", "must not publish or make off-machine writes",
			},
		},
		{
			name: "epic reference is available as a section",
			args: []string{"show", "tickets", "Epics"},
			want: []string{"## Epics", "kind: epic", "--kind epic", "--parent", "human_review", "children", "display order", "every status", "kontora view --body EPIC_ID", "not inlined",
				"epic_execution: agent", "kontora run EPIC_ID", "--status todo", "same Git repository",
				"stops only its", "supervisor", "Children already running continue", "rework", "history", "session",
				"base_branch", "max_concurrent_epic_supervisors", "four additional agent processes", "Cancelled and archived",
				"epic_child_pipeline", "standalone, without a pipeline", "ephemeral preview snapshot",
				`kontora skills show pipelines "Default selection"`, "may name another repository for a passive epic",
				"Remote and API creation still require a daemon-host path", "kontora skills show tickets Compatibility",
				`kontora skills show tickets "Epic supervision"`},
			notWant: []string{
				"## Body", "## Epic supervision", "per-operation approval", "may merge child branches", "Publication requires explicit user approval",
				"Existing assignments are not migrated", "integration worktree",
			},
		},
		{
			name: "epic supervision reference names the authority and its limits",
			args: []string{"show", "tickets", "Epic supervision"},
			want: []string{"## Epic supervision", "The run prompt is the authority", "kontora skills show tickets Epics",
				"base_branch", "does not rebase", "without\nper-operation approval", "`main`, `master`", "default branch",
				"detached HEAD", "If any check fails or ownership is unclear, ask before writing.",
				"published or shared history", "investigate questions", "safe, reversible defaults",
				"Publication requires explicit user approval", "specific off-machine action", "children, scripts, or tools",
				"Reuse", "recorded approval", "Read-only lookups",
				"Use approved CI", "intermediate stages", "exact commit or tree", "verification handoff, not acceptance",
				"Do not change, clear or replace a child's pipeline", "pipeline definitions, stage definitions or project pipeline defaults",
				"without `--pipeline`", "`init`, stage skipping, direct file edits or pause/retry sequences",
				"do not start it", "Record the conflict and request an operator decision", "Do not substitute another pipeline",
				"prompt instructions, not runtime enforcement", "`commit-no-push` still contains a commit stage",
				"may merge child branches", "A child may merge the epic's branch", "Retry requires explicit operator action",
				"the epic's stored repository path, not your integration worktree",
			},
			notWant: []string{"## Body", "## Epics", "Resolve detached HEAD", "Report the conflict", "must not publish or make off-machine writes"},
		},
		{
			name: "show of a section prints that section only",
			args: []string{"show", "cli", "kontora new"},
			want: []string{
				"## kontora new", "--description-file", "--kind epic", "--parent", "--status open",
				"`--pipeline` wins", "epic_child_pipeline", "project `pipeline`, global `default_pipeline`",
				"Passive and agent epics both qualify", "ordinary-ticket parent does not qualify",
				"independent agent resolution", "lexical matching",
				"kontora skills show config Compatibility` before rolling out notification defaults or persistent Off",
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
			name: "list reference exposes epic, Git and waiting fields",
			args: []string{"show", "cli", "kontora ls"},
			want: []string{
				"kind", "epic_execution", "child_order", "branch", "base_branch", "not membership", "parent",
				"--attention", "waiting_for_input", "waiting_since", "waiting_tool", "waiting_question",
			},
			notWant: []string{"omits `kind`"},
		},
		{
			name: "polling recipe is available as a section",
			args: []string{"show", "cli", "Polling for ticket attention"},
			want: []string{
				"## Polling for ticket attention", "kontora ls --json --attention", "read-only", "read mode without approval",
				"daemon's primary store", "human_review", "waiting_for_input: true", "`[]`",
				"Only Pi runs report live questions", "A parked workflow alone does not set the flag",
				"Plain local `ls --json` reads ticket files and cannot see live questions",
				"`--url` or `KONTORA_URL`", "`--token` or `KONTORA_TOKEN`",
				"matching local daemon", "exits non-zero without a list",
				"short-lived changes between reads", "hooks", "kontora skills show cli 'kontora automation'",
			},
			notWant: []string{"## kontora search", "curlie", "jq"},
		},
		{
			name: "HTTP API reference covers authentication and reads",
			args: []string{"show", "cli", "HTTP API"},
			want: []string{
				"first credential present:", "`Authorization: Bearer TOKEN`, the `kontora_token` cookie, then `?token=TOKEN`",
				"`/?token=TOKEN` in the browser to set or refresh the cookie", "`GET /health` is public",
				"GET /api/tickets`", "board columns or with an active execution", "`?all=true` for all primary tickets",
				"GET /api/tickets/{id}`", "GET /api/tickets/{id}/logs", "GET /api/tickets/{id}/activity",
				"GET /api/tickets/{id}/changes", "GET /api/tickets/{id}/chain", "GET /api/tickets/{id}/cost",
				"finished, priced runs, with counts of tracked and priced runs",
				"GET /api/tickets/archived", "GET /api/config", "daemon identity fields when available",
				"GET /api/stats", "GET /api/events", "without an initial snapshot or replay", "after connecting or reconnecting",
				"waiting_for_input", "waiting_since", "waiting_tool", "waiting_question", "Use the CLI to make changes",
			},
			notWant: []string{"## kontora ls"},
		},
		{
			name: "run reference explains explicit epic activation",
			args: []string{"show", "cli", "kontora run"},
			want: []string{
				"epic_execution: agent", "same Git repository", "human_review", "Creation and draft edits never activate",
				"without a pipeline", `kontora skills show tickets "Epic supervision"`, "workflow rules",
			},
			notWant: []string{"report the conflict", "changing, clearing or replacing child pipelines"},
		},
		{
			name: "pause reference limits epic cancellation to the supervisor",
			args: []string{"show", "cli", "kontora pause"},
			want: []string{"only the supervisor", "Running children continue"},
		},
		{
			name: "config reference exposes live worker and supervisor capacity",
			args: []string{"show", "config", "Top-level fields"},
			want: []string{
				"max_concurrent_agents", "max_concurrent_epic_supervisors", "`4`", "negative values are rejected",
				"Reloads live without stopping running workers", "Reloads live without stopping running supervisors",
				"four agent processes", "oldest eligible", "Unconfirmed", "kontora skills show config Compatibility` before activating epics",
				"auto_pick_up", "explicit runs, pipeline continuations, recovery, or scheduled automations",
			},
			notWant: []string{"Requires restart"},
		},
		{
			name: "project reference links to pipeline precedence",
			args: []string{"show", "config", "projects"},
			want: []string{
				"epic_child_pipeline", "passive or agent", `kontora skills show pipelines "Default selection"`,
				"neither accepts\n`none`", "inheritance omits `pipeline`; standalone sends `none`", "lexical",
				"ticket-level override for every pipeline step", "Leave it unset to keep each step's configured agent",
				"kontora skills show config Compatibility` before adding `epic_child_pipeline`",
			},
			notWant: []string{"1. Explicit request", "2. Stored nonempty pipeline"},
		},
		{
			name:    "ticket fields link to pipeline precedence",
			args:    []string{"show", "tickets", "User-set fields"},
			want:    []string{`kontora skills show pipelines "Default selection"`},
			notWant: []string{"stored nonempty pipeline"},
		},
		{
			name: "ticket waits describe status and controls",
			args: []string{"show", "tickets", "Durable workflow waits"},
			want: []string{"status: paused", "graph_state.wait", "waiting_for_input", "Cannot bypass the wait", "kontora skills show pipelines 'Durable waits'"},
		},
		{
			name: "config waits describe fields and template data",
			args: []string{"show", "config", "Durable waits"},
			want: []string{
				"wake.max", "check_timeout", "type: wait", "{{ with .Wake }}", "kontora skills show pipelines 'Durable waits'",
				"`.Receipt` is optional", "absent on timer and probe wakes",
			},
		},
		{
			name: "notification configuration points to rollout guidance",
			args: []string{"show", "config", "notifications"},
			want: []string{"kontora skills show config Compatibility` before rolling out notification defaults or persistent Off"},
		},
		{
			name: "config reference explains config selection",
			args: []string{"show", "config"},
			want: []string{
				"`default_pipeline` |",
				"`--config` selects the file explicitly", "`$KONTORA_CONFIG`",
				"when `XDG_CONFIG_HOME` is unset", "without merging",
			},
		},
		{
			name: "agent reference requires explicit executable wrappers",
			args: []string{"show", "config", "agents"},
			want: []string{
				"resolves `binary` as an executable", "passes `args` literally",
				"Shell functions and aliases do not apply", "Configure wrappers explicitly",
			},
		},
		{
			name: "config reference gives omitted retries a zero budget",
			args: []string{"show", "config", "stages and pipelines"},
			want: []string{"`max_retries` defaults to `0` when omitted", "the first failure pauses the ticket"},
		},
		{
			name: "pipeline defaults reference is available as a section",
			args: []string{"show", "pipelines", "Default selection"},
			want: []string{
				"1. Explicit request", "2. Stored nonempty pipeline",
				"3. Project `epic_child_pipeline`, for a child of any actual epic, passive or agent",
				"4. Project `pipeline`", "5. Global `default_pipeline`", "6. Standalone execution",
				"Missing or ordinary-ticket parents", "Existing assignments are not migrated", "detail-path edits",
				"Raw uploads and daemon startup", "not durable provenance", "later initialization can fill the blank",
				"supervisors run standalone", "Agent resolution stays independent", "or `default_agent` if unset",
				"local `init` opens the pipeline picker",
				"kontora skills show pipelines Compatibility", "ticket-level opt-out",
			},
			notWant: []string{"## Stages"},
		},
		{
			name: "pipeline waits describe recovery and link to status and template data",
			args: []string{"show", "pipelines", "Durable waits"},
			want: []string{
				"kontora skills show tickets 'Durable workflow waits'", "kontora skills show config 'Durable waits'",
				"workflow-probe.json", "workflow-control.json", "An elapsed deadline does not bypass recovery.",
				"Same-status changes do not send notifications", "a wake has no dedicated notification",
				"Restore the matching config and retry if Kontora paused the wait or continuation",
				"kontora skills show pipelines 'Workflow graphs'", "kontora skills show pipelines Compatibility",
			},
			notWant: []string{"exactly-once", "host-power-loss"},
		},
		{
			name: "config compatibility explains strict YAML readers and older writers",
			args: []string{"show", "config", "Compatibility"},
			want: []string{
				"Upgrade all clients and daemon binaries", "before adding\n  `epic_child_pipeline`",
				"Older strict-YAML loaders reject the key", "Older clients treat custom-active tickets as parked work",
				"Older writers can delete the empty-list marker", "Older loaders reject `default_statuses`",
				"Epic activation: See `kontora skills show tickets Compatibility`",
			},
		},
		{
			name: "ticket compatibility prohibits mixed versions for activated epics",
			args: []string{"show", "tickets", "Compatibility"},
			want: []string{"Upgrade every daemon", "can overwrite supervisor status", "Do not run mixed versions", "kontora skills show config Compatibility"},
		},
		{
			name: "pipeline compatibility explains versioned graphs and waits",
			args: []string{"show", "pipelines", "Compatibility"},
			want: []string{
				"before enabling graphs", "result v3 or graph-state v2", "Fingerprints stay unchanged for workflows without waits",
				"Version 1 graph state, version 1 and 2 results, and existing recovery contracts remain compatible",
			},
		},
		{
			name: "cli compatibility describes supervised rollout and links to wait requirements",
			args: []string{"show", "cli", "Compatibility"},
			want: []string{
				"Upgrade the CLI and daemon together", "the prompt each supervised Pi or Claude run receives",
				"local-delivery rules in Epic supervision", "Do not restart live epics or migrate ticket state",
				"kontora skills show pipelines Compatibility", "Epic activation: See `kontora skills show tickets Compatibility`",
			},
		},
		{
			name:    "pipeline reference links to supervisor rules without restricting operators",
			args:    []string{"show", "pipelines", "Moving a ticket by hand"},
			want:    []string{`kontora skills show tickets "Epic supervision"`, "workflow rules", "ordinary operators"},
			notWant: []string{"report the conflict", "changing, clearing or replacing child pipelines"},
		},
		{
			name: "reload reference applies both capacities live",
			args: []string{"show", "config", "Reloading"},
			want: []string{
				"`max_concurrent_agents` and `max_concurrent_epic_supervisors` reload live",
				"Raising a limit lets queued work start", "Lowering a limit does not stop running",
				"new work waits until usage falls below the limit",
				"default_pipeline", "Run settings apply to the next run", "later creation and initialization",
				"not existing assignments", "preserves stored nonempty pipelines", "invalid child-pipeline reference",
				"keeps the running config", "except for `plannotator.reviews_dir`",
				"Enabling `auto_pick_up` queues existing eligible `todo` tickets",
				"including tickets already queued", "recovery, and scheduled automations remain possible",
			},
		},
		{
			name: "pipeline reference separates worker and supervisor runs",
			args: []string{"show", "pipelines", "What a run is"},
			want: []string{"max_concurrent_epic_supervisors", "four additional agent processes", "oldest", "pool has capacity", "human_review", "rework", "history"},
		},
		{
			name:    "a section matches on a unique substring",
			args:    []string{"show", "cli", "ontora new"},
			want:    []string{"## kontora new"},
			notWant: []string{"## kontora view"},
		},
		{
			name:    "ticket and automation sections require a specific name",
			args:    []string{"show", "cli", "new"},
			wantErr: []string{"matches 2 sections", "kontora new", "kontora automation new"},
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

func TestConfigSkillRestartOnlyFields(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, Skills(&buf, []string{"show", "config", "Reloading"}))
	_, restart, ok := strings.Cut(buf.String(), "These need a restart:")
	require.True(t, ok)
	for _, field := range []string{
		"tickets_dir", "secondary_tickets_dirs", "worktrees_dir", "logs_dir",
		"instance_name", "tmux_session", "plannotator.reviews_dir", "web", "metrics",
	} {
		assert.Contains(t, restart, "`"+field+"`")
	}
	for _, field := range []string{"max_concurrent_agents", "max_concurrent_epic_supervisors"} {
		assert.NotContains(t, restart, field)
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
