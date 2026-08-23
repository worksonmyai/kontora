# Configuration Reference

Kontora reads configuration from a YAML file. It checks these paths in order: `.kontora/config.yaml` in the current directory, then `$XDG_CONFIG_HOME/kontora/config.yaml` (or `~/.config/kontora/config.yaml` if unset). Override with `--config`. Unknown fields are rejected. See also: [Ticket Format](tickets.md).

## Creating the config

```bash
kontora setup           # interactive wizard
kontora setup --agent   # print instructions for a coding agent
```

`kontora setup` writes a starter config with two pipelines when the resolved
path holds no file. An existing config is never overwritten: the command prints
its path and the two ways to change it.

`kontora setup --agent` writes nothing. It prints a plain-text brief for a
coding agent: the resolved config path, whether the file is missing, valid, or
invalid, the validation error when there is one, and the symlink target when the
path is a link. The brief is embedded in the binary, so it describes the schema
that version accepts. It never prints the config file itself, so no token or
environment value can leak into the output; a validation error can still quote
the names, paths, and patterns it rejected. Use it to create the first config or
to add agents, stages, pipelines, and projects later.

Both forms are local-only and take no positional arguments. `--agent` here is a
boolean, unlike the agent-name `--agent` on `kontora new` and `kontora init`.
The wizard needs a terminal on stdin and stdout; without one it exits non-zero
and points at `kontora setup --agent`.

After setup:

```bash
kontora doctor   # validate config, tools, agent binaries, port
kontora start    # start the daemon; runs until Ctrl-C
```

Then, in another terminal:

```bash
kontora new --path ~/projects/myrepo "Add a health check endpoint"
```

## Minimal example

```yaml
tickets_dir: ~/.kontora/tickets

agents:
  claude:
    binary: claude

stages:
  code:
    prompt: Write code.

pipelines:
  default:
    - stage: code
      agent: claude
      on_success: human_review
      on_failure: pause
```

## Full example

```yaml
tickets_dir: ~/.kontora/tickets
branch_prefix: kontora
worktrees_dir: ~/.kontora/worktrees
logs_dir: ~/.kontora/logs
editor: nvim
max_concurrent_agents: 3

web:
  enabled: true
  host: 127.0.0.1
  port: 8080

agents:
  claude-sonnet:
    binary: claude
    args: ["--dangerously-skip-permissions", "--model", "sonnet"]
  claude-opus:
    binary: claude
    args: ["--dangerously-skip-permissions", "--model", "opus"]

stages:
  code:
    prompt: |
      {{ .Ticket.Description }}
    timeout: 30m
  implement:
    prompt: |
      {{ .Ticket.Description }}

      Do NOT commit or push. Only implement the code and run tests.
    timeout: 60m
  review:
    prompt: |
      Review the code changes for this ticket. Check for:
      - Correctness and edge cases
      - Code quality and maintainability
      - Test coverage

      Write all review results to the ticket {{ .Ticket.ID }}
    timeout: 30m
  fix-review:
    prompt: |
      Read the code review from the notes of {{ .Ticket.ID }}

      If there are issues listed, fix all of them and run tests.
    timeout: 30m
  commit:
    prompt: |
      Commit uncommitted files. This is the ready and reviewed
      implementation of the ticket {{ .Ticket.ID }}.

      You have full authorization to commit.
      Do not ask for confirmation — just do it.
    timeout: 5m

pipelines:
  default:
    - stage: code
      agent: claude-sonnet
      on_success: human_review
      on_failure: pause

  implement-review-commit:
    - stage: implement
      agent: claude-sonnet
      on_success: next
      on_failure: pause
    - stage: review
      agent: claude-sonnet
      on_success: next
      on_failure: retry
      max_retries: 1
    - stage: fix-review
      agent: claude-sonnet
      on_success: next
      on_failure: retry
      max_retries: 1
    - stage: commit
      agent: claude-sonnet
      on_success: human_review
      on_failure: retry
      max_retries: 1
```

