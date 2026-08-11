package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/prompt"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// The decisions `plannotator annotate --json` reports. Unlike `plannotator
// review`, annotate mode has a structured output contract
// (apps/hook/server/annotate-output.ts in the plannotator bundle), so the daemon
// parses a decision instead of matching sentinel lines on stdout.
const (
	annotateApproved  = "approved"
	annotateDismissed = "dismissed"
	annotateAnnotated = "annotated"
)

// annotateDecision is the JSON `plannotator annotate --gate --json` prints. A
// dismissed session carries no feedback.
type annotateDecision struct {
	Decision string `json:"decision"`
	Feedback string `json:"feedback"`
}

// parseAnnotateDecision reads plannotator's stdout. Anything that is not JSON
// naming a decision the daemon knows is an error, never feedback: acting on
// unrecognised output would rewrite a ticket from a diagnostic message.
func parseAnnotateDecision(stdout string) (annotateDecision, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return annotateDecision{}, fmt.Errorf("plannotator wrote no decision")
	}
	var dec annotateDecision
	if err := json.Unmarshal([]byte(trimmed), &dec); err != nil {
		return annotateDecision{}, fmt.Errorf("unparseable decision, plannotator wrote: %s", truncateForMessage(trimmed))
	}
	switch dec.Decision {
	case annotateApproved, annotateDismissed, annotateAnnotated:
		return dec, nil
	default:
		return annotateDecision{}, fmt.Errorf("unknown decision %q, plannotator wrote: %s", dec.Decision, truncateForMessage(trimmed))
	}
}

// truncateForMessage caps raw plannotator output quoted into an error or an SSE
// message.
func truncateForMessage(s string) string {
	return truncate(s, 200, "…")
}

// StartPlannotatorAnnotate opens the ticket's own markdown file in Plannotator's
// annotation UI. Submitted annotations park the ticket for a run that rewrites it
// and then restores its status; approving or dismissing leaves the ticket
// untouched.
//
// Unlike StartPlannotatorReview there is no worktree to build: the file lives in
// tickets_dir, so there is no merge base, no diff, and nothing to clean up.
//
// Synchronous errors:
//   - web.ErrTicketNotFound: unknown ticket
//   - web.ErrInvalidState: the ticket is not initialized, its status does not
//     allow editing, or an annotation run is already pending. The message names
//     which of the three it was.
//   - web.ErrPlannotatorInFlight: a review or annotation is already running
//   - web.ErrPlannotatorBinary: the plannotator binary is not on PATH
//
// Anything else goes over SSE as a plannotator_finished event.
func (d *Daemon) StartPlannotatorAnnotate(id string) error {
	log := d.ticketLog(id)

	if !ticket.IsSafeID(id) {
		return fmt.Errorf("%w: unsafe ticket id", web.ErrTicketNotFound)
	}

	cfg := d.config()
	// Checked here as well as in claimAnnotateSession, so that an unknown ticket or
	// a status that forbids an edit is reported instead of a missing binary.
	d.mu.Lock()
	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}
	refusal := annotateRefusal(cfg, ts.ticket)
	d.mu.Unlock()
	if refusal != nil {
		return refusal
	}

	binaryPath, err := d.plannotatorLookup(cfg.Plannotator.Binary)
	if err != nil {
		return fmt.Errorf("%w: %s", web.ErrPlannotatorBinary, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	filePath, err := d.claimAnnotateSession(id, cfg, cancel)
	if err != nil {
		cancel()
		return err
	}

	d.broker.Broadcast(web.TicketEvent{
		Type:   "plannotator_started",
		Ticket: web.TicketInfo{ID: id},
	})

	// The whole run uses the settings it started with, so a reload mid-session
	// cannot move the reviews directory or change the timeout.
	go d.runPlannotatorAnnotate(ctx, log, cfg.Plannotator, id, binaryPath, filePath)

	return nil
}

// claimAnnotateSession registers the session and returns the ticket file it
// opens. Every condition the session depends on is checked here, under the lock
// that registers it: the scheduler claims a ticket under the same lock, and a
// pickup that started a moment earlier would edit the file the reviewer is
// reading. From here runTicket leaves the ticket alone until the session closes.
func (d *Daemon) claimAnnotateSession(id string, cfg *config.Config, cancel context.CancelFunc) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ts, ok := d.tickets[id]
	if !ok {
		return "", web.ErrTicketNotFound
	}
	if err := annotateRefusal(cfg, ts.ticket); err != nil {
		return "", err
	}
	if _, running := d.running[id]; running {
		return "", fmt.Errorf("%w: the ticket is running", web.ErrInvalidState)
	}
	if _, inFlight := d.plannotator[id]; inFlight {
		return "", web.ErrPlannotatorInFlight
	}
	d.plannotator[id] = cancel
	return ts.filePath, nil
}

