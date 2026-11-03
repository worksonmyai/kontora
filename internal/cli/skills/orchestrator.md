# Standing orchestrator

## Availability

Managed execution is disabled. A board with registered orchestrator authority refuses daemon startup before ordinary ticket recovery. Do not create an authority registry on a live board to test the components.

Ordinary tickets and passive epics retain their existing behavior. A completed epic does not accept an objective.

The optional objective service, checked Git delivery, verification records, and acceptance records are developer components. The production daemon does not install the service or start a standing planner. Flat objective commands and the browser objective interface are not installed.

## Operator controls

The optional API requires transport-established operator identity, separate from the shared web token. A worker cannot declare itself an operator in JSON. Attempt capabilities cannot approve actions, accept objectives, change configuration, or open terminals.

Each mutation supplies the board identity, expected revision, and request identity. Exact retries return the recorded outcome. Changed content with a reused identity returns a conflict. Refresh the authoritative snapshot after reconnect or a stale request; do not retry a mutation against a guessed revision.

The disabled service can record draft assignments, pause, cancellation, feedback, answers, and bound action approvals. Resume, acceptance, and release remain refused. An uninstalled service answers HTTP 503. There is no ticket-file fallback.

## Retained work

Managed workspaces use persistent branch and path reservations. Keep dirty work, unfinished merges, result references, and cancellation reservations until an explicit disposition authorizes cleanup or release. Never reset, stash, rebase, or delete that work to make recovery pass.

Successful process exit is not a checked result. Verification must identify exact outputs, consumed result generations, definitions, execution configuration, and retained logs. Research needs retained sources and review evidence. No-change work needs an explicit reason and evidence, not an empty commit.

Human acceptance must match the current criteria, plan, checks, required generations, qualified outputs, and combined review. A changed input requires fresh evidence. Feedback remains tracked work until acceptance.

## Security limits

Workflow authorization is not OS isolation. A same-user shell worker can still access files and invoke Git or network programs outside supported API operations. Use independently enforced isolation for untrusted workers.

Worker environments must replace inheritance after removing shared and operator credentials, including custom aliases. Do not weaken a sandbox wrapper or drop required hooks to admit work. No proposed authority, history, or resource defaults are approved by the component implementation.
