# Ticket Format

Tickets are markdown files with YAML frontmatter. They live in the directory specified by `tickets_dir` in the [configuration](configuration.md). Any tool that can write a markdown file — a text editor, Obsidian, a script — is a valid client.

For your first ticket and run, follow [Getting started](getting-started.md). This page describes the ticket file format and lifecycle.

Kontora lists every valid ticket file that has an `id`, even when the file does not have `kontora: true`. Files without an `id` are treated as ordinary notes and are hidden. Tickets without `kontora: true` are shown with a `not a kontora ticket` marker and must be initialized before Kontora will run an agent for them.

Deleting the markdown file removes the ticket from the daemon and web UI, but does not clean up any git worktree. If you want the normal cleanup path, cancel the ticket before deleting its file.

## Example

```yaml
---
id: kon-q88f
kontora: true
status: todo
path: ~/projects/kontora
created: 2026-02-25T19:39:45Z
---
# Add GoReleaser to kontora

Automate GitHub Releases with zig cc cross-compilation.
```

## Description editing

The ordinary ticket editor and epic brief editor change only the text before the first `## Notes` line. Surrounding whitespace on that line is ignored. The editors show everything from that heading onward as read-only content, including later sections. Use note actions to change notes. Use `kontora update TICKET_ID --body-file FILE` to edit later sections or intentionally replace the entire body, including notes.

Description saves merge with the current stored tail inside a transaction. They preserve note formatting and identities even if the browser has not received a note event. Saves for the same ticket remain ordered when its editor closes and reopens. Acknowledgements do not replace text typed after submission. Failed drafts remain in the current browser page; reloading the page loses them. Separate browser sessions do not detect conflicting description edits.

The whole body, including notes and later sections, remains available to stage prompts.

## Claim from a secondary folder

