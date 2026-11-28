# Configuration

One YAML file. Unknown fields are rejected, so a typo fails to load rather than
being ignored. `kontora config` prints the effective configuration with
defaults applied and `web.token` masked; in remote mode it prints the daemon's
pipelines, agents and custom statuses.

`--config` selects the file explicitly. Otherwise Kontora uses `$KONTORA_CONFIG`,
then an existing `./.kontora/config.yaml`, then `$XDG_CONFIG_HOME/kontora/config.yaml`
(or `~/.config/kontora/config.yaml` when `XDG_CONFIG_HOME` is unset).
Kontora reads one file, without merging.

This topic names every top-level field. Nested blocks, such as `hooks` and
`web`, have their own sections.
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
| `secondary_tickets_dirs` | none | Extra ticket folders the dashboard shows beside the primary board. See Secondary ticket folders. |
| `author` | the OS user | Name a person's notes are signed with. An agent's notes use `$KONTORA_AGENT`. |
| `editor` | `$EDITOR`, then `vi` | Editor for `kontora edit` and `kontora config edit`. |
| `pager` | none | Pager for `view`, `logs` and `activity` on a terminal. `$KONTORA_PAGER`, `$TICKET_PAGER` and `$PAGER` win over it. |
| `system_prompt`, `resume_prompt`, `annotation_prompt` | built-in | Prompt overrides. See Prompts and summaries. |
| `summary_model`, `summary_effort` | none | Model and effort of the ticket summary pass. See Prompts and summaries. |
| `hooks` | none | Shell commands at worktree and stage events. See hooks. |
| `automations` | none | Scheduled agent runs without tickets. See automations. |
| `agents`, `stages`, `pipelines`, `projects` | none | See their sections. |
| `notifications`, `web`, `assistant`, `plannotator`, `metrics` | see section | See their sections. |

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
See `kontora skills show config Compatibility` before activating epics.

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
| `failure_patterns` | Regexes checked against agent output when the run finishes. Raw-only tmux agents scan the full attempt independently of the display-log limit. A match pauses the ticket even on a clean exit. `[]` disables pattern matching, but structured Claude and Pi session checks remain active. |
| `resume` | `false` makes every stage start a new conversation, even after a restart interrupted one. |
| `checkpoint_compaction_tokens` | Positive turns on phase-boundary compaction for `pi` and `claude`. |

An agent name is what a pipeline step, a ticket's `agent` field and
`assistant.agent` all refer to.

Kontora resolves `binary` as an executable and passes `args` literally.
Shell functions and aliases do not apply. Configure wrappers explicitly in
`binary` and `args`.

## stages and pipelines

`stages:` maps a stage name to its prompt template, timeout, model and effort.
`pipelines:` maps each name to either a legacy ordered step list or a version 2
workflow graph. Legacy steps name a stage, agent and success/failure policies.
A legacy step's `max_retries` defaults to `0` when omitted.
With `on_failure: retry` and no retry budget, the first failure pauses the ticket.

Graphs declare `version: 2`, `entry`, `nodes`, and `edges`. Each edge uses
`from: {node: NODE_ID, port: PORT}` and `to: NODE_ID`. Node types are `agent`,
`check`, `condition`, `loop`, `wait`, and `end`. Execution is sequential and follows
edges, never the YAML node list order. Distinct agent nodes can share one stage
template. A condition compares the latest executable verdict or check exit code;
a loop's positive `max` bounds its repeat transitions for the whole execution.
Every port needs one destination; every cycle must cross `loop.repeat` and
include an executable. Graph aliases, merge keys and unknown fields are refused.
See `kontora skills show pipelines 'Workflow graphs'` for complete YAML and
node fields. Do not use a legacy stage-list editor to author graphs.

Graph agent nodes require version 2 invocation-bound results, or version 3 with
`wake`, including a declared verdict on completion when configured. The ticket's `stage` is a node ID; daemon-owned
`graph_state` holds the execution, revision, fingerprint, visits and loop counts.
Changed effective definitions pause rather than silently changing live traversal.
`retry` preserves executable identity and loop counts and is refused at `end`.
`skip` and `set-stage` are refused for graphs.

