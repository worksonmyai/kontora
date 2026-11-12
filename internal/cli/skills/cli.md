# kontora CLI

Use `kontora <command> [flags] [arguments]` to create tickets, control runs,
and inspect results. Both `-flag` and `--flag` work.

Put flags before positional arguments. For `new`, put them before the title;
for `edit`, before the ticket ID.
For `note` and `summary`, put all flags before the ticket ID, including when using stdin.

Verbs that take a `TICKET_ID` accept a unique prefix, except for `result`,
which requires the exact invocation ticket ID. `kontora view kon-q` finds
`kon-q88f`. A prefix matching two tickets is an error naming both.
Matching is from the start of the id only.

Exit codes: 0 on success, non-zero on failure with the reason on stderr. A
search that matches nothing still exits 0.

## If you are a stage agent

When `KONTORA_TICKET_ID` is set, this process is the agent running that ticket's
current stage. You cannot change that ticket's status: `done`, `cancel`, `move`,
`pause`, `retry`, `skip`, `set-stage` and `schedule` aimed at it are refused, as
is `archive`. Kontora owns the next status. For unsupervised execution, exit 0 when
the stage's work is done and non-zero when it is not. Activated epic supervisors
and their children must also submit an explicit result; exit zero alone cannot
advance supervised work. See `kontora skills show cli 'kontora result'`.

Record what you did with `note` and `summary` through the Handoff submission
method below. Neither command has the lifecycle restriction. Every verb works
normally against any other ticket.

## Handoff submission

Prefer Pi's `kontora_note({text: string})` and
`kontora_summary({text: string})` tools when available. Kontora's injected
extension binds them to the current ticket; do not supply a ticket ID.
Availability requires ticket context, not checkpoint compaction. Tool
restrictions still apply. The dashboard assistant does not receive these tools.

Native tools reject blank text, embedded NUL, and text above 32,768 UTF-8 bytes.
They pass text literally without shell evaluation. The CLI still applies the
normalization described under `kontora note` and `kontora summary`.
Native text appears in process arguments. Use stdin for large or sensitive text.

If native tools are unavailable, or stdin is needed:

1. Use your file-writing tool to create unique scratch files outside the repository.
2. Put the note or summary text in the corresponding file.
3. Submit the file through stdin using the commands below.

```sh
kontora note TICKET_ID < /absolute/path/note.md
kontora summary TICKET_ID < /absolute/path/summary.txt
```

Replace `TICKET_ID` and each example path with the actual ticket ID and scratch path.
Do not embed generated text in shell arguments or payload-bearing shell commands.
Do not create the files with `echo` or `printf`.

If a launched submission fails or lacks acknowledgement, read the ticket before resubmitting.
The write outcome may be unknown. Do not retry automatically or switch submission methods automatically.

## kontora result

```sh
kontora result --input-file /absolute/path/result.json TICKET_ID
kontora result TICKET_ID < /absolute/path/result.json
```

Mandatory for activated epic supervisors and their actual children, including
fresh, resumed, and rework runs. Unrelated tickets, passive-epic children,
annotations, and assistant-only runs retain their existing requirements.

Prefer Pi's `kontora_result({outcome, summary, next_step?})` tool when available;
it is independent of checkpoint compaction and binds the current invocation.
Otherwise create a unique scratch JSON file outside the repository with a
file-writing tool, then use the CLI above. Put flags before the ticket ID. Do not
interpolate generated payloads into shell commands. Without `--input-file`, the
CLI reads stdin; the input-file path must be absolute.

```json
{"outcome":"blocked","summary":"Required device evidence is unavailable.","next_step":"Provide device access or explicitly change the requirement."}
```

Only `complete` and `blocked` outcomes are allowed. `summary` must be nonblank;
`blocked` also requires nonblank `next_step`. `complete` means this stage's assigned
work, not the whole ticket. A review stage can complete while fixes remain.
Fixture work alone is not complete when production foundations or other required
evidence remain. All payload fields are strings; optional `next_step` may be
omitted for complete. Only those three fields are accepted. The limit is 32 KiB
of valid UTF-8 JSON; NULs, duplicate/unknown fields and trailing content are rejected.

The daemon supplies `KONTORA_RESULT_VERSION` (`1`), `KONTORA_INVOCATION_ID`,
`KONTORA_RESULT_TICKET`, `KONTORA_RESULT_STAGE`, and absolute `KONTORA_RESULT_FILE`
after configured environment merging. Do not select or override invocation
identity. Missing context or an incompatible version is an error, not a no-op.
Each physical launch gets a fresh identity, including retry and resume-to-fresh
fallback. Check acknowledgement: identical resubmission is idempotent; conflicting
replacement is rejected. Old results cannot finalize newer work.

