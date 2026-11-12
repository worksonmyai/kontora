# Pipelines and runs

A pipeline is an ordered list of steps. Each step names a stage and the agent
that runs it, and says what happens on success and on failure. A ticket
references a pipeline by name in its `pipeline` frontmatter field.

An ordinary ticket with no pipeline runs standalone: one agent invocation with
the default agent, no stages. An epic has no pipeline. A passive epic never
runs; an explicitly activated agent epic runs a standalone supervisor in its own worktree.

## Default selection

For ordinary tickets, creation and initialization resolve pipelines in this order:

1. Explicit request, including `none` for standalone execution.
2. Stored nonempty pipeline, during initialization only.
3. Project `epic_child_pipeline`, for a child of any actual epic, passive or agent.
4. Project `pipeline`.
5. Global `default_pipeline`.
6. Standalone execution.

Missing or ordinary-ticket parents do not qualify for the child default.
Kontora writes the result into frontmatter. Existing assignments are not migrated
after config changes, reparenting or detail-path edits. Operators must retarget
them explicitly. `none` is not durable provenance: later initialization can fill
the blank unless the caller opts out again. Agent resolution stays independent.

Upgrade all clients and daemon binaries that read the config before adding
`epic_child_pipeline`. Older strict-YAML loaders reject the key. The setting must
name a configured pipeline; `none` remains a ticket-level opt-out.

## Stages

A stage is a named prompt template plus its timeout, model and reasoning
effort. It says *what* an agent should do; the pipeline step says *when* and
*with which agent*.

```yaml
stages:
  code:
    prompt: |
      {{ .Ticket.Description }}
    timeout: 30m
```

Prompt templates are Go `text/template` with these expressions:

| Expression | Description |
|------------|-------------|
| `{{ .Ticket.ID }}` | Ticket id. |
| `{{ .Ticket.Title }}` | The body's first `# Heading`. |
| `{{ .Ticket.Description }}` | This ticket's body and notes, without its epic's brief. |
| `{{ .Ticket.FilePath }}` | Absolute path to the ticket's markdown file. |
| `{{ file "PLAN.md" }}` | Contents of a file, relative to the worktree. |
| `{{ plannotatorReview }}` | Feedback from the last Plannotator code review. |
| `{{ plannotatorAnnotations }}` | Pending Plannotator annotations on the ticket. |

For a child of an epic, Kontora appends the epic id, title and a command to read
its brief. This does not require a template variable and also applies to
standalone and resumed runs. The brief and notes are not inlined; the agent
must read them with `kontora view --body EPIC_ID`. Missing parents and parents
that are not epics add no context. An empty rendered prompt stays empty.

`file` is how stages communicate. Every stage of a ticket shares one git
worktree, so a plan stage writes `PLAN.md` and a code stage reads it with
`{{ file "PLAN.md" }}`. There is no other channel between stages except the
ticket body, which `kontora note` appends to — signed with the agent name the
daemon exports as `$KONTORA_AGENT`, so the next stage can see who wrote what.

## Pipeline steps

```yaml
pipelines:
  default:
    - stage: plan
      agent: claude
      on_success: next
      on_failure: retry
      max_retries: 2
    - stage: code
      agent: claude
      on_success: human_review
      on_failure: pause
```

| Field | Required | Values |
|-------|----------|--------|
| `stage` | yes | A stage name from `stages:`. |
| `agent` | yes | An agent name from `agents:`. |
| `on_success` | yes | `next`, `done`, `human_review`, or a custom status. |
| `on_failure` | yes | `retry`, `back`, `pause`, `human_review`, or a custom status. |
| `max_retries` | no | Retry attempts, default 0. Only read when `on_failure: retry`. |

`on_success: next` advances to the next step and sets the ticket back to `todo`
so the scheduler picks it up again. `on_failure: back` returns to the previous
step and is not allowed on the first one. The last step must not have
`on_success: next`.

A stage cannot appear twice in the same pipeline.

## What a run is

One run is one agent process for one stage of one ticket, started in the
ticket's git worktree, in a detached tmux window. A stage that retries produces
several runs; the run number counts them within the stage.

The daemon runs at most `max_concurrent_agents` workers at once. Epic supervisors
use a separate `max_concurrent_epic_supervisors` pool, default `4`. This allows
four additional agent processes. The queue takes `todo` kontora tickets whose
deps are closed, oldest `created` first among tickets whose pool has capacity.
Unconfirmed termination retains the reservation in the pool that started it.