`kontora workflow-restart ID` creates a new execution at entry with fresh visit
and loop counters, retaining history and worktree.
`kontora workflow-recover --execution-id ID --revision N TICKET_ID` acknowledges
termination for inspected state. Inspect processes, journals, results and
worktree first; recovery does not start work. Both controls require the daemon.
Graph annotations and Plannotator rework are deferred and refused.

See `kontora skills show config Compatibility` before enabling graphs.

## Durable waits

Agent nodes in version 2 graphs opt in with `wake: {max: 12, check_timeout: 60s}`.
`wake.max` is required and positive; it bounds wakes within one executable visit, not polls.
Omitted or zero `check_timeout` uses `60s`; negative values are invalid.

Declared nodes use `type: wait`, `mode: human` or `deployment`, and optional `timeout`.
Deployment requires `environment`; human waits cannot set it.
Every wait needs a `released` edge. A positive timeout also requires a `timeout` edge;
omitted or zero timeout has no deadline or timeout edge. Negative timeouts are invalid.
Durations use Go units: `168h`, not `7d`.

Use `{{ with .Wake }}` … `{{ end }}` to read `.Reason`, `.Output`, `.Count`, `.Next`, and `.WaitID`.
`.Receipt` is optional and is absent on timer and probe wakes. Guard receipt fields separately
inside the `.Wake` block: `{{ with .Receipt }}{{ .ID }}{{ end }}`.
Kontora appends the wake handoff automatically.
Context stays frozen for the continuation and its retries, not unrelated later nodes.
`{{ fileOptional "RESULTS.md" }}` returns empty text only for an absent file;
other read errors fail rendering, as with `file`.

See `kontora skills show pipelines 'Durable waits'` for wait behavior, recovery and storage.
For shared boards, see `kontora skills show config Compatibility`.

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
| `notify` | Optional status default before global `notifications.default_statuses`. `[]` means Off. |
| `notify_channels` | Channel override before `notifications.default`; `[none]` silences delivery. |

The name is what `--project` on `ls`, `search`, `archive` and `stats` matches.
Two projects may not resolve to the same directory. Matching is lexical after
`~` expansion and cleaning; linked worktrees and symlinks do not gain a match.

See `kontora skills show pipelines "Default selection"` for pipeline precedence and stored assignments.

A project's `agent` becomes a ticket-level override for every pipeline step.
Leave it unset to keep each step's configured agent.

Project `pipeline` and `epic_child_pipeline` must name configured pipelines; neither accepts
`none`. No pipeline or agent may be named `none`.

Browser inheritance omits `pipeline`; standalone sends `none`. The backend resolves
current defaults at submission, independently of the preview snapshot.
See `kontora skills show config Compatibility` before adding `epic_child_pipeline`.

## hooks

Hooks run shell commands at three events in a ticket's life. A typical use is
to copy a gitignored file, such as `.env`, into a fresh worktree before the
agent starts.

```yaml
hooks:
  worktree_created:
    - name: copy env file
      run: cp "$KONTORA_REPO_PATH/.env" .env
      timeout: 30s
projects:
  myrepo:
    path: ~/projects/myrepo
    hooks:
      stage_start:
        - run: make deps
          on_failure: warn
```

| Event | When it runs |
|-------|--------------|
| `worktree_created` | After the daemon creates a worktree, before the agent starts. It does not run for a worktree that a stage reuses, unless an earlier hook failure left that worktree half prepared. |
| `stage_start` | Before each stage agent starts, including a resume after a daemon restart. Make it safe to run twice. |
| `stage_end` | After each stage agent exits, before the pipeline chooses the next step. |

| Field | Default | Description |
|-------|---------|-------------|
| `run` | required | Command line for `/bin/sh -c`. It runs in the ticket's worktree. |
| `name` | `<event>[<index>]` | Label in the logs and in a recorded failure. |
| `timeout` | `5m` | The command stops after this long. |
| `on_failure` | `pause`, or `warn` for `stage_end` | `pause` pauses the ticket and skips the rest of the event's hooks. `warn` logs the failure and continues. |

Top-level hooks run first, then the hooks of the project that matches the
ticket's path. Hooks in one list run in order, one at a time. An annotation run
fires no stage hooks.

