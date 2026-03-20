# Configuration Reference

Kontora reads configuration from a YAML file. It checks these paths in order: `.kontora/config.yaml` in the current directory, then `$XDG_CONFIG_HOME/kontora/config.yaml` (or `~/.config/kontora/config.yaml` if unset). Override with `--config`. Unknown fields are rejected. See also: [Ticket Format](tickets.md).

## Minimal example

```yaml
tickets_dir: ~/.kontora/tickets

agents:
  claude:
    binary: claude

roles:
  code:
    prompt: Write code.

pipelines:
  default:
    - role: code
      agent: claude
      on_success: done
      on_failure: pause
```

## Full example

```yaml
tickets_dir: ~/.kontora/tickets
branch_prefix: kontora
worktrees_dir: ~/.kontora/worktrees
logs_dir: ~/.kontora/logs
editor: nvim
max_concurrent_agents: 3

web:
  enabled: true
  host: 127.0.0.1
  port: 8080

agents:
  claude-sonnet:
    binary: claude
    args: ["--dangerously-skip-permissions", "--model", "sonnet"]
  claude-opus:
    binary: claude
    args: ["--dangerously-skip-permissions", "--model", "opus"]

roles:
  code:
    prompt: |
      {{ .Ticket.Description }}
    timeout: 30m
  implement:
    prompt: |
      {{ .Ticket.Description }}

      Do NOT commit or push. Only implement the code and run tests.
    timeout: 60m
  review:
    prompt: |
      Review the code changes for this ticket. Check for:
      - Correctness and edge cases
      - Code quality and maintainability
      - Test coverage

      Write all review results to the ticket {{ .Ticket.ID }}
    timeout: 30m
  fix-review:
    prompt: |
      Read the code review from the notes of {{ .Ticket.ID }}

      If there are issues listed, fix all of them and run tests.
    timeout: 30m
  commit:
    prompt: |
      Commit uncommitted files. This is the ready and reviewed
      implementation of the ticket {{ .Ticket.ID }}.

      You have full authorization to commit.
      Do not ask for confirmation — just do it.
    timeout: 5m

pipelines:
  default:
    - role: code
      agent: claude-sonnet
      on_success: done
      on_failure: pause

  implement-review-commit:
    - role: implement
      agent: claude-sonnet
      on_success: next
      on_failure: pause
    - role: review
      agent: claude-sonnet
      on_success: next
      on_failure: retry
      max_retries: 1
    - role: fix-review
      agent: claude-sonnet
      on_success: next
      on_failure: retry
      max_retries: 1
    - role: commit
      agent: claude-sonnet
      on_success: done
      on_failure: retry
      max_retries: 1
```

