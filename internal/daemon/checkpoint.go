package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/tmux"
)

// maxCheckpointsPerRun bounds how many phase boundaries one run is driven
// through. An agent that keeps signalling boundaries would otherwise keep the
// stage alive forever; past the cap the run is ended.
const maxCheckpointsPerRun = 20

const (
	// checkpointPoll is how often a wake with no new checkpoint re-reads the
	// sidecar and the transcript.
	checkpointPoll = 500 * time.Millisecond
	// checkpointQuiet is how long the transcript must stop growing, with its last
	// entry a finished reply, before such a wake is taken as the agent really
	// being done.
	checkpointQuiet = 5 * time.Second
	// checkpointStaleWindow is how soon after a continuation a wake is suspect.
	// A latched signal arrives at once; a phase takes minutes, so a wake later
	// than this is the agent going idle for real.
	checkpointStaleWindow = 20 * time.Second
	// checkpointStuckQuiet is the silence needed to end the run when the
	// transcript says the agent is mid-turn, or says nothing at all. A turn in
	// flight writes an entry per thinking block and per tool call, and the
	// longest gap seen in real sessions is under two minutes, so a transcript
	// this quiet belongs to an agent that is not coming back.
	checkpointStuckQuiet = 5 * time.Minute
)

// checkpointsPath is the phase-checkpoint sidecar for one run of a stage. It
// sits beside <stage>.log, like the events sidecar.
func checkpointsPath(cfg *config.Config, ticketID, stageName string, run int) string {
	name := fmt.Sprintf("%s.%d.checkpoints.jsonl", stageName, run)
	return filepath.Join(expandTilde(cfg.LogsDir), ticketID, name)
}

// checkpointSetup is what a run needs to drive phase-boundary compaction. The
// zero value marks a run that does not: every run whose agent is not Claude,
// carries no positive threshold, or is an annotation rather than stage work.
type checkpointSetup struct {
	sidecar   string
	threshold int
	log       *slog.Logger
	// compactChannel is the tmux wait-for channel this run's PostCompact hook
	// signals, scoped to the run so an older signal cannot be read as this
	// run's compaction landing.
	compactChannel string
}

func (c checkpointSetup) enabled() bool { return c.threshold > 0 && c.sidecar != "" }

// checkpointController decides what a Claude run does each time its agent goes
// idle. The agent signals a phase boundary by appending a phase_complete record
// to the sidecar; the controller measures the context, asks the runner to
// compact when it is over the threshold, and hands back the prompt that starts
// the next phase.
//
// It never fails the stage. A transcript it cannot read, a compaction that does
// not land and a sidecar it cannot write are each recorded and stepped over.
//
// The runner calls onIdle from one goroutine at a time, so the state below
// needs no lock.
type checkpointController struct {
	log       *slog.Logger
	sidecar   string
	threshold int
	env       map[string]string
	sessionID string

	poll        time.Duration
	quiet       time.Duration
	stuckQuiet  time.Duration
	staleWindow time.Duration

	// consumed is how many phase_complete records have been acted on. It starts
	// at the number of accepted markers already in the sidecar so a run resumed
	// after a daemon restart does not replay the boundaries the previous one
	// handled.
	consumed int
	// delivered counts the continuations typed, which is what the per-run cap
	// bounds.
	delivered int
	// deliveredAt is when the last continuation decision was made. It is zero
	// until one has been: only after a continuation can a wake be a signal left
	// latched by the turn that just ended.
	deliveredAt time.Time
	pending     *pendingCompaction
}

// pendingCompaction is a compaction the runner was asked for and whose outcome
// is not known yet: the runner reports it on the next idle.
type pendingCompaction struct {
	record        cli.CheckpointRecord
	contextTokens int
	// manualBefore is how many manual compact boundaries the transcript held
	// when /compact was sent, so a new one identifies this compaction.
	manualBefore int
}