Activate an epic explicitly with `kontora run EPIC_ID` or init with `--status todo`.
The epic and children must share one Git repository. Children keep their own
pipelines, branches and worktrees. Pausing the epic stops only its supervisor.
A complete supervisor result with clean execution, confirmed termination, and
successful final validation goes to `human_review`; Plannotator feedback starts
supervisor rework. The epic records supervisor history, logs, activity and sessions.
See `kontora skills show tickets Epics` for Git and approval limits and upgrade requirements.

An assistant chat turn is not a run: it is a headless process, takes no
concurrency slot, and belongs to no ticket.

## Moving a ticket by hand

Supervisor prompts prohibit changing, clearing or replacing child pipelines and
editing pipeline definitions, stage definitions or project pipeline defaults.
Supervisors must create children without `--pipeline`. They must not bypass a
workflow through `init`, stage skipping, direct file edits or pause/retry sequences.
If configured work conflicts with their authority, they must report the conflict,
request an operator decision and leave that work unstarted. They must not substitute
another pipeline. These are prompt instructions, not runtime enforcement.
A pipeline default does not authorize commits or publication; `commit-no-push`
still contains a commit stage.

The following commands remain available to ordinary operators:

- `kontora skip ID` — intermediate skip sets the next stage to `todo`.
  Final skip marks the ticket done without recording a successful run.
- `kontora set-stage ID STAGE` — select a named pipeline stage without retrying.
  Status and attempt stay unchanged; a `todo` ticket remains eligible for pickup.
- `kontora retry ID` — reset an initialized ticket to `todo` with the attempt
  counter cleared. Refused for `in_progress`, `todo`, `archived`, and passive epics.
  Use it to prepare a `paused` ticket to run again.
- `kontora pause ID` — stop the running agent and park the ticket in `paused`.
- `kontora run ID` — enqueue an `open` or `todo` ticket.

Daemon/API retry and skip queue dependency-ready work even with `auto_pick_up: false`.
Local CLI retry and skip only update the ticket file; they do not enqueue.
Use `kontora run TICKET_ID` afterward when automatic pickup is off.

`skip` and `set-stage` require a pipeline and stopped execution. Pause first,
wait for termination, and close any active Plannotator review. A persisted
`paused` status does not prove teardown finished. The daemon refuses stage
changes until execution stops; a disk-only CLI cannot observe daemon teardown.

## Where the files are

Defaults; the config can move all three.

| What | Path |
|------|------|
| Tickets | `~/.kontora/tickets/<id>.md` |
| Worktrees | `~/.kontora/worktrees/<repo>/<id>/` |
| Stage log | `~/.kontora/logs/<id>/<stage>.log` |
| Activity sidecar | `~/.kontora/logs/<id>/<stage>.<run>.events.json` |
| Assistant chats | `~/.kontora/logs/assistant/<chat-id>/` |

`kontora logs ID` prints the stage log and `kontora activity ID` the structured
transcript. `kontora sessions ID` prints the paths themselves, including the
agent's own session JSONL, which for claude lives under `~/.claude/projects/`
and not under `logs_dir`.

Every run of a stage overwrites `<stage>.log`, so that file only ever holds the
newest one. The activity sidecar is per run.

## Failures

A stage fails when the agent exits non-zero, when it exceeds the stage timeout,
or when its output matches one of the agent's `failure_patterns` even on a
clean exit. `last_error` on the ticket holds the reason. What happens next is
the step's `on_failure`.

After a clean exit, Kontora also checks Claude and Pi session records for
structured failures. A synthetic Claude API error, or a final Pi assistant
message with an `error` or `aborted` stop reason, pauses the ticket before
pipeline evaluation. The failure does not consume a retry or advance the
stage. Kontora records the reason in `last_error` and failed history. If Pi wrote
no session file, Kontora uses the process exit code and output patterns instead. Result validation
still applies.

After a daemon restart mid-stage, all ticket executions first follow the
reconciliation rules below. Recovery can pause a ticket rather than schedule
replacement work. When recovery permits conversation reuse, `claude` and `pi`
resume the interrupted conversation with `resume_prompt` instead of the stage
prompt. Otherwise the agent starts fresh on top of its half-finished work.

## Ticket results and recovery

