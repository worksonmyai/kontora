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

These are set when creating a ticket (manually or via `kontora new`):

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `id` | yes | — | Unique identifier, format `<prefix>-<4 alphanum>` (e.g., `kon-q88f`). |
| `status` | yes | `todo` | Current ticket status (see [Status lifecycle](#status-lifecycle)). |
| `pipeline` | no | — | Name of the pipeline to run (must exist in config). Filled in at creation from a matching [project](configuration.md#projects) when one is configured and no pipeline is given. When it ends up empty, the ticket runs in standalone mode with the default agent. |
| `agent` | no | — | Override the agent for this ticket. Applies to standalone tickets or overrides the pipeline's agent at every stage. Filled in at creation from a matching [project](configuration.md#projects) when one is configured and no agent is given. |
| `path` | yes | — | Path to the repository (supports `~`, e.g., `~/projects/kontora`). |
| `created` | no | — | RFC 3339 timestamp. Set automatically by `kontora new`. |

### Daemon-managed fields

These are set and updated by the daemon as the ticket progresses through its pipeline. Do not edit them manually while the daemon is running.

| Field | Description |
|-------|-------------|
| `kontora` | Boolean. When `true`, the daemon may manage and execute this ticket. Tickets without `kontora: true` are still listed, but are not auto-picked up and cannot be run/retried/skipped until initialized. Set by `kontora init`. |
| `stage` | Current pipeline stage name. Set to the first stage on pickup. |
| `attempt` | Retry counter for the current stage. Reset to 0 on advance/back. |
| `started_at` | When the current stage started. |
| `completed_at` | When the ticket finished (status became `done`). |
| `branch` | Git worktree branch name (`kontora/<ticket-id>`). |
| `history` | List of completed stage records. |
| `last_error` | Error message from the last failed stage run. |
| `summary` | Summary of the stage run that ended most recently: the text the agent wrote with `kontora summary`, or its final session message as a fallback. Cleared when a new stage starts; each stage keeps its own copy on its `history` entry. |
| `claimed_by` | The daemon [`instance_name`](configuration.md) that last picked the ticket up. Written in the same update that sets `status: in_progress`. Consulted only while the ticket is `in_progress`; a stale value on a `todo` or `done` ticket is ignored. |

### Custom fields

Any field not listed above is preserved through round-trips. The daemon will not overwrite or remove fields it doesn't recognize:

```yaml
---
id: poi-q88f
status: todo
pipeline: default
deps: []
type: ticket
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
| `open` | Drafted but not ready for the daemon to pick up. |
| `todo` | Ready for the daemon. The scheduler will pick it up in creation order. |
| `in_progress` | An agent is currently working on it. |
| `paused` | Stopped by a failure policy or by the user. Set `status: todo` to resume. |
| `done` | All pipeline stages completed successfully. |
| `cancelled` | Manually cancelled by the user. |
| `archived` | An old closed ticket, hidden from the main views. Terminal; still on disk. |

The daemon only picks up tickets that have both `kontora: true` and `status: todo`. A non-Kontora ticket can still appear in any status column, but Kontora will not start or resume work on it until you initialize it. To pause a running Kontora ticket, use `kontora pause <id>` or set `status: paused` in the file — the daemon will detect the change and stop the agent.

## Running on multiple machines

When a `tickets_dir` is synced across machines (iCloud, Syncthing, ...), two daemons can see the same tickets. File sync is eventually consistent, so there is no cross-machine lock; instead each daemon records its [`instance_name`](configuration.md) in `claimed_by` when it picks a ticket up, and consults that claim before acting on an `in_progress` ticket:

- **Crash recovery** on startup resets an `in_progress` ticket back to `todo` only when `claimed_by` is empty or matches this daemon. A ticket claimed by another instance is left running; starting a second daemon no longer steals or kills the first's work.
- **During a run**, if a foreign claim arrives through sync, the daemon cancels its own agent and leaves the file untouched. The losing worktree is kept (it may hold unpushed commits).
- **Both sides can briefly claim** the same `todo` ticket within the sync window. Once sync settles, one claim wins and the other daemon yields; a ticket left `in_progress` with a self-claim but no running agent is reset to `todo` and re-enqueued.

There is no automatic takeover of a claim by claim age or a heartbeat. If the owning machine goes offline mid-run, its tickets stay `in_progress` until that daemon restarts or someone manually sets `status: todo`. Two machines that share a hostname must set `instance_name` explicitly, or the protection cannot tell them apart.

`archived` is a terminal built-in status for old `done`/`cancelled` tickets. Archived tickets are hidden from `kontora ls` (including `--closed`), the TUI, and the WebUI board, but their markdown files are kept on disk. The daemon never enqueues an archived ticket, and a live transition to `archived` stops any running agent and cleans up its worktree, just like `done`/`cancelled`. Create archived tickets with `kontora archive` (below) or by editing a file's `status` field directly; it cannot be set as a custom status or through the WebUI move actions.

## Archiving old tickets

```bash
kontora archive --days 30            # archive done/cancelled tickets untouched for 30+ days
kontora archive --days 30 --yes      # skip the confirmation prompt
kontora archive --days 30 --dry-run  # list what would be archived, write nothing
kontora archive --days 30 --path ~/projects/kontora  # limit the run to one repository
kontora archive --days 30 --project kontora          # the same, by configured project name
kontora archive --days 7 --status cancelled          # sweep cancelled tickets sooner than done ones
```

`kontora archive --days N` marks every `done` or `cancelled` ticket whose markdown file was last modified at or before `now - N days` as `archived`. The cutoff uses the file's modification time (the same value the WebUI shows as "updated"), because cancelled tickets have no `completed_at`. `--days` is required and must be a positive number.

Every run first prints a table of the matching tickets, with the ID, the closed status, the title taken from the ticket's first markdown heading, and the repository path. `--dry-run` stops there and changes nothing. Otherwise the command asks `Archive N tickets? [y/N]` and archives only on `y` or `yes`; any other answer leaves every file alone. `-y`/`--yes` archives without asking. When stdin is not a terminal, the prompt cannot be answered, so a run that would write files fails and asks for `--yes` instead.

`--path` limits the run to tickets whose `path` field is that repository. Both sides are compared after tilde expansion and cleaning, so `~/projects/kontora`, the same directory in absolute form, and a trailing slash all select the same tickets. Only the complete path matches: a ticket pointing at a subdirectory of the given path is left alone, as is a ticket with no `path` field.

`--project NAME` selects the same tickets through the `path` of a project in the config `projects` map, so the path does not have to be typed. A name that is not configured is an error, because a typo would otherwise look like a run where nothing was old enough. `--path` and `--project` cannot be combined.

`--status` narrows the run to one of the two closed statuses, `done` or `cancelled`. Any other value is rejected. Without it both are archived.

## History

The daemon appends a record to `history` after each stage completes:

```yaml
history:
  - stage: plan
    agent: claude-opus
    exit_code: 0
    started_at: 2026-03-01T10:01:00Z
    completed_at: 2026-03-01T10:05:00Z
  - stage: code
    agent: claude-sonnet
    exit_code: 1
    started_at: 2026-03-01T10:06:00Z
    completed_at: 2026-03-01T10:15:00Z
```

## Notes

Use `kontora note <ticket-id> "message"` to append timestamped notes under a `## Notes` section in the body. This is how you communicate with the agent between stages — the next stage's prompt can include `{{ .Ticket.Description }}` to read the full body including notes.

```
## Notes

**2026-03-06T12:00:00Z**

Use the existing search index, don't create a new one.
```

## Ticket ID format

IDs are `<prefix>-<4 random alphanumeric chars>` (e.g., `poi-q88f`).

The prefix is derived from the first 3 lowercase alphanumeric characters of the directory name (from the ticket's `path` field).

CLI commands accept prefix matches: `kontora done kon` resolves to the ticket with ID `kon-q88f` if it's the only match.

## Creating tickets

**With the CLI:**

```bash
kontora new "Fix the search index"                              # uses current git root
kontora new --path ~/projects/kontora "Fix the search index"      # explicit path
```

**Manually:** Create a `.md` file in `tickets_dir` with the frontmatter above. The daemon watches the directory and lists valid ticket files with an `id`. It only picks up new `status: todo` tickets automatically when they also have `kontora: true`; otherwise they remain visible as `not a kontora ticket` until initialized.
