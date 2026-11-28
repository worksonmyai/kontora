# kontora CLI

Use `kontora <command> [flags] [arguments]` to create tickets, control runs,
and inspect results. Both `-flag` and `--flag` work. `kontora <command> -h`
prints the command's synopsis, description and flags, and names its section in
this topic.

Put flags before positional arguments. For `new`, put them before the title;
for `edit`, before the ticket ID.
For `note` and `summary`, put all flags before the ticket ID, including when using stdin.
`new`, `note` and `summary` check the words of their text. A word that names
one of the command's own flags, such as `--status`, makes the command fail
instead of becoming part of the text.

Verbs that take a `TICKET_ID` accept a unique prefix, except for `result`,
which requires the exact ticket or automation ID of the invocation.
`kontora view kon-q` finds `kon-q88f`. A prefix matching two tickets is an
error naming both. Matching is from the start of the id only.

Exit codes: 0 on success, non-zero on failure with the reason on stderr. A
search that matches nothing still exits 0.

## Common tasks

Replace `TICKET_ID`, the `/absolute/path/...` values and the other
placeholders with real values.

Create a draft from a description file, read it, then start it:

```sh
kontora new --status open --quiet --path /absolute/path/to/repo \
  --description-file /absolute/path/brief.md "Add a health check endpoint"
kontora view --body TICKET_ID
kontora run TICKET_ID
```

`--quiet` prints only the new ID. Without `--status open` the ticket is `todo`,
and a daemon with `auto_pick_up: true` can start it before you read it.

Make one ticket wait for another:

```sh
kontora dep SECOND_ID FIRST_ID
```

`SECOND_ID` stays in `todo` until `FIRST_ID` is `human_review`, `done`,
`cancelled` or `archived`. `kontora ls --blocked` lists the `todo` tickets that
still wait.

Find out why a ticket stopped:

```sh
kontora view TICKET_ID
kontora view --last 3 TICKET_ID
kontora logs TICKET_ID
kontora activity TICKET_ID
```

`view` prints the status, stage, run history and last error. `--last 3` prints
the three newest notes. `logs` prints the newest stage log. `activity` prints
one run's transcript and needs a running daemon.

Review a ticket in `human_review`, then close it:

```sh
kontora view TICKET_ID
kontora changes TICKET_ID
git -C /absolute/path/to/repo worktree list
git -C /absolute/path/to/worktree status --short
kontora done TICKET_ID
```

`view` prints the repository `path` and the `branch`. Find the worktree of that
branch in `git worktree list`. `changes` shows committed work only and needs a
running daemon, so check the worktree for uncommitted and untracked files.
`done` does not commit, merge or push the work. The `kontora done` section
says what cleanup keeps.

Send a ticket back for another pass:

```sh
kontora note TICKET_ID < /absolute/path/feedback.md
kontora retry TICKET_ID
```

Write the feedback file with a file-writing tool. `retry` sets the ticket back
to `todo` at its current stage. In local mode with `auto_pick_up: false`,
follow it with `kontora run TICKET_ID`. A graph workflow at an `end` node
refuses `retry`. For such a ticket, use `kontora workflow-restart TICKET_ID`,
which starts the graph again at its entry node.

Start work later:

```sh
kontora schedule TICKET_ID --after 2h
kontora new --after 24h --path /absolute/path/to/repo "Rerun the nightly checks"
```

List the tickets in `human_review` or `paused`:

```sh
kontora ls --json --status human_review
kontora ls --json --status paused
```

## If you are a stage agent

When `KONTORA_TICKET_ID` is set, this process is the agent running that ticket's
current stage. You cannot change that ticket's status: `done`, `cancel`, `move`,
`pause`, `retry`, `skip`, `set-stage` and `schedule` aimed at it are refused, as
is `archive`. Kontora owns the next status. For unsupervised execution, exit 0 when
the stage's work is done and non-zero when it is not. Activated epic supervisors
and their children must also submit an explicit result; exit zero alone cannot
advance supervised work. Graph agents require result v2, or v3 with `wake`, even outside epic supervision.
See `kontora skills show cli 'kontora result'`.

