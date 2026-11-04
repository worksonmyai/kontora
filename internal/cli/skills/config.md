# Configuration

One YAML file. Unknown fields are rejected, so a typo fails to load rather than
being ignored. `kontora config` prints the effective configuration with
defaults applied and `web.token` masked; in remote mode it prints the daemon's
pipelines, agents and custom statuses.

The file is found at `$KONTORA_CONFIG`, then `./.kontora/config.yaml`, then
`$XDG_CONFIG_HOME/kontora/config.yaml`, then `~/.config/kontora/config.yaml`.

Only the fields worth knowing when answering a question about a board are here.
Read `kontora config` for what a particular board actually runs.

## Top-level fields

| Field | Default | Description |
|-------|---------|-------------|
| `tickets_dir` | `~/.kontora/tickets` | Where the ticket markdown files live. |
| `worktrees_dir` | `~/.kontora/worktrees` | Where git worktrees are created. |
| `logs_dir` | `~/.kontora/logs` | Where agent logs and assistant chats are stored. |
| `branch_prefix` | `kontora` | Prefix of a ticket's work branch. A project can override it. |
| `branch_naming` | `mode: slug` | How a ticket with an empty `branch` is named. `off` uses `<prefix>/<id>`. |
| `default_agent` | inferred | Agent for tickets with no pipeline. Defaults to `claude` when an agent has that name, otherwise the only agent. |
| `max_concurrent_agents` | `3` | How many stage runs happen at once. |
| `auto_pick_up` | `true` | Whether the daemon claims new `todo` tickets on its own. |
| `instance_name` | hostname | Identifies this daemon when several share a synced `tickets_dir`. Written to `claimed_by`. |
| `tmux_session` | `kontora` | The tmux session agent windows go in. |
| `statuses` | — | Extra parked statuses beyond the built-ins. |
| `environment` | — | Environment variables set for every agent process. |

For `tickets_dir` alone the environment beats the file: `--tickets-dir`, then
`$KONTORA_TICKETS_DIR`, then `$TICKETS_DIR`, then the config. Every other
setting is the other way round.

All paths take `~`, expanded at runtime rather than at load.

## agents

Map of agent name to the binary that runs it. Any CLI that takes a prompt can
be an agent; the rendered prompt is appended as the last argument.

```yaml
agents:
  claude:
    binary: claude
    args: ["--dangerously-skip-permissions", "--model", "sonnet"]
```

| Field | Description |
|-------|-------------|
| `binary` | Executable name or path. Required. |
| `args` | Arguments before the prompt. |
| `environment` | Variables for this agent only, merged over the top-level map. |
| `effort` | Reasoning effort every invocation starts from. Only `claude` and `pi` take a flag for it. |
| `failure_patterns` | Regexes matched against the output log after the agent exits. A match pauses the ticket even on a clean exit. `[]` disables pattern matching, but structured Claude and Pi session checks remain active. |
| `resume` | `false` makes every stage start a new conversation, even after a restart interrupted one. |
| `checkpoint_compaction_tokens` | Positive turns on phase-boundary compaction for `pi` and `claude`. |

An agent name is what a pipeline step, a ticket's `agent` field and
`assistant.agent` all refer to.

## stages and pipelines

`stages:` maps a stage name to its prompt template, timeout, model and effort.
`pipelines:` maps a pipeline name to an ordered list of steps, each naming a
stage, an agent and its success and failure policies. See the `pipelines`
topic.

## projects

Map of project name to a repository and the defaults its tickets get.

```yaml
projects:
  kontora:
    path: ~/projects/kontora
    pipeline: default
    agent: claude
```

| Field | Description |
|-------|-------------|
| `path` | Repository directory. Required. |
| `pipeline` | Default pipeline for tickets created for this path. |
| `agent` | Default agent for the same. |
| `prefix` | Pin the ticket id prefix instead of deriving it from the directory name. |
| `branch_prefix`, `branch_naming` | Override the top-level branch settings. |
| `hooks` | Lifecycle hooks scoped to this project. |

The name is what `--project` on `ls`, `search`, `archive` and `stats` matches.
Two projects may not resolve to the same directory.

Once a project sets a default pipeline, leaving `--pipeline` blank no longer
makes a standalone ticket. Pass the literal `none` for that. A pipeline or an
agent named `none` fails validation, because the word is reserved.

## statuses

```yaml
statuses: [needs_qa, blocked_on_design]
```

