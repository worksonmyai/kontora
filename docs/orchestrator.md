# Standing orchestrator components

Managed execution is disabled. The daemon refuses startup on a board with registered orchestrator authority before ordinary recovery can change owned tickets. Ordinary boards retain their existing scheduling and epic behavior.

The Git, verification, acceptance, and operator-control packages provide tested components. They do not provide an enabled objective workflow. Do not create an authority registry on a live board to try them.

## Checked delivery

A managed workspace starts at a full commit identifier and requires a persistent branch and workspace reservation. Its branch, path, repository identity, and owner must match on retries. Existing dirty work, conflicting reservations, and unexpected reference changes require reconciliation. Kontora does not reset, stash, or rebase that work.

Committed child results have immutable references under `refs/kontora/results/`. Cleanup retains the result before removing a clean workspace. It refuses tracked files marked `assume-unchanged` or `skip-worktree`, because Git status can hide local edits. Branch reservations survive cleanup.

Delivery uses an unchecked-out branch. A fast-forward retains the existing child commits. Divergent histories produce an uncommitted merge in a separate reserved workspace. Conflicts and staged merge changes remain available for repair. These helpers never create a commit. A commit still requires approval for its exact tree, parents, and message; the production commit executor is not installed.

The daemon delivery component journals an intent containing the old delivery commit, checked candidate, result generation, and ownership before updating the reference. Git compares the old commit when applying the update. Recovery handles three observations:

| Delivery reference | Recovery |
| --- | --- |
| Old commit | Keep the intent; do not repeat work during inspection |
| Recorded candidate | Record the receipt without repeating the reference update |
| Another commit or a missing reference | Park the intent and retain work for reconciliation |

Git hooks, merge drivers, and filters can execute programs. Git helpers do not establish OS isolation or approve those programs. Preparation refuses hooks that could run before commit approval; it does not disable them. The caller must authorize the effective Git configuration before admitting managed work.

## Verification and acceptance

Checked results identify their repository-qualified commits or immutable artifacts, consumed generations, and pinned review ranges. Legacy process-success results cannot support checked acceptance.

A verification policy defines the command, working directory, timeout, and execution-configuration revision. Check evidence must match a successfully stopped, admitted check attempt and its retained log. Changing an input, definition, or output makes the old evidence unsuitable for acceptance.

A no-change result requires an explicit reason and evidence. It does not require an empty commit. Research results require hashed artifacts, retained sources, and review evidence.

Acceptance requires a review of the combined result. The acceptance binding includes the criteria revision, plan revision, verification policy, required generations, and current qualified outputs. Stale requests are rejected. Review findings require producer and verification attempts admitted after the finding. Replaced repair work needs an explicit successor recorded in the replacement plan. Operator feedback remains an acceptance requirement until checked work resolves it. Differing commit heads in one repository need a checked output that consumes their results together. Cancellation preserves results and reservations; returning work to ordinary scheduling requires a separate release implementation.

These records validate evidence supplied by their caller. The production verification runner, review handoff, and acceptance routes remain unconnected.

## Optional operator API

The web server can receive an `OrchestratorService` and a separate authority configuration through explicit server options. The production daemon does not install that service. Existing ticket-service implementations do not need new methods.

| Route | Purpose |
| --- | --- |
| `GET /api/objectives` | Authoritative board revision and objective list |
| `GET /api/objectives/{id}` | Objective, plans, work, attempts, results, verification policies, reviews, questions, and decisions |
| `POST /api/objectives` | Create a draft assignment |
| `POST /api/objectives/{id}/controls` | Submit a revision-checked operator request |

An uninstalled service answers HTTP 503. The disabled daemon service can record drafts, pause, cancellation, feedback, answers, and exact action approvals. It refuses resume, release, and acceptance. It starts no workers or planner turns.

Requests include `board_id`, `request_id`, `expected_revision`, `operation`, and `objective_id`. The transport supplies the operator identity. The request body cannot declare it. Feedback creates durable work rather than a transient message.

The request receipt and control changes share one journal transaction. Repeating the same request returns its recorded response, including after restart. Reusing the identity with changed content, operator, board, or expected revision returns a conflict. Revision conflicts are not automatically retried. GET snapshots remain authoritative after reconnect; an event notification is not a replacement for a snapshot.

The remote client supports these contracts without a ticket-file fallback. Flat objective commands and the browser objective interface are not installed.

## Authority and isolation

Opt-in authority mode requires a separate `X-Kontora-Operator-Token` credential. The shared web token does not grant operator authority. Request bodies, query parameters, cookies, and forwarded identity headers cannot establish that authority.

Attempt capabilities expire and identify the board, work item, and exact attempt. They permit only their own inspection routes and, when approved, proposal submission. They cannot approve actions, accept objectives, change configuration, use the assistant controls, or open terminals. The service must recheck attempt validity with any mutation.

The environment sanitizer filters both inherited values and overrides. Launchers must use its output as a complete replacement environment. They must also list custom credential aliases. Appending the sanitized values to the inherited environment restores the secrets.

Transport authorization controls supported API operations. A same-user shell process can still change files, invoke Git, or access credentials through other OS resources. Independently enforced OS isolation is required for untrusted workers.

## Remaining rollout work

Before execution can be enabled, complete the production managed owner, watcher reconciliation, exact recovery inspectors, stage and job handoffs, planner loop, and effect executor. Connect verification, repair dispatch, hook and profile authorization, commit approval, launcher sanitation, terminal credentials, accounting, and operator acceptance together.

Repository-writing policy, history policy, and execution limits require explicit operator decisions. The component tests do not approve proposed defaults. The full CLI, browser controls, metrics, and isolated end-to-end workflow also remain required.
