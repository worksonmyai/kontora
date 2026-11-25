package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/worktree"
)

type WorkflowWaitTimer interface {
	C() <-chan time.Time
	Stop() bool
}
type WorkflowWaitClock interface {
	Now() time.Time
	NewTimer(time.Duration) WorkflowWaitTimer
}
type realWorkflowWaitClock struct{}
type realWorkflowWaitTimer struct{ *time.Timer }

func (realWorkflowWaitClock) Now() time.Time { return time.Now() }
func (realWorkflowWaitClock) NewTimer(d time.Duration) WorkflowWaitTimer {
	return realWorkflowWaitTimer{time.NewTimer(d)}
}
func (t realWorkflowWaitTimer) C() <-chan time.Time { return t.Timer.C }
func WithWorkflowWaitClock(clock WorkflowWaitClock) Option {
	return func(d *Daemon) { d.workflowClock = clock }
}
func (d *Daemon) waitNow() time.Time {
	if d.workflowClock == nil {
		return time.Now()
	}
	return d.workflowClock.Now()
}
func (d *Daemon) signalWorkflowWait() {
	select {
	case d.workflowWaitWake <- struct{}{}:
	default:
	}
}

func workflowStateVersion(graph *config.WorkflowGraph) int {
	return pipeline.GraphStateVersion(graph)
}
func workflowWait(t *ticket.Ticket) *ticket.WorkflowWait {
	if t == nil || t.GraphState == nil || t.GraphState.Wait == nil || t.Status == ticket.StatusCancelled || t.Status == ticket.StatusArchived || t.Status == ticket.StatusDone {
		return nil
	}
	return t.GraphState.Wait
}
func workflowReservation(t *ticket.Ticket) *ticket.WorkflowWait {
	if w := workflowWait(t); w != nil {
		return w
	}
	if t != nil && t.GraphState != nil && t.GraphState.Continuation && len(t.GraphState.WaitHistory) > 0 && t.Status != ticket.StatusCancelled && t.Status != ticket.StatusDone && t.Status != ticket.StatusArchived {
		return &t.GraphState.WaitHistory[len(t.GraphState.WaitHistory)-1]
	}
	return nil
}
func workflowWaitArmed(t *ticket.Ticket, w *ticket.WorkflowWait) bool {
	return !w.Disarmed && w.Phase != "recovery_required" && (t.GraphState.Wait != nil || t.Status == ticket.StatusTodo)
}
func retainFinishedWait(s *ticket.GraphState) {
	if s.Wait != nil {
		s.WaitHistory = append(s.WaitHistory, *s.Wait.Clone())
		s.Wait = nil
	}
	s.Continuation = false
}
func validateWaitWorkspace(t *ticket.Ticket, w *ticket.WorkflowWait) error {
	if w.Worktree == "" || w.Repository == "" || w.Branch == "" || t.Branch != w.Branch {
		return fmt.Errorf("workflow wait workspace identity is missing or changed")
	}
	common, err := worktree.CommonDir(expandTilde(t.Path))
	if err != nil || common != w.Repository {
		return fmt.Errorf("workflow wait repository identity changed: %v", err)
	}
	path, err := worktree.FindWorktreeForBranch(expandTilde(t.Path), w.Branch)
	if err != nil || path == "" {
		return fmt.Errorf("workflow wait worktree is missing: %v", err)
	}
	actual, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("workflow wait worktree is unavailable: %w", err)
	}
	expected, err := filepath.EvalSymlinks(w.Worktree)
	if err != nil || actual != expected {
		return fmt.Errorf("workflow wait worktree identity changed")
	}
	common, err = worktree.CommonDir(w.Worktree)
	if err != nil || common != w.Repository {
		return fmt.Errorf("workflow wait worktree repository changed")
	}
	return nil
}

