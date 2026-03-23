# Ticket Format

Tickets are markdown files with YAML frontmatter. They live in the directory specified by `tickets_dir` in the [configuration](configuration.md). Any tool that can write a markdown file — a text editor, Obsidian, a script — is a valid client.

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
| `pipeline` | no | — | Name of the pipeline to run (must exist in config). When omitted, the ticket runs in standalone mode with the default agent. |
| `agent` | no | — | Override the agent for this ticket. Applies to standalone tickets or overrides the pipeline's agent at every stage. |
| `path` | yes | — | Path to the repository (supports `~`, e.g., `~/projects/kontora`). |
| `created` | no | — | RFC 3339 timestamp. Set automatically by `kontora new`. |

### Daemon-managed fields

These are set and updated by the daemon as the ticket progresses through its pipeline. Do not edit them manually while the daemon is running.

| Field | Description |
|-------|-------------|
| `kontora` | Boolean. When `true`, the daemon manages this ticket. Set by `kontora init`. |
| `stage` | Current pipeline stage name. Set to the first stage on pickup. |
| `attempt` | Retry counter for the current stage. Reset to 0 on advance/back. |
| `started_at` | When the current stage started. |
| `completed_at` | When the ticket finished (status became `done`). |
| `branch` | Git worktree branch name (`kontora/<ticket-id>`). |
| `history` | List of completed stage records. |

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
open → todo → in_progress → done
                   ↓
            paused / cancelled
```

| Status | Meaning |
|--------|---------|
| `open` | Drafted but not ready for the daemon to pick up. |
| `todo` | Ready for the daemon. The scheduler will pick it up in creation order. |
| `in_progress` | An agent is currently working on it. |
| `paused` | Stopped by a failure policy or by the user. Set `status: todo` to resume. |
| `done` | All pipeline stages completed successfully. |
| `cancelled` | Manually cancelled by the user. |

The daemon only picks up tickets with `status: todo`. To pause a running ticket, use `kontora pause <id>` or set `status: paused` in the file — the daemon will detect the change and stop the agent.

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

**Manually:** Create a `.md` file in `tickets_dir` with the frontmatter above. The daemon watches the directory and picks up new `status: todo` tickets automatically.