Notes, summaries, final prose, questions, and phase checkpoints are not terminal
results. Missing/invalid reports after a clean run pause the same stage; blocked
also pauses without consuming a retry, entering pipeline retry/back policies, or
removing the worktree. Complete cannot override execution/provider errors, hooks,
or epic child checks. Do not change your own lifecycle status.

If requirements or authority block work, investigate and resolve permitted local
setup, then ask a concrete resource/authority/scope question with attempted checks,
options, costs, and a recommended default. Save the question and answer together
immediately and confirm persistence. Do not weaken requirements or substitute
emulation for required real-device evidence. When no authorized work remains,
submit blocked and stop. Retry requires explicit operator action; saving an
answer, reading a notification, or dismissing attention does not restart work.
On Retry inspect saved decisions, current children, worktrees, dirty files, and
interrupted Git operations. Upgrade CLI and daemon together.

## Common flags

- `--config PATH` — config file. Defaults to `$KONTORA_CONFIG`, then
  `./.kontora/config.yaml`, then `$XDG_CONFIG_HOME/kontora/config.yaml`, then
  `~/.config/kontora/config.yaml`.
- `--tickets-dir PATH` — overrides `tickets_dir`. Ignored in remote mode.
- `--url URL` — remote daemon URL. Non-empty switches on remote mode. Defaults
  to `$KONTORA_URL`.
- `--token TOKEN` — bearer token for the remote daemon. Defaults to
  `$KONTORA_TOKEN`.
- `--operator-token TOKEN` — operator credential for every `automation` child
  and top-level `attach`. Defaults to `$KONTORA_OPERATOR_TOKEN`. The client
  sends bearer and operator credentials independently, including terminal connections.

## Remote mode

`KONTORA_URL` (or `--url`) points every client command at a daemon over HTTP
instead of at local files. Paths in `--path` then name directories on the
daemon host.

Supported: `ls`, `view`, `new`, `init`, `update`, `delete`, `run`, `schedule`,
`pause`, `retry`, `cancel`, `done`, `move`, `skip`, `set-stage`, `note`,
`summary`, `dep`, `undep`, `link`, `unlink`, `logs`, `activity`, `changes`,
`stats`, all `automation` children, `review`, `annotate`, `config`, `attach`.

Rejected, because they act on local files: `edit`, `search`, `archive`,
`estimate-compaction`, `sessions`, `doctor`, `start`, `setup`.

Unaffected, because they read neither config nor daemon: `fmt`, `completion`,
`version`, `help`, `phase-complete`, `skills`.

## kontora ls

`kontora ls [flags]` — list tickets. On a terminal with no flags it opens the
kanban TUI; any filter, `--json`, `--static`, `--closed` or `--archived` makes
it print a table instead. Prefer `--json` when parsing.

- `--closed` — include done, cancelled and legacy `closed`.
- `--archived` — include archived.
- `--status STATUS` — only this status. Naming a hidden status shows it, so
  `--status done` needs no `--closed`.
- `--project NAME` — only this configured project.
- `--path PATH` — only this repository path.
- `--ready` — only `todo` kontora tickets whose deps are all closed.
- `--blocked` — only `todo` kontora tickets waiting on a dep.
- `--limit N` — print at most N.
- `--json` — print JSON.
- `--static` — print the table even on a terminal.

`--ready` and `--blocked` cannot be combined. `--json` prints an array of
objects with `id`, `title`, `status`, `kontora`, `path`, `project`, `pipeline`,
`stage`, `agent`, `created_at`, `started_at`, `completed_at`, `deps`, `links`,
`parent`, `kind`, `epic_execution`, `child_order`, `branch`, `base_branch`,
`ready` and `blockers`. `epic_execution: "agent"` identifies a supervisor.
`child_order` is stored display order, not membership. Read each child's `parent`
across the complete listing to find all children. The listing omits the ticket body.

## kontora search

`kontora search [flags] QUERY` — match a Go regular expression against every
ticket file, frontmatter and body alike, and print the matching lines grouped
by ticket. Every status is searched, archived included. Local only.

Case follows ripgrep's smart-case rule. A query starting with `-` needs `--`
in front of it: `kontora search -- -race`.

- `-i` / `-s` — force case-insensitive / case-sensitive. Cannot be combined.
- `-F` — literal string, not a regex.
- `-l` — print matching ticket IDs only.
- `-m N` — at most N matching lines per ticket, default 5, `0` for all.
- `--body` — search the body only, not the frontmatter.
- `--json` — print JSON. Cannot be combined with `-l`.
- `--status`, `--project`, `--path`, `--pipeline`, `--agent` — narrow the set.

Exits 0 whether or not anything matched.

