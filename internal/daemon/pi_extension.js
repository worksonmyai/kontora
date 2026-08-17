// This source stays import-free so Node can run its state-machine tests without
// pi's loader aliases. Go replaces the two placeholders before writing it.

export default function (pi) {
  const THRESHOLD = __CHECKPOINT_THRESHOLD__;
  const ENABLED = __CHECKPOINT_ENABLED__;

  let shutdownRequested = false;
  let checkpointPending = false;
  let compactionInFlight = false;
  let continuationPending = false;
  let generation = 0;
  let consumedCheckpoints = {};
  let pendingCheckpoint = null;

  function tryShutdown(ctx) {
    if (!shutdownRequested) return;
    if (checkpointPending || compactionInFlight || continuationPending) return;
    ctx.shutdown();
  }

  pi.on("agent_settled", function (_event, ctx) {
    shutdownRequested = true;
    tryShutdown(ctx);
  });

  pi.on("agent_start", function (_event, _ctx) {
    continuationPending = false;
  });

  pi.on("session_start", function (_event, ctx) {
    // Rebuild consumed checkpoints from the branch so we reject duplicates
    // after a daemon restart that re-delivers the same session.
    var branch = ctx.sessionManager.getBranch();
    for (var i = 0; i < branch.length; i++) {
      var entry = branch[i];
      if (
        entry.type === "custom" &&
        entry.customType === "kontora-phase-checkpoint"
      ) {
        var d = entry.data;
        if (d && d.completed_phase) {
          consumedCheckpoints[d.completed_phase] = true;
        }
      }
    }
  });

  pi.on("session_shutdown", function (_event, _ctx) {
    // A callback from the previous generation must not enqueue a message.
    generation++;
  });

  if (ENABLED && THRESHOLD > 0) {
    pi.registerTool({
      name: "kontora_phase_complete",
      label: "Phase Complete",
      description:
        "Signal that a top-level ticket phase is complete and the next phase " +
        "should begin. Call this only at boundaries between top-level ticket " +
        "phases, after all phase-specific tests pass and a phase note has been " +
        "written. Do not call after the final phase.",
      parameters: {
        type: "object",
        properties: {
          completed_phase: {
            type: "string",
            description:
              "The phase that just completed, e.g. 'Phase 2: Expose the estimator as a local command'",
          },
          next_phase: {
            type: "string",
            description:
              "The phase to begin next, e.g. 'Phase 3: Add the agent option and the embedded extension'",
          },
        },
        required: ["completed_phase", "next_phase"],
      },
      execute: function (_toolCallId, params, _signal, _onUpdate, ctx) {
        var completed = params && params.completed_phase;
        var next = params && params.next_phase;

        if (
          typeof completed !== "string" ||
          completed.trim() === "" ||
          typeof next !== "string" ||
          next.trim() === ""
        ) {
          return {
            content: [
              {
                type: "text",
                text: "Both completed_phase and next_phase must be non-empty strings.",
              },
            ],
            isError: true,
          };
        }

        if (consumedCheckpoints[completed]) {
          return {
            content: [
              {
                type: "text",
                text:
                  "Phase '" +
                  completed +
                  "' was already checkpointed. Continue with " +
                  next +
                  ".",
              },
            ],
          };
        }

        if (checkpointPending) {
          return {
            content: [
              {
                type: "text",
                text:
                  "A checkpoint is already pending. Continue with " +
                  next +
                  " without calling this tool again.",
              },
            ],
          };
        }

        if (compactionInFlight) {
          return {
            content: [
              {
                type: "text",
                text:
                  "A compaction is already in flight. Continue with " +
                  next +
                  " without calling this tool again.",
              },
            ],
          };
        }

        var usage = ctx.getContextUsage();
        var tokens =
          usage && typeof usage.tokens === "number" ? usage.tokens : null;

        var outcome;
        if (tokens === null) {
          outcome = "skipped";
        } else if (tokens <= THRESHOLD) {
          outcome = "skipped";
        } else {
          outcome = "requested";
        }

        pi.appendEntry("kontora-phase-checkpoint", {
          completed_phase: completed,
          next_phase: next,
          context_tokens: tokens,
          threshold: THRESHOLD,
          outcome: outcome,
        });

        consumedCheckpoints[completed] = true;

        if (outcome === "requested") {
          checkpointPending = true;
          pendingCheckpoint = {
            completed_phase: completed,
            next_phase: next,
            context_tokens: tokens,
          };
          return {
            content: [
              {
                type: "text",
                text:
                  "Checkpoint recorded for '" +
                  completed +
                  "'. Context (" +
                  tokens +
                  " tokens) exceeds threshold (" +
                  THRESHOLD +
                  "). Compaction will start after this turn. Continue with " +
                  next +
                  ".",
              },
            ],
          };
        }

        return {
          content: [
            {
              type: "text",
              text:
                "Checkpoint recorded for '" +
                completed +
                "'. Context " +
                (tokens === null
                  ? "is unknown"
                  : "(" + tokens + " tokens) is within threshold (" + THRESHOLD + ")") +
                ", no compaction needed. Continue with " +
                next +
                ".",
            },
          ],
        };
      },
    });

    pi.on("turn_end", function (_event, ctx) {
      if (!checkpointPending || !pendingCheckpoint) return;

      var cp = pendingCheckpoint;
      checkpointPending = false;
      pendingCheckpoint = null;

      if (compactionInFlight) return;

      var capturedGen = generation;
      var callbackHandled = false;
      compactionInFlight = true;

      var continuationText =
        "Continue with " +
        cp.next_phase +
        ". Re-read the ticket note, then inspect git status and the " +
        "current diff before editing. Do not redo completed phases and " +
        "do not rely on the compaction summary as the only source of truth.";

      ctx.compact({
        customInstructions:
          "Preserve the ticket goal, requirements, cross-phase invariants, " +
          "modified files, test results, decisions, unresolved failures, " +
          "and the named next phase: " +
          cp.next_phase +
          ".",
        onComplete: function (result) {
          if (callbackHandled) return;
          callbackHandled = true;
          compactionInFlight = false;

          if (capturedGen !== generation) return;

          pi.appendEntry("kontora-phase-checkpoint", {
            completed_phase: cp.completed_phase,
            next_phase: cp.next_phase,
            context_tokens: cp.context_tokens,
            threshold: THRESHOLD,
            outcome: "compacted",
            compaction_usage: result && result.usage,
            estimated_post_compaction_tokens:
              result && result.estimatedTokensAfter,
          });

          if (!continuationPending) {
            continuationPending = true;
            pi.sendMessage(
              {
                customType: "kontora-continuation",
                content: continuationText,
                display: false,
              },
              { triggerTurn: true, deliverAs: "followUp" }
            );
          }
        },
        onError: function (error) {
          if (callbackHandled) return;
          callbackHandled = true;
          compactionInFlight = false;

          if (capturedGen !== generation) return;

          var msg = error && error.message ? error.message : "";
          var outcome = msg.toLowerCase().indexOf("already compacted") !== -1
            ? "compacted"
            : "failed";

          pi.appendEntry("kontora-phase-checkpoint", {
            completed_phase: cp.completed_phase,
            next_phase: cp.next_phase,
            context_tokens: cp.context_tokens,
            threshold: THRESHOLD,
            outcome: outcome,
            error: msg,
          });

          if (!continuationPending) {
            continuationPending = true;
            pi.sendMessage(
              {
                customType: "kontora-continuation",
                content: continuationText,
                display: false,
              },
              { triggerTurn: true, deliverAs: "followUp" }
            );
          }
        },
      });
    });
  }
}
