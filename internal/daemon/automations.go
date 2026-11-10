package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

const automationLateness = 5 * time.Minute

func WithAutomationRoot(root string) Option {
	return func(d *Daemon) { d.automationRoot = root }
}

func (d *Daemon) startAutomations(primary string) func() {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	d.automationStore, d.automationErr = automation.Open(d.automationRoot, primary)
	if d.automationErr != nil {
		d.log.Error("automations unavailable", "err", d.automationErr)
	} else if err := d.reconcileAutomationsLocked(time.Now()); err != nil {
		d.log.Error("reconciling automations", "err", err)
	}
	return func() {
		d.automationMu.Lock()
		defer d.automationMu.Unlock()
		if d.automationStore != nil {
			_ = d.automationStore.Close()
			d.automationStore = nil
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
	return d.automationStore.List()
}

func (d *Daemon) GetAutomation(id string) (automation.Definition, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return automation.Definition{}, err
	}
	return d.automationStore.Get(id)
}

func (d *Daemon) validateAutomation(input automation.Definition) error {
	if input.Agent == config.NoneSentinel {
		return fmt.Errorf("%w: choose a configured agent", automation.ErrValidation)
	}
	if _, ok := d.config().Agents[input.Agent]; !ok {
		return fmt.Errorf("%w: unknown agent %q", automation.ErrValidation, input.Agent)
	}
	if err := cli.CheckRepo(input.Path, ""); err != nil {
		return fmt.Errorf("%w: %v", automation.ErrValidation, err)
	}
	return nil
}

func (d *Daemon) CreateAutomation(requestID string, input automation.Definition) (automation.Definition, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return automation.Definition{}, err
	}
	release, err := d.ordinaryMutationAdmission("")
	if err != nil {
		return automation.Definition{}, err
	}
	defer release()
	if saved, found, err := d.automationStore.CreateReceipt(requestID, input); err != nil || found {
		return saved, err
	}
	if err := d.validateAutomation(input); err != nil {
		return automation.Definition{}, err
	}
	saved, err := d.automationStore.Create(requestID, input, time.Now())
	if err == nil {
		d.automationChanged()
	}
	return saved, err
}

func (d *Daemon) UpdateAutomation(input automation.Definition) (automation.Definition, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return automation.Definition{}, err
	}
	release, err := d.ordinaryMutationAdmission("")
	if err != nil {
		return automation.Definition{}, err
	}
	defer release()
	// Pausing remains possible after an agent or repository is removed.
	if input.Enabled {
		if err := d.validateAutomation(input); err != nil {
			return automation.Definition{}, err
		}
	}
	saved, err := d.automationStore.Update(input, time.Now())
	if err == nil {
		d.automationChanged()
	}
	return saved, err
}

func (d *Daemon) AutomationOccurrences(id string) ([]automation.Occurrence, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return nil, err
	}
	return d.automationStore.Occurrences(id)
}

func (d *Daemon) SkipAutomationOccurrence(id, occurrenceID string, revision uint64) (automation.Occurrence, error) {
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return automation.Occurrence{}, err
	}
	release, err := d.ordinaryMutationAdmission("")
	if err != nil {
		return automation.Occurrence{}, err
	}
	defer release()
	records, err := d.automationStore.Occurrences(id)
	if err != nil {
		return automation.Occurrence{}, err
	}
	for _, o := range records {
		if o.ID != occurrenceID {
			continue
		}
		if o.Revision != revision {
			return automation.Occurrence{}, automation.ErrConflict
		}
		if o.Phase != "blocked" {
			return automation.Occurrence{}, fmt.Errorf("%w: only unresolved occurrences can be acknowledged", automation.ErrValidation)
		}
		def, err := d.automationStore.Get(id)
		if err != nil {
			return automation.Occurrence{}, err
		}
		next, err := def.Schedule.Next(time.Now())
		if err != nil {
			return automation.Occurrence{}, err
		}
		if next.Before(def.Next) {
			next = def.Next
		}
		// Persist the future cursor while the occurrence still blocks admission.
		if err := d.automationStore.SetNext(id, next); err != nil {
			return automation.Occurrence{}, err
		}
		o.Phase, o.Reason = "skipped", "unresolved publication acknowledged by operator; no replacement ticket"
		saved, err := d.saveAutomationOccurrence(o)
		if err != nil {
			return automation.Occurrence{}, err
		}
		d.automationChanged()
		return saved, nil
	}
	return automation.Occurrence{}, automation.ErrNotFound
}

func (d *Daemon) saveAutomationOccurrence(o automation.Occurrence) (automation.Occurrence, error) {
	saved, err := d.automationStore.SaveOccurrence(o)
	if err == nil {
		d.log.Info("automation occurrence", "automation", o.AutomationID, "occurrence", o.ID, "disposition", o.Phase)
		d.automationChanged()
	}
	return saved, err
}

