# Configuration Reference

Kontora reads configuration from a YAML file. It checks these paths in order: `.kontora/config.yaml` in the current directory, then `$XDG_CONFIG_HOME/kontora/config.yaml` (or `~/.config/kontora/config.yaml` if unset). Override with `--config`. Unknown fields are rejected. See also: [Ticket Format](tickets.md).

## Creating the config

```bash
kontora setup           # interactive wizard
kontora setup --agent   # print instructions for a coding agent
```

`kontora setup` writes a starter config with two pipelines when the resolved
path holds no file. An existing config is never overwritten: the command prints
its path and the two ways to change it.

`kontora setup --agent` writes nothing. It prints a plain-text brief for a
coding agent: the resolved config path, whether the file is missing, valid, or
invalid, the validation error when there is one, and the symlink target when the
path is a link. The brief is embedded in the binary, so it describes the schema
that version accepts. It never prints the config file itself, so no token or
environment value can leak into the output; a validation error can still quote
the names, paths, and patterns it rejected. Use it to create the first config or
to add agents, stages, pipelines, and projects later.

Both forms are local-only and take no positional arguments. `--agent` here is a
boolean, unlike the agent-name `--agent` on `kontora new` and `kontora init`.
The wizard needs a terminal on stdin and stdout; without one it exits non-zero
and points at `kontora setup --agent`.

After setup:

```bash
kontora doctor   # validate config, tools, agent binaries, port
kontora start    # start the daemon; runs until Ctrl-C
```

Then, in another terminal:

```bash
kontora new --path ~/projects/myrepo "Add a health check endpoint"
```

## Minimal example

```yaml
tickets_dir: ~/.kontora/tickets

agents:
  claude:
    binary: claude

stages:
  code:
    prompt: Write code.

pipelines:
  default:
    - stage: code
      agent: claude
      on_success: human_review
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

stages:
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
    - stage: code
      agent: claude-sonnet
      on_success: human_review
      on_failure: pause

  implement-review-commit:
    - stage: implement
      agent: claude-sonnet
      on_success: next
      on_failure: pause
    - stage: review
      agent: claude-sonnet
      on_success: next
      on_failure: retry
      max_retries: 1
    - stage: fix-review
      agent: claude-sonnet
      on_success: next
      on_failure: retry
      max_retries: 1
    - stage: commit
      agent: claude-sonnet
      on_success: human_review
      on_failure: retry
      max_retries: 1
```

