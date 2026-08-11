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

## Interview

Ask about each of these. Skip a question when the current config already answers
it and the user does not want it changed.

- Which coding agent CLIs are installed, and what does each need? Binary name or
  full path, arguments, model flag, wrapper command, authentication.
- Where should tickets, logs, and git worktrees live?
- Which repositories will run tickets? Each one that needs its own defaults
  becomes a `projects:` entry.
- What git branch prefix should Kontora use for work branches?
- How many agents may run at once?
- Where must automation stop and wait for a human?
- May a stage commit? May a stage push or open a pull request?

Kontora runs a configured binary directly with `exec`. It does not start a
shell, so shell functions, aliases, and shell-only PATH entries do not apply.
A wrapper command has to be spelled out in `binary` and `args`:

```yaml
agents:
  claude:
    binary: nono
    args: ["run", "--profile", "claude", "--", "claude", "--dangerously-skip-permissions"]
```

Kontora reads the real agent through the `--` separator for `nono` and `op`, so
resume support still works behind those two wrappers.

## Config shape

Top-level keys: `tickets_dir`, `logs_dir`, `worktrees_dir`, `branch_prefix`,
`branch_naming`, `editor`, `default_agent`, `max_concurrent_agents`,
`auto_pick_up`, `instance_name`, `tmux_session`, `statuses`, `environment`,
`resume_prompt`, `annotation_prompt`, `web`, `agents`, `stages`, `pipelines`,
`projects`, `plannotator`.

```yaml
tickets_dir: ~/.kontora/tickets
logs_dir: ~/.kontora/logs
worktrees_dir: ~/.kontora/worktrees
branch_prefix: kontora
max_concurrent_agents: 3
default_agent: claude

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
    agent: claude
```

A stage prompt is a Go `text/template` string. Available expressions:
`{{ .Ticket.ID }}`, `{{ .Ticket.Title }}`, `{{ .Ticket.Description }}`,
`{{ .Ticket.FilePath }}`, and `{{ file "PLAN.md" }}`, which reads a file from
the ticket's worktree. Stages in one pipeline share a worktree, so a file is how
one stage hands work to the next: a plan stage writes `PLAN.md`, the next stage
reads it.

`on_success` accepts `next`, `done`, `human_review`, or a custom status from
`statuses:`. `on_failure` accepts `retry`, `back`, `pause`, `human_review`, or a
custom status. `max_retries` applies to `retry`.

A project's `agent` is stamped into each new ticket for that repository, and a
ticket-level agent replaces the agent on every step of its pipeline at runtime.
Set a project agent only when one agent should run the whole pipeline for that
repository.

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
- A project's `pipeline` and `agent`, when set, exist.
- A custom status matches `[a-z][a-z0-9_]*`, is not repeated, and does not clash
  with a built-in status or with `next`, `retry`, `back`.
- `tmux_session` is 1 to 64 characters from `A-Za-z0-9_-` and does not start
  with `-`.
- `branch_naming.mode`, at the top level and inside a project, is `off` or
  `slug`.
- Every `timeout` carries a unit: `60m`, `2h`, `90s`. A bare number does not
  parse.

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
`pipelines`, `projects`, `statuses`, `environment`, `auto_pick_up`,
`default_agent`, `branch_prefix`, `branch_naming`, `editor`, `resume_prompt`,
`annotation_prompt`, and `plannotator`.

These need a daemon restart: `tickets_dir`, `worktrees_dir`, `logs_dir`,
`instance_name`, `tmux_session`, `max_concurrent_agents`, and the whole `web`
block. The daemon keeps the running value and logs one warning per field that
differs on disk.

A running agent keeps the prompt, arguments, timeout, and binary it started
with. An edit takes effect on the next stage that spawns.

A reload is all-or-nothing: an invalid file is logged and the daemon keeps
running on the previous config.

Tell the user what to do next. `kontora start` runs until Ctrl-C, so the second
command needs another terminal, and `kontora new` needs `--path` unless it runs
inside the repository:

    kontora start
    kontora new --path ~/projects/myrepo "Add a health check endpoint"

The daemon watches the config file, so a later edit reloads on its own.