You cannot release your own human wait or publish bulk deployment receipts from stage-agent context.
Use `kontora wait` only when the invocation permits it; publication does not directly change ticket status.

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
Graph agent nodes always require results: version 2, or version 3 with `wake`.

Automation runs always require a result. Pass the automation ID in place of the
ticket ID: `kontora result --input-file /absolute/path/result.json AUTOMATION_ID`.
A zero exit without a result is a failed run.

`--verdict VALUE` sets the verdict for result version 2 or 3. If the JSON also
has a `verdict`, the two must match.

Prefer Pi's `kontora_result({outcome, summary, next_step?, verdict?})` tool when available;
it is independent of checkpoint compaction and binds the current invocation.
Otherwise create a unique scratch JSON file outside the repository with a
file-writing tool, then use the CLI above. Put flags before the ticket ID. Do not
interpolate generated payloads into shell commands. Without `--input-file`, the
CLI reads stdin; the input-file path must be absolute.

```json
{"outcome":"blocked","summary":"Required device evidence is unavailable.","next_step":"Provide device access or explicitly change the requirement."}
```

Versions 1 and 2 accept `complete` and `blocked`. Version 3 also accepts `waiting`.
`summary` must be nonblank;
`blocked` also requires nonblank `next_step`. `complete` means this stage's assigned
work, not the whole ticket. A review stage can complete while fixes remain.
Fixture work alone is not complete when production foundations or other required
evidence remain. Optional `next_step` may be omitted for complete.
Versions 2 and 3 also accept `verdict`; completion requires one of the node's declared verdicts when configured.
Version 3 waiting requires nonblank `next_step` and a typed `wait`, and forbids `verdict`.
Other outcomes forbid `wait`. Fields except `wait` are strings.
The limit is 32 KiB of valid UTF-8 JSON. Field names are case-sensitive;
NULs, invalid Unicode, duplicate/unknown fields and trailing content are rejected.

The daemon supplies `KONTORA_RESULT_VERSION` (`1`, `2`, or `3`), `KONTORA_INVOCATION_ID`,
`KONTORA_RESULT_TICKET`, `KONTORA_RESULT_STAGE`, and absolute `KONTORA_RESULT_FILE`
after configured environment merging. Graph context also binds `KONTORA_WORKFLOW_EXECUTION_ID`,
`KONTORA_NODE_VISIT_ID`, `KONTORA_DEFINITION_FINGERPRINT` and JSON `KONTORA_RESULT_VERDICTS`.
Version 3 adds positive `KONTORA_WAKE_MAX` and `KONTORA_CHECK_TIMEOUT` (Go duration).
Do not select or override invocation
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

## kontora wait

```sh
kontora wait --check 'test -f DONE' --poll 10m \
  --deadline "$DEADLINE" --next "Read the collected data and report the result."
kontora wait --until "$VERIFY_AT" --next "Repeat the production checks."
```

Takes no positional arguments. Requires daemon-bound result version 3 and graph wake capabilities.
Ticket, result path and execution identity come from invocation environment, never a lookup.
It needs no config, daemon connection or lifecycle write permission. Do not set invocation variables yourself.

- Exactly one of `--check COMMAND` and `--until TIME` is required.
- Checks require positive `--poll DURATION` and absolute `--deadline TIME`.
- Timers reject `--poll` and `--deadline`.
- `--next TEXT` is required and nonblank.
- Optional `--summary TEXT` replaces the default timer/check summary.

CLI poll values use Go durations (`10m`, not `d`/`w`). Times use RFC 3339 with timezone,
including fractional seconds. Expired times remain valid; the daemon applies deadline/timer precedence.
Equivalent `kontora result` input:

```json
{"outcome":"waiting","summary":"Waiting for data.","next_step":"Read the data.","wait":{"kind":"check","command":"test -f DONE","poll":600000000000,"deadline":"2099-09-01T07:00:00Z"}}
```

A typed `WaitRequest` uses `kind: check` with `command`, positive `poll` and `deadline`,
or `kind: timer` with only `until`. JSON `poll` is integer Go-duration nanoseconds,
not a string, fraction or exponent. Times are RFC 3339 strings.
Unknown, duplicate, null, wrong-kind or incorrectly typed fields are rejected.

