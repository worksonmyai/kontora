# CLI Reference

`kontora <command> [flags] [arguments]`. There is no command framework: every verb is a flat top-level word, and flags use Go's `flag` package, so `-flag` and `--flag` both work.

Run `kontora help` for the same list of commands this page documents.

## Common flags

| Flag | Applies to | Description |
|------|-----------|-------------|
| `--config PATH` | every command that reads the config | Path to the config file. Defaults to `$KONTORA_CONFIG`, then `./.kontora/config.yaml`, then `$XDG_CONFIG_HOME/kontora/config.yaml`, then `~/.config/kontora/config.yaml`. |
| `--tickets-dir PATH` | every command that reads the local ticket store | Overrides `tickets_dir`. The highest precedence there is: above `$KONTORA_TICKETS_DIR`, `$TICKETS_DIR` and the config file. Ignored in remote mode, where the daemon owns the store. |
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
| `KONTORA_TICKETS_DIR` | every command that reads the local ticket store | Overrides `tickets_dir` from the config file. The daemon exports its resolved value to the agents it spawns. |
| `TICKETS_DIR` | the same commands | Compatibility alias for the standalone `ticket` CLI. Read only when `KONTORA_TICKETS_DIR` is unset or blank. |
| `KONTORA_PAGER` | `view`, `logs`, `activity` | Pager command, split on whitespace the way `$EDITOR` is. Used only when stdout is a terminal, so a redirect or a pipe is never paged. Set but blank turns paging off. |
| `TICKET_PAGER` | the same commands | Read when `KONTORA_PAGER` is unset, so a `ticket` user's setting carries over. |
| `PAGER` | the same commands | The last fallback before the config's `pager`. |

The tickets dir is resolved `--tickets-dir` → `$KONTORA_TICKETS_DIR` → `$TICKETS_DIR` → `tickets_dir:` → `~/.kontora/tickets`, and the pager `$KONTORA_PAGER` → `$TICKET_PAGER` → `$PAGER` → `pager:` → none.

Note that unlike every other setting, the environment beats the config file for the tickets dir. A stray export therefore changes which store every command reads, and a daemon that cannot see the variable — one started by launchd or systemd, which pass a minimal environment — keeps reading the file. `kontora doctor` names the source that won and warns when the two disagree.

## Tickets

### `kontora ls`

Lists tickets. On a terminal with no flags it opens the kanban TUI; any filter, `--json`, `--static`, `--closed` or `--archived` makes it print instead. Done, cancelled and legacy `closed` tickets are hidden unless `--closed` is given; archived ones need `--archived`.

| Flag | Description |
|------|-------------|
| `--closed` | Include done, cancelled and legacy `closed` tickets. |
| `--archived` | Include archived tickets. |
| `--status STATUS` | Only tickets in this status. Naming a hidden status shows it, so `--status done` needs no `--closed` and `--status archived` no `--archived`. |
| `--project NAME` | Only tickets for this configured project. |
| `--path PATH` | Only tickets for this repository path. |
| `--ready` | Only `todo` kontora tickets whose dependencies are all closed. |
| `--blocked` | Only `todo` kontora tickets waiting on a dependency. |
| `--limit N` | Print at most N tickets. |
| `--json` | Print JSON instead of a table. |
| `--static` | Print the static table even on a terminal. |

`--ready` and `--blocked` select opposite sets and cannot be combined; passing both exits non-zero. Both follow scheduler order, oldest `created` first. Every other listing keeps the board's order: by status, then most recently touched.

The static table has ID, status, title, project, stage, agent, and the ids blocking the ticket.

`--json` prints an array of objects with `id`, `title`, `status`, `kontora`, `path`, `project`, `pipeline`, `stage`, `agent`, `created_at`, `started_at`, `completed_at`, `deps`, `links`, `parent`, `ready` and `blockers`. It never carries the ticket body; `kontora view --body` is what prints that. `deps`, `links` and `blockers` are always arrays, and an empty result is `[]`, so a script can iterate without a nil check.

Remote mode asks the daemon for every ticket, including the hidden statuses, and applies the same filters locally, so a flag means the same thing on both sides.

### `kontora search [flags] QUERY`

