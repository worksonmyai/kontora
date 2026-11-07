package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/worktree"
)

var errExecutionChanged = errors.New("ticket execution changed")

type executionIdentity struct {
	id, pipeline, stage, path, branch, base, owner, epic, parent, invocation string
	kind                                                                     ticket.Kind
	annotation                                                               ticket.Status
	attempt                                                                  int
	started                                                                  string
	kontora                                                                  bool
}

func executionOf(t *ticket.Ticket) executionIdentity {
	return executionIdentity{
		id: t.ID, pipeline: t.Pipeline, stage: t.Stage, path: t.Path,
		branch: t.Branch, base: t.BaseBranch, owner: t.ClaimedBy,
		epic: t.EpicExecution, parent: t.Parent, kind: t.Kind,
		annotation: t.AnnotationReturnStatus, attempt: t.Attempt, invocation: t.InvocationID,
		started: derefTime(t.StartedAt).UTC().Format(time.RFC3339Nano), kontora: t.Kontora,
	}
}

// The pointer is a daemon-local token, not a durable invocation identifier.
type executionClaim struct {
	expected executionIdentity
	snapshot *ticket.Ticket
}
type executionContextKey struct{}

func executionFrom(ctx context.Context) *executionClaim {
	claim, _ := ctx.Value(executionContextKey{}).(*executionClaim)
	return claim
}

func (d *Daemon) executionMatchesLocked(ctx context.Context, snapshot, current *ticket.Ticket) bool {
	expected := executionOf(snapshot)
	if claim := executionFrom(ctx); claim != nil {
		if d.executions[current.ID] != claim {
			return false
		}
		expected = claim.expected
	}
	return !current.HasOwnershipMetadata() && current.Status == ticket.StatusInProgress && executionOf(current) == expected
}

func (d *Daemon) patchExecution(ctx context.Context, snapshot *ticket.Ticket, filePath string, patch func(*ticket.Ticket) error) (*ticket.Ticket, error) {
	var saved *ticket.Ticket
	err := d.withTicketMutationOrigin(notify.OriginDaemon, func(repo app.MutationRepository) error {
		st, err := repo.Get(snapshot.ID)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || st.FilePath != filePath || !d.executionMatchesLocked(ctx, snapshot, st.Ticket) {
			return errExecutionChanged
		}
		if err := patch(st.Ticket); err != nil {
			return err
		}
		if err := repo.Save(st); err != nil {
			return err
		}
		saved = st.Ticket.Clone()
		return nil
	})
	if err == nil {
		d.broadcastTicketUpdateLocking(snapshot.ID)
	}
	return saved, err
}

func clearExecutionSummaries(current, snapshot *ticket.Ticket, final bool) {
	if current.Summary == snapshot.Summary {
		_ = current.SetField("summary", "")
	}
	if final && current.FinalSummary == snapshot.FinalSummary {
		_ = current.SetField("final_summary", "")
	}
}

func (d *Daemon) pauseExecution(ctx context.Context, snapshot *ticket.Ticket, filePath, reason string) {
	_, err := d.patchExecution(ctx, snapshot, filePath, func(t *ticket.Ticket) error {
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", reason)
		appendSystemNote(t, reason)
		return nil
	})
	if err != nil && !errors.Is(err, errExecutionChanged) {
		d.ticketLog(snapshot.ID).Error("pause execution", "err", err)
	}
}

// Errors wrapping process.ErrTerminationUncertain retain execution reservations.
type ExecutionPreparer func(context.Context, string, string) error

func WithExecutionPreparer(fn ExecutionPreparer) Option {
	return func(d *Daemon) { d.executionPreparer = fn; d.executionPreparerExplicit = true }
}

func (d *Daemon) executionCurrent(ctx context.Context, snapshot *ticket.Ticket, filePath string) bool {
	err := d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := repo.Get(snapshot.ID)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || st.FilePath != filePath || !d.executionMatchesLocked(ctx, snapshot, st.Ticket) {
			return errExecutionChanged
		}
		return nil
	})
	return err == nil
}

func (d *Daemon) prepareExecution(ctx context.Context, snapshot *ticket.Ticket, filePath string) bool {
	if !d.executionCurrent(ctx, snapshot, filePath) {
		return false
	}
	if d.executionPreparer != nil {
		if err := d.executionPreparer(ctx, d.tmuxSession, snapshot.ID); err != nil {
			d.pauseExecution(ctx, snapshot, filePath, "replacement cleanup failed; inspect the prior tmux window: "+err.Error())
			if errors.Is(err, process.ErrTerminationUncertain) {
				retainOrdinaryReservation(ctx)
			}
			return false
		}
	}
	return d.executionCurrent(ctx, snapshot, filePath)
}