// annotateRefusal reports why a ticket cannot be opened in Plannotator's
// annotation UI, or nil when it can. buildTicketInfo asks the same question to
// decide whether the dashboard offers the button, so the UI cannot offer a pass
// the daemon refuses. Must be called with d.mu held, or on a ticket the caller
// owns.
func annotateRefusal(cfg *config.Config, t *ticket.Ticket) error {
	switch {
	case !t.Kontora:
		// The scheduler only picks up a kontora ticket, so annotating anything else
		// would park it with feedback nothing ever reads.
		return fmt.Errorf("%w: ticket is not initialized", web.ErrInvalidState)
	case !cfg.StatusAllowsEdit(string(t.Status)):
		return fmt.Errorf("%w: a ticket in %s cannot be edited", web.ErrInvalidState, t.Status)
	case t.AnnotationReturnStatus != "":
		// A second pass would overwrite the pending annotations and record the
		// parked status as the one to return to.
		return fmt.Errorf("%w: an annotation run is already pending", web.ErrInvalidState)
	}
	return nil
}

func (d *Daemon) runPlannotatorAnnotate(ctx context.Context, log *slog.Logger, pcfg config.Plannotator, id, binaryPath, filePath string) {
	defer d.releasePlannotator(id)

	fail := func(msg string, args ...any) {
		log.Error("plannotator annotate: "+msg, args...)
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeError,
			Message: msg,
		})
	}

	stdout, err := d.plannotatorSpawner(ctx, PlannotatorParams{
		Binary: binaryPath,
		// --gate adds the Approve button, --json makes the outcome a decision
		// the daemon can switch on. Plannotator does not write the file itself,
		// so the annotation run is what applies the annotations.
		Args:    []string{"annotate", filePath, "--gate", "--json"},
		Dir:     filepath.Dir(filePath),
		Env:     map[string]string{"PLANNOTATOR_REMOTE": "0"},
		Timeout: pcfg.Timeout.Duration,
	})
	if err != nil {
		fail("spawn failed: "+err.Error(), "err", err)
		return
	}

	dec, err := parseAnnotateDecision(stdout)
	if err != nil {
		fail(err.Error(), "err", err)
		return
	}

	if dec.Decision != annotateAnnotated || strings.TrimSpace(dec.Feedback) == "" {
		outcome := web.PlannotatorOutcomeApproved
		if dec.Decision != annotateApproved {
			outcome = web.PlannotatorOutcomeCancelled
		}
		// An approval may carry notes. They are not feedback to act on, so the
		// ticket is left alone; log the size so the drop is visible.
		log.Info("plannotator annotate: no changes requested",
			"decision", dec.Decision, "feedback_bytes", len(dec.Feedback))
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: outcome,
		})
		return
	}

	reviewsDir := config.ExpandTilde(pcfg.ReviewsDir)
	if mkErr := os.MkdirAll(reviewsDir, 0o755); mkErr != nil {
		fail("mkdir reviews_dir: "+mkErr.Error(), "err", mkErr)
		return
	}
	annotationsFile := annotationsPath(reviewsDir, id)
	if wErr := os.WriteFile(annotationsFile, []byte(dec.Feedback), 0o644); wErr != nil {
		fail("write annotations: "+wErr.Error(), "err", wErr, "path", annotationsFile)
		return
	}

	if pErr := d.parkForAnnotation(id); pErr != nil {
		// The annotations file stays: it is the only copy of what the reviewer
		// wrote, and the message names it so nothing has to be retyped.
		fail(fmt.Sprintf("park: %s (annotations kept at %s)", pErr, annotationsFile), "err", pErr)
		return
	}

	log.Info("plannotator annotate: feedback captured, ticket parked", "bytes", len(dec.Feedback))
	d.broker.Broadcast(web.TicketEvent{
		Type:    "plannotator_finished",
		Ticket:  web.TicketInfo{ID: id},
		Outcome: web.PlannotatorOutcomeAnnotated,
	})
}