Configured [secondary folders](configuration.md#secondary-ticket-folders) appear on
the dashboard beside primary tickets. Source labels distinguish tickets with the
same ID. Secondary tickets support description, execution settings, notes, reactions,
summary, and folder-local relationship edits in every status. They cannot run,
change status or stage, schedule, initialize, archive, restore, delete, or open an
agent-backed review through this daemon. Another daemon that owns the source as
primary can still react to edits.

Claim moves one ticket and its reaction sidecar into primary storage under the same
ID. The browser saves the editor first. If saving fails, Claim stops and retains the
draft. Claim requires the revision the browser reviewed and refuses:

- Active execution, including custom running statuses, stale `in_progress`, invocation markers, or pending annotations.
- Managed tickets or controlled source boards.
- Symlinked or nonregular ticket and sidecar files.
- Destination ticket, reaction, log, or reserved execution identities.
- Changes to the source since the reviewed revision.

A stale `claimed_by` alone does not prevent Claim. Claim sets the destination to
`open` and clears runtime, scheduling, archive, and current error/log fields.
It preserves enrollment, custom fields, relationships, summaries, notes, and history.
Claim never starts an agent. A passive epic resumes normal derived-status handling
in primary storage.

Claim does not move dependencies, links, parents, or children. Links, parents, and
children remain folder-local. Primary dependencies can resolve a unique match in a
configured secondary folder. The confirmation warns about missing references and primary namesakes.
Imported history retains its summaries and timestamps, but does not use primary logs
or costs. `imported_history_count` marks that history prefix. Later local stage runs
keep distinct indexes. Source worktrees, logs, sessions, and journals are not moved.

Transfers use ordered local locks, private backups, and no-overwrite publication.
Startup recovers pending transfers before primary discovery. Unresolved conflicts
retain recoverable files, exclude the destination identity from execution, and log
the transfer location for investigation. Do not remove these backups before resolving
the conflict.

All local writers must use an updated Kontora binary for coordination. Older binaries,
arbitrary editors, and independently synced machines can bypass the locks. Claim
checks stored markers, not remote process health; manually clearing runtime markers
can defeat that protection.

## Frontmatter fields

### User-defined fields

Set these fields when creating a ticket, manually or with `kontora new`. Both `kontora new` and `kontora update` accept `--notify` and `--notify-channels`. The dashboard, [API](#notifications), and hand edits also support these fields.

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `id` | yes | — | Unique identifier, format `<prefix>-<4 alphanum>` (e.g., `kon-q88f`). |
| `status` | yes | `todo` | Current ticket status (see [Status lifecycle](#status-lifecycle)). On a passive epic it is derived from the children. On an agent epic it tracks the supervisor. |
| `kind` | no | — | `epic` for a ticket that groups others; absent for ordinary work. See [Epics](#epics). |
| `epic_execution` | no | — | Empty for a passive epic; `agent` after explicit activation. Invalid on ordinary tickets. See [Epics](#epics). |
| `pipeline` | no | — | Configured pipeline name, stored at creation or initialization using the [default precedence](configuration.md#projects). Empty means standalone execution with the ticket's agent or `default_agent`. |
| `agent` | no | — | Override the agent for this ticket. Applies to standalone tickets or overrides the pipeline's agent at every stage. Filled in at creation from a matching [project](configuration.md#projects) when one is configured and no agent is given. |
| `path` | yes | — | Path to the repository (supports `~`, e.g., `~/projects/kontora`). |
| `base_branch` | no | — | Branch the ticket's work branch starts from. Empty means the repository's default branch. See [Base branch](#base-branch). |
| `created` | no | — | RFC 3339 timestamp. Set automatically by `kontora new`. |
| `scheduled_at` | no | — | RFC 3339 instant at which the daemon moves an `open` ticket to `todo`. See [Scheduled pickup](#scheduled-pickup). |
| `deps` | no | — | Ids of the tickets this one waits on. The scheduler holds the ticket back until every one of them is resolved; see [Relations](#relations). |
| `links` | no | — | Ids of related tickets. See [Relations](#relations). |
| `parent` | no | — | Id of the epic this ticket belongs to. See [Relations](#relations). |
| `children` | no | — | On an epic, the manual order of its sub-tickets. Order only; membership is what each child's `parent` says. See [Epics](#epics). |
| `notify` | no | — | Statuses whose arrival this ticket asks to be told about. Without it the ticket is silent. See [Notifications](#notifications). |
| `notify_channels` | no | — | Channels this ticket's notifications go to, above the project's and the global default. See [Notifications](#notifications). |

#### Relations

`deps`, `links` and `parent` name other tickets. Kontora reads them and shows each id as a link to that ticket, with a hover card giving its title and status. The details rail lists every relation as its own row. The ones you navigate by are also on the ticket tab itself: `deps`, `links` and the derived `blocks` on a strip at the top, one line unless a ticket carries too many ids to fit, and `parent` as a crumb in the app header, between `board` and the open ticket's id.

`deps` and `links` are written by [`kontora dep`, `kontora undep`, `kontora link` and `kontora unlink`](cli.md#relations). `parent` is written by `kontora new` with a parent flag, by the epic page, or by hand. Only `deps` controls scheduling, through [dependency-aware scheduling](#dependency-aware-scheduling). `parent` supplies epic context and can select a creation or initialization default; it does not set execution order.

```yaml
deps: [kon-a1b2]
links: [kon-c3d4, kon-e5f6]
parent: kon-9zzz
```

Both the flow form above and a block sequence are read. An id with no ticket file behind it stays on screen as plain text: the frontmatter still points at it, and there is nothing to open.

Two more relations are derived on read rather than stored, because each edge is written on the other ticket alone. `blocks` is the reverse of `deps`: the tickets whose `deps` name this one. `children` is the reverse of `parent`: the tickets whose `parent` names this one. They are read by scanning every ticket file, so both cover tickets the board hides.

`children` renders as a sub-ticket tree at the top of the ticket tab, one row per child with its title, id, stage and elapsed time, and a `3 of 5` rollup counting the children that are `done`. The tree draws 12 rows before it offers a reveal for the rest, and a click on a row opens that child. A ticket with no children draws no tree.

#### Epics

An epic has `kind: epic` and no pipeline. Its body holds the shared brief, design, decisions and success criteria for its children.

An epic starts passive, with no `epic_execution` value. A passive epic runs no agent and creates no branch, worktree or run history. Its status summarizes its children. Existing epics stay passive until explicitly started.

```yaml
id: kon-e7c1
kontora: true
kind: epic
status: in_progress      # derived, written back by the daemon
path: ~/projects/kontora
children: [kon-b12e, kon-c04d, kon-9b31]   # order only
```

Create an epic and an open child:

```bash
kontora new --kind epic --status open "Add authentication"
kontora new --parent EPIC_ID --status open "Implement the login API"
```

Replace `EPIC_ID` with the ID printed by the first command. To file an existing ticket under an epic, use `POST /api/tickets/{id}/parent`. Both child creation and reparenting require an existing epic. Epics do not nest: setting `parent` on a ticket whose `kind` is `epic` is refused.

An ordinary child of any actual epic can use the matching project's
`epic_child_pipeline`. This applies under passive and agent epics, independently of
supervisor activation. Missing or ordinary-ticket parents do not qualify.

Pipeline precedence at creation or initialization is:

1. Explicit request, including `none` for standalone execution.
2. Stored nonempty pipeline, during initialization only.
3. Project `epic_child_pipeline`, for an actual epic child.
4. Project `pipeline`.
5. Global `default_pipeline`.
6. Standalone execution.

Kontora stores the resolved pipeline in the ticket, not a live link to a default.
Existing assignments are not migrated after config changes, reparenting or detail-path
edits. Operators must retarget them explicitly. `none` stores a blank, not durable
opt-out provenance. A later initialization can fill that blank unless the caller
opts out again. Agent resolution stays independent of the pipeline opt-out.

`epic_child` in API responses is an ephemeral membership snapshot for previews.
It is not frontmatter or authoritative default resolution. Browser inheritance omits
`pipeline`; explicit standalone sends `none`. Creation and initialization resolve the
current default at submission.

For a child ticket, Kontora adds the epic id, title and a brief-reading command to each non-empty agent prompt. This includes pipeline stages, standalone runs and resumed runs. The agent is instructed to run `kontora view --body EPIC_ID` before starting and keep its work scoped to the child. The brief and notes are not copied into the prompt. Missing parents and parents that are not epics add no context.

The daemon rewrites a passive epic's status whenever its children change. Archived children are ignored. Any child in `todo`, `in_progress`, `paused` or `human_review`, or active in a custom status, gives `in_progress`. Otherwise, terminal children with at least one `done` or legacy `closed` give `done`. All remaining children `cancelled` gives `cancelled`; anything else, including no children, gives `open`.

A passive epic's brief stays editable in every status. Manual status changes, pause, retry, skip and schedule are refused. Archiving stops status derivation; restoring resumes it.

To activate a supervisor, use `kontora run EPIC_ID`, the dashboard's Run action, or explicit `kontora init EPIC_ID --status todo`. Activation writes `epic_execution: agent` and queues the epic. Creating an epic, editing its brief or changing its children does not activate it. The epic and every child must name the same Git repository. Linked worktrees and symlink paths to that repository are allowed. Passive epics can group different repositories until activation.

The supervisor runs standalone, without a pipeline, using an ordinary coding agent
in the epic's own branch and worktree. It reads the brief and children, creates open children, starts selected work, monitors progress and integrates child results. Children keep their own pipelines, branches, worktrees and commits. Display order does not set execution order.

Supervisor prompts instruct it to preserve child workflows:

- Do not change, clear or replace a child's pipeline.
- Do not edit pipeline definitions, stage definitions or project pipeline defaults.
- Create children without `--pipeline` so Kontora selects the configured default.
- Do not bypass workflows through `init`, stage skipping, direct file edits or pause/retry sequences.
- If configured work conflicts with your authority, do not start it. Report the conflict and request an operator decision.
- Do not substitute another pipeline.

These are prompt instructions, not runtime enforcement. Ordinary operator commands
remain available. A default pipeline grants no permission to commit or publish;
`commit-no-push` still contains a commit stage.

Pausing an agent epic stops only its supervisor. Children already running continue; pause those children separately if needed. Retry requires explicit operator action. The next supervisor execution must inspect saved decisions, existing child results, worktrees, dirty files, and interrupted Git operations before continuing. Saving an answer, reading a notification, or dismissing attention does not restart work. Agent epics use ordinary edit restrictions while running or terminal.

A complete supervisor result with clean execution, confirmed termination, and successful final validation sends the epic to `human_review`, never directly to `done`. Kontora requires at least one child, with every child `done`, legacy `closed` or `human_review`. Cancelled and archived children do not satisfy that check. The check does not prove integration or test claims, and a child's `human_review` does not mean human acceptance. Review the combined epic diff. Plannotator feedback starts another supervisor run for rework. Supervisor runs record history, summaries, logs, activity and session references on the epic.

Git integration remains agent work. Dependencies control scheduling; they do not put prerequisite commits into a child's base branch. Set `base_branch` before first execution where needed. Changing it later does not rebase an existing worktree. The supervisor must serialize integration writes and must not modify a running child's worktree.

The local-delivery exception applies only to an activated supervisor (`kind: epic`,
`epic_execution: agent`) and children whose actual `parent` names it. Read ticket
files to verify those fields; a prompt claim, display order, or a running
supervisor is not proof of membership. Runs must not change fields to qualify
themselves. Annotation-only runs receive no implementation or commit authority.
Narrower user, ticket, or stage instructions still win, including “do not commit”.

Within that scope, implementation, tests, linters, and local commits on the run's
own verified branch need no per-commit approval. Before each commit, merge, or
rebase, verify that HEAD is not detached, the current branch equals the ticket's
`branch`, and the working directory is the invocation's worktree. Never commit,
merge, rebase, or reset on `main`, `master`, or the repository's default branch.
If a check fails or ownership is unclear, ask before writing.

A run may rebase only its own verified branch onto its `base_branch` (or the
default branch if unset). The supervisor integrates by merging child branches
into its own branch, not by rebasing children. A child may merge the epic's branch
or its own `base_branch` into its own verified branch. Never modify a running
child's worktree. Do not discard, stash, or overwrite work the run did not create,
or rewrite commits present on a remote or the default branch or published/shared
history.

Declared project dependencies and tools at an exact version named by the project
or ticket may be installed in the worktree or a temporary directory; a package
manager's own cache is allowed. Global installation, real-account access (apart
from read-only lookups), device access, and service exposure require approval.
For setup requests, name the package/version, destination, and purpose.

Supervisors and children must investigate questions, read saved decisions, and
resolve permitted local setup before asking. Continue independent authorized
work where possible. Ask a concrete question for the missing resource, authority,
or scope decision, including attempted checks, concrete options, costs, and a
recommended default. Save the question and answer together immediately on the
current ticket, distinguishing operator answers from assumptions, and confirm
persistence before continuing. Do not silently weaken acceptance requirements or
substitute emulation for required real-device evidence. If no authorized work
remains, submit `blocked` with an actionable `next_step` and stop.

Activated supervisors and their children must submit an invocation-bound
`complete` or `blocked` result for each stage, including resumed and rework
execution. The requirement is independent of a custom `system_prompt`. Missing
or invalid results after a clean run pause the same stage; notes or exit zero do
not substitute for a report. Unrelated tickets, passive-epic children,
annotations, and assistant-only runs retain their existing requirements.
See [Supervised results](cli.md#supervised-results) for schema and transports.

The supervisor must not publish or make off-machine writes, including through children, scripts, or tools. This prohibits pushes, pull requests, remote issues or comments, messages, uploads, and releases. Read-only external lookups are allowed. Results and progress notes stay local. These are agent instructions, not runtime enforcement.

Supervisors use a separate pool: `max_concurrent_epic_supervisors`, default `4`, in addition to `max_concurrent_agents`. At default settings, epics can add four agent processes beyond the worker limit. The queue chooses the oldest eligible ticket whose pool has capacity.

Upgrade all clients and daemon binaries that read the config before adding
`epic_child_pipeline`; older strict-YAML loaders reject the key.
Upgrade every daemon and client sharing the ticket directory before activating an epic. Older versions treat all epics as passive and can overwrite supervisor status through child rollups. Do not run mixed versions against activated epics.

`children` orders the sub-tickets and nothing else. Membership is what each child's `parent` says, so an id here that names no child is ignored on read, and a child the list does not name sorts after every child it does, by `created`. Dragging a row on the epic page writes this one list, and writes no child file.

Deleting an epic through the dashboard or API, including CLI remote mode, clears each child's `parent`. Local `kontora delete` and direct file deletion leave those references unchanged. No child file is deleted or archived.

#### Base branch

Kontora normally cuts a ticket's worktree from the repository's default branch. Set `base_branch` to start it somewhere else:

```yaml
base_branch: develop
```

The value must name a branch. Kontora looks it up as a local branch first (`refs/heads/develop`) and then as a remote-tracking branch (`refs/remotes/origin/develop`), so both `develop` and `origin/develop` work. Tags, raw commit SHAs, and revision expressions such as `HEAD~1` are rejected, even when the name looks like a branch: a `v1.0` tag does not resolve unless a `v1.0` branch also exists.

Kontora never runs `git fetch`, so `base_branch: develop` uses whatever the local `develop` points at right now. A branch that exists only on the remote does not resolve.

The base applies once, when the worktree is first created. Changing `base_branch` afterwards does not move, recreate, or rebase the work branch, because that could discard the agent's work. It does change how later human reviews are built: the review diff is measured from the merge-base of the work branch and the resolved base, so a ticket based on `develop` shows only the agent's commits and not `develop`'s.

`base_branch` must not name the ticket's own work branch. That combination fails worktree creation and pauses the ticket. Without the check, git would silently check the base branch out and the agent would commit onto it.

When `base_branch` is set through `kontora new` or `POST /api/tickets`, Kontora checks that the branch exists before writing the ticket file. The check is skipped for a ticket created with `status: open`, which is not ready to run and never reaches the repository check. A hand-written ticket or a value set through `kontora update` is not checked up front either, because the repository at `path` can change afterwards. Such a value surfaces later: `git worktree add` fails, the daemon pauses the ticket, and the resolution error is written to `last_error`.

#### Notifications

`notify` lists the statuses that trigger notifications for this ticket. Without the field nothing is sent, however many channels the config names. For new ordinary tickets, Kontora writes the [project's `notify` default](configuration.md#projects) into the file. An explicit list overrides that default; `kontora new --notify none` or `--notify ""` skips it. Epics and initialization do not inherit this default.

```yaml
notify: [human_review, done]
notify_channels: [tg]
```

Both a single status (`notify: done`) and a list are read. Any status works, including a [custom one](configuration.md#top-level-fields), plus the pseudo-status `waiting`.

By default, only a transition the daemon decided on its own sends. Set `notifications.all_changes: true` to also send for commands, API calls, dashboard actions, and file edits. The switch reloads live and does not distinguish people from agents or scripts. Eligible changes enqueue one notification per channel; unchanged statuses and derived epic statuses stay silent. The watcher can combine rapid file edits into one observed change.

A ticket's first sighting is always silent. With the switch on, a later crash-recovery change to `todo` or a recovery pause can send if the ticket lists that status.

A `done` notification carries the ticket's per-run `summary`. It fires from the write that ends the run, and `final_summary` is written up to two minutes later by a separate pass, so it cannot be in the message.

`waiting` fires when a running agent blocks on a question, once per question rather than once per poll. It works for `pi` agents only: the marker behind it is published by the pi extension Kontora injects, so `notify: [waiting]` on a claude ticket never fires. The marker is polled every two seconds, so a question answered faster than that is legitimately never seen.

`notify_channels` names where this ticket's notifications go. It is resolved as the first non-empty of the ticket's list, its project's, and `notifications.default`; `none` in the list silences the ticket while leaving its `notify:` list readable, and the same channel twice is one message. See [notifications](configuration.md#notifications) for the channels themselves.

Three things that would otherwise make a ticket quiet with no explanation are warned about when the daemon reads it, at startup and on every later edit, and then ignored: a status in `notify` that nothing reaches, a channel name nothing answers to, and a `notify:` list that resolves to no channel at all. A malformed `notify:` value makes the whole ticket unparseable, the same as a malformed `deps:`.

Both fields are editable from the dashboard. The start-ticket modal uses `notify me`; the details rail uses `notify`. The row has three presets: off, when it needs me (`[paused, human_review, waiting]`), and when it is finished (`[human_review, done]`). The full status set is behind `custom`.

The start-ticket modal disables notification controls while configuration loads. With no configured channels, only off is available. Add a channel under [`notifications.channels`](configuration.md#notifications). Channel changes reload without a daemon restart. The modal links to these instructions in a separate tab, so the draft stays open. You can start with notifications off without configuring a channel.

Opening the modal preserves existing preferences. Choosing off with no configured channels clears both notification fields, including references to removed channels. When channels are configured, off keeps the ticket's channel choices.

The row states one inherited channel instead of asking for it. If no inherited route exists, the channel picker appears. The dashboard refuses to save or start until the ticket has a route, is silenced, or turns notifications off. The rail row opens only where the API accepts a frontmatter edit. A running ticket shows its setting but cannot change it because the agent owns the file.

The API accepts `notify` and `notify_channels` on `POST /api/tickets`, `POST /api/tickets/{id}/init`, and `PUT /api/tickets/{id}`. During creation, an absent `notify` inherits the project default and `[]` skips it. During initialization or update, an absent key preserves the stored field and `[]` removes it.

The CLI flags take comma-separated lists. On update, `--notify ""` or `--notify none` removes the status list. `--notify-channels ""` removes the channel override; `--notify-channels none` writes `[none]` to silence delivery. The CLI and API reject unknown statuses and channels; `none` is allowed only alone in a channel list. API validation failures return 400. Hand edits instead produce daemon warnings.

#### Scheduled pickup

`scheduled_at` holds the instant an `open` ticket becomes `todo`:

```yaml
status: open
scheduled_at: "2099-09-01T07:00:00Z"
```

Write it with [`kontora schedule`](cli.md#kontora-schedule-ticket_id), with `kontora new --at`/`--after`, or from the dashboard — the ticket detail panel, the schedule scope in the command palette, the Start-at field on the new-ticket page, or the phone's schedule sheet. All of them normalize the instant to UTC, second precision, so a schedule set from two time zones compares equal, and all of them refuse an instant already in the past: a mistyped year would otherwise start the agent at once.

The time itself is written the same way everywhere: an absolute instant (`2099-09-01T09:00:00+02:00`, or `"2099-09-01 09:00"` in your own zone) or a delay from now (`90m`, `24h`, `3d`, `2w`). A date with no time is refused, because which midnight it means depends on the zone reading it.

The timestamp is a one-time trigger, not a status. A scheduled ticket sits in the Open column and stays out of the ready queue until its deadline, and its card shows the time it starts in your own zone.

At or after that instant the daemon writes `status: todo` and removes `scheduled_at` in one save, then hands the ticket to the ordinary pickup rules. It does not bypass them:

- With [`auto_pick_up: false`](configuration.md#top-level-fields) the ticket becomes `todo` and waits there for an explicit `kontora run`.
- An unresolved dependency keeps it out of the queue, and [dependency-aware scheduling](#dependency-aware-scheduling) queues it once the dependency closes.

The daemon rebuilds its wake-ups from the ticket files at startup, so a schedule survives a restart. One whose deadline passed while the daemon was down is promoted on the first pass after the initial scan.

A promotion is deferred, not lost, while a run or a Plannotator session owns the ticket; it happens once that ends. A value the RFC 3339 parser rejects is left alone and shown as it stands, so a hand-edited typo leaves the ticket open rather than starting it at an instant nobody asked for.

`kontora run` on a scheduled ticket removes the timestamp and sets `status: todo` in the same save, and queues the ticket even with `auto_pick_up: false`. Every other lifecycle move off `open` drops the timestamp too: `retry`, `skip`, `move`, an `init` that queues the ticket, and parking the ticket for an [annotation run](configuration.md#plannotator). Each of them has already answered the question the schedule was asking. An uploaded `.md` file loses any `scheduled_at` it carries, for the same reason the upload is clamped to `open`: it arrives as a draft, not as a run request.

#### Automations

[Automations](automations.md) run agents directly from entries under `automations:` in the daemon's YAML configuration.
They do not create tickets, inherit ticket pipelines, or use ticket lifecycle controls.
Each automation run has its own number, result, terminal, and logs.

Use `scheduled_at` for one-time promotion of an existing ticket.
Use an automation for a recurring prompt or a manual run of that saved prompt.
Automations share agent capacity with tickets, but their enabled schedules and manual requests work with `auto_pick_up: false`.

Pause disables future scheduled runs without cancelling accepted runs. Stop targets an automation run; deletion stops its unfinished work.
Overlap policies are `skip`, `queue`, and `replace`. Queue keeps at most one pending run; replacement waits for confirmed termination.
Ticket deletion does not affect automation history or receipts.

### Daemon-managed fields

These are set and updated by the daemon as the ticket progresses through its pipeline. Do not edit them manually while the daemon is running.

| Field | Description |
|-------|-------------|
| `kontora` | Boolean. When `true`, the daemon may manage and execute this ticket. Tickets without `kontora: true` are still listed, but are not auto-picked up and cannot be run/retried/skipped until initialized. Set by `kontora init`. |
| `stage` | Current pipeline stage name. Set to the first stage on pickup. |
| `attempt` | Retry counter for the current stage. Reset to 0 on advance/back. |
| `started_at` | When the current stage started. |
| `running_status` | Snapshot of a custom-active stage's actual status. Written atomically with `status` on claim; absent or empty for default execution and parked tickets. |
| `completed_at` | When the ticket finished (status became `done`). |
| `branch` | Git worktree branch. Set it before pickup to choose the name. If empty, the default is `<prefix>/<title-slug>-<ticket-id>`; [`branch_naming`](configuration.md#branch-naming) mode `off` uses `<prefix>/<ticket-id>`. The daemon stores a slug name before worktree creation and stores the ID-derived name after worktree creation succeeds. |
| `history` | List of completed stage records. |
| `imported_history_count` | Number of history entries imported by Claim. Imported rows never use local artifact or cost enrichment. |
| `last_error` | Error message from the last failed stage run. |
| `summary` | Summary of the stage run that ended most recently: the text the agent wrote with `kontora summary`, or its final session message as a fallback. Cleared when a new stage starts; each stage keeps its own copy on its `history` entry. |
| `final_summary` | Summary of the whole ticket, written after a run ends the pipeline successfully or a rework run succeeds, and at least two runs recorded a summary. The agent that ran that stage writes it from the ticket text and every recorded run summary, in one non-interactive pass with no tools and a two-minute timeout. It opens with a few sentences on what the ticket asked for, then describes what was done; a ticket whose body says nothing beyond its title gets the description alone. It is not a copy of any run: `summary` and the `history` entries keep their own text. Cleared when a new run starts. Generation is best effort: a failure leaves the field empty and is reported in the daemon log, so a ticket can finish without it. |
| `claimed_by` | The daemon [`instance_name`](configuration.md) that last picked the ticket up. Written with the execution claim. Consulted during default or custom-active execution; a stale value on an inactive ticket is ignored. |

### Custom fields

Any field not listed above is preserved through round-trips. The daemon will not overwrite or remove fields it doesn't recognize:

```yaml
---
id: poi-q88f
status: todo
pipeline: default
type: ticket
priority: 2
tags: [ui, backend]
---
```

## Body

Everything after the closing `---` is the ticket body. It's standard markdown.

The first `# Heading` is treated as the ticket title and is available in prompt templates as `{{ .Ticket.Title }}`. The full body is available as `{{ .Ticket.Description }}`.

## Status lifecycle

```
open → todo → in_progress → done ──────→ archived
                   ↓                       ↑
            paused / cancelled ────────────┘
```

| Status | Meaning |
|--------|---------|
| `open` | Drafted but not ready for the daemon to pick up. A [`scheduled_at`](#scheduled-pickup) stamp moves it to `todo` at a set time. |
| `todo` | Ready for the daemon. The scheduler picks it up in creation order, once its `deps` are resolved. |
| `in_progress` | An agent is currently working on it. |
| `paused` | Stopped by a failure policy or by the user. Set `status: todo` to resume. |
| `human_review` | Parked for a person to look at, by a stage policy or `kontora move`. The agent's work is finished, so this releases the tickets that depend on it. |
| `done` | All pipeline stages completed successfully. |
| `cancelled` | Manually cancelled by the user. |
| `archived` | An old closed ticket, hidden from the main views. Terminal; still on disk. |
| `closed` | Read-only compatibility. Tickets written by the external ticket CLI carry it. Kontora reads it as closed: it releases dependents, `kontora ls --closed` shows it, and the archive sweep accepts it. No command writes it. |

The daemon only picks up tickets that have both `kontora: true` and `status: todo`. A non-Kontora ticket can still appear in any status column, but Kontora will not start or resume work on it until you initialize it. To pause a running Kontora ticket, use `kontora pause <id>` or set `status: paused` in the file — the daemon will detect the change and stop the agent.

### Active and parked custom statuses

A pipeline step can set [`running_status`](configuration.md#running-in-a-custom-column)
to a declared custom status. The daemon stores that actual status while its agent
runs. A ticket with `status: awaiting_data` and `running_status: awaiting_data` is
active; another ticket with the same status but no matching snapshot is parked.
Legacy `in_progress` tickets remain active without a snapshot.

The snapshot is internal metadata, not a user setting. Ordinary status assignments
clear it, even when the status name stays unchanged. Notes and summary updates
preserve it. A stale mismatched snapshot does not make the ticket active.

Custom-active tickets support Pause, Cancel, live output and attachment, with the
same editing and stage-change restrictions as ordinary running tickets. They keep
their slot and branch reservation and block dependencies. A false
`execution_active` API field after Pause does not prove the process stopped.

`on_success` and `on_failure` custom destinations still park tickets. Moving into
a custom column never starts a stage. Same-status manual moves do not stop an
agent; use Pause. Config removal does not erase an active snapshot: the ticket
stays visible in a display-only column until execution ends. Restart uses normal
recovery and resume behavior, not a continuation of an interrupted shell sleep.

Upgrade all CLI and daemon binaries sharing the store before enabling custom
running statuses. Existing parked tickets need no migration and are not activated.

## Dependency-aware scheduling

The daemon starts an agent for a `kontora: true` ticket in `todo` only when every id in its `deps` names a ticket that no longer holds work: `human_review`, `done`, `cancelled`, `archived`, or the legacy `closed` that tickets from the external ticket CLI carry. `human_review` releases dependents because the agent has finished and only a person's verdict is left; the review can still send the ticket back, and a dependent that is already running then keeps going. Every other status blocks, including a custom one. So does an id with no ticket file, because nothing on disk says whether that work is finished.

A dependency in `human_review` usually has commits that nobody merged yet, so the dependent's worktree does not contain them. When a `human_review` dependency has a local `branch` whose tip the dependent's worktree lacks, Kontora names that branch and its commit in the dependent's prompt under `## Unmerged dependencies`. It does not merge the branch.

Primary dependencies first look for the exact ID in `tickets_dir`. If absent, they
look in configured secondary folders. A unique match supplies its status, including
completed work in another folder. A primary namesake always takes precedence, even
when unfinished. Duplicate secondary IDs, missing tickets, and unavailable sources
do not release work. Secondary prerequisites appear with their title, status, and
source in the dependency chain, but their own relations stay within their folder.
Kontora never starts a secondary ticket to satisfy a dependency.

Readiness is derived on every read. It is not a status and is not written to any file: there is no `blocked` status, and a ticket waiting on a dependency stays `todo`.

The queue reacts to the whole store, not just to edits of the waiting ticket:

- Closing a dependency queues every ready ticket that waited on it, with no further edit to their files. This includes changes in configured secondary folders.
- Adding a dependency to a queued ticket drops it from the queue again. A ticket that is already running is left alone: an edge added mid-run does not interrupt it.
- Deleting a resolved dependency blocks its dependents again, because the id now names nothing.
- Readiness is checked twice, once when the ticket joins the queue and once when the agent is about to be spawned, which closes the gap between the two.

A cycle never starts an agent. Every ticket on it waits on another ticket that is not closed, so none of them is ever ready. `kontora dep` refuses to create a new cycle, but one that arrived through hand-edited files or file sync resolves this way rather than by an error.

Ready tickets run oldest `created` first among tickets whose capacity pool has a free slot. Workers and epic supervisors use separate pools. Two tickets created at the same time run in id order.

## Running on multiple machines

When a `tickets_dir` is synced across machines (iCloud, Syncthing, ...), two daemons can see the same tickets. File sync is eventually consistent, so there is no cross-machine lock; instead each daemon records its [`instance_name`](configuration.md) in `claimed_by` when it picks a ticket up, and consults that claim before acting on a default or custom-active ticket:

- **Crash recovery** on startup resets an active ticket back to `todo` only when `claimed_by` is empty or matches this daemon. A ticket claimed by another instance is left running; starting a second daemon no longer steals or kills the first's work.
- **During a run**, if a foreign claim arrives through sync, the daemon cancels its own agent and leaves the file untouched. The losing worktree is kept (it may hold unpushed commits).
- **Both sides can briefly claim** the same `todo` ticket within the sync window. Once sync settles, one claim wins and the other daemon yields; an active ticket with a self-claim but no running agent is reset to `todo` and re-enqueued.

There is no automatic takeover of a claim by claim age or a heartbeat. If the owning machine goes offline mid-run, its tickets stay in their running status until that daemon restarts or someone manually sets `status: todo`. Two machines that share a hostname must set `instance_name` explicitly, or the protection cannot tell them apart.

`archived` is a terminal built-in status for closed tickets. Archived tickets are hidden from `kontora ls` (including `--closed`), the TUI, and the WebUI board, but their markdown files are kept on disk. The daemon never enqueues an archived ticket, and a live transition to `archived` stops any running agent and cleans up its worktree, just like `done`/`cancelled`. It cannot be set as a custom status, and `move` still refuses it in both directions: archiving and restoring have their own verbs.

A ticket reaches `archived` through the `kontora archive` sweep (below), through the WebUI's Archive item on a `done` or `cancelled` ticket's card and detail menus, or by editing a file's `status` field. The sweep and the WebUI write the same four fields, so a swept ticket and one archived from the UI are the same thing. A file edited by hand carries only what was typed into it, and the restore fallback below is what covers a ticket that arrived in `archived` without them:

```yaml
status: archived
archived_from: done      # the closed status held before archiving
archived_at: 2026-08-26T09:12:00Z
archived_by: web         # "web" or "sweep"
archive_note: superseded by kon-244
```

Restoring is the WebUI's Archive view, from a row or from the open ticket: it writes `archived_from` back into `status` and removes all four fields. A ticket archived before those fields existed, or one whose `archived_from` names a status that is no longer a board column, restores to `done` rather than staying stranded.

## Archiving old tickets

```bash
kontora archive --days 30            # archive done/cancelled tickets untouched for 30+ days
kontora archive --days 30 --yes      # skip the confirmation prompt
kontora archive --days 30 --dry-run  # list what would be archived, write nothing
kontora archive --days 30 --path ~/projects/kontora  # limit the run to one repository
kontora archive --days 30 --project kontora          # the same, by configured project name
kontora archive --days 7 --status cancelled          # sweep cancelled tickets sooner than done ones
```

`kontora archive --days N` marks every `done`, `cancelled` or legacy `closed` ticket whose markdown file was last modified at or before `now - N days` as `archived`. The cutoff uses the file's modification time (the same value the WebUI shows as "updated"), because cancelled tickets have no `completed_at`. `--days` is required and must be a positive number.

Every run first prints a table of the matching tickets, with the ID, the closed status, the title taken from the ticket's first markdown heading, and the repository path. `--dry-run` stops there and changes nothing. Otherwise the command asks `Archive N tickets? [y/N]` and archives only on `y` or `yes`; any other answer leaves every file alone. `-y`/`--yes` archives without asking. When stdin is not a terminal, the prompt cannot be answered, so a run that would write files fails and asks for `--yes` instead.

`--path` limits the run to tickets whose `path` field is that repository. Both sides are compared after tilde expansion and cleaning, so `~/projects/kontora`, the same directory in absolute form, and a trailing slash all select the same tickets. Only the complete path matches: a ticket pointing at a subdirectory of the given path is left alone, as is a ticket with no `path` field.

`--project NAME` selects the same tickets through the `path` of a project in the config `projects` map, so the path does not have to be typed. A name that is not configured is an error, because a typo would otherwise look like a run where nothing was old enough. `--path` and `--project` cannot be combined.

`--status` narrows the run to one closed status: `done`, `cancelled`, or the legacy `closed` that tickets from the external ticket CLI carry. Any other value is rejected. Without it all three are archived. The sweep only ever writes `archived`; no command writes `closed`, and every ticket it archives gets the four archive fields above, with `archived_by: sweep`.

## History

The daemon appends a record to `history` after each stage run is finalized:

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
  - stage: code
    agent: claude-sonnet
    exit_code: 1
    started_at: 2026-03-01T10:06:00Z
    completed_at: 2026-03-01T10:15:00Z
    session_kind: pi
    session_ref: pi-sessions/code/01JC9F3K7Q.jsonl
```

Supervised history may also carry `run_outcome`, `outcome_reason`, and
`invocation_id`. These describe the final semantic outcome, its reason, and the
physical invocation, separately from `exit_code` (runner exit status, synthesized
for interactive Claude). An exit code of `-1` means recovery could not observe
the runner status. Outcomes are finalized after hooks and epic child
checks, not merely copied from the submitted report. A blocked run with exit zero
is not completed delivery. Histories without `run_outcome` remain unverified task
outcomes: preserve their runner status without inferring completion from exit
codes or old prose. CLI, browser, summaries, statistics, and metrics must keep
this distinction. Standalone supervised children record real history and must
not also be counted as synthetic legacy runs.

`model` and `effort` are what the agent ran with for that run, resolved from the
stage override, then the agent's own key, then the flags in the agent's `args`.
An absent key means nothing selected one and the agent used its CLI's own
default. They are not what the agent reported running as: an agent given
`model: opus` can report `claude-opus-4-6`.

`session_kind` and `session_ref` name the agent's own session JSONL for that
run. `kontora sessions` turns them back into paths; see
[the CLI reference](cli.md#kontora-sessions-flags-ticket_id). Both are absent
when the run wrote no session Kontora can point at, which is every run by an
agent that is neither Claude nor pi, and every run recorded before the two
fields existed.

The reference is an identifier rather than a path, because a path would name a
file on one machine and ticket files travel between them. For `claude` it is the
session UUID Kontora minted, resolved against
`$CLAUDE_CONFIG_DIR/projects/*/<uuid>.jsonl`; for `pi` it is the file's path
relative to `<logs_dir>/<ticket-id>`, because pi names its own file. Read on the
wrong machine, an identifier visibly resolves to nothing, while an absolute path
often points at a real file holding another run's bytes.

`session_kind` is recorded rather than derived from `agent`, because `agent` is a
config name and the config that gave it a kind can be edited or gone by the time
the row is read.

## Notes

Use `kontora note` to append a note under a `## Notes` section in the body. The next stage can read the note through `{{ .Ticket.Description }}`. The web UI also has a `notes` tab on the ticket page.

Read notes with `kontora view --notes TICKET_ID`, or recent notes with `kontora view --last 5 TICKET_ID`. Both print stored markdown without a pager, locally or remotely, and retain later sections through the end of the body.

`--last N` implies `--notes` and requires a positive integer. It starts at the beginning of the Nth-last note. Legacy notes without bylines also count. If N exceeds the note count, it prints the whole section, including `## Notes`. A ticket without that section prints nothing and exits 0. Neither flag can be combined with `--body`.

Prefer Pi's `kontora_note({text: string})` tool when available; it targets the current ticket. Otherwise, use a file-writing tool to create a unique scratch file outside the repository. Submit the file through stdin:

```sh
kontora note TICKET_ID < /absolute/path/note.md
```

Replace `TICKET_ID` and the example path with the actual ticket ID and scratch path. Do not put generated text in shell arguments or payload-bearing shell commands. Do not create the file with `echo` or `printf`. Put flags such as `--author` and `--reply-to` before the ticket ID.

Positional text remains supported. Stdin removes trailing LF characters, and notes trim surrounding whitespace.

Adding or editing a note normalizes its text so headings and byline-like text cannot split the note:

- Outside fenced code, headings shift only when the shallowest heading is h1 or h2. That heading becomes h3.
- Those headings shift by the same amount, preserving relative levels, except that levels above h6 become h6. Notes whose shallowest heading is h3 or deeper need no shift.
- Backtick and tilde fenced code stays unchanged. If a fence remains open at the end, Kontora appends a closing fence.
- A bold-only line outside fenced code that matches the byline rule below uses outer `__` instead of `**` when stored. Both render as bold.

Outside fenced code, a line beginning with `# ` or `## ` at column zero ends the Notes section. Inside fences, those lines do not end Notes, and bold-only lines do not open notes.

Existing tickets are not migrated. Old sections after Notes remain in the description editor's read-only area and in `kontora view --notes TICKET_ID`. Full-body writes, including `update --body-file` and API `body`, do not normalize note text.

For summaries, prefer `kontora_summary({text: string})` when available, or submit a scratch file through stdin:

```sh
kontora summary TICKET_ID < /absolute/path/summary.txt
```

Summaries preserve spaces and internal newlines after stdin removes trailing LF characters. The CLI does not truncate summaries; later stage capture cuts text above 4,000 bytes and appends `\n[truncated]`. See the [CLI reference](cli.md#kontora-note-ticket_id-text-kontora-summary-ticket_id-text) for native input limits and process-list visibility.

Kontora writes each note as a bold byline followed by its text. The byline is a `·`-separated field list: timestamp, author, note id, then optional flags.

```
## Notes

**2026-03-06T12:00:00Z · developer · q88f**

Use the existing search index, don't create a new one.

**2026-03-06T12:04:10Z · claude · a1b2 · re:q88f**

Done — reusing internal/search.

**2026-03-06T12:30:00Z · kontora · n3x0 · edited**

paused: stage-end hook failed
```

The id is 4 lowercase alphanumerics, unique within the ticket, and it is what the [note endpoints](api.md#notes) address. `re:<id>` makes the note a reply to that id; one level only, so a reply cannot be replied to, and deleting a note deletes its replies with it. `edited` marks a note whose text was replaced.

The author is a plain name and cannot contain `·`, a newline or `**`. Kontora signs its own notes `kontora`, an agent signs with its configured name from `$KONTORA_AGENT`, and a person signs with the [`author`](configuration.md#top-level-fields) config field. A ticket that pauses gets a `kontora` note carrying the reason, which is why a pause shows up in the conversation rather than as a lifecycle row.

Outside fenced code, a bold-only line opens a note only when either condition holds:

- Its first `·`-separated field starts with a `YYYY-MM-DD` date.
- It matches the full `at · author · id` grammar, with only the optional `re:<id>` and `edited` flags after the id. Both `at` and author may be empty, as in `** ·  · cd34**`.

Other bold-only lines, such as `**Verification:**`, remain note text. A date-led byline that does not match the full grammar still opens a legacy note. Its entire inner text becomes `at`, with no author or stored id. For example, `**2026-03-06T12:00:00Z**` still parses. Such a note is addressed by position (`#0`, `#1`) until an action mints a real id in the same write.

Reactions are not in the body. They live in `<tickets_dir>/<ticket-id>.notes.json` beside the ticket file, so the body stays readable to the agent that receives it in a stage prompt. A ticket copied without that file keeps its conversation and loses its reactions.

## Ticket ID format

IDs are `<prefix>-<4 random alphanumeric chars>` (e.g., `poi-q88f`).

The prefix comes from the directory name (the ticket's `path` field): the first alphanumeric character of each `-` or `_` separated segment, lowercased and joined, with no length limit. A name that yields fewer than two of them uses its own first 3 alphanumerics instead, so a single-word repository still gets a readable prefix.

| Directory | Prefix |
|-----------|--------|
| `alpha-lib` | `al` |
| `customer-portal-app` | `cpa` |
| `build_tools` | `bt` |
| `demo-17-sample-client-sdk` | `d1scs` |
| `kontora` | `kon` |
| `sample` | `sam` |

A project can pin a prefix instead of deriving one, with [`prefix`](configuration.md#projects) in its config entry.

CLI commands accept prefix matches: `kontora done kon` resolves to the ticket with ID `kon-q88f` if it's the only match.

## Creating tickets

**With the CLI:**

```bash
kontora new "Fix the search index"                              # uses current git root
kontora new --path ~/projects/kontora "Fix the search index"      # explicit path
```

**Manually:** Create a `.md` file in `tickets_dir` with the frontmatter above. The daemon watches the directory and lists valid ticket files with an `id`. It only picks up new `status: todo` tickets automatically when they also have `kontora: true`; otherwise they remain visible as `not a kontora ticket` until initialized.
