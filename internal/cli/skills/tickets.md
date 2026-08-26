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
| `status` | yes | See the status lifecycle below. Defaults to `todo`. |
| `path` | yes | Repository the ticket works in. `~` is expanded at runtime. |
| `pipeline` | no | Name of a pipeline in the config. Empty means the ticket runs standalone with the default agent. |
| `agent` | no | Agent override for this ticket. Replaces the pipeline's agent at every stage. |
| `base_branch` | no | Branch the work branch starts from. Empty means the repository's default branch. |
| `created` | no | RFC 3339 timestamp. Set by `kontora new`. |
| `deps` | no | Ids this ticket waits on. |
| `links` | no | Ids of related tickets. |
| `parent` | no | Id of the epic or parent ticket. No command writes it. |

`pipeline` and `agent` are filled in at creation from a matching project in the
config when one exists and neither is given. Pass the literal `none` to opt out
of a project default.

Any field not listed here is preserved through a round trip. The daemon does
not remove or overwrite fields it does not recognise.

## Daemon-managed fields

Written by the daemon as a ticket progresses. Never set them by hand.

| Field | Description |
|-------|-------------|
| `kontora` | `true` means the daemon may run this ticket. Set by `kontora init`. |
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

```
open -> todo -> in_progress -> done ------> archived
                    |                          ^
             paused / cancelled ---------------+
```

| Status | Meaning |
|--------|---------|
| `open` | Drafted, not ready for the daemon to pick up. |
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

The daemon only picks up a ticket that has both `kontora: true` and
`status: todo`.

## Relations

`deps` names the tickets this one waits on. The scheduler holds the ticket in
`todo` until every id in `deps` names a ticket that is `human_review`, `done`,
`cancelled`, `archived` or legacy `closed`. `human_review` releases dependents
because the agent's work is done and only a person's verdict is left. Any other
status blocks, and so does an id with no file behind it.

`links` is a symmetric "related" list and means nothing to the scheduler.
`parent` names an epic and is read-only.

Two relations are derived on read rather than stored, because each edge lives
on the other ticket: `blocks` is the reverse of `deps`, and `children` is the
reverse of `parent`.

Readiness is derived on every read. It is not a status: there is no `blocked`
status, and a ticket waiting on a dependency stays `todo`. Closing a dependency
queues every ticket that waited on it, with no edit to their files.

A cycle never starts an agent: every ticket on it waits on one that is not
closed. `kontora dep` refuses to create a new cycle.

Write these with `kontora dep`, `kontora undep`, `kontora link` and
`kontora unlink`.

## Body

Everything after the closing `---` is markdown. The first `# Heading` is the
ticket title, available to a stage prompt as `{{ .Ticket.Title }}`; the whole
body is `{{ .Ticket.Description }}`.

`kontora note` appends a timestamped entry under a `## Notes` heading. That is
how one stage leaves something for the next, because the next stage's prompt
usually includes the whole body.

## Ticket ID format

`<prefix>-<4 random alphanumerics>`, e.g. `kon-q88f`.

The prefix is derived from the directory name in the ticket's `path`: the first
alphanumeric of each `-` or `_` separated segment, lowercased and joined. A
name yielding fewer than two of them uses its own first three alphanumerics
instead. `kontora` gives `kon`, `deployment_tools` gives `dt`, `sigil` gives
`sig`. A project can pin a prefix with `prefix:` in its config entry.

## History

Each completed stage run appends a record:

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

`model` and `effort` are what the run resolved to, not what the agent reported
running as. `session_kind` and `session_ref` name the agent's own session file;
`kontora sessions` turns them back into paths. Both are absent for an agent
that is neither claude nor pi.
