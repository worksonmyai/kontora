package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

type automationExecution struct {
	run      automation.Run
	ctx      context.Context
	cancel   context.CancelFunc
	reserved bool
}

type automationDispatchJob struct {
	key string
	run func(context.Context) bool
}

func WithAutomationRoot(root string) Option { return func(d *Daemon) { d.automationRoot = root } }

func (d *Daemon) startAutomations(primary string) func() {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	scope := primary
	if d.configPath != "" {
		scope = d.configPath
		if resolved, err := filepath.EvalSymlinks(scope); err == nil {
			scope = resolved
		}
		if absolute, err := filepath.Abs(scope); err == nil {
			scope = absolute
		}
	}
	d.automationExecutions = map[string]*automationExecution{}
	d.automationStore, d.automationErr = automation.Open(d.automationRoot, scope)
	if d.automationErr == nil {
		d.automationErr = d.lockAutomationLogs()
	}
	if d.automationErr == nil {
		d.automationErr = d.automationStore.Reconcile(d.config().Automations, time.Now())
	}
	if d.automationErr != nil {
		d.log.Error("automations unavailable", "err", d.automationErr)
	}
	return func() {
		d.automationMu.Lock()
		defer d.automationMu.Unlock()
		for _, execution := range d.automationExecutions {
			execution.cancel()
		}
		if d.automationStore != nil {
			_ = d.automationStore.Close()
			d.automationStore = nil
		}
		if d.automationLogLock != nil {
			_ = d.automationLogLock.Close()
			d.automationLogLock = nil
		}
	}
}

func (d *Daemon) automationAvailableLocked() error {
	if d.automationErr != nil {
		return fmt.Errorf("%w: %v", automation.ErrUnavailable, d.automationErr)
	}
	if d.automationStore == nil {
		return automation.ErrUnavailable
	}
	return nil
}

func (d *Daemon) signalAutomations() {
	select {
	case d.automationWake <- struct{}{}:
	default:
	}
}
func (d *Daemon) automationChanged() {
	if d.broker != nil {
		d.broker.Broadcast(web.TicketEvent{Type: "automations_updated"})
	}
	d.signalAutomations()
}

func (d *Daemon) ListAutomations() ([]automation.Definition, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return nil, err
	}
	rows, err := d.automationStore.List()
	for i := range rows {
		rows[i] = d.automationDecorate(rows[i])
	}
	return rows, err
}

func (d *Daemon) GetAutomation(id string) (automation.Definition, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return automation.Definition{}, err
	}
	row, err := d.automationStore.Get(id)
	if err != nil {
		return row, err
	}
	return d.automationDecorate(row), nil
}

func (d *Daemon) automationDecorate(row automation.Definition) automation.Definition {
	row.Project, _, _ = d.config().ProjectFor(row.Path)
	return row
}

func (d *Daemon) AutomationRuns(id string) ([]automation.Run, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return nil, err
	}
	return d.automationStore.Runs(id)
}

func (d *Daemon) RunAutomation(id, requestID string) (automation.Run, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return automation.Run{}, err
	}
	run, err := d.acceptAutomationLocked(id, "manual", requestID, time.Time{}, time.Time{})
	if err == nil {
		d.automationChanged()
	}
	return run, err
}

func (d *Daemon) acceptAutomationLocked(id, trigger, requestID string, scheduledFor, next time.Time, at ...time.Time) (automation.Run, error) {
	now := time.Now()
	if len(at) > 0 {
		now = at[0]
	}
	definition, err := d.automationStore.Get(id)
	if err != nil {
		run, _, receiptErr := d.automationStore.Allocate(id, trigger, requestID, scheduledFor, next, now)
		return run, receiptErr
	}
	runs, err := d.automationStore.Runs(id)
	if err != nil {
		return automation.Run{}, err
	}
	var occupied *automation.Run
	var pending []automation.Run
	for _, previous := range runs {
		if previous.Phase == "terminal" {
			continue
		}
		if execution := d.automationExecutions[id]; execution != nil && execution.run.N == previous.N || previous.Phase != "queued" {
			prior := previous
			occupied = &prior
		} else {
			pending = append(pending, previous)
		}
	}
	if occupied == nil && len(pending) > 0 {
		prior := pending[len(pending)-1]
		occupied = &prior
		pending = pending[:len(pending)-1]
	}
	decision := automation.Admission{}
	if trigger == "schedule" && now.Sub(scheduledFor) > time.Minute {
		decision.SkipReason = "scheduled slot missed during downtime"
	} else if occupied != nil {
		switch definition.Overlap {
		case "skip":
			decision.SkipReason = "an earlier run is unfinished"
		case "queue":
			if len(pending) > 0 {
				decision.SkipReason = "overlap queue is full"
			}
		case "replace":
			for _, previous := range pending {
				decision.Supersede = append(decision.Supersede, previous.N)
			}
			if occupied.Phase == "queued" {
				decision.StopQueued = append(decision.StopQueued, occupied.N)
			}
		}
	}
	run, replay, err := d.automationStore.Allocate(id, trigger, requestID, scheduledFor, next, now, decision)
	if err != nil || replay || run.Phase == "terminal" {
		return run, err
	}
	if run, err = d.freezeAutomationRun(run, d.config()); err != nil {
		return run, err
	}
	if occupied == nil {
		d.queueAutomationLocked(run)
		return run, nil
	}
	if run.Spec.Overlap == "replace" {
		if _, err := d.stopAutomationLocked(id, occupied.N, "replaced by a newer run"); err != nil {
			return run, err
		}
	}
	return run, nil
}

