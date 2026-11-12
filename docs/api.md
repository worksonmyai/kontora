# API Reference

The API uses the web server's authentication. In shared-token mode, a configured `web.token` is required for `/api/*` and `/ws/*`.
Send it in `Authorization: Bearer`; browser clients can use the `kontora_token` cookie or `token` query parameter.
Without a token, ordinary access requires no credentials. The static UI and `GET /health` remain public.

API responses are JSON unless noted otherwise. Send `Content-Type: application/json` for JSON request bodies; ticket uploads use `multipart/form-data`.
Host and origin checks apply independently of authentication, including when no token is configured.
See [Remote access](remote.md) and [Host and origin checks](configuration.md#host-and-origin-checks) for setup and restrictions.

With `KONTORA_URL` and `KONTORA_TOKEN` set, this read-only request fetches statistics:

```bash
curlie GET "${KONTORA_URL%/}/api/stats" "Authorization:Bearer $KONTORA_TOKEN"
```

When the web server is enabled, the following endpoints are exposed:

| Endpoint | Description |
|----------|-------------|
| `GET /` | Static dashboard UI. |
| `GET /api/tickets` | List all board tickets (JSON). `?all=true` adds the ones whose status has no board column: archived, legacy `closed`, and any foreign status. |
| `POST /api/tickets` | Create a new ticket (JSON body: `title`, `path`, optional `pipeline`, `agent`, `status`, `body`, `branch`, `base_branch`, `scheduled_at`, `kind`, `parent`, `notify`, `notify_channels`). The notify fields are arrays; an absent `notify` inherits the project's default for ordinary tickets, while `[]` skips it. Unknown statuses or channels return 400. A `scheduled_at` creates the ticket `open` and cannot be combined with another status; one that is malformed or already past answers 400. |
| `GET /api/tickets/{id}` | Get ticket details (JSON). |
| `GET /api/tickets/{id}/cost` | Estimate the cost of finished tracked runs from their activity sidecars and the bundled model-price catalog. |
| `GET /api/tickets/{id}/activity` | Get a run's structured activity, with a plaintext log fallback. Accepts `stage`, `run`, and `after` (for live activity); supports `ETag`/`If-None-Match` revalidation. |
| `DELETE /api/tickets/{id}` | Delete the ticket markdown file without worktree cleanup. Requires `X-Kontora-Confirm: delete-ticket-file`. Only deletes files inside `tickets_dir`. |
| `POST /api/tickets/{id}/run` | Queue a ticket. On a passive epic, explicitly activate a supervisor. See [Epics](#epics). |
| `POST /api/tickets/{id}/pause` | Pause a running ticket. On an agent epic, stop only its supervisor; running children continue. |
| `POST /api/tickets/{id}/retry` | Retry a paused ticket. |
| `POST /api/tickets/{id}/skip` | Skip the current pipeline stage. |
| `POST /api/tickets/{id}/set-stage` | Move ticket to a specific pipeline stage (`{"stage": "..."}` body). |
| `POST /api/tickets/{id}/schedule` | Set or clear the ticket's future pickup time (`{"scheduled_at": "<RFC 3339>"}` or `{"clear": true}` body). A `scheduled_at` that is malformed or already past answers 400. Clearing is refused only while the ticket is running. Answers the updated ticket. See [scheduled pickup](tickets.md#scheduled-pickup). |
| `POST /api/tickets/{id}/move` | Set ticket status (`{"status": "..."}` body). |
| `GET /api/tickets/archived` | The archived tickets, one row per ticket, for the Archive view. |
| `GET /api/stats` | Aggregate ticket and run statistics (JSON). Optional filters: `range` (`1d`, `1w`, `30d`, `90d`, or `all`; default `90d`), `project`, and `pipeline`. |
| `POST /api/tickets/{id}/archive` | Archive a closed ticket (optional `{"note": "..."}` body). |
| `POST /api/tickets/{id}/restore` | Return an archived ticket to the status it was archived from. |
| `GET /api/config` | Available pipelines, agents, and projects (JSON). `agents` remains the sorted agent-name list; `agent_infos` adds sorted `{name, model, effort}` metadata for each agent. The top-level `default_pipeline` exposes the global pipeline default. Projects are sorted by name and carry `name`, `path`, `resolved_path` (`~` expanded), `pipeline`, `epic_child_pipeline`, `agent`, and `notify_channels`. `channels` and `default_channels` are the configured notification channel names and `notifications.default`; names only, never a chat id or a webhook URL. |
| `GET /api/config/raw` | Read the full on-disk YAML configuration as JSON `content`, without redaction. See [Raw configuration](#raw-configuration). |
| `PUT /api/config/raw` | Validate, replace, and reload the configuration using `content` and optional `expected_content`. See [Raw configuration](#raw-configuration). |
| `GET /api/tickets/{id}/logs` | Get agent logs for a ticket (optional `?stage=` query param). |
| `POST /api/tickets/{id}/summary` | Set the ticket's `summary` field (`{"text": "..."}` body). |
| `GET /api/tickets/{id}/changes` | Commits and changed files on the ticket's branch relative to its `base_branch`, or the repo's default branch when unset. Empty payload when the ticket has no branch or the branch was deleted. |
| `GET /api/tickets/{id}/chain` | The dependency chain through the ticket: everything it transitively waits on, itself, and everything that transitively waits on it. |
| `POST /api/tickets/{id}/init` | Initialize a non-kontora ticket (`path`, optional `pipeline`, `agent`, `branch`, `status`, `notify`, `notify_channels`). `status` is `todo` by default; `open` leaves the ticket a draft and keeps any `scheduled_at` it carries. See [notifications](tickets.md#notifications) for the two notify fields. |
| `POST /api/tickets/{id}/dep` | Make the ticket wait on another one (`{"related": ["<id>"]}` body, exactly one id). |
| `POST /api/tickets/{id}/undep` | Drop a dependency edge (`{"related": ["<id>"]}` body, exactly one id). |
| `POST /api/tickets/{id}/link` | Relate the ticket to each id in `{"related": [...]}`, on both sides. |
| `POST /api/tickets/{id}/unlink` | Remove the relation between the ticket and each id in `{"related": [...]}`. |
| `POST /api/tickets/{id}/parent` | Set a child's epic (`{"parent":"EPIC_ID"}`). Agent epics require the same repository. |
| `POST /api/tickets/{id}/unparent` | Clear a child's parent. |
| `PUT /api/tickets/{id}/children` | Set an epic's display order (`{"children":["CHILD_ID"]}`). Does not change membership or execution order. |
| `PUT /api/tickets/{id}` | Update an editable ticket's description, full body or frontmatter fields (`description`, `body`, `pipeline`, `path`, `agent`, `branch`, `base_branch`, `notify`, `notify_channels`). On the two notify fields an absent key leaves the ticket's own alone and `[]` removes it; a status nothing reaches or a channel nothing answers to is refused with 400. See [notifications](tickets.md#notifications). |
| `POST /api/tickets/upload` | Import tickets from raw `.md` file content (multipart form). Requires `X-Kontora-Confirm: upload-tickets`. Every uploaded ticket arrives as an `open` draft: the status is clamped and any `scheduled_at` is dropped. |
| `POST /api/tickets/{id}/plannotator-review` | Open the ticket's branch diff in Plannotator. Only in `human_review`. Submitted feedback routes the ticket to the built-in rework stage. See [plannotator](configuration.md#plannotator). |
| `POST /api/tickets/{id}/plannotator-annotate` | Open the ticket's own markdown in Plannotator. Only in `open`. Submitted annotations set `kontora: true` and schedule a run that rewrites the ticket. |
| `GET /api/assistant` | Whether the [assistant](configuration.md#assistant) is configured: `enabled`, and when it is, `agent`, `kind`, `model`, `autonomy` and `workdir`. `enabled: false` carries a `hint` naming what to set, which is what the pane shows in place of its composer. |
| `GET /api/assistant/companion` | Companion health, revision, current attention and recent changes. Available without `assistant.agent`. |
| `PATCH /api/assistant/companion` | Record an explicit caught-up checkpoint (`{"revision": 12}`). |
| `PATCH /api/assistant/companion/items/{id}` | Set shared read state using the item's revision (`{"revision": 7, "read": true}`). |
| `POST /api/assistant/companion/messages` | Request a briefing, investigation or follow-up. See [Companion](#companion). |
| `GET /api/assistant/threads` | The chat history, most recently used first. |
| `POST /api/assistant/threads` | Open a chat (optional `{"autonomy": "read"\|"ask"\|"auto"}`; the configured default otherwise). Answers 201 with the chat. |
| `GET /api/assistant/threads/{id}` | One chat and the messages posted to it. |
| `DELETE /api/assistant/threads/{id}` | Drop a chat and its transcript. |
| `GET /api/assistant/threads/{id}/activity` | One poll of a chat: the transcript as a `logfmt` tape sliced at `?after=N`, the messages, whether a turn is running, the message being written right now, and any change waiting on the user. Carries an `ETag` and answers `If-None-Match` with 304. |
| `GET /api/assistant/threads/{id}/stream` | Server-Sent Events stream of the message being written, pushed as it grows. Answers 204 when nothing is running and nothing was typed. |
| `POST /api/assistant/threads/{id}/messages` | Post an ordinary chat message (`{"text": "...", "autonomy": "..."}`). Answers 202: the reply arrives through the activity poll. Companion-purpose threads reject this route. |
| `POST /api/assistant/threads/{id}/stop` | Cancel the turn the chat is running. |
| `POST /api/assistant/gate/{gid}` | Answer a change the assistant is waiting on (`{"decision": "approve"\|"skip"}`). |
| `POST /api/assistant/gate/ask` | The agent side of the tool gate, called by the claude `PreToolUse` hook and the pi `tool_call` handler. Authenticated by the per-turn secret in the agent's environment, not by the web token alone. Blocks while a change waits on the user. |
| `GET /api/automations` | Definition list and availability. See [Automations](#automations). |
| `POST /api/automations` | Create a configuration entry with a persistent request identity. |
| `POST /api/automations/preview` | Validate a schedule and return four future timestamps and a description. |
| `GET /api/automations/{id}` | Full automation definition and statistics. |
| `PUT /api/automations/{id}` | Replace editable fields using the current revision. |
| `DELETE /api/automations/{id}` | Remove the configuration entry and stop unfinished work. |
| `POST /api/automations/{id}/run` | Request a manual run with an idempotency identity. |
| `POST /api/automations/{id}/stop` | Stop a specific run. |
| `POST /api/automations/{id}/pause` | Disable future scheduled runs. |
| `POST /api/automations/{id}/resume` | Enable future scheduled runs. |
| `GET /api/automations/{id}/runs` | Paginated run history, newest first. |
| `GET /api/automations/{id}/runs/{n}/log` | Structured and raw logs; `?raw=1` downloads plain text. |
| `GET /api/automations/{id}/runs/{n}/terminal` | Resolve the run's terminal identity on the server. |
| `GET /ws/automations/{id}/{n}` | WebSocket terminal for a server-resolved automation run. |
| `GET /api/events` | Server-Sent Events stream of ticket updates, companion invalidations, and automation invalidations. |
| `GET /ws/terminal/{id}` | WebSocket terminal for a running agent. Read-only by default; `rw=1` enables input. See [Terminal streams](#terminal-streams). |
| `GET /health` | Health check (returns 200). |

Each `agent_infos` entry in `GET /api/config` reports the model and effort that Kontora detects. Kontora reads the agent's `effort` field and supported CLI flags. Pipeline and stage overrides are not applied. Both response fields are always present. An empty value means Kontora detected no setting.

`GET /api/config` also exposes optional runtime identity:

| Field | Meaning |
|-------|---------|
| `tickets_dir` | Absolute directory for the running ticket store, with symlinks resolved. Omitted if unavailable. |
| `tmux_session` | The running daemon's tmux session name. |
| `tmux_socket` | The existing server's absolute socket path, discovered in the background. Omitted until discovery succeeds or if the session disappears. Discovery does not start a server. |

The daemon captures directory identity when it opens the store. Retargeting a store symlink or editing restart-only settings does not change that identity. Socket discovery does not delay config responses. Automatic local TUI connections require matching filesystem directory identity. Missing identity, including responses from older daemons, selects read-only file mode. A missing socket disables attachment without disabling ticket actions on a verified board. Explicit remote clients do not require these fields. Initial verification does not protect a long-lived client against server replacement at the same address.

### Raw configuration

`GET /api/config/raw` returns `{"content":"<YAML>"}` containing the daemon's on-disk configuration.
The response is not redacted and can include tokens, webhook URLs, and other credentials stored in the file.
Treat the response as sensitive.

`PUT /api/config/raw` accepts a JSON object:

- `content` is the complete replacement YAML, not a partial update.
- Optional `expected_content` must exactly match the current file contents. A mismatch returns `409` without saving.
  Omitting it or sending `null` does not protect against changes since the client's last read.

PUT validates a single YAML document before saving and reloading it. Invalid configuration returns `400`; success returns `204`.
There is no check-only mode: a `check` field or query parameter does not prevent saving.
[Restart-only settings](configuration.md#what-needs-a-restart) keep their running values until restart.
If reload fails after saving, the file remains changed even though the request returns an error.

### Automations

See [Automations](automations.md) for timing, overlap, workspaces, and recovery.
All automation routes use the existing shared-token, Host, and origin protections.
Without a configured token or authority mode, ordinary local access remains available.
With authority mode enabled, automation routes require an operator credential in `X-Kontora-Operator-Token`.
Worker attempt capabilities cannot read or mutate automations.

Send `Content-Type: application/json` for request bodies.
Automation handlers accept one JSON object, limited to 512 KiB, and reject unknown fields and trailing JSON values.
The limit allows JSON escaping of a 65,536-byte prompt.
Errors use `{"error":"..."}`:

| Status | Meaning |
|--------|---------|
| `400` | Invalid fields, schedule, JSON, run number, or pagination. |
| `404` | Definition or run not found. |
| `409` | Stale revision, conflicting request identity, or an external configuration change. |
| `503` | Automation storage unavailable. |

#### Definitions and creation

`GET /api/automations` returns `{"available":true,"definitions":[...]}`.
An empty list is `[]`, never `null`.
If the optional service or storage is unavailable, the list still returns `200`:

```json
{"available":false,"error":"automation storage unavailable","definitions":[]}
```

Other management and history routes return `503` when unavailable. Stateless preview remains available.
`GET /api/automations/{id}` returns a definition directly.

`POST /api/automations` accepts:

```json
{
  "request_id": "report-creation-1",
  "key": "morning-report",
  "definition": {
    "name": "Morning repository report",
    "prompt": "Inspect recent changes and report potential regressions.",
    "path": "/srv/projects/app",
    "agent": "claude",
    "model": "",
    "schedule": "0 9 * * 1-5",
    "timezone": "Europe/Berlin",
    "workspace": "worktree",
    "overlap": "skip",
    "timeout": "30m",
    "enabled": false
  }
}
```

`key` is optional. An omitted key is derived from the name.
Explicit keys must use lowercase kebab-case, start with a letter, and contain at most 100 bytes.
`request_id` must be nonblank and at most 200 bytes. Keep it unchanged when retrying the same creation.
Matching retries return the recorded definition; different inputs with the same identity return `409`.
Success returns `201`, including on a retry.

Only the fields shown inside `definition` are editable.
Defaults are `timezone: UTC`, `workspace: worktree`, `overlap: skip`, `timeout: 30m`, and `enabled: false`.
Use a configured agent and repository. A model override requires a supported agent.
`workspace` is `worktree` or `repo`; `overlap` is `skip`, `queue`, or `replace`.
See [schedules](automations.md#schedules) for numeric cron and whole-minute intervals.
Read-only fields such as `id`, `revision`, `next`, and terminal targets are not accepted inside `definition`.

Responses embed the specification and add:

| Field | Meaning |
|-------|---------|
| `id`, `key`, `revision`, `created_at` | Server identity, configuration key, editable revision, and creation time. |
| `next`, `next_runs`, `schedule_human` | Schedule cursor, future timestamps, and a readable description. |
| `project` | Matching configured project. |
| `running`, `current_run`, `last_run` | Current execution state and run summaries. |
| `runs_total`, `success_rate`, `avg_wall` | Run count, nullable success percentage, and nullable average wall time in seconds. |
| `spend_30d`, `priced_runs`, `tracked_runs` | Nullable decimal cost estimate and its coverage counts. Unknown cost is not zero. |

`PUT /api/automations/{id}` accepts `{"revision":7,"definition":{...}}`.
Send the complete editable specification and current nonzero revision. PUT is not a partial update.
Success returns `200` with the updated definition.
Cursor advancement does not change the editable revision. Clients cannot set the cursor.
A stale revision or changed configuration returns `409` without overwriting the file.
Refetch and compare saved fields with the draft before retrying.

`DELETE /api/automations/{id}` accepts `{"revision":7}` and returns `204`.
Deletion removes the YAML entry and stops unfinished work. Historical runs and artifacts remain.

`POST /api/automations/{id}/pause` and `/resume` accept `{"revision":7}` and return the updated definition with `200`.
Pause changes `enabled`, not the current run. Resume selects a future schedule slot.

#### Schedule preview

`POST /api/automations/preview` accepts:

```json
{"schedule":"0 9 * * 1-5","timezone":"Europe/Berlin"}
```

`timezone` defaults to `UTC`. Use an IANA name; `Local` is invalid.
Numeric five-field cron and intervals of at least one whole minute are supported.
`1m`, `90m`, and `2h` are valid; `30s`, `90s`, and `@daily` are invalid.
Success returns `200` with four RFC 3339 timestamps and a readable description:

```json
{"occurrences":["2030-01-02T09:00:00+01:00","2030-01-03T09:00:00+01:00","2030-01-04T09:00:00+01:00","2030-01-07T09:00:00+01:00"],"human":"weekdays at 09:00"}
```

Timestamps are after the current server time. Preview does not save a definition or start a run.

#### Runs and logs

`POST /api/automations/{id}/run` accepts `{"request_id":"manual-run-1"}` and returns a Run with `202`.
The request identity is nonblank and at most 200 bytes. Matching retries return the same run without another launch.
Manual requests work while paused and follow the saved overlap policy.
A `202` response means accepted, not completed; overlap can produce a skipped run.

`POST /api/automations/{id}/stop` accepts `{"n":12}` and returns the run with `202`.
The positive run number selects the target. Stopping does not prove termination; inspect the returned phase and subsequent history.

`GET /api/automations/{id}/runs?offset=0&limit=20` returns `{"runs":[],"total":0}`.
History is newest first. `offset` defaults to `0`; `limit` defaults to `20` and must be between `1` and `200`.
An offset beyond the last run returns `[]` with the unchanged total.
Invalid, negative, overflowing, or repeated pagination parameters return `400`.

Run fields include:

- `automation_id`, `key`, `n`, `revision`, and `invocation_id`.
- `phase`: `queued`, `preparing`, `running`, `stopping`, `terminal`, or `recovery_required`.
- `outcome`: empty until terminal, then `complete`, `failed`, or `skipped`.
- `trigger`: `schedule` or `manual`; `scheduled_for`, `accepted_at`, `started_at`, and `completed_at` timestamps.
- Nullable `exit_code`, `observed_exit_code`, and decimal-string `cost`; `result` contains the summary or failure reason.

Operator responses can also expose the frozen `spec`, workspace, branch, artifact paths, and exact terminal metadata.
These fields are server-owned. Clients cannot select process targets through mutation bodies.

`GET /api/automations/{id}/runs/{n}/log` returns `{"lines":[...],"raw":"..."}`.
Each line contains `at`, `source` (`kontora` or `agent`), `kind`, and `text`.
Kinds are `lifecycle`, `text`, `tool_call`, `tool_result`, `success`, `warning`, and `error`.
`?raw=1` returns `text/plain; charset=utf-8` instead.

`GET /api/automations/{id}/runs/{n}/terminal` resolves the saved target.
`GET /ws/automations/{id}/{n}` opens its terminal WebSocket. `rw=1` enables input; the default is read-only.
The server verifies the saved terminal identity. Client-supplied session or window names do not select a target.

The `automations_updated` Server-Sent Events event contains `{}`.
Refetch definitions and visible history after invalidation or reconnect, without replacing unsaved drafts.

### Terminal streams

`GET /ws/terminal/{id}` accepts `cols` and `rows` for the initial terminal size
(default 80 by 24, maximum 1000 each). Without `stream=pane`, binary frames contain
the tmux-rendered attach stream. Existing CLI clients can keep using that protocol.

With `stream=pane`, binary frames contain the agent pane's raw terminal output.
The first frame is a JSON text message: `{"type":"reset","cols":200,"rows":50}`.
Reset and resize the terminal to those dimensions before processing the following
binary frames. They restore up to 5000 history lines and the visible screen, then
continue with live output. Another reset and snapshot follow a pane size change
or an output queue overflow. Binary frames are at most 32 KiB.

Clients send JSON text messages:

- `{"type":"resize","cols":200,"rows":50}` requests a new size for the shared pane.
  The most recent viewer to request a size controls that pane's dimensions.
- `{"type":"input","data":"hello\n"}` sends UTF-8 input only with `rw=1`.
  Read-only pane streams discard input messages.

Pane-stream clients must suppress replies to `CSI c`, `CSI > c`, `CSI n`,
`CSI ? n`, `CSI $ p`, `CSI ? $ p` and `DCS $ q`.
Suppress OSC 4/10/11/12 only when every colour specification is `?`.
The browser registers xterm.js parser handlers for these queries. tmux answers
supported queries itself; unmatched palette queries remain unanswered.
Focus reports remain input. Inline applications get local scrollback;
applications using the alternate screen do not. Snapshots cannot restore the
current text attributes, saved cursor, character sets or custom tab stops from tmux.
With tmux before 3.7, snapshots also reset bracketed paste and synchronized output
because tmux does not expose those modes.

### Secondary ticket sources

Existing `/api/tickets` routes remain primary-only. Secondary operations use the
configured source ID, never a client-supplied filesystem path.

| Endpoint | Description |
|----------|-------------|
| `GET /api/ticket-sources` | Configured source IDs, paths, availability, and diagnostics. |
| `GET /api/sources/{source}/tickets` | Secondary tickets across all statuses. |
| `GET /api/sources/{source}/tickets/{id}` | Source-local detail, relations, revision, and capabilities. |
| `PUT /api/sources/{source}/tickets/{id}` | Existing editable fields, regardless of status. Rejects unknown or lifecycle fields. |
| `POST /api/sources/{source}/tickets/{id}/claim` | Claim into primary using `{"revision":"REVIEWED_REVISION"}`. Returns primary detail without starting execution. |

Source-qualified note, reaction, summary, dependency, link, parent, and child-order
routes use the same suffixes and request bodies as primary routes. Relationships
resolve only within that source. Lifecycle and artifact routes are refused; they do
not fall back to primary tickets with the same ID.

Secondary payloads add `source`, `revision`, `can_edit`, `can_control`, `can_claim`,
`refusal_reason`, and `claim_warnings`. Capabilities are advisory; each mutation
rechecks permissions and stored state. `can_control` is false for secondary tickets.
Unavailable sources refuse edits and Claim. Controlled boards remain browse-only.

Claim checks both boards under local locks and refuses revision changes, running
markers, unsafe files, destination collisions, and reserved primary identities.
See [Claim rules and coordination limits](tickets.md#claim-from-a-secondary-folder).
Imported history rows expose `imported: true`; activity lookup refuses those runs,
and cost/statistics enrichment excludes them without reusing their run indexes.

Secondary updates use `secondary_ticket_updated` and `secondary_ticket_deleted`
SSE events. Their ticket payload includes both `source` and `id`. Clients must not
replace a primary namesake on receipt of a secondary event.

Assistant message requests naming a secondary `source` are refused before agent
admission. Secondary-context assistant workflows are unsupported.

### Stage changes

`skip` and `set-stage` return 409 while execution or Plannotator review owns the ticket.
Pause first, wait for termination, and close the review. A `paused` response alone does not confirm teardown finished.

On a stopped ticket, `skip` queues the next stage. On the final stage, it marks the ticket done without successful run history.
`set-stage` leaves status and attempt unchanged and does not retry. An existing `todo` ticket remains eligible for pickup.

A successful `retry` response means accepted and queued. Replacement cleanup runs before workspace preparation.
If tmux inspection or cleanup cannot establish termination, the daemon refuses replacement and keeps its execution reservations.

### Epics

`POST /api/tickets` with `kind: "epic"` creates a passive epic in `open`.
Creation never starts an agent. A passive epic can store agent and branch settings,
but has no pipeline, worktree or run history. Their status summarizes children, and their brief stays
editable in every status. Epics cannot nest.

`POST /api/tickets/{id}/run` explicitly activates a passive epic. Initialization
with explicit `{"status":"todo"}` also activates it; omit pipeline and stage.
Activation writes `epic_execution: "agent"`, resolves the supervisor agent and
queues the epic. The epic and every child must use the same Git repository.
Linked worktrees and symlink paths to that repository are allowed. Passive epics
can group different repositories until activation. Draft edits never activate them.

Ticket payloads include `kind`, `epic_execution`, `branch` and `base_branch`.
`epic_execution: "agent"` distinguishes an activated epic from passive grouping.
`child_order` is the stored display order. Detail payloads also include the
derived `children` array; membership comes from each child's `parent`.
The CLI's `ls --json` exposes the execution and Git fields and `child_order` too.

`TicketInfo.epic_child` is a derived boolean on list, detail and event payloads.
It is true for an ordinary ticket whose parent is an epic in the daemon's current
index, regardless of passive or agent execution. It is an ephemeral preview
snapshot, never stored frontmatter or authoritative default resolution.
Creation and initialization resolve actual membership themselves.

The supervisor runs standalone, without a pipeline, in the epic's own branch and worktree.
Children retain their own pipelines, branches, worktrees and commits. Pause stops only the supervisor;
running children continue. Agent epics use ordinary edit restrictions while
running or terminal. A complete supervisor result with clean execution, confirmed
termination, and successful final validation parks the epic in `human_review`,
never directly in `done`. Kontora requires a nonempty child set, with every child
`done`, legacy `closed` or `human_review`. Cancelled and archived children do not
pass. This does not prove integration, verification claims or human acceptance.
Plannotator reviews the combined epic diff; feedback starts supervisor rework.
The epic records supervisor history, summaries, logs, activity and sessions.

Supervisors use `max_concurrent_epic_supervisors`, default `4`, separately from
worker capacity. This permits four additional agent processes. Dependencies
control scheduling, not the commits present in a child's Git base. Local delivery
is limited to an activated supervisor and its actual children, after verification
of ticket fields, the current ticket's branch, non-detached HEAD, and invocation
worktree. Narrower user, ticket, and stage instructions win; annotation runs get
no implementation authority. A run may commit on its own verified branch and
rebase only that branch onto its base (default branch if unset). A supervisor may
merge child branches into its own branch, not rebase children; a child may merge
the epic's branch or its own base into its own branch. Never write on `main`,
`master`, or the default branch, modify a running child's worktree, discard
unrelated work, or rewrite remote/default-branch or published/shared history.
Declared dependencies and project/ticket-pinned tools may be set up in worktrees
or temporary directories (including package-manager caches). Global installation,
real-account/device access, and service exposure require approval.

Supervisors and children must investigate prerequisites, reuse saved decisions,
resolve permitted local setup, and continue independent work. Escalations name
attempted checks, a concrete resource/authority/scope question, options, costs,
and a recommended default. Save answers immediately. Do not silently weaken
requirements or replace required real-device evidence with emulation. If no
authorized work remains, submit blocked. Retry requires explicit operator action;
saving an answer, reading a notification, or dismissing attention does not restart
execution. Publication and off-machine writes remain prohibited, including through
children, scripts, or tools; read-only lookups are allowed. These authority limits
are agent instructions, not an OS security boundary.
Supervisor prompts prohibit changing, clearing or replacing child pipelines and
editing pipeline definitions, stage definitions or project pipeline defaults.
Supervisors must omit pipeline overrides when creating children. They must not bypass the
workflow through `init`, stage skipping, direct file edits or pause/retry sequences.
If configured work conflicts with their authority, they must report the conflict,
request an operator decision and leave that work unstarted. They must not substitute
another pipeline. These are prompt instructions, not runtime enforcement or
restrictions on ordinary operator APIs.
A default pipeline does not authorize commits or publication.
See [Epics](tickets.md#epics) for integration rules and safety limits.

Upgrade every daemon and client sharing the ticket directory before activation.
Older versions treat all epics as passive and can overwrite supervisor status.
Do not run mixed versions against activated epics.

### Supervised outcomes and recovery

Activated epic supervisors and their actual children, including child rework,
require invocation-bound `complete` or `blocked` results. This is not an additional
HTTP lifecycle endpoint: agents use Pi's `kontora_result` or `kontora result`
with daemon-bound execution context. See [schema, environment, and CLI transport](cli.md#supervised-results).
Unrelated tickets, passive-epic children, annotations, and assistant-only runs
retain their existing result requirements. Completing a stage is not whole-ticket
completion or human acceptance.

Blocked results and missing/invalid results after a clean supervised run pause
the same stage, retaining worktree and retry count. Complete reports do not
override execution/provider failure, hooks, or child validation. History exposes
optional `run_outcome`, `outcome_reason`, and `invocation_id` separately from
`exit_code` (runner exit status, or `-1` when recovery could not observe it).
Outcomes are final after hooks and child checks.
Rows without semantic outcomes are legacy, unverified task outcomes; exit zero
or old prose must not be reinterpreted as verified completion. The same distinction
applies to summaries, statistics, and metrics. Standalone supervised runs with
real history are not also counted as synthetic legacy runs.

Recovery reconciles invocation metadata, reports, and finalization before
replacement. It confirms termination first; uncertainty retains reservations and
prevents branch reuse. A marker precedes hooks, and a replayable transition is
persisted after hooks and child checks. Replay uses invocation identity to avoid
duplicate history and does not rerun hooks. Unknown hook effects pause for
inspection. Atomic ticket replacement must succeed before metadata is retired.
Foreign ownership and operator lifecycle changes take precedence. See
[recovery rules](configuration.md#supervised-recovery) for all interrupted states.
This covers daemon-process recovery, not exactly-once shell effects, exactly-once
telemetry across restart, or host-power-loss durability. Upgrade CLI and daemon
together; do not restart live epics or migrate ticket state as part of rollout.

### Companion

The companion endpoints use the same authentication as the other web endpoints.
`GET /api/assistant/companion` returns a snapshot with:

- `enabled`, `name`, `ai_available` and a configuration `hint` when needed;
- `health` and an `error` when storage is degraded;
- `revision`, the snapshot revision, and `caught_up`, the saved checkpoint;
- `observed_at`, the last observation time;
- `items`, the current attention and retained inactive records, including read
  state and item revisions.

The inbox and read state are shared across devices, not isolated per person.
The snapshot remains available without a configured model agent. Only requested
AI actions need `assistant.agent`.

`PATCH /api/assistant/companion` requires `{"revision": 12}`, using the snapshot
revision the user explicitly caught up to.
`PATCH /api/assistant/companion/items/{id}` requires `{"revision": 7, "read": true}`
or `read: false`, using that item's revision. Both return the updated snapshot.
If an item revision is stale, refetch before retrying. Neither operation changes a
ticket, answers a question or completes a review. Storage failures refuse
updates that cannot be saved; task execution and notifications continue.

`POST /api/assistant/companion/messages` accepts:

```json
{
  "kind": "investigation",
  "text": "Why is this ticket paused?",
  "ticket_ids": ["kon-example"]
}
```

`kind` must be `briefing`, `investigation` or `followup`.
`text` must be non-blank and at most 32 KiB (32,768 UTF-8 bytes).
`ticket_ids` accepts at most five tickets. An investigation requires at least one; other request kinds can omit it. Optional `thread_id`
continues a companion-purpose thread; omitting it creates one. An accepted
request returns 202 with the thread, including its ID. Use the existing thread,
activity, stream, Stop and delete endpoints for the conversation.

Kontora builds evidence server-side and saves it before starting the model.
Each request has a 32 KiB evidence limit. Capture metadata includes times,
source revisions, observation windows, omissions and an outdated marker when
sources change. Metadata remains available during streaming and after reopening
the chat. Historical explanations do not change current attention.

Companion threads have immutable `purpose: "companion"`. The ordinary message
route rejects them, so clients cannot bypass evidence capture. Companion turns
deny every tool call and autonomy escalation. They cannot mutate tickets,
execute approvals or send outbound messages. Enforcement uses runtime hooks
with a trusted configured agent, not an OS sandbox.

After successful persistence, `GET /api/events` sends a payload-free
`companion_updated` invalidation. Clients must refetch the snapshot on connection,
reconnection, focus and a periodic fallback. The event stream is not an inbox
history. Startup, observation, reload and reconnection never start model turns.

The store retains all active items plus at most 500 inactive records, each no
older than 30 days. Degraded health means the store cannot provide normal
persistence guarantees. Disabling the companion on reload retains data and
blocks new requests; an already-running requested turn remains stoppable.
Polling can miss short-lived transitions, and offline events are not a complete
history. A disappeared question is not proof that someone answered it.
See [companion configuration](configuration.md#companion-inbox-and-requested-explanations).

### Cost estimates

`GET /api/tickets/{id}/cost` returns the known ticket subtotal and its coverage:

```json
{
  "id": "kon-example",
  "cost_usd": "0.012345",
  "priced_runs": 2,
  "tracked_runs": 3,
  "stages": [
    {"name": "implement", "cost_usd": "0.012345", "priced_runs": 2, "tracked_runs": 3}
  ],
  "runs": [
    {
      "history_index": 0,
      "stage": "implement",
      "run": 0,
      "model": "openai-codex/gpt-5.6-sol",
      "model_source": "sidecar",
      "sidecar_model": "openai-codex/gpt-5.6-sol",
      "history_model": "openai/gpt-5.6-sol",
      "resolved_model": "openai/gpt-5.6-sol",
      "cost_usd": "0.012345"
    },
    {"history_index": 1, "stage": "implement", "run": 1, "cost_usd": null}
  ]
}
```

Every `cost_usd` is a decimal US-dollar string or `null`. A measured free run
returns `"0.000000"`. An unpriced run returns `null`. A ticket or stage with
partial coverage returns its known subtotal and reports fewer `priced_runs`
than `tracked_runs`. Its subtotal is `null` only when none of its runs can be
priced. `sidecar_model` and `history_model` preserve the source values when
present; `model` is the value chosen for resolution. An unknown ticket returns
404.

Run sidecars are matched by their position among all rows for a stage. This also
covers old History rows and annotation rows whose stored `run` field does not
identify the sidecar. `history_index` addresses the row in the ticket's History
array. It is `null` for the synthetic `default` run of an ended standalone
ticket, which has a sidecar but no History row.

Kontora calculates estimates from fresh input, output, cache-read, and
cache-write tokens. It uses the sidecar model first and the History model as a
fallback. Kontora leaves a run unpriced if:

- usage is missing or partial;
- the stage name is empty, `.`, `..`, or contains `/` or `\`;
- the model is unknown;
- a token count is negative; or
- a used token category has no rate.

The bundled catalog is a snapshot of OpenRouter's [Models
API](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties).
Model pricing data is provided by [OpenRouter](https://openrouter.ai/). The
snapshot uses standard top-provider text-token rates and ignores conditional
long-context, time-window, request, tool, and media prices. Kontora applies the
snapshot in the current binary to old runs, so an estimate can change after an
upgrade and is not a bill.

The estimate excludes calls that do not write complete activity sidecars. This
includes final-summary runs, the Plannotator subprocess, and nested Pi tool
calls. If a sidecar records a model change, Kontora prices the entire run at the
final recorded model because the sidecar stores one run total, not totals per
model.

Maintainers can refresh the committed offline snapshot with:

```bash
make model-prices
```

The command fetches the OpenRouter Models API, validates and sorts the retained
records, then replaces `internal/pricing/catalog.json` atomically. Builds and
tests never fetch the catalog.

### The message being written

`GET /api/assistant/threads/{id}/activity` carries three fields for the message
the agent is writing before its session file records it: `partial`, the text so
far; `partial_gen`, bumped when a new block starts, so a reader replaces rather
than appends; and `partial_tool`, the tool call whose arguments are still being
generated. `partial` is empty once the tape carries the same words, and the
response that empties it is the one that carries them, so nothing renders twice
and nothing blanks between the two. A turn that ended without recording its
message keeps its `partial` alongside `running: false`, since that copy is then
the only one there is. The text is held in daemon memory only: it is never part
of the `logfmt` tape, which is the parsed form of a file.

`GET /api/assistant/threads/{id}/stream` pushes the same text at roughly ten
frames a second, so a reader sees it as typing rather than in 1.5s steps. It is
an enhancement over the poll: a client that cannot open it still renders growing
`partial` text from successive polls.

```
event: reset   data: {"gen":3,"text":"<everything so far>","tool":""}
event: delta   data: {"gen":3,"text":"<suffix since the last frame>"}
event: tool    data: {"gen":3,"name":"Bash"}
event: end     data: {}     # the block stopped growing; the text stays
event: done    data: {}     # the turn is over and the server is closing
: keepalive                 # after 20s of silence
```

A `reset` arrives on connect and again whenever `gen` changes; a client ignores
a `delta` whose `gen` it does not hold. The connection answers 204, rather than
opening, when nothing is running and nothing was typed: `EventSource` does not
retry a 204, so a stale pane stops asking. Like `GET /api/events`, the response
is never gzipped.

For ordinary tickets, `POST /api/tickets` and `POST /api/tickets/{id}/init` resolve
`pipeline` in this order:

1. An explicit request value, including `none` for standalone execution.
2. A stored nonempty pipeline, on initialization only.
3. The matching project's `epic_child_pipeline`, when the resolved parent is an actual epic.
4. The matching project's `pipeline`.
5. The global `default_pipeline`.
6. Standalone execution.

Project matching uses the final request-overridden `path`. Passive and agent epics
both qualify as parents; missing or ordinary-ticket parents do not. Epics themselves
never receive a pipeline. See [projects](configuration.md#projects) for lexical path matching.

An omitted or blank `agent` preserves the stored agent during initialization, then
uses the project agent default. Pipeline and agent selection are independent:
`"pipeline":"none"` does not skip agent resolution. The literal `none` clears the
selected field and skips its defaults for this request. Kontora stores a blank,
not durable opt-out provenance. A later initialization can fill it unless the
caller sends `none` again.

Browser inheritance omits `pipeline`; explicit standalone sends `"pipeline":"none"`.
A named choice sends the configured name, even when it equals the inherited preview.
Browser choice tokens such as `pipeline:NAME` are not API pipeline names.
The backend resolves current defaults rather than trusting a preview snapshot.

`PUT /api/tickets/{id}` treats `none` as "clear this field" and applies no defaults.

Existing assignments are not migrated when config or membership changes. Changing
`path` in ticket details preserves the stored pipeline; retarget it explicitly.

Both Plannotator endpoints return 202 once the session is accepted, and report its outcome over `GET /api/events` as a `plannotator_finished` event. They return 404 for an unknown ticket, 409 when a Plannotator process is already open for the ticket or its status does not allow the requested pass, and 500 when the `plannotator` binary is not installed. `GET /api/tickets` and `GET /api/tickets/{id}` carry `can_annotate`, which answers the same status rules the annotate endpoint enforces.

## Ticket event stream

`GET /api/events` reports changes over SSE. It is not an initial snapshot or a durable
change log. `ticket_updated` carries the changed ticket's projection;
`ticket_deleted` removes that ticket. Secondary ticket events include `source`
and must be keyed by `(source, id)`, independently of primary tickets. An
unchanged secondary observation emits no secondary ticket updates.

Ticket update events omit `body`, `description`, and `description_tail`.
Fetch `GET /api/tickets/{id}` for primary detail or
`GET /api/sources/{source}/tickets/{id}` for secondary detail rather than
replacing a loaded document with an event. An omitted body field does not
mean the description was cleared.

There are no replay IDs or `Last-Event-ID` recovery guarantees. Each subscriber
has a 64-event queue; overflow disconnects that slow subscriber rather than
silently continuing with a gap. Other subscribers are unaffected. After an SSE
connection opens or reconnects, clients must refetch their authoritative lists
(including the archive when open) and any open detail. Events received during
that refresh must not be overwritten by an older snapshot. A reconnect cannot
recover intermediate transitions, and this stream is not an audit trail.

Relations ride both ticket payloads as arrays of `{id, title, status}`. `GET /api/tickets` carries the id alone; `GET /api/tickets/{id}` and the SSE ticket updates also carry the title and status of every ticket the daemon still has on disk, and add the two derived reverse edges: `blocks`, the tickets whose `deps` name this one, and `children`, the tickets whose `parent` names it. `blocks` is sorted by id. Epic children follow the stored display order, then creation order. Both are absent from the list payload. Dependency refs may include `source` when they resolve in a configured secondary folder. Other relations remain folder-local. A ref with no title and no status has no resolved match. See [Relations](tickets.md#relations).

`GET /api/tickets/{id}/chain` is the derived view of the same edges. It walks the whole primary store, so archived tickets and statuses with no board column stay in the graph. Unique secondary prerequisites appear as leaf nodes with their title, status, and `source`; their own relations are not traversed. Use the source-qualified ticket endpoint to open them. Secondary nodes cannot be run or resumed by the primary daemon. `nodes` is sorted roots to goal with `depth` monotonic non-decreasing, and every upstream node comes before the ticket itself, which comes before every downstream node. Each node carries `depth`, `direction` (`upstream`, `self` or `downstream`), `on_critical_path`, `holds_chain`, `waits_on` (`{open, total}` over that node's own `deps`, including the deps outside the chain) and `missing`. At most one node has `holds_chain` set: the node the chain waits on. While a dep blocks the ticket that is an unresolved dep upstream with no unresolved dep of its own, so it names work that can start now; while nothing blocks the ticket it is the first unresolved node from the ticket onward along the critical path, which is the ticket itself until it closes. A chain whose nodes are all resolved has no such node.

`verdict` is `blocked`, `ready` or `cycle`. It reports the deps alone rather than the ticket's own status, so a `done` ticket with closed deps reads `ready`. `position`, `path_length` and `goal` place the ticket on the critical path. On `cycle` the ids on the cycle are in `cycle`, `nodes` is empty, and `position`, `path_length` and `goal` are zero: a graph with a cycle has no order to draw. `total` is the true node count and `nodes` is capped at 200, so a `total` larger than the length of `nodes` means the payload was trimmed; the critical path is kept whatever the cap. `done` counts the nodes that no longer block, which includes the ones in `human_review` and the cancelled and archived ones. A node whose id names no ticket file has `missing: true` with an empty `title` and `status`. An unknown ticket id returns 404.

A `children` entry carries more than the other relation arrays, so a sub-ticket row renders without another request: `stage`, `stage_index` and `stage_count` for the child's position in its own pipeline, and `started_at` and `completed_at` for its wall time. `stage_index` is 1-based and absent when the child's stage is not in its pipeline. The timestamps bound the child's whole run, first pickup to last exit, rather than the current stage. `completed_at` is absent while the child is `in_progress`.

Both ticket payloads carry `project`, the configured project whose path is the ticket's, and the derived readiness: `ready` when no dependency holds the ticket back, and `blockers` naming the dependency ids that do when one does. Neither is stored; see [dependency-aware scheduling](tickets.md#dependency-aware-scheduling). `POST /api/tickets/{id}/run` answers with the ticket, so a caller can see from `blockers` whether the ticket it just moved to `todo` will actually be picked up.

The four relation endpoints take the same body, `{"related": [...]}`, and answer with the changed ticket. They return 400 for an empty list, an empty id, or more than one id on the two dependency verbs; 404 for an id no ticket answers; and 409 when the ticket is related to itself or when a dependency would close a cycle, with the cycle named in the error. A rejected call writes no file. Repeating a call that has nothing left to do returns 200 and writes nothing.

A link is written to both tickets, one file at a time, because two markdown files cannot be written together. When the second write fails the error names both tickets and which one was already changed, and repeating the request repairs the missing side.

## Description and full-body updates

Send `{"description":"# Title\n\nNew description.\n\n"}` to replace the prefix before the first recognized `## Notes` line. Recognition ignores surrounding whitespace. The server preserves everything from that heading onward, including later sections and unknown note formatting. Description and note writes share a transaction, so a stale description draft cannot remove acknowledged notes.

- Omitted or null `description` makes no description-only update; an empty string clears the prefix.
- A description containing a recognized `## Notes` line returns 400.
- A request with non-null `body` and non-null `description` returns 400.
- If a nonempty replacement lacks a final newline before an existing tail, the server inserts one.

`body` remains an explicit replacement of the whole document, including notes. Existing CLI full-body updates keep that behavior. Stage prompts still receive the entire body.

Detail responses include `description` and `description_tail`, even when empty. Board lists and ticket events omit both fields. `description_tail` runs from the `## Notes` heading through the end of the body, including later sections. Remote `kontora view --notes TICKET_ID` prints that field without a pager. Remote `kontora view --last 5 TICKET_ID` prints from the start of the fifth-last note in `body` and retains later sections. Legacy notes without bylines also count. See [view flags](cli.md#kontora-view-ticket_id) for limits and conflicts.

Browser editors change only `description` and show the protected tail read-only. Saves for one ticket remain ordered across closing and reopening its editor. Failed description drafts remain recoverable in the current page, but do not survive a page reload. Independent browser sessions still use last-write-wins for description edits.

## Archive

`POST /api/tickets/{id}/archive` sets `status: archived` and writes four frontmatter fields: `archived_from`, the closed status the ticket held; `archived_at`; `archived_by`, which is `web` here and `sweep` for [`kontora archive`](cli.md); and `archive_note` when the body carried one. It answers 409 unless the ticket is `done`, `cancelled` or the legacy `closed`, and writes no file when it refuses. An archived ticket keeps its file, its branch and its logs, and leaves `GET /api/tickets` — `?all=true` still lists it, and `GET /api/tickets/{id}` still answers it, with the four fields on the payload.

`POST /api/tickets/{id}/restore` writes `archived_from` back into `status` and removes all four fields. A ticket archived before those fields existed, or one whose `archived_from` names a status that is no longer a board column, restores to `done` rather than staying stranded in `archived`. It answers 409 for a ticket that is not archived.

`GET /api/tickets/archived` answers `{"tickets": [...]}`. Each row carries `id`, `title`, `project`, `pipeline`, `agent`, `branch`, `path`, `archived_at`, `archived_by`, a `status` holding the ticket's `archived_from`, and `wall_seconds`: the interval from the first history entry's `started_at` to the last `completed_at`, so it spans every run rather than the newest one. A ticket with no `archived_at` reports its file mtime instead, which is the date the sweep's own cutoff uses. The row's `agent` uses the same resolution as ticket views. An explicit override wins, followed by a matching pipeline stage's agent. If built-in `rework` is absent from an existing pipeline, resolution uses the latest nonempty history agent, then the configured default. Managed tickets without a pipeline use the configured default, except passive epics. Unknown pipelines and other unmatched stages have no default fallback. The path is more specific than `GET /api/tickets/{id}`, so a ticket whose id is literally `archived` is unreachable by that one path; every other route still reaches it.

A ticket whose `branch` is empty carries `auto_branch` in `GET /api/tickets` and `GET /api/tickets/{id}`: the branch the daemon would assign at pickup, resolved for the path the ticket names and the current [branch naming](configuration.md#branch-naming) mode. It is a read-only projection, not a stored field, and it is absent once `branch` is set.

Assistant model-turn endpoints answer 501 when no `assistant.agent` is configured.
The companion snapshot and inbox updates do not require one.
Requests for an unknown chat return 404.
Posting another message to a chat whose turn is still running returns 409.
A chat keeps its original agent. Repointing or removing that agent makes later
messages return 409; an agent session cannot resume on another CLI. Start a new
chat instead. The daemon's global turn cap returns 503, independently of any
single chat.

`POST /api/assistant/gate/{gid}` answers 404 after the change is answered or its
turn ends, so a stale card cannot resolve twice. `POST /api/assistant/gate/ask`
answers 403 when the per-turn nonce does not match the chat's current turn.
Only the agent-side `/gate/ask` route checks that nonce. The approval route
uses shared web authentication and Host/origin checks, not the nonce.
Local processes are trusted, including with a tokenless loopback daemon.
Tool approval is not an OS sandbox.

## Notes

A ticket's notes live in the `## Notes` section of its markdown body, so a stage prompt that interpolates the body carries them. See [Notes](tickets.md#notes) for the on-disk format.

| Endpoint | Description |
|----------|-------------|
| `POST /api/tickets/{id}/note` | Append a note (`{"text": "...", "author": "...", "parent": "<note-id>"}`; `author` and `parent` optional). |
| `PATCH /api/tickets/{id}/notes/{noteID}` | Replace one note's text (`{"text": "..."}`). The note's byline gains the `edited` flag. |
| `DELETE /api/tickets/{id}/notes/{noteID}` | Delete one note and its replies. |
| `POST /api/tickets/{id}/notes/{noteID}/reactions` | Add one actor's reaction (`{"emoji": "👍", "actor": "..."}`; `actor` optional). |
| `DELETE /api/tickets/{id}/notes/{noteID}/reactions/{emoji}` | Drop it again. The emoji is percent-encoded in the path (`👍` is `%F0%9F%91%8D`); an optional `?actor=` names whose reaction to drop. |

All five answer 200 with the whole ticket, so a caller replaces its copy rather than reconciling, and all five broadcast a `ticket_updated` event.

Appending or replacing note text trims surrounding whitespace and normalizes headings outside fenced code. Headings shift only when the shallowest heading is h1 or h2, making it h3. Relative levels stay the same, capped at h6. Notes whose shallowest heading is h3 or deeper need no shift. Backtick and tilde fenced code stays unchanged; the server appends a closing fence if needed. Outside fences, a line beginning with `# ` or `## ` at column zero ends the Notes section. Inside fences, those lines do not end Notes, and bold-only lines do not open notes.

Outside fences, a bold-only line opens a note under either rule:

- Its first `·`-separated field starts with `YYYY-MM-DD`.
- It matches the full `at · author · id` grammar, followed only by optional `re:<id>` and `edited` flags. Both `at` and author may be empty, as in `** ·  · cd34**`.

Other bold-only lines remain note text. On append or replacement, byline-like lines in note text use outer `__` instead of `**` when stored. Both render as bold.

Existing tickets are not migrated. Old sections after Notes remain in `description_tail`, outside the parsed `notes` array. Full-body writes through `body`, creation, upload or transfer do not normalize note text.

Every note the daemon writes carries a 4-character `id`, unique within the ticket, which is what `{noteID}` names. A note written before the format carried one is addressed as `#<index>` — its position in `notes` — and acting on it mints a real id in the same write. An id no note carries answers 404.

An empty `author` signs as the daemon's configured [`author`](configuration.md#top-level-fields), and so does an empty reaction `actor`. An author containing `·`, a newline or `**` is rejected with 400: the separator is what holds the byline together.

A `parent` makes the note a reply. One level only: a `parent` naming a note that is itself a reply answers 409 and writes nothing. Deleting a note deletes its replies with it.

Notes ride `GET /api/tickets/{id}` and the SSE `ticket_updated` event as `notes`, and are absent from `GET /api/tickets`, which carries no body. Each entry is `{id, at, author, author_kind, parent_id, edited, text, reactions}`. `at` is the byline's first field when the full grammar matches; it may be empty. Kontora writes RFC3339 timestamps. For a date-led legacy byline that does not match the full grammar, `at` holds the entire inner text, without an author or stored id. `author_kind` is derived from the config, not stored — `system` for `kontora`, `agent` for an author a configured agent is named after, `human` for anything else, and absent for a note with no author.

Reactions are stored in `<tickets_dir>/<ticket-id>.notes.json`, never in the ticket body, so the body stays readable to the agent that receives it in a stage prompt. The sidecar is invisible to ticket listing and to the file watcher, a missing one reads as no reactions, and `DELETE /api/tickets/{id}` removes it with the ticket file. A ticket copied without its sidecar keeps its conversation and loses its reactions.

Deleting a note needs no `X-Kontora-Confirm` header. That guard exists for the ticket file, whose deletion is unrecoverable across the whole store.

## Base branch validation

`base_branch` names the branch a ticket's worktree is cut from. See [Base branch](tickets.md#base-branch) for what the field means.

Both `POST /api/tickets` and `PUT /api/tickets/{id}` check the name's format and return 400 with `invalid base branch name` when it is malformed. Only creation checks that the branch exists, and it fails before writing the ticket file. A create request with `"status":"open"` skips that check, because an open ticket is not ready to run. An update accepts a name that resolves to nothing, because the repository at the ticket's `path` can change afterwards; the failure surfaces when the daemon builds the worktree and pauses the ticket.

The format check accepts any valid git branch name, which includes tag-shaped names such as `v1.0`. Restricting the base to real branches happens during resolution, not during format validation.