## kontora new

`kontora new [flags] TITLE` — create a ticket and print its id.
Locally, omitting `--path` with `--parent EPIC_ID` uses the resolved parent epic's
stored `path`. Without `--parent`, it uses the current git root.
An explicit `--path` always wins. Remote mode still requires `--path` on the
daemon host, even with `--parent`. API creation likewise requires `path`.

Project defaults use the selected path and lexical matching. An explicit
`--pipeline` wins; otherwise an ordinary ticket under any actual epic uses project
`epic_child_pipeline`, then project `pipeline`, global `default_pipeline`, then
standalone execution. Passive and agent epics both qualify. A missing or
ordinary-ticket parent does not qualify. `--pipeline none` skips pipeline defaults
without changing independent agent resolution.

- `--path PATH` — repository the ticket works in.
- `--pipeline NAME` — pipeline to run, or `none` for a standalone ticket.
- `--agent NAME` — agent to use, or `none` to skip the project default.
- `--notify STATUSES`: comma-separated statuses. Omit to inherit the project's list;
  `""` or `none` skips it. Epics do not inherit.
- `--notify-channels NAMES`: comma-separated channel names; `none` silences delivery.
- `--branch NAME` — work branch name.
- `--base-branch NAME` — branch the work branch starts from.
- `--status STATUS`: `open` or `todo`. Defaults to `todo` for ordinary tickets;
  epics accept only `open` and default to it.
- `--kind epic`: create a passive shared brief. Creation never starts a supervisor.
- `--parent EPIC_ID`: create an ordinary ticket under an existing epic. Accepts
  a unique id prefix. Epics cannot have parents.
- `--at TIME` — schedule pickup for an instant: RFC 3339, or a local wall time
  such as `"2099-09-01 09:00"`.
- `--after DURATION` — schedule pickup this long from now, e.g. `90m`, `24h`,
  `3d`, `2w`.
- `--description-file PATH` — read the body from a file, `-` for stdin.
- `--quiet` — print only the new ticket id.

Use `--description-file` for anything longer than a line. The body is written
after a generated `# <title>` heading.

A ticket created with `--status open` is never visible as `todo`, so a daemon
with `auto_pick_up: true` cannot claim it before you finish editing it.

With `--kind epic`, omit pipeline, parent and schedule flags. `--agent` selects
the future supervisor without starting it. Project defaults are skipped.
The daemon derives the passive epic's status from its children; its brief remains editable.
Use `kontora run EPIC_ID` later to activate a supervisor.

```sh
kontora new --kind epic --status open --path ~/projects/myrepo --description-file brief.md "Add authentication"
kontora new --parent EPIC_ID --status open "Implement the login API"
```

Replace `EPIC_ID` with the new epic's id. The example uses local mode and inherits
the epic's stored repository path. `--parent` groups the child; it does
not queue it or create dependencies. See `kontora skills show tickets Epics`.

`--at` and `--after` write `status: open` and `scheduled_at` in the same single
write. Neither combines with the other, or with `--status todo`, and neither
accepts an instant already in the past. A scheduled ticket has its repository
checked at creation the way a `todo` one does.

## kontora view

`kontora view [--body | --notes | --last N] TICKET_ID` prints the ticket's
status, pipeline position, effective agent and body by default. A ticket's
`agent` override takes precedence over the pipeline stage's agent.

| Flag | Description |
|------|-------------|
| `--body` | Print only the stored markdown after the frontmatter, exactly what `kontora update --body-file` writes back. |
| `--notes` | Print from the `## Notes` heading through the end of the body, including later sections. |
| `--last N` | Print from the start of the Nth-last note through the end of the body. N must be a positive integer; implies `--notes`. |

```sh
kontora view TICKET_ID
kontora view --body TICKET_ID
kontora view --notes TICKET_ID
kontora view --last 5 TICKET_ID
```

Legacy notes without bylines also count.
If N exceeds the note count, `--last N` prints the whole section, including
`## Notes`. Both note views keep trailing sections, including old review text.
Without a Notes section, both print nothing and exit 0. Combining `--body` with
`--notes` or `--last` fails with a usage error. All three flags print unstyled
markdown without a pager in local and remote mode.

`--notes` prints only the body from its `## Notes` heading to the end, unstyled
and unpaged. Use it when you need the ticket history and the full output is too
long to read in one tool call.

## kontora edit

`kontora edit TICKET_ID` — open the ticket file in `$EDITOR`. Local only, and
interactive: never run it from an agent.

## kontora update

`kontora update TICKET_ID [flags]` — change frontmatter fields or the body. At
least one flag is required. An empty value, or `none` where noted, clears the
field.

