// This source stays import-free so Node can run its state-machine tests without
// pi's loader aliases. Go replaces the three placeholders before writing it.
// Node builtins are reached through process.getBuiltinModule for the same
// reason: it needs no import and works under the tests' Function wrapper.

export default function (pi) {
  const THRESHOLD = __CHECKPOINT_THRESHOLD__;
  const ENABLED = __CHECKPOINT_ENABLED__;
  // Where to publish the "blocked on a question" marker the daemon polls.
  // Empty for a run the daemon does not watch.
  const WAIT_MARKER = __WAIT_MARKER_PATH__;
  // The question only has to fit a board badge and one line of last_error.
  // Bytes, not characters, so Go and this file cut identically.
  const QUESTION_BYTES = 200;

  // Tool call id -> the marker entry for that question, for the calls still in
  // flight. A null prototype so a tool call id of "constructor" cannot read a
  // function where an entry is meant.
  let openQuestions = Object.create(null);
  let shutdownRequested = false;
  let checkpointPending = false;
  let compactionInFlight = false;
  let continuationPending = false;
  let generation = 0;
  let consumedCheckpoints = {};
  let pendingCheckpoint = null;

  // A call to one of these does not return until the human answers, and no pi
  // event says "a question is open", so the tool call is the only signal.
  function isQuestionTool(name) {
    return name === "ask_user_question" || name === "question";
  }

  function nodeModule(name) {
    try {
      return process.getBuiltinModule(name);
    } catch (e) {
      return null;
    }
  }

  // Cut to at most limit bytes without splitting a UTF-8 character: step back
  // off any continuation byte the cut landed on.
  function truncateBytes(s, limit) {
    var buf = Buffer.from(s, "utf8");
    if (buf.length <= limit) return s;
    var end = limit;
    while (end > 0 && (buf[end] & 0xc0) === 0x80) end--;
    return buf.toString("utf8", 0, end);
  }

  // ask_user_question takes {questions: [{question, ...}]}, pi's example
  // question tool takes {question, options}. An unknown shape leaves the badge
  // with the tool name alone.
  function questionText(args) {
    if (!args || typeof args !== "object") return "";
    var raw = "";
    if (Array.isArray(args.questions) && args.questions.length > 0) {
      var first = args.questions[0];
      if (first && typeof first.question === "string") raw = first.question;
      else if (typeof first === "string") raw = first;
    } else if (typeof args.question === "string") {
      raw = args.question;
    }
    return truncateBytes(raw.replace(/\s+/g, " ").trim(), QUESTION_BYTES);
  }

  // The daemon polls the marker path with no coordination, so the write goes to
  // a temp file in the same directory and is renamed in: a rename is atomic
  // within one directory, a partial write is not.
  function writeMarker(entry) {
    var fs = nodeModule("node:fs");
    if (!fs || !WAIT_MARKER) return;
    var tmp = WAIT_MARKER + ".tmp." + process.pid;
    try {
      fs.mkdirSync(WAIT_MARKER.replace(/[/\\][^/\\]*$/, ""), { recursive: true });
      fs.writeFileSync(tmp, JSON.stringify(entry));
      fs.renameSync(tmp, WAIT_MARKER);
    } catch (e) {
      try {
        fs.rmSync(tmp, { force: true });
      } catch (e2) {
        /* the temp file is the daemon's to ignore either way */
      }
    }
  }

  function removeMarker() {
    var fs = nodeModule("node:fs");
    if (!fs || !WAIT_MARKER) return;
    try {
      fs.rmSync(WAIT_MARKER, { force: true });
    } catch (e) {
      /* a marker left behind is cleared by the daemon when the run ends */
    }
  }

  // The oldest open question is the one that has blocked the longest, so it is
  // what the marker falls back to. ISO-8601 UTC strings sort chronologically.
  function oldestOpenQuestion() {
    var oldest = null;
    for (var id in openQuestions) {
      var e = openQuestions[id];
      if (!oldest || e.started_at < oldest.started_at) oldest = e;
    }
    return oldest;
  }

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

  if (WAIT_MARKER) {
    pi.on("tool_execution_start", function (event, _ctx) {
      if (!event || !isQuestionTool(event.toolName)) return;
      var entry = {
        tool: event.toolName,
        tool_call_id: event.toolCallId,
        started_at: new Date().toISOString(),
        question: questionText(event.args),
      };
      openQuestions[event.toolCallId] = entry;
      // Rewritten on every question that opens, so the marker describes the
      // most recent one rather than one the human may already have answered.
      writeMarker(entry);
    });

    pi.on("tool_execution_end", function (event, _ctx) {
      if (!event || !isQuestionTool(event.toolName)) return;
      delete openQuestions[event.toolCallId];
      // Questions can be answered out of order, so the marker falls back to the
      // one still blocking rather than keeping the answered call's text.
      var next = oldestOpenQuestion();
      if (next) writeMarker(next);
      else removeMarker();
    });
  }

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
    // A clean /exit while a question is open must not leave the marker behind.
    openQuestions = Object.create(null);
    removeMarker();
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
