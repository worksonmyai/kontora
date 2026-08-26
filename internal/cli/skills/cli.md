# kontora CLI

Every verb is a flat top-level word: `kontora <verb> [flags] [arguments]`.
Flags use Go's `flag` package, so `-flag` and `--flag` both work.

Go's flag parser stops at the first positional argument. Put flags before the
free-form text of `new`, `note` and `summary`, or they are read as part of the
text.

Any verb that takes a `TICKET_ID` accepts a unique prefix of one: `kontora view
kon-q` finds `kon-q88f`. A prefix matching two tickets is an error naming both.
Matching is from the start of the id only.

Exit codes: 0 on success, non-zero on failure with the reason on stderr. A
search that matches nothing still exits 0.

## Common flags

- `--config PATH` — config file. Defaults to `$KONTORA_CONFIG`, then
  `./.kontora/config.yaml`, then `$XDG_CONFIG_HOME/kontora/config.yaml`, then
  `~/.config/kontora/config.yaml`.
- `--tickets-dir PATH` — overrides `tickets_dir`. Ignored in remote mode.
- `--url URL` — remote daemon URL. Non-empty switches on remote mode. Defaults
  to `$KONTORA_URL`.
- `--token TOKEN` — bearer token for the remote daemon. Defaults to
  `$KONTORA_TOKEN`.

## Remote mode

`KONTORA_URL` (or `--url`) points every client command at a daemon over HTTP
instead of at local files. Paths in `--path` then name directories on the
daemon host.

Supported: `ls`, `view`, `new`, `init`, `update`, `delete`, `run`, `pause`,
`retry`, `cancel`, `done`, `move`, `skip`, `set-stage`, `note`, `summary`,
`logs`, `activity`, `changes`, `stats`, `review`, `annotate`, `config`,
`attach`.

Rejected, because they act on local files: `edit`, `search`, `archive`,
`estimate-compaction`, `sessions`, `doctor`, `start`, `setup`.

Unaffected, because they read neither config nor daemon: `fmt`, `completion`,
`version`, `help`, `phase-complete`, `skills`.

## kontora ls

`kontora ls [flags]` — list tickets. On a terminal with no flags it opens the
kanban TUI; any filter, `--json`, `--static`, `--closed` or `--archived` makes
it print a table instead. Prefer `--json` when parsing.

- `--closed` — include done, cancelled and legacy `closed`.
- `--archived` — include archived.
- `--status STATUS` — only this status. Naming a hidden status shows it, so
  `--status done` needs no `--closed`.
- `--project NAME` — only this configured project.
- `--path PATH` — only this repository path.
- `--ready` — only `todo` kontora tickets whose deps are all closed.
- `--blocked` — only `todo` kontora tickets waiting on a dep.
- `--limit N` — print at most N.
- `--json` — print JSON.
- `--static` — print the table even on a terminal.

`--ready` and `--blocked` cannot be combined. `--json` prints an array of
objects with `id`, `title`, `status`, `kontora`, `path`, `project`, `pipeline`,
`stage`, `agent`, `created_at`, `started_at`, `completed_at`, `deps`, `links`,
`parent`, `ready` and `blockers`. It never carries the ticket body.

## kontora search

`kontora search [flags] QUERY` — match a Go regular expression against every
ticket file, frontmatter and body alike, and print the matching lines grouped
by ticket. Every status is searched, archived included. Local only.

Case follows ripgrep's smart-case rule. A query starting with `-` needs `--`
in front of it: `kontora search -- -race`.

- `-i` / `-s` — force case-insensitive / case-sensitive. Cannot be combined.
- `-F` — literal string, not a regex.
- `-l` — print matching ticket IDs only.
- `-m N` — at most N matching lines per ticket, default 5, `0` for all.
- `--body` — search the body only, not the frontmatter.
- `--json` — print JSON. Cannot be combined with `-l`.
- `--status`, `--project`, `--path`, `--pipeline`, `--agent` — narrow the set.

Exits 0 whether or not anything matched.

## kontora new

`kontora new [flags] TITLE` — create a ticket and print its id. Without
`--path` it uses the current git root; in remote mode `--path` is required.

- `--path PATH` — repository the ticket works in.
- `--pipeline NAME` — pipeline to run, or `none` for a standalone ticket.
- `--agent NAME` — agent to use, or `none` to skip the project default.
- `--branch NAME` — work branch name.
- `--base-branch NAME` — branch the work branch starts from.
- `--status STATUS` — `open` or `todo`. Defaults to `todo`.
- `--description-file PATH` — read the body from a file, `-` for stdin.
- `--quiet` — print only the new ticket id.

Use `--description-file` for anything longer than a line. The body is written
after a generated `# <title>` heading.

A ticket created with `--status open` is never visible as `todo`, so a daemon
with `auto_pick_up: true` cannot claim it before you finish editing it.

## kontora view

`kontora view [--body] TICKET_ID` — print the ticket's status, pipeline
position and body.

`--body` prints only the stored markdown after the frontmatter, unstyled and
unpaged, which is exactly what `kontora update --body-file` writes back.

## kontora edit