- `--body-file PATH` — replace the body from a file, `-` for stdin.
- `--pipeline NAME` — set the pipeline (`""` or `none` clears it).
- `--path PATH` — set the repository path.
- `--agent NAME` — set the agent override (`""` or `none` clears it).
- `--notify STATUSES`: set comma-separated statuses (`""` or `none` removes the field).
- `--notify-channels NAMES`: set comma-separated channels (`""` removes the override;
  `none` silences delivery).
- `--branch NAME` — set the work branch (`""` clears it).
- `--base-branch NAME` — set the base branch (`""` clears it).

A passive epic's brief is editable in every status, including `in_progress`,
through `--body-file`. Agent epics and ordinary tickets reject edits while
running or terminal. Epics never accept a pipeline. There
are no `--kind` or `--parent` update flags; change an existing ticket's parent
through the web UI or parent API described in `kontora skills show tickets Epics`.

## kontora delete

`kontora delete TICKET_ID -f` — delete the ticket's markdown file. `-f` or
`--yes` is required. The worktree is not cleaned up; cancel the ticket first if
you want the normal cleanup path.

For an epic, use remote mode (`--url` or `KONTORA_URL`) so the daemon clears
its children's parent references before deletion. The children are kept. Local
mode only deletes the epic file and leaves those references unchanged.

## kontora init

`kontora init TICKET_ID [flags]` — mark an existing ticket file `kontora: true`
so the daemon will run it. Without flags it prompts for the missing fields, so
always name them from a script or an agent.

- `--path PATH` — repository path. Required when the frontmatter has none.
- `--pipeline NAME` — pipeline, or `none` for a standalone ticket.
- `--agent NAME` — agent, or `none` to skip the project default.
- `--stage NAME` — starting stage. Defaults to the pipeline's first.
- `--status STATUS` — `open` or `todo`. Defaults to asking.

For ordinary tickets, pipeline precedence is explicit request, stored nonempty
pipeline, project `epic_child_pipeline` for an actual epic child, project `pipeline`,
global `default_pipeline`. Initialization resolves actual membership
before filling a blank pipeline, for both passive and agent epics.

If the pipeline remains blank, local `init` opens the pipeline picker, or errors if no pipelines are configured.
Pass `--pipeline none` for standalone execution.

`--pipeline none` stores a blank for this request, not durable opt-out provenance.
A later initialization can fill it unless the caller opts out again. Agent
resolution stays independent. Existing assignments are not migrated after config,
parent or detail-path changes; operators must retarget them explicitly.

For an epic, explicit `--status todo` activates a supervisor. Omit `--pipeline`
and `--stage`; `--agent` selects the supervisor. Passive epic initialization
without explicit `--status todo` is refused. Activation requires the epic and
every child to use the same Git repository.
Local `init` requires a running daemon when starting an epic.
It queues the supervisor even with `auto_pick_up: false`.

For ordinary tickets in remote mode, `--pipeline` and `--path` are both required.

## kontora run

`kontora run TICKET_ID` — enqueue an `open` or `todo` ticket. Needs a running
daemon. A ticket whose deps are not all closed is reported as blocked, naming
them, rather than queued.

On a passive epic, this explicitly activates a supervisor and writes
`epic_execution: agent`. Creation and draft edits never activate it. The epic
and every child must use the same Git repository. The supervisor uses its own
branch and worktree, without a pipeline, while children keep theirs. Success parks
the epic in `human_review`.

Supervisor prompts prohibit changing, clearing or replacing child pipelines and
editing pipeline definitions, stage definitions or project pipeline defaults.
Create children without `--pipeline`. Do not bypass workflows through `init`,
stage skipping, direct file edits or pause/retry sequences. If configured work
conflicts with your authority, report the conflict, request an operator decision
and leave that work unstarted. Do not substitute another pipeline. These are
prompt instructions, not runtime enforcement; operator commands remain available.
Pipeline defaults do not authorize commits or publication.

See `kontora skills show tickets Epics` for review, rework,
Git and approval limits, capacity and mixed-version upgrade requirements.

## kontora schedule

`kontora schedule TICKET_ID --at TIME | --after DURATION | --clear` — set or
clear the time the daemon moves an `open` ticket to `todo`. Needs a running
daemon. Exactly one of the three flags is required.

- `--at TIME` — an RFC 3339 instant or a local wall time such as
  `"2099-09-01 09:00"`, stored normalized to UTC. A date with no time is
  refused. An instant already in the past is refused.
- `--after DURATION` — a duration from now (Go's units plus `d` and `w`, so
  `3d` and `1w2d3h` both work), resolved on this machine's clock.
- `--clear` — remove the schedule. The ticket stays `open`.

