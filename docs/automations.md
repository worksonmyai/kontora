# Automations

An automation runs an agent with a saved prompt, repository, and schedule. Each run has its own identity, terminal, and log.
Automations do not create tickets or inherit ticket pipelines. They share the daemon's agent capacity with tickets.
Enabled schedules and manual runs work with `auto_pick_up: false`.

## Creating and controlling an automation

Open Automations in the dashboard, use `kontora automation new`, or edit `automations:` in the daemon's YAML configuration.
The dashboard and [HTTP API](api.md#automations) write the same configuration file.
Omitting `enabled` in YAML or API requests leaves scheduling disabled. The CLI also defaults to disabled.
Review the dashboard's Enabled setting, schedule preview, and agent permissions before saving unattended runs.

The dashboard list (`#/automations`) supports state and text filters.
Detail (`#/automations/<id>`) shows run history, the prompt, and the current terminal.
Use `#/automations/new` to create a definition or `#/automations/<id>/edit` to edit one.
Duplicate creates a separate definition. Delete removes the configuration entry and stops unfinished work; historical runs and logs remain.

Pause disables future scheduled runs. It does not cancel accepted runs.
Run now also works while paused. Stop targets a specific run, including a queued run.
Resume selects a future schedule slot instead of replaying paused time.
Use `kontora attach <automation-id>` for the current terminal. Attachment is read-only unless you select read-write mode.

Prompts do not enforce permissions. For example, “do not edit files” does not make an agent read-only.
Configure the agent's permissions for the task.

## Configuration

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
    model: ""
    schedule: "0 9 * * 1-5"
    timezone: Europe/Berlin
    workspace: worktree
    overlap: skip
    timeout: 30m
    enabled: false
```

The map key is a lowercase kebab-case name, such as `morning-report`. The server assigns a separate ID: `aut-` followed by 32 lowercase hexadecimal characters.
Required fields are `name`, `prompt`, `path`, and `schedule`. `agent` defaults to `default_agent` when loading YAML.
A model override requires an agent that supports model selection.
Defaults are `timezone: UTC`, `workspace: worktree`, `overlap: skip`, and `timeout: 30m`.
The timeout must be a positive duration.

Configuration reloads validate the complete file before applying changes.
Dashboard edits preserve unrelated YAML comments and follow a configuration symlink without replacing the link.
Entries with anchors, aliases, or merge keys require a direct configuration edit.
Revision checks and conditional writes refuse stale edits rather than overwrite an external change.

## Schedules

`schedule` accepts numeric five-field cron or a positive whole-minute duration:

```yaml
schedule: "0 9 * * 1-5"  # 09:00, Monday through Friday
timezone: Europe/Berlin
```

```yaml
schedule: 90m
timezone: UTC
```

The minimum interval is one minute. `1m`, `90m`, and `2h` are valid; `30s` and `90s` are invalid.
Cron uses minute, hour, day of month, month, and day of week. Named fields, seconds fields, and descriptors such as `@daily` are invalid.
Use an IANA timezone such as `Europe/Berlin`, `America/New_York`, or `UTC`. `Local` is invalid.
Cron follows timezone clock changes. A nonexistent local time has no run; repeated local times can produce two distinct instants.
Intervals use elapsed time rather than local wall time.
Preview returns four future timestamps without changing configuration.

Creating, enabling, or rescheduling a definition selects a future slot.
Other edits preserve the schedule cursor. Cursor advancement does not change the definition's editable revision.
Recorded schedule receipts prevent the same slot from running twice after a retry or restart.
A slot more than one minute late is recorded as skipped. The daemon advances to a future slot without replaying every missed interval.
The schedule controls acceptance, not the exact launch time: a run can wait for agent capacity.

For a one-time ticket delay, use [`scheduled_at`](tickets.md#scheduled-pickup) instead. It promotes an existing ticket to `todo`.

## Overlap

The overlap policy applies to unfinished runs from the same automation. It covers manual and scheduled requests.
Only one run executes at a time:

| Policy | New request while a run is unfinished |
|--------|--------------------------------------|
| `skip` | Record the new run as skipped. |
| `queue` | Keep one pending run. Record further requests as skipped while that pending slot is occupied. |
| `replace` | Stop the current run, skip older pending replacements, and keep the newest request. |

A replacement waits until termination of the previous run is confirmed.
Uncertain termination blocks another launch, even after an overlap policy change.

## Workspaces, prompts, and results

`workspace: worktree` creates a fresh worktree at the repository's current commit.
The branch is `auto/<key>-<n>`. Kontora removes a clean, unchanged worktree after the run.
Worktrees with commits or uncommitted changes remain available for inspection.
`workspace: repo` runs directly in the repository checkout. Its changes remain there.
Automation hooks receive automation and run identities without a synthetic ticket identity.

Prompts use Go templates with these contexts:

| Context | Available fields |
|---------|------------------|
| `.Automation` | `ID`, `Key`, `Name`, `Path`, `Agent`, `Model`, `Schedule`, `Timezone`, `Workspace`, `Overlap`, `Timeout`, `Enabled` |
| `.Run` | `Number` (also `N`), `InvocationID`, `Branch`, `Date`, `Trigger`, `ScheduledFor`, `AcceptedAt`, `StartedAt`, `CompletedAt`, `Outcome`, `Result`, `ExitCode`, `Cost` |
| `.LastRun` | The same run fields for the previous executed terminal run, excluding skipped runs. Zero values before the first execution. |

`{{ file "context.md" }}` reads from the run's working directory.
Ticket and epic template contexts are not available.
A run freezes its definition, agent command, environment, hooks, timeout, and workspace paths when accepted.
Later configuration changes apply to new runs.

The agent must publish an explicit result with `kontora result --input-file <absolute-json-file> <automation-id>`.
The JSON contains `outcome` (`complete` or `blocked`), `summary`, and `next_step` when blocked.
A zero process exit alone is not completion. Missing or invalid results, blocked results, nonzero exits, cancellation, and timeouts produce failed runs.

## History, logs, and recovery

Runs have a per-key number, an invocation identity, and a revision.
Numbers continue after deletion and recreation of the same key.
Phases are `queued`, `preparing`, `running`, `stopping`, `terminal`, and `recovery_required`.
Only terminal runs have an outcome: `complete`, `failed`, or `skipped`.
Stopping is a request, not proof that the process stopped.

History shows manual or scheduled triggers, timestamps, logical and observed exit codes, result text, and estimated cost.
Cost is nullable. Missing usage or pricing is unknown, not zero.
Definition statistics report priced and tracked runs so partial spend coverage remains visible.

Structured logs combine Kontora lifecycle events with agent text and tool activity.
Download raw logs from run history. The server resolves the live terminal target; clients cannot select arbitrary sessions.

At restart, Kontora inspects unfinished runs and their saved terminal identities.
Interrupted execution is not automatically launched again. Uncertain termination remains `recovery_required` and blocks overlap.
Inspect the run and use Stop to request recovery. Do not remove state files to bypass that block.
Hook termination is recorded separately from the agent's tmux window. If a crash leaves hook termination unconfirmed, recovery retains the reservation.
Inspect those hook processes on the daemon host. An absent tmux window does not prove that the hooks stopped.
Automations cannot guarantee exactly-once external effects.

## Storage and backup

Definitions live in the daemon's YAML configuration. Runtime state is machine-local and scoped to that configuration path:

```text
~/.kontora/automations/<scope-hash>/state.json
```

`KONTORA_AUTOMATIONS_ROOT` overrides the state root. Keep it stable across restarts.
A daemon without a configuration path falls back to its primary ticket directory as the scope.
One local daemon owns a scope's lock. A separate lock protects the automation log namespace from another owner.
Lock contention or invalid snapshots makes Automations unavailable without disabling ordinary tickets.
State directories use `0700`; snapshots, lock files, and run artifacts use private file permissions.
Frozen prompts remain plaintext inside private state and artifacts.

Run artifacts are under `<logs_dir>/automations/<key>/<n>/`.
The composed log is `<logs_dir>/automations/<key>/<n>.log`.
History is retained without automatic pruning. Pagination does not reduce state size.

Before backup, finish or stop active agents, then stop the daemon.
Copy the configuration, automation state, logs, and retained worktrees together. Preserve their private permissions.
Restoring YAML alone does not restore run numbers, receipts, or recovery evidence.
Local locks do not coordinate separate machines or copied state roots.
