// Dialogs share focus management. Dialogs, mobile sheets and detail share inert
// state. Closing one must not reactivate a background another still covers.
const dialogStates = [
  ['auth', 'needsAuth'],
  ['automation-delete', 'automationsDeleteOpen'],
  ['archive', 'archivePromptId'],
  ['settings', 'settingsGuard'],
  ['palette', 'paletteOpen'],
  ['note-delete', 'noteDeleteTarget'],
  ['delete', 'deleteModal'],
  ['init', 'initModal'],
  ['archive-mobile', 'archiveMobilePicker'],
];
const tabbable = 'a[href], button, input, select, textarea, [tabindex], [contenteditable="true"]';
const mobileDetail = '.mobile-detail, .mobile-epic, .mobile-duo-ticket-controls';
const coveredElements = new WeakMap();

export function activeDialog(app) {
  return dialogStates.find(([, state]) => app[state])?.[0] || null;
}

export function blockingInteraction(app) {
  return activeDialog(app) || (app.isMobile && app.sheet ? 'sheet' : null);
}

export function mobileDetailCovered(app) {
  return app.isMobile && app.currentView === 'board' && (!!app.selectedEpic || (!!app.selectedTicket && app.mobileLayout !== 'duo-split'));
}

export function focusAvailable(el) {
  return !!el?.isConnected && typeof el.focus === 'function' && !el.closest('[inert], [hidden], [aria-hidden="true"]') &&
    !el.matches(':disabled') && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== 'hidden';
}

export function dialogFocusable(panel) {
  return [...panel.querySelectorAll(tabbable)].filter(el => el.tabIndex >= 0 && focusAvailable(el) && !el.closest('[data-agent-picker-panel]'));
}

export function stepDialogTab(event, panel, from) {
  const items = dialogFocusable(panel);
  const active = document.activeElement;
  const index = items.indexOf(from || active);
  let next = index < 0 ? (event.shiftKey ? items.length - 1 : 0) : index + (event.shiftKey ? -1 : 1);
  // A programmatically focused heading or proposal has a place in the DOM,
  // even when it isn't a Tab stop. Continue from there instead of wrapping.
  if (!from && index < 0 && active !== panel && active?.compareDocumentPosition && panel.contains(active)) {
    const direction = event.shiftKey ? active.DOCUMENT_POSITION_PRECEDING : active.DOCUMENT_POSITION_FOLLOWING;
    const inDirection = item => active.compareDocumentPosition(item) & direction;
    const nearby = event.shiftKey ? items.findLastIndex(inDirection) : items.findIndex(inDirection);
    if (nearby >= 0) next = nearby;
  }
  if (from || index < 0 || next < 0 || next >= items.length) {
    event.preventDefault();
    (items.length ? items[(next + items.length) % items.length] : panel).focus();
  }
  return true;
}

function applyCoverage(app, desired) {
  const key = app.interactionCoverageKey?.() || app;
  let covered = coveredElements.get(key);
  if (!covered) coveredElements.set(key, covered = new Map());
  for (const [el, before] of covered) {
    if (desired.has(el)) continue;
    el.inert = before.inert;
    if (!before.inert) el.removeAttribute?.('inert');
    if (before.hidden == null) el.removeAttribute?.('aria-hidden');
    else el.setAttribute('aria-hidden', before.hidden);
    covered.delete(el);
  }
  for (const el of desired) {
    if (!covered.has(el)) covered.set(el, { inert: !!(el.inert || el.hasAttribute?.('inert')), hidden: el.getAttribute?.('aria-hidden') });
    el.inert = true;
    el.setAttribute?.('inert', '');
    el.setAttribute?.('aria-hidden', 'true');
  }
}