func (d *Daemon) initializeExecution(cfg *config.Config, t *ticket.Ticket, now time.Time) error {
	if t.AnnotationReturnStatus == "" {
		_ = t.SetField("branch", effectiveClaimBranch(cfg, t))
		if t.IsAgentEpic() {
			_ = t.SetField("completed_at", nil)
		}
	}
	if t.AnnotationReturnStatus == "" && !t.IsAgentEpic() && t.Pipeline != "" {
		stages, exists := cfg.Pipelines[t.Pipeline]
		if !exists || len(stages) == 0 {
			return fmt.Errorf("unknown pipeline %q", t.Pipeline)
		}
		if t.Stage == "" {
			_ = t.SetField("stage", stages[0].Stage)
		}
		builtinRework := cfg.ReworkIsBuiltin && t.Stage == config.ReworkStageName && !stageInPipeline(stages, t.Stage)
		if !builtinRework {
			action, err := pipeline.Evaluate(t, stages, pipeline.Event{Kind: pipeline.EventPickedUp, Timestamp: now})
			if err != nil {
				return err
			}
			if err := d.applyAction(t, action); err != nil {
				return err
			}
		}
	}
	_ = t.SetField("status", string(ticket.StatusInProgress))
	_ = t.SetField("started_at", now.Format(time.RFC3339Nano))
	return t.SetField("claimed_by", d.instanceName)
}

func effectiveClaimBranch(cfg *config.Config, t *ticket.Ticket) string {
	if cfg.BranchNamingFor(t.Path).Mode == config.BranchNamingModeSlug && t.Branch == "" {
		if branch, generated := generatedTicketBranch(cfg, t); generated {
			return branch
		}
	}
	return ticketBranch(cfg, t)
}

func (d *Daemon) claimExecution(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) (context.Context, context.CancelFunc, *ticket.Ticket, string, string, error) {
	d.mu.Lock()
	ts, ok := d.claimableLocked(id, log)
	if !ok {
		d.mu.Unlock()
		return nil, nil, nil, "", "", errExecutionChanged
	}
	before := ts.ticket.Clone()
	d.mu.Unlock()
	var identity string
	var err error
	if before.AnnotationReturnStatus == "" {
		identity, err = worktree.CommonDir(expandTilde(before.Path))
		if err != nil {
			d.pauseTicket(before, ts.filePath, "resolve repository identity: "+err.Error())
			return nil, nil, nil, "", "", err
		}
	}
	taskCtx, cancel := context.WithCancel(ctx)
	var snapshot *ticket.Ticket
	var filePath, branchKey string
	var claim *executionClaim
	err = d.withTicketMutationOrigin(notify.OriginDaemon, func(repo app.MutationRepository) error {
		st, err := repo.Get(id)
		if err != nil {
			return err
		}
		t := st.Ticket
		if ctx.Err() != nil || !t.Kontora || t.Status != ticket.StatusTodo || t.IsPassiveEpic() || t.HasOwnershipMetadata() {
			return errExecutionChanged
		}
		if t.Path != before.Path || t.Branch != before.Branch || t.AnnotationReturnStatus != before.AnnotationReturnStatus {
			return errExecutionChanged
		}
		if _, running := d.running[id]; running {
			return errExecutionChanged
		}
		if _, reviewing := d.plannotator[id]; reviewing {
			d.plannotatorDeferred[id] = struct{}{}
			return errExecutionChanged
		}
		if !dispatchPoolMatches(ctx, t) {
			d.setTicketState(id, t.Clone(), st.FilePath)
			d.enqueue(t)
			return errExecutionChanged
		}
		for _, dep := range t.Deps {
			if dep == "" {
				continue
			}
			other, err := repo.Get(dep)
			if err != nil || !ticket.IsDependencyResolved(other.Ticket.Status) {
				return errExecutionChanged
			}
		}
		if err := d.initializeExecution(cfg, t, time.Now()); err != nil {
			return err
		}
		if t.AnnotationReturnStatus == "" {
			branchKey = identity + "\x00" + t.Branch
			if holder, exists := d.branchHolderLocked(branchKey); exists {
				return fmt.Errorf("branch %s in use by ticket %s", t.Branch, holder)
			}
		}
		claim = &executionClaim{expected: executionOf(t)}
		d.running[id] = cancel
		d.executions[id] = claim
		if branchKey != "" {
			d.runningBranches[branchKey] = id
		}
		if err := repo.Save(st); err != nil {
			delete(d.running, id)
			delete(d.executions, id)
			if branchKey != "" {
				delete(d.runningBranches, branchKey)
			}
			return err
		}
		snapshot = t.Clone()
		claim.snapshot = snapshot
		filePath = st.FilePath
		return nil
	})
	if err != nil {
		cancel()
		if !errors.Is(err, errExecutionChanged) {
			d.pauseTicket(before, ts.filePath, err.Error())
		}
		return nil, nil, nil, "", "", err
	}
	taskCtx = context.WithValue(taskCtx, executionContextKey{}, claim)
	d.broadcastTicketUpdateLocking(id)
	return taskCtx, cancel, snapshot, filePath, branchKey, nil
}
