# Configuration

One YAML file. Unknown fields are rejected, so a typo fails to load rather than
being ignored. `kontora config` prints the effective configuration with
defaults applied and `web.token` masked; in remote mode it prints the daemon's
pipelines, agents and custom statuses.

Upgrade all clients and daemon binaries that read the config before adding
`epic_child_pipeline`. Older strict-YAML loaders reject the key.

`--config` selects the file explicitly. Otherwise Kontora uses `$KONTORA_CONFIG`,
then an existing `./.kontora/config.yaml`, then `$XDG_CONFIG_HOME/kontora/config.yaml`
(or `~/.config/kontora/config.yaml` when `XDG_CONFIG_HOME` is unset).
Kontora reads one file, without merging.

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
| `default_pipeline` | — | Pipeline fallback after applicable project defaults. Must name a configured pipeline. |
| `max_concurrent_agents` | `3` | Maximum worker runs at once, excluding epic supervisors. Zero or omission uses `3`; negative values are rejected. Reloads live without stopping running workers. |
| `max_concurrent_epic_supervisors` | `4` | Maximum epic supervisors in a separate pool. Zero or omission uses `4`; negative values are rejected. Reloads live without stopping running supervisors. |
| `auto_pick_up` | `true` | Automatically claim eligible `todo` tickets. Reloads live. Disabling it does not stop explicit runs, pipeline continuations, recovery, or scheduled automations. |
| `instance_name` | hostname | Identifies this daemon when several share a synced `tickets_dir`. Written to `claimed_by`. |
| `tmux_session` | `kontora` | The tmux session agent windows go in. |
| `statuses` | — | Extra statuses for parking or a step's `running_status`. |
| `environment` | — | Environment variables set for every agent process. |

For `tickets_dir` alone the environment beats the file: `--tickets-dir`, then
`$KONTORA_TICKETS_DIR`, then `$TICKETS_DIR`, then the config. Every other
setting is the other way round.

All paths take `~`, expanded at runtime rather than at load.

Epic supervisors can add four agent processes beyond the default three workers.
A supervisor waiting for children does not occupy a worker slot. The queue
chooses the oldest eligible ticket whose pool has capacity. Unconfirmed
termination retains the reservation in the pool that started the process.
Creation leaves epics passive. Activate explicitly with `kontora run EPIC_ID`
or init with `--status todo`. The epic and children must share one Git repository.
Upgrade every daemon and client sharing the ticket directory before activation;
older versions can overwrite supervisor status. See `kontora skills show tickets Epics`.

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

Kontora resolves `binary` as an executable and passes `args` literally.
Shell functions and aliases do not apply. Configure wrappers explicitly in
`binary` and `args`.

## stages and pipelines

`stages:` maps a stage name to its prompt template, timeout, model and effort.
`pipelines:` maps a pipeline name to an ordered list of steps, each naming a
stage, an agent and its success and failure policies. See the `pipelines`
topic. A step's `max_retries` defaults to `0` when omitted.
With `on_failure: retry` and no retry budget, the first failure pauses the ticket.

## projects

Map of project name to a repository and the defaults its tickets get.

```yaml
projects:
  kontora:
    path: ~/projects/kontora
    pipeline: default
    epic_child_pipeline: default
    agent: claude
```

| Field | Description |
|-------|-------------|
| `path` | Repository directory. Required. |
| `pipeline` | Ordinary pipeline default at creation or initialization for this path. |
| `epic_child_pipeline` | Pipeline default for ordinary children of any actual epic, passive or agent. Falls back to `pipeline` when unset. |
| `agent` | Default agent for the same. |
| `prefix` | Pin the ticket id prefix instead of deriving it from the directory name. |
| `branch_prefix`, `branch_naming` | Override the top-level branch settings. |
| `hooks` | Lifecycle hooks scoped to this project. |

The name is what `--project` on `ls`, `search`, `archive` and `stats` matches.
Two projects may not resolve to the same directory. Matching is lexical after
`~` expansion and cleaning; linked worktrees and symlinks do not gain a match.