Scheduling a `todo` ticket returns it to `open` and drops it from the queue.
Setting a schedule is refused on a running, closed or archived ticket, and while
a Plannotator session or a pending annotation owns it; `--clear` is refused only
while the ticket is running. `kontora run` clears the schedule and queues the
ticket now.

## kontora pause

`kontora pause TICKET_ID` — stop a running ticket and park it in `paused`. A
ticket that already closed cannot be paused. Refused on your own ticket.
On an agent epic, this stops only the supervisor. Running children continue.

## kontora retry

`kontora retry TICKET_ID` — reset an initialized ticket to `todo` with the
attempt counter cleared. Refused for `in_progress`, `todo`, `archived`, passive
epics, and your own ticket.

Local mode only updates the ticket file. With `auto_pick_up: false`, follow retry
with `kontora run TICKET_ID`. Remote mode and dashboard/API retry queue
dependency-ready work even with automatic pickup off.

## kontora done

`kontora done TICKET_ID` — move the ticket to `done`, stopping any running
agent and cleaning up its worktree. Refused when the ticket is the one this
process is a stage of; the pipeline sets the status on exit.

## kontora cancel

`kontora cancel TICKET_ID` — move the ticket to `cancelled`, stopping any
running agent and cleaning up its worktree. Refused on your own ticket, like
`done`.

## kontora move

`kontora move TICKET_ID STATUS` — move a ticket to any status the config
allows: `open`, `todo`, `paused`, `human_review`, `done`, `cancelled`, or a
custom status from `statuses:`. This is the only way to reach `human_review` or
a custom status from the command line. `archived` is not accepted; use
`kontora archive`. Refused on your own ticket, whatever the target status: the
run's history entry is lost with it.

## kontora skip

`kontora skip TICKET_ID` — intermediate skip sets the next stage to `todo`.
Daemon/API skip queues dependency-ready work even with `auto_pick_up: false`.
Disk-only skip does not enqueue; use `kontora run TICKET_ID` afterward when
automatic pickup is off. Final skip marks the ticket done without recording a
successful run.

Refused on your own ticket. Pause a running ticket first and wait for
termination. Close any active Plannotator review before changing stages.

## kontora set-stage

`kontora set-stage TICKET_ID STAGE` — move the ticket to a named stage of its
pipeline without retrying it. Status and attempt stay unchanged; a `todo` ticket
remains eligible for pickup. Pause first and wait for termination. Close active
Plannotator reviews. Refused on your own ticket.

## kontora note

`kontora note TICKET_ID [TEXT]` — append a note under the body's `## Notes`
section. Positional text remains supported. Without text arguments, it reads
stdin and removes trailing LF characters. Notes also trim surrounding whitespace
and reject empty text. For literal submission, use the Handoff submission
method: `kontora skills show cli "Handoff submission"`.

This is how you leave something for the next stage to read: a stage prompt
containing `{{ .Ticket.Description }}` gets the whole body, notes included.

Adding or editing note text shifts headings outside fenced code only when the
shallowest heading is h1 or h2. That heading becomes h3. Relative levels stay
the same, capped at h6. Notes whose shallowest heading is h3 or deeper need no
shift. Backtick and tilde fenced code stays unchanged; Kontora appends a closing fence
if needed. Outside fenced code, a line beginning with `# ` or `## ` at column
zero ends the Notes section. Inside fences, those lines do not end Notes, and
bold-only lines do not open notes.

Kontora writes each note with a byline containing its timestamp, author and 4-character id:
`**2026-03-06T12:00:00Z · claude · q88f**`. Outside fences, a bold-only line
opens a note only if its first `·` field starts with `YYYY-MM-DD`, or if it
matches the full `at · author · id` grammar. That grammar permits empty `at`
and author fields, plus optional `re:<id>` and `edited` flags. For example,
`** ·  · cd34**` is a byline. `**Verification:**` stays note text.
On add or edit, byline-like bold lines in note text use outer `__` instead of
`**` when stored. Both render as bold.

Existing tickets are not migrated. Full-body writes do not normalize note text.
Use `kontora view --notes TICKET_ID` to include old sections after Notes.

Under the daemon, the author defaults to `$KONTORA_AGENT`, the name of the
agent for the run, so you do not have to pass it.

| Flag | Description |
|------|-------------|
| `--author NAME` | Override the author. |
| `--reply-to NOTE_ID` | Reply to that note. One level only: replying to a reply is refused. |

Put both flags before the ticket ID, including when using stdin:

```sh
kontora note --reply-to NOTE_ID TICKET_ID < /absolute/path/note.md
```

## kontora summary

`kontora summary TICKET_ID [TEXT]` — set the ticket's summary.
Positional text remains supported. Without text arguments, it reads stdin and
removes trailing LF characters. Summaries preserve spaces and internal newlines;
the command does not shorten them or force them onto one line.