Pi exposes `kontora_wait({check?, poll?, deadline?, until?, next, summary?})` only for v3.
Poll/time parameters are strings in CLI format. It shares terminal-publication exclusion with `kontora_result`;
successful publication prevents later tool calls. The acknowledgement confirms immutable publication, not committed suspension.
Successful runner termination and required hooks must finish before the daemon parks the wait.
A conflicting result cannot replace the first terminal publication.

Wakes start fresh invocations at the same executable visit without failure retry or loop consumption.
Commands must be trusted and safe to repeat; daemon permissions apply without a sandbox.
See `kontora skills show pipelines 'Durable waits'` for probe outcomes and recovery.
Upgrade sharing clients/daemons before wait config, result v3 or state v2;
existing no-wait fingerprints and v1/v2 contracts remain compatible.

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

`KONTORA_URL` (or `--url`) points client commands at a daemon over HTTP.
Remote mode does not check local `tickets_dir`. Paths in `--path` name
directories on the daemon host.

Without a URL, commands that need the daemon, such as `run` and `changes`,
require `web.enabled` and connect over loopback. They use `web.token` if set.
Before acting, they check `/api/config` for the same ticket directory. A symlink
to the same directory counts as a match. If verification fails, the command
exits instead of falling back to ticket files.

For a daemon listening only on a non-loopback address, set `--url` or
`KONTORA_URL`, even on the daemon host. If the daemon requires a bearer token,
supply it with `--token` or `KONTORA_TOKEN`.

Supported: `ls`, `view`, `new`, `init`, `update`, `delete`, `run`, `schedule`,
`pause`, `retry`, `cancel`, `done`, `move`, `skip`, `set-stage`, `note`,
`summary`, `dep`, `undep`, `link`, `unlink`, `logs`, `activity`, `changes`,
`stats`, `workflow-restart`, `workflow-recover`, `release`, all `automation` children,
`review`, `annotate`, `config`, `attach`.

Rejected, because they act on local files: `edit`, `search`, `archive`,
`estimate-compaction`, `sessions`, `doctor`, `start`, `setup`.

Unaffected, because they read neither config nor daemon: `fmt`, `completion`,
`version`, `help`, `phase-complete`, `skills`, invocation-bound `result` and `wait`.

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

```sh
kontora ls --json
kontora ls --json --closed --archived
kontora ls --static --ready --project myrepo
```

`--ready` and `--blocked` cannot be combined. `--json` prints an array of
objects with `id`, `title`, `status`, `execution_active`, `kontora`, `path`,
`project`, `pipeline`, `stage`, `agent`, `created_at`, `started_at`,
`completed_at`, `scheduled_at`, `deps`, `links`, `parent`, `kind`,
`epic_execution`, `child_order`, `branch`, `base_branch`, `ready` and
`blockers`. `epic_execution: "agent"` identifies a supervisor.
`child_order` is stored display order, not membership. Read each child's `parent`
across the complete listing to find all children. The listing omits the ticket body.

The object omits an empty `path`, `project`, `pipeline`, `stage`, `agent`,
`parent`, `scheduled_at` or timestamp. `stage` is `standalone` for a ticket
without a pipeline. Kontora derives `ready` and `blockers` on every read.
`ready` means something only for a `todo` ticket. One object, for a paused
ticket:

```json
{
  "id": "app-q88f",
  "title": "Add a health check endpoint",
  "status": "paused",
  "execution_active": false,
  "kontora": true,
  "path": "/home/me/projects/app",
  "pipeline": "default",
  "stage": "implement",
  "agent": "claude",
  "created_at": "2026-11-27T09:00:00Z",
  "started_at": "2026-11-27T09:01:00Z",
  "deps": [],
  "links": [],
  "kind": "",
  "epic_execution": "",
  "child_order": [],
  "branch": "kontora/add-health-check-endpoint-app-q88f",
  "base_branch": "",
  "ready": true,
  "blockers": []
}
```

## Polling for ticket attention