export function syncInteractionCoverage(app, sheet = !!(app.isMobile && app.sheet)) {
  if (typeof document === 'undefined') return;
  const desired = new Set();
  const root = activeDialog(app);
  const add = (selector, except) => document.querySelectorAll?.(selector).forEach(el => {
    if (!except || (!el.contains(except) && !except.contains(el))) desired.add(el);
  });
  if (mobileDetailCovered(app)) add('[data-mobile-board-background]');
  if (root || sheet) add('[data-mobile-sheet-background]', !root && document.querySelector?.('[data-mobile-sheet-dialog]'));
  if (root) {
    add('[data-dialog-background]');
    add('.mobile-sheet-viewport');
  }
  // Also cover a closed layer while its exit transition is still visible.
  document.querySelectorAll?.('[data-dialog-layer]').forEach(layer => {
    const name = layer.getAttribute?.('data-dialog-layer');
    if (name && name !== root) desired.add(layer);
  });
  applyCoverage(app, desired);
}

export function canReturnInteractionFocus(app, target) {
  if (!focusAvailable(target) || app._appDestroyed || activeDialog(app)) return false;
  if (app.isMobile && app.sheet) return !!document.querySelector('[data-mobile-sheet-dialog]')?.contains(target);
  return !mobileDetailCovered(app) || !!target.closest(mobileDetail);
}

