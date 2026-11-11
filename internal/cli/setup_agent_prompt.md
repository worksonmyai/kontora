# Kontora setup brief

You are a coding agent configuring Kontora for the user on this machine.

Kontora is an agent orchestration daemon. It watches a directory of markdown
tickets, creates a git worktree and a tmux window per ticket, and runs coding
agent CLIs through pipeline stages.

The installed `kontora` binary printed this brief, so it describes the config
schema that binary accepts. Do not follow a schema you remember from elsewhere.

## Current state

- Config path: [[ .ConfigPath ]]
- State: [[ .State ]]
[[- if .SymlinkTarget ]]
- Symlink target: [[ .SymlinkTarget ]]
[[- end ]]
[[ if .Missing ]]
There is no file at that path. You will create one.
[[- end ]]
[[- if .Invalid ]]
The file at that path does not load. Kontora reported:

    [[ .ValidationError ]]

Repair it. Keep every setting the user meant to keep.
[[- end ]]
[[- if .Valid ]]
The file at that path loads. Change only what the user asks for. Leave every
other setting as it is.
[[- end ]]
[[- if .SymlinkTarget ]]

The config path is a symbolic link, so keep the link.
Edit `[[ .SymlinkTarget ]]` instead.
Do not replace `[[ .ConfigPath ]]` with a regular file: an atomic rename onto
the link path destroys the link, and the user's dotfiles repository then holds a
file nothing reads.
[[ end ]]

## Rules for this task

1. Read the raw config file at the path above. Do not use `kontora config` as
   the source you write back: it prints the effective config, which includes
   built-in defaults such as the `rework` stage. Writing that output to disk
   turns defaults into user settings.
2. Ask the user the questions in the interview below before you write anything.
   Do not guess an agent binary, a repository path, or an automation boundary.
3. Show the user the agents, stages, pipelines, and projects you intend to
   write, and where automation stops for human review. Get an explicit yes.
4. Write the file only after the user agrees and after validation passes.

An ordinary `kontora new` defaults to `todo`. With `auto_pick_up: true` (the default),
a running daemon can start it immediately.
Use `new --status open` for a draft, then an explicit `kontora run` after review.

## Interview

Ask about each of these. Skip a question when the current config already answers
it and the user does not want it changed.

- Which coding agent CLIs are installed, and what does each need? Binary name or
  full path, arguments, model flag, wrapper command, authentication.
- Where should tickets, logs, and git worktrees live?
- Which repositories will run tickets? Each one that needs its own defaults
  becomes a `projects:` entry.
- Which pipeline should a new ticket run when nothing names one? That answer is
  `default_pipeline`. Without it, such a ticket runs one agent on its
  description instead of a pipeline.
- Which pipeline should each project use for ordinary tickets? Should children of
  any epic use a separate `epic_child_pipeline`, including children of passive epics?
  Both project fields must name configured pipelines. Neither accepts `none`.
- What git branch prefix should Kontora use for work branches?
- How many agents may run at once?
- Where must automation stop and wait for a human?
- May a stage commit? May a stage push or open a pull request?
- Should Kotik observe ticket attention? It is off by default and shares one
  inbox and read state across devices. Model explanations run only when requested.

Kontora resolves `binary` as an executable and passes `args` literally.
It does not use shell functions or aliases, or read your interactive shell configuration.
A wrapper must be configured explicitly in `binary` and `args`.

Use this example only if the user already has `nono` and a configured profile.
Replace `<existing-profile>` with that profile's actual name. Kontora does not install or create profiles.

```yaml
agents:
  claude:
    binary: nono
    args: ["run", "--profile", "<existing-profile>", "--", "claude", "--dangerously-skip-permissions"]
```

Kontora reads the real agent through the `--` separator for `nono` and `op`, so
resume support still works behind those two wrappers.

## Config shape

Top-level keys: `tickets_dir`, `secondary_tickets_dirs`, `logs_dir`, `worktrees_dir`,
`branch_prefix`, `branch_naming`, `editor`, `pager`, `default_agent`, `default_pipeline`,
`max_concurrent_agents`, `max_concurrent_epic_supervisors`, `auto_pick_up`,
`instance_name`, `author`, `tmux_session`, `statuses`, `environment`, `hooks`,
`system_prompt`, `resume_prompt`, `annotation_prompt`, `summary_model`, `summary_effort`,
`web`, `agents`, `stages`, `pipelines`, `projects`, `automations`, `plannotator`,
`assistant`, `metrics`, `notifications`.