## Top-level fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `tickets_dir` | no | `~/.kontora/tickets` | Directory containing ticket markdown files. Overridden by `$TICKETS_DIR`, then `$KONTORA_TICKETS_DIR`, then `--tickets-dir`: for this one field the environment beats the file (see [environment variables](cli.md#environment-variables)). |
| `branch_prefix` | no | `kontora` | Git branch prefix. A project can override it (see [projects](#projects)). |
| `branch_naming` | no | `mode: slug` | How the daemon names a ticket with an empty `branch` field (see [branch naming](#branch-naming)). |
| `worktrees_dir` | no | `~/.kontora/worktrees` | Where git worktrees are created. |
| `logs_dir` | no | `~/.kontora/logs` | Where agent output logs are stored. |
| `editor` | no | `$EDITOR` or `vi` | Editor for `kontora edit`. Falls back to `$EDITOR`, then `vi`. |
| `pager` | no | none | Pager for `kontora view`, `logs` and `activity`, split on whitespace. Used only when stdout is a terminal. `$KONTORA_PAGER`, `$TICKET_PAGER` and `$PAGER` all outrank it, and it has no effect in remote mode, which reads no config. |
| `default_agent` | no | (inferred) | Agent used for tickets without a pipeline. Defaults to `claude` if an agent with that name exists, otherwise inferred when there is exactly one agent. Must be set explicitly when multiple agents are defined and none is named `claude`. |
| `max_concurrent_agents` | no | `3` | Maximum number of agents running simultaneously. |
| `instance_name` | no | `os.Hostname()` | Identifies this daemon when several run against one synced `tickets_dir`. Written to a ticket's `claimed_by` on pickup so daemons don't steal or kill each other's work (see [multi-machine tickets](tickets.md#running-on-multiple-machines)). Falls back to `default` if the hostname can't be read. Two machines that share a hostname must set this explicitly, or the protection can't tell them apart. |
| `tmux_session` | no | `kontora` | The tmux session the daemon puts agent windows in. Allowed characters are `A-Z a-z 0-9 _ -`, 1 to 64 of them, and the name cannot start with `-`. Set a distinct value per daemon when you run more than one on a machine: startup cleanup, `kontora attach`, the TUI, and the web terminal are all scoped to this session, and two daemons sharing it can also signal each other's agents through the tmux `wait-for` channel when their ticket IDs collide. |
| `statuses` | no | — | Extra parked statuses beyond the built-ins. Agents can park tickets here via `on_success`/`on_failure`. |
| `projects` | no | — | Per-repository pipeline, agent, and branch naming defaults (see [projects](#projects)). |
| `environment` | no | — | Map of environment variables to set for all agent processes. |
| `hooks` | no | — | Commands run at a ticket's lifecycle events (see [hooks](#hooks)). |
| `resume_prompt` | no | (built-in) | Prompt sent to an agent whose stage a daemon restart interrupted, in place of the stage prompt (see [resuming after a restart](#resuming-after-a-restart)). Same template fields as a stage prompt. |
| `annotation_prompt` | no | (built-in) | Prompt sent to the run that rewrites a ticket from submitted Plannotator annotations (see [plannotator](#plannotator)). Same template fields as a stage prompt. |
| `summary_model` | no | — | Model the ticket-level summary pass runs on, resolved against the agent that ran the last stage. Same two forms as a stage's `model` (see [stages](#stages)). |
| `summary_effort` | no | — | Reasoning effort that same pass runs on, resolved the same way. It overrides the agent's own `effort` (see [agents](#agents)). |
| `web` | no | — | Web dashboard settings (see [web](#web)). Enabled by default. |

All paths support `~` for the home directory. Tilde expansion happens at runtime, not at config load time.

## branch naming

`branch_naming` controls how the daemon names a ticket whose `branch` field is
empty when the run starts:

```yaml
branch_naming:
  mode: off
```

| Mode | Generated branch |
|------|------------------|
| `slug` | `<prefix>/<title-slug>-<ticket-id>`, for example `kontora/fix-retry-double-count-kon-a3f2`. This is the default. |
| `off` | `<prefix>/<ticket-id>`, for example `kontora/kon-a3f2`. |

The slug comes from the ticket's first heading. The daemon removes a leading
`[project]` tag and common filler words, converts the remaining text to
lowercase ASCII words joined by hyphens, and limits the slug to 48 characters.
If the heading has no ASCII letters or digits, the daemon uses
`<prefix>/<ticket-id>`.

In `slug` mode, the daemon stores the generated name before it creates the
worktree. In `off` mode, it stores the ID-derived name after worktree creation
succeeds. Later runs and cleanup use the stored name. A branch already set on
the ticket is never replaced.

The web UI shows the name a ticket would get in the empty branch field of the
start and edit forms, so you can read it before the run starts and type over it
to choose another.

A project can override the top-level mode:

```yaml
projects:
  legacy:
    path: ~/projects/legacy
    branch_naming:
      mode: off
```

`mode` must be `off` or `slug`. Any other value fails config validation.

## web

Optional HTTP server for monitoring and controlling tickets from a browser.

```yaml
web:
  enabled: true
  host: 0.0.0.0
  port: 9090
  token: ""
  allowed_hosts:
    - kontora.tailnet.ts.net
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `enabled` | no | `true` | Start the web server when the daemon runs. |
| `host` | no | `127.0.0.1` | Bind address. Set to a tailnet IP to allow remote access. |
| `port` | no | `8080` | Listen port. |
| `allowed_hosts` | no | `[]` | Extra `Host` header values the server answers. |
| `token` | no | `""` | Shared bearer token. When set, `/api/*` and `/ws/*` require it (via `Authorization: Bearer`, a `kontora_token` cookie, or a `token` query param); empty leaves the API open. `GET /health` and the static UI stay public. |

### Host and origin checks

Every request must carry a `Host` header naming this machine: loopback, the
configured `host`, or the machine's hostname. Anything else is refused with
`403`, which is what stops a page on another site from reaching the daemon
after its own DNS re-resolves to `127.0.0.1`. Reaching the UI under any other
name, a tailnet name or a reverse proxy's hostname, needs that name in
`allowed_hosts`.

A wildcard bind address (`0.0.0.0` or `::`) is not itself an allowed host: it
is not a name a client can send. Binding it and then reaching the daemon at
`http://192.168.1.5:8080` or a tailnet address needs that address in
`allowed_hosts`. The daemon logs a warning at startup when the bind address is
a wildcard and `allowed_hosts` is empty.

The daemon also refuses a request whose `Origin` names another site and one
whose `Sec-Fetch-Site` is cross-site, except a top-level navigation, so a link
to the UI still opens. A client that sends neither header, the CLI or `curl`,
is unaffected. Writes that carry a body must be `application/json`; the upload
route takes `multipart/form-data` and an `X-Kontora-Confirm` header.

Behind a TLS-terminating reverse proxy, forward `X-Forwarded-Proto`. Without it
the daemon cannot tell which port an `Origin` that omits its own port means, so
it accepts only the default port of the origin's own scheme. The header also
decides whether the auth cookie gets its `Secure` flag.

`allowed_hosts` is read at startup only. Changing it needs a daemon restart,
like the rest of the `web` block.

### Content Security Policy

The UI document is served with a policy that keeps every resource same-origin:
`script-src 'self'`, `connect-src 'self'`, `img-src 'self' data:`. Third-party
libraries and fonts are vendored into the binary, so nothing the UI needs is
external. One user-visible consequence: an `<img>` in a ticket body or note
pointing at a remote URL does not load. Inline it as a `data:` URI or serve it
from the daemon.

When a token is set, the CLI can drive the daemon remotely with `KONTORA_URL` and `KONTORA_TOKEN` (see the Remote mode section of the README). The token is the only access control and agents run with `--dangerously-skip-permissions`; a tailnet encrypts the transport, but on untrusted networks put the daemon behind TLS.

See [API Reference](api.md) for the full list of endpoints.

## agents

Map of agent name to its binary and arguments. Any CLI tool that accepts a prompt on stdin or as an argument can be an agent.

```yaml
agents:
  claude-sonnet:
    binary: claude
    args: ["--dangerously-skip-permissions", "--model", "sonnet"]
```

| Field | Required | Description |
|-------|----------|-------------|
| `binary` | yes | Executable name or path. |
| `args` | no | Arguments passed to the binary. The rendered prompt is appended as the last argument. |
| `environment` | no | Map of environment variables to set for this agent's processes (merged with top-level `environment`). |
| `failure_patterns` | no | Regexes matched against the agent's output log after it exits. A match pauses the ticket even on a clean exit — catching agents that report failures (quota, API errors) without a non-zero exit code. Unset uses the built-in defaults (below); set an explicit list to override, or `[]` to disable. Claude also gets structural detection from its session log regardless. |
| `resume` | no | Set `false` to make every stage this agent runs start a new conversation, even after a daemon restart interrupted one (see [resuming after a restart](#resuming-after-a-restart)). Unset means resume is on for `claude` and `pi`; any other agent always starts fresh. |
| `effort` | no | Reasoning effort every invocation of this agent starts from, passed as `--effort` to `claude` and `--thinking` to `pi`. It replaces the same flag in this agent's own `args`, and a stage's `effort` overrides it (see [stages](#stages)). Only those two CLIs take a flag for it: an `effort` on any other agent fails to load. The level names are passed through unchecked, so a new one works without a Kontora release, and a typo shows up as an agent that fails to start. |
| `checkpoint_compaction_tokens` | no | Enables phase-boundary compaction for `pi` and `claude` when positive. Compaction runs only when measured context tokens are greater than this value. Zero or unset disables it. Negative values, and any value on an agent that is neither `pi` nor `claude`, fail validation. Wrapped agents, such as `nono run -- pi` or `nono run -- claude`, are supported. |

When checkpoint compaction is enabled, the prompt requires a durable `phase-N:` ticket note before each phase-boundary signal. The note records changed files, decisions, test results, unresolved issues, and the next phase. A compaction failure never fails the stage: the agent continues the next phase from the uncompacted session.

The two agents signal the boundary differently, because they expose different triggers:

- Pi calls the `kontora_phase_complete` tool, and the embedded extension compacts in process. Pi records skipped, requested, compacted, and failed checkpoints in the session.
- Claude runs `kontora phase-complete TICKET_ID --completed TEXT --next TEXT` (see [the CLI reference](cli.md)) and ends its turn. The daemon then measures the context from the session JSONL, types `/compact` into the tmux window when it is over the threshold, and types the continuation prompt for the next phase. The sidecar `<logs_dir>/<ticket>/<stage>.<run>.checkpoints.jsonl`, beside the stage log, holds the whole exchange: the agent's `phase_complete` record, the `accepted` marker the daemon writes when it takes that boundary on, and the `outcome` record once it knows how the boundary went. A compaction that leaves no compact boundary in the session JSONL is recorded as `failed`, whether the wait timed out or returned quietly.

Driving Claude this way needs a window the daemon can type into unattended. A Claude agent that stops on a permission prompt blocks before the boundary and never reaches it, so checkpointing has no effect there. Annotation runs and Plannotator rework runs never checkpoint, whatever the agent's threshold: they rewrite the ticket rather than working through its phases.

A config reload applies a changed threshold to the next agent invocation. It does not change a running agent. The web Settings form preserves `checkpoint_compaction_tokens` when it edits another field, but does not expose a control for it.

When `failure_patterns` is omitted, an agent inherits these defaults, tuned to match agent/provider error output rather than source code or prose (so implementing a rate limiter won't self-pause):

```
(?im)^\s*API Error:               # Claude Code: 4xx/5xx, overloaded, timeouts, ECONNRESET
(?i)Please run /login             # auth lost: "Not logged in" / "Invalid API key"
(?i)usage limit reached           # "Claude AI usage limit reached"
(?i)You've hit your (usage )?limit # usage/session limit stop
(?i)Prompt is too long            # context window exceeded
(?i)insufficient_quota            # OpenAI-backed agents: billing
(?i)exceeded your current quota   # OpenAI-backed agents: billing
(?i)Rate limit reached for        # OpenAI-backed agents: rate limit
```

To turn detection off for an agent, set `failure_patterns: []`.

### Resuming after a restart

When the daemon stops while an agent is mid-stage, the ticket goes back to `todo` and the stage is scheduled again. The worktree and branch survive, so without resume the agent starts a new conversation on top of its own half-finished work, with no memory of why it made those changes. Resume reattaches the stage to the conversation it was in, and sends `resume_prompt` instead of the stage prompt so the agent continues rather than starts over.

The daemon writes `<logs_dir>/<ticket-id>/<stage>.session` while a stage's agent runs and deletes it as soon as the agent returns. A record left on disk therefore means the daemon itself went away. It is not part of the ticket file: everything it points at, including the Claude session files under `~/.claude/projects/`, is local to one machine.

The record is keyed by stage and holds one session, so the next run of the same stage overwrites it. Which session each individual run wrote is kept in the ticket's history instead, as [`session_kind` and `session_ref`](tickets.md#history), which are identifiers rather than paths for the same machine-local reason.

A stage resumes only when every one of these holds. Any other case runs the stage fresh and logs why; nothing here pauses a ticket.

- The agent is `claude` or `pi` and its `resume` is not `false`.
- The record names this same stage, this same agent, this daemon's `instance_name`, and this worktree.
- The agent's session file still exists.
- No tmux window for the ticket is live, which would mean a process may still hold the session.

Pausing a ticket and running `kontora retry` are deliberate restarts: the daemon is still up when the agent dies, so the record is already gone and the stage starts over.

An agent that had in fact finished its work can exit within a second of resuming, which the tmux startup guard reads as a crash. Rather than pause the ticket, the daemon runs the stage once more from its normal prompt in a new session. That fallback happens at most once per scheduled stage and does not consume a pipeline retry. It only covers a resumed run that failed inside its first two seconds: a run that failed later did work that is now in the worktree, so the ticket pauses instead of repeating it.

## stages

Map of stage name to its prompt template, timeout, model, and reasoning effort. A stage defines *what* an agent should do at a pipeline step.

```yaml
stages:
  code:
    prompt: |
      {{ .Ticket.Description }}
    timeout: 30m
  commit:
    prompt: Stage, commit, and push.
    timeout: 5m
    # One value for every agent that runs this stage:
    model: haiku
    effort: low
  push-pr:
    prompt: Open a pull request.
    # Or one per agent, because a model name is not portable between CLIs.
    # A key is an agent name or an agent kind (claude, pi); the agent name wins.
    model:
      claude: haiku
      pi: anthropic/claude-haiku-4-5
    # The agent name wins over the kind, so one agent can differ from the rest.
    effort:
      claude: high
      claude-opus: xhigh
```

| Field | Required | Description |
|-------|----------|-------------|
| `prompt` | yes | Go template rendered before passing to the agent. |
| `timeout` | no | Maximum duration for the agent (e.g., `10m`, `1h30m`). |
| `model` | no | Model this stage runs on, passed to the agent as `--model`. Either one pattern, or a map from agent name or agent kind (`claude`, `pi`) to a pattern. It replaces any `--model` in the agent's own `args`. Only `claude` and `pi` take the flag: a stage that resolves a model for any other agent fails to load, or pauses the ticket when the ticket's own `agent` field picks that agent. |
| `effort` | no | Reasoning effort this stage runs on, passed as `--effort` to `claude` and `--thinking` to `pi`. Same two forms and the same agent-name-over-kind rule as `model`, and the same failure for an agent that takes no flag for it. It overrides the agent's own `effort`. |

For `pi`, `--thinking` and the `:<level>` suffix of a model pattern (`anthropic/claude-opus-5:high`) set the same thing. Either alone works; both together fail with an error naming the two values. A pipeline step whose stage and agent pair them fails to load. A pair reached any other way — through the ticket's own `agent` field, or through `summary_effort` — is caught when the stage spawns, which pauses the ticket. Only the model and the effort the run actually uses count, so a stage `model` with no suffix clears the conflict with a suffixed one in the agent's `args`, and a `--thinking` in those `args` conflicts with a suffixed model exactly as a configured `effort` does.

### Prompt templates

Prompts are Go [text/template](https://pkg.go.dev/text/template) strings with these variables and functions:

| Expression | Description |
|------------|-------------|
| `{{ .Ticket.ID }}` | Ticket ID (e.g., `poi-q88f`). |
| `{{ .Ticket.Title }}` | First `# Heading` from the ticket body. |
| `{{ .Ticket.Description }}` | Full ticket body (markdown after frontmatter). |
| `{{ .Ticket.FilePath }}` | Absolute path to the ticket's markdown file. |
| `{{ file "PLAN.md" }}` | Contents of a file relative to the ticket's worktree. |
| `{{ plannotatorReview }}` | Feedback from the last Plannotator code review. Reading it deletes the file, so only the built-in `rework` stage uses it. |
| `{{ plannotatorAnnotations }}` | Pending Plannotator annotations on the ticket. Reading it leaves the file in place (see [plannotator](#plannotator)). |

The `file` function is how stages communicate — an earlier stage writes a file (e.g., `PLAN.md`) and a later stage reads it via the template.

## pipelines

Map of pipeline name to an ordered list of stages. Each ticket references a pipeline by name in its `pipeline` frontmatter field.

```yaml
pipelines:
  default:
    - stage: code
      agent: claude
      on_success: human_review
      on_failure: pause
```

### Stage fields

| Field | Required | Values | Description |
|-------|----------|--------|-------------|
| `stage` | yes | — | Stage to run at this pipeline step. |
| `agent` | yes | — | Agent to run the stage. |
| `on_success` | yes | `next`, `done`, `human_review`, or a custom status | What to do when the agent exits 0. |
| `on_failure` | yes | `retry`, `back`, `pause`, `human_review`, or a custom status | What to do when the agent exits non-zero. |
| `max_retries` | no | integer (default `0`) | Maximum retry attempts (only relevant when `on_failure=retry`). |

### Policies

**`on_success`**:
- `next` — advance to the next stage (set status back to `todo` so the scheduler picks it up).
- `done` — mark the ticket as complete.
- `human_review` — park the ticket in `human_review` for a human to look at.
- A custom status declared in top-level `statuses:` — park the ticket in that status.

**`on_failure`**:
- `retry` — re-run the same stage (up to `max_retries`, then pause).
- `back` — go back to the previous stage. Not allowed on the first stage.
- `pause` — set the ticket to `paused`.
- `human_review` — park the ticket in `human_review`.
- A custom status declared in top-level `statuses:` — park the ticket in that status.

### Validation rules

- Every pipeline step must reference a stage and agent that exist in the config.
- `on_success` must be `next`, `done`, `human_review`, or a custom status declared in top-level `statuses:`.
- `on_failure` must be `retry`, `back`, `pause`, `human_review`, or a custom status declared in top-level `statuses:`.
- `on_failure=back` is not allowed on the first stage.
- The last stage must not have `on_success=next` (it must terminate with `done`, `human_review`, or a custom status).
- A stage cannot appear more than once in the same pipeline.

## projects

Map of project name to a repository and the defaults tickets for it should get.
Without a `projects:` block nothing changes: a ticket created for a repository
with no entry still gets no pipeline and no agent.

```yaml
projects:
  kontora:
    path: ~/projects/kontora
    pipeline: implement-review-commit
    agent: claude
  widget-api:
    path: ~/projects/widget-api
    pipeline: default
```

| Field | Required | Description |
|-------|----------|-------------|
| `path` | yes | Repository the entry applies to. |
| `pipeline` | no | Pipeline written into new tickets for this repository. |
| `agent` | no | Agent written into new tickets for this repository. |
| `prefix` | no | Ticket-ID prefix for this repository, instead of one derived from the directory name. Lowercase letters and digits only. |
| `branch_prefix` | no | Overrides the top-level `branch_prefix` for this repository. |
| `branch_naming` | no | Overrides the top-level [`branch_naming`](#branch-naming) mode for this repository. |
| `hooks` | no | Commands run at this repository's lifecycle events, after the top-level ones (see [hooks](#hooks)). |

`pipeline`, `agent` and `prefix` are read when the ticket is created. The
daemon reads `branch_prefix` and `branch_naming` when it names an empty branch,
so the current config applies until pickup. A ticket that already carries a
`branch` keeps it.

Set `prefix` when the derived one is not what you want, or when two
repositories would otherwise share it. Without it the prefix comes from the
directory name; see [ticket ID format](tickets.md#ticket-id-format) for the
rule. Changing it renames nothing: existing IDs stay as they are.

The pipeline and agent defaults are applied when a ticket is created (`kontora new`, `POST /api/tickets`, the TUI and web create forms) or initialized (`kontora init`, `POST /api/tickets/{id}/init`), and are written into the ticket's frontmatter. The ticket file keeps saying exactly what will run, and an existing ticket is never rewritten.

A value you supply yourself wins over the project default. On `kontora init` a value the ticket file already declares wins too: the project fills a blank field, it never replaces a pipeline or an agent the ticket chose. The two fields are independent: naming a pipeline still lets the agent come from the project.

`path` is matched after `~` expansion and cleaning, so `~/projects/kontora`, `/home/you/projects/kontora`, and a trailing slash all reach the same entry. Only the complete path matches. A ticket pointing at `~/projects/kontora/internal` inherits nothing, and symlinks are not resolved. A relative path is not resolved either, because only the daemon host knows what it would be relative to: `kontora new --path . "..."` matches no project. Without `--path`, `kontora new` fills in the current git root, which is absolute and does match.

### Opting out with `none`

Once a project sets a default pipeline, leaving `--pipeline` blank no longer produces a standalone ticket. Pass the literal `none` instead:

```bash
kontora new --path ~/projects/kontora --pipeline none "one agent run, no stages"
```

`none` means "leave this field blank and skip the project default". It is accepted wherever a command takes a pipeline or agent name: the `kontora new`, `kontora update`, and remote `kontora init` flags, the `pipeline` and `agent` fields of `POST /api/tickets`, `POST /api/tickets/{id}/init`, and `PUT /api/tickets/{id}`, the TUI create form, and the web selects. The literal string never reaches the frontmatter. Because it is reserved, a pipeline or an agent named `none` fails config validation.

Only those commands read the sentinel. `pipeline: none` written by hand into a ticket that is already initialized stays there, and the daemon pauses the ticket with `unknown pipeline "none"`. Leave the field out instead.

### Validation rules

- `path` is required.
- `pipeline` and `agent`, when set, must exist in `pipelines:` and `agents:`.
- Two projects may not have paths that expand and clean to the same directory, which would make the lookup pick one at random.
- No pipeline and no agent may be named `none`.

## hooks

Hooks run your own shell commands at points in a ticket's life. The case they
exist for: a fresh worktree does not carry the gitignored files a repository
needs, so an agent that expects a `.env` fails on a missing file after burning a
run. A hook copies it in before the agent starts.

```yaml
hooks:
  worktree_created:
    - name: copy claude settings
      run: cp "$KONTORA_REPO_PATH/.claude/settings.local.json" .claude/ 2>/dev/null || true

projects:
  kontora:
    path: ~/projects/kontora
    hooks:
      worktree_created:
        - name: copy env file
          run: cp "$KONTORA_REPO_PATH/.env" .env
          timeout: 30s
      stage_start:
        - run: make deps
          on_failure: warn
```

### Events

| Event | When it runs |
|-------|--------------|
| `worktree_created` | After the daemon creates a worktree, before the stage's agent starts. It does not run when a stage reuses a worktree that is already there, unless an earlier run left that worktree half-prepared (see [failure](#failure)). |
| `stage_start` | Before each stage agent starts. |
| `stage_end` | After each stage agent exits, before the pipeline decides what happens next. |

An [annotation run](#plannotator), which rewrites the ticket rather than doing
the stage's work, fires no stage hooks.

`stage_start` also runs when a stage resumes after the daemon died mid-run, so a
hook that must not run twice for one stage has to be written to tolerate it.

A `stage_start` that completes is followed by a `stage_end`, including for a run
that ends before the pipeline evaluates it — a runner failure, or an agent that
hides an error behind a clean exit. Two things break the pair. A `stage_start`
hook that fails under `pause` stops the stage before it starts, so no
`stage_end` runs; and neither runs when the exit is not the daemon's to act on,
which is when the ticket was cancelled, its status was changed by hand while the
agent ran, or another instance claimed it.

### Fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `run` | yes | — | The command line, run as `/bin/sh -c`. |
| `name` | no | — | Labels the hook in the logs and in the error a failure records. Without one the hook is `<event>[<index>]`. |
| `timeout` | no | `5m` | How long the command may run before it is terminated. |
| `on_failure` | no | per event | `pause` or `warn`. Defaults to `pause` for `worktree_created` and `stage_start`, `warn` for `stage_end`. |

### Scope and order

Hooks are defined at the top level and under a project entry. For a ticket whose
repository path matches a project, both sets run for the same event, top-level
first. A repository that matches no project runs the top-level hooks alone.
Hooks within one set run in the order they are written, and the daemon waits for
each before starting the next.

### Environment

Every hook runs with its working directory set to the ticket's worktree, and
receives the daemon's own environment plus:

| Variable | Value |
|----------|-------|
| `KONTORA_EVENT` | The event that fired. |
| `KONTORA_TICKET_ID` | The ticket's ID. |
| `KONTORA_TICKET_FILE` | Path to the ticket's markdown file. |
| `KONTORA_WORKTREE` | The worktree the hook runs in. |
| `KONTORA_REPO_PATH` | The repository the worktree was cut from. |
| `KONTORA_BRANCH` | The ticket's branch. |
| `KONTORA_STAGE` | The stage this run belongs to. |
| `KONTORA_AGENT` | The configured name of the agent the stage runs. |
| `KONTORA_PROJECT` | The `projects:` entry that matched, empty when none did. |
| `KONTORA_EXIT_CODE` | The agent's exit code. Set for `stage_end` only. |

The top-level `environment:` map is deliberately not merged in: it configures
agent processes, not hooks. There is no templating in `run` — write
`"$KONTORA_REPO_PATH"`, and the quoting rules are the shell's own.

### Failure

A hook that exits non-zero or exceeds its timeout pauses the ticket when its
resolved `on_failure` is `pause`, and is logged and skipped past when it is
`warn`. A pause records the failure in `last_error` and as a ticket note, and
stops the hooks after it in the same event.

A `worktree_created` failure under `pause` removes the worktree the daemon
created in that pickup before pausing, so retrying the ticket creates it again
and runs the hook again rather than reusing a half-prepared one. A worktree the
hook left with uncommitted changes is kept, the same as anywhere else: the
pause reason then names it, and the next pickup runs the `worktree_created`
hooks again on the worktree that is still there rather than treating it as
ready. The same applies when the daemon is stopped while these hooks run.

A `stage_end` failure does not change what the pipeline decided: the stage's
history, its next stage, and its `last_error` are recorded as they would have
been, and only the status is forced to `paused` on top. The hook's message goes
into `last_error` only when the pipeline recorded none. A ticket the pipeline
had completed loses its `completed_at` with the pause, because the run is not
over.

Copying a gitignored file does not make a worktree dirty, so cleanup on
completion still works. A hook that writes a tracked file does block removal.

### Logging

The combined output of every hook is appended to
`<logs_dir>/<ticket-id>/hooks/hooks.log`, each run behind a
`=== <time> <event> <hook> ===` line. It sits in a directory of its own rather
than beside the stage logs, which the daemon scans for the agent's
[failure patterns](#agents): hook output in a stage log would pause tickets
whose agent did nothing wrong.

### Validation rules

- The event name must be `worktree_created`, `stage_start`, or `stage_end`.
- `run` is required and must not be blank.
- `on_failure`, when set, must be `pause` or `warn`.
- `timeout` must not be negative.

A hook runs arbitrary shell at the same trust level as `agents.<name>.binary`.
Anything that can write your config can run commands as you.

## plannotator

[Plannotator](https://plannotator.ai) is the UI the daemon spawns for the two
passes a human drives: reviewing the branch diff, and annotating the ticket.
Start either from the ticket detail pane or over the [API](api.md).

```yaml
plannotator:
  binary: plannotator
  timeout: 30m
  reviews_dir: ~/.kontora/plannotator-reviews
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `binary` | no | `plannotator` | The binary to spawn. Resolved on the daemon's `PATH`, or used as-is when absolute. |
| `timeout` | no | `30m` | How long a session may stay open before it is cancelled. |
| `reviews_dir` | no | `~/.kontora/plannotator-reviews` | Where captured feedback waits for the agent that consumes it. |

Only one Plannotator process runs per ticket, so a review and an annotation
cannot overlap. While a session is open the scheduler leaves the ticket alone,
and picks it up again when the session closes: a stage run would edit the file
under the reviewer.

### Reviewing the code

A ticket in `human_review` opens its branch diff. The daemon builds a throwaway
detached worktree at the merge base and applies the branch's diff on top, so the
default "unstaged" view shows everything the agent committed.

Submitted feedback is written to `<reviews_dir>/<id>.md` and the ticket moves to
the built-in `rework` stage, whose prompt reads it through
`{{ plannotatorReview }}`. That read deletes the file.

### Annotating the ticket

A ticket in `open` opens its own markdown file. Later statuses are refused: a
stage has run against the ticket text by then, and a rewrite would contradict the
work it produced. There is no worktree, no merge base, and no diff: the target is
the file in `tickets_dir`. A ticket that is already parked for an annotation run
is refused, so a second set of notes cannot overwrite the pending one.

The ticket does not have to be initialized. Submitting annotations sets
`kontora: true`, because the scheduler only picks up a kontora ticket and the
notes would otherwise never be read. It is the same adoption a status change on
the board performs, and it adds no pipeline or agent.

Approving or dismissing leaves the ticket untouched. Submitting annotations:

1. writes them to `<reviews_dir>/<id>.annotations.md`, a different file from code
   review feedback so the two can never overwrite each other,
2. records the ticket's current status in `annotation_return_status` and sends
   the ticket back to `todo` with `attempt: 0`, leaving `stage` alone,
3. schedules one run with `annotation_prompt`, which reads the annotations
   through `{{ plannotatorAnnotations }}`.

That run rewrites the ticket and nothing else. It evaluates no pipeline action,
so it cannot advance the ticket or repeat the stage's work, and it does not
regenerate the ticket-level summary. On success the ticket returns to the status
in `annotation_return_status`, the field is cleared, and the annotations file is
deleted. On a nonzero exit the ticket pauses with `last_error`, and both the
field and the annotations file stay, so `kontora retry` runs again against the
same feedback. Each run adds one history entry with `kind: annotation`.

A custom `annotation_prompt` must include `{{ plannotatorAnnotations }}`. The
daemon checks that the rendered prompt carries the annotations and pauses the
ticket if it does not, because an agent that never received the notes would
still report success, and that success is what deletes them.

If the annotations file is gone by the time the run starts, nothing runs and the
ticket returns to its status. To call off a pending run, move the ticket to
another status (`kontora pause <id>`, the board menu, or any other status move):
that clears `annotation_return_status`, and the status you chose is where the
ticket stays.

The restriction to the ticket file is stated in the prompt and only there. A
stage carries no tool policy, so an agent that ignores the instruction is not
stopped by anything. Set the ticket's `agent` to a profile with narrower
permissions if that matters to you.

The run works in the ticket's existing worktree when a previous run created one,
and in the repository itself otherwise. A ticket with no `path` runs in
`tickets_dir`, which is the only directory such a ticket has; the agent then
answers the notes without the code in front of it. It never creates a worktree or
a branch: a ticket annotated before its first stage has no work to put on a
branch.

Where it can, the run continues the conversation the ticket's current stage
ended in, so the agent already knows what it built and why. The daemon records
that session in `<logs_dir>/<ticket-id>/<stage>.completed-session` when the
agent returns. Reuse needs every one of these, the same conditions as [resume
after a restart](#resuming-after-a-restart) plus one:

- the agent is `claude` or `pi`, with `resume` not `false`,
- the record names this stage, this agent, this `instance_name`, and this working
  directory,
- the session file it names still exists,
- no tmux window for the ticket is live,
- the stage has no interrupted run of its own waiting to recover. That run must
  continue its own conversation, and appending to the recorded one would make it
  the newest session in the stage's directory, which is how the interrupted run
  is identified.

Any other case starts a new conversation, logs why, and does not pause the
ticket. The history entry's `session_reused` field reports which happened, and
its [`session_kind` and `session_ref`](tickets.md#history) name the session the
run ended up writing, whether it continued one or opened its own.

This record is deliberately separate from the crash-recovery record a stage
writes while it runs. Only the annotation run reads it, so an ordinary
`kontora retry` of a stage that finished still starts fresh.

## assistant

The dashboard's assistant pane. It answers questions about the board and drives
Kontora through the same verbs the CLI uses, running one of the agents already
listed under `agents:`.

```yaml
assistant:
  agent: claude          # required to enable; no agent means no assistant
  model: sonnet          # optional
  effort: medium         # optional
  workdir: ~/.kontora/tickets   # optional, defaults to tickets_dir
  timeout: 10m           # optional, per turn
  autonomy: ask          # optional: read | ask | auto
  prompt: ""             # optional, replaces the built-in system brief
  stream: true           # optional, claude only; unset means on
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `agent` | no | none | Which `agents:` entry the assistant runs. Empty disables the pane, which then shows a configure hint instead of a composer. |
| `model` | no | the agent's own | Applied the way a stage's `model` is. |
| `effort` | no | the agent's own | Reasoning effort. Rejected for an agent whose CLI takes no flag for it. |
| `workdir` | no | `tickets_dir` | The working directory every turn runs in. |
| `timeout` | no | `10m` | Bounds one turn. |
| `autonomy` | no | `ask` | The mode a new chat starts in. |
| `prompt` | no | built-in | Replaces the whole system brief, mode paragraph included. |
| `stream` | no | `true` | Whether a claude turn asks for the message as it is written, so the pane renders prose before the message completes. |

### Replies as they are written

The pane renders an assistant message while the agent is still writing it. A
claude turn runs with `--include-partial-messages`, and the daemon keeps the
text those records carry in memory until the session file records the message.
Nothing reaches the run transcript or the turn log that did not before: a
partial record is a fragment of a message the log already carries whole.

Set `stream: false` to turn it off. A claude build that does not know the flag
rejects it at argument parsing, before it reads the prompt. The daemon sees the
flag named in the error and runs the turn again without it, so the reply still
arrives, whole rather than as it is written. The retry is logged as a warning
naming the flag; `stream: false` stops the wasted first attempt.

pi has no equivalent flag, so a pi chat behaves as it did before and the field
does nothing.

### Only claude and pi

`assistant.agent` must name an agent whose binary is `claude` or `pi`. Config
validation rejects any other, naming the two.

One chat is one agent session: the first message mints a session id and every
later message resumes it, so the chat remembers what was already said. Only
those two CLIs take a flag for that alongside a headless print mode. An agent
without one would start from nothing on every message, which is not the feature.

For the same reason the working directory is fixed when the chat opens rather
than following the board. Claude keys its session files by working directory, so
a chat whose directory moved could not resume.

The agent, the model and the effort are fixed at the same point. Repointing
`assistant.agent` after a chat has run applies to new chats; the old ones refuse
a further message and say so, rather than resuming a claude session with pi and
silently starting from nothing.

### Autonomy

The mode is enforced at the agent's tool boundary, not by asking the agent to
behave: claude runs a `PreToolUse` hook, pi a `tool_call` handler, and both ask
the daemon before the tool runs. A tool call the daemon has no rule for counts
as a change, so a mode can only ever be stricter than expected, never looser.

| Mode | Reads | Changes | Deleting a ticket |
|------|-------|---------|-------------------|
| `read` | run | refused, with a reason the agent reports | refused |
| `ask` | run | held until you approve or skip it in the pane | held |
| `auto` | run | run | held |

A change left waiting is refused after five minutes, so a chat you walked away
from does not leave an agent process blocked.

The mode is per chat and the pane's selector sets it for the next message, so a
chat can start read-only and be opened up once you have seen what it proposes.

### Turns and the scheduler

An assistant turn is a headless agent process, not a tmux window and not a
pipeline stage. It does not take a `max_concurrent_agents` slot: someone waiting
at the pane should not queue behind the board, and should not push a ticket run
out of the way either. At most two turns run at once across every chat, and one
per chat, because a chat's own session cannot take two.

The agent is given `KONTORA_URL` and `KONTORA_TOKEN`, so its `kontora` calls go
through the running daemon and the board updates as it works, and a per-turn
secret the tool gate authenticates its calls with. `KONTORA_URL` names loopback
even when `web.host` is a wildcard: the daemon refuses a request whose `Host` is
`0.0.0.0`, so the agent has to reach it by an address it answers to.

Chats are stored under `<logs_dir>/assistant/<chat-id>/`. Deleting one from the
history removes its transcript with it.

## metrics

Optional OTLP export of what the daemon measures: stage runs and their
durations, pipeline transitions, agent failures, token spend, and scheduler
state. It is off by default and pushes over OTLP/HTTP to a collector you
supply. There is no `/metrics` route and no scrape target; the daemon's HTTP
surface does not change.

```yaml
metrics:
  enabled: true
  endpoint: localhost:4318
  insecure: true
  interval: 60s
  headers:
    authorization: Bearer <token>
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `enabled` | no | `false` | Export metrics. When false the daemon builds no exporter and opens no connection. |
| `endpoint` | no | `""` | Collector address, as a bare `host:port` or a full URL. Empty leaves the address to `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. |
| `insecure` | no | `false` | Send over plain HTTP. Ignored when `endpoint` states its own scheme, and when `endpoint` is empty. |
| `interval` | no | `60s` | How often the collected measurements are pushed. |
| `headers` | no | `{}` | Headers added to each export request, for a collector that needs auth. |

An `endpoint` that states a scheme decides the transport, whatever `insecure`
says: `http://collector:4318` is always plain and `https://collector:4318`
always TLS. A bare `host:port` leaves the choice to `insecure`. Combining
`https://` with `insecure: true` contradicts itself, so the daemon keeps the
scheme and logs a warning. Only `http` and `https` are accepted; the config
fails to load on any other scheme.

`insecure` describes the configured `endpoint` and nothing else. With no
`endpoint`, the transport comes from `OTEL_EXPORTER_OTLP_ENDPOINT` along with
the address, so `insecure: true` on its own cannot downgrade an `https://`
endpoint set in the environment.

The request path is `/v1/metrics` unless the endpoint names one. Both
`collector:4318` and `http://collector:4318` post to
`http://collector:4318/v1/metrics`; write the path out
(`https://collector.example/otlp/v1/metrics`) only when the collector serves it
somewhere else.

The SDK's own `OTEL_EXPORTER_OTLP_*` variables are read for the address and
headers the config leaves unset, and `OTEL_RESOURCE_ATTRIBUTES` is merged into
the resource. `OTEL_METRIC_EXPORT_INTERVAL` is not: `interval` always has a
value, its own or the 60s default, and it is passed to the reader on every
start. `metrics.enabled` is the only kill switch: `OTEL_SDK_DISABLED` is not
implemented by the Go SDK and does nothing here.

Exporting never blocks ticket work. A collector that is down, or an exporter
that cannot be built at all, produces a warning and the daemon runs on with
metrics off.

### What is exported

| Name | Kind | Unit | Attributes |
|------|------|------|------------|
| `kontora.stage.runs` | counter | `{run}` | `stage`, `agent`, `pipeline`, `outcome`, `annotation`, `exit_code` |
| `kontora.stage.duration` | histogram | `s` | `stage`, `agent`, `pipeline`, `outcome`, `annotation` |
| `kontora.stage.transitions` | counter | `{transition}` | `stage`, `action` |
| `kontora.agent.errors` | counter | `{error}` | `stage`, `agent`, `kind` |
| `kontora.agent.tokens` | counter | `{token}` | `stage`, `agent`, `kind` |
| `kontora.queue.wait` | histogram | `s` | none |
| `kontora.scheduler.active` | gauge | `{agent}` | none |
| `kontora.scheduler.capacity` | gauge | `{agent}` | none |
| `kontora.queue.depth` | gauge | `{ticket}` | none |

`outcome` is `success`, `failure`, or `cancelled`. `action` is the pipeline
action the exit produced: `advance`, `complete`, `retry`, `back`, `pause`, or
`park`. On `kontora.agent.errors`, `kind` is `session_api_error` for a failure
found in Claude's session record and `failure_pattern` for one matched against
the agent's output log. On `kontora.agent.tokens` it is `input`, `output`,
`cache_create`, or `cache_read`. A run is dropped whole if any one of its
session records left its usage key unfilled; the counter never reports a
partial figure.

A ticket that runs without a pipeline reports `pipeline=""` and `stage=default`,
which is the same key its log and its history rows already use.

`annotation` is `true` for a run that answers review annotations instead of
doing the stage's work. Such a run borrows the stage's name, so a query about a
stage's own runs and durations has to ask for `annotation="false"`. Its token
spend has no such attribute: it is attributed to the stage whose conversation it
continues.

A stage run is counted once however many agent invocations it took, so a stage
that resumes an interrupted session and then falls back to a fresh one is one
run, not two. Token spend is counted per invocation, because each invocation is
a separate real spend. An annotation run continues the session its stage
finished, and records only the tokens it adds to it rather than the session's
running totals. A run that recovers a session the daemon died in reports that
session's totals whole, because the invocation that spent them never got to
report anything.

`kontora.queue.wait` measures a ticket from the moment it is enqueued to the
moment its run starts, so it includes the wait for a free concurrency slot.

Some work is not measured at all. The final-summary agent runs with session
persistence off, so it writes no session file to read token counts from; it
appears in neither `kontora.stage.runs` nor `kontora.agent.tokens`. The
`plannotator` subprocess is not instrumented either. Total wall time per ticket
therefore does not add up from the stage durations alone.

The built-in `rework` stage produces `kontora.stage.runs` and
`kontora.stage.duration` but no `kontora.stage.transitions`, because it routes
the ticket itself rather than through the pipeline.

No ticket ID appears in any attribute. A store holding thousands of tickets
would make that unbounded.

The resource carries `service.name` (`kontora`), `service.version`, and
`service.instance.id`, which is the daemon's `instance_name`. Two daemons
sharing one collector are told apart by that last one.

## Reloading the config

The running daemon applies most config edits without a restart, so you can
change a prompt or an agent's arguments while agents are working.

### What reloads live

`agents`, `stages`, `pipelines`, `projects`, `statuses`, `environment`,
`auto_pick_up`, `default_agent`, `branch_prefix`, `branch_naming`,
`resume_prompt`, `annotation_prompt`, `summary_model`, `summary_effort`, and the
whole `plannotator` block.

A run reads the prompts and the `plannotator` block when it starts, so a reload
changes the next run, never one already going.

Editing `projects` reloads live, but the pipeline and agent defaults are stamped
into a ticket when it is created or initialized. A reload changes what the next
ticket gets, never what an existing one already carries. A project's
`branch_prefix` and `branch_naming` are not stamped, so a reload changes how the
daemon names an existing ticket whose branch is still empty.

### What needs a restart

`tickets_dir`, `worktrees_dir`, `logs_dir`, `instance_name`, `tmux_session`,
`max_concurrent_agents`, and the whole `web` and `metrics` blocks
(`metrics.enabled`, `metrics.endpoint`, `metrics.insecure`, `metrics.interval`,
`metrics.headers`). The daemon reads these once at startup: the watched
directory, the worktree root, the claim name, the tmux session, the semaphore
size, the HTTP listener, and the metric exporter are all fixed by then. A
reload keeps the running value and logs one warning per field that differs on
disk, naming the field, the running value, and the value it ignored. The
`web.token` and `metrics.headers` values are never logged.

A reload resolves `tickets_dir` from the environment before that check, the same
way startup does. So a daemon started with `$KONTORA_TICKETS_DIR` or
`$TICKETS_DIR` set keeps that store across reloads, and editing `tickets_dir` in
the file changes nothing and warns about nothing: the environment already
outranked the file on both sides of the comparison.

To rename `tmux_session`, stop the daemon, run `tmux kill-session -t =<old-name>`,
then start it with the new name. The old session is not renamed for you, and it
must not be left running: its windows are invisible to the new daemon, so crash
recovery re-queues a ticket whose first agent is still alive and starts a second
agent on the same worktree and branch.

`editor` is read by the CLI, not the daemon, so it takes effect on the next
command either way.

If you started the daemon with `--address` or `--port`, those keep winning over
the file after a reload, the same as at startup. Editing `web.host` or
`web.port` on disk then changes nothing and logs no warning: the flag already
overrides it, and a restart with the same flag would too.

### How to trigger a reload

- Send `SIGHUP`: `kill -HUP $(pgrep -f 'kontora start')`.
- Edit the config file. The daemon watches it and reloads after the debounce
  interval, whether the editor writes in place or replaces the file with an
  atomic rename. If the config path is a symlink, the daemon watches the
  symlink's directory and the target's directory, so an edit through a dotfiles
  symlink also reloads. Other files in the same directory (the daemon lock file,
  the `.kontora-config-*.tmp` files an atomic save creates and renames away)
  trigger nothing.
- Save through `kontora config edit --url ...`. The daemon writes the file and
  reloads it before the request returns.

### Rules

A reload is all-or-nothing. The daemon parses and validates the whole file
before applying any of it: if anything fails, it logs the error and keeps
running on the old config. Saving a half-written file mid-edit is harmless, and
the next complete save reloads.

**A running agent keeps the settings it started with.** The prompt, arguments,
model, effort, timeout, and binary are fixed when the stage spawns. Editing a prompt
while a ticket is mid-stage does nothing visible until the next stage starts.
That is expected, not a failed reload.

If a reload removes the pipeline or the stage a `todo` ticket sits on, the
daemon pauses that ticket and writes the reason to `last_error` rather than
leaving it stuck with no explanation.

## Shell completions

`kontora completion <shell>` prints a script for bash, fish or zsh. All three
complete the verbs, their flags and, where the verb takes one, a ticket ID. The
ID list comes from `kontora ls --closed`, so it includes closed tickets.

```bash
# Fish - activate in current session
kontora completion fish | source

# Fish - persist across sessions
kontora completion fish > ~/.config/fish/completions/kontora.fish
```

```bash
# Zsh - activate in current session
source <(kontora completion zsh)

# Zsh - persist across sessions. The directory has to be in $fpath before
# compinit runs, and the file has to be named _kontora.
mkdir -p ~/.zsh/completions
kontora completion zsh > ~/.zsh/completions/_kontora
# then in ~/.zshrc, above `compinit`:
#   fpath=(~/.zsh/completions $fpath)
```

```bash
# Bash - activate in current session
source <(kontora completion bash)

# Bash - persist across sessions (Linux, and macOS with Homebrew bash-completion@2)
mkdir -p ~/.local/share/bash-completion/completions
kontora completion bash > ~/.local/share/bash-completion/completions/kontora
```

The script itself runs on bash 3.2, so macOS's system bash is enough. The
directory above is where bash-completion 2.x looks; without bash-completion,
source the file from `~/.bashrc` instead.