func (d *Daemon) agentWaitAdvance(t *ticket.Ticket, rec *executionRecord, run agentRun) pipeline.GraphAdvance {
	s := t.GraphState.Clone()
	after := pipeline.GraphAdvance{Stage: t.Stage, State: s, Status: ticket.StatusPaused}
	if run.WaitRequest == nil || s.Revision == math.MaxUint64 || rec.Invocation.WakeMax <= s.WakeCount {
		after.Reason = "invalid suspension or exhausted wake allowance"
		return after
	}
	retainFinishedWait(s)
	s.Version = 2
	s.Revision++
	r := run.WaitRequest
	now := d.waitNow()
	w := &ticket.WorkflowWait{ID: newSessionID(), Kind: r.Kind, Phase: "pending", ExecutionID: s.ExecutionID, NodeID: t.Stage,
		VisitID: s.VisitID, InvocationID: rec.Invocation.ID, RequestedAt: now, Until: r.Until, Deadline: r.Deadline,
		Command: r.Command, Poll: r.Poll, Next: run.NextStep, Worktree: rec.Worktree, Repository: rec.RepoCommonDir, Branch: t.Branch}
	if r.Kind == "check" {
		w.NextPoll = now
	}
	s.Wait = w
	return after
}

// A pending control wait never creates a physical invocation or a history row.
func (d *Daemon) finishControlAdvance(t *ticket.Ticket, after pipeline.GraphAdvance) error {
	before := t.Clone()
	if after.Wait != nil && after.State.Wait == nil {
		node := after.Wait
		branch := effectiveClaimBranch(d.config(), t)
		common, err := worktree.CommonDir(expandTilde(t.Path))
		if err != nil {
			return err
		}
		if holder, exists := d.branchHolderLocked(common + "\x00" + branch); exists && holder != t.ID {
			return fmt.Errorf("branch reserved by ticket %s", holder)
		}
		path, err := worktree.FindWorktreeForBranch(expandTilde(t.Path), branch)
		if err != nil {
			return err
		}
		// Entry human waits need no checkout; repository identity still reserves the branch.
		if path == "" && node.Mode == "deployment" {
			return fmt.Errorf("deployment wait requires an existing worktree")
		}
		now := d.waitNow()
		w := &ticket.WorkflowWait{ID: newSessionID(), Kind: node.Mode, Phase: "pending", ExecutionID: after.State.ExecutionID,
			NodeID: node.ID, VisitID: after.State.VisitID, RequestedAt: now, Worktree: path, Repository: common, Branch: branch, Environment: node.Environment}
		if node.Timeout.Duration > 0 {
			w.Deadline = now.Add(node.Timeout.Duration)
		}
		if node.Mode == "deployment" {
			repository, commit, err := worktree.DeploymentEvidence(expandTilde(t.Path), path)
			if err != nil {
				return err
			}
			w.Repository, w.RequiredCommit = repository, commit
		}
		after.State.Version, after.State.Wait = 2, w
		_ = t.SetField("branch", branch)
	}
	if err := installGraphAdvance(t, after); err != nil {
		return err
	}
	if after.Status == ticket.StatusDone {
		_ = t.SetField("completed_at", d.waitNow())
		if err := d.persistControlCleanup(t); err != nil {
			return err
		}
	}
	return d.persistControlTransition(before, t)
}

func (d *Daemon) patchWorkflowWait(snapshot *ticket.Ticket, path string, patch func(*ticket.Ticket) error) (*ticket.Ticket, error) {
	var saved *ticket.Ticket
	err := d.withTicketMutationOrigin(notify.OriginDaemon, func(repo app.MutationRepository) error {
		st, err := repo.Get(snapshot.ID)
		if err != nil {
			return err
		}
		t := st.Ticket
		w, before := workflowWait(t), workflowWait(snapshot)
		if w == nil || before == nil || st.FilePath != path || t.Path != snapshot.Path || t.Branch != snapshot.Branch || t.Pipeline != snapshot.Pipeline || t.HasOwnershipMetadata() || d.claimedElsewhere(t) || t.IsRunning() || t.InvocationID != "" ||
			t.GraphState.ExecutionID != snapshot.GraphState.ExecutionID || t.GraphState.Revision != snapshot.GraphState.Revision || w.ID != before.ID || w.ExecutionID != t.GraphState.ExecutionID || w.NodeID != t.Stage || t.GraphState.Revision == math.MaxUint64 {
			return errExecutionChanged
		}
		graph := d.config().Pipelines[t.Pipeline].Graph
		if graph == nil {
			return fmt.Errorf("workflow definition changed")
		}
		fingerprint, err := workflowFingerprint(d.config(), t, graph)
		if err != nil || fingerprint != t.GraphState.DefinitionFingerprint || fingerprint != snapshot.GraphState.DefinitionFingerprint {
			return fmt.Errorf("workflow definition changed; restore it before continuing")
		}
		if _, err := loadExecution(d.config(), t.ID); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workflow invocation journal must retire before wait progress")
		}
		s := t.GraphState.Clone()
		s.Revision++
		_ = t.SetField("graph_state", s)
		if err := patch(t); err != nil {
			return err
		}
		if err := t.SetField("graph_state", t.GraphState); err != nil {
			return err
		}
		if err := repo.Save(st); err != nil {
			return err
		}
		saved = t.Clone()
		return nil
	})
	if err == nil {
		d.broadcastTicketUpdateLocking(snapshot.ID)
	}
	return saved, err
}