## Top-level fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `tickets_dir` | no | `~/.kontora/tickets` | Directory containing ticket markdown files. |
| `branch_prefix` | no | `kontora` | Git branch prefix. A project can override it (see [projects](#projects)). |
| `branch_naming` | no | `mode: slug` | How the daemon names a ticket with an empty `branch` field (see [branch naming](#branch-naming)). |
| `worktrees_dir` | no | `~/.kontora/worktrees` | Where git worktrees are created. |
| `logs_dir` | no | `~/.kontora/logs` | Where agent output logs are stored. |
| `editor` | no | `$EDITOR` or `vi` | Editor for `kontora edit`. Falls back to `$EDITOR`, then `vi`. |
| `default_agent` | no | (inferred) | Agent used for tickets without a pipeline. Defaults to `claude` if an agent with that name exists, otherwise inferred when there is exactly one agent. Must be set explicitly when multiple agents are defined and none is named `claude`. |
| `max_concurrent_agents` | no | `3` | Maximum number of agents running simultaneously. |
| `instance_name` | no | `os.Hostname()` | Identifies this daemon when several run against one synced `tickets_dir`. Written to a ticket's `claimed_by` on pickup so daemons don't steal or kill each other's work (see [multi-machine tickets](tickets.md#running-on-multiple-machines)). Falls back to `default` if the hostname can't be read. Two machines that share a hostname must set this explicitly, or the protection can't tell them apart. |
| `tmux_session` | no | `kontora` | The tmux session the daemon puts agent windows in. Allowed characters are `A-Z a-z 0-9 _ -`, 1 to 64 of them, and the name cannot start with `-`. Set a distinct value per daemon when you run more than one on a machine: startup cleanup, `kontora attach`, the TUI, and the web terminal are all scoped to this session, and two daemons sharing it can also signal each other's agents through the tmux `wait-for` channel when their ticket IDs collide. |
| `statuses` | no | — | Extra parked statuses beyond the built-ins. Agents can park tickets here via `on_success`/`on_failure`. |
| `projects` | no | — | Per-repository pipeline, agent, and branch naming defaults (see [projects](#projects)). |
| `environment` | no | — | Map of environment variables to set for all agent processes. |
| `resume_prompt` | no | (built-in) | Prompt sent to an agent whose stage a daemon restart interrupted, in place of the stage prompt (see [resuming after a restart](#resuming-after-a-restart)). Same template fields as a stage prompt. |
| `web` | no | — | Web dashboard settings (see [web](#web)). Enabled by default. |

All paths support `~` for the home directory. Tilde expansion happens at runtime, not at config load time.

## branch naming

`branch_naming` controls how the daemon names a ticket whose `branch` field is
empty when the run starts:

```yaml
branch_naming:
  mode: off
```

| Mode | Generated branch |
|------|------------------|
| `slug` | `<prefix>/<title-slug>-<ticket-id>`, for example `kontora/fix-retry-double-count-kon-a3f2`. This is the default. |
| `off` | `<prefix>/<ticket-id>`, for example `kontora/kon-a3f2`. |

The slug comes from the ticket's first heading. The daemon removes a leading
`[project]` tag and common filler words, converts the remaining text to
lowercase ASCII words joined by hyphens, and limits the slug to 48 characters.
If the heading has no ASCII letters or digits, the daemon uses
`<prefix>/<ticket-id>`.

In `slug` mode, the daemon stores the generated name before it creates the
worktree. In `off` mode, it stores the ID-derived name after worktree creation
succeeds. Later runs and cleanup use the stored name. A branch already set on
the ticket is never replaced.

The web UI shows the name a ticket would get in the empty branch field of the
start and edit forms, so you can read it before the run starts and type over it
to choose another.

A project can override the top-level mode:

```yaml
projects:
  legacy:
    path: ~/projects/legacy
    branch_naming:
      mode: off
```

`mode` must be `off` or `slug`. Any other value fails config validation.

## web

Optional HTTP server for monitoring and controlling tickets from a browser.

```yaml
web:
  enabled: true
  host: 0.0.0.0
  port: 9090
  token: ""
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `enabled` | no | `true` | Start the web server when the daemon runs. |
| `host` | no | `127.0.0.1` | Bind address. Set to a tailnet IP to allow remote access. |
| `port` | no | `8080` | Listen port. |
| `token` | no | `""` | Shared bearer token. When set, `/api/*` and `/ws/*` require it (via `Authorization: Bearer`, a `kontora_token` cookie, or a `token` query param); empty leaves the API open. `GET /health` and the static UI stay public. |

When a token is set, the CLI can drive the daemon remotely with `KONTORA_URL` and `KONTORA_TOKEN` (see the Remote mode section of the README). The token is the only access control and agents run with `--dangerously-skip-permissions`; a tailnet encrypts the transport, but on untrusted networks put the daemon behind TLS.

See [API Reference](api.md) for the full list of endpoints.

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
| `failure_patterns` | no | Regexes matched against the agent's output log after it exits. A match pauses the ticket even on a clean exit — catching agents that report failures (quota, API errors) without a non-zero exit code. Unset uses the built-in defaults (below); set an explicit list to override, or `[]` to disable. Claude also gets structural detection from its session log regardless. |
| `resume` | no | Set `false` to make every stage this agent runs start a new conversation, even after a daemon restart interrupted one (see [resuming after a restart](#resuming-after-a-restart)). Unset means resume is on for `claude` and `pi`; any other agent always starts fresh. |

When `failure_patterns` is omitted, an agent inherits these defaults, tuned to match agent/provider error output rather than source code or prose (so implementing a rate limiter won't self-pause):

```
(?im)^\s*API Error:               # Claude Code: 4xx/5xx, overloaded, timeouts, ECONNRESET
(?i)Please run /login             # auth lost: "Not logged in" / "Invalid API key"
(?i)usage limit reached           # "Claude AI usage limit reached"
(?i)You've hit your (usage )?limit # usage/session limit stop
(?i)Prompt is too long            # context window exceeded
(?i)insufficient_quota            # OpenAI-backed agents: billing
(?i)exceeded your current quota   # OpenAI-backed agents: billing
(?i)Rate limit reached for        # OpenAI-backed agents: rate limit
```

To turn detection off for an agent, set `failure_patterns: []`.

### Resuming after a restart

When the daemon stops while an agent is mid-stage, the ticket goes back to `todo` and the stage is scheduled again. The worktree and branch survive, so without resume the agent starts a new conversation on top of its own half-finished work, with no memory of why it made those changes. Resume reattaches the stage to the conversation it was in, and sends `resume_prompt` instead of the stage prompt so the agent continues rather than starts over.

The daemon writes `<logs_dir>/<ticket-id>/<stage>.session` while a stage's agent runs and deletes it as soon as the agent returns. A record left on disk therefore means the daemon itself went away. It is not part of the ticket file: everything it points at, including the Claude session files under `~/.claude/projects/`, is local to one machine.

A stage resumes only when every one of these holds. Any other case runs the stage fresh and logs why; nothing here pauses a ticket.

- The agent is `claude` or `pi` and its `resume` is not `false`.
- The record names this same stage, this same agent, this daemon's `instance_name`, and this worktree.
- The agent's session file still exists.
- No tmux window for the ticket is live, which would mean a process may still hold the session.

Pausing a ticket and running `kontora retry` are deliberate restarts: the daemon is still up when the agent dies, so the record is already gone and the stage starts over.

An agent that had in fact finished its work can exit within a second of resuming, which the tmux startup guard reads as a crash. Rather than pause the ticket, the daemon runs the stage once more from its normal prompt in a new session. That fallback happens at most once per scheduled stage and does not consume a pipeline retry. It only covers a resumed run that failed inside its first two seconds: a run that failed later did work that is now in the worktree, so the ticket pauses instead of repeating it.

## stages

Map of stage name to its prompt template and timeout. A stage defines *what* an agent should do at a pipeline step.

```yaml
stages:
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
    - stage: code
      agent: claude
      on_success: human_review
      on_failure: pause
```

### Stage fields

| Field | Required | Values | Description |
|-------|----------|--------|-------------|
| `stage` | yes | — | Stage to run at this pipeline step. |
| `agent` | yes | — | Agent to run the stage. |
| `on_success` | yes | `next`, `done`, `human_review`, or a custom status | What to do when the agent exits 0. |
| `on_failure` | yes | `retry`, `back`, `pause`, `human_review`, or a custom status | What to do when the agent exits non-zero. |
| `max_retries` | no | integer (default `0`) | Maximum retry attempts (only relevant when `on_failure=retry`). |

### Policies

**`on_success`**:
- `next` — advance to the next stage (set status back to `todo` so the scheduler picks it up).
- `done` — mark the ticket as complete.
- `human_review` — park the ticket in `human_review` for a human to look at.
- A custom status declared in top-level `statuses:` — park the ticket in that status.

**`on_failure`**:
- `retry` — re-run the same stage (up to `max_retries`, then pause).
- `back` — go back to the previous stage. Not allowed on the first stage.
- `pause` — set the ticket to `paused`.
- `human_review` — park the ticket in `human_review`.
- A custom status declared in top-level `statuses:` — park the ticket in that status.

### Validation rules

- Every pipeline step must reference a stage and agent that exist in the config.
- `on_success` must be `next`, `done`, `human_review`, or a custom status declared in top-level `statuses:`.
- `on_failure` must be `retry`, `back`, `pause`, `human_review`, or a custom status declared in top-level `statuses:`.
- `on_failure=back` is not allowed on the first stage.
- The last stage must not have `on_success=next` (it must terminate with `done`, `human_review`, or a custom status).
- A stage cannot appear more than once in the same pipeline.

## projects

Map of project name to a repository and the defaults tickets for it should get.
Without a `projects:` block nothing changes: a ticket created for a repository
with no entry still gets no pipeline and no agent.

```yaml
projects:
  kontora:
    path: ~/projects/kontora
    pipeline: implement-review-commit
    agent: claude
  sigil:
    path: ~/projects/sigil
    pipeline: default
```

| Field | Required | Description |
|-------|----------|-------------|
| `path` | yes | Repository the entry applies to. |
| `pipeline` | no | Pipeline written into new tickets for this repository. |
| `agent` | no | Agent written into new tickets for this repository. |
| `branch_prefix` | no | Overrides the top-level `branch_prefix` for this repository. |
| `branch_naming` | no | Overrides the top-level [`branch_naming`](#branch-naming) mode for this repository. |

`pipeline` and `agent` are stamped into the ticket when it is created. The
daemon reads `branch_prefix` and `branch_naming` when it names an empty branch,
so the current config applies until pickup. A ticket that already carries a
`branch` keeps it.

The pipeline and agent defaults are applied when a ticket is created (`kontora new`, `POST /api/tickets`, the TUI and web create forms) or initialized (`kontora init`, `POST /api/tickets/{id}/init`), and are written into the ticket's frontmatter. The ticket file keeps saying exactly what will run, and an existing ticket is never rewritten.

A value you supply yourself wins over the project default. On `kontora init` a value the ticket file already declares wins too: the project fills a blank field, it never replaces a pipeline or an agent the ticket chose. The two fields are independent: naming a pipeline still lets the agent come from the project.

`path` is matched after `~` expansion and cleaning, so `~/projects/kontora`, `/home/you/projects/kontora`, and a trailing slash all reach the same entry. Only the complete path matches. A ticket pointing at `~/projects/kontora/internal` inherits nothing, and symlinks are not resolved. A relative path is not resolved either, because only the daemon host knows what it would be relative to: `kontora new --path . "..."` matches no project. Without `--path`, `kontora new` fills in the current git root, which is absolute and does match.

### Opting out with `none`

Once a project sets a default pipeline, leaving `--pipeline` blank no longer produces a standalone ticket. Pass the literal `none` instead:

```bash
kontora new --path ~/projects/kontora --pipeline none "one agent run, no stages"
```

`none` means "leave this field blank and skip the project default". It is accepted wherever a command takes a pipeline or agent name: the `kontora new`, `kontora update`, and remote `kontora init` flags, the `pipeline` and `agent` fields of `POST /api/tickets`, `POST /api/tickets/{id}/init`, and `PUT /api/tickets/{id}`, the TUI create form, and the web selects. The literal string never reaches the frontmatter. Because it is reserved, a pipeline or an agent named `none` fails config validation.

Only those commands read the sentinel. `pipeline: none` written by hand into a ticket that is already initialized stays there, and the daemon pauses the ticket with `unknown pipeline "none"`. Leave the field out instead.

### Validation rules

- `path` is required.
- `pipeline` and `agent`, when set, must exist in `pipelines:` and `agents:`.
- Two projects may not have paths that expand and clean to the same directory, which would make the lookup pick one at random.
- No pipeline and no agent may be named `none`.

## Reloading the config

The running daemon applies most config edits without a restart, so you can
change a prompt or an agent's arguments while agents are working.

### What reloads live

`agents`, `stages`, `pipelines`, `projects`, `statuses`, `environment`,
`auto_pick_up`, `default_agent`, `branch_prefix`, `branch_naming`, and the whole
`plannotator` block.

Editing `projects` reloads live, but the pipeline and agent defaults are stamped
into a ticket when it is created or initialized. A reload changes what the next
ticket gets, never what an existing one already carries. A project's
`branch_prefix` and `branch_naming` are not stamped, so a reload changes how the
daemon names an existing ticket whose branch is still empty.

### What needs a restart

`tickets_dir`, `worktrees_dir`, `logs_dir`, `instance_name`, `tmux_session`,
`max_concurrent_agents`, and the whole `web` block. The daemon reads these once
at startup: the watched directory, the worktree root, the claim name, the tmux
session, the semaphore size, and the HTTP listener are all fixed by then. A
reload keeps the running value and logs one warning per field that differs on
disk, naming the field, the running value, and the value it ignored. The
`web.token` value is never logged.

To rename `tmux_session`, stop the daemon, run `tmux kill-session -t =<old-name>`,
then start it with the new name. The old session is not renamed for you, and it
must not be left running: its windows are invisible to the new daemon, so crash
recovery re-queues a ticket whose first agent is still alive and starts a second
agent on the same worktree and branch.

`editor` is read by the CLI, not the daemon, so it takes effect on the next
command either way.

If you started the daemon with `--address` or `--port`, those keep winning over
the file after a reload, the same as at startup. Editing `web.host` or
`web.port` on disk then changes nothing and logs no warning: the flag already
overrides it, and a restart with the same flag would too.

### How to trigger a reload

- Send `SIGHUP`: `kill -HUP $(pgrep -f 'kontora start')`.
- Edit the config file. The daemon watches it and reloads after the debounce
  interval, whether the editor writes in place or replaces the file with an
  atomic rename. If the config path is a symlink, the daemon watches the
  symlink's directory and the target's directory, so an edit through a dotfiles
  symlink also reloads. Other files in the same directory (the daemon lock file,
  the `.kontora-config-*.tmp` files an atomic save creates and renames away)
  trigger nothing.
- Save through `kontora config edit --url ...`. The daemon writes the file and
  reloads it before the request returns.

### Rules

A reload is all-or-nothing. The daemon parses and validates the whole file
before applying any of it: if anything fails, it logs the error and keeps
running on the old config. Saving a half-written file mid-edit is harmless, and
the next complete save reloads.

**A running agent keeps the settings it started with.** The prompt, arguments,
timeout, and binary are fixed when the stage spawns. Editing a prompt while a
ticket is mid-stage does nothing visible until the next stage starts. That is
expected, not a failed reload.

If a reload removes the pipeline or the stage a `todo` ticket sits on, the
daemon pauses that ticket and writes the reason to `last_error` rather than
leaving it stuck with no explanation.

## Shell completions

```bash
# Fish - activate in current session
kontora completion fish | source

# Fish - persist across sessions
kontora completion fish > ~/.config/fish/completions/kontora.fish
```