// annotationsPath is where a ticket's pending annotations wait for the run that
// applies them.
func annotationsPath(reviewsDir, id string) string {
	return filepath.Join(reviewsDir, id+prompt.AnnotationsSuffix)
}

// parkForAnnotation records the ticket's current status in
// annotation_return_status and sends it back to todo with attempt=0, leaving the
// stage alone so the annotation run can continue that stage's session.
//
// The whole read-modify-write holds d.mu, because a scheduler pickup takes the
// same lock to claim a ticket. Without that, a pickup could claim the ticket
// between the read and the write and then write back a ticket it read before the
// park, which would drop the marker and orphan the annotations.
func (d *Daemon) parkForAnnotation(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	ts, ok := d.tickets[id]
	if !ok {
		return web.ErrTicketNotFound
	}
	filePath := ts.filePath
	if _, running := d.running[id]; running {
		return fmt.Errorf("%w: the ticket started running while the annotation session was open", web.ErrInvalidState)
	}

	// Read from disk rather than from the cache: the file is the state a pickup
	// would act on.
	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return fmt.Errorf("re-read ticket: %w", err)
	}
	// A Plannotator session can stay open for as long as its timeout allows, and
	// the ticket can be moved or finished in the meantime. Parking then would
	// record a status the annotation run must not restore.
	if !d.config().StatusAllowsEdit(string(t2.Status)) {
		return fmt.Errorf("%w: ticket moved to %s while the annotation session was open", web.ErrInvalidState, t2.Status)
	}
	if t2.AnnotationReturnStatus != "" {
		return fmt.Errorf("%w: the ticket is already parked for an annotation run", web.ErrInvalidState)
	}

	if err := t2.SetField("annotation_return_status", string(t2.Status)); err != nil {
		return fmt.Errorf("set annotation_return_status: %w", err)
	}
	if err := t2.SetField("status", string(ticket.StatusTodo)); err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	// The stage runs again from the start against the rewritten ticket, so its
	// attempts start over. last_error is left alone: a ticket parked from paused
	// returns to paused, and the reason it paused still applies.
	if err := t2.SetField("attempt", 0); err != nil {
		return fmt.Errorf("reset attempt: %w", err)
	}
	if err := d.writeTicketLocked(t2, filePath); err != nil {
		return fmt.Errorf("write ticket: %w", err)
	}

	d.setTicketState(id, t2, filePath)
	d.enqueue(t2)
	d.broadcastTicketUpdate(id)
	return nil
}