func newCheckpointController(setup checkpointSetup, env map[string]string, sessionID string) *checkpointController {
	c := &checkpointController{
		log:         setup.log,
		sidecar:     setup.sidecar,
		threshold:   setup.threshold,
		env:         env,
		sessionID:   sessionID,
		poll:        checkpointPoll,
		quiet:       checkpointQuiet,
		stuckQuiet:  checkpointStuckQuiet,
		staleWindow: checkpointStaleWindow,
	}
	c.consumed = countAccepted(c.sidecar)
	return c
}

// onIdle is the decision the runner asks for on every idle signal. It returns
// as soon as ctx is cancelled: the runner is inside this call, so a wait that
// ignored ctx would hold up a shutdown or a stage timeout.
func (c *checkpointController) onIdle(ctx context.Context, ev tmux.IdleEvent) tmux.IdleDecision {
	compacted := c.settlePending(ev.CompactErr)

	rec, ok := c.nextCheckpoint()
	if !ok && c.suspectStaleWake(compacted) {
		rec, ok = c.awaitCheckpoint(ctx)
	}
	if !ok {
		return tmux.IdleDecision{Action: tmux.IdleFinish}
	}

	if c.delivered >= maxCheckpointsPerRun {
		// The cap is there to stop a run that signals boundaries forever, so it
		// ends the run rather than driving one more phase. The outcome record
		// says the remaining phases were dropped.
		c.log.Warn("checkpoint cap reached, ending the run",
			"cap", maxCheckpointsPerRun, "completed_phase", rec.CompletedPhase)
		c.record(cli.CheckpointRecord{
			Kind:           cli.CheckpointKindOutcome,
			CompletedPhase: rec.CompletedPhase,
			NextPhase:      rec.NextPhase,
			Outcome:        cli.CheckpointOutcomeSkipped,
			Threshold:      c.threshold,
			Error:          fmt.Sprintf("checkpoint cap of %d reached for this run", maxCheckpointsPerRun),
		})
		return tmux.IdleDecision{Action: tmux.IdleFinish}
	}

	c.delivered++
	c.deliveredAt = time.Now()
	continuation := continuationPrompt(rec.NextPhase)

	scan, err := c.scan()
	tokens := scan.contextTokens
	switch {
	case err != nil || !scan.hasContext:
		// An unmeasurable context is not a reason to compact blind.
		if err != nil {
			c.log.Warn("checkpoint: reading the transcript failed", "err", err)
		}
		c.record(cli.CheckpointRecord{
			Kind:           cli.CheckpointKindOutcome,
			CompletedPhase: rec.CompletedPhase,
			NextPhase:      rec.NextPhase,
			Outcome:        cli.CheckpointOutcomeSkipped,
			Threshold:      c.threshold,
			Error:          "context tokens unknown",
		})
		return tmux.IdleDecision{Action: tmux.IdlePrompt, Continuation: continuation}

	case tokens <= c.threshold:
		c.log.Info("checkpoint: context within threshold",
			"context_tokens", tokens, "threshold", c.threshold, "next_phase", rec.NextPhase)
		c.record(cli.CheckpointRecord{
			Kind:           cli.CheckpointKindOutcome,
			CompletedPhase: rec.CompletedPhase,
			NextPhase:      rec.NextPhase,
			Outcome:        cli.CheckpointOutcomeSkipped,
			ContextTokens:  tokens,
			Threshold:      c.threshold,
		})
		return tmux.IdleDecision{Action: tmux.IdlePrompt, Continuation: continuation}
	}

	c.log.Info("checkpoint: compacting at a phase boundary",
		"context_tokens", tokens, "threshold", c.threshold, "next_phase", rec.NextPhase)
	c.pending = &pendingCompaction{record: rec, contextTokens: tokens, manualBefore: scan.manualCompacts}
	return tmux.IdleDecision{
		Action:              tmux.IdleCompact,
		CompactInstructions: compactInstructions(rec.NextPhase),
		Continuation:        continuation,
	}
}

