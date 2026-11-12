# Tickets

A ticket is one markdown file with YAML frontmatter, in `tickets_dir`. There is
no database: the files are the state.

Never edit a ticket file by hand while the daemon runs. The daemon owns those
files and a hand edit races it. Use the CLI verbs, which go through the daemon
when `KONTORA_URL` is set.

Kontora lists every file with an `id`, whether or not it has `kontora: true`.
A file with no `id` is an ordinary note and is hidden. A ticket without
`kontora: true` shows as `not a kontora ticket` and no agent runs for it until
`kontora init` sets the flag.

## Example

```yaml
---
id: kon-q88f
kontora: true
status: todo
pipeline: default
path: ~/projects/kontora
created: 2026-02-25T19:39:45Z
---
# Add GoReleaser to kontora

Automate GitHub Releases with zig cc cross-compilation.
```

## User-set fields

Set at creation, by `kontora new`, `kontora init` or `kontora update`, or by
hand before the ticket runs.

| Field | Required | Description |
|-------|----------|-------------|
| `id` | yes | `<prefix>-<4 alphanumerics>`, e.g. `kon-q88f`. |
| `status` | yes | Defaults to `todo` for ordinary tickets. A passive epic derives status from children; an agent epic tracks its supervisor. |
| `kind` | no | `epic` for a shared brief that groups tickets. Absent for ordinary work. |
| `epic_execution` | no | Empty for passive grouping; `agent` after explicit activation. Invalid on ordinary tickets. |
| `path` | yes | Repository the ticket works in. `~` is expanded at runtime. |
| `pipeline` | no | Configured pipeline name, stored at creation or initialization. Empty means standalone with the ticket's agent or `default_agent`. |
| `agent` | no | Agent override for this ticket. Replaces the pipeline's agent at every stage. |
| `base_branch` | no | Branch the work branch starts from. Empty means the repository's default branch. |
| `created` | no | RFC 3339 timestamp. Set by `kontora new`. |
| `scheduled_at` | no | RFC 3339 instant at which the daemon moves an `open` ticket to `todo`. See the scheduled pickup section. |
| `deps` | no | Ids this ticket waits on. |
| `links` | no | Ids of related tickets. |
| `parent` | no | Epic id. Set with `kontora new --parent ID`, or change through the web UI or parent API. |
| `children` | no | An epic's preferred display order of child ids. Membership comes from each child's `parent`. |

For ordinary tickets, creation and initialization store resolved defaults in
frontmatter. Pipeline precedence is explicit request, stored nonempty pipeline
(initialization only), project `epic_child_pipeline` for an actual epic child,
project `pipeline`, global `default_pipeline`, then standalone. Agent resolution
is independent, including when `--pipeline none` opts out of pipeline defaults.
`none` stores a blank, not durable opt-out provenance; later initialization can
fill it unless the caller opts out again.
Passive epics skip these defaults at creation. They reject pipelines but accept an explicit agent without starting it.
Activation resolves the supervisor agent, but never assigns a pipeline.

Any field not listed here is preserved through a round trip. The daemon does
not remove or overwrite fields it does not recognise.

## Scheduled pickup

`scheduled_at` is the instant an `open` ticket becomes `todo`:

```yaml
status: open
scheduled_at: "2099-09-01T07:00:00Z"
```

Write it with `kontora schedule TICKET_ID --at TIME | --after DURATION`, or
create it that way with `kontora new --after 24h`. `--at` takes RFC 3339 or a
local wall time such as `"2099-09-01 09:00"`; `--after` takes Go's duration
units plus `d` and `w`, so `3d` and `2w` work. Both normalize the instant to
UTC and refuse one already in the past. `kontora schedule TICKET_ID --clear`
removes it and leaves the ticket open.

At the deadline the daemon writes `status: todo` and removes `scheduled_at` in
one save. It does not bypass the usual rules: with `auto_pick_up: false` the
ticket waits in `todo` for an explicit `kontora run`, and an unresolved
dependency keeps it out of the queue until that dependency closes.

Schedules are rebuilt from the ticket files at startup, so one whose deadline
passed while the daemon was down is promoted when it comes back. A value the
RFC 3339 parser rejects is left alone, so a typo leaves the ticket open.

