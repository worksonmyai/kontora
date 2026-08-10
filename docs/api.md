# API Reference

When the web server is enabled, the following endpoints are exposed:

| Endpoint | Description |
|----------|-------------|
| `GET /` | Static dashboard UI. |
| `GET /api/tickets` | List all tickets (JSON). |
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
| `POST /api/tickets/{id}/init` | Initialize a non-kontora ticket (`pipeline`, `path`, optional `agent`). |
| `PUT /api/tickets/{id}` | Update an open ticket's body or frontmatter fields (`body`, `pipeline`, `path`, `agent`, `branch`, `base_branch`). |
| `POST /api/tickets/upload` | Import tickets from raw `.md` file content (multipart form). |
| `GET /api/events` | Server-Sent Events stream of ticket updates. |
| `GET /ws/terminal/{id}` | Read-only WebSocket relay of a running agent's tmux session. |
| `GET /health` | Health check (returns 200). |

On `POST /api/tickets` and `POST /api/tickets/{id}/init`, a blank `pipeline` or `agent` takes the default of the [project](configuration.md#projects) matching `path`. Send the literal `none` to leave that field blank and skip its project default. `PUT /api/tickets/{id}` reads `none` the same way, as "clear this field"; it has no project default to skip.

On `POST /api/tickets/{id}/init`, a blank `pipeline` or `agent` first falls back to what the ticket file already declares, so the project default fills an empty field instead of replacing the ticket's own value.

A ticket whose `branch` is empty carries `auto_branch` in `GET /api/tickets` and `GET /api/tickets/{id}`: the branch the daemon would assign at pickup, resolved for the path the ticket names and the current [branch naming](configuration.md#branch-naming) mode. It is a read-only projection, not a stored field, and it is absent once `branch` is set.

## Base branch validation

`base_branch` names the branch a ticket's worktree is cut from. See [Base branch](tickets.md#base-branch) for what the field means.

Both `POST /api/tickets` and `PUT /api/tickets/{id}` check the name's format and return 400 with `invalid base branch name` when it is malformed. Only creation checks that the branch exists, and it fails before writing the ticket file. A create request with `"status":"open"` skips that check, because an open ticket is not ready to run. An update accepts a name that resolves to nothing, because the repository at the ticket's `path` can change afterwards; the failure surfaces when the daemon builds the worktree and pauses the ticket.

The format check accepts any valid git branch name, which includes tag-shaped names such as `v1.0`. Restricting the base to real branches happens during resolution, not during format validation.