`kontora edit TICKET_ID` — open the ticket file in `$EDITOR`. Local only, and
interactive: never run it from an agent.

## kontora update

`kontora update TICKET_ID [flags]` — change frontmatter fields or the body. At
least one flag is required. An empty value, or `none` where noted, clears the
field.

- `--body-file PATH` — replace the body from a file, `-` for stdin.
- `--pipeline NAME` — set the pipeline (`""` or `none` clears it).
- `--path PATH` — set the repository path.
- `--agent NAME` — set the agent override (`""` or `none` clears it).
- `--branch NAME` — set the work branch (`""` clears it).
- `--base-branch NAME` — set the base branch (`""` clears it).

## kontora delete

`kontora delete TICKET_ID -f` — delete the ticket's markdown file. `-f` or
`--yes` is required. The worktree is not cleaned up; cancel the ticket first if
you want the normal cleanup path.

## kontora init

`kontora init TICKET_ID [flags]` — mark an existing ticket file `kontora: true`
so the daemon will run it. Without flags it prompts for the missing fields, so
always name them from a script or an agent.

- `--path PATH` — repository path. Required when the frontmatter has none.
- `--pipeline NAME` — pipeline, or `none` for a standalone ticket.
- `--agent NAME` — agent, or `none` to skip the project default.
- `--stage NAME` — starting stage. Defaults to the pipeline's first.
- `--status STATUS` — `open` or `todo`. Defaults to asking.

In remote mode `--pipeline` and `--path` are both required.

## kontora run

`kontora run TICKET_ID` — enqueue an `open` or `todo` ticket. Needs a running
daemon. A ticket whose deps are not all closed is reported as blocked, naming
them, rather than queued.

## kontora pause

`kontora pause TICKET_ID` — stop a running ticket and park it in `paused`. A
ticket that already closed cannot be paused.

## kontora retry

`kontora retry TICKET_ID` — reset a non-running ticket to `todo` with the
attempt counter cleared, and re-enqueue it.

## kontora done

`kontora done TICKET_ID` — move the ticket to `done`, stopping any running
agent and cleaning up its worktree.

## kontora cancel

`kontora cancel TICKET_ID` — move the ticket to `cancelled`, stopping any
running agent and cleaning up its worktree.

## kontora move

`kontora move TICKET_ID STATUS` — move a ticket to any status the config
allows: `open`, `todo`, `paused`, `human_review`, `done`, `cancelled`, or a
custom status from `statuses:`. This is the only way to reach `human_review` or
a custom status from the command line. `archived` is not accepted; use
`kontora archive`.

## kontora skip

`kontora skip TICKET_ID` — advance the ticket to the next stage of its
pipeline, or mark it done when it is already on the last one. Runs nothing.

## kontora set-stage

`kontora set-stage TICKET_ID STAGE` — move the ticket to a named stage of its
pipeline without running anything.

## kontora note

`kontora note TICKET_ID [TEXT]` — append a note under the body's `## Notes`
section. Reads the text from stdin when it is not given as an argument, so
`git log --oneline | kontora note kon-q88f` works.

This is how you leave something for the next stage to read: a stage prompt
containing `{{ .Ticket.Description }}` gets the whole body, notes included.

Each note is written with a byline carrying its timestamp, its author and a
4-character id: `**2026-03-06T12:00:00Z · claude · q88f**`. Running under the
daemon, the author defaults to `$KONTORA_AGENT`, the name of the agent this run
is, so you do not have to pass it.

| Flag | Description |
|------|-------------|
| `--author NAME` | Override the author. |
| `--reply-to NOTE_ID` | Reply to that note. One level only: replying to a reply is refused. |

Both are flags, so write them before the free-form text.

## kontora summary

`kontora summary TICKET_ID [TEXT]` — set the ticket's one-line summary. Reads
stdin when the text is not an argument. The daemon clears it when a new stage
starts, and each stage keeps its own copy on its `history` entry.

## kontora dep

`kontora dep TICKET_ID DEPENDENCY_ID` — make `TICKET_ID` wait on
`DEPENDENCY_ID`. Only the dependent ticket is written; the reverse edge
(`blocks`) is derived on read. A dependency that would close a cycle is
rejected and the error names the cycle.

## kontora undep

`kontora undep TICKET_ID DEPENDENCY_ID` — drop the dependency edge.

## kontora link

`kontora link TICKET_ID TICKET_ID...` — relate the first ticket to each of the
others, and not the others to each other. Both sides of each link are written.

## kontora unlink

`kontora unlink TICKET_ID TICKET_ID...` — remove the relation between the first
ticket and each of the others, on both sides.

## kontora archive

`kontora archive --days N [flags]` — mark every `done`, `cancelled` or legacy
`closed` ticket whose file has not been modified for at least N days as
`archived`, which hides it everywhere and keeps the file. Local only.

- `--days N` — required, positive. Age threshold on the file's mtime.
- `--dry-run` — list what would be archived, write nothing.
- `--path PATH` / `--project NAME` — limit to one repository. Not combinable.
- `--status STATUS` — one of `done`, `cancelled`, `closed`.
- `-y`, `--yes` — skip the confirmation prompt. Required when stdin is not a
  terminal, so an agent must pass it or the run fails.