Use the Handoff submission method for literal text:
`kontora skills show cli "Handoff submission"`.

```sh
kontora summary TICKET_ID < /absolute/path/summary.txt
```

The daemon clears the summary when a new stage starts. At stage completion,
it captures the summary on the ticket and its `history` entry. Stage capture
cuts text above 4,000 bytes at a UTF-8 boundary and appends `\n[truncated]`.

## kontora dep

`kontora dep TICKET_ID DEPENDENCY_ID` — make `TICKET_ID` wait on
`DEPENDENCY_ID`. Only the dependent ticket is written; the reverse edge
(`blocks`) is derived on read. A dependency that would close a cycle is
rejected and the error names the cycle.

## kontora undep

`kontora undep TICKET_ID DEPENDENCY_ID` — drop the dependency edge.

## kontora link

`kontora link TICKET_ID TICKET_ID...` — relate the first ticket to each of the
others, and not the others to each other. Both sides of each link are written.

## kontora unlink

`kontora unlink TICKET_ID TICKET_ID...` — remove the relation between the first
ticket and each of the others, on both sides.

## kontora archive

`kontora archive --days N [flags]` — mark every `done`, `cancelled` or legacy
`closed` ticket whose file has not been modified for at least N days as
`archived`, which hides it everywhere and keeps the file. Local only.

- `--days N` — required, positive. Age threshold on the file's mtime.
- `--dry-run` — list what would be archived, write nothing.
- `--path PATH` / `--project NAME` — limit to one repository. Not combinable.
- `--status STATUS` — one of `done`, `cancelled`, `closed`.
- `-y`, `--yes` — skip the confirmation prompt. Required when stdin is not a
  terminal, so an agent must pass it or the run fails.

## kontora logs

`kontora logs [--stage NAME] TICKET_ID` — print the agent's plaintext log.
Without `--stage` it shows the most recent one. Paged only on a terminal.

## kontora activity

`kontora activity [--stage NAME] [--run N] TICKET_ID` — print one stage run's
structured transcript: the agent's messages and tool calls, one per line.
Falls back to the plaintext log for agents that write no session record. Needs
a running daemon.

## kontora sessions

`kontora sessions [flags] TICKET_ID` — print the files behind the ticket's
runs, one row per history row. Columns are stage, run, artifact, path, and a
reason when the path is `-`. Local only: every path names a file on this
machine.

- `--stage NAME` — only this stage.
- `--run N` — only this run number within the stage.
- `--logs` — stage log paths instead of session files.
- `--events` — activity sidecar paths instead of session files.
- `--all` — all three for every run.

In the run column, `-` marks the per-stage log and `?` a file no run claims.

## kontora changes

`kontora changes TICKET_ID` — print the commits on the ticket's branch and the
per-file line counts against its base. Needs a running daemon.
Uncommitted and untracked files are not included; inspect them in the ticket's worktree.

## kontora stats

`kontora stats [flags]` — print throughput, median cycle time, first-pass rate,
and per-stage, per-agent and per-project breakdowns. Needs a running daemon.

- `--range RANGE` — `1d`, `1w`, `30d`, `90d` or `all`. Defaults to `90d`.
- `--project NAME` — only this configured project.
- `--pipeline NAME` — only this pipeline.

## kontora automation

`kontora automation COMMAND [flags]` manages scheduled agents without tickets.
All operations use the daemon API, including local reads and schedule preview.
There is no offline fallback to runtime storage. An explicit `--url` works without
local configuration. Parent and child `-h` work offline.

Every child accepts `--config PATH`, `--url URL`, `--token TOKEN`, and
`--operator-token TOKEN`. Credentials default to `KONTORA_TOKEN` and
`KONTORA_OPERATOR_TOKEN`. HTTP and terminal connections send both independently.
Automation commands do not accept `--tickets-dir` or use ticket ID resolution.

`REF` accepts full automation IDs (`aut-` plus 32 lowercase hexadecimal characters)
or exact configuration keys only. Display names and abbreviated IDs are rejected.
Keys resolve from active definitions. After deletion, use the original full ID
for retained history and logs; a recreated key identifies a different definition.

Every noninteractive child accepts `--json`. Stdout contains API objects;
request identities and diagnostics go to stderr. List and history keep availability
and pagination envelopes. Empty collections remain `[]`; unknown costs and
statistics remain `null`.
`ls --json` prints the availability envelope when HTTP 200 reports unavailable storage.
The CLI explains the error on stderr and exits nonzero.
HTTP errors produce diagnostics and a nonzero exit without JSON stdout.
Human output distinguishes scheduling state from execution phase and unknown cost from zero.

