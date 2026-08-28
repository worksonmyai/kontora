# Ticket Format

Tickets are markdown files with YAML frontmatter. They live in the directory specified by `tickets_dir` in the [configuration](configuration.md). Any tool that can write a markdown file — a text editor, Obsidian, a script — is a valid client.

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

## Frontmatter fields

### User-defined fields

These are set when creating a ticket (manually or via `kontora new`). `notify` and `notify_channels` are the exception: `kontora new` does not take them, so they come from the dashboard's `notify me` row, from `kontora update`, or from a hand edit.

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `id` | yes | — | Unique identifier, format `<prefix>-<4 alphanum>` (e.g., `kon-q88f`). |
| `status` | yes | `todo` | Current ticket status (see [Status lifecycle](#status-lifecycle)). On an epic it is derived from the children and written back by the daemon. |
| `kind` | no | — | `epic` for a ticket that groups others; absent for ordinary work. See [Epics](#epics). |
| `pipeline` | no | — | Name of the pipeline to run (must exist in config). Filled in at creation from a matching [project](configuration.md#projects) when one is configured and no pipeline is given. When it ends up empty, the ticket runs in standalone mode with the default agent. |
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

`deps` and `links` are written by [`kontora dep`, `kontora undep`, `kontora link` and `kontora unlink`](cli.md#relations). `parent` is written by `kontora new` with a parent flag, by the epic page, or by hand. Only `deps` reaches the scheduler, through [dependency-aware scheduling](#dependency-aware-scheduling); `links` and `parent` are for navigation and for the epic rollup.

```yaml
deps: [kon-a1b2]
links: [kon-c3d4, kon-e5f6]
parent: kon-9zzz
```

Both the flow form above and a block sequence are read. An id with no ticket file behind it stays on screen as plain text: the frontmatter still points at it, and there is nothing to open.

Two more relations are derived on read rather than stored, because each edge is written on the other ticket alone. `blocks` is the reverse of `deps`: the tickets whose `deps` name this one. `children` is the reverse of `parent`: the tickets whose `parent` names this one. They are read by scanning every ticket file, so both cover tickets the board hides.

`children` renders as a sub-ticket tree at the top of the ticket tab, one row per child with its title, id, stage and elapsed time, and a `3 of 5` rollup counting the children that are `done`. The tree draws 12 rows before it offers a reveal for the rest, and a click on a row opens that child. A ticket with no children draws no tree.

#### Epics

An epic is a ticket file like any other with two differences: `kind: epic`, and no pipeline. It is not runnable work — no agent, no branch, no worktree, no run history — and the scheduler never enqueues, claims or spawns anything for it, whatever status its file carries. What it holds is the brief for work that spans several tickets: the description, the design, the decisions, the open questions, the success criteria.

```yaml
id: kon-e7c1
kontora: true
kind: epic
status: in_progress      # derived, written back by the daemon
path: ~/projects/kontora
children: [kon-b12e, kon-c04d, kon-9b31]   # order only
```

Create one with `kontora new -kind epic`, and file a ticket under it with `kontora new -parent kon-e7c1` or `POST /api/tickets/{id}/parent`. Both creation paths check the parent the same way `POST .../parent` does: it has to exist and it has to be an epic. Epics do not nest: setting `parent` on a ticket whose `kind` is `epic` is refused.

The status is derived from the children and rewritten by the daemon whenever one of them changes, so moving an epic by hand is refused too, and so are the verbs that would move it as a side effect: `pause`, `run`, `retry`, `skip`, `init` and `schedule`. Archiving one is allowed and is terminal: the derivation stops there, and a restore hands the epic back to it. Archived children are ignored. Any child in `todo`, `in_progress`, `paused` or `human_review` gives `in_progress`; otherwise every remaining child being terminal with at least one `done` gives `done`; every remaining child `cancelled` gives `cancelled`; anything else, including no children at all, gives `open`. An epic that closes itself records a note saying which child landed last. The brief stays editable in every one of those statuses, because no agent ever owns an epic's file.

`children` orders the sub-tickets and nothing else. Membership is what each child's `parent` says, so an id here that names no child is ignored on read, and a child the list does not name sorts after every child it does, by `created`. Dragging a row on the epic page writes this one list, and writes no child file.

Deleting an epic keeps its work: every child has its `parent` cleared, and no child file is deleted or archived.

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

`notify` lists the statuses this ticket asks to be told about. It is opt-in per ticket: without the field nothing is sent, however many channels the config names.

```yaml
notify: [human_review, done]
notify_channels: [tg]
```

Both a single status (`notify: done`) and a list are read. Any status works, including a [custom one](configuration.md#top-level-fields), plus the pseudo-status `waiting`.

Only a transition the daemon decided on its own sends. `kontora done <id>`, a drag in the dashboard, and any other change a person asked for update what the daemon remembers and send nothing: the point is to hear about work you were not watching, and you were watching when you made the change yourself. A ticket the daemon has not seen before is recorded without sending, so a restart over a ticket already in a listed status is silent.

A `done` notification carries the ticket's per-run `summary`. It fires from the write that ends the run, and `final_summary` is written up to two minutes later by a separate pass, so it cannot be in the message.

`waiting` fires when a running agent blocks on a question, once per question rather than once per poll. It works for `pi` agents only: the marker behind it is published by the pi extension Kontora injects, so `notify: [waiting]` on a claude ticket never fires. The marker is polled every two seconds, so a question answered faster than that is legitimately never seen.

`notify_channels` names where this ticket's notifications go. It is resolved as the first non-empty of the ticket's list, its project's, and `notifications.default`; `none` in the list silences the ticket while leaving its `notify:` list readable, and the same channel twice is one message. See [notifications](configuration.md#notifications) for the channels themselves.

Three things that would otherwise make a ticket quiet with no explanation are warned about when the daemon reads it, at startup and on every later edit, and then ignored: a status in `notify` that nothing reaches, a channel name nothing answers to, and a `notify:` list that resolves to no channel at all. A malformed `notify:` value makes the whole ticket unparseable, the same as a malformed `deps:`.

Both fields are also editable from the dashboard, in the `notify me` row of the start-ticket modal and in the `notify` row of a ticket's details rail. The row asks one question with three answers — off, when it needs me (`[paused, human_review, waiting]`), when it is finished (`[done]`) — and puts the full status set behind `custom`. It states the channel it resolves to rather than asking, until a second channel is configured. The rail row opens only where the API accepts a frontmatter edit, so a running ticket shows its setting and cannot change it: an agent owns the file, and the write would be lost when the run puts it back.

The API takes them too: `notify` and `notify_channels` on `POST /api/tickets/{id}/init` and `PUT /api/tickets/{id}`. An absent key leaves the ticket's own field alone and `[]` removes it. Unlike a hand edit, a request naming a status nothing reaches or a channel nothing answers to is refused with a 400 rather than warned about.

#### Scheduled pickup

`scheduled_at` holds the instant an `open` ticket becomes `todo`:

```yaml
status: open
scheduled_at: "2026-09-01T07:00:00Z"
```

Write it with [`kontora schedule`](cli.md#kontora-schedule-ticket_id), with `kontora new --at`/`--after`, or from the dashboard — the ticket detail panel, the schedule scope in the command palette, the Start-at field on the new-ticket page, or the phone's schedule sheet. All of them normalize the instant to UTC, second precision, so a schedule set from two time zones compares equal, and all of them refuse an instant already in the past: a mistyped year would otherwise start the agent at once.

The time itself is written the same way everywhere: an absolute instant (`2026-09-01T09:00:00+02:00`, or `"2026-09-01 09:00"` in your own zone) or a delay from now (`90m`, `24h`, `3d`, `2w`). A date with no time is refused, because which midnight it means depends on the zone reading it.

The timestamp is a one-time trigger, not a status. A scheduled ticket sits in the Open column and stays out of the ready queue until its deadline, and its card shows the time it starts in your own zone.

At or after that instant the daemon writes `status: todo` and removes `scheduled_at` in one save, then hands the ticket to the ordinary pickup rules. It does not bypass them:

- With [`auto_pick_up: false`](configuration.md#auto_pick_up) the ticket becomes `todo` and waits there for an explicit `kontora run`.
- An unresolved dependency keeps it out of the queue, and [dependency-aware scheduling](#dependency-aware-scheduling) queues it once the dependency closes.

The daemon rebuilds its wake-ups from the ticket files at startup, so a schedule survives a restart. One whose deadline passed while the daemon was down is promoted on the first pass after the initial scan.

A promotion is deferred, not lost, while a run or a Plannotator session owns the ticket; it happens once that ends. A value the RFC 3339 parser rejects is left alone and shown as it stands, so a hand-edited typo leaves the ticket open rather than starting it at an instant nobody asked for.

`kontora run` on a scheduled ticket removes the timestamp and sets `status: todo` in the same save, and queues the ticket even with `auto_pick_up: false`. Every other lifecycle move off `open` drops the timestamp too: `retry`, `skip`, `move`, an `init` that queues the ticket, and parking the ticket for an [annotation run](configuration.md#plannotator). Each of them has already answered the question the schedule was asking. An uploaded `.md` file loses any `scheduled_at` it carries, for the same reason the upload is clamped to `open`: it arrives as a draft, not as a run request.

### Daemon-managed fields

These are set and updated by the daemon as the ticket progresses through its pipeline. Do not edit them manually while the daemon is running.

| Field | Description |
|-------|-------------|
| `kontora` | Boolean. When `true`, the daemon may manage and execute this ticket. Tickets without `kontora: true` are still listed, but are not auto-picked up and cannot be run/retried/skipped until initialized. Set by `kontora init`. |
| `stage` | Current pipeline stage name. Set to the first stage on pickup. |
| `attempt` | Retry counter for the current stage. Reset to 0 on advance/back. |
| `started_at` | When the current stage started. |
| `completed_at` | When the ticket finished (status became `done`). |
| `branch` | Git worktree branch. Set it before pickup to choose the name. If empty, the default is `<prefix>/<title-slug>-<ticket-id>`; [`branch_naming`](configuration.md#branch-naming) mode `off` uses `<prefix>/<ticket-id>`. The daemon stores a slug name before worktree creation and stores the ID-derived name after worktree creation succeeds. |
| `history` | List of completed stage records. |
| `last_error` | Error message from the last failed stage run. |
| `summary` | Summary of the stage run that ended most recently: the text the agent wrote with `kontora summary`, or its final session message as a fallback. Cleared when a new stage starts; each stage keeps its own copy on its `history` entry. |
| `final_summary` | Summary of the whole ticket, written after a run ends the pipeline successfully or a rework run succeeds, and at least two runs recorded a summary. The agent that ran that stage writes it from the ticket text and every recorded run summary, in one non-interactive pass with no tools and a two-minute timeout. It opens with a few sentences on what the ticket asked for, then describes what was done; a ticket whose body says nothing beyond its title gets the description alone. It is not a copy of any run: `summary` and the `history` entries keep their own text. Cleared when a new run starts. Generation is best effort: a failure leaves the field empty and is reported in the daemon log, so a ticket can finish without it. |
| `claimed_by` | The daemon [`instance_name`](configuration.md) that last picked the ticket up. Written in the same update that sets `status: in_progress`. Consulted only while the ticket is `in_progress`; a stale value on a `todo` or `done` ticket is ignored. |

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

## Dependency-aware scheduling

The daemon starts an agent for a `kontora: true` ticket in `todo` only when every id in its `deps` names a ticket that no longer holds work: `human_review`, `done`, `cancelled`, `archived`, or the legacy `closed` that tickets from the external ticket CLI carry. `human_review` releases dependents because the agent has finished and only a person's verdict is left; the review can still send the ticket back, and a dependent that is already running then keeps going. Every other status blocks, including a custom one. So does an id with no ticket file, because nothing on disk says whether that work is finished.

Readiness is derived on every read. It is not a status and is not written to any file: there is no `blocked` status, and a ticket waiting on a dependency stays `todo`.

The queue reacts to the whole store, not just to edits of the waiting ticket:

- Closing a dependency queues every ready ticket that waited on it, with no further edit to their files.
- Adding a dependency to a queued ticket drops it from the queue again. A ticket that is already running is left alone: an edge added mid-run does not interrupt it.
- Deleting a resolved dependency blocks its dependents again, because the id now names nothing.
- Readiness is checked twice, once when the ticket joins the queue and once when the agent is about to be spawned, which closes the gap between the two.

A cycle never starts an agent. Every ticket on it waits on another ticket that is not closed, so none of them is ever ready. `kontora dep` refuses to create a new cycle, but one that arrived through hand-edited files or file sync resolves this way rather than by an error.

Ready tickets run oldest `created` first. Two tickets created at the same time run in id order, so the queue is deterministic.

## Running on multiple machines

When a `tickets_dir` is synced across machines (iCloud, Syncthing, ...), two daemons can see the same tickets. File sync is eventually consistent, so there is no cross-machine lock; instead each daemon records its [`instance_name`](configuration.md) in `claimed_by` when it picks a ticket up, and consults that claim before acting on an `in_progress` ticket:

- **Crash recovery** on startup resets an `in_progress` ticket back to `todo` only when `claimed_by` is empty or matches this daemon. A ticket claimed by another instance is left running; starting a second daemon no longer steals or kills the first's work.
- **During a run**, if a foreign claim arrives through sync, the daemon cancels its own agent and leaves the file untouched. The losing worktree is kept (it may hold unpushed commits).
- **Both sides can briefly claim** the same `todo` ticket within the sync window. Once sync settles, one claim wins and the other daemon yields; a ticket left `in_progress` with a self-claim but no running agent is reset to `todo` and re-enqueued.

There is no automatic takeover of a claim by claim age or a heartbeat. If the owning machine goes offline mid-run, its tickets stay `in_progress` until that daemon restarts or someone manually sets `status: todo`. Two machines that share a hostname must set `instance_name` explicitly, or the protection cannot tell them apart.

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

The daemon appends a record to `history` after each stage completes:

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

Use `kontora note <ticket-id> "message"` to append a note under a `## Notes` section in the body. This is how you communicate with the agent between stages — the next stage's prompt can include `{{ .Ticket.Description }}` to read the full body including notes. The web UI has a `notes` tab on the ticket page for the same thing.

A note is a bold byline followed by its text. The byline is a `·`-separated field list: timestamp, author, note id, then optional flags.

```
## Notes

**2026-03-06T12:00:00Z · alexander · q88f**

Use the existing search index, don't create a new one.

**2026-03-06T12:04:10Z · claude · a1b2 · re:q88f**

Done — reusing internal/search.

**2026-03-06T12:30:00Z · kontora · n3x0 · edited**

paused: stage-end hook failed
```

The id is 4 lowercase alphanumerics, unique within the ticket, and it is what the [note endpoints](api.md#notes) address. `re:<id>` makes the note a reply to that id; one level only, so a reply cannot be replied to, and deleting a note deletes its replies with it. `edited` marks a note whose text was replaced.

The author is a plain name and cannot contain `·`, a newline or `**`. Kontora signs its own notes `kontora`, an agent signs with its configured name from `$KONTORA_AGENT`, and a person signs with the [`author`](configuration.md#author) config field. A ticket that pauses gets a `kontora` note carrying the reason, which is why a pause shows up in the conversation rather than as a lifecycle row.

A byline that does not match this grammar is left alone: `**2026-03-06T12:00:00Z**` written before this format, or anything hand-typed, still parses, with no author and no id. Such a note is addressed by position (`#0`, `#1`) until something acts on it, at which point it is minted a real id in the same write.

Reactions are not in the body. They live in `<tickets_dir>/<ticket-id>.notes.json` beside the ticket file, so the body stays readable to the agent that receives it in a stage prompt. A ticket copied without that file keeps its conversation and loses its reactions.

## Ticket ID format

IDs are `<prefix>-<4 random alphanumeric chars>` (e.g., `poi-q88f`).

The prefix comes from the directory name (the ticket's `path` field): the first alphanumeric character of each `-` or `_` separated segment, lowercased and joined, with no length limit. A name that yields fewer than two of them uses its own first 3 alphanumerics instead, so a single-word repository still gets a readable prefix.

| Directory | Prefix |
|-----------|--------|
| `astra-l` | `al` |
| `grafana-assistant-app` | `gaa` |
| `deployment_tools` | `dt` |
| `hackathon-17-haimdall-sigil-sdk` | `h1hss` |
| `kontora` | `kon` |
| `sigil` | `sig` |

A project can pin a prefix instead of deriving one, with [`prefix`](configuration.md#projects) in its config entry.

CLI commands accept prefix matches: `kontora done kon` resolves to the ticket with ID `kon-q88f` if it's the only match.

## Creating tickets

**With the CLI:**

```bash
kontora new "Fix the search index"                              # uses current git root
kontora new --path ~/projects/kontora "Fix the search index"      # explicit path
```

**Manually:** Create a `.md` file in `tickets_dir` with the frontmatter above. The daemon watches the directory and lists valid ticket files with an `id`. It only picks up new `status: todo` tickets automatically when they also have `kontora: true`; otherwise they remain visible as `not a kontora ticket` until initialized.