`kontora run` on a scheduled ticket clears the timestamp and queues it now. So
does every other move off `open`: `retry`, `skip`, `move`, an `init` that queues
the ticket, and parking it for an annotation run. An uploaded `.md` file loses
any `scheduled_at` it carries.

## Daemon-managed fields

Written by the daemon as a ticket progresses. Never set them by hand.

| Field | Description |
|-------|-------------|
| `kontora` | `true` makes ordinary tickets and activated agent epics eligible to run. Set by `kontora new` or `kontora init`. |
| `stage` | Current pipeline stage. Set to the first stage on pickup. |
| `attempt` | Retry counter for the current stage. Reset on advance or back. |
| `started_at` | When the current stage started. |
| `completed_at` | When the ticket became `done`. |
| `branch` | The worktree's git branch. |
| `history` | One record per completed stage run. |
| `last_error` | Message from the last failed stage run. |
| `summary` | Summary of the run that ended most recently. Cleared when a new stage starts. |
| `final_summary` | Summary of the whole ticket, written after the pipeline ends successfully. |
| `claimed_by` | The daemon `instance_name` that last picked the ticket up. |

## Status lifecycle

This lifecycle describes ordinary tickets. Passive epics summarize their children.
Agent epics track their supervisor and finish successful runs in `human_review`.
See `kontora skills show tickets Epics`.

```
open -> todo -> in_progress -> done ------> archived
                    |                          ^
             paused / cancelled ---------------+
```

| Status | Meaning |
|--------|---------|
| `open` | Drafted, not ready for the daemon to pick up. A `scheduled_at` stamp moves it to `todo` at a set time. |
| `todo` | Ready. The scheduler takes it in creation order once its deps are closed. |
| `in_progress` | An agent is working on it now. |
| `paused` | Stopped by a failure policy or by a person. `kontora retry` resumes it. |
| `human_review` | Parked for a person to look at. Reached by a stage policy or `kontora move`. Releases the tickets that depend on it. |
| `done` | Every pipeline stage completed. |
| `cancelled` | Cancelled by a person. |
| `archived` | An old closed ticket, hidden everywhere, file kept. Terminal. |
| `closed` | Read-only compatibility with the external `ticket` CLI. No kontora command writes it. |

A config may declare extra parked statuses under `statuses:`. A stage's
`on_success` or `on_failure` can park a ticket in one, and `kontora move` can
put a ticket there.

The daemon picks up ordinary tickets and agent epics with `kontora: true` and
`status: todo`. A passive epic is never picked up, even if its file says `todo`.

## Relations

`deps` names the tickets this one waits on. The scheduler holds the ticket in
`todo` until every id in `deps` names a ticket that is `human_review`, `done`,
`cancelled`, `archived` or legacy `closed`. `human_review` releases dependents
because the agent's work is done and only a person's verdict is left. Any other
status blocks, and so does an id with no file behind it.

`links` is a symmetric "related" list and means nothing to the scheduler.
`parent` names the epic that groups a ticket. Create a child with
`kontora new --parent EPIC_ID`. To move an existing ticket into or out of an
epic, use the web UI or the parent API. There are no CLI parent/unparent verbs.

`blocks` is the reverse of `deps`. Child membership is the reverse of `parent`.
Both are derived on read. The epic's stored `children` list controls display
order only: it does not add members or set execution order. Use `deps` for
execution prerequisites.

Readiness is derived on every read. It is not a status: there is no `blocked`
status, and a ticket waiting on a dependency stays `todo`. Closing a dependency
queues every ticket that waited on it, with no edit to their files.

A cycle never starts an agent: every ticket on it waits on one that is not
closed. `kontora dep` refuses to create a new cycle.

Write these with `kontora dep`, `kontora undep`, `kontora link` and
`kontora unlink`.

## Epics

Use an epic for a shared brief spanning separately executable tickets. Its body
holds the goal, design, decisions and success criteria. Each child remains an
ordinary ticket with its own agent and pipeline.

Project `epic_child_pipeline` applies to ordinary children of any actual epic,
passive or agent, independently of supervisor activation. Missing or ordinary-ticket
parents do not qualify. Initialization resolves actual membership before filling
a blank pipeline. Existing assignments are not migrated after config changes,
reparenting or detail-path edits; operators must retarget them explicitly.