func (d *Daemon) enqueueWorkflowContinuation(t *ticket.Ticket) {
	if t == nil || t.Status != ticket.StatusTodo {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ts, ok := d.tickets[t.ID]; ok && ts.ticket.GraphState != nil && ts.ticket.GraphState.Continuation {
		d.enqueue(ts.ticket)
	}
}
func wakeWorkflowAgent(t *ticket.Ticket, reason string) error {
	s, w := t.GraphState, t.GraphState.Wait
	s.WakeCount++
	s.Wake = &ticket.WakeContext{Reason: reason, Output: w.Output, Count: s.WakeCount, Next: w.Next, WaitID: w.ID, Receipt: w.Receipt.Clone()}
	s.Continuation = true
	w.Phase = "ready"
	w.PollID = ""
	if err := t.SetField("graph_state", s); err != nil {
		return err
	}
	_ = t.SetField("last_error", "")
	return t.SetField("status", string(ticket.StatusTodo))
}
func (d *Daemon) applyWaitReason(t *ticket.Ticket, reason string) error {
	w := t.GraphState.Wait
	if w.Kind == "human" || w.Kind == "deployment" {
		after := pipeline.ReleaseGraphWait(d.config().Pipelines[t.Pipeline].Graph, t.Stage, t.GraphState, "timeout")
		if after.Rejected {
			return fmt.Errorf("release wait: %s", after.Reason)
		}
		if after.Next != nil {
			after.State.Continuation = true
		}
		return d.finishControlAdvance(t, after)
	}
	return wakeWorkflowAgent(t, reason)
}
func (d *Daemon) workflowControlOnlyLocked(t *ticket.Ticket) bool {
	graph := d.config().Pipelines[t.Pipeline].Graph
	if graph == nil || t.InvocationID != "" {
		return false
	}
	stage := t.Stage
	if stage == "" {
		stage = graph.Entry
	}
	node, ok := graph.Node(stage)
	return ok && (node.Type == "wait" || node.Type == "end")
}
func (d *Daemon) workflowWaitLoop(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	// One worker and one coalesced request keep receipt history off the timer
	// path without creating a goroutine or a queued scan for every wake.
	events := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-events:
				d.reconcileWorkflowEventsContext(ctx)
			}
		}
	}()
	defer func() { cancel(); <-done }()
	for ctx.Err() == nil {
		d.reconcileControlTransitions()
		d.reconcileWorkflowWaits(ctx)
		d.reconcileControlCleanup()
		select {
		case events <- struct{}{}:
		default:
		}
		clock := d.workflowClock
		if clock == nil {
			clock = realWorkflowWaitClock{}
		}
		timer := clock.NewTimer(d.nextWorkflowWait(d.waitNow()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		case <-d.workflowWaitWake:
		}
		timer.Stop()
	}
}
func (d *Daemon) workflowProbeQueuedLocked(id string) bool {
	for _, job := range d.workflowProbeJobs {
		if job.snapshot.ID == id {
			return true
		}
	}
	return false
}
func (d *Daemon) nextWorkflowWait(now time.Time) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	delay := time.Minute
	for id, ts := range d.tickets {
		w := workflowWait(ts.ticket)
		if w == nil || w.Disarmed || ts.ticket.IsRunning() || w.Phase == "ready" || w.Phase == "recovery_required" {
			continue
		}
		times := []time.Time{w.Deadline, w.Until}
		if !d.workflowProbeActiveLocked(id) && !d.workflowProbeQueuedLocked(id) {
			times = append(times, w.NextPoll)
		}
		for _, at := range times {
			if !at.IsZero() {
				delay = min(delay, max(at.Sub(now), 10*time.Millisecond))
			}
		}
	}
	return delay
}
func (d *Daemon) reconcileWorkflowWaits(ctx context.Context) {
	workflowEventsMu.Lock()
	defer workflowEventsMu.Unlock()
	d.mu.Lock()
	for id, p := range d.workflowProbes {
		ts := d.tickets[id]
		if ts == nil || workflowWait(ts.ticket) == nil || workflowWait(ts.ticket).Disarmed {
			if p.cancel != nil {
				p.cancel()
			}
		}
	}
	var snapshots []*ticketState
	for _, ts := range d.tickets {
		if w := workflowReservation(ts.ticket); w != nil && !ts.ticket.IsRunning() && ts.ticket.InvocationID == "" {
			snapshots = append(snapshots, &ticketState{ticket: ts.ticket.Clone(), filePath: ts.filePath})
		}
	}
	d.mu.Unlock()
	slices.SortFunc(snapshots, func(a, b *ticketState) int {
		if a.ticket.ID < b.ticket.ID {
			return -1
		}
		if a.ticket.ID > b.ticket.ID {
			return 1
		}
		return 0
	})
	for _, ts := range snapshots {
		if ctx.Err() != nil {
			return
		}
		t, w := ts.ticket, workflowReservation(ts.ticket)
		if !workflowWaitArmed(t, w) {
			continue
		}
		if err := d.validateWaitProgress(t, w); err != nil {
			d.pauseInvalidWorkflowWait(t, ts.filePath, err.Error())
			continue
		}
		if t.GraphState.Continuation {
			d.enqueueWorkflowContinuation(t)
			continue
		}
		now := d.waitNow()
		reason := ""
		if !w.Deadline.IsZero() && !now.Before(w.Deadline) {
			reason = "deadline"
		} else if w.Kind == "timer" && !now.Before(w.Until) {
			reason = "timer"
		}
		d.mu.Lock()
		active := d.workflowProbes[t.ID]
		if reason != "" && active != nil && active.cancel != nil {
			active.cancel()
		}
		d.mu.Unlock()
		if active != nil {
			continue
		}
		if reason != "" && d.canExpireWorkflowWait(t) {
			saved, err := d.patchWorkflowWait(t, ts.filePath, func(current *ticket.Ticket) error { return d.applyWaitReason(current, reason) })
			if err == nil {
				d.enqueueWorkflowContinuation(saved)
			}
			continue
		}
		if w.Kind == "check" && !now.Before(w.NextPoll) {
			d.queueWorkflowProbe(t, ts.filePath)
		}
	}
}