Matches QUERY against every ticket file in `tickets_dir`, frontmatter block and body alike, and prints the matching lines grouped by ticket. Every status is searched, archived included, so this is the only command that can find a closed ticket by what is written in it. Local only: it reads the files directly and needs no daemon.

QUERY is a [Go regular expression](https://pkg.go.dev/regexp/syntax). Case follows ripgrep's smart-case rule: an all-lowercase query matches case-insensitively, and a query holding an uppercase letter matches case-sensitively. A query that starts with `-` needs `--` in front of it, as in `kontora search -- -race`.

Because the match runs over the raw markdown, frontmatter keys kontora does not own are searchable too: `kontora search 'priority: 2'` finds them. The one thing not in the file is the project name, which comes from the config; a query matching a configured project name reports that project's tickets with a synthetic `project` line.

| Flag | Description |
|------|-------------|
| `-i` | Case-insensitive match, overriding smart-case. |
| `-s` | Case-sensitive match, overriding smart-case. |
| `-F` | Treat the query as a literal string, not a regex. |
| `-l` | Print matching ticket IDs only, one per line, for piping into `xargs`. |
| `-m N` | Show at most N matching lines per ticket. Defaults to 5; `0` shows all. |
| `--body` | Search the markdown body only, skipping the frontmatter. Project names are not matched either: a project name is in neither. |
| `--json` | Print a JSON array of results instead of the grouped listing. |
| `--status STATUS` | Only search tickets with this status. A status that is neither built in nor configured is a warning on stderr, since a ticket written by another tool may carry any status. |
| `--project NAME` | Only search tickets for this configured project. An unknown name is an error. |
| `--path PATH` | The same, by repository path. |
| `--pipeline NAME` | Only search tickets for this pipeline. An unknown name is an error. |
| `--agent NAME` | Only search tickets carrying this agent override. An unknown name is an error. |

Each `--json` object holds `id`, `title`, `status`, `stage`, `pipeline`, `agent`, `path`, `project`, `file`, `total` and a `matches` array of `{line, field, text, start, end}`. `line` is the 1-based file line, `0` for the synthetic project match; `start` and `end` are byte offsets of the match inside `text`; `total` counts every match found, the project match included, so it exceeds `len(matches)` when `-m` cut them. `-m` never cuts the project match, which is the reason the ticket is in the results at all.

`-l` and `--json`, and `-i` and `-s`, cannot be combined. The exit status is 0 whether or not anything matched, unlike grep: a script that needs the difference reads the output, for example `test -n "$(kontora search -l worktree)"`.

A ticket whose frontmatter cannot be parsed is reported on stderr with the count and the parse error, rather than dropped, so a malformed ticket does not silently disappear from a search that should have found it. Only files the query matches are parsed, so a malformed ticket the query is nowhere in stays quiet.

`^` and `$` are line anchors, so `kontora search '^status: done'` matches the frontmatter line rather than the whole file. Note that a query matching a configured project name also matches the `kontora: true` marker line in every ticket kontora manages, so it returns most of the store.

### `kontora new [flags] TITLE`

Creates a ticket and prints its ID. Without `--path` it uses the current git root; in remote mode `--path` is required and names a path on the daemon host.

| Flag | Description |
|------|-------------|
| `--path PATH` | Repository the ticket works in. |
| `--pipeline NAME` | Pipeline to run, or `none` to skip the project default and run standalone. |
| `--agent NAME` | Agent to use, or `none` to skip the project default. |
| `--branch NAME` | Work branch name. Defaults to `<branch_prefix>/<id>`. |
| `--base-branch NAME` | Branch the work branch starts from. Defaults to the repository's default branch. |
| `--status STATUS` | `open` or `todo`. Defaults to `todo`. |
| `--description-file PATH` | Read the markdown that follows the generated `# <title>` heading from a file, or `-` for stdin. |
| `--quiet` | Print only the new ticket ID. |

The ticket file is written once, complete. A ticket created with `--status open` is therefore never visible as `todo`, so a daemon with `auto_pick_up: true` watching the directory cannot claim it before you finish editing it. `--status open` also skips the repository check creation normally runs, because an open ticket is not ready to run.

### `kontora view TICKET_ID`

Prints the ticket's status, pipeline position, and body.

| Flag | Description |
|------|-------------|
| `--body` | Print only the stored markdown body: no metadata, no styling, no synthesized relation sections. |

`--body` prints exactly what the file holds after the closing frontmatter delimiter, which is what `kontora update --body-file` writes back. Reading a body out, editing it, and writing it back changes nothing else in the ticket.

The default output goes through the [pager](#environment-variables) when one is set and stdout is a terminal. `--body` never does, so the round trip stays byte-stable.

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

Enqueues an `open` or `todo` ticket. Needs a running daemon. When the ticket's dependencies are not all closed it reports it as blocked, naming them, rather than saying it was queued: the daemon will not pick it up until they close. See [dependency-aware scheduling](tickets.md#dependency-aware-scheduling).

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

### `kontora phase-complete TICKET_ID --completed TEXT --next TEXT`

Signals a boundary between two top-level ticket phases. Agents call it, not people. It appends one record to the sidecar named by `KONTORA_CHECKPOINT_FILE` and prints what the agent should do next; it reads no config and talks to no daemon.

| Flag | Description |
|------|-------------|
| `--completed TEXT` | The phase that just finished, e.g. `Phase 2: Expose the estimator`. Required. |
| `--next TEXT` | The phase to begin next. Required. |

The daemon sets `KONTORA_CHECKPOINT_FILE` only for runs whose agent has a positive `checkpoint_compaction_tokens`. Without it the command records nothing, prints that checkpoint compaction is off for the run, and exits 0, so an agent that calls it in an unrelated run is not derailed.

## Relations

A ticket carries two relation lists in its frontmatter. `deps` names the tickets it waits on, and the daemon will not run it until every one of them is `done`, `cancelled`, `archived`, or legacy `closed`. See [dependency-aware scheduling](tickets.md#dependency-aware-scheduling). `links` is a symmetric "related" list that means nothing to the scheduler.

All four verbs reject a missing ID and a ticket related to itself, and leave every file unchanged when they do. Repeating a call that has nothing left to do succeeds and writes nothing.

### `kontora dep TICKET_ID DEPENDENCY_ID`

Makes `TICKET_ID` wait on `DEPENDENCY_ID`. Only the dependent ticket is written; the reverse edge is derived when it is read. A dependency that would close a cycle is rejected, and the error names the cycle.

### `kontora undep TICKET_ID DEPENDENCY_ID`

Drops the dependency edge.

### `kontora link TICKET_ID TICKET_ID...`

Relates the first ticket to each of the others. It does not relate the others to each other.

Both sides of a link are written, and two markdown files cannot be written together. If the second write fails, the error names both tickets and says which one was already changed; running the same command again repairs the missing side.

### `kontora unlink TICKET_ID TICKET_ID...`

Removes the relation between the first ticket and each of the others, on both sides.

### `kontora archive --days N`

Marks every `done` or `cancelled` ticket whose file has not been modified for at least N days as `archived`, which hides it everywhere while keeping the file. `--days` is required and must be positive. Local only.

| Flag | Description |
|------|-------------|
| `--days N` | Required. Age threshold in days, measured on the file's modification time. |
| `--dry-run` | List what would be archived and write nothing. |
| `--path PATH` | Only archive tickets for this repository path. |
| `--project NAME` | The same, by configured project name. |
| `--status STATUS` | Only archive one of `done`, `cancelled` or `closed`. |
| `-y`, `--yes` | Skip the confirmation prompt. Required when stdin is not a terminal. |

## Inspecting a run

### `kontora logs [--stage NAME] TICKET_ID`

Prints the agent's log. Without `--stage` it shows the most recent one, falling back to the ticket's run history when there are no log files. Goes through the [pager](#environment-variables) when one is set and stdout is a terminal.

### `kontora activity [--stage NAME] [--run N] TICKET_ID`

Prints one stage run's structured transcript: the agent's messages and tool calls, one per line. Falls back to the plaintext log for agents that write no session record. Needs a running daemon. Goes through the [pager](#environment-variables) when one is set and stdout is a terminal.

### `kontora sessions [flags] TICKET_ID`

Prints the files behind the ticket's runs: by default the agent's own session
JSONL, which is the transcript another tool would read. One row per history
row, in history order, then the files no row claims.

| Flag | Description |
|------|-------------|
| `--stage NAME` | Only this stage, and the annotation runs that borrowed its name. |
| `--run N` | Only this run number within the stage. Drops the stage log and the unattributed files, neither of which has a run number. |
| `--logs` | Print the stage log paths in place of the session files. Combines with `--events`. |
| `--events` | Print the activity sidecar paths in place of the session files. Combines with `--logs`. |
| `--all` | Print all three for every run. |

The columns are stage, run, artifact, path, and, when there is something to say
about the row, a reason or a note. The path column holds `-` when nothing on
this machine matches, so a fourth-field filter in `awk` gives the files that
exist. In the run column, `-` marks the stage log, which is per stage, and `?`
marks a file no run claims, including the session of the stage running now.

```text
plan       0  session  /Users/a/.claude/projects/-Users-a-projects-kontora/2f1e0c7a-….jsonl
implement  0  session  /Users/a/.kontora/logs/kon-q88f/pi-sessions/implement/01JC9….jsonl
implement  1  session  /Users/a/.kontora/logs/kon-q88f/pi-sessions/implement/01JC9….jsonl  resumed: same file as run 0
implement  2  session  -  no session recorded (run predates session_ref)
review     0  session  -  session file missing: no /Users/a/.claude/projects/*/9a2c….jsonl
ship       0  session  -  agent "shell-runner" writes no session
```

Three things worth knowing about the rows:

- **A resumed run shares its file with the run it continued.** Claude's
  `--resume` and pi's `--session` both append to the existing JSONL, so one
  file can back several runs. The second row above says so rather than
  pretending the run has a file of its own.
- **A run that predates the `session_ref` field cannot be resolved back.** For
  those tickets the command falls back to what is still on disk: a Claude
  stage's last run is recovered from its session record, and a pi stage's
  transcripts are listed with `?` in the run column, because nothing says which
  file belongs to which run. The rest of such a ticket is gone for good: Claude
  runs other than the last of each stage, the run each pi file belongs to, and
  any run by an agent that is neither Claude nor pi.
- **A run is never dropped.** A row with no path prints the reason instead,
  because a missing row reads as "this stage never ran".

`--logs` prints one row per stage, not per run: every run of a stage overwrites
`<stage>.log`, so it only ever holds the newest one. `--events` is per run.

Local only. Every path names a file on the machine that runs the command. In
remote mode the daemon's path either does not exist here or, if the caller runs
kontora too, holds another ticket's bytes.

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

### `kontora completion <shell>`

Prints a completion script for `bash`, `fish` or `zsh`. Each one completes the verbs, their flags, and, for the verbs that take one, a ticket ID. The ID list comes from `kontora ls --closed`, so closed tickets are offered too.

```bash
kontora completion fish > ~/.config/fish/completions/kontora.fish
kontora completion zsh  > ~/.zsh/completions/_kontora   # dir must be in $fpath before compinit
kontora completion bash > ~/.local/share/bash-completion/completions/kontora
```

See [Shell completions](configuration.md#shell-completions) for the per-shell install steps.

### `kontora version`

Prints the version.

## Remote mode

Setting `--url` or `KONTORA_URL` points every client command at a daemon over HTTP instead of the local files. See [Remote mode](https://github.com/worksonmyai/kontora#remote-mode) in the README for the daemon-side setup.

Supported: `ls`, `view`, `new`, `init`, `update`, `delete`, `run`, `pause`, `retry`, `cancel`, `done`, `move`, `skip`, `set-stage`, `note`, `summary`, `logs`, `activity`, `changes`, `stats`, `review`, `annotate`, `config`, `attach`.

Rejected, because they act on local files: `edit`, `search`, `archive`, `estimate-compaction`, `sessions`, `doctor`, `start`, `setup`.

Unaffected, because they touch neither the daemon nor a config file: `fmt`, `completion`, `version`, `help`, `phase-complete`.

Paths passed to `--path` name locations on the daemon host, not on the caller's machine.