API `epic_child` is an ephemeral preview snapshot, not frontmatter or authoritative
default resolution. Browser inheritance omits `pipeline`; standalone sends `none`.
The backend resolves the current default at submission.

Child-agent prompts include the epic id, title and a command to read its brief.
The agent must run `kontora view --body EPIC_ID` before starting work. The brief
and notes are not inlined. This applies to pipeline stages, standalone runs and
resumed runs. Missing parents and parents that are not epics add no context;
an empty prompt stays empty.

An epic has `kind: epic` and no pipeline. It starts passive, with no
`epic_execution` value, worktree or run history. Agent and branch settings can be stored before activation. Existing epics
stay passive until explicitly activated. Epics cannot nest.

```sh
kontora new --kind epic --status open --path ~/projects/myrepo --description-file brief.md "Add authentication"
kontora new --parent EPIC_ID --status open "Implement the login API"
```

Replace `EPIC_ID` with the id returned by the first command. Do not pass
`--pipeline`, `--at`, `--after` or `--parent` when creating an epic.
The children remain open until explicitly queued.

Supervisors creating children locally should pass `--parent` and omit `--path`
and `--pipeline`, so Kontora selects the configured default.
The child inherits the resolved parent epic's stored `path`, not the current git root.
An explicit `--path` wins; use the epic's stored repository path, not its integration worktree.
Remote and API creation still require a daemon-host path, even with a parent.

The daemon derives a passive epic's status whenever its children change.
Archived children are ignored. Apply these rules in order:

| Remaining children | Epic status |
|--------------------|-------------|
| Any `todo`, `in_progress`, `paused` or `human_review` child | `in_progress` |
| No children, or any child outside `done`, `closed` and `cancelled` | `open` |
| All children terminal, with at least one `done` or legacy `closed` child | `done` |
| All children `cancelled` | `cancelled` |

A passive epic refuses retry, pause, skip, schedule and manual status changes.
Its brief stays editable in every status through
`kontora update EPIC_ID --body-file FILE`. Archiving stops status derivation;
restoring through the web UI resumes it.

Activate the epic with `kontora run EPIC_ID`, the dashboard's Run action, or
explicit `kontora init EPIC_ID --status todo`. Activation writes
`epic_execution: agent` and queues a supervisor. Creation and draft edits never
activate it. The epic and every child must use the same Git repository.
Linked worktrees and symlink paths to that repository are allowed. Passive
epics can group different repositories until activation.

The supervisor runs standalone, without a pipeline, as an ordinary coding agent
in the epic's own branch and worktree. It reads the brief and children, creates open children, starts selected
work, monitors progress and integrates results. Children keep their own
pipelines, branches, worktrees and commits. Pausing the epic stops only its
supervisor. Children already running continue; pause them separately if needed.
Retry requires explicit operator action and must inspect saved decisions,
existing child results, worktrees, dirty files, and interrupted merges or rebases
before continuing. Saving an answer, reading a notification, or dismissing
attention does not restart work. Agent epics use ordinary edit restrictions while running or terminal.

A complete supervisor result with clean execution, confirmed termination, and
successful final validation parks the epic in `human_review`, never directly
in `done`. Kontora requires a nonempty child set, with every child `done`,
legacy `closed` or `human_review`. Cancelled and archived children do not pass.
That check does not prove integration, verification claims or human acceptance.
Review the combined epic diff. Plannotator feedback starts supervisor rework.
Every supervisor run records history, summaries, logs, activity and session
references on the epic.

Supervisor prompts require workflow preservation:

- Do not change, clear or replace a child's pipeline.
- Do not edit pipeline definitions, stage definitions or project pipeline defaults.
- Create children without `--pipeline` so Kontora selects the configured default.
- Do not bypass workflows through `init`, stage skipping, direct file edits or pause/retry sequences.
- If configured work conflicts with your authority, do not start it. Report the conflict and request an operator decision.
- Do not substitute another pipeline.

These are prompt instructions, not runtime enforcement. Ordinary operator commands
remain available. Pipeline defaults do not authorize commits or publication;
`commit-no-push` still contains a commit stage.

