# Pipelines and runs

A pipeline is an ordered list of steps. Each step names a stage and the agent
that runs it, and says what happens on success and on failure. A ticket
references a pipeline by name in its `pipeline` frontmatter field.

A ticket with no pipeline runs standalone: one agent invocation with the
default agent, no stages.

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
| `{{ .Ticket.Description }}` | The whole body, notes included. |
| `{{ .Ticket.FilePath }}` | Absolute path to the ticket's markdown file. |
| `{{ file "PLAN.md" }}` | Contents of a file, relative to the worktree. |
| `{{ plannotatorReview }}` | Feedback from the last Plannotator code review. |
| `{{ plannotatorAnnotations }}` | Pending Plannotator annotations on the ticket. |

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

The daemon runs at most `max_concurrent_agents` of them at once, taking `todo`
kontora tickets whose deps are closed, oldest `created` first.

An assistant chat turn is not a run: it is a headless process, takes no
concurrency slot, and belongs to no ticket.

## Moving a ticket by hand

- `kontora skip ID` — advance to the next stage without running the current
  one, or mark the ticket done when it is already on the last stage.
- `kontora set-stage ID STAGE` — jump to a named stage of the ticket's
  pipeline. Nothing runs; the ticket stays in whatever status it is in.
- `kontora retry ID` — reset a non-running ticket to `todo` with the attempt
  counter cleared, and re-enqueue it. This is what resumes a `paused` ticket.
- `kontora pause ID` — stop the running agent and park the ticket in `paused`.
- `kontora run ID` — enqueue an `open` or `todo` ticket.

`skip` and `set-stage` both need the ticket to have a pipeline.

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

A daemon restart mid-stage puts the ticket back to `todo` and schedules the
stage again. For `claude` and `pi` the run resumes the interrupted conversation
and gets `resume_prompt` instead of the stage prompt, so the agent continues
rather than starting over on top of its own half-finished work.