func (d *Daemon) skipAutomationLocked(run automation.Run, reason string) (automation.Run, error) {
	return d.automationStore.UpdateRun(run.AutomationID, run.N, func(current *automation.Run) error {
		if current.Phase == "terminal" {
			return nil
		}
		current.Phase, current.Outcome, current.Result, current.CompletedAt = "terminal", "skipped", reason, time.Now()
		return nil
	})
}

func (d *Daemon) StopAutomation(id string, n uint64) (automation.Run, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return automation.Run{}, err
	}
	run, err := d.stopAutomationLocked(id, n, "stopped by user")
	if err == nil {
		d.automationChanged()
	}
	return run, err
}

func (d *Daemon) stopAutomationLocked(id string, n uint64, reason string) (automation.Run, error) {
	run, err := d.automationStore.GetRun(id, n)
	if err != nil || run.Phase == "terminal" {
		return run, err
	}
	if run.Phase == "recovery_required" {
		run, err = d.automationStore.UpdateRun(id, n, func(r *automation.Run) error { r.Phase = "stopping"; r.StopReason = reason; return nil })
		if err == nil {
			d.background.Go(func() { d.recoverAutomationRun(run) })
		}
		return run, err
	}
	execution := d.automationExecutions[id]
	if execution != nil && execution.run.N == n {
		run, err = d.automationStore.UpdateRun(id, n, func(current *automation.Run) error {
			current.StopReason = reason
			current.Phase = "stopping"
			return nil
		})
		if err == nil {
			execution.cancel()
		}
		return run, err
	}
	if run.Phase == "queued" || (run.Phase == "stopping" && run.StartedAt.IsZero() && run.WindowID == "") {
		return d.skipAutomationLocked(run, reason)
	}
	return run, fmt.Errorf("%w: interrupted execution needs termination recovery", automation.ErrConflict)
}

func (d *Daemon) queueAutomationLocked(run automation.Run) {
	if d.automationRecoveryBlocked {
		return
	}
	if _, exists := d.automationExecutions[run.AutomationID]; exists {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	execution := &automationExecution{run: run, ctx: ctx, cancel: cancel}
	d.automationExecutions[run.AutomationID] = execution
	d.mu.Lock()
	d.automationJobs = append(d.automationJobs, &automationDispatchJob{key: run.InvocationID, run: func(parent context.Context) bool {
		stop := context.AfterFunc(parent, cancel)
		defer stop()
		d.automationMu.Lock()
		execution.reserved = true
		d.automationMu.Unlock()
		return d.executeAutomation(execution)
	}})
	d.queueCond.Broadcast()
	d.mu.Unlock()
}

func (d *Daemon) automationLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		d.automationMu.Lock()
		defer d.automationMu.Unlock()
		for _, e := range d.automationExecutions {
			e.cancel()
		}
	}()
	for {
		if err := d.automationTick(time.Now()); err != nil && !errors.Is(err, automation.ErrUnavailable) {
			d.log.Error("automation scheduling failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.automationWake:
		}
	}
}

func (d *Daemon) automationTick(now time.Time) error {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return err
	}
	rows, err := d.automationStore.List()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Enabled && !row.Next.IsZero() && !row.Next.After(now) {
			schedule, err := automation.Compile(row.Schedule, row.Timezone)
			if err != nil {
				return err
			}
			next := schedule.Next(now)
			if interval, err := time.ParseDuration(row.Schedule); err == nil && interval > 0 {
				steps := int64(now.Sub(row.Next)/interval) + 1
				next = row.Next.Add(time.Duration(steps * int64(interval)))
			}
			if next.IsZero() {
				continue
			}
			if _, err := d.acceptAutomationLocked(row.ID, "schedule", "schedule:"+row.Next.UTC().Format(time.RFC3339Nano), row.Next, next, now); err != nil {
				return err
			}
			d.automationChanged()
		}
		if _, occupied := d.automationExecutions[row.ID]; occupied {
			continue
		}
		runs, err := d.automationStore.Runs(row.ID)
		if err != nil {
			return err
		}
		blocked := false
		var queued []automation.Run
		for _, run := range runs {
			if run.Phase == "queued" {
				queued = append(queued, run)
			} else if run.Phase != "terminal" {
				blocked = true
			}
		}
		if !blocked && len(queued) > 0 {
			sort.Slice(queued, func(i, j int) bool { return queued[i].N < queued[j].N })
			d.queueAutomationLocked(queued[0])
		}
	}
	return nil
}

func (d *Daemon) publishAutomationConfig(next *config.Config) error {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if d.automationStore != nil && d.automationErr == nil {
		before, err := d.automationStore.List()
		if err != nil {
			return err
		}
		if err := d.automationStore.Reconcile(next.Automations, time.Now()); err != nil {
			return err
		}
		for _, row := range before {
			if _, exists := next.Automations[row.Key]; exists {
				continue
			}
			runs, err := d.automationStore.Runs(row.ID)
			if err != nil {
				return err
			}
			for _, run := range runs {
				if run.Phase == "terminal" {
					continue
				}
				if _, err := d.stopAutomationLocked(row.ID, run.N, "automation deleted"); err != nil {
					d.log.Warn("deleted automation still needs recovery", "automation", row.ID, "err", err)
				}
			}
		}
	}
	d.cfg.Store(next)
	return nil
}

func automationPrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func automationRunDir(cfg *config.Config, run automation.Run) string {
	if run.ResultPath != "" {
		return filepath.Dir(run.ResultPath)
	}
	return filepath.Join(config.ExpandTilde(cfg.LogsDir), "automations", run.Key, fmt.Sprint(run.N))
}