## kontora automation ls

`kontora automation ls [flags]` lists definitions in key order, including full ID,
key, name, enabled state, run phase, schedule, timezone, next occurrence, agent,
and last outcome.

- `--state STATE` — `all` (default), `enabled`, or `paused`.
- `--query TEXT` — case-insensitive substring across searchable definition fields.
- `--project NAME` / `--agent NAME` — exact project or agent filters.

## kontora automation view

`kontora automation view REF [flags]` shows the complete specification, prompt,
revision, schedule, current and last runs, statistics, and cost coverage.

## kontora automation new

`kontora automation new [flags]` creates a definition in the daemon's YAML.
Scheduling defaults to disabled.

- `--name NAME` — required display name.
- `--prompt-file FILE` — required local prompt template file; `-` reads stdin.
- `--path PATH` — required repository path on the daemon host.
- `--agent NAME` — required configured agent.
- `--schedule VALUE` — required numeric five-field cron or whole-minute interval.
- `--key KEY` — configuration key; derived from the name when omitted.
- `--model MODEL` — optional supported model override.
- `--timezone ZONE` — IANA timezone, default `UTC`.
- `--workspace MODE` — `worktree` (default) or `repo`.
- `--overlap POLICY` — `skip` (default), `queue`, or `replace`.
- `--timeout DURATION` — positive duration, default `30m`.
- `--enabled` — enable scheduling, default false. `--enabled=false` sets false explicitly.
- `--request-id ID` — stable request identity, generated once when omitted.

Creation, duplication, and manual runs print the request ID to stderr before
sending. Mutations do not retry automatically. Keep the identity after an
uncertain response and repeat the same inputs with the same request ID.

## kontora automation update

`kontora automation update REF [flags]` changes only explicitly supplied fields.
Accepts every specification flag from `new` except the immutable `--key`:
`--name`, `--prompt-file`, `--path`, `--agent`, `--model`, `--schedule`,
`--timezone`, `--workspace`, `--overlap`, `--timeout`, and `--enabled`.
At least one field is required. The CLI fetches the definition, preserves omitted
fields, and submits the complete specification. `--model ''` clears the override;
`--enabled=false` disables scheduling. `--prompt-file -` reads stdin.
Update does not accept `--request-id`.

`--revision N` supplies a positive expected revision. Otherwise the CLI uses the
fetched revision. Delete, pause, and resume use the same policy. Stale revisions
fail without an automatic retry or overwrite. Inspect the definition before
repeating a change.

## kontora automation duplicate

`kontora automation duplicate REF [flags]` copies the specification into a new,
disabled definition. Accepts `--name NAME`, `--key KEY`, `--enabled`, and
`--request-id ID`. The default name is `<name> (copy)`; the server derives the key
when omitted. Retry with unchanged source inputs and the same request ID.
Changed inputs under the same request ID remain a conflict.

## kontora automation delete

`kontora automation delete REF --yes [flags]` deletes the definition and requests
termination of unfinished work. Historical runs and logs remain.
`--yes` or `-f` is required before a deletion request. Accepts `--revision N`.
JSON confirmation contains the resolved `id` and `deleted` status.

## kontora automation pause

`kontora automation pause REF [flags]` disables future scheduled runs without
cancelling accepted runs. Manual runs remain available while paused.
Accepts `--revision N`.

## kontora automation resume

`kontora automation resume REF [flags]` enables future scheduling without
replaying paused time. Accepts `--revision N`.

## kontora automation run

`kontora automation run REF [flags]` requests a manual run, including while paused.
Accepts `--request-id ID`. The response's phase, outcome, and result describe the
accepted request, not proof of completion. Overlap policy can queue or skip it.

Manual runs print the resolved full automation ID and request ID to stderr before
sending. After an uncertain response, retry with that original identity pair.
An explicit `--request-id` requires the original full automation ID, never a key.
A recreated key could otherwise launch a different definition.

## kontora automation stop

`kontora automation stop REF --run N [flags]` requests termination of an exact
positive run number. It never switches to another current run.
The returned `stopping` or `recovery_required` phase does not prove termination.
Inspect the returned phase and outcome rather than assuming the run stopped.

## kontora automation runs

`kontora automation runs REF [flags]` reads one history page.
`--offset N` defaults to `0`; `--limit N` defaults to `20` and accepts `1` through `200`.
Rows include number, trigger, phase, outcome, timestamps, result, exit codes, and
nullable cost. New runs can shift later pages; pagination does not promise a snapshot.
Use the original full ID to read retained history after deletion.

## kontora automation logs