Poll the daemon with a read-only `GET /api/tickets` request. Use the API because
`kontora ls --json` omits `waiting_for_input` in both local and remote mode.

Set `KONTORA_URL` to the daemon's web URL. Set `KONTORA_TOKEN` to that daemon's
bearer token. If the daemon requires no token, unset `KONTORA_TOKEN`. Use a URL
that the shell can reach directly: the snippet bypasses proxies to protect
loopback bearer tokens.

Run the Bash snippet with `curlie` and `jq` in an external shell at each polling
interval. The dashboard assistant can show the recipe. It refuses the snippet in
read mode and requires approval in ask and auto modes.

```bash
set -o pipefail
url=${KONTORA_URL:?Set KONTORA_URL to the daemon URL}
while [[ $url == */ ]]; do url=${url%/}; done
args=(--noproxy '*' --fail --silent --show-error --max-time 5 GET "$url/api/tickets")
if [[ -n ${KONTORA_TOKEN:-} ]]; then args+=("Authorization:Bearer $KONTORA_TOKEN"); fi
curlie "${args[@]}" | jq -ce '[.tickets[] | select(.status == "human_review" or .waiting_for_input == true)]'
```

Each successful poll prints a JSON array of tickets on the primary board in
`human_review` or with a live question reported by a Pi stage. It prints `[]`
when neither condition matches. A parked workflow does not imply a live question.
Polling can miss short-lived changes between reads. Use `hooks` for worktree
or stage actions. For scheduled agent runs, use Automations (see
`kontora skills show cli 'kontora automation'`).

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

```sh
kontora search -l --status paused 'timeout|deadline'
kontora search -F --body 'go test ./...'
```

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

Notification precedence is explicit request, saved ticket choice, project `notify`,
then `notifications.default_statuses` (initially unset). Lists do not merge.
Defaults also apply at explicit initialization/starts of unset older or imported tickets.
Saved choices, including `notify: []` for Off, survive default changes.

Older versions deleted Off choices; select Off again where needed and upgrade all
participating clients and daemons. See `kontora skills show config notifications`
for start operations, compatibility, and Pi-only `waiting`.

- `--path PATH` — repository the ticket works in.
- `--pipeline NAME` — pipeline to run, or `none` for a standalone ticket.
- `--agent NAME` — agent to use, or `none` to skip the project default.
- `--notify STATUSES`: comma-separated statuses. Omit to inherit project then global status defaults;
  `""` or `none` stores persistent Off (`notify: []`). Epic creation skips defaults.
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

Default output for a paused ticket. `stage:` lists the pipeline's stages and
puts the current one in brackets. `unknown/unverified` marks a run with no
recorded outcome:

```text
app-q88f  paused
Add a health check endpoint
pipeline:  default
stage:     [implement]
path:      /home/me/projects/app
branch:    kontora/add-health-check-endpoint-app-q88f
agent:     claude
started:   16h ago
attempt:   1

History:
  implement (claude) outcome: unknown/unverified · runner exit 1

⚠ Last error: agent exited with code 1

# Add a health check endpoint

Serve `GET /healthz` with status 200.
```

## kontora edit

`kontora edit TICKET_ID` — open the ticket file in `$EDITOR`. Local only, and
interactive: never run it from an agent.

## kontora update

`kontora update TICKET_ID [flags]` — change frontmatter fields or the body. At
least one flag is required. Empty values clear fields except `--notify ""`,
which stores persistent Off.

- `--body-file PATH` — replace the body from a file, `-` for stdin.
- `--pipeline NAME` — set the pipeline (`""` or `none` clears it).
- `--path PATH` — set the repository path.
- `--agent NAME` — set the agent override (`""` or `none` clears it).
- `--notify STATUSES`: set comma-separated statuses (`""` or `none` stores persistent Off as `notify: []`).
- `--notify-channels NAMES`: set comma-separated channels (`""` removes the override;
  `none` silences delivery).
- `--branch NAME` — set the work branch (`""` clears it).
- `--base-branch NAME` — set the base branch (`""` clears it).