## Top-level fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `tickets_dir` | no | `~/.kontora/tickets` | Directory containing ticket markdown files. |
| `branch_prefix` | no | `kontora` | Git branch prefix. Branches are named `<prefix>/<ticket-id>`. |
| `worktrees_dir` | no | `~/.kontora/worktrees` | Where git worktrees are created. |
| `logs_dir` | no | `~/.kontora/logs` | Where agent output logs are stored. |
| `editor` | no | `$EDITOR` or `vi` | Editor for `kontora edit`. Falls back to `$EDITOR`, then `vi`. |
| `default_agent` | no | (inferred) | Agent used for tickets without a pipeline. Defaults to `claude` if an agent with that name exists, otherwise inferred when there is exactly one agent. Must be set explicitly when multiple agents are defined and none is named `claude`. |
| `max_concurrent_agents` | no | `3` | Maximum number of agents running simultaneously. |
| `environment` | no | — | Map of environment variables to set for all agent processes. |
| `web` | no | — | Web dashboard settings (see [web](#web)). Enabled by default. |

All paths support `~` for the home directory. Tilde expansion happens at runtime, not at config load time.

## web

Optional HTTP server for monitoring and controlling tickets from a browser.

```yaml
web:
  enabled: true
  host: 0.0.0.0
  port: 9090
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `enabled` | no | `true` | Start the web server when the daemon runs. |
| `host` | no | `127.0.0.1` | Bind address. |
| `port` | no | `8080` | Listen port. |

When enabled, the server exposes:

| Endpoint | Description |
|----------|-------------|
| `GET /` | Static dashboard UI. |
| `GET /api/tickets` | List all tickets (JSON). |
| `POST /api/tickets` | Create a new ticket (JSON body: `title`, `path`, optional `pipeline`, `status`). |
| `GET /api/tickets/{id}` | Get ticket details (JSON). |
| `DELETE /api/tickets/{id}` | Delete the ticket markdown file without worktree cleanup. Requires `X-Kontora-Confirm: delete-ticket-file`. Only deletes files inside `tickets_dir`. |
| `POST /api/tickets/{id}/pause` | Pause a running ticket. |
| `POST /api/tickets/{id}/retry` | Retry a paused ticket. |
| `POST /api/tickets/{id}/skip` | Skip the current pipeline stage. |
| `POST /api/tickets/{id}/set-stage` | Move ticket to a specific pipeline stage (`{"stage": "..."}` body). |
| `POST /api/tickets/{id}/move` | Set ticket status (`{"status": "..."}` body). |
| `GET /api/config` | Available repos and pipelines (JSON). |
| `GET /api/tickets/{id}/logs` | Get agent logs for a ticket (optional `?stage=` query param). |
| `POST /api/tickets/{id}/init` | Initialize a non-kontora ticket (`pipeline`, `path`, optional `agent`). |
| `PUT /api/tickets/{id}` | Update an open ticket's body or frontmatter fields. |
| `POST /api/tickets/upload` | Import tickets from raw `.md` file content (multipart form). |
| `GET /api/events` | Server-Sent Events stream of ticket updates. |
| `GET /ws/terminal/{id}` | Read-only WebSocket relay of a running agent's tmux session. |
| `GET /health` | Health check (returns 200). |

## agents

Map of agent name to its binary and arguments. Any CLI tool that accepts a prompt on stdin or as an argument can be an agent.

```yaml
agents:
  claude-sonnet:
    binary: claude
    args: ["--dangerously-skip-permissions", "--model", "sonnet"]
```

| Field | Required | Description |
|-------|----------|-------------|
| `binary` | yes | Executable name or path. |
| `args` | no | Arguments passed to the binary. The rendered prompt is appended as the last argument. |
| `environment` | no | Map of environment variables to set for this agent's processes (merged with top-level `environment`). |

## roles

Map of role name to its prompt template and timeout. A role defines *what* an agent should do at a pipeline stage.

```yaml
roles:
  code:
    prompt: |
      {{ .Ticket.Description }}
    timeout: 30m
```

| Field | Required | Description |
|-------|----------|-------------|
| `prompt` | yes | Go template rendered before passing to the agent. |
| `timeout` | no | Maximum duration for the agent (e.g., `10m`, `1h30m`). |

### Prompt templates

Prompts are Go [text/template](https://pkg.go.dev/text/template) strings with these variables and functions:

| Expression | Description |
|------------|-------------|
| `{{ .Ticket.ID }}` | Ticket ID (e.g., `poi-q88f`). |
| `{{ .Ticket.Title }}` | First `# Heading` from the ticket body. |
| `{{ .Ticket.Description }}` | Full ticket body (markdown after frontmatter). |
| `{{ .Ticket.FilePath }}` | Absolute path to the ticket's markdown file. |
| `{{ file "PLAN.md" }}` | Contents of a file relative to the ticket's worktree. |

The `file` function is how stages communicate — an earlier stage writes a file (e.g., `PLAN.md`) and a later stage reads it via the template.

## pipelines

Map of pipeline name to an ordered list of stages. Each ticket references a pipeline by name in its `pipeline` frontmatter field.

```yaml
pipelines:
  default:
    - role: code
      agent: claude
      on_success: done
      on_failure: pause
```

### Stage fields

| Field | Required | Values | Description |
|-------|----------|--------|-------------|
| `role` | yes | — | Role to run at this stage. |
| `agent` | yes | — | Agent to run the role. |
| `on_success` | yes | `next`, `done` | What to do when the agent exits 0. |
| `on_failure` | yes | `retry`, `back`, `pause` | What to do when the agent exits non-zero. |
| `max_retries` | no | integer (default `0`) | Maximum retry attempts (only relevant when `on_failure=retry`). |

### Policies

**`on_success`**:
- `next` — advance to the next stage (set status back to `todo` so the scheduler picks it up).
- `done` — mark the ticket as complete. Required on the last stage.

**`on_failure`**:
- `retry` — re-run the same stage (up to `max_retries`, then pause).
- `back` — go back to the previous stage. Not allowed on the first stage.
- `pause` — set the ticket to `paused` for human review.

### Validation rules

- Every stage must reference a role and agent that exist in the config.
- `on_success` must be `next` or `done`.
- `on_failure` must be `retry`, `back`, or `pause`.
- `on_failure=back` is not allowed on the first stage.
- The last stage must have `on_success=done`.
- A role cannot appear more than once in the same pipeline.