Every hook gets the daemon's environment plus `KONTORA_EVENT`,
`KONTORA_TICKET_ID`, `KONTORA_TICKET_FILE`, `KONTORA_WORKTREE`,
`KONTORA_REPO_PATH`, `KONTORA_BRANCH`, `KONTORA_STAGE`, `KONTORA_AGENT`,
`KONTORA_PROJECT` and `KONTORA_RUN_OUTCOME`. `stage_end` also gets
`KONTORA_EXIT_CODE`, the runner's exit status. An automation hook also gets
`KONTORA_AUTOMATION_ID` and `KONTORA_RUN_NUMBER`. Its ticket variables are
empty. The top-level `environment:` map does not apply to hooks. `run` is not a
template.

When a `stage_end` hook fails under `pause`, Kontora records the stage's
history and next stage as the pipeline decided, then sets the status to
`paused`. Hook output goes to
`<logs_dir>/<ticket-id>/hooks/hooks.log`, not to the stage log, so it cannot
match an agent's `failure_patterns`. A hook runs with the same trust as an
agent binary: anyone who can edit the config can run commands as you.

## automations

An automation runs an agent on a saved prompt, repository and schedule. It
creates no ticket and uses no pipeline, but it shares agent capacity with
tickets. Schedules and manual runs also work with `auto_pick_up: false`. The
CLI, the dashboard and the API all edit this block through the daemon. See
`kontora skills show cli 'kontora automation'`.

```yaml
automations:
  morning-report:
    name: Morning repository report
    prompt: |
      Inspect recent changes in {{ .Automation.Path }}.
      Write a report for run {{ .Run.Number }}.
      {{ if .LastRun.Number }}Previous result: {{ .LastRun.Result }}{{ end }}
    path: ~/projects/app
    agent: claude
    schedule: "0 9 * * 1-5"
    timezone: Europe/Berlin
    workspace: worktree
    overlap: skip
    timeout: 30m
    enabled: false
```

The key is lowercase kebab-case. Kontora also assigns an ID: `aut-` plus 32
lowercase hexadecimal characters. `name`, `prompt`, `path` and `schedule` are
required. `agent` defaults to `default_agent`. `enabled` defaults to `false`, so
a new definition does not run on its schedule until you enable it.

`schedule` is numeric five-field cron, such as `0 9 * * 1-5`, or a whole-minute
interval, such as `90m` or `2h`. Seconds, named fields and descriptors such as
`@daily` are invalid. `timezone` takes an IANA name and defaults to `UTC`;
`Local` is invalid. A slot that the daemon reaches more than one minute late is
recorded as skipped, and missed slots are not replayed. A run accepted at its
slot can still wait for agent capacity.

| `overlap` | What happens to a new request while a run is unfinished |
|-----------|---------------------------------------------------------|
| `skip` (default) | It is recorded as skipped. |
| `queue` | It waits as the one pending run. Further requests are skipped. |
| `replace` | The current run stops, and the newest request runs. |

`workspace: worktree` (default) creates a fresh worktree on branch
`auto/<key>-<n>`. Kontora removes it after the run if it has no commits or
changes. `workspace: repo` runs in the repository checkout, and the changes stay
there.

The prompt is a Go template with three contexts:

| Context | Fields |
|---------|--------|
| `.Automation` | `ID`, `Key`, `Name`, `Path`, `Agent`, `Model`, `Schedule`, `Timezone`, `Workspace`, `Overlap`, `Timeout`, `Enabled` |
| `.Run` | `Number`, `InvocationID`, `Branch`, `Date`, `Trigger`, `ScheduledFor`, `AcceptedAt`, `StartedAt`, `CompletedAt`, `Outcome`, `Result`, `ExitCode`, `Cost` |
| `.LastRun` | The same fields for the previous executed run. Zero values before the first run. |

`{{ file "NAME" }}` and `{{ fileOptional "NAME" }}` read from the run's working
directory. Ticket fields are not available.

The agent must publish a result with
`kontora result --input-file /absolute/path/result.json AUTOMATION_ID`. A zero
exit without a result is a failed run. So are a `blocked` result, a nonzero
exit and a timeout. A prompt does not limit what the agent can do, so configure
the agent's own permissions for unattended runs.

## notifications

Status defaults and delivery routes are separate:

```yaml
notifications:
  default_statuses: [paused, human_review, waiting] # Optional; initially unset.
```