// suspectStaleWake reports whether a wake that carries no new checkpoint could
// be a signal left latched by the turn that just ended rather than the agent
// going idle again. A wake long after the last continuation is the agent
// finishing for real; one right after it, or one right after a compaction the
// runner has just waited out, is not to be trusted.
func (c *checkpointController) suspectStaleWake(justCompacted bool) bool {
	if c.deliveredAt.IsZero() {
		return false
	}
	return justCompacted || time.Since(c.deliveredAt) < c.staleWindow
}

// settlePending records how the compaction asked for on the previous idle went,
// and reports whether there was one. The transcript is the judge: a compaction
// that landed leaves a manual compact boundary behind, and without one the
// outcome is a failure however quietly the runner returned. The run carries on
// uncompacted either way.
func (c *checkpointController) settlePending(compactErr error) bool {
	if c.pending == nil {
		return false
	}
	p := c.pending
	c.pending = nil

	scan, err := c.scan()
	landed := err == nil && scan.manualCompacts > p.manualBefore

	out := cli.CheckpointRecord{
		Kind:           cli.CheckpointKindOutcome,
		CompletedPhase: p.record.CompletedPhase,
		NextPhase:      p.record.NextPhase,
		ContextTokens:  p.contextTokens,
		Threshold:      c.threshold,
		Outcome:        cli.CheckpointOutcomeCompacted,
	}
	if landed {
		out.PreTokens = scan.lastPreTokens
	} else {
		out.Outcome = cli.CheckpointOutcomeFailed
		out.Error = compactionFailure(compactErr, err)
		c.log.Warn("checkpoint: compaction did not land, continuing uncompacted", "err", out.Error)
	}
	c.record(out)
	return true
}

// compactionFailure says why a compaction is recorded as failed: what the
// runner reported, what reading the transcript back reported, or that the
// transcript simply never gained the boundary a compaction leaves.
func compactionFailure(compactErr, scanErr error) string {
	switch {
	case compactErr != nil:
		return compactErr.Error()
	case scanErr != nil:
		return "could not read the transcript back: " + scanErr.Error()
	default:
		return "no compact boundary appeared in the transcript"
	}
}

// awaitCheckpoint handles a wake that carries no new checkpoint while a
// continuation is outstanding. Claude's Stop and idle_prompt hooks share one
// tmux channel, and tmux latches a signal sent while nobody waits: the next
// wait returns at once. Such a wake can therefore be the end of the previous
// turn arriving late rather than the agent going idle again, and deciding on it
// would send /exit into a working agent.
//
// Rather than reason about the latch, watch the agent. A checkpoint appearing
// answers the question outright. Silence does not: Claude writes the user entry
// when the continuation is typed and then nothing until the first thinking
// block or tool call completes, which in real sessions is a gap of up to a
// minute and a half. What separates the two is the last entry in the
// transcript, so the run only ends on a transcript that has gone quiet with the
// agent's reply as its last word. A transcript that says the turn is still
// running, or that cannot be read at all, has to go quiet for far longer.
func (c *checkpointController) awaitCheckpoint(ctx context.Context) (cli.CheckpointRecord, bool) {
	size := c.transcriptSize()
	quietSince := time.Now()
	turn := turnUnknown
	// readAt is the size the turn state was read at, so a transcript that is not
	// growing is parsed once rather than on every poll.
	readAt := int64(-1)

	for {
		select {
		case <-ctx.Done():
			return cli.CheckpointRecord{}, false
		case <-time.After(c.poll):
		}

		if rec, ok := c.nextCheckpoint(); ok {
			return rec, true
		}

		if now := c.transcriptSize(); now != size {
			size, quietSince = now, time.Now()
			continue
		}

		quiet := time.Since(quietSince)
		if quiet >= c.quiet && readAt != size {
			readAt = size
			if scan, err := c.scan(); err == nil {
				turn = scan.turn
			} else {
				turn = turnUnknown
			}
		}
		if turn == turnEnded && quiet >= c.quiet {
			return cli.CheckpointRecord{}, false
		}
		if turn != turnEnded && quiet >= c.stuckQuiet {
			c.log.Warn("checkpoint: the agent stopped writing mid-turn, ending the run",
				"quiet_for", c.stuckQuiet)
			return cli.CheckpointRecord{}, false
		}
	}
}