For ordinary tickets, pipeline precedence is:

1. Explicit request, including `none` for standalone execution.
2. Stored nonempty pipeline, during initialization only.
3. Project `epic_child_pipeline`, when the resolved parent is an actual epic.
4. Project `pipeline`.
5. Global `default_pipeline`.
6. Standalone execution.

Missing or ordinary-ticket parents do not qualify for the child default. Epics
never get a pipeline; supervisors run standalone. Pipeline and agent resolution
are independent, including when the caller passes `--pipeline none`.

A project's `agent` becomes a ticket-level override for every pipeline step.
Leave it unset to keep each step's configured agent.

Kontora stamps defaults into frontmatter at creation or initialization. Existing
assignments are not migrated. Config reloads, reparenting and detail-path edits do
not replace stored pipelines; operators must retarget existing tickets explicitly.
Raw uploads and daemon startup do not apply defaults.

`none` is a ticket-level opt-out for this request, not durable provenance. A later
initialization can fill the blank unless the caller opts out again. Project
`pipeline` and `epic_child_pipeline` must name configured pipelines; neither accepts
`none`. No pipeline or agent may be named `none`.

Browser inheritance omits `pipeline`; standalone sends `none`. The backend resolves
current defaults at submission, independently of the preview snapshot.

## statuses

```yaml
statuses: [needs_qa, blocked_on_design]
```

Extra statuses a ticket can be parked in, beyond `open`, `todo`,
`in_progress`, `paused`, `human_review`, `done`, `cancelled` and `archived`.
A pipeline step's `on_success` or `on_failure` can name one, and
`kontora move ID STATUS` can put a ticket there. These actions park work and
never start an agent. A step can instead set `running_status` to a declared
custom name to run its agent in that column. Omission means `in_progress`;
other built-ins, legacy `closed` and reserved policy names are invalid.

The daemon claims from `todo`, then writes the actual custom status and a ticket
`running_status` snapshot together. An ordinary status assignment clears the
snapshot, including same-name parking. The active agent keeps its slot and branch
reservation. `timeout: 0s` disables only Kontora's stage timeout. The agent owns
polling; there is no daemon-managed readiness check.

Active runs keep their captured status after reload. Removed declarations leave
only display-only fallback columns; they do not authorize new move destinations.
Upgrade all CLI and daemon binaries sharing the store before enabling the setting.
Older config loaders reject the new key and older clients treat custom activity
as parked work. Existing parked tickets are not activated or migrated.

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
`stages`, `pipelines`, `projects`, `automations`, `statuses`, `environment`, `hooks`,
`auto_pick_up`, `default_agent`, `default_pipeline`, the branch settings,
`author`, `system_prompt`, `resume_prompt`, `annotation_prompt`, `summary_model`,
`summary_effort`, `notifications`, and `assistant` all reload live.
The `plannotator` block reloads live except for `plannotator.reviews_dir`.
The CLI reads `editor` and `pager` on the next command.
Run settings apply to the next run, never to one already going.
Pipeline defaults affect later creation and initialization, not existing assignments.
Initialization preserves stored nonempty pipelines.
An invalid child-pipeline reference rejects the reload and keeps the running config.

`max_concurrent_agents` and `max_concurrent_epic_supervisors` reload live.
Raising a limit lets queued work start. Lowering a limit does not stop running
agents; new work waits until usage falls below the limit.

Enabling `auto_pick_up` queues existing eligible `todo` tickets. Disabling it
stops automatic starts, including tickets already queued. Explicit runs,
pipeline continuations, recovery, and scheduled automations remain possible.

Disabling the companion stops observation and blocks new companion requests without deleting stored
data. An already-running requested turn remains stoppable.

These need a restart: `tickets_dir`, `secondary_tickets_dirs`, `worktrees_dir`,
`logs_dir`, `instance_name`, `tmux_session`, `plannotator.reviews_dir`, and the whole
`web` and `metrics` blocks. A reload keeps the running value and logs a warning
naming each ignored field. A file that fails to load leaves the running configuration in place.