Status selection uses the first applicable list, without merging:

1. Explicit request.
2. Saved ticket `notify`.
3. Project `notify`.
4. Global `notifications.default_statuses`.
5. No subscription.

An empty status list means persistent Off. Missing, null, or blank ticket statuses
are unset. Project and global defaults accept known statuses and `waiting`, never
`none`, even as a custom status. Config export preserves missing versus empty lists.

Defaults apply at ordinary creation and explicit initialization/starts, including
unset older/imported tickets. Starts include run, retry, move to `todo`, advancing
skip, workflow restart, and reviewer-requested rework or annotation runs.
Initialization uses the final path. Epic creation skips defaults; activation
applies them only to the supervisor, not its children.
Startup, imports, reloads, automatic pickup, stage progression, and wakes do not add subscriptions.
Saved choices survive default changes.

Settings edits global statuses with shared presets or custom lists and reloads live.
Start dialogs preselect saved choices or effective defaults. Untouched forms follow
path/config changes and omit `notify`; saved or edited choices submit explicit arrays.
Start dialogs require loaded config and a route, explicit silence, or Off.
Channel-only edits keep unset statuses unset.
There is no dashboard reset-to-default action. Remove stored `notify` manually to restore default eligibility.

`notify: []` stays Off across starts and returns `[]` through the ticket API.
See `kontora skills show config Compatibility` before rolling out notification defaults or persistent Off
on a shared board, or to restore a deleted Off choice.

Routes still use ticket `notify_channels`, project `notify_channels`, then
`notifications.default`. `notify_channels: []` removes an override; `[none]` silences it.
`enabled` controls delivery; `all_changes` includes command/API/dashboard/file status changes.
Neither switch changes status defaults. Defaults validate without channels, but delivery needs a route.
`waiting` covers only live Pi questions, not Claude questions or durable workflow wakes.
Channels are in `kontora skills show config 'Notification channels'`.

## Notification channels

A channel is a named delivery target under `notifications.channels`. Ticket and
project `notify_channels` and `notifications.default` refer to these names.

```yaml
notifications:
  channels:
    tg:
      type: telegram
      secret_env: KONTORA_TELEGRAM_TOKEN
      chat_id: "123456789"
    mm:
      type: mattermost
      secret_file: ~/.config/kontora/mattermost-webhook
    ops:
      type: webhook
      url: https://ops.example.com/kontora
      headers:
        X-Team: platform
  default: [tg]
```

| Field | Applies to | Description |
|-------|-----------|-------------|
| `type` | all | `telegram`, `mattermost` or `webhook`. |
| `secret_env` | all | Environment variable that holds the credential. |
| `secret_file` | all | File that holds the credential. Set at most one of `secret_env` and `secret_file`. |
| `chat_id` | telegram | Chat the bot posts to. Required. |
| `channel` | mattermost | Overrides the channel of the incoming webhook. |
| `url` | webhook | `http` or `https` endpoint. Required. |
| `method` | webhook | `POST` (default), `PUT` or `PATCH`. |
| `headers` | webhook | Extra request headers. |

The credential is the bot token for `telegram`, the whole incoming-webhook URL
for `mattermost`, and an optional bearer token for `webhook`. The config cannot
hold a credential itself: `secret:` is rejected at load. `kontora doctor`
checks that each channel's credential resolves, without printing it.

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | `false` builds no channels. |
| `all_changes` | `false` | Also notify for status changes made by commands, the API, the dashboard and file edits. |
| `default` | `[]` | Channels used when neither the ticket nor its project names one. |
| `timeout` | `10s` | Limit for one delivery attempt. |
| `attempts` | `3` | Attempts per notification, from 1 to 10. |
| `backoff` | `1s` | Wait before the second attempt, doubled after each further attempt. |

A transport error, a `429` or a `5xx` is retried. Other `4xx` responses are
not. Delivery is best effort: Kontora persists nothing and replays nothing, so a
notification can be lost.

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
polling in a legacy custom-active step. For daemon-managed probes without an idle
agent, use graph durable waits instead.

Active runs keep their captured status after reload. Removed declarations leave
only display-only fallback columns; they do not authorize new move destinations.
See `kontora skills show config Compatibility` before enabling `running_status`.

