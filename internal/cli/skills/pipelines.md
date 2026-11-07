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
A successful supervisor run goes to `human_review`; Plannotator feedback starts
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
- `kontora retry ID` — reset a non-running ticket to `todo` with the attempt
  counter cleared, and re-enqueue it. This is what resumes a `paused` ticket.
- `kontora pause ID` — stop the running agent and park the ticket in `paused`.
- `kontora run ID` — enqueue an `open` or `todo` ticket.

Daemon/API skip queues dependency-ready work even with `auto_pick_up: false`.
Disk-only skip does not enqueue; use `kontora run TICKET_ID` afterward when
automatic pickup is off.

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
stage. Kontora adds the reason as a system note. If Pi wrote no session file,
Kontora uses the process exit code and output patterns instead.

A daemon restart mid-stage puts the ticket back to `todo` and schedules the
stage again. For `claude` and `pi` the run resumes the interrupted conversation
and gets `resume_prompt` instead of the stage prompt, so the agent continues
rather than starting over on top of its own half-finished work.