```yaml
tickets_dir: ~/.kontora/tickets
logs_dir: ~/.kontora/logs
worktrees_dir: ~/.kontora/worktrees
branch_prefix: kontora
max_concurrent_agents: 3
default_agent: claude
default_pipeline: default

web:
  enabled: true
  host: 127.0.0.1
  port: 8080

agents:
  claude:
    binary: claude
    args: ["--dangerously-skip-permissions"]

stages:
  implement:
    prompt: |
      {{ .Ticket.Description }}

      Do NOT commit or push. Only implement the code and run tests.
    timeout: 60m
    # Optional `model:`. Kontora passes it to the agent as `--model`, replacing
    # any `--model` in the agent's args. It is one pattern for every agent
    # (model: haiku), or a map from agent name or agent kind to a pattern
    # (model: {claude: haiku, pi: anthropic/claude-haiku-4-5}).

pipelines:
  default:
    - stage: implement
      agent: claude
      on_success: human_review
      on_failure: pause

projects:
  myrepo:
    path: ~/projects/myrepo
    pipeline: default
    epic_child_pipeline: default # optional; can name a different configured pipeline
    agent: claude
    prefix: mr # optional; ticket IDs are <prefix>-<4 random alphanumerics>
```

Without `prefix`, a ticket ID takes the first alphanumeric of each `-` or `_`
separated segment of the directory name (`customer-portal-app` gives `cpa`),
falling back to the name's first 3 alphanumerics when that gives fewer than two
(`kontora` gives `kon`). Set `prefix` only to override that.

A stage prompt is a Go `text/template` string. Available expressions:
`{{ .Ticket.ID }}`, `{{ .Ticket.Title }}`, `{{ .Ticket.Description }}`,
`{{ .Ticket.FilePath }}`, and `{{ file "PLAN.md" }}`, which reads a file from
the ticket's worktree. Stages in one pipeline share a worktree, so a file is how
one stage hands work to the next: a plan stage writes `PLAN.md`, the next stage
reads it.

`on_success` accepts `next`, `done`, `human_review`, or a custom status from
`statuses:`. `on_failure` accepts `retry`, `back`, `pause`, `human_review`, or a
custom status. `max_retries` applies to `retry` and defaults to `0` when omitted.
With `on_failure: retry` and no retry budget, the first failure pauses the ticket.

A project's `agent` is stamped into each new ticket for that repository, and a
ticket-level agent replaces the agent on every step of its pipeline at runtime.
Set a project agent only when one agent should run the whole pipeline for that
repository.

## Epic-child pipeline defaults

For an ordinary ticket, pipeline precedence is:

1. Explicit request, including `none` for standalone execution.
2. Stored nonempty pipeline, during initialization only.
3. Project `epic_child_pipeline`, when the resolved parent is an actual epic.
4. Project `pipeline`.
5. Global `default_pipeline`.
6. Standalone execution.

The child default applies under passive and agent epics, independently of supervisor
activation. Missing or ordinary-ticket parents do not qualify. Initialization resolves
actual membership before filling a blank pipeline. Project matching remains lexical;
linked worktrees and symlinks do not gain a match.

Kontora stores defaults at creation or initialization. Existing assignments are not
migrated after config changes, reparenting or detail-path edits. Operators must
retarget existing tickets explicitly. Raw uploads and daemon startup do not apply
defaults. `none` stores a blank, not durable opt-out provenance. A later initialization
can fill it unless the caller opts out again. Agent resolution stays independent,
including when the request uses `--pipeline none`.

Epics never get pipelines; supervisors run standalone. Supervisor prompts instruct
them not to change, clear or replace child pipelines. They must not edit pipeline
definitions, stage definitions or project pipeline defaults.
Supervisors must create children without `--pipeline`. They must not bypass workflows through `init`, stage skipping, direct
file edits or pause/retry sequences. If configured work conflicts with their authority,
they must report the conflict, request an operator decision and leave that work
unstarted. They must not substitute another pipeline. These are prompt instructions,
not runtime enforcement. Ordinary operator commands remain available.

A pipeline default does not authorize commits or publication. `commit-no-push` still
contains a commit stage. Ask about the user's authority boundary before selecting
child workflows, rather than removing stages to make conflicting work runnable.

Upgrade all clients and daemon binaries that read the config before adding
`epic_child_pipeline`. Older strict-YAML loaders reject the key. Do not add the key
until compatible binaries are installed.

## Optional companion

Configure Kotik under `assistant`, not as a pipeline or notification channel:

```yaml
assistant:
  companion:
    enabled: false
    name: Kotik
```

Enable observation only if the user wants it. `enabled` defaults to `false`;
`name` defaults to `Kotik`, including when explicitly empty.

The daemon maintains one shared inbox without an open browser or configured
model. Reading an item does not answer a question, complete a review or mutate
a ticket. The inbox retains all active items plus at most 500 inactive records.
Inactive records expire after 30 days. State lives at
`<logs_dir>/companion/<daemon-scope>/state.json`, scoped by normalized daemon-lock
and ticket-directory paths.

Briefings, investigations and follow-ups require explicit user requests and
`assistant.agent`, naming an existing `claude` or `pi` agent. No model runs on
startup, polling, reload or browser reconnection. Each request accepts at most
five selected tickets and 32 KiB of server-built evidence. Kontora saves the
evidence before starting the model. Capture metadata and omissions remain
available when reopening chats. Changed sources mark explanations outdated;
they do not rewrite historical answers or current attention.

Companion chats deny all tools and autonomy escalation. They cannot run
commands, inspect arbitrary files, mutate tickets, answer agents, execute
approvals or send outbound messages. `assistant.autonomy` and `assistant.prompt`
do not override companion restrictions. Kontora uses runtime hooks with a
trusted configured agent; this is not an OS sandbox against malicious binaries
or installed extensions. Existing notifications remain independent.

If storage is corrupt, unsupported or unavailable, the companion reports
degraded health and preserves the file. Kontora refuses read-state updates it
cannot save, while task execution and notifications continue. Polling can miss
short-lived transitions, and offline events are not a complete history.
A disappeared question is not proof that someone answered it.

## Workflow archetypes

Offer these. Create only the ones the user picks. Do not write all of them.

1. One agent run, then `human_review`.
2. Implement, review, fix, then `human_review`.
3. Implement, review, fix, commit, then `human_review`.
4. Push and open a pull request. Add this only after the user says so
   explicitly, in that session, for that repository.
5. Plan only, when the user asks for a plan before any code.

The default boundary is `human_review`. A pipeline that ends in `done` gives the
user no place to look at the result before the ticket closes.

## Validation rules

The config fails to load when any of these does not hold:

- Every agent has a non-empty `binary`.
- Every `failure_pattern` compiles as a Go regular expression.
- `default_agent` names an agent that exists. It is inferred only when an agent
  is named `claude` or when exactly one agent is defined. Two or more agents
  with none named `claude` need it set explicitly.
- `checkpoint_compaction_tokens` is non-negative. A positive value is valid only
  on a `pi` or `claude` agent, including a wrapped one such as `nono run -- pi`
  or `nono run -- claude`.
- `default_pipeline`, when set, names a pipeline that exists.
- No agent and no pipeline is named `none`. That word is the opt-out sentinel.
- Every pipeline has at least one step.
- Every pipeline step names a stage and an agent that exist.
- Every pipeline step sets both `on_success` and `on_failure`. Neither has a
  default, so a step that omits one does not load.
- The first step of a pipeline does not use `on_failure: back`.
- The last step of a pipeline does not use `on_success: next`.
- A stage appears at most once in the same pipeline.
- Every project has a `path`, and no two projects normalize to the same
  directory.
- A project's `pipeline` and `epic_child_pipeline`, when set, name configured
  pipelines. Neither accepts `none`; it is only a ticket-level opt-out.
- A project's `agent`, when set, names a configured agent.
- A custom status matches `[a-z][a-z0-9_]*`, is not repeated, and does not clash
  with a built-in status or with `next`, `retry`, `back`.
- `tmux_session` is 1 to 64 characters from `A-Za-z0-9_-` and does not start
  with `-`.
- `branch_naming.mode`, at the top level and inside a project, is `off` or
  `slug`.
- `assistant.companion.name` is at most 64 Unicode characters, not whitespace-only,
  and contains no control characters. An empty value defaults to `Kotik`.
  Validation does not depend on `assistant.agent` or `companion.enabled`.
- Every `timeout` carries a unit: `60m`, `2h`, `90s`. A bare number does not
  parse.
- A `model`, on a stage or in `summary_model`, is one pattern or a map. Every
  map key is a configured agent name or an agent kind (`claude`, `pi`). A list
  does not parse.
- No pipeline step runs a stage that resolves a model on an agent that is
  neither `claude` nor `pi`. Those two are the only CLIs Kontora passes
  `--model` to.