// runAnnotationRun hands the agent the reviewer's annotations and the ticket
// file, and asks it to rewrite the ticket. It does not evaluate the pipeline, so
// the run cannot advance the ticket or repeat the stage's work; on success the
// ticket returns to the status it was annotated in.
func (d *Daemon) runAnnotationRun(ctx, taskCtx context.Context, cfg *config.Config, log *slog.Logger, ticketID string, t *ticket.Ticket, filePath string) {
	returnStatus := t.AnnotationReturnStatus

	// The annotations are read here to find out whether the run has anything to
	// act on, and kept so that the rendered prompt can be checked against them
	// below. The prompt itself reads the file through {{ plannotatorAnnotations }}.
	annotationsFile := annotationsPath(config.ExpandTilde(cfg.Plannotator.ReviewsDir), ticketID)
	pendingBytes, readErr := os.ReadFile(annotationsFile)
	pending := strings.TrimSpace(string(pendingBytes))
	switch {
	case readErr != nil && !os.IsNotExist(readErr):
		// The file is there but cannot be read. Running the agent would render an
		// empty set of notes, and its clean exit would then delete the file and
		// clear the marker as if they had been applied.
		log.Error("annotation: reading the annotations failed", "path", annotationsFile, "err", readErr)
		d.pauseTicket(t, filePath, "annotation: reading the annotations failed: "+readErr.Error())
		return
	case pending == "":
		// Nothing to act on: the file was removed, or reviews_dir moved between the
		// annotation and this pickup. Checked before the agent is resolved, so the
		// ticket is handed back rather than paused over an agent it no longer needs.
		log.Warn("annotation: no annotations pending, returning the ticket to its status",
			"path", annotationsFile, "status", returnStatus)
		d.clearAnnotationMarker(log, ticketID, filePath, returnStatus)
		return
	}

	agentName := d.reworkAgent(cfg, t)
	agentCfg, ok := cfg.Agents[agentName]
	if !ok {
		log.Error("annotation: unknown agent", "agent", agentName)
		d.pauseTicket(t, filePath, fmt.Sprintf("annotation: unknown agent %q", agentName))
		return
	}

	stageName := annotationStageName
	switch {
	case t.Stage != "":
		stageName = t.Stage
	case t.Pipeline == "":
		// A ticket with no pipeline runs under the simple key, which is where its
		// session, its log and its history rows already are.
		stageName = simpleStageName
	}

	wtPath, err := d.annotationWorkDir(t, ticketID)
	if err != nil {
		log.Error("annotation: resolve work dir failed", "err", err)
		d.pauseTicket(t, filePath, "annotation: resolve work dir failed: "+err.Error())
		return
	}

	_ = t.SetField("status", string(ticket.StatusInProgress))
	_ = t.SetField("started_at", time.Now().Format(time.RFC3339))
	_ = t.SetField("claimed_by", d.instanceName)
	// Only last_log moves: unlike a stage pickup this run must not clear summary
	// or final_summary, which still describe the work the ticket has done.
	_ = t.SetField("last_log", d.stageLogPath(ticketID, stageName))
	if err := d.writeTicket(t, filePath); err != nil {
		log.Error("annotation: write failed", "phase", "pickup", "err", err)
		return
	}
	log.Info("annotation picked up", "agent", agentName, "stage", stageName, "dir", wtPath)
	d.broadcastTicketUpdateLocking(ticketID)

	if err := taskCtx.Err(); err != nil {
		log.Info("annotation cancelled before the agent started")
		return
	}

	tmpl := cfg.AnnotationPrompt
	if tmpl == "" {
		tmpl = defaultAnnotationPrompt
	}
	rendered, err := d.renderTicketPrompt(cfg, tmpl, t, filePath, wtPath)
	if err != nil {
		log.Error("annotation: render prompt failed", "err", err)
		d.pauseTicket(t, filePath, "annotation: render prompt failed: "+err.Error())
		return
	}
	if !strings.Contains(rendered, pending) {
		// The agent would be asked to answer notes it was never given, would report
		// success, and that success is what deletes the notes. Reached by an
		// annotation_prompt that leaves out {{ plannotatorAnnotations }}.
		log.Error("annotation: the rendered prompt does not carry the annotations", "path", annotationsFile)
		d.pauseTicket(t, filePath,
			"annotation: the rendered prompt does not carry the annotations; annotation_prompt must include {{ plannotatorAnnotations }}")
		return
	}
	if rendered != "" {
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, false)
	}

	runIndex := stageRunIndex(t, stageName)
	priorSummary := t.Summary
	run, spawnOK := d.spawnAgentRun(taskCtx, t, spawnAgentParams{
		cfg:       cfg,
		ctx:       ctx,
		log:       log,
		ticketID:  ticketID,
		filePath:  filePath,
		stageName: stageName,
		run:       runIndex,
		wtPath:    wtPath,
		rendered:  rendered,
		agentCfg:  agentCfg,
		// Only the timeout is borrowed from the stage. Its prompt describes stage
		// work, which is the one thing this run must not do.
		stageCfg:   config.Stage{Timeout: cfg.Stages[stageName].Timeout},
		isPipeline: false,
		annotation: true,
	})
	if !spawnOK {
		return
	}

	if taskCtx.Err() != nil {
		if ctx.Err() != nil {
			log.Warn("annotation: interrupted by shutdown")
			return
		}
		log.Info("annotation: interrupted by user")
		return
	}

	d.finishAnnotationRun(log, annotationExit{
		ticketID:        ticketID,
		filePath:        filePath,
		annotationsFile: annotationsFile,
		stageName:       stageName,
		runIndex:        runIndex,
		agentName:       agentName,
		returnStatus:    returnStatus,
		priorSummary:    priorSummary,
		run:             run,
	})
}

