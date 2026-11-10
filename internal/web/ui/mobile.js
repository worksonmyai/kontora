// The phone-width layer, gated on isMobile: a status tab strip and card list
// for the board, a full-screen tabbed detail (terminal / logs / ticket), and
// bottom sheets for actions and the new-ticket form. It reuses the desktop
// board's ticket data and operations — only the layout and a few view-state
// fields (activeColumn, detailTab, sheet) are its own.
import { isoWithOffset, localScheduleText, parseScheduleInput, pickerToScheduleText, schedulePresets } from './format.js';

export function kontoraMobile() {
  // Keep DOM nodes and listener handles out of Alpine's reactive state.
  let lifecycle = null;
  let sheetFocus = null;
  let descriptionSequence = 0;
  let columnScrollSequence = 0;

  return {
    initMobile() {
      if (lifecycle || this._appDestroyed) return;
      const owner = lifecycle = { listeners: [], watches: [], frame: null, styles: [] };
      const listen = (target, name, handler, options) => {
        if (!target?.addEventListener) return;
        target.addEventListener(name, handler, options);
        owner.listeners.push(() => target.removeEventListener(name, handler, options));
      };
      const update = () => {
        if (lifecycle !== owner || this._appDestroyed) return;
        // Use the layout viewport, never the keyboard/pinch-zoom viewport, for
        // the board breakpoint.
        this.isMobile = window.innerWidth < 768;
        if (owner.frame !== null) return;
        owner.frame = requestAnimationFrame(() => {
          owner.frame = null;
          if (lifecycle === owner && !this._appDestroyed) this._updateMobileViewport();
        });
      };
      listen(window, 'resize', update);
      const viewport = window.visualViewport;
      listen(viewport, 'resize', update);
      listen(viewport, 'scroll', update);
      listen(document, 'keydown', e => this._mobileSheetKeydown(e), true);
      listen(document, 'focusin', e => this._mobileSheetFocusin(e), true);
      if (this.$watch) {
        const unwatch = this.$watch('sheet', () => {
          if (lifecycle === owner && !this._appDestroyed) this._syncMobileSheet();
        });
        if (typeof unwatch === 'function') owner.watches.push(unwatch);
      }
      this.isMobile = window.innerWidth < 768;
      this._updateMobileViewport();
      this._syncMobileSheet();
    },

    destroyMobile() {
      const owner = lifecycle;
      lifecycle = null;
      ++columnScrollSequence;
      ++descriptionSequence;
      this.closeAgentPicker(false);
      this.flushEditSave();
      this.editingBody = false;
      this.closeSheet(false);
      if (!owner) return;
      owner.listeners.forEach(remove => remove());
      owner.watches.forEach(remove => remove());
      if (owner.frame !== null) cancelAnimationFrame(owner.frame);
      owner.styles.forEach(({ name, value, priority }) => {
        const style = document.documentElement.style;
        if (value) style.setProperty(name, value, priority);
        else style.removeProperty(name);
      });
    },

    mobileViewportHeight: 0,

    mobileTerminalCompact() {
      return this.isMobile && this.detailTab === 'terminal' &&
        (this.terminalRW || (this.mobileViewportHeight > 0 && this.mobileViewportHeight < 500));
    },

    mobileViewportGeometry() {
      const viewport = window.visualViewport;
      const positive = (value, fallback) => Number.isFinite(value) && value > 0 ? value : fallback;
      const offset = value => Number.isFinite(value) && value >= 0 ? value : 0;
      const top = offset(viewport?.offsetTop);
      const height = positive(viewport?.height, window.innerHeight);
      return {
        top, left: offset(viewport?.offsetLeft),
        width: positive(viewport?.width, window.innerWidth), height,
        bottom: Math.max(0, window.innerHeight - top - height),
      };
    },

    _updateMobileViewport() {
      if (!lifecycle) return;
      const geometry = this.mobileViewportGeometry();
      this.mobileViewportHeight = geometry.height;
      const style = document.documentElement.style;
      Object.entries(geometry).forEach(([key, value]) => {
        const name = '--mobile-vv-' + key;
        if (!lifecycle.styles.some(saved => saved.name === name)) {
          lifecycle.styles.push({ name, value: style.getPropertyValue(name), priority: style.getPropertyPriority(name) });
        }
        style.setProperty(name, value + 'px');
      });
    },

    mobileBreakpointChanged() {
      ++columnScrollSequence;
      ++descriptionSequence;
      this.flushEditSave();
      this.editingBody = false;
      this.closeSheet(false);
    },

    // Navigation guards must run before this cleanup.
    mobileBeforeNavigate(view) {
      if (view === this.currentView) return;
      ++columnScrollSequence;
      ++descriptionSequence;
      if (this.isMobile) {
        this.flushEditSave();
        this.editingBody = false;
      }
      this.closeSheet(false);
    },

    mobileCanEditDescription() {
      return this.isMobile && this.currentView === 'board' && this.detailTab === 'ticket' && !this.detailLoading &&
        this.ticketDescriptionLoaded() && !!this.selectedTicket &&
        !this.isSecondary(this.selectedTicket) && this.editorCanEdit(this.selectedTicket) && !this.editorClaimLocked();
    },

    async mobileBeginDescriptionEdit() {
      if (!this.mobileCanEditDescription()) return false;
      const sequence = ++descriptionSequence;
      const key = this.ticketKey(this.selectedTicket);
      const detailSequence = this._ticketDetailSeq;
      // Reinitializing an active editor would replace an unsaved draft.
      const starting = this.editing ? null : this.startEditing();
      const generation = this._editorGeneration;
      if (starting) await starting;
      if (sequence !== descriptionSequence || this._appDestroyed || detailSequence !== this._ticketDetailSeq ||
          generation !== this._editorGeneration || this.ticketKey(this.selectedTicket) !== key ||
          !this.editing || !this.mobileCanEditDescription()) return false;
      this.editingBody = true;
      await this.$nextTick();
      if (sequence !== descriptionSequence || this._appDestroyed || !this.mobileCanEditDescription() || !this.editingBody ||
          detailSequence !== this._ticketDetailSeq || this.ticketKey(this.selectedTicket) !== key || generation !== this._editorGeneration) return false;
      this.$refs.mobileDescriptionEditor?.focus({ preventScroll: true });
      return true;
    },

    mobileDescriptionInput(value) {
      if (!this.editing || !this.mobileCanEditDescription()) return;
      // Explicit assignment works with or without x-model listener ordering.
      this.editForm.body = value;
      this.debounceSaveEdit();
    },

    mobileFinishDescriptionEdit() {
      ++descriptionSequence;
      const saved = this.flushEditSave();
      this.editingBody = false;
      return saved;
    },

    // Inert only siblings of the sheet. Native picker popovers stay in its DOM subtree.
    _syncMobileSheet() {
      if (!lifecycle || this._appDestroyed) return;
      const sheet = this.isMobile ? this.sheet : null;
      if (sheetFocus?.sheet === sheet) return;
      const returnFocus = sheetFocus?.returnFocus || document.activeElement;
      this._releaseMobileSheetFocus(!sheet);
      if (!sheet) return;
      const owner = lifecycle;
      const focus = sheetFocus = { sheet, returnFocus, dialog: null, backgrounds: [] };
      this.$nextTick(() => {
        if (lifecycle !== owner || sheetFocus !== focus || this.sheet !== sheet || !this.isMobile || this._appDestroyed) return;
        const dialog = document.querySelector('[data-mobile-sheet-dialog]') || document.querySelector('.sheet-panel');
        if (!dialog) return;
        focus.dialog = dialog;
        document.querySelectorAll('[data-mobile-sheet-background]').forEach(el => {
          if (el.contains(dialog) || dialog.contains(el)) return;
          focus.backgrounds.push({ el, inert: el.hasAttribute('inert'), hidden: el.getAttribute('aria-hidden') });
          el.setAttribute('inert', '');
          el.setAttribute('aria-hidden', 'true');
        });
        const initial = this._mobileSheetFocusable().find(el => el.hasAttribute('data-mobile-sheet-initial-focus'));
        (initial || this._mobileSheetFocusable()[0] || dialog).focus({ preventScroll: true });
      });
    },

    _releaseMobileSheetFocus(restore) {
      const focus = sheetFocus;
      sheetFocus = null;
      if (!focus) return;
      focus.backgrounds.forEach(({ el, inert, hidden }) => {
        if (!inert) el.removeAttribute('inert');
        if (hidden === null) el.removeAttribute('aria-hidden');
        else el.setAttribute('aria-hidden', hidden);
      });
      if (!restore) return;
      const target = this._mobileFocusAvailable(focus.returnFocus) ? focus.returnFocus
        : document.querySelector('[data-mobile-focus-fallback]');
      if (this._mobileFocusAvailable(target)) target.focus({ preventScroll: true });
    },

    _mobileFocusAvailable(el) {
      return !!el?.isConnected && typeof el.focus === 'function' && !el.closest('[inert], [hidden], [aria-hidden="true"]') &&
        !el.matches(':disabled') && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== 'hidden';
    },

    _mobileSheetFocusable() {
      const dialog = sheetFocus?.dialog;
      if (!dialog) return [];
      return Array.from(dialog.querySelectorAll('a[href], button, input, select, textarea, [tabindex], [contenteditable="true"]'))
        .filter(el => el.tabIndex >= 0 && this._mobileFocusAvailable(el) && !el.closest('[data-agent-picker-panel]'));
    },

    // The picker calls this with its trigger on Tab, so closing the top-layer
    // panel advances within the sheet rather than escaping to the page behind.
    mobileSheetTab(event, from) {
      const dialog = sheetFocus?.dialog;
      if (!dialog || !this.sheet || !this.isMobile) return false;
      const items = this._mobileSheetFocusable();
      const active = from || document.activeElement;
      const index = items.indexOf(active);
      const next = index < 0 ? (event.shiftKey ? items.length - 1 : 0) : index + (event.shiftKey ? -1 : 1);
      if (from || index < 0 || next < 0 || next >= items.length) {
        event.preventDefault();
        const target = items.length ? items[(next + items.length) % items.length] : dialog;
        target.focus();
      }
      return true;
    },

    _mobileSheetKeydown(event) {
      if (!sheetFocus?.dialog || !this.sheet || !this.isMobile || event.defaultPrevented) return;
      if (this._agentPickerOpenHost) {
        if (event.key !== 'Escape') return;
        event.preventDefault();
        event.stopImmediatePropagation();
        this.closeAgentPicker(true);
        return;
      }
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopImmediatePropagation();
        this.closeSheet();
      } else if (event.key === 'Tab') this.mobileSheetTab(event);
    },

    _mobileSheetFocusin(event) {
      const dialog = sheetFocus?.dialog;
      if (!dialog || !this.sheet || !this.isMobile || dialog.contains(event.target)) return;
      if (this.agentPickerOwnsTarget(event.target)) return;
      (this._mobileSheetFocusable()[0] || dialog).focus({ preventScroll: true });
    },

    // The status column currently shown on the board, clamped to a valid index.
    mobileColumn() {
      var cols = this.columns;
      var i = Math.max(0, Math.min(this.activeColumn, cols.length - 1));
      return cols[i];
    },

    setActiveColumn(i) {
      this.activeColumn = Math.max(0, Math.min(i, this.columns.length - 1));
      const sequence = ++columnScrollSequence;
      const owner = lifecycle;
      const strip = this.$refs?.mobileStatusTabs;
      if (!owner || !strip || !this.isMobile || this._appDestroyed) return;
      this.$nextTick(() => {
        if (sequence !== columnScrollSequence || lifecycle !== owner || this._appDestroyed || !this.isMobile ||
            this.currentView !== 'board' || this.selectedTicket || this.selectedEpic || this.sheet ||
            this.$refs?.mobileStatusTabs !== strip || !strip.isConnected || !strip.getClientRects().length) return;
        this.activeColumn = Math.max(0, Math.min(this.activeColumn, this.columns.length - 1));
        const button = strip.querySelectorAll('button')[this.activeColumn];
        if (!button) return;
        const bounds = strip.getBoundingClientRect();
        const selected = button.getBoundingClientRect();
        const style = getComputedStyle(strip);
        const left = bounds.left + strip.clientLeft + (parseFloat(style.paddingLeft) || 0);
        const right = bounds.left + strip.clientLeft + strip.clientWidth - (parseFloat(style.paddingRight) || 0);
        // Oversized labels stay start-aligned so repeated boundary swipes cannot alternate edges.
        const delta = selected.right - selected.left > right - left || selected.left < left
          ? selected.left - left : Math.max(0, selected.right - right);
        // scrollIntoView can also scroll the page and its vertical ancestors.
        if (delta) strip.scrollBy({ left: delta, behavior: 'instant' });
      });
    },

    // Horizontal swipe on the card list moves between status columns. Mirrors
    // the prototype's threshold: dominant horizontal travel of at least 55px.
    onBoardTouchStart(e) {
      var t = e.changedTouches[0];
      this._swipeX = t.clientX;
      this._swipeY = t.clientY;
    },
    onBoardTouchEnd(e) {
      if (this.selectedTicket || this.selectedEpic || this.sheet) return;
      var t = e.changedTouches[0];
      var dx = t.clientX - (this._swipeX || 0);
      var dy = t.clientY - (this._swipeY || 0);
      if (Math.abs(dx) > 55 && Math.abs(dx) > Math.abs(dy) * 1.4) {
        this.setActiveColumn(this.activeColumn + (dx < 0 ? 1 : -1));
      }
    },

    // A ticket "has a run" once it leaves the draft/queued states; that's when
    // the terminal/logs tabs and the detail tab bar appear.
    mobileHasRun(t) {
      if (!t || this.isSecondary(t) || (t.kind === 'epic' && !this.epicIsAgent(t))) return false;
      if (!['open', 'todo'].includes(t.status) || (t.history || []).length > 0) return true;
      return this.selectedTicket?.id === t.id && (this.ticketCost?.tracked_runs || 0) > 0;
    },
    // The first (non-ticket) detail tab: a live terminal while running, the
    // stage-log history otherwise.
    mobileFirstTab() {
      return this.selectedTicket && this.selectedTicket.status === 'in_progress' ? 'terminal' : 'logs';
    },
    mobileFirstTabLabel() {
      return this.selectedTicket && this.selectedTicket.status === 'in_progress' ? '›_ terminal' : '≡ logs';
    },

    async openMobileTicket(t, navigation = ++this._navigationSeq) {
      if (this.currentView !== 'board') {
        if (await this.gotoView('board', navigation, { view: 'board', ticketId: t.id, source: t.source }) === false) return;
        if (navigation !== this._navigationSeq) return;
      }
      // Pick the detail tab from the card's status before selectTicket() runs,
      // so the right pane is already visible when the live terminal attaches.
      if (t.status === 'in_progress' && this.mobileHasRun(t)) this.detailTab = 'terminal';
      else if (this.mobileHasRun(t)) this.detailTab = 'logs';
      else this.detailTab = 'ticket';
      await this.selectTicket(t, navigation);
      if (navigation !== this._navigationSeq || !this.sameTicket(this.selectedTicket, t)) return;
      // The board card lacks detail fields, so the summary check runs on the
      // full ticket selectTicket fetched.
      if (this.detailTab === 'logs' && this.summaryFirst(this.selectedTicket)) {
        this.detailTab = 'ticket';
      }
      // selectTicket() applies the desktop tab heuristics, which only fetch logs
      // when the ticket carries history. The mobile logs tab needs them either
      // way, so load them here if nothing started (logViewLoading is set
      // synchronously when selectTicket did kick off a fetch).
      if (this.detailTab === 'logs' && this.logViewContent === null && !this.logViewLoading) {
        this.mobileSwitchTab('logs');
      }
    },

    mobileSwitchTab(tab) {
      if (!this.selectedTicket || (this.isSecondary(this.selectedTicket) && tab !== 'ticket')) return;
      if (tab !== 'ticket' && this.editing) this.mobileFinishDescriptionEdit();
      if (tab !== this.detailTab) {
        this._invalidateStageLogs();
        this._invalidateRawLog();
      }
      this._activityLoadSeq++;
      this.activityLoading = false;
      this._stopActivityPoll();
      this.detailTab = tab;
      if (tab === 'terminal') {
        this.logViewContent = null;
        this.logViewStage = null;
        if (this.selectedTicket.status === 'in_progress') this.openTerminal();
      } else {
        if (this.terminalOpen) this.closeTerminal();
        if (tab === 'logs') {
          var t = this.selectedTicket;
          var stage = t.stage;
          if (!stage && t.history && t.history.length) stage = t.history[t.history.length - 1].stage;
          this.fetchStageLogs(t.id, stage);
        }
      }
    },

    mobileTicketCostLabel() {
      var cost = this.currentTicketCost();
      var label = this.costLabel(cost);
      if (!label) return '';
      if (cost.priced_runs < cost.tracked_runs) {
        label += ' · ' + cost.priced_runs + '/' + cost.tracked_runs;
      }
      return 'est. ' + label;
    },

    mobileStageLabel(seg) {
      if (!seg.cost || !seg.cost.tracked_runs) return seg.name;
      var label = seg.name + ' · ' + this.costLabel(seg.cost);
      if (seg.cost.priced_runs < seg.cost.tracked_runs) {
        label += ' · ' + seg.cost.priced_runs + '/' + seg.cost.tracked_runs;
      }
      return label;
    },

    // Inline style for one chip in the logs-tab stage strip.
    mobileStageStyle(stage) {
      var base = "font-family:'JetBrains Mono',monospace;font-size:12px;padding:4px 10px;border-radius:6px;white-space:nowrap;flex:none;";
      var t = this.selectedTicket;
      var stages = (t && t.stages) || [];
      var cur = stages.indexOf(t && t.stage);
      var i = stages.indexOf(stage);
      if (i === cur) return base + 'background:rgba(var(--surface-800),1);color:rgba(var(--tx),1);border:1px solid rgba(var(--surface-700),1);';
      if (cur >= 0 && i < cur) return base + 'color:rgba(var(--surface-600),1);';
      return base + 'color:rgba(var(--surface-700),1);';
    },

    // Status-aware action-bar buttons. The primary performs the main transition
    // directly, the secondary the alternate one; unknown statuses fall back to
    // the actions sheet.
    mobilePrimaryAction(t) {
      if (!this.ticketCanControl(t)) return { label: this.ticketCanClaim(t) ? 'claim' : 'view', run: () => this.ticketCanClaim(t) ? this.claimTicket(t) : this._paletteOpenTicket(t) };
      var self = this, id = t && t.id;
      var run = function(ep) { return function() { self.moveTicketVia(id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(id, 'move', { status: st }); }; };
      if (this.epicCanStart(t)) return { label: 'start epic', run: function() { self.openInitModal(t); } };
      if (this.epicIsAgent(t) && ['paused', 'done', 'cancelled'].includes(t.status)) {
        return { label: t.status === 'paused' ? 'retry supervisor' : 'restart supervisor', run: run('retry') };
      }
      var map = {
        open:         { label: 'queue',          run: run('run') },
        todo:         { label: 'queue',          run: run('run') },
        in_progress:  { label: 'send to review', run: move('human_review') },
        paused:       { label: 'resume',         run: run('retry') },
        human_review: { label: 'approve',        run: move('done') },
        done:         { label: 're-open',        run: run('retry') },
        cancelled:    { label: 're-open',        run: run('retry') },
      };
      return map[t && t.status] || { label: 'actions', run: function() { self.openActionsSheet(t); } };
    },
    mobileSecondaryAction(t) {
      if (!this.ticketCanControl(t)) return { label: 'details', run: () => this._paletteOpenTicket(t) };
      var self = this, id = t && t.id;
      var run = function(ep) { return function() { self.moveTicketVia(id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(id, 'move', { status: st }); }; };
      if (this.epicIsAgent(t) && t.status === 'in_progress') {
        return { label: 'pause supervisor', run: function() { self.action('pause'); } };
      }
      if (t?.kind === 'epic' && !this.epicIsAgent(t)) {
        return { label: 'epic overview', run: function() { self.openEpic(id); } };
      }
      var map = {
        open:         { label: 'cancel',    run: move('cancelled') },
        todo:         { label: 'cancel',    run: move('cancelled') },
        in_progress:  { label: 'pause',     run: function() { self.action('pause'); } },
        paused:       { label: 'cancel',    run: move('cancelled') },
        human_review: { label: 'send back', run: run('retry') },
        done:         { label: 'delete',    run: function() { self.openDeleteModal(); } },
        cancelled:    { label: 'delete',    run: function() { self.openDeleteModal(); } },
      };
      return map[t && t.status] || { label: 'more', run: function() { self.openActionsSheet(t); } };
    },

    openActionsSheet(t) {
      if (!t || this._appDestroyed) return;
      this.closeAgentPicker(false);
      this.sheet = { type: 'actions', ticket: t };
      this._syncMobileSheet();
    },
    async openNewSheet() {
      if (this._appDestroyed) return;
      const owner = lifecycle;
      this.closeAgentPicker(false);
      this.resetCreateForm();
      this.sheet = { type: 'new' };
      const sheet = this.sheet;
      const form = this.createForm;
      this._syncMobileSheet();
      await this.loadCreateConfig();
      return lifecycle === owner && !this._appDestroyed && this.sheet === sheet && this.createForm === form;
    },
    closeSheet(restoreFocus = true) {
      this.closeAgentPicker(false);
      if (this.sheet?.type === 'new') this.invalidateCreateForm();
      this.sheet = null;
      this._releaseMobileSheetFocus(restoreFocus);
    },

    onMobileCreateSchedulePicked(value) {
      this.createForm.scheduled_at = value;
      this.setCreateScheduleMode(value ? 'later' : 'now');
    },

    async submitCreateTicketMobile() {
      const sheet = this.sheet;
      if (sheet?.type !== 'new' || this.createSubmitting) return false;
      const form = this.createForm;
      const owner = lifecycle;
      const ok = await this.submitCreateTicket();
      if (ok === true && lifecycle === owner && !this._appDestroyed && this.sheet === sheet && this.createForm === form) this.closeSheet();
      return ok === true;
    },

    // Action rows for the actions sheet, status by status. Each row closes the
    // sheet, then runs against a real endpoint (or opens the relevant modal).
    // The phone's schedule editor. It is a sheet rather than an inline row: the
    // detail overlay has no spare width for a date field and its buttons.
    openScheduleSheet(t) {
      if (this._appDestroyed || !this.canSchedule(t)) return;
      this.closeAgentPicker(false);
      this.openScheduleEditor(t);
      // The draft is the text the one field holds, in the grammar the CLI and
      // the desktop form take, not the datetime-local value openScheduleEditor
      // wrote for the rail.
      this.scheduleDraft = t && t.scheduled_at ? localScheduleText(new Date(t.scheduled_at)) : '';
      this.sheet = { type: 'schedule', ticket: t };
      this._syncMobileSheet();
    },

    // The three presets the sheet offers, the first three of the shared table.
    mobileSchedulePresets() {
      return schedulePresets(new Date()).slice(0, 3);
    },

    mobileSchedulePick(preset) {
      this.scheduleDraft = localScheduleText(preset.at);
      this.scheduleError = null;
    },

    // What the draft currently means, or why it means nothing. One read backs
    // the selected row, the consequence line and the save button.
    mobileScheduleParsed() {
      return parseScheduleInput(this.scheduleDraft, new Date());
    },

    mobileSchedulePresetSelected(preset) {
      return this.mobileScheduleParsed().iso === preset.iso;
    },

    // The sheet's one line of consequence: the exact instant being sent, then
    // what still stands between the ticket and an agent.
    mobileScheduleConsequence() {
      var parsed = this.mobileScheduleParsed();
      if (!parsed.iso) return '';
      return 'Sent as ' + isoWithOffset(parsed.at) + '. Stays open until then; pickup still needs auto_pick_up and resolved dependencies.';
    },

    // The current schedule as the sheet header states it: "starts Fri 09:00 ·
    // in 18h". Empty for a ticket with none, where the header has nothing to
    // report yet.
    mobileScheduleCurrent(t) {
      var chip = this.scheduleChip(t);
      if (!chip) return '';
      return 'starts ' + chip.abs + (chip.rel ? ' · in ' + chip.rel : '');
    },

    onMobileSchedulePicked(value) {
      var text = pickerToScheduleText(value);
      if (!text) return;
      this.scheduleDraft = text;
      this.scheduleError = null;
    },

    async submitScheduleMobile(t) {
      var parsed = this.mobileScheduleParsed();
      if (!parsed.iso) {
        this.scheduleError = parsed.error || 'Enter a time or a duration.';
        return;
      }
      if (this.scheduleSubmitting) return;
      const sheet = this.sheet;
      await this._postSchedule(t, { scheduled_at: parsed.iso });
      if (!this._appDestroyed && this.sheet === sheet && !this.scheduleError) this.closeSheet();
    },

    // Clearing from inside the sheet, where the error line is on screen and the
    // toast fallback the actions sheet needs is not wanted.
    async clearScheduleSheet(t) {
      if (this.scheduleSubmitting) return;
      const sheet = this.sheet;
      await this.clearTicketSchedule(t);
      if (!this._appDestroyed && this.sheet === sheet && !this.scheduleError) this.closeSheet();
    },

    // "run now instead": the daemon drops the timestamp in the same save that
    // queues the ticket, so this is the ordinary start action.
    async runNowFromSheet(t) {
      this.closeSheet();
      await this.moveTicketVia(t.id, 'run', null);
    },

    // The actions sheet closes before the clear is posted, and scheduleError
    // only renders in the detail rail and the schedule sheet, neither of which
    // is on screen. The toast is the surface a phone has left.
    async clearScheduleMobile(t) {
      await this.clearTicketSchedule(t);
      if (this.scheduleError) this.error = this.scheduleError;
    },

    mobileSheetActions(t) {
      if (!this.ticketCanControl(t)) return this.ticketCanClaim(t) ? [{ label: 'Claim into primary', kind: 'primary', run: () => this.claimTicket(t) }] : [];
      if (!t) return [];
      var self = this, rows = [];
      var add = function(label, kind, fn) { rows.push({ label: label, kind: kind, run: function() { self.closeSheet(); fn(); } }); };
      var run = function(ep) { return function() { self.moveTicketVia(t.id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(t.id, 'move', { status: st }); }; };
      var s = t.status;
      if (t.kind === 'epic') {
        add('Epic overview', 'default', function() { self.openEpic(t.id); });
        if (this.epicCanStart(t)) {
          add('Start epic', 'primary', function() { self.openInitModal(t); });
          return rows;
        }
        if (!this.epicIsAgent(t)) return rows;
      }
      if ((s === 'open' || s === 'todo') && !t.kontora) add('Initialize ticket', 'warn', function() { self.openInitModal(t); });
      // On a scheduled ticket "Queue agent" is the run-now action: the daemon
      // drops the timestamp in the same save.
      if (this.canSchedule(t)) {
        add(t.scheduled_at ? 'Reschedule…' : 'Schedule…', 'default', function() { self.openScheduleSheet(t); });
        if (t.scheduled_at) add('Clear schedule', 'default', function() { self.clearScheduleMobile(t); });
      }
      if (s === 'open') {
        add('Queue agent', 'primary', run('run'));
        add('Move to Human Review', 'default', move('human_review'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'todo') {
        add('Queue agent', 'primary', run('run'));
        add('Move to open', 'default', move('open'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'in_progress') {
        add(this.epicIsAgent(t) ? 'Pause supervisor' : 'Pause agent before changing stages', 'warn', function() { self.moveTicketVia(t.id, 'pause', null); });
        add('Send to review', 'primary', move('human_review'));
        add('Mark done', 'default', move('done'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'paused') {
        add(this.epicIsAgent(t) ? 'Retry supervisor' : 'Resume', 'primary', run('retry'));
        add('Mark done', 'default', move('done'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'human_review') {
        if (t.kontora) add('Plannotator review', 'default', function() { self.startPlannotatorReview(t); });
        add('Approve & merge', 'primary', move('done'));
        add('Send back', 'warn', run('retry'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'done') {
        add(this.epicIsAgent(t) ? 'Restart supervisor' : 'Re-open', 'default', run('retry'));
        add('Send to review', 'default', move('human_review'));
        if (t.branch) add('Copy branch', 'default', function() { self.copyBranch(t.branch); });
        add('Delete file', 'danger', function() { self.openDeleteModal(); });
      } else if (s === 'cancelled') {
        add(this.epicIsAgent(t) ? 'Restart supervisor' : 'Re-open', 'default', run('retry'));
        add('Delete file', 'danger', function() { self.openDeleteModal(); });
      } else {
        (this.validMoves[s] || []).forEach(function(mv) {
          add(mv.label, 'default', mv.status ? move(mv.status) : run(mv.endpoint));
        });
      }
      if (this.canSkipStage(t)) add('Skip stage', 'default', run('skip'));
      return rows;
    },
    mobileActionColor(kind) {
      if (kind === 'danger') return 'rgba(var(--err),1)';
      if (kind === 'warn') return 'hsl(var(--st-paused))';
      if (kind === 'primary') return 'rgba(var(--accent),1)';
      return 'rgba(var(--tx-2),1)';
    },
    mobileActionDotColor(kind) {
      if (kind === 'danger') return 'rgba(var(--err),1)';
      if (kind === 'warn') return 'hsl(var(--st-paused))';
      if (kind === 'primary') return 'rgba(var(--accent),1)';
      return 'rgba(var(--surface-600),1)';
    },
  };
}