Dependencies control scheduling, not Git contents. Verify prerequisite commits
exist in a child's base before starting it. Set `base_branch` before first
execution; changing it later does not rebase an existing worktree. Serialize
integration writes. Never modify a running child's worktree.

The local-delivery exception applies to the activated supervisor and its actual
children. Verify `kind: epic` and `epic_execution: agent` in the epic file, and
`parent` in the child file; a prompt claim is not proof. Do not change fields to
qualify yourself. Display order and whether the supervisor is running do not
matter. Annotation-only runs get no implementation or commit authority. Narrower
user, ticket, and stage instructions win, including “do not commit”.

On its own verified branch a supervisor or child may implement, test, and commit without
per-operation approval. Before each commit, merge, or rebase, verify the current
branch equals the current ticket's `branch`, HEAD is not detached, and the working
directory is the invocation's worktree. Resolve detached HEAD or unclear ownership
before writing. Never commit, merge, rebase, or reset on `main`, `master`, or the
default branch. Rebase only your own verified branch onto `base_branch` (or the
default branch if unset). The supervisor may merge child branches into its own
branch, not rebase children. A child may merge the epic's branch or its own
`base_branch` into its own verified branch. Never modify a running child's worktree.
Do not discard, stash, or overwrite work this run did not create. Do not rewrite
commits present on a remote or the default branch, or published or shared history.

Declared project dependencies and tools at an exact version named by the project
or ticket may be installed in the worktree or a temporary directory; package-manager
caches are allowed. Global installation, real-account access (except read-only
lookups), device access, and service exposure need approval. For setup requests,
name the package/version, destination, and purpose.

The supervisor and children must investigate questions and read saved decisions
before asking. Choose safe, reversible defaults within scope and record important
assumptions in epic notes. Resolve permitted local setup and continue independent
authorized work. For an unavailable prerequisite, ask a concrete question naming
the missing resource, authority, or scope decision, with attempted checks, options,
costs, and a recommended default. Save the question and answer together immediately
on the current ticket, preserving selected options and free text and distinguishing
operator answers from assumptions; confirm persistence before continuing. Do not
silently weaken acceptance requirements or substitute emulation for required
real-device evidence. If no authorized work remains, submit blocked and stop.

Supervisors and their actual children must submit an invocation-bound `complete`
or `blocked` result for the current stage, including resumed and rework execution,
independently of a custom `system_prompt`. Stage completion is not whole-ticket
acceptance. Missing/invalid reports after clean execution pause the same stage;
notes and exit zero are not substitutes. Unrelated tickets, passive-epic children,
annotations, and assistant-only runs retain their existing requirements. See
`kontora skills show cli 'kontora result'` for schema, environment, and transports.

The supervisor must not publish or make off-machine writes, including through
children, scripts, or tools. This prohibits pushes, pull requests, remote issues
or comments, messages, uploads, and releases. Read-only external lookups are
allowed. Results and progress notes stay local.

Supervisors use `max_concurrent_epic_supervisors`, default `4`, separately from
`max_concurrent_agents`. This allows four additional agent processes. The queue
chooses the oldest eligible ticket whose pool has capacity.

Upgrade all clients and daemon binaries that read the config before adding
`epic_child_pipeline`; older strict-YAML loaders reject the key.
Upgrade every daemon and client sharing the ticket directory before activation.
Older versions treat all epics as passive and can overwrite supervisor status.
Do not run mixed versions against activated epics.

The web UI provides these relation operations. The API equivalents are:

- `POST /api/tickets/{id}/parent` with `{"parent":"EPIC_ID"}` sets a child's parent.
- `POST /api/tickets/{id}/unparent` clears a child's parent.
- `PUT /api/tickets/{id}/children` with `{"children":["CHILD_ID"]}` sets an epic's display order.

The parent must exist and be an epic. Reordering does not modify child tickets
or create dependencies. Deleting an epic through the daemon keeps its children
and clears their parent references. Local `kontora delete` only removes the
epic file; use remote mode for that cleanup.

On desktop, create an epic from New ticket by choosing Kind: epic. Epics appear
above Projects in the sidebar, not in board columns. The sidebar defaults to
hidden, and its Epics section is hidden when the board has no epics.
`kontora ls --json` includes `kind`, `epic_execution`, `child_order`, `branch`
and `base_branch`. `child_order` is the stored display order, not child membership.
Use `kontora ls --json --closed --archived` without additional filters or a limit.
Inspect each ticket's `parent` to find every child.