// nextCheckpoint returns the first phase_complete record the controller has not
// acted on yet, and marks it as taken. The marker goes in before the boundary
// is decided, so a daemon that dies while compacting leaves a sidecar the next
// controller reads the same way this one does.
func (c *checkpointController) nextCheckpoint() (cli.CheckpointRecord, bool) {
	recs, err := readCheckpointRecords(c.sidecar)
	if err != nil {
		c.log.Warn("checkpoint: reading the sidecar failed", "path", c.sidecar, "err", err)
		return cli.CheckpointRecord{}, false
	}
	seen := 0
	for _, r := range recs {
		if r.Kind != cli.CheckpointKindPhaseComplete {
			continue
		}
		if seen == c.consumed {
			c.consumed++
			c.record(cli.CheckpointRecord{
				Kind:           cli.CheckpointKindAccepted,
				CompletedPhase: r.CompletedPhase,
				NextPhase:      r.NextPhase,
			})
			return r, true
		}
		seen++
	}
	return cli.CheckpointRecord{}, false
}

func (c *checkpointController) record(rec cli.CheckpointRecord) {
	rec.Time = time.Now()
	if err := cli.AppendCheckpointRecord(c.sidecar, rec); err != nil {
		c.log.Warn("checkpoint: writing the sidecar failed", "path", c.sidecar, "err", err)
	}
}

// transcriptPath is the session JSONL Claude is appending to, or "" before it
// has written one.
func (c *checkpointController) transcriptPath() string {
	if c.sessionID == "" {
		return ""
	}
	matches, _, err := claudeSessionFiles(c.env, c.sessionID)
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func (c *checkpointController) scan() (transcriptScan, error) {
	path := c.transcriptPath()
	if path == "" {
		return transcriptScan{}, fmt.Errorf("no session transcript for %q", c.sessionID)
	}
	return scanTranscript(path)
}

// transcriptSize is what the controller watches to tell a working agent from a
// finished one. The transcript is append-only, so its size answers the question
// without re-parsing a file that can be megabytes on every poll.
func (c *checkpointController) transcriptSize() int64 {
	path := c.transcriptPath()
	if path == "" {
		return 0
	}
	return fileSize(path)
}

// continuationPrompt is what the agent is told after a boundary. It matches the
// pi extension's wording, so both agents resume a phase the same way.
func continuationPrompt(nextPhase string) string {
	return "Continue with " + nextPhase + ". Re-read the ticket note, then inspect git status and the " +
		"current diff before editing. Do not redo completed phases and do not rely on the compaction " +
		"summary as the only source of truth."
}

// compactInstructions is appended to /compact, and matches the pi extension's
// custom instructions.
func compactInstructions(nextPhase string) string {
	return "Preserve the ticket goal, requirements, cross-phase invariants, modified files, test results, " +
		"decisions, unresolved failures, and the named next phase: " + nextPhase + "."
}

// readCheckpointRecords reads the sidecar. A sidecar that does not exist yet is
// an empty one: the agent has not signalled a boundary.
func readCheckpointRecords(path string) ([]cli.CheckpointRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var recs []cli.CheckpointRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxTranscriptLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec cli.CheckpointRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// A half-written last line is what an append race looks like; the
			// next read sees it whole.
			continue
		}
		recs = append(recs, rec)
	}
	return recs, sc.Err()
}