- Every `hooks` key, at the top level and inside a project, is one of
  `worktree_created`, `stage_start`, `stage_end`. Every hook has a non-blank
  `run`; its `on_failure`, when set, is `pause` or `warn`; its `timeout` is not
  negative.

Unknown YAML keys are rejected. A typo fails the whole load, so a field you
invent will not be ignored. Keep the settings the user already has, and do not
drop a block you do not recognize without asking.

Do not put secrets in the config. For the web token, tell the user to pass
`KONTORA_WEB_TOKEN` to `kontora start` instead of writing `web.token`.

## Apply procedure

1. Create the directory that holds the config. Nothing else creates it before
   the first write:

       mkdir -p [[ .ConfigDir ]]

2. Write the candidate to a temporary file, for example
   `[[ .ConfigDir ]]/config.candidate.yaml`.
3. Validate it:

       kontora doctor --config [[ .ConfigDir ]]/config.candidate.yaml

   `doctor` fails when the config does not parse or validate, when `git` or
   `tmux` is missing, and when an agent binary cannot be resolved. It resolves
   the `binary` field, so a wrapped agent such as `binary: nono` is reported
   against `nono`, not against the agent behind the `--`. Check that one
   yourself. Resolution searches `$PATH`, then `~/.local/bin`,
   `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, `/bin`.

   Two checks only warn, and both are normal: a tickets or logs directory that
   does not exist yet, which the daemon creates on use, and a web port that is
   unavailable, which is what an already running daemon looks like. Fix the
   failures. Read the warnings and decide.
4. Back up the current file when one exists.
[[ if .SymlinkTarget -]]
5. Copy the candidate over `[[ .SymlinkTarget ]]`. Write through the target so
   the symlink at `[[ .ConfigPath ]]` survives. Keep the target's permissions.
[[ else -]]
5. Copy the candidate over `[[ .ConfigPath ]]`. Keep the file's permissions.
[[ end -]]
6. Delete the candidate file.
7. Validate the live path:

       kontora doctor --config [[ .ConfigPath ]]

`doctor` does not parse stage prompt templates. A broken `{{` in a prompt is
found when that stage starts, not before. Read every prompt you write.

## After the write

A running daemon reloads these without a restart: `agents`, `stages`,
`pipelines`, `projects`, `automations`, `statuses`, `environment`, `hooks`,
`auto_pick_up`, `default_agent`, `default_pipeline`, `branch_prefix`, `branch_naming`,
`author`, `system_prompt`, `resume_prompt`, `annotation_prompt`, `summary_model`,
`summary_effort`, `max_concurrent_agents`, `max_concurrent_epic_supervisors`,
`notifications`, and `assistant`. The `plannotator` block reloads live except for
`plannotator.reviews_dir`. The CLI reads `editor` and `pager` on the next command.

Both concurrency limits apply to the next admission. Raising a limit lets queued
work start. Lowering a limit does not stop running agents; new work waits until
usage falls below the limit.

Enabling `auto_pick_up` queues existing eligible `todo` tickets. Disabling it
stops automatic starts, including tickets already queued. Explicit runs,
pipeline continuations, recovery, and scheduled automations remain possible.

Disabling `assistant.companion.enabled` stops observation and prevents new
companion requests. Disabling retains stored data. An already-running requested
turn remains stoppable.

These need a daemon restart: `tickets_dir`, `secondary_tickets_dirs`, `worktrees_dir`,
`logs_dir`, `instance_name`, `tmux_session`, `plannotator.reviews_dir`, and the whole
`web` and `metrics` blocks. The daemon keeps the running value and logs one warning
per field that differs on disk.

A running agent keeps the prompt, arguments, model, timeout, and binary it
started with. An edit takes effect on the next stage that spawns. Changes to project
`epic_child_pipeline` or other pipeline defaults affect later creation and
initialization without rewriting existing assignments. Initialization preserves
stored nonempty pipelines before applying defaults.

A reload is all-or-nothing: an invalid file is logged and the daemon keeps
running on the previous config.

Suggest these next steps. Do not start the daemon or create tickets without the user's approval.
Replace the example repository path with the user's actual path.

If no daemon is running, start it in one terminal. It runs until Ctrl-C:

    kontora start --config [[ .ConfigPath ]]

In another terminal, create a draft:

    kontora new --config [[ .ConfigPath ]] --status open --path ~/projects/myrepo "Add a health check endpoint"

Review the ticket before starting work. Replace `TICKET_ID` with the ID printed by `new`:

    kontora run --config [[ .ConfigPath ]] TICKET_ID

The daemon watches the config file, so a later edit reloads on its own.