```sh
kontora view --body TICKET_ID
kontora update TICKET_ID --body-file /absolute/path/body.md
kontora update TICKET_ID --pipeline default --base-branch release-2.0
```

Read the complete body before you replace it. Keep its title heading and its
`## Notes` section.

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
- `--stage NAME` — starting legacy stage. Graphs must start at `entry`; arbitrary starting nodes are refused.
- `--status STATUS` — `open` or `todo`. Defaults to asking.

```sh
kontora init TICKET_ID --path /absolute/path/to/repo --pipeline default --status open
```

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

```sh
kontora schedule TICKET_ID --at "2099-09-01 09:00"
kontora schedule TICKET_ID --after 3d
kontora schedule TICKET_ID --clear
```

Scheduling a `todo` ticket returns it to `open` and drops it from the queue.
Setting a schedule is refused on a running, closed or archived ticket, and while
a Plannotator session or a pending annotation owns it; `--clear` is refused only
while the ticket is running. `kontora run` clears the schedule and queues the
ticket now.

## kontora pause

`kontora pause TICKET_ID` — stop a running ticket and park it in `paused`. A
ticket that already closed cannot be paused. Refused on your own ticket.
On an agent epic, this stops only the supervisor. Running children continue.
On a durable graph wait, disarms progress without resetting its deadline.

## kontora retry

`kontora retry TICKET_ID` — reset an initialized ticket to `todo` with the
attempt counter cleared. Refused for `in_progress`, `todo`, `archived`, passive
epics, and your own ticket.

Graph retry preserves execution/visit identity and loop counts, with a fresh invocation;
retry at `end` is refused. For an active wait, retry re-arms the same wait after required recovery,
without resetting its deadline or bypassing release conditions.
For other work, local mode only updates the ticket file. With `auto_pick_up: false`, follow retry
with `kontora run TICKET_ID`. Remote mode and dashboard/API retry queue dependency-ready work.

## kontora workflow-restart

`kontora workflow-restart TICKET_ID` requires the daemon. Creates a new graph execution at entry,
resetting visits and loop counts, invalidating old wait IDs, and retaining history and worktree.
It does not undo prior effects. Stop execution and resolve required recovery first.

## kontora workflow-recover

`kontora workflow-recover --execution-id EXECUTION_ID --revision N TICKET_ID` requires the daemon.
Inspect processes, journals, results and worktree first. Send only the inspected execution ID and revision.
Stale state requires fresh inspection, not automatic retry. A missing window does not prove descendants stopped.
Acknowledgement does not launch work or reset counts; use retry separately.
Uncertain probe termination requires this acknowledgement before re-arming a wait, even after its deadline.
Recovery does not promise exactly-once shell effects or host-power-loss durability.

## kontora release

Both forms require the daemon and print the receipt ID. No offline fallback exists.
Inspect current detail through `GET /api/tickets/{id}` or the TUI before releasing a declared human wait.
Use `graph_execution.execution_id`, `graph_execution.wait.id` and `graph_execution.revision` from that state:

```sh
kontora release --execution-id EXECUTION_ID --wait-id WAIT_ID --revision N \
  --request-id APPROVAL_ID --reason "Approved" TICKET_ID
```

All five flags are required. Copy execution ID, wait ID and revision from the state inspected.
Use the original full ticket ID and stable request ID for retries.
Identical retries return the stored receipt; conflicting reuse or new stale requests fail.
A stage agent cannot release its own ticket.

Deployment release takes no ticket or human-release flags:

```sh
kontora release --repo /srv/projects/app --environment production \
  --rev "$DEPLOYED_COMMIT" --event-id "$DEPLOYMENT_ID"
```

All four flags are required. `--repo` must name a checkout on the daemon host,
not its `.git` or Git common directory. Use the ticket's captured worktree path.
`--rev` is the full lowercase commit ID actually deployed (40 or 64 hexadecimal characters).
Refs, `HEAD`, tags and abbreviated hashes are invalid. Capture the deployed commit during deployment;
do not resolve a possibly changed HEAD afterward. Deployment release is refused from stage-agent context.