// Accepted releases retain authority past the deadline while event recovery
// applies their journaled transitions. Unreadable journals also prevent expiry.
func (d *Daemon) canExpireWorkflowWait(t *ticket.Ticket) bool {
	switch workflowWait(t).Kind {
	case "human", "deployment":
		return d.checkAcceptedWaitRelease(t, "") == nil
	default:
		return true
	}
}

func (d *Daemon) validateWaitProgress(t *ticket.Ticket, w *ticket.WorkflowWait) error {
	graph := d.config().Pipelines[t.Pipeline].Graph
	if graph == nil {
		return fmt.Errorf("workflow definition changed; restore it before continuing")
	}
	fingerprint, err := workflowFingerprint(d.config(), t, graph)
	if err != nil || fingerprint != t.GraphState.DefinitionFingerprint {
		return fmt.Errorf("workflow definition changed; restore it before continuing")
	}
	if w.Worktree != "" || (w.Kind != "human" && w.Kind != "deployment") {
		return validateWaitWorkspace(t, w)
	}
	common, err := worktree.CommonDir(expandTilde(t.Path))
	if err != nil || common != w.Repository || w.Branch != t.Branch {
		return fmt.Errorf("workflow wait workspace identity changed")
	}
	return nil
}
func (d *Daemon) pauseInvalidWorkflowWait(snapshot *ticket.Ticket, path, reason string) {
	_ = d.withTicketMutationOrigin(notify.OriginDaemon, func(repo app.MutationRepository) error {
		st, err := repo.Get(snapshot.ID)
		if err != nil {
			return err
		}
		t := st.Ticket
		w, before := workflowReservation(t), workflowReservation(snapshot)
		if w == nil || before == nil || st.FilePath != path || w.ID != before.ID || t.GraphState.ExecutionID != snapshot.GraphState.ExecutionID || t.GraphState.Revision != snapshot.GraphState.Revision || t.IsRunning() {
			return errExecutionChanged
		}
		s := t.GraphState.Clone()
		s.Revision++
		if s.Wait != nil {
			s.Wait.Disarmed = true
		}
		_ = t.SetField("graph_state", s)
		_ = t.SetField("last_error", reason)
		_ = t.SetField("status", string(ticket.StatusPaused))
		d.cancelWorkflowProbeLocked(t.ID)
		return repo.Save(st)
	})
}

