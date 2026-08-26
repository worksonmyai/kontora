# API Reference

When the web server is enabled, the following endpoints are exposed:

| Endpoint | Description |
|----------|-------------|
| `GET /` | Static dashboard UI. |
| `GET /api/tickets` | List all board tickets (JSON). `?all=true` adds the ones whose status has no board column: archived, legacy `closed`, and any foreign status. |
| `POST /api/tickets` | Create a new ticket (JSON body: `title`, `path`, optional `pipeline`, `agent`, `status`, `body`, `branch`, `base_branch`). |
| `GET /api/tickets/{id}` | Get ticket details (JSON). |
| `DELETE /api/tickets/{id}` | Delete the ticket markdown file without worktree cleanup. Requires `X-Kontora-Confirm: delete-ticket-file`. Only deletes files inside `tickets_dir`. |
| `POST /api/tickets/{id}/pause` | Pause a running ticket. |
| `POST /api/tickets/{id}/retry` | Retry a paused ticket. |
| `POST /api/tickets/{id}/skip` | Skip the current pipeline stage. |
| `POST /api/tickets/{id}/set-stage` | Move ticket to a specific pipeline stage (`{"stage": "..."}` body). |
| `POST /api/tickets/{id}/move` | Set ticket status (`{"status": "..."}` body). |
| `GET /api/config` | Available pipelines, agents, and projects (JSON). Projects are sorted by name and carry `name`, `path`, `resolved_path` (`~` expanded), `pipeline`, and `agent`. |
| `GET /api/tickets/{id}/logs` | Get agent logs for a ticket (optional `?stage=` query param). |
| `POST /api/tickets/{id}/summary` | Set the ticket's `summary` field (`{"text": "..."}` body). |
| `GET /api/tickets/{id}/changes` | Commits and changed files on the ticket's branch relative to its `base_branch`, or the repo's default branch when unset. Empty payload when the ticket has no branch or the branch was deleted. |
| `GET /api/tickets/{id}/chain` | The dependency chain through the ticket: everything it transitively waits on, itself, and everything that transitively waits on it. |
| `POST /api/tickets/{id}/init` | Initialize a non-kontora ticket (`pipeline`, `path`, optional `agent`). |
| `POST /api/tickets/{id}/dep` | Make the ticket wait on another one (`{"related": ["<id>"]}` body, exactly one id). |
| `POST /api/tickets/{id}/undep` | Drop a dependency edge (`{"related": ["<id>"]}` body, exactly one id). |
| `POST /api/tickets/{id}/link` | Relate the ticket to each id in `{"related": [...]}`, on both sides. |
| `POST /api/tickets/{id}/unlink` | Remove the relation between the ticket and each id in `{"related": [...]}`. |
| `PUT /api/tickets/{id}` | Update an open ticket's body or frontmatter fields (`body`, `pipeline`, `path`, `agent`, `branch`, `base_branch`). |
| `POST /api/tickets/upload` | Import tickets from raw `.md` file content (multipart form). Requires `X-Kontora-Confirm: upload-tickets`. |
| `POST /api/tickets/{id}/plannotator-review` | Open the ticket's branch diff in Plannotator. Only in `human_review`. Submitted feedback routes the ticket to the built-in rework stage. See [plannotator](configuration.md#plannotator). |
| `POST /api/tickets/{id}/plannotator-annotate` | Open the ticket's own markdown in Plannotator. Only in `open`. Submitted annotations set `kontora: true` and schedule a run that rewrites the ticket. |
| `GET /api/assistant` | Whether the [assistant](configuration.md#assistant) is configured: `enabled`, and when it is, `agent`, `kind`, `model`, `autonomy` and `workdir`. `enabled: false` carries a `hint` naming what to set, which is what the pane shows in place of its composer. |
| `GET /api/assistant/threads` | The chat history, most recently used first. |
| `POST /api/assistant/threads` | Open a chat (optional `{"autonomy": "read"\|"ask"\|"auto"}`; the configured default otherwise). Answers 201 with the chat. |
| `GET /api/assistant/threads/{id}` | One chat and the messages posted to it. |
| `DELETE /api/assistant/threads/{id}` | Drop a chat and its transcript. |
| `GET /api/assistant/threads/{id}/activity` | One poll of a chat: the transcript as a `logfmt` tape sliced at `?after=N`, the messages, whether a turn is running, the message being written right now, and any change waiting on the user. Carries an `ETag` and answers `If-None-Match` with 304. |
| `GET /api/assistant/threads/{id}/stream` | Server-Sent Events stream of the message being written, pushed as it grows. Answers 204 when nothing is running and nothing was typed. |
| `POST /api/assistant/threads/{id}/messages` | Post a message (`{"text": "...", "autonomy": "..."}`). Answers 202: the reply arrives through the activity poll. |
| `POST /api/assistant/threads/{id}/stop` | Cancel the turn the chat is running. |
| `POST /api/assistant/gate/{gid}` | Answer a change the assistant is waiting on (`{"decision": "approve"\|"skip"}`). |
| `POST /api/assistant/gate/ask` | The agent side of the tool gate, called by the claude `PreToolUse` hook and the pi `tool_call` handler. Authenticated by the per-turn secret in the agent's environment, not by the web token alone. Blocks while a change waits on the user. |
| `GET /api/events` | Server-Sent Events stream of ticket updates. |
| `GET /ws/terminal/{id}` | Read-only WebSocket relay of a running agent's tmux session. |
| `GET /health` | Health check (returns 200). |

### The message being written

`GET /api/assistant/threads/{id}/activity` carries three fields for the message
the agent is writing before its session file records it: `partial`, the text so
far; `partial_gen`, bumped when a new block starts, so a reader replaces rather
than appends; and `partial_tool`, the tool call whose arguments are still being
generated. `partial` is empty once the tape carries the same words, and the
response that empties it is the one that carries them, so nothing renders twice
and nothing blanks between the two. A turn that ended without recording its
message keeps its `partial` alongside `running: false`, since that copy is then
the only one there is. The text is held in daemon memory only: it is never part
of the `logfmt` tape, which is the parsed form of a file.

`GET /api/assistant/threads/{id}/stream` pushes the same text at roughly ten
frames a second, so a reader sees it as typing rather than in 1.5s steps. It is
an enhancement over the poll: a client that cannot open it still renders growing
`partial` text from successive polls.

```
event: reset   data: {"gen":3,"text":"<everything so far>","tool":""}
event: delta   data: {"gen":3,"text":"<suffix since the last frame>"}
event: tool    data: {"gen":3,"name":"Bash"}
event: end     data: {}     # the block stopped growing; the text stays
event: done    data: {}     # the turn is over and the server is closing
: keepalive                 # after 20s of silence
```

A `reset` arrives on connect and again whenever `gen` changes; a client ignores
a `delta` whose `gen` it does not hold. The connection answers 204, rather than
opening, when nothing is running and nothing was typed: `EventSource` does not
retry a 204, so a stale pane stops asking. Like `GET /api/events`, the response
is never gzipped.

On `POST /api/tickets` and `POST /api/tickets/{id}/init`, a blank `pipeline` or `agent` takes the default of the [project](configuration.md#projects) matching `path`. Send the literal `none` to leave that field blank and skip its project default. `PUT /api/tickets/{id}` reads `none` the same way, as "clear this field"; it has no project default to skip.

On `POST /api/tickets/{id}/init`, a blank `pipeline` or `agent` first falls back to what the ticket file already declares, so the project default fills an empty field instead of replacing the ticket's own value.

Both Plannotator endpoints return 202 once the session is accepted, and report its outcome over `GET /api/events` as a `plannotator_finished` event. They return 404 for an unknown ticket, 409 when a Plannotator process is already open for the ticket or its status does not allow the requested pass, and 500 when the `plannotator` binary is not installed. `GET /api/tickets` and `GET /api/tickets/{id}` carry `can_annotate`, which answers the same status rules the annotate endpoint enforces.

Relations ride both ticket payloads as arrays of `{id, title, status}`. `GET /api/tickets` carries the id alone; `GET /api/tickets/{id}` and the SSE ticket updates also carry the title and status of every ticket the daemon still has on disk, and add the two derived reverse edges: `blocks`, the tickets whose `deps` name this one, and `children`, the tickets whose `parent` names it. Both are sorted by id and absent from the list payload. A ref with no title and no status names a ticket that is not in `tickets_dir`. See [Relations](tickets.md#relations).

`GET /api/tickets/{id}/chain` is the derived view of the same edges. It walks the whole store, so archived tickets and statuses with no board column stay in the graph. `nodes` is sorted roots to goal with `depth` monotonic non-decreasing, and every upstream node comes before the ticket itself, which comes before every downstream node. Each node carries `depth`, `direction` (`upstream`, `self` or `downstream`), `on_critical_path`, `holds_chain`, `waits_on` (`{open, total}` over that node's own `deps`, including the deps outside the chain) and `missing`. At most one node has `holds_chain` set: the node the chain waits on. While a dep blocks the ticket that is an unresolved dep upstream with no unresolved dep of its own, so it names work that can start now; while nothing blocks the ticket it is the first unresolved node from the ticket onward along the critical path, which is the ticket itself until it closes. A chain whose nodes are all resolved has no such node.

`verdict` is `blocked`, `ready` or `cycle`. It reports the deps alone rather than the ticket's own status, so a `done` ticket with closed deps reads `ready`. `position`, `path_length` and `goal` place the ticket on the critical path. On `cycle` the ids on the cycle are in `cycle`, `nodes` is empty, and `position`, `path_length` and `goal` are zero: a graph with a cycle has no order to draw. `total` is the true node count and `nodes` is capped at 200, so a `total` larger than the length of `nodes` means the payload was trimmed; the critical path is kept whatever the cap. `done` counts the nodes that no longer block, which includes the ones in `human_review` and the cancelled and archived ones. A node whose id names no ticket file has `missing: true` with an empty `title` and `status`. An unknown ticket id returns 404.

A `children` entry carries more than the other relation arrays, so a sub-ticket row renders without another request: `stage`, `stage_index` and `stage_count` for the child's position in its own pipeline, and `started_at` and `completed_at` for its wall time. `stage_index` is 1-based and absent when the child's stage is not in its pipeline. The timestamps bound the child's whole run, first pickup to last exit, rather than the current stage. `completed_at` is absent while the child is `in_progress`.

Both ticket payloads carry `project`, the configured project whose path is the ticket's, and the derived readiness: `ready` when no dependency holds the ticket back, and `blockers` naming the dependency ids that do when one does. Neither is stored; see [dependency-aware scheduling](tickets.md#dependency-aware-scheduling). `POST /api/tickets/{id}/run` answers with the ticket, so a caller can see from `blockers` whether the ticket it just moved to `todo` will actually be picked up.

The four relation endpoints take the same body, `{"related": [...]}`, and answer with the changed ticket. They return 400 for an empty list, an empty id, or more than one id on the two dependency verbs; 404 for an id no ticket answers; and 409 when the ticket is related to itself or when a dependency would close a cycle, with the cycle named in the error. A rejected call writes no file. Repeating a call that has nothing left to do returns 200 and writes nothing.

A link is written to both tickets, one file at a time, because two markdown files cannot be written together. When the second write fails the error names both tickets and which one was already changed, and repeating the request repairs the missing side.

A ticket whose `branch` is empty carries `auto_branch` in `GET /api/tickets` and `GET /api/tickets/{id}`: the branch the daemon would assign at pickup, resolved for the path the ticket names and the current [branch naming](configuration.md#branch-naming) mode. It is a read-only projection, not a stored field, and it is absent once `branch` is set.

The assistant endpoints answer 501 when no `assistant.agent` is configured, 404 for an unknown chat, and 409 for a second message on a chat whose turn is still running. A chat keeps the agent it was created with, so a message to one whose agent has been repointed or removed also answers 409: its agent session cannot resume on another CLI, and the chat has to be started again. 503 is the separate refusal for the daemon's global turn cap, which is not about this chat at all. `POST /api/assistant/gate/{gid}` answers 404 once the change has already been answered or the turn that raised it has ended, so a stale card cannot resolve twice. `POST /api/assistant/gate/ask` answers 403 when the secret does not match the chat's current turn, which is what stops an unrelated local process approving its own writes against a tokenless loopback daemon.

## Base branch validation

`base_branch` names the branch a ticket's worktree is cut from. See [Base branch](tickets.md#base-branch) for what the field means.

Both `POST /api/tickets` and `PUT /api/tickets/{id}` check the name's format and return 400 with `invalid base branch name` when it is malformed. Only creation checks that the branch exists, and it fails before writing the ticket file. A create request with `"status":"open"` skips that check, because an open ticket is not ready to run. An update accepts a name that resolves to nothing, because the repository at the ticket's `path` can change afterwards; the failure surfaces when the daemon builds the worktree and pauses the ticket.

The format check accepts any valid git branch name, which includes tag-shaped names such as `v1.0`. Restricting the base to real branches happens during resolution, not during format validation.