Extra statuses a ticket can be parked in, beyond `open`, `todo`,
`in_progress`, `paused`, `human_review`, `done`, `cancelled` and `archived`.
A pipeline step's `on_success` or `on_failure` can name one, and
`kontora move ID STATUS` can put a ticket there. They are parked states: the
daemon never runs a ticket out of one on its own.

## web

```yaml
web:
  enabled: true
  host: 127.0.0.1
  port: 8080
  token: ""
  allowed_hosts: []
```

The dashboard and the HTTP API the client commands use in remote mode.
`KONTORA_WEB_TOKEN` overrides `token` on the daemon side.

## assistant

The dashboard's assistant pane supports ordinary chats and the optional Kotik
companion inbox.

```yaml
assistant:
  agent: claude
  model: sonnet
  effort: medium
  workdir: ~/.kontora/tickets
  timeout: 10m
  autonomy: ask
  prompt: ""
  companion:
    enabled: false
    name: Kotik
```

| Field | Default | Description |
|-------|---------|-------------|
| `agent` | none | Which `agents:` entry runs model turns. Empty disables chat, not an enabled companion inbox. Its binary must be `claude` or `pi`. |
| `model`, `effort` | the agent's own | Applied the way a stage's are. |
| `workdir` | `tickets_dir` | The directory every turn runs in. Fixed when a chat opens. |
| `timeout` | `10m` | Bounds one turn. |
| `autonomy` | `ask` | The mode a new chat starts in: `read`, `ask` or `auto`. |
| `prompt` | built-in | Replaces the ordinary chat brief, not the companion brief. |
| `companion.enabled` | `false` | Enable ticket observation without requiring a model or open browser. |
| `companion.name` | `Kotik` | At most 64 Unicode characters; no blank names or control characters. An empty value uses `Kotik`. |

Ordinary chat autonomy is enforced at the agent's tool boundary, not by asking
the agent to behave. A tool call with no rule counts as a change, so a mode can
only ever be stricter than expected.

| Mode | Reads | Changes | Deleting a ticket |
|------|-------|---------|-------------------|
| `read` | run | refused | refused |
| `ask` | run | held for approval | held |
| `auto` | run | run | held |

### Companion

Set `assistant.companion.enabled: true` to enable Kotik. Kontora validates the
name even without an assistant agent or when observation is disabled. The daemon
owns one shared inbox and read state across devices. It tracks questions, paused tickets,
human review, recent completions and changes since an explicit caught-up
checkpoint. Reading attention does not answer questions or change tickets.

The inbox works without `assistant.agent`; only AI actions need that setting.
Startup, polling, reload and browser reconnection never start model turns.
Briefings, investigations and follow-ups require an explicit user request.
Each request accepts at most five selected tickets and 32 KiB of server-built
evidence. Kontora saves evidence before model admission. Capture metadata and
omissions remain visible when the chat reopens. Source changes mark explanations
outdated instead of changing historical answers or current attention.

Companion chats deny every tool call and autonomy escalation, regardless of
`assistant.autonomy` or `assistant.prompt`. They cannot run commands, inspect
arbitrary files, mutate tickets, answer agents, execute approvals or send
outbound messages. Existing notifications remain separate. Kontora enforces
these restrictions through runtime hooks with a trusted configured agent.
This is not an OS sandbox against malicious binaries or installed extensions.

Attention lives at `<logs_dir>/companion/<daemon-scope>/state.json`, scoped by
normalized daemon-lock and ticket-directory paths. The inbox retains all active
items plus at most 500 inactive records, with inactive records kept at most
30 days. A corrupt, unsupported or unavailable store reports degraded health
and preserves the existing file. Kontora refuses read-state updates it cannot
save; task execution and notifications continue.

Polling can miss short-lived transitions, and offline history is incomplete.
A question that disappears is no longer observed, not proven answered.

## Reloading

The daemon watches its config file, and `SIGHUP` reloads it too. `agents`,
`stages`, `pipelines`, `projects`, `statuses`, `environment`, `auto_pick_up`,
`default_agent`, the branch settings and the `assistant` block all reload live
and apply to the next run, never to one already going. Disabling the companion
stops observation and blocks new companion requests without deleting stored
data. An already-running requested turn remains stoppable.

`tickets_dir`, `worktrees_dir`, `logs_dir`, `instance_name`, `tmux_session`,
`max_concurrent_agents` and the whole `web` and `metrics` blocks need a
restart: a reload keeps the running value and logs a warning naming the one it
ignored. A file that fails to load leaves the running configuration in place.