// clearAnnotationMarker hands the ticket back its status without running
// anything. It is the way out of a pickup that has no annotations to act on,
// which would otherwise repeat on every transition to todo.
func (d *Daemon) clearAnnotationMarker(log *slog.Logger, ticketID, filePath string, returnStatus ticket.Status) {
	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		log.Error("annotation: re-read failed", "phase", "clear marker", "err", err)
		return
	}
	// The same guards the exit handler applies: whoever moved or claimed the
	// ticket while this pickup was starting owns it now.
	if d.isUserOverride(t2.Status) {
		log.Info("annotation: user override before the run started", "status", t2.Status)
		return
	}
	if d.claimedElsewhere(t2) {
		log.Info("annotation: claimed by another instance", "claimed_by", t2.ClaimedBy)
		return
	}
	status, lastError := d.restoredStatus(returnStatus)
	_ = t2.SetField("status", string(status))
	_ = t2.SetField("annotation_return_status", "")
	if lastError != "" {
		_ = t2.SetField("last_error", lastError)
	}
	if err := d.writeTicket(t2, filePath); err != nil {
		log.Error("annotation: write failed", "phase", "clear marker", "err", err)
		return
	}
	d.mu.Lock()
	d.setTicketState(ticketID, t2, filePath)
	if t2.Status == ticket.StatusTodo {
		d.enqueue(t2)
	}
	d.broadcastTicketUpdate(ticketID)
	d.mu.Unlock()
}

// annotationStageName keys the log of an annotation run on a ticket whose
// pipeline has not started, so the run does not take the key of a stage that has
// yet to run. Nothing has recorded a session under it, so such a run always
// starts fresh.
const annotationStageName = "annotation"

type annotationExit struct {
	ticketID        string
	filePath        string
	annotationsFile string
	stageName       string
	runIndex        int
	agentName       string
	// returnStatus is the status the ticket carried when it was annotated, read
	// before the run so a rewrite of the frontmatter cannot lose it.
	returnStatus ticket.Status
	// priorSummary is the summary the ticket's own work left behind, read before
	// the run so this one can put it back.
	priorSummary string
	run          agentRun
}

