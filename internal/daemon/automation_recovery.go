package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/tmux"
)

func (d *Daemon) automationWindow(run automation.Run) (tmux.WindowIdentity, error) {
	b, err := readAutomationArtifact(filepath.Join(automationRunDir(d.config(), run), "terminal.json"))
	if err != nil {
		return tmux.WindowIdentity{}, err
	}
	var identity tmux.WindowIdentity
	if err := json.Unmarshal(b, &identity); err != nil {
		return identity, err
	}
	if run.WindowID == "" || identity.WindowID != run.WindowID {
		return identity, errors.New("run window identity is missing or mismatched")
	}
	return identity, nil
}

func sameAutomationExecution(a, b automation.Run) bool {
	return a.AutomationID == b.AutomationID && a.N == b.N && a.InvocationID == b.InvocationID
}

func sameAutomationTarget(a, b automation.Run) bool {
	return sameAutomationExecution(a, b) && a.Key == b.Key && a.WindowID == b.WindowID && a.Session == b.Session && a.Window == b.Window
}

func automationNeedsRecovery(run automation.Run) bool {
	return run.Phase != "queued" && run.Phase != "terminal"
}

// The prefix cannot collide with a ticket ID. Reuse the dispatcher's recovery
// accounting so executions beyond channel capacity also block new admissions.
func automationRecoveryKey(run automation.Run) string {
	return fmt.Sprintf("automation-recovery:%s:%d:%s", run.AutomationID, run.N, run.InvocationID)
}