Matching requires the same repository, exact environment and equal/descendant deployed commit, using local evidence only.
Kontora does not fetch, merge or deploy. Missing objects, unsupported Git options and uncertain ancestry fail closed.
Non-equal commits require complete, non-shallow history; cherry-picked content is not equivalent.
Historical receipts can satisfy later waits and several tickets. They do not prove current environment state after rollback.
Acceptance is persisted before acknowledgement, not proof of immediate continuation.
Existing daemon access controls apply; shared-token/unauthenticated access is not verified named approval.

Receipts/application journals live in `<tickets_dir>/.workflow-events/`, outside logs and reserved `.kontora`, without automatic expiry.
Keep wait state and invocation/probe journals for recovery. See `kontora skills show tickets 'Durable workflow waits'`.

## kontora done

`kontora done TICKET_ID` — move the ticket to `done`, stopping any running
agent and cleaning up its worktree. Refused when the ticket is the one this
process is a stage of; the pipeline sets the status on exit.

Cleanup removes the worktree only when it has no uncommitted or untracked
files. A worktree with such files stays for inspection. The branch always
stays. Kontora does not commit, merge or push the work.

## kontora cancel

`kontora cancel TICKET_ID` — move the ticket to `cancelled`, stopping any
running agent and cleaning up its worktree. Refused on your own ticket, like
`done`. Cleanup keeps the branch and any worktree with uncommitted or
untracked files, as for `done`.

## kontora move

`kontora move TICKET_ID STATUS` — move a ticket to any status the config
allows: `open`, `todo`, `paused`, `human_review`, `done`, `cancelled`, or a
custom status from `statuses:`. This is the only way to reach `human_review` or
a custom status from the command line. `archived` is not accepted; use
`kontora archive`. Refused on your own ticket, whatever the target status: the
run's history entry is lost with it.

```sh
kontora move TICKET_ID human_review
```

## kontora skip

Legacy lists only; graph skip is refused.
`kontora skip TICKET_ID` — intermediate skip sets the next stage to `todo`.
Daemon/API skip queues dependency-ready work even with `auto_pick_up: false`.
Disk-only skip does not enqueue; use `kontora run TICKET_ID` afterward when
automatic pickup is off. Final skip marks the ticket done without recording a
successful run.

Refused on your own ticket. Pause a running ticket first and wait for
termination. Close any active Plannotator review before changing stages.

## kontora set-stage

Legacy lists only; graph set-stage is refused.
`kontora set-stage TICKET_ID STAGE` — move the ticket to a named stage of its
pipeline without retrying it. Status and attempt stay unchanged; a `todo` ticket
remains eligible for pickup. Pause first and wait for termination. Close active
Plannotator reviews. Refused on your own ticket.

Graphs refuse `skip` and `set-stage`. Active waits also refuse run, move, schedule, init and done as bypasses.
Cancel invalidates wake authority and stops a probe. Delete/transfer require cancellation first;
upload strips resumable graph/wait authority.

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

A flag written after the ticket ID is refused.

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

```sh
kontora dep SECOND_ID FIRST_ID
```

`SECOND_ID` stays in `todo` until `FIRST_ID` is `human_review`, `done`,
`cancelled`, `archived` or legacy `closed`. See
`kontora skills show tickets Relations`.

## kontora undep

`kontora undep TICKET_ID DEPENDENCY_ID` — drop the dependency edge.

## kontora link

`kontora link TICKET_ID TICKET_ID...` — relate the first ticket to each of the
others, and not the others to each other. Both sides of each link are written.

```sh
kontora link TICKET_ID OTHER_ID THIRD_ID
```

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

```sh
kontora archive --days 30 --dry-run
kontora archive --days 30 --yes
```

## kontora logs

`kontora logs [--stage NAME] TICKET_ID` — print the agent's plaintext log.
Without `--stage` it shows the most recent one. Paged only on a terminal.
Graph logs retain `<node>.<visit-id>.<invocation-id>.log`, with older visit-log fallback.
Probe output is `<logs_dir>/<ticket>/waits/<wait-id>/<poll-id>.log`; wait projections retain a log reference.

```sh
kontora logs TICKET_ID
kontora logs --stage implement TICKET_ID
```

