# Recurring automations

Automations create ordinary tickets on a calendar schedule. Each occurrence uses the selected repository, agent, and instructions.
Definitions and occurrence history survive page reloads and daemon restarts.

## Creating and pausing an automation

Open Automations in the web dashboard. Choose a repository, a configured agent, instructions, and an explicit timezone.
Save the definition, inspect its preview, then enable it. New definitions start disabled.

Each admitted occurrence creates a standalone ticket. Automations do not inherit project or global pipelines.
Enabled automations are explicit run requests, even with `auto_pick_up: false`.
Tickets still wait for ordinary worker capacity, dependencies, ownership checks, and execution recovery.
No separate worker pool runs automation tickets.

Disabling an automation stops future ticket creation. It does not cancel existing tickets or remove queued work.
Use ordinary ticket controls to pause or cancel those tickets.
There are no delete-definition or run-now actions.

Automations use ordinary worktrees, hooks, and agent permissions.
Instructions such as “do not edit files” do not enforce read-only access.
Do not enable unattended work unless the agent's configured permissions match the task.

## Calendar rules

Schedules support daily, weekdays, and weekly frequencies at a local `HH:MM` time.
Weekly schedules select weekdays with integers from `0` (Sunday) through `6` (Saturday).
Daily and weekdays schedules leave `days` empty.

Save an explicit IANA timezone, such as `Europe/Berlin`, `America/New_York`, or `UTC`.
The daemon does not use the browser's timezone or accept `Local`.
The server calculates the next three preview dates in the saved timezone.

- During a forward clock change, nonexistent local times are skipped.
- During a backward clock change, a repeated local time runs at its earliest UTC match.
- Creating, enabling, or rescheduling an automation selects the next future slot.
- Editing instructions or other non-schedule fields preserves the existing schedule cursor.
- Moving the system clock backward does not make a recorded slot eligible again.

The daemon admits only the latest due slot, and only within five minutes of its scheduled time.
Older missed slots are recorded as skipped. Long gaps use one history record with a slot count and the last missed date.
An outage does not create a backlog of tickets.
The schedule controls ticket creation, not the exact time an agent starts.

One-time [`scheduled_at`](tickets.md#scheduled-pickup) scheduling behaves differently.
It promotes an existing open ticket to `todo` and follows ordinary automatic pickup settings.
An automation creates a new ticket per admitted occurrence and requests execution explicitly.

## Overlap

New definitions default to skipping overlap. The overlap check applies to earlier work from the same automation.
Any unfinished or execution-reserved ticket blocks another occurrence, including queued, paused, and `human_review` tickets.
Uncertain completion evidence also blocks admission.

Clear the overlap option to permit concurrent tickets under ordinary daemon limits.
Changing the option does not cancel earlier tickets or release their execution reservations.

## History and recovery

Occurrence history records scheduling decisions separately from ticket execution status.
An admitted occurrence means the ticket was published; it does not mean the agent completed successfully.
Ticket links refer to the primary ticket store, including archived tickets.
A deleted ticket remains a missing historical reference. Its stored receipt prevents recreation.

Before publication, the daemon saves an occurrence identity, a reserved ticket ID, and the frozen ticket inputs.
The daemon then records that publication started before writing the ticket.
Recovery uses these records:

| Saved state | Recovery |
|-------------|----------|
| Reserved, publication not started | Continue only if the occurrence is still eligible; otherwise skip it. |
| Publication started, matching ticket exists | Adopt the matching ticket without replacing or reopening it. |
| Publication started, ticket missing | Block automatic retry because publication followed by deletion cannot be ruled out. |
| Ticket exists with different provenance | Block and report the conflict. |
| Ticket receipt already saved | Never recreate the occurrence, even after ticket deletion. |

If an occurrence is blocked, inspect its reason and the referenced ticket before acknowledging it.
The revision-checked Skip action records the unresolved occurrence as skipped.
Skip does not create a replacement ticket, stop an agent, or release execution reservations.
An existing matching ticket still blocks overlap until its work finishes.
Future scheduling resumes from a future slot when no other block remains.
If the occurrence changes before acknowledgement, reload history and inspect it again.

## Storage and backup

Automation state is machine-local:

```text
~/.kontora/automations/<hash-of-canonical-primary-ticket-directory>/state.json
```

Kontora expands and resolves the primary ticket directory before deriving its hash.
`KONTORA_AUTOMATIONS_ROOT` overrides the parent directory for isolated environments and tests.
Keep that root stable. Different roots create independent state even for the same ticket directory.
The state directory uses `0700` permissions; state and lock files use `0600`.
The snapshot includes definitions, create-request receipts, occurrence history, and the primary directory identity.
Instructions are stored in plaintext inside this private state.

One daemon holds the scope's lock for its lifetime.
If another daemon owns the lock, Automations reports unavailable without disabling ordinary ticket functionality.
Invalid or unsupported snapshots also report unavailable. Kontora does not silently replace them with empty state.
Do not remove the lock file or edit a live snapshot to bypass recovery.

Before backup, stop or finish active agents, then stop the daemon.
Copy the automation scope directory with its associated ticket store.
Keep the private permissions when restoring. Start only one owner of the restored scope.
Restoring ticket files alone loses scheduling decisions and request receipts.
Changing the resolved primary directory selects a different scope; moving a snapshot alone is not a supported migration.

All occurrence records are retained. HTTP history is paginated, but the snapshot grows with history.
There is no automatic pruning or database.
Local locks do not coordinate multiple machines or independently synced copies.
Ticket publication and automation snapshots are separate durable writes, so uncertain publication requires explicit recovery.
Automations do not guarantee exactly-once external agent effects.

See the [automation HTTP API](api.md#automations) for request formats, revisions, and transport protections.