// Caller holds automationMu. Each physical invocation owns its reservation;
// a stale recovery must not consume the token of its successor.
func (d *Daemon) reserveAutomationRecoveryLocked(run automation.Run) *automationExecution {
	e := d.automationExecutions[run.AutomationID]
	if e == nil {
		ctx, cancel := context.WithCancel(context.Background())
		e = &automationExecution{run: run, ctx: ctx, cancel: cancel}
		if d.automationExecutions == nil {
			d.automationExecutions = make(map[string]*automationExecution)
		}
		d.automationExecutions[run.AutomationID] = e
	} else if !sameAutomationExecution(e.run, run) {
		e = nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := automationRecoveryKey(run)
	if _, exists := d.recoveredReservations[key]; exists {
		return e
	}
	reservation := recoveredReservation{pool: d.sem, token: e != nil && e.reserved}
	if !reservation.token {
		select {
		case d.sem <- struct{}{}:
			reservation.token = true
		default:
		}
	}
	if e != nil {
		e.reserved = reservation.token
	}
	if d.recoveredReservations == nil {
		d.recoveredReservations = make(map[string]recoveredReservation)
	}
	d.recoveredReservations[key] = reservation
	return e
}

func (d *Daemon) refreshAutomationRecoveryBarrierLocked() {
	d.automationRecoveryBlocked = false
	remaining, err := d.automationStore.UnfinishedRuns()
	if err != nil {
		d.automationRecoveryBlocked = true
		return
	}
	for _, other := range remaining {
		if other.Phase != "queued" && other.Spec.Workspace == "repo" {
			if err := d.retainAutomationRepoLock(other); err != nil {
				d.automationRecoveryBlocked = true
			}
		}
	}
}

func (d *Daemon) recoverAutomationRuns() {
	d.automationMu.Lock()
	if d.automationAvailableLocked() != nil {
		d.automationMu.Unlock()
		return
	}
	runs, err := d.automationStore.UnfinishedRuns()
	if err != nil {
		d.log.Error("reading automation recovery", "err", err)
		d.automationMu.Unlock()
		return
	}
	var interrupted []automation.Run
	for _, run := range runs {
		if run.Phase == "queued" {
			// A live definition keeps its frozen queued snapshot. A retired
			// definition has no scheduler that could safely dispatch this run.
			if _, err := d.automationStore.Get(run.AutomationID); errors.Is(err, automation.ErrNotFound) {
				if _, err := d.skipAutomationLocked(run, "automation retired before launch"); err != nil {
					d.log.Error("skipping retired automation", "err", err)
				}
			}
			continue
		}
		if run.Phase == "stopping" && run.StartedAt.IsZero() && run.WindowID == "" {
			if _, err := d.skipAutomationLocked(run, run.StopReason); err != nil {
				d.log.Error("saving unlaunched stop", "err", err)
			}
			continue
		}
		if err := d.retainAutomationRepoLock(run); err != nil {
			d.automationRecoveryBlocked = true
			d.log.Error("automation repository recovery blocks admission", "err", err)
		}
		d.reserveAutomationRecoveryLocked(run)
		interrupted = append(interrupted, run)
	}
	d.automationMu.Unlock()
	for _, run := range interrupted {
		d.recoverAutomationRun(run)
	}
	d.automationChanged()
}

func (d *Daemon) recoverAutomationRun(snapshot automation.Run) {
	d.automationMu.Lock()
	if d.automationAvailableLocked() != nil {
		d.automationMu.Unlock()
		return
	}
	run, err := d.automationStore.GetRun(snapshot.AutomationID, snapshot.N)
	if err != nil || !sameAutomationExecution(snapshot, run) || !automationNeedsRecovery(run) {
		d.automationMu.Unlock()
		return
	}
	execution := d.reserveAutomationRecoveryLocked(run)
	d.automationMu.Unlock()

	identity, terminationErr := d.automationWindow(run)
	if run.Termination == automationHookPending {
		terminationErr = errors.New("hook termination is unconfirmed; inspect hook processes on the daemon host")
	}
	if terminationErr == nil {
		var present bool
		present, terminationErr = tmux.InspectWindowIdentity(identity)
		if terminationErr == nil && present {
			terminationErr = tmux.KillWindowIdentity(identity)
		}
		if terminationErr == nil {
			present, terminationErr = tmux.InspectWindowIdentity(identity)
			if terminationErr == nil && present {
				terminationErr = errors.New("owned window still exists")
			}
		}
	}

	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if d.automationAvailableLocked() != nil {
		return
	}
	if terminationErr == nil {
		confirmed, err := d.automationWindow(run)
		if err != nil || confirmed != identity {
			terminationErr = errors.New("run terminal identity changed during recovery")
		}
	}
	finished := false
	_, err = d.automationStore.UpdateRun(run.AutomationID, run.N, func(current *automation.Run) error {
		if !sameAutomationTarget(run, *current) || !automationNeedsRecovery(*current) {
			return automation.ErrConflict
		}
		if current.Termination == automationHookPending {
			terminationErr = errors.New("hook termination is unconfirmed; inspect hook processes on the daemon host")
		}
		if terminationErr == nil {
			current.Phase, current.Outcome, current.CompletedAt = "terminal", "failed", time.Now()
			current.Result = "interrupted by daemon restart"
			if current.StopReason != "" {
				current.Result = current.StopReason
				current.ExitCode = new(143)
			}
			current.Termination = "tmux_window_gone"
			finished = true
		} else {
			current.Phase = "recovery_required"
			current.Result = "termination recovery: " + terminationErr.Error()
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, automation.ErrConflict) {
			d.log.Error("saving automation recovery", "err", err)
		}
		return
	}
	if finished {
		d.mu.Lock()
		key := automationRecoveryKey(run)
		if reservation, exists := d.recoveredReservations[key]; exists {
			delete(d.recoveredReservations, key)
			if reservation.token {
				<-reservation.pool
			}
		}
		d.queueCond.Broadcast()
		d.mu.Unlock()
		if execution != nil && d.automationExecutions[run.AutomationID] == execution && sameAutomationExecution(execution.run, run) {
			execution.cancel()
			execution.reserved = false
			delete(d.automationExecutions, run.AutomationID)
		}
		d.releaseAutomationRepoLock(run)
		d.refreshAutomationRecoveryBarrierLocked()
		if err := d.automationEvent(run, "warning", fmt.Sprintf("run #%d interrupted; owned window confirmed absent", run.N)); err != nil {
			d.log.Warn("writing recovery log", "err", err)
		}
	}
	d.automationChanged()
}
