package daemon

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/web"
)

func (d *Daemon) AutomationRunningAgents() int { return int(d.automationActive.Load()) }

func (d *Daemon) AutomationTerminal(id string, n uint64) (web.AutomationTerminalTarget, error) {
	d.automationMu.Lock()
	if err := d.automationAvailableLocked(); err != nil {
		d.automationMu.Unlock()
		return web.AutomationTerminalTarget{}, err
	}
	run, err := d.automationStore.GetRun(id, n)
	d.automationMu.Unlock()
	if err != nil {
		return web.AutomationTerminalTarget{}, err
	}
	if !automationHasTerminal(run) {
		return web.AutomationTerminalTarget{}, fmt.Errorf("%w: run has no live terminal", automation.ErrNotFound)
	}
	identity, err := d.automationWindow(run)
	if err != nil {
		return web.AutomationTerminalTarget{}, fmt.Errorf("%w: %v", automation.ErrUnavailable, err)
	}
	present, err := tmux.InspectWindowIdentity(identity)
	if err != nil {
		return web.AutomationTerminalTarget{}, fmt.Errorf("%w: %v", automation.ErrUnavailable, err)
	}
	if !present {
		return web.AutomationTerminalTarget{}, automation.ErrNotFound
	}
	// Inspection runs outside automationMu. A stop or replacement can finish
	// while tmux answers, so resolve the durable run and artifact again.
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	if err := d.automationAvailableLocked(); err != nil {
		return web.AutomationTerminalTarget{}, err
	}
	current, err := d.automationStore.GetRun(id, n)
	if err != nil {
		return web.AutomationTerminalTarget{}, err
	}
	if !automationHasTerminal(current) || !sameAutomationTarget(run, current) {
		return web.AutomationTerminalTarget{}, automation.ErrNotFound
	}
	confirmed, err := d.automationWindow(current)
	if err != nil || confirmed != identity {
		return web.AutomationTerminalTarget{}, fmt.Errorf("%w: run terminal identity changed", automation.ErrUnavailable)
	}
	return web.AutomationTerminalTarget{Session: run.Session, Window: run.Window, Identity: identity}, nil
}

func automationHasTerminal(run automation.Run) bool {
	return run.Phase == "running" || run.Phase == "stopping"
}
