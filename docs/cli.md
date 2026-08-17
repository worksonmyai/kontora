# CLI Reference

`kontora <command> [flags] [arguments]`. There is no command framework: every verb is a flat top-level word, and flags use Go's `flag` package, so `-flag` and `--flag` both work.

Run `kontora help` for the same list of commands this page documents.

## Common flags

| Flag | Applies to | Description |
|------|-----------|-------------|
| `--config PATH` | every command that reads the config | Path to the config file. Defaults to `$KONTORA_CONFIG`, then `./.kontora/config.yaml`, then `$XDG_CONFIG_HOME/kontora/config.yaml`, then `~/.config/kontora/config.yaml`. |
| `--url URL` | every command that can drive a daemon | Remote daemon URL. A non-empty value switches the command into [remote mode](#remote-mode). Defaults to `$KONTORA_URL`. |
| `--token TOKEN` | the same commands | Bearer token for the remote daemon. Defaults to `$KONTORA_TOKEN`. |

Flags may be written before, between, or after the positional arguments: `kontora pause abc --config ~/k.yaml` is the same as `kontora pause --config ~/k.yaml abc`. The exceptions are `new`, `note`, and `summary`, whose last argument is free-form text — a flag-looking word there is taken as part of the text, so pass flags before the text.

## Ticket IDs

Every command that takes a `TICKET_ID` accepts a unique prefix of one: `kontora view kon-q` finds `kon-q88f`. A prefix that matches more than one ticket is an error naming every match, rather than a guess.

## Environment variables

| Variable | Read by | Description |
|----------|---------|-------------|
| `KONTORA_CONFIG` | every command | Config file path. The daemon exports it to the agents it spawns, so `kontora note` inside a worktree writes to the daemon's own config. |
| `KONTORA_URL` | client commands | Remote daemon URL; setting it turns on remote mode. |
| `KONTORA_TOKEN` | client commands | Bearer token sent to the remote daemon. |
| `KONTORA_WEB_TOKEN` | `kontora start` only | Overrides `web.token` on the daemon side, so a deployment can inject the secret instead of writing it into the config file. |

## Tickets

### `kontora ls`

Lists tickets. On a terminal it opens the kanban TUI; otherwise it prints a static table. Done and cancelled tickets are hidden unless `--closed` is given, and archived tickets are always hidden.

| Flag | Description |
|------|-------------|
| `--closed` | Include done and cancelled tickets. |
| `--static` | Print the static table even on a terminal. |

### `kontora new [flags] TITLE`

Creates a ticket and prints its ID. Without `--path` it uses the current git root; in remote mode `--path` is required and names a path on the daemon host.

| Flag | Description |
|------|-------------|
| `--path PATH` | Repository the ticket works in. |
| `--pipeline NAME` | Pipeline to run, or `none` to skip the project default and run standalone. |
| `--agent NAME` | Agent to use, or `none` to skip the project default. |
| `--branch NAME` | Work branch name. Defaults to `<branch_prefix>/<id>`. |
| `--base-branch NAME` | Branch the work branch starts from. Defaults to the repository's default branch. |

### `kontora view TICKET_ID`

Prints the ticket's status, pipeline position, and body.

### `kontora edit TICKET_ID`

Opens the ticket file in `$EDITOR`. Local only.

### `kontora update TICKET_ID [flags]`

Changes frontmatter fields or the body of an existing ticket. At least one flag is required. Passing an empty value, or `none` where noted, clears the field.

| Flag | Description |
|------|-------------|
| `--body-file PATH` | Replace the body from a file, or `-` for stdin. |
| `--pipeline NAME` | Set the pipeline (`""` or `none` clears it). |
| `--path PATH` | Set the repository path. |
| `--agent NAME` | Set the agent override (`""` or `none` clears it). |
| `--branch NAME` | Set the work branch (`""` clears it). |
| `--base-branch NAME` | Set the base branch (`""` clears it). |

### `kontora delete TICKET_ID -f`

Deletes the ticket's markdown file. `-f` or `--yes` is required. The worktree is not cleaned up; cancel the ticket first if you want the normal cleanup path.

### `kontora init TICKET_ID [flags]`

Marks an existing ticket file `kontora: true` so the daemon will run it. Without flags it prompts for the missing fields; naming them makes the command non-interactive, which is what a script or a coding agent needs.

| Flag | Description |
|------|-------------|
| `--path PATH` | Repository path. Required when the frontmatter has none. |
| `--pipeline NAME` | Pipeline to run, or `none` for a standalone ticket. |
| `--agent NAME` | Agent to use, or `none` to skip the project default. |
| `--stage NAME` | Starting stage. Defaults to the pipeline's first stage when `--pipeline` is given. |
| `--status STATUS` | `open` or `todo`. Defaults to asking. |

In remote mode `--pipeline` and `--path` are both required, because the pickers cannot run over HTTP.

## Lifecycle

### `kontora run TICKET_ID`

Enqueues an `open` or `todo` ticket. Needs a running daemon.

### `kontora pause TICKET_ID`

Stops a running ticket and parks it in `paused`. A ticket that already closed cannot be paused; use `retry` to reopen it.

### `kontora retry TICKET_ID`

Resets a non-running ticket to `todo` with the attempt counter cleared, and re-enqueues it.

### `kontora cancel TICKET_ID` / `kontora done TICKET_ID`

Move the ticket to `cancelled` or `done`, stopping any running agent and cleaning up its worktree.

### `kontora move TICKET_ID STATUS`

The general form of the three above: moves a ticket to any status the config allows, which is the only way to reach `human_review` or a custom status from `statuses:` on the command line. Valid values are `open`, `todo`, `paused`, `human_review`, `done`, `cancelled`, plus any custom status. `archived` is deliberately excluded — see [`kontora archive`](#kontora-archive-days-n).

### `kontora skip TICKET_ID`

Advances the ticket to the next stage of its pipeline, or marks it done when it is already on the last one.

### `kontora set-stage TICKET_ID STAGE`

Moves the ticket to a named stage of its pipeline without running anything.

### `kontora note TICKET_ID [TEXT]` / `kontora summary TICKET_ID [TEXT]`

Append a timestamped note, or set the ticket's one-line summary. Both read the text from stdin when it is not given as an argument, so `git log --oneline | kontora note kon-q88f` works.

### `kontora archive --days N`

Marks every `done` or `cancelled` ticket whose file has not been modified for at least N days as `archived`, which hides it everywhere while keeping the file. `--days` is required and must be positive. Local only.

| Flag | Description |
|------|-------------|
| `--days N` | Required. Age threshold in days, measured on the file's modification time. |
| `--dry-run` | List what would be archived and write nothing. |
| `--path PATH` | Only archive tickets for this repository path. |
| `--project NAME` | The same, by configured project name. |
| `--status STATUS` | Only archive `done` or only `cancelled`. |
| `-y`, `--yes` | Skip the confirmation prompt. Required when stdin is not a terminal. |

## Inspecting a run

### `kontora logs [--stage NAME] TICKET_ID`

Prints the agent's log. Without `--stage` it shows the most recent one, falling back to the ticket's run history when there are no log files.

### `kontora activity [--stage NAME] [--run N] TICKET_ID`

Prints one stage run's structured transcript: the agent's messages and tool calls, one per line. Falls back to the plaintext log for agents that write no session record. Needs a running daemon.

### `kontora changes TICKET_ID`

Prints the commits on the ticket's branch and the per-file line counts against its base. Needs a running daemon.

### `kontora stats [flags]`

Prints the figures the dashboard's Stats page shows: throughput, median cycle time, first-pass rate, and per-stage, per-agent and per-project breakdowns. Needs a running daemon.

| Flag | Description |
|------|-------------|
| `--range RANGE` | `1d`, `1w`, `30d`, `90d` or `all`. Defaults to `90d`. |
| `--project NAME` | Only count tickets for this configured project. |
| `--pipeline NAME` | Only count tickets for this pipeline. |

### `kontora attach [--rw] TICKET_ID`

Attaches to the running ticket's terminal: the tmux window locally, a WebSocket to the daemon in remote mode. Read-only unless `--rw` is given. Locally, omitting the ticket ID opens a picker of running sessions.

### `kontora review TICKET_ID` / `kontora annotate TICKET_ID`

Open the ticket's diff, or its markdown, in [Plannotator](https://plannotator.ai). The daemon spawns the binary, so in remote mode the window opens on the daemon host, not on the caller's screen. `review` needs the ticket to be in `human_review`.

## Analysis

### `kontora estimate-compaction [flags]`

Projects token savings from checkpoint compaction by replaying completed Pi session traces. Read-only: it scans JSONL files and writes nothing. Local only.

| Flag | Description |
|------|-------------|
| `--logs-dir PATH` | Path to the logs directory. Defaults to the config's `logs_dir`. When provided, the config file is not required. |
| `--stage NAME` | Pipeline stage to analyze. Defaults to `implement`. |
| `--thresholds LIST` | Comma-separated token thresholds to evaluate, e.g. `100000,150000,200000`. Defaults to `100000,125000,150000,200000,250000`. |
| `--top N` | Number of top sessions to include in the report. Defaults to `20`. |

The report includes:

- Coverage includes scanned and eligible sessions. It also counts `branch_summary`, `no_usage`, `unreadable`, `broken_chain`, `has_compaction`, and `live_resume` exclusions. A session is treated as live when a resume marker identifies it or its latest recorded activity is less than 15 minutes old. This check uses timestamps inside the JSONL, so copying old sessions does not mark them as live.
- Calibration shows the summary ratio, summary output tokens, and post-compaction context. Each statistic includes its sample count and low, median, and high values. Without a usable post-compaction sample, the report prints no token-reduction figures.
- Scenarios show projected compaction counts and raw-token reductions for `CAL P25`, `CAL P50`, and `CAL P75`. These labels are calibration percentiles, not reduction bounds. `CAL P25` uses smaller summaries and post-compaction contexts and usually projects the largest reduction; `CAL P75` uses larger values and usually projects the smallest. The scenarios replay the recorded call sequence. They do not predict changed agent behavior.
- Top sessions rank session files by projected median raw-token reduction at the lowest requested threshold. Each row includes the ticket, stage, and session filename so retries remain distinguishable.
- Limits state why the report does not estimate implementation quality or cost. They cover changed tool calls, cache identity, and pricing.

Example:

```text
kontora estimate-compaction \
  --stage implement \
  --thresholds 100000,125000,150000,200000,250000 \
  --top 20
```

## Daemon and configuration

### `kontora start [flags]`

Runs the daemon in the foreground. Only one instance per config directory: a second one exits on the file lock. Local only.

| Flag | Description |
|------|-------------|
| `--address HOST` | Web server listen address, overriding `web.host`. |
| `--port PORT` | Web server port, overriding `web.port`. |

### `kontora setup [--agent]`

Writes the config file. Plain `setup` runs an interactive wizard. `setup --agent` prints a brief for a coding agent to follow and writes nothing itself; the brief is embedded in the binary, so it describes the schema the installed version accepts. Local only.

### `kontora doctor`

Checks the config, the required tools (`git`, `tmux`), each agent binary, each configured project's repository, Plannotator, and the web port. Exits non-zero when a hard prerequisite fails; missing directories and an occupied port are warnings. Local only.

### `kontora config` / `kontora config edit`

`config` prints the effective configuration with defaults applied, with `web.token` replaced by a placeholder. In remote mode it prints the daemon's pipelines, agents, and custom statuses instead.

`config edit` opens the config in `$EDITOR`. In remote mode it fetches the daemon's file, validates the result locally, and uploads it; the daemon reloads as part of the save.

## Utilities

### `kontora fmt`

Reads Claude Code stream-json on stdin and prints it as readable text. Takes no flags and talks to nothing, so it works with `KONTORA_URL` set.

### `kontora completion fish`

Prints a fish completion script. Install it for the session with `kontora completion fish | source`, or persist it to `~/.config/fish/completions/kontora.fish`. fish is the only shell supported today.

### `kontora version`

Prints the version.

## Remote mode

Setting `--url` or `KONTORA_URL` points every client command at a daemon over HTTP instead of the local files. See [Remote mode](https://github.com/worksonmyai/kontora#remote-mode) in the README for the daemon-side setup.

Supported: `ls`, `view`, `new`, `init`, `update`, `delete`, `run`, `pause`, `retry`, `cancel`, `done`, `move`, `skip`, `set-stage`, `note`, `summary`, `logs`, `activity`, `changes`, `stats`, `review`, `annotate`, `config`, `attach`.

Rejected, because they act on local files: `edit`, `archive`, `estimate-compaction`, `doctor`, `start`, `setup`.

Unaffected, because they touch neither the daemon nor a config file: `fmt`, `completion`, `version`, `help`.

Paths passed to `--path` name locations on the daemon host, not on the caller's machine.