## kontora logs

`kontora logs [--stage NAME] TICKET_ID` — print the agent's plaintext log.
Without `--stage` it shows the most recent one. Paged only on a terminal.

## kontora activity

`kontora activity [--stage NAME] [--run N] TICKET_ID` — print one stage run's
structured transcript: the agent's messages and tool calls, one per line.
Falls back to the plaintext log for agents that write no session record. Needs
a running daemon.

## kontora sessions

`kontora sessions [flags] TICKET_ID` — print the files behind the ticket's
runs, one row per history row. Columns are stage, run, artifact, path, and a
reason when the path is `-`. Local only: every path names a file on this
machine.

- `--stage NAME` — only this stage.
- `--run N` — only this run number within the stage.
- `--logs` — stage log paths instead of session files.
- `--events` — activity sidecar paths instead of session files.
- `--all` — all three for every run.

In the run column, `-` marks the per-stage log and `?` a file no run claims.

## kontora changes

`kontora changes TICKET_ID` — print the commits on the ticket's branch and the
per-file line counts against its base. Needs a running daemon.

## kontora stats

`kontora stats [flags]` — print throughput, median cycle time, first-pass rate,
and per-stage, per-agent and per-project breakdowns. Needs a running daemon.

- `--range RANGE` — `1d`, `1w`, `30d`, `90d` or `all`. Defaults to `90d`.
- `--project NAME` — only this configured project.
- `--pipeline NAME` — only this pipeline.

## kontora attach

`kontora attach [--rw] TICKET_ID` — attach to the running ticket's terminal:
the tmux window locally, a WebSocket in remote mode. Read-only unless `--rw`.
Interactive: it takes over the terminal, so never run it from an agent.

## kontora review

`kontora review TICKET_ID` — open the ticket's diff in Plannotator. The daemon
spawns the binary, so in remote mode the window opens on the daemon host. The
ticket has to be in `human_review`.

## kontora annotate

`kontora annotate TICKET_ID` — open the ticket's markdown in Plannotator, on
the daemon host in remote mode.

## kontora phase-complete

`kontora phase-complete TICKET_ID --completed TEXT --next TEXT` — signal a
boundary between two ticket phases from inside a checkpointing run. Agents call
it, not people. It reads no config and talks to no daemon.

Without `KONTORA_CHECKPOINT_FILE` in the environment it records nothing, says
checkpoint compaction is off for the run, and exits 0.

## kontora estimate-compaction

`kontora estimate-compaction [flags]` — project token savings from checkpoint
compaction by replaying completed pi session traces. Read-only, local only.

- `--logs-dir PATH` — logs directory. Defaults to the config's `logs_dir`.
- `--stage NAME` — stage to analyse. Defaults to `implement`.
- `--thresholds LIST` — comma-separated token thresholds.
- `--top N` — sessions in the report. Defaults to 20.

## kontora start

`kontora start [--address HOST] [--port PORT]` — run the daemon in the
foreground. One instance per config directory; a second exits on the file lock.
Local only, and it does not return.

## kontora setup

`kontora setup [--agent]` — write the config file. Plain `setup` runs an
interactive wizard. `setup --agent` prints a brief for a coding agent and
writes nothing itself. Local only.

## kontora doctor

`kontora doctor` — check the config, `git`, `tmux`, each agent binary, each
project's repository, Plannotator and the web port. Exits non-zero when a hard
prerequisite fails; missing directories and an occupied port are warnings.
Local only.

## kontora config

`kontora config` — print the effective configuration with defaults applied and
`web.token` replaced by a placeholder. In remote mode it prints the daemon's
pipelines, agents and custom statuses instead.

`kontora config edit` opens the config in `$EDITOR`. Interactive: never run it
from an agent, and it is classified as a change.

## kontora skills

`kontora skills list` — print the reference topics, one line each.
`kontora skills list TOPIC` — print that topic's section headings.
`kontora skills show TOPIC` — print the whole topic as markdown.
`kontora skills show TOPIC SECTION` — print one section.

SECTION matches a heading exactly first, then as a unique case-insensitive
substring, so `kontora skills show cli new` prints `## kontora new`. An
ambiguous match prints the candidates and exits 1; so does an unknown topic.

Reads neither config nor ticket store, so it works in remote mode and before
`kontora setup`.

## kontora fmt

`kontora fmt` — read Claude stream-json on stdin and print it as readable text.
Takes no flags and talks to nothing.

## kontora completion

`kontora completion <bash|fish|zsh>` — print a completion script.

## kontora version

`kontora version` — print the version.

## kontora help

`kontora help` — print the command list. `kontora <verb> -h` prints that verb's
own usage and exits without doing anything. Write it with nothing after the
flag: `kontora note kon-1 -h` appends a note reading `-h`, because `note`,
`summary` and `new` take free-form text as their last argument and parse flags
only once, so the parser has already stopped at the ticket id. The other verbs
re-parse after every positional, so `kontora delete kon-1 -h` does print usage,
but the safe form is the same for all of them.