Every ticket execution must submit invocation-bound `complete` or `blocked`, with
no opt-out. This includes pipeline stages, standalone workers, passive-epic
children, activated supervisors, and their children on fresh, resumed, and rework
runs. Annotation-only runs, dashboard assistants, and summary generation are
excluded. Automations keep their existing result contract. Result instructions
apply independently of a custom `system_prompt`. See
`kontora skills show cli 'kontora result'` for schema, environment, and transports.

Actual epic membership remains snapshotted for local-authority instructions and
recovery, not inferred from display order or whether the supervisor is running.
Within verified epic scope, local-authority and escalation instructions also apply
on fresh, resumed, and rework runs independently of `system_prompt`.
Ordinary workers remain workers. Reporting grants no new commit, merge, rebase,
or publication authority. Stored pipelines and review stages remain unchanged.

Complete is the agent's report that its current stage is complete. Independent
tests, hooks, review stages, and human review remain responsible for acceptance.
A review stage can complete while fixes remain for later stages.

A blocked result, or a missing/malformed report after a clean exit, pauses the
same stage, preserving `attempt` and the worktree. It never enters pipeline
`retry` or `back` policies. Complete cannot override interruption,
execution/provider failures, hooks, or final validation, including epic child checks.
An early submitted blocked result prevents resume-to-fresh fallback.
Each physical launch gets a fresh invocation identity.

Waiting for a question's answer is not terminal blockage. Report blocked only
when execution must stop. `ls --blocked` still selects dependency-blocked `todo`
tickets, not paused executions.

The daemon persists active invocation metadata before launch independently of
optional conversation resume records, including for ordinary workers. Startup
and stale-self-claim recovery reconcile journals, recorded scope, reports, and
finalization for all ticket executions before replacement:

- Check owned legacy ticket windows without journals too, including paused or
  `todo` tickets. Preserve operator-selected status until termination is confirmed.
- Confirm termination first. If uncertain, retain the reservation and prevent
  branch reuse; failed cleanup or persistence must not enqueue work. A missing
  tmux window is not proof that arbitrary detached descendants stopped.
- No terminal report: resume the same stage under a fresh identity after confirmed
  termination. A complete report without confirmed finalization also resumes for
  inspection; it cannot advance from the report alone.
- Blocked or malformed report: pause with its reason, preserving stage, `attempt`,
  and worktree.
- Persist a finalization marker before hooks and a replayable transition after
  hooks and final validation, including applicable child checks. Replay the transition
  idempotently, using invocation identity to prevent duplicate history, without rerunning hooks.
  Unknown hook effects pause for inspection rather than guessing.
- Retire metadata only after atomic ticket replacement succeeds. Foreign claims
  and explicit operator lifecycle changes take precedence.

Terminal reporting suppresses further checkpoint continuation; it does not kill
the agent. Normal shutdown and termination confirmation must finish. Recovery
covers daemon-process interruption, not exactly-once arbitrary shell effects or
host-power-loss durability. Metrics are emitted once per logical run within a
live daemon, not guaranteed exactly once across restart.

All Pi ticket executions require TUI mode inside tmux. Their launches reject
`--print`/`-p` and `--mode json`/`rpc`: Pi's non-TUI abort does not clear queues.
Dashboard assistant modes are unchanged.

`KONTORA_EXIT_CODE` remains runner exit status. `KONTORA_RUN_OUTCOME` gives hooks
the pre-hook semantic outcome; later hooks and final validation can reject completion.
History's optional `run_outcome`, `outcome_reason`, and `invocation_id` describe
the final outcome separately. Legacy rows without semantic outcomes remain
unknown/unverified task outcomes, even with exit zero; do not reinterpret their prose.
Standalone ticket executions with real history are counted once, not again as
synthetic legacy runs.

Save escalation answers immediately. Saving an answer, reading a notification,
or dismissing attention does not restart execution. Retry requires explicit
operator action and inspection of saved decisions, applicable child state, branches,
worktrees, dirty files, and interrupted Git operations. Do not silently weaken
requirements or replace required real-device evidence with emulation.

Upgrade CLI and daemon together under operator control. Custom agents must
support the result transport before running tickets. Verify effective Pi/Claude
instructions before enabling autonomous local commits. Do not restart live
executions or migrate existing ticket statuses as part of rollout. Authority
limits are agent instructions, not an OS publication boundary or independent
proof of reported completion.