// Cleanup also runs inside recovery's mutex, so read durable reservations here.
func (d *Daemon) workflowWorktreeReserved(repoPath, path string) bool {
	common, err := worktree.CommonDir(repoPath)
	if err != nil {
		return true
	}
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	owns := func(w *ticket.WorkflowWait) bool {
		if w == nil || w.Repository != common {
			return false
		}
		candidate := w.Worktree
		if candidate == "" {
			candidate, err = worktree.FindWorktreeForBranch(repoPath, w.Branch)
			if err != nil {
				return true
			}
		}
		if candidate == "" {
			return false
		}
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		return resolveErr != nil || resolved == target
	}
	entries, err := os.ReadDir(expandTilde(d.config().TicketsDir))
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		t, readErr := ticket.ParseFile(filepath.Join(expandTilde(d.config().TicketsDir), entry.Name()))
		if readErr != nil {
			return true
		}
		if owns(workflowReservation(t)) {
			return true
		}
	}
	journals, err := filepath.Glob(filepath.Join(expandTilde(d.config().LogsDir), "*", "workflow-probe.json"))
	if err != nil {
		return true
	}
	for _, journal := range journals {
		data, readErr := os.ReadFile(journal)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return true
		}
		var j workflowProbeJournal
		if json.Unmarshal(data, &j) != nil {
			return true
		}
		if j.Result != nil && j.Result.Stopped {
			continue
		}
		if j.Active != nil && owns(j.Active.Wait) {
			return true
		}
	}
	return false
}

func controlCleanupPath(cfg *config.Config, id string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), id, "workflow-cleanup.json")
}
func (d *Daemon) persistControlCleanup(t *ticket.Ticket) error {
	// Agent finalization already journals its cleanup; this also covers release-only completion.
	data, err := t.Marshal()
	if err != nil {
		return err
	}
	path := controlCleanupPath(d.config(), t.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}
func (d *Daemon) reconcileControlCleanup() {
	d.mu.Lock()
	var tickets []*ticket.Ticket
	for _, ts := range d.tickets {
		if ts.ticket.Status == ticket.StatusDone && ts.ticket.GraphState != nil {
			tickets = append(tickets, ts.ticket.Clone())
		}
	}
	d.mu.Unlock()
	for _, t := range tickets {
		path := controlCleanupPath(d.config(), t.ID)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		saved, err := ticket.ParseBytes(data)
		if err != nil || saved.GraphState == nil || saved.GraphState.ExecutionID != t.GraphState.ExecutionID || saved.GraphState.Revision != t.GraphState.Revision {
			continue
		}
		d.cleanupWorktree(d.ticketLog(t.ID), t)
		remaining, lookupErr := worktree.FindWorktreeForBranch(expandTilde(t.Path), t.Branch)
		if lookupErr == nil && remaining == "" {
			_ = os.Remove(path)
		}
	}
}