// countAccepted is how many boundaries have already been taken on, which is
// where a controller built over an existing sidecar starts reading. Outcomes
// are written minutes after the boundary they answer, so counting those instead
// would replay a boundary whose outcome a daemon death swallowed.
func countAccepted(path string) int {
	recs, err := readCheckpointRecords(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range recs {
		if r.Kind == cli.CheckpointKindAccepted {
			n++
		}
	}
	return n
}

// maxTranscriptLine caps one JSONL line. Claude writes whole tool results into
// a single entry, so the default scanner buffer is far too small.
const maxTranscriptLine = 16 * 1024 * 1024

// transcriptScan is what one pass over a Claude session JSONL yields.
type transcriptScan struct {
	// contextTokens is the context the last non-sidechain assistant entry was
	// called with. hasContext is false when no entry carried usable usage.
	contextTokens int
	hasContext    bool
	// manualCompacts counts compact boundaries kontora asked for, and
	// lastPreTokens is the context the last of them started from.
	manualCompacts int
	lastPreTokens  int
	// turn is what the main conversation's last entry says the agent is doing.
	turn turnState
}

// turnState is how far through a turn the transcript says the agent is.
type turnState int

const (
	// turnUnknown is a transcript with nothing of the main conversation in it
	// yet, which says nothing about what the agent is doing.
	turnUnknown turnState = iota
	// turnRunning is a turn in flight: a prompt or a tool result waiting to be
	// answered, a tool call waiting to return, or a thinking block with the
	// reply still to come.
	turnRunning
	// turnEnded is the agent having replied and stopped, which is the only
	// transcript state that means it is waiting for input.
	turnEnded
)

// isSpeech reports whether an entry is part of the conversation itself rather
// than the bookkeeping Claude writes around it (progress, hook summaries, file
// snapshots, compact boundaries). Only those entries move the turn state.
func (e transcriptEntry) isSpeech() bool {
	if e.IsSidechain {
		return false
	}
	return e.Type == "assistant" || (e.Type == "user" && !e.IsMeta)
}

// endsTurn reports whether an assistant entry is the end of a reply: it says
// something and asks for no tool. Claude writes one entry per content block, so
// a thinking block on its own is a turn still in flight.
func (e transcriptEntry) endsTurn() bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(e.Message.Content, &blocks); err != nil {
		return false
	}
	said := false
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			return false
		case "text":
			said = true
		}
	}
	return said
}

// scanTranscript reads a Claude session JSONL once.
//
// Context size is taken from the last non-sidechain assistant entry rather than
// summed over the file: the prompt Claude is called with already carries the
// whole conversation, and a subagent's turn is a separate context that would
// otherwise inflate the number.
func scanTranscript(path string) (transcriptScan, error) {
	f, err := os.Open(path)
	if err != nil {
		return transcriptScan{}, err
	}
	defer f.Close()

	var out transcriptScan
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxTranscriptLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e transcriptEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.isSpeech() {
			out.turn = turnRunning
			if e.Type == "assistant" && e.endsTurn() {
				out.turn = turnEnded
			}
		}

		switch {
		case e.Type == "assistant" && !e.IsSidechain:
			u := e.Message.Usage
			if tokens := u.Input + u.CacheRead + u.CacheCreate; tokens > 0 {
				out.contextTokens = tokens
				out.hasContext = true
			}
		case e.Type == "system" && e.Subtype == "compact_boundary":
			if e.CompactMetadata.Trigger == "manual" {
				out.manualCompacts++
				out.lastPreTokens = e.CompactMetadata.PreTokens
			}
		}
	}
	return out, sc.Err()
}

// lastContextTokens reports the context the last non-sidechain assistant entry
// was called with, and whether the transcript carried one at all. An unknown
// count is what a run that has not replied yet, or one whose transcript cannot
// be read, reports; the caller treats it as a boundary to skip.
func lastContextTokens(path string) (int, bool) {
	scan, err := scanTranscript(path)
	if err != nil {
		return 0, false
	}
	return scan.contextTokens, scan.hasContext
}

// transcriptEntry is the part of a Claude session JSONL entry the controller
// reads.
type transcriptEntry struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Message     struct {
		Usage struct {
			Input       int `json:"input_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		// Content is a block list on an assistant entry and either that or a
		// bare string on a user one, so it is decoded on demand rather than
		// failing the whole entry.
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	CompactMetadata struct {
		Trigger   string `json:"trigger"`
		PreTokens int    `json:"preTokens"`
	} `json:"compactMetadata"`
}