`kontora automation logs REF --run N [flags]` reads composed logs for a positive
run number. Human output includes timestamp, source, kind, and text.
`--json` prints structured logs. `--raw` prints unchanged endpoint text without
styling, paging, or status prose. `--raw` and `--json` are mutually exclusive.
Raw logs are composed text, not original terminal capture.
Use the original full ID for retained logs after deletion.

## kontora automation preview

`kontora automation preview --schedule VALUE [--timezone ZONE] [flags]` returns
the server's schedule description and four future occurrences using the daemon's
clock. The timezone defaults to `UTC`. Preview does not change configuration and
has no offline mode.

## kontora automation terminal

`kontora automation terminal REF --run N [flags]` inspects server-resolved
terminal metadata for a positive run number. The server verifies terminal identity
and eligibility. The command does not attach or construct a local tmux target.

## kontora automation attach

`kontora automation attach REF [--run N] [--rw] [flags]` attaches read-only to the
current run. `--run N` selects an exact positive run number without requiring a
live definition. The server verifies the saved terminal identity and eligibility.
Queued, completed, or unavailable runs have no attachable terminal.
`--rw` enables input. There is no `--json` flag.
Interactive: never run attachment from an agent.

## kontora attach

`kontora attach [--rw] TICKET_ID` — attach to the running ticket's terminal:
the tmux window locally, a WebSocket in remote mode. Read-only unless `--rw`.
Interactive: it takes over the terminal, so never run it from an agent.
For automations, `kontora attach AUTOMATION_ID` accepts a full ID and selects the
current run through the daemon. `--operator-token TOKEN` defaults to
`KONTORA_OPERATOR_TOKEN`. Use `kontora automation attach REF --run N` for an exact
run or a configuration key.

## kontora review

`kontora review TICKET_ID` — open the ticket's diff in Plannotator. The daemon
spawns the binary, so in remote mode the window opens on the daemon host. The
ticket has to be in `human_review`.

## kontora annotate

`kontora annotate TICKET_ID` — open the ticket's markdown in Plannotator, on
the daemon host in remote mode.

## kontora phase-complete

`kontora phase-complete TICKET_ID --completed TEXT --next TEXT` — signal a
boundary between two ticket phases from inside a checkpointing run. Agents call
it, not people. It reads no config and talks to no daemon.

Without `KONTORA_CHECKPOINT_FILE` in the environment it records nothing, says
checkpoint compaction is off for the run, and exits 0.

## kontora estimate-compaction

`kontora estimate-compaction [flags]` — project token savings from checkpoint
compaction by replaying completed pi session traces. Read-only, local only.

- `--logs-dir PATH` — logs directory. Defaults to the config's `logs_dir`.
- `--stage NAME` — stage to analyse. Defaults to `implement`.
- `--thresholds LIST` — comma-separated token thresholds.
- `--top N` — sessions in the report. Defaults to 20.

## kontora start

`kontora start [--address HOST] [--port PORT]` — run the daemon in the
foreground. One instance per config directory; a second exits on the file lock.
Local only, and it does not return.

## kontora setup

`kontora setup [--agent]` — write the config file. Plain `setup` runs an
interactive wizard. `setup --agent` prints a brief for a coding agent and
writes nothing itself. Local only.

## kontora doctor

`kontora doctor` — check the config, `git`, `tmux`, each agent binary, each
project's repository, Plannotator and the web port. Exits non-zero when a hard
prerequisite fails; missing directories and an occupied port are warnings.
Local only.

## kontora config

`kontora config` — print the effective configuration with defaults applied and
`web.token` replaced by a placeholder. In remote mode it prints the daemon's
pipelines, agents and custom statuses instead.

`kontora config edit` opens the config in `$EDITOR`. Interactive: never run it
from an agent, and it is classified as a change.

## kontora skills

`kontora skills list` — print the reference topics, one line each.
`kontora skills list TOPIC` — print that topic's section headings.
`kontora skills show TOPIC` — print the whole topic as markdown.
`kontora skills show TOPIC SECTION` — print one section.

SECTION matches a heading exactly first, then as a unique case-insensitive
substring, so `kontora skills show cli new` prints `## kontora new`. An
ambiguous match prints the candidates and exits 1; so does an unknown topic.

Reads neither config nor ticket store, so it works in remote mode and before
`kontora setup`.

## kontora fmt

`kontora fmt` — read Claude stream-json on stdin and print it as readable text.
Takes no flags and talks to nothing.

## kontora completion

`kontora completion <bash|fish|zsh>` — print a completion script.

## kontora version

`kontora version` — print the version.

## kontora help

`kontora help` — print the command list. `kontora <verb> -h` prints that verb's
own usage and exits without doing anything. Write it with nothing after the
flag: `kontora note kon-1 -h` appends a note reading `-h` instead of showing help.
Use `kontora note -h`, without a ticket ID.