// finishAnnotationRun applies the run's outcome: the ticket's own status on
// success, paused with last_error otherwise. It records one annotation history
// entry either way, and starts no final-summary pass.
func (d *Daemon) finishAnnotationRun(log *slog.Logger, p annotationExit) {
	t2, err := ticket.ParseFile(p.filePath)
	if err != nil {
		log.Error("annotation: re-read failed after agent exit", "err", err)
		return
	}
	if d.isUserOverride(t2.Status) {
		log.Info("annotation: user override during execution", "status", t2.Status)
		return
	}
	if d.claimedElsewhere(t2) {
		log.Info("annotation: discarding exit result, claimed by another instance", "claimed_by", t2.ClaimedBy)
		d.killTaskWindow(p.ticketID)
		return
	}

	result := p.run.Result
	// Every run is asked for a summary, and `kontora summary` writes it to the
	// ticket. This one describes a ticket rewrite rather than the work the ticket
	// did, so it goes to the history row and the ticket gets its own summary back.
	written := t2.Summary
	if written == p.priorSummary {
		written = ""
	}
	_ = t2.SetField("summary", p.priorSummary)
	history := t2.History
	history = append(history, ticket.HistoryEntry{
		Stage:         p.stageName,
		Agent:         p.agentName,
		ExitCode:      result.ExitCode,
		Run:           p.runIndex,
		StartedAt:     t2.StartedAt,
		CompletedAt:   &result.ExitedAt,
		Summary:       runSummary(written, p.run.FinalMessage),
		Kind:          ticket.KindAnnotation,
		SessionReused: p.run.Resumed,
	})
	_ = t2.SetField("history", history)

	done := result.ExitCode == 0
	if done {
		status, lastError := d.restoredStatus(p.returnStatus)
		_ = t2.SetField("status", string(status))
		_ = t2.SetField("annotation_return_status", "")
		if lastError != "" {
			_ = t2.SetField("last_error", lastError)
		}
		log.Info("annotation completed", "status", status, "session_reused", p.run.Resumed)
		d.killTaskWindow(p.ticketID)
	} else {
		// annotation_return_status and the annotations file both stay, so a
		// retry runs against the same feedback.
		_ = t2.SetField("status", string(ticket.StatusPaused))
		_ = t2.SetField("last_error", fmt.Sprintf("annotation agent exited with code %d", result.ExitCode))
		log.Warn("annotation paused", "exit_code", result.ExitCode)
	}

	if err := d.writeTicket(t2, p.filePath); err != nil {
		// The annotations stay on disk: they are the only copy of what the reviewer
		// wrote, and the marker is still set, so the next pickup runs against them
		// again.
		log.Error("annotation: write failed", "phase", "exit", "err", err)
		return
	}
	// Removed only once the ticket no longer says an annotation is pending. The
	// other order loses the feedback if that write fails.
	if done {
		if rmErr := os.Remove(p.annotationsFile); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Warn("annotation: removing the annotations file failed", "path", p.annotationsFile, "err", rmErr)
		}
	}

	d.mu.Lock()
	d.setTicketState(p.ticketID, t2, p.filePath)
	// A ticket annotated while it was waiting to run goes back to waiting, and a
	// todo ticket that is not queued would sit there until something else moved
	// it.
	if t2.Status == ticket.StatusTodo {
		d.enqueue(t2)
	}
	d.broadcastTicketUpdate(p.ticketID)
	d.mu.Unlock()
}

// restoredStatus is the status an annotation run hands back, with the error to
// record beside it. A custom status the config has dropped since the ticket was
// annotated would take the ticket off the board, and clearing the marker leaves
// nothing to recover it from, so such a ticket pauses instead.
func (d *Daemon) restoredStatus(returnStatus ticket.Status) (status ticket.Status, lastError string) {
	if d.config().StatusAllowsEdit(string(returnStatus)) {
		return returnStatus, ""
	}
	return ticket.StatusPaused,
		fmt.Sprintf("annotation: %q is no longer a configured status", returnStatus)
}

// annotationWorkDir picks the directory the run works in: the ticket's worktree
// when a previous run made one, and the repository itself otherwise. It creates
// neither a worktree nor a branch, because the run edits one file in tickets_dir
// and a ticket annotated before its first stage has no work to put on a branch.
func (d *Daemon) annotationWorkDir(t *ticket.Ticket, ticketID string) (string, error) {
	repoName, repoPath, err := d.resolvePath(t)
	if err != nil {
		return "", err
	}
	wtPath := d.worktrees.Path(repoName, ticketID)
	if fi, statErr := os.Stat(wtPath); statErr == nil && fi.IsDir() {
		return wtPath, nil
	}
	return repoPath, nil
}