## kontora activity

`kontora activity [--stage NAME] [--run N] TICKET_ID` — print one stage run's
structured transcript: the agent's messages and tool calls, one per line.
Falls back to the plaintext log for agents that write no session record. Needs
a running daemon.

```sh
kontora activity --stage implement --run 2 TICKET_ID
```

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

```sh
kontora stats --range 1w --project myrepo
```

## kontora automation

`kontora automation COMMAND [flags]` manages scheduled agents without tickets.
An automation runs an agent on a saved prompt, repository and schedule. See
`kontora skills show config automations` for the YAML, schedule syntax, overlap
policies, workspaces and prompt data.
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

```sh
kontora automation preview --schedule "0 9 * * 1-5" --timezone Europe/Berlin
kontora automation new --name "Morning report" --path /absolute/path/to/repo \
  --agent claude --schedule "0 9 * * 1-5" --timezone Europe/Berlin \
  --prompt-file /absolute/path/prompt.md
```

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

`kontora setup [--agent | --recipe NAME]` — all setup modes are local-only.
Plain `setup` runs the interactive wizard to write a fresh config.
`setup --agent` prints the setup brief without writing a config.

Use `kontora setup --recipe NAME` to print only the named YAML fragment. Choose
`implement-review`, `plan-only`, or the opt-in graph `implement-test-review`.
Preview needs no terminal and does not read or change the config, create tickets,
or start work. Do not combine it with `--agent` or `--config`.

To install a recipe, replace `YOUR_AGENT` with a configured agent name in each step
or agent node. Merge the entries under `stages:` and `pipelines:` without replacing
existing entries. Rename collisions and update references to renamed stages. For
`implement-test-review`, replace the `tests` node's `exit 2` and the repair
prompt's `YOUR_TEST_COMMAND` with the same trusted test command. Use `0` for
pass, `1` only for repairable test failures, and another nonzero exit for other
failures. Keep one YAML document and run `kontora doctor` after the merge.

Select a recipe explicitly on a draft, for example
`kontora new --status open --pipeline implement-test-review "TITLE"`.
Run the example from the target repository's main checkout or pass `--path`.
Use `--pipeline plan-only` for a plan without implementation. If you renamed a
pipeline, select it by its new name.

## kontora workflow-simulate

`kontora workflow-simulate --recipe implement-test-review --scenario scenario.json`

`kontora workflow-simulate --config /path/to/config.yaml --pipeline NAME --preflight`

Choose one source (the built-in graph recipe or an explicit config and graph
pipeline) and one mode (`--scenario FILE` or `--preflight`). The command is
read-only and offline, ignoring `KONTORA_URL`. It never runs checks, hooks, Git,
or agents, and never opens a ticket store or makes worktrees or logs.

A scenario is a JSON array. Each event names an executable node or declared wait
in `node`. Executables accept `outcome: complete` or `failed` and an integer
`exit_code` from `0` to `255`. Checks require an exit code; agents default to
zero. Omitted outcome defaults to `complete` for zero and `failed` otherwise.
Checks route by exit code, reject `failed` with zero, and cannot supply a verdict.
An agent verdict must be declared. A successful agent must supply one if the node
declares verdicts. Wait events use `wait: released` or, when configured,
`wait: timeout`, with no completion fields.

The final line distinguishes end, waiting, missing input, and rejection. Only
rejection exits nonzero among those stops.

Preflight reports reachable incompatible condition inputs with a witness path.
Rejections and inconclusive exploration (more than 4,096 interpreter steps
needed) exit nonzero. It models normal completions and declared waits, not real
execution, suspension, or release authority. See the `pipelines` skill for a
starter scenario.

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
substring, so `kontora skills show cli "kontora new"` prints `## kontora new`.
An ambiguous match prints the candidates and exits 1; so does an unknown topic.
A bare `new` is ambiguous because it also matches `kontora automation new`.

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
synopsis, description, flags and section name in this topic, and exits without
doing anything. Put `-h` right after the verb: `kontora note kon-1 -h` fails
because `-h` comes after the ticket ID. Use `kontora note -h`, without a ticket
ID.