func (d *Daemon) automationLoop(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	backoff := scheduleRetryInitial
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		d.automationMu.Lock()
		wait := time.Minute
		if d.automationAvailableLocked() == nil {
			if err := d.reconcileAutomationsLocked(time.Now()); err != nil {
				d.log.Error("reconciling automations", "err", err)
				wait = backoff
				backoff = min(backoff*2, scheduleRetryMax)
			} else {
				backoff = scheduleRetryInitial
				wait = d.automationWaitLocked(time.Now())
			}
		}
		// Consume durable-change wakes before an edit can take the mutex.
		select {
		case <-d.automationWake:
		default:
		}
		d.automationMu.Unlock()
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-d.automationWake:
		}
	}
}

func (d *Daemon) automationWaitLocked(now time.Time) time.Duration {
	wait := time.Minute
	defs, err := d.automationStore.List()
	if err != nil {
		return 5 * time.Second
	}
	for _, def := range defs {
		records, err := d.automationStore.Occurrences(def.ID)
		if err != nil {
			return 5 * time.Second
		}
		blocked := false
		for _, o := range records {
			blocked = blocked || o.Phase == "blocked"
			if o.TicketID != "" && (o.Phase == "admitted" || o.Phase == "skipped") && d.automationTicketOutstanding(o) && wait > 5*time.Second {
				wait = 5 * time.Second
			}
		}
		if def.Enabled && !blocked && !def.Next.IsZero() {
			if delta := def.Next.Sub(now); delta < wait {
				wait = delta
			}
		}
	}
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

func automationOccurrenceID(def automation.Definition, at time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s", def.ID, def.ScheduleRevision, at.UTC().Format(time.RFC3339))))
	return fmt.Sprintf("%x", sum[:16])
}

func (d *Daemon) reconcileAutomationsLocked(now time.Time) error {
	defs, err := d.automationStore.List()
	if err != nil {
		return err
	}
	for _, def := range defs {
		if err := d.reconcileAutomationLocked(def, now); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) reconcileAutomationLocked(def automation.Definition, now time.Time) error {
	records, err := d.automationStore.Occurrences(def.ID)
	if err != nil {
		return err
	}
	blocked := false
	for i, o := range records {
		switch o.Phase {
		case "reserved", "publishing":
			o, err = d.recoverAutomationOccurrence(def, o, now)
			if err != nil {
				return err
			}
			records[i] = o
		}
		if o.Phase == "blocked" {
			blocked = true
		}
		if o.Phase == "admitted" {
			d.admitAutomationTicket(o)
		}
		// A crash after recording the disposition but before the cursor save
		// must not reconsider that slot.
		if o.ScheduleRevision == def.ScheduleRevision && o.ScheduledFor.Equal(def.Next) && o.Phase != "reserved" && o.Phase != "publishing" && o.Phase != "blocked" {
			through := o.ScheduledFor
			if !o.Through.IsZero() {
				through = o.Through
			}
			def.Next, err = def.Schedule.Next(through)
			if err != nil {
				return err
			}
			if err := d.automationStore.SetNext(def.ID, def.Next); err != nil {
				return err
			}
		}
	}
	if blocked || !def.Enabled || def.Next.IsZero() || def.Next.After(now) {
		return nil
	}
	latest, count, err := def.Schedule.LatestDue(def.Next, now)
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if count > 1 {
		through, missed, err := def.Schedule.LatestDue(def.Next, latest.Add(-time.Nanosecond))
		if err != nil {
			return err
		}
		o := automation.Occurrence{ID: automationOccurrenceID(def, def.Next), AutomationID: def.ID, ScheduleRevision: def.ScheduleRevision, ScheduledFor: def.Next, Through: through, Count: missed, Phase: "skipped", Reason: "missed while scheduler was unavailable"}
		if _, err := d.saveAutomationOccurrence(o); err != nil {
			return err
		}
		if err := d.automationStore.SetNext(def.ID, latest); err != nil {
			return err
		}
		def.Next = latest
	}
	o := automation.Occurrence{ID: automationOccurrenceID(def, latest), AutomationID: def.ID, ScheduleRevision: def.ScheduleRevision, ScheduledFor: latest, Count: 1, Payload: def}
	if now.Sub(latest) > automationLateness {
		o.Phase, o.Reason = "skipped", "outside five-minute lateness window"
	} else if err := d.validateAutomation(def); err != nil {
		o.Phase, o.Reason = "failed", err.Error()
	} else if def.SkipOverlap && d.automationOverlap(records) {
		o.Phase, o.Reason = "skipped", "earlier automation ticket remains unfinished or execution-reserved"
	} else {
		o.TicketID, err = cli.GenerateID(d.config(), def.Path)
		if err != nil {
			o.Phase, o.Reason = "failed", fmt.Sprintf("generating ticket ID: %v", err)
		} else {
			o.Phase = "reserved"
		}
	}
	o, err = d.saveAutomationOccurrence(o)
	if err != nil {
		return err
	}
	if o.Phase == "reserved" {
		o, err = d.recoverAutomationOccurrence(def, o, now)
		if err != nil {
			return err
		}
	}
	if o.Phase == "blocked" {
		return nil
	}
	next, err := def.Schedule.Next(latest)
	if err != nil {
		return err
	}
	if err := d.automationStore.SetNext(def.ID, next); err != nil {
		return err
	}
	d.automationChanged()
	if o.Phase == "admitted" {
		d.admitAutomationTicket(o)
	}
	return nil
}

func automationMatches(t *ticket.Ticket, o automation.Occurrence) bool {
	return t.ID == o.TicketID && t.AutomationID == o.AutomationID && t.OccurrenceID == o.ID && t.ScheduledFor == o.ScheduledFor.UTC().Format(time.RFC3339)
}

func (d *Daemon) automationTicketPath(o automation.Occurrence) string {
	return filepath.Join(config.ExpandTilde(d.config().TicketsDir), o.TicketID+".md")
}

func (d *Daemon) recoverAutomationOccurrence(def automation.Definition, o automation.Occurrence, now time.Time) (automation.Occurrence, error) {
	if o.Phase == "reserved" {
		if def.Enabled && def.ScheduleRevision == o.ScheduleRevision && !o.ScheduledFor.Before(def.Next) && now.Before(o.ScheduledFor) {
			return o, nil
		}
		records, err := d.automationStore.Occurrences(def.ID)
		if err != nil {
			return o, err
		}
		reason := ""
		if !def.Enabled || def.ScheduleRevision != o.ScheduleRevision {
			reason = "definition paused or rescheduled before publication"
		} else if o.ScheduledFor.Before(def.Next) {
			reason = "schedule cursor advanced before publication"
		} else if now.Sub(o.ScheduledFor) > automationLateness {
			reason = "reservation expired before publication"
		} else if err := d.validateAutomation(o.Payload); err != nil {
			reason = err.Error()
		} else if def.SkipOverlap && d.automationOverlap(records) {
			reason = "earlier automation ticket remains unfinished or execution-reserved"
		}
		if reason != "" {
			o.Phase, o.Reason = "skipped", reason
			return d.saveAutomationOccurrence(o)
		}
		o.Phase = "publishing"
		o, err = d.saveAutomationOccurrence(o)
		if err != nil {
			return o, err
		}
		_, publishErr := d.createTicket(web.CreateTicketRequest{Title: o.Payload.Name, Path: o.Payload.Path, Agent: o.Payload.Agent, Pipeline: config.NoneSentinel, Status: "todo", Body: o.Payload.Instructions}, &o)
		if publishErr != nil {
			d.log.Error("automation ticket publication uncertain", "occurrence", o.ID, "ticket", o.TicketID, "err", publishErr)
		}
	}
	if o.Phase != "publishing" {
		return o, nil
	}
	path := d.automationTicketPath(o)
	t, err := ticket.ParseFile(path)
	if err != nil {
		o.Phase, o.Reason = "blocked", "publication started but matching ticket cannot be read; acknowledge without retrying"
	} else if !automationMatches(t, o) {
		o.Phase, o.Reason = "blocked", "reserved ticket ID has different provenance"
	} else if err := syncAutomationTicket(path); err != nil {
		o.Phase, o.Reason = "blocked", "cannot confirm ticket publication durability"
	} else {
		o.Phase, o.Reason = "admitted", ""
	}
	return d.saveAutomationOccurrence(o)
}

func syncAutomationTicket(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (d *Daemon) automationOverlap(records []automation.Occurrence) bool {
	for _, o := range records {
		if o.Phase == "publishing" || o.Phase == "blocked" {
			return true
		}
		if o.TicketID != "" && (o.Phase == "admitted" || o.Phase == "skipped") && d.automationTicketOutstanding(o) {
			return true
		}
	}
	return false
}

func (d *Daemon) automationTicketOutstanding(o automation.Occurrence) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.running[o.TicketID]; ok {
		return true
	}
	if _, ok := d.executions[o.TicketID]; ok {
		return true
	}
	if _, ok := d.recoveredReservations[o.TicketID]; ok {
		return true
	}
	t, err := ticket.ParseFile(d.automationTicketPath(o))
	if err != nil {
		return o.Phase != "skipped" || !errors.Is(err, os.ErrNotExist)
	}
	if !automationMatches(t, o) {
		return o.Phase != "skipped"
	}
	if t.HasOwnershipMetadata() {
		return true
	}
	switch t.Status {
	case ticket.StatusDone, ticket.StatusCancelled, ticket.StatusArchived, ticket.Status("closed"):
		return false
	default:
		return true
	}
}

func (d *Daemon) admitAutomationTicket(o automation.Occurrence) {
	release, err := d.ordinaryMutationAdmission(o.TicketID)
	if err != nil {
		return
	}
	defer release()
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.running[o.TicketID]; ok {
		return
	}
	if _, ok := d.executions[o.TicketID]; ok {
		return
	}
	if _, ok := d.recoveredReservations[o.TicketID]; ok {
		return
	}
	path := d.automationTicketPath(o)
	t, err := ticket.ParseFile(path)
	if err != nil || !automationMatches(t, o) || t.Status != ticket.StatusTodo {
		return
	}
	if t.ClaimedBy != "" && t.ClaimedBy != d.instanceName {
		return
	}
	d.tickets[t.ID] = newTicketState(t, path)
	d.enqueue(t)
}

var _ web.AutomationService = (*Daemon)(nil)