## Body

Everything after the closing `---` is markdown. The first `# Heading` is the
ticket title, available to a stage prompt as `{{ .Ticket.Title }}`; the whole
body is `{{ .Ticket.Description }}`.

`kontora note` appends an entry under a `## Notes` heading. That is how one
stage leaves something for the next, because the next stage's prompt usually
includes the whole body.

Kontora writes each note with a bold byline of timestamp, author and a 4-character id,
`**2026-03-06T12:00:00Z · claude · q88f**`, with `· re:<id>` on a reply and
`· edited` on one whose text was replaced.

Outside fenced code, a bold-only line opens a note only under either rule:

- Its first `·`-separated field starts with a `YYYY-MM-DD` date.
- It matches the full `at · author · id` grammar, with only optional
  `re:<id>` and `edited` flags after the id. Both `at` and author may be empty,
  as in `** ·  · cd34**`.

A bare date-led timestamp still opens a legacy note without an author or stored
id. Other bold-only lines, such as `**Verification:**`, remain note text.

Adding or editing a note shifts headings outside fences only when the shallowest
heading is h1 or h2. That heading becomes h3. Relative levels stay the same,
capped at h6. Notes whose shallowest heading is h3 or deeper need no shift.
Backtick and tilde fenced code stays unchanged; Kontora appends a closing fence
if needed.
Byline-like bold lines outside fences use outer `__` instead of `**` when stored.
Both render as bold. Outside fenced code, a line beginning with `# ` or `## `
at column zero ends the Notes section. Inside fences, those lines do not end
Notes, and bold-only lines do not open notes.

Existing tickets are not migrated. Full-body writes do not normalize note text.
Old sections after Notes stay visible in the description editor's read-only area.
`kontora view --notes TICKET_ID` prints from `## Notes` through the body's end.
`kontora view --last 5 TICKET_ID` starts at the beginning of the fifth-last note
and keeps later sections. Legacy notes without bylines also count.
`--last N` implies `--notes` and requires a positive integer.
If N exceeds the note count, output includes the whole section and `## Notes`.
Without a Notes section, both commands print nothing and exit 0. Neither flag
can be combined with `--body`. Both views are unpaged in local and remote mode.

Reactions live in `<ticket-id>.notes.json` beside the ticket file, not in the
body, so what you receive in a prompt stays readable.

## Ticket ID format

`<prefix>-<4 random alphanumerics>`, e.g. `kon-q88f`.

The prefix is derived from the directory name in the ticket's `path`: the first
alphanumeric of each `-` or `_` separated segment, lowercased and joined. A
name yielding fewer than two of them uses its own first three alphanumerics
instead. `kontora` gives `kon`, `deployment_tools` gives `dt`, `sigil` gives
`sig`. A project can pin a prefix with `prefix:` in its config entry.

## History

Each finalized stage run appends a record:

```yaml
history:
  - stage: plan
    agent: claude-opus
    model: haiku
    effort: low
    exit_code: 0
    started_at: 2026-03-01T10:01:00Z
    completed_at: 2026-03-01T10:05:00Z
    session_kind: claude
    session_ref: 2f1e0c7a-9b3e-4d21-8f77-0c1a5e6b2d44
```

Supervised records may also have `run_outcome`, `outcome_reason`, and
`invocation_id`. `exit_code` is runner exit status (synthesized for interactive
Claude), not task delivery. `-1` means recovery could not observe the runner
status. The semantic outcome is final after hooks and child
checks; blocked with exit zero is not completed delivery. Legacy entries without
`run_outcome` remain unverified task outcomes: do not infer completion from their
exit codes or prose. Summaries, statistics, metrics, and CLI/browser views must
preserve that distinction. A standalone supervised child's real history is not
also counted as a synthetic legacy run.

`model` and `effort` are what the run resolved to, not what the agent reported
running as. `session_kind` and `session_ref` name the agent's own session file;
`kontora sessions` turns them back into paths. Both are absent for an agent
that is neither claude nor pi.