export function kontoraDialogs() {
  let owner = null;
  let current = null;
  let frame = null;
  const records = new Map();
  const nextTriggers = new Map();
  const dispatchOrigins = new Map();
  const coverageKey = {};

  const panelFor = name => document.querySelector?.(`[data-dialog-layer="${name}"] [data-dialog-panel]`);
  const focusInitial = (app, name, panel) => {
    const initial = name === 'init' ? (app.initForm?.kind === 'epic' ? '#init-agent' : '#init-pipeline') : '[data-dialog-initial]';
    const target = panel.querySelector(initial);
    (focusAvailable(target) ? target : dialogFocusable(panel)[0] || panel).focus({ preventScroll: true });
  };
  const stop = event => { event.preventDefault(); event.stopImmediatePropagation(); };
  const cancelFrame = () => {
    if (frame !== null) cancelAnimationFrame(frame);
    frame = null;
  };

  return {
    // Alpine supplies a different scope proxy to different bindings.
    interactionCoverageKey() { return coverageKey; },
    activeBlockingDialog() { return activeDialog(this); },
    interactionOwner() { return blockingInteraction(this); },
    restoreUiFocus(target) {
      if (canReturnInteractionFocus(this, target)) target.focus({ preventScroll: true });
    },
    dialogFocusManaged() { return !!owner; },

    // The card menu closes before dispatch can open a dialog. Keep its trigger
    // as the return target for this ticket and dialog.
    async dispatchWithDialogOrigin(layer, key, trigger, dispatch) {
      const id = JSON.stringify([layer, key]);
      if (dispatchOrigins.has(id)) return dispatch();
      dispatchOrigins.set(id, trigger);
      try { return await dispatch(); }
      finally { dispatchOrigins.delete(id); }
    },
    dialogTrigger(layer, key) {
      return dispatchOrigins.get(JSON.stringify([layer, key])) || globalThis.document?.activeElement;
    },
    rememberDialogTrigger(layer, trigger) {
      if (owner && trigger) nextTriggers.set(layer, trigger);
    },

    initDialogs() {
      if (owner || this._appDestroyed || typeof document === 'undefined') return;
      const disposers = owner = [];
      const listen = (name, fn) => {
        document.addEventListener?.(name, fn, true);
        disposers.push(() => document.removeEventListener?.(name, fn, true));
      };
      const changed = () => {
        if (owner !== disposers || this._appDestroyed) return;
        const before = document.activeElement;
        const name = activeDialog(this);
        const previous = current;
        const closed = previous ? records.get(previous) : null;
        for (const [layer, key] of dialogStates) {
          if (this[key] && !records.has(layer)) {
            // When palette navigation raises the Settings guard, canceling
            // should return to the field that had focus before the palette.
            const paletteOrigin = layer === 'settings' && previous === 'palette' && !this.paletteOpen ? closed?.origin : null;
            // Use each dialog's explicit trigger if set. Dialogs first observed
            // together without triggers share the current focus as their fallback.
            records.set(layer, { origin: nextTriggers.get(layer) || paletteOrigin || before });
            nextTriggers.delete(layer);
          } else if (!this[key]) records.delete(layer);
        }
        current = name;
        if (name !== previous && this._agentPickerOpenHost) this.closeAgentPicker(false);
        if (name !== previous) cancelFrame();
        // Wait for Alpine to update dialog visibility before syncing coverage and focus.
        this.$nextTick(() => {
          if (owner !== disposers || this._appDestroyed || activeDialog(this) !== name) return;
          syncInteractionCoverage(this);
          if (name === previous) return;
          if (name === null) {
            const target = closed?.origin;
            const active = document.activeElement;
            // An explicit focus move (e.g. a destination field after navigating
            // away from Settings) wins over the old dialog's return target.
            if (active && active !== document.body && canReturnInteractionFocus(this, active)) return;
            if (canReturnInteractionFocus(this, target)) {
              target.focus({ preventScroll: true });
            } else if (this.isMobile && this.sheet) {
              this._focusMobileSheet?.();
            } else if (closed && mobileDetailCovered(this)) {
              this.focusMobileDetail?.();
            } else if (previous === 'archive-mobile') {
              // Restoring an archived ticket removes the dialog's opener.
              this.archiveMobileFocusList?.();
            }
            return;
          }
          let attempts = 0;
          const focus = () => {
            frame = null;
            if (owner !== disposers || this._appDestroyed || activeDialog(this) !== name) return;
            const panel = panelFor(name);
            if (focusAvailable(panel)) {
              if (panel.contains(document.activeElement)) return;
              if (panel.contains(closed?.origin) && focusAvailable(closed.origin)) closed.origin.focus({ preventScroll: true });
              else focusInitial(this, name, panel);
            } else if (++attempts < 8) frame = requestAnimationFrame(focus);
          };
          focus();
        });
      };
      for (const [, key] of dialogStates) {
        const unwatch = this.$watch?.(key, changed);
        if (typeof unwatch === 'function') disposers.push(unwatch);
      }
      listen('focusin', event => {
        const name = activeDialog(this);
        if (!name) return;
        const panel = panelFor(name);
        if (focusAvailable(panel) && !panel.contains(event.target)) focusInitial(this, name, panel);
      });
      listen('keydown', event => {
        const name = activeDialog(this);
        if (!name || event.defaultPrevented) return;
        const panel = panelFor(name);
        if (event.key === 'Escape') {
          if (event.isComposing || event.keyCode === 229) { event.stopImmediatePropagation(); return; }
          stop(event);
          if (this._agentPickerOpenHost && panel?.contains(event.target) && this.agentPickerOwnsTarget(event.target)) {
            this.closeAgentPicker(true);
          } else {
            switch (name) {
              case 'automation-delete': this.automationsCloseDelete(); break;
              case 'archive': this.closeArchivePrompt(); break;
              case 'archive-mobile': this.archiveMobileClosePicker(); break;
              case 'settings': if (!this.settingsSaving) this.settingsGuardKeepEditing(); break;
              case 'palette': this.palettePop(); break;
              case 'note-delete': this.noteCloseDelete(); break;
              case 'delete': this.closeDeleteModal(); break;
              case 'init': this.closeInitModal(); break;
              // Sign-in has no cancel action.
            }
          }
        } else if (event.key === 'Tab') {
          if (!focusAvailable(panel)) { event.preventDefault(); return; }
          if (this._agentPickerOpenHost && panel.contains(event.target) && this.agentPickerOwnsTarget(event.target)) {
            const trigger = document.getElementById(this.agentPickerTriggerID(this._agentPickerOpenHost));
            this.closeAgentPicker(false);
            stepDialogTab(event, panel, trigger || panel);
            event.stopImmediatePropagation();
          } else if (!(name === 'palette' && event.target === this.$refs?.paletteInput && this.paletteDrillFromKey(event))) {
            stepDialogTab(event, panel);
          }
        }
      });
      changed();
    },

    destroyDialogs() {
      const disposers = owner;
      owner = null;
      current = null;
      cancelFrame();
      disposers?.forEach(dispose => dispose());
      records.clear();
      nextTriggers.clear();
      dispatchOrigins.clear();
      applyCoverage(this, new Set());
    },
  };
}