## Prompts and summaries

| Field | Default | Description |
|-------|---------|-------------|
| `system_prompt` | built-in brief | Added to the system prompt of every `claude` and `pi` stage agent. It replaces the built-in brief and is not a template. |
| `resume_prompt` | built-in | Sent instead of the stage prompt when a daemon restart interrupted the stage and the agent resumes its conversation. Same template fields as a stage prompt. |
| `annotation_prompt` | built-in | Prompt of the run that rewrites a ticket from Plannotator annotations. Same template fields as a stage prompt. |
| `summary_model` | none | Model of the pass that writes `final_summary`, resolved for the agent that ran the last stage. One value or a per-agent map, as for a stage `model`. |
| `summary_effort` | none | Reasoning effort of the same pass, in the same two forms. |

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

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | Start the dashboard and the HTTP API with the daemon. |
| `host` | `127.0.0.1` | Bind address. |
| `port` | `8080` | Listen port. |
| `token` | `""` | Shared bearer token for `/api/*` and `/ws/*`. Empty leaves the API open. |
| `allowed_hosts` | `[]` | Extra `Host` header values the server answers. |
| `theme` | `system` | Dashboard theme: `system`, `light` or `dark`. |

Local CLI commands that need the daemon, such as `run`, `changes` and
`activity`, call this server at `host` and `port`. With `enabled: false` they
cannot reach the daemon.

## plannotator

```yaml
plannotator:
  binary: plannotator
  timeout: 30m
  reviews_dir: ~/.kontora/plannotator-reviews
```

`kontora review` opens the branch diff of a `human_review` ticket in
Plannotator. `kontora annotate` opens the markdown of an `open` ticket. The
daemon resolves `binary` on its own `PATH`. `timeout` is how long a session
can stay open before Kontora cancels it. Submitted review feedback moves the
ticket to the built-in
`rework` stage, whose prompt reads it with `{{ plannotatorReview }}`. Submitted
annotations queue one run with `annotation_prompt`, which reads them with
`{{ plannotatorAnnotations }}`. Graph pipelines refuse both.

## metrics

```yaml
metrics:
  enabled: true
  endpoint: localhost:4318
  insecure: true
  interval: 60s
```

Metrics are off by default. When enabled, the daemon pushes OTLP/HTTP metrics
to `endpoint`: stage runs and durations, pipeline transitions, agent failures,
token spend and scheduler state. There is no `/metrics` scrape route. An empty
`endpoint` uses `OTEL_EXPORTER_OTLP_ENDPOINT`. With a bare `host:port`,
`insecure: true` sends plain HTTP. A scheme in `endpoint` decides the transport
by itself. `headers` adds request headers for a collector that needs them. A
collector that is down never blocks ticket work.

## Secondary ticket folders

```yaml
tickets_dir: ~/.kontora/tickets
secondary_tickets_dirs:
  - ~/shared/team-tickets
```

`tickets_dir` is the primary board. The dashboard shows the tickets of each
secondary folder beside it, labelled with their folder. The daemon never runs,
recovers, schedules or derives epic statuses in a secondary folder. Agent
counts use only `tickets_dir`. A primary dependency whose ID is not in the
primary folder can resolve to a unique ticket in a secondary folder. Discovery
is not recursive. A missing folder shows as unavailable and does not stop the
primary board.

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
Notification defaults affect later ordinary creation and explicit initialization/starts,
not saved choices or automatic lifecycle transitions.
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

## Compatibility

- Epic defaults: Upgrade all clients and daemon binaries that read the config before adding
  `epic_child_pipeline`. Older strict-YAML loaders reject the key.
- Epic activation: See `kontora skills show tickets Compatibility` for mixed versions.
- Workflow graphs and durable waits: See `kontora skills show pipelines Compatibility`.
- Custom `running_status`: Upgrade every CLI and daemon sharing the store before enabling the setting.
  Older config loaders reject the key. Older clients treat custom-active tickets as parked work.
  Existing parked tickets are not activated or migrated.
- Notification defaults and Off: Upgrade all participating clients and daemons.
  Older writers can delete the empty-list marker. Older loaders reject `default_statuses`.
  Tickets whose Off choice was deleted inherit at their next explicit start unless the user selects Off again.
