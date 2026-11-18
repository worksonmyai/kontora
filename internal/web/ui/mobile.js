// The phone-width layer, gated on isMobile: a status tab strip and card list
// for the board, a full-screen tabbed detail, and
// bottom sheets for actions and navigation. It reuses the desktop
// board's ticket data and operations — only the layout and a few view-state
// fields (activeColumn, detailTab, sheet) are its own.
import { isoWithOffset, localScheduleText, parseScheduleInput, pickerToScheduleText, schedulePresets } from './format.js';

export function kontoraMobile() {
  // Keep DOM nodes and listener handles out of Alpine's reactive state.
  let lifecycle = null;
  let sheetFocus = null;
  let descriptionSequence = 0;
  let columnScrollSequence = 0;
  let terminalFocusSequence = 0;

  return {
    _mobileWindowKey: '',
    _mobileWindowSize: 100,

    mobileWindowKey() {
      return JSON.stringify([this.mobileColumn().key, this.searchQuery, this.sourceFilter]);
    },

    mobileTickets() {
      var size = this._mobileWindowKey === this.mobileWindowKey() ? this._mobileWindowSize : 100;
      return this.boardTickets(this.mobileColumn().key).slice(0, size);
    },

    mobileLoadMore() {
      var key = this.mobileWindowKey();
      this._mobileWindowSize = (this._mobileWindowKey === key ? this._mobileWindowSize : 100) + 100;
      this._mobileWindowKey = key;
    },

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
      listen(document, 'focusin', e => {
        this._mobileSheetFocusin(e);
        this._mobileTerminalFocusin(e);
      }, true);
      listen(document, 'focusout', e => this._mobileTerminalFocusout(e), true);
      if (this.$watch) {
        const unwatch = this.$watch('sheet', () => {
          if (lifecycle === owner && !this._appDestroyed) this._syncMobileSheet();
        });
        if (typeof unwatch === 'function') owner.watches.push(unwatch);
        for (const key of ['currentView', 'selectedTicket', 'selectedEpic']) {
          const stop = this.$watch(key, () => {
            if (lifecycle !== owner || this._appDestroyed) return;
            this.$nextTick(() => {
              if (lifecycle !== owner || this._appDestroyed) return;
              this.syncDetailCoverage();
              if (key === 'currentView' && this.currentView === 'board') this.restoreDetailScroll();
            });
          });
          if (typeof stop === 'function') owner.watches.push(stop);
        }
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
      this.mobileResetTerminalInput();
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

    mobileTerminalReading() {
      return this.isMobile && this.currentView === 'board' && this.terminalWanted() && !this.mobileTerminalTyping();
    },

    mobileTerminalPause() {
      const subject = this.selectedTicket || this.selectedEpic;
      const command = this.lifecycleCommand(subject, 'pause');
      return command && this._lifecycleAllowed(this.lifecycleResolve(subject), command) ? command : null;
    },

    mobilePauseTerminal() {
      const command = this.mobileTerminalPause();
      if (command) return this.dispatchLifecycle(command.ticketKey, command);
    },

    mobileTerminalCompact() {
      var session = this.selectedEpic ? this.epicTab === 'session' : this.detailTab === 'terminal';
      return this.isMobile && session &&
        (this.mobileTerminalTyping() || (this.mobileViewportHeight > 0 && this.mobileViewportHeight < 500));
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
      const owner = lifecycle;
      const geometry = this.mobileViewportGeometry();
      this.mobileViewportHeight = geometry.height;
      this.$nextTick(() => {
        if (lifecycle === owner && !this._appDestroyed) this._mobileResizeComposers();
      });
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
      this.mobileResetTerminalInput();
      ++columnScrollSequence;
      ++descriptionSequence;
      this.flushEditSave();
      this.editingBody = false;
      this.closeSheet(false);
    },

    // Navigation guards must run before this cleanup.
    mobileBeforeNavigate(view) {
      if (view === this.currentView) return;
      this.rememberDetailView();
      this.mobileResetTerminalInput();
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

    mobileDescriptionFocused() {
      return this.editing && this.editingBody && this.mobileCanEditDescription();
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
      if (sheetFocus?.sheet?.type === 'assistant' && this.assistantOpen) this.closeAssistant();
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
      if (this.sheet.type === 'assistant') {
        if (event.isComposing || event.keyCode === 229 || this._assistantComposing || this._assistantCompositionEnding) return;
        if (event.key === 'Escape' && (this.assistantMenu || this.assistantAutonomyOpen)) {
          event.preventDefault();
          event.stopImmediatePropagation();
          this.assistantMenu = null;
          this.assistantAutonomyOpen = false;
          return;
        }
        if (event.key === 'Tab' && this.assistantMenu && event.target?.matches('textarea[aria-autocomplete="list"]')) return;
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
      return this.executionActive(this.selectedTicket) ? 'terminal' : 'logs';
    },
    mobileFirstTabLabel() {
      return this.executionActive(this.selectedTicket) ? 'Terminal' : 'Logs';
    },

    mobilePanel() {
      if (['terminal', 'activity', 'logs'].includes(this.detailTab)) return 'output';
      return this.detailTab === 'diff' ? 'changes' : this.detailTab;
    },

    mobileSwitchPanel(panel) {
      const mode = panel === 'output' ? (this.isSecondary(this.selectedTicket) || !this.mobileHasRun(this.selectedTicket) || (this.activityStage !== null && this._activityExplicit) ? 'activity' : this.mobileFirstTab())
        : panel === 'changes' ? 'diff' : panel;
      this.mobileSwitchTab(mode);
    },
    mobileTagLabel(t) {
      return t?.pipeline || this.pathBasename(t?.path) || '—';
    },

    async openMobileTicket(t, navigation = ++this._navigationSeq) {
      if (this.currentView !== 'board') {
        if (await this.gotoView('board', navigation, { view: 'board', ticketId: t.id, source: t.source }) === false) return;
        if (navigation !== this._navigationSeq) return;
      }
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

    mobileSwitchTab(tab, { logStage = null } = {}) {
      if (!this.selectedTicket || !['ticket', 'notes', 'diff', 'terminal', 'activity', 'logs'].includes(tab) ||
          (this.isSecondary(this.selectedTicket) && ['terminal', 'logs'].includes(tab))) return;
      if (tab === 'terminal' && !this.executionActive(this.selectedTicket)) tab = 'activity';
      if (tab !== this.detailTab) this.rememberDetailView();
      if (tab !== 'ticket' && this.editing) this.mobileFinishDescriptionEdit();
      if (tab !== this.detailTab) {
        this._invalidateStageLogs();
        this._invalidateRawLog();
      }
      if (tab !== 'activity') {
        this._activityLoadSeq++;
        this.activityLoading = false;
      }
      this._stopActivityPoll();
      this.detailTab = tab;
      this.activeTab = tab === 'terminal' ? 'session' : tab;
      if (tab === 'ticket' && !this.editing && this.editorCanEdit(this.selectedTicket)) this.startEditing();
      if (tab === 'diff') this.fetchChanges(this.selectedTicket.id);
      this.restoreDetailScroll();
      if (tab === 'terminal') {
        this.logViewContent = null;
        this.logViewStage = null;
        if (this.executionActive(this.selectedTicket)) this.openTerminal();
      } else {
        if (this.terminalOpen) this.closeTerminal();
        if (tab === 'logs') {
          var t = this.selectedTicket;
          var stage = logStage ?? t.stage;
          if (!stage && t.history && t.history.length) stage = t.history[t.history.length - 1].stage;
          this.fetchStageLogs(t.id, stage);
        } else if (tab === 'activity' && !this.isSecondary(this.selectedTicket)) {
          var target = this._activityExplicit && this.activityStage !== null
            ? { stage: this.activityStage, run: this.activityRun } : this.activityTarget();
          if (target && !this.activityLoading && (!this.activity || target.stage !== this.activityStage || target.run !== this.activityRun)) {
            this.fetchActivity(target.stage, target.run);
          } else if (this.activity?.live) {
            this._armActivityPoll(this.activityStage, this.activityRun, true);
          }
        }
      }
    },

    mobileTicketCostLabel() {
      var cost = this.currentTicketCost();
      var label = this.costLabel(cost);
      if (!label) return '';
      if (cost.priced_runs < cost.tracked_runs) {
        label += ' (' + cost.priced_runs + '/' + cost.tracked_runs + ' runs priced)';
      }
      return 'est. ' + label;
    },

    mobileStageLabel(seg) {
      if (!seg.cost || !seg.cost.tracked_runs) return seg.name;
      var label = seg.name + ': ' + this.costLabel(seg.cost);
      if (seg.cost.priced_runs < seg.cost.tracked_runs) {
        label += ' (' + seg.cost.priced_runs + '/' + seg.cost.tracked_runs + ' runs priced)';
      }
      return label;
    },

    mobileStageMeta(seg) {
      if (!seg.runs && seg.cost?.tracked_runs > 0) {
        var runs = seg.cost.tracked_runs;
        var duration = seg.state === 'running' ? seg.meta : 'duration unavailable';
        return runs + ' tracked ' + (runs === 1 ? 'run' : 'runs') + ' · ' + duration;
      }
      return seg.meta + ' · ' + seg.runs + ' runs';
    },

    mobileStageState(seg) {
      var t = this.selectedTicket;
      if (seg.name === t?.stage && ['paused', 'human_review'].includes(t.status)) return this.statusLabel(t.status);
      if (t?.graph_definition || t?.graph_execution) return { running: 'Running node', done: 'Recorded visit', queued: 'No recorded visit' }[seg.state];
      if (t?.status === 'done' && !seg.runs && !(seg.cost?.tracked_runs > 0)) {
        var stages = t.stages || [];
        var index = stages.indexOf(seg.name);
        if (index < 0 || index >= stages.indexOf(t.stage)) return 'Ticket marked done; no recorded run';
      }
      return { running: 'Running', done: 'Recorded or earlier stage', queued: 'Not started' }[seg.state];
    },

    mobileStageStyle(seg) {
      var t = this.selectedTicket;
      var token = { running: '--st-progress', done: '--st-done', queued: '--surface-600' }[seg.state];
      var current = seg.name === t?.stage && ['paused', 'human_review'].includes(t.status);
      if (current) token = t.status === 'paused' ? '--st-paused' : '--st-review';
      var color = token === '--surface-600' ? 'rgb(var(' + token + '))' : 'hsl(var(' + token + '))';
      var fill = current || seg.state === 'running' ? '50%' : (seg.state === 'done' ? '100%' : '0%');
      var foreground = seg.state === 'queued' && !current ? 'rgb(var(--tx-2))' : color;
      return 'color:' + foreground + ';--mobile-stage-color:' + color + ';--mobile-stage-fill:' + fill + ';';
    },

    // Status-aware action-bar buttons. The primary performs the main transition
    // directly, the secondary the alternate one; unknown statuses fall back to
    // the actions sheet.
    mobilePrimaryAction(t) {
      const key = this.ticketKey(t);
      const command = t?.status === 'human_review' && !this.executionActive(t)
        ? this.lifecycleCommand(key, 'move:done') : this.lifecycleActions(key)[0];
      if (!command) return { label: t?.status === 'todo' ? 'Queued' : 'Actions', disabled: t?.status === 'todo', run: () => this.openActionsSheet(t) };
      return { label: command.id === 'move:done' ? 'Mark done' : command.label,
        disabled: this.lifecycleBusy(key), run: () => this.dispatchLifecycle(key, command) };
    },

    mobileSecondaryAction(t) {
      return { label: 'Move to', disabled: this.lifecycleBusy(t) || !this.lifecycleDestinations(t).length,
        run: () => this.openMoveSheet(t) };
    },

    openMoveSheet(t) {
      if (!this.lifecycleDestinations(t).length || this.lifecycleBusy(t)) return;
      this.closeAgentPicker(false);
      this.sheet = { type: 'move', ticket: t, destinations: this.lifecycleDestinations(t) };
      this._syncMobileSheet();
    },

    mobileMoveDestinations(t = this.sheet?.ticket || this.selectedTicket) {
      return this.sheet?.type === 'move' && this.sameTicket(t, this.sheet.ticket)
        ? this.sheet.destinations : this.lifecycleDestinations(t);
    },

    mobileMoveTo(status, t = this.sheet?.ticket || this.selectedTicket) {
      const key = this.ticketKey(t);
      const command = this.mobileMoveDestinations(t).find(destination => destination.status === status);
      this.closeSheet();
      return this.dispatchLifecycle(key, command);
    },

    mobileTerminalInputTarget: null,
    mobileComposeDrafts: {},

    mobileCanCompose() {
      return !this._appDestroyed && this.isMobile && this.currentView === 'board' && this.terminalWanted();
    },

    mobileTerminalTyping() {
      return this.mobileCanCompose() && this.mobileTerminalInputTarget !== null;
    },

    get mobileComposeDraft() {
      const key = this.outputSubjectKey();
      return key ? (this.mobileComposeDrafts[key] || '') : '';
    },

    set mobileComposeDraft(value) {
      const key = this.outputSubjectKey();
      if (key) this.mobileComposeDrafts[key] = value;
    },

    mobileBeginTerminalCompose(el) {
      if (!this.mobileCanCompose()) return;
      ++terminalFocusSequence;
      this.mobileTerminalInputTarget = 'composer';
      this.setTerminalRW(true);
      this.mobileResizeCompose(el);
    },

    mobileBeginTerminalDirect() {
      if (!this.mobileCanCompose()) return;
      if (document.activeElement?.matches('[data-mobile-terminal-input], .mobile-terminal-direct')) document.activeElement.blur();
      ++terminalFocusSequence;
      this.mobileTerminalInputTarget = 'terminal';
      this.setTerminalRW(true);
    },

    mobileResetTerminalInput() {
      ++terminalFocusSequence;
      this.mobileTerminalInputTarget = null;
      const active = document.activeElement;
      if (active?.matches('[data-mobile-terminal-input], .mobile-terminal-host .xterm-helper-textarea')) active.blur();
    },

    mobileFinishTerminalInput() {
      this.setTerminalRW(false);
    },

    mobileTerminalInputReady() {
      // Enabling xterm blurs a connecting tap, but the requested input session stays open.
      ++terminalFocusSequence;
    },

    _mobileTerminalFocusin(event) {
      if (!this.mobileCanCompose() || !this.terminalRW) return;
      if (event.target?.closest('.mobile-terminal-host')) {
        ++terminalFocusSequence;
        this.mobileTerminalInputTarget = 'terminal';
      }
    },

    _mobileTerminalFocusout(event) {
      // Entering input hides the reading header and can blur its focused Back button.
      if (event.target?.closest('.mobile-reading-bar')) return;
      if (!this.mobileTerminalTyping() || !event.target?.closest('.mobile-terminal-dock, .mobile-terminal-host, .mobile-terminal-bar')) return;
      const owner = lifecycle;
      const sequence = ++terminalFocusSequence;
      const key = this.outputSubjectKey();
      this.$nextTick(() => {
        if (lifecycle !== owner || sequence !== terminalFocusSequence || this._appDestroyed ||
            key !== this.outputSubjectKey() || !this.mobileTerminalTyping()) return;
        if (!document.activeElement?.closest('.mobile-terminal-dock, .mobile-terminal-host, .mobile-terminal-bar')) {
          this.mobileFinishTerminalInput();
        }
      });
    },

    mobileResizeCompose(el) {
      if (!el || !this.mobileCanCompose()) return;
      const style = getComputedStyle(el);
      const cap = 4 * parseFloat(style.lineHeight) + parseFloat(style.paddingTop) + parseFloat(style.paddingBottom) +
        parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth);
      el.style.height = 'auto';
      el.style.height = Math.max(44, Math.min(el.scrollHeight + parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth), cap)) + 'px';
    },

    _mobileResizeComposers() {
      this.mobileResizeCompose(this.$refs?.mobileTerminalCompose);
      this.mobileResizeCompose(this.$refs?.mobileEpicTerminalCompose);
    },

    submitComposeMobile() {
      if (!this.mobileCanCompose() || this.terminalSubjectKey !== this.outputSubjectKey() ||
          !this.sendTerminalCompose(this.mobileComposeDraft)) return false;
      this.mobileComposeDraft = '';
      this.$nextTick(() => this._mobileResizeComposers());
      return true;
    },

    openActionsSheet(t) {
      if (!t || this._appDestroyed) return;
      this.closeSheet(false);
      this.sheet = { type: 'actions', ticket: t };
      this._syncMobileSheet();
    },
    async openNewSheet() {
      if (this._appDestroyed) return false;
      return this.gotoView('new');
    },

    mobileNavigationCount(view) {
      if (view === 'board') return (this.tickets || []).length;
      if (view === 'automations') return (this.automations || []).length;
      if (view === 'archive') return (this.archiveRows || []).length;
      if (view === 'settings' && (this.settingsDirty() || this._settingsHasDrafts())) return 'Unsaved changes';
      if (view === 'new' && this.createSubmitting) return 'Creating…';
      return '';
    },
    closeSheet(restoreFocus = true) {
      if (this.sheet?.type === 'assistant') this.closeAssistant();
      this.closeAgentPicker(false);
      this.sheet = null;
      this._releaseMobileSheetFocus(restoreFocus);
    },

    // Action rows for the actions sheet, status by status. Each row closes the
    // sheet, then runs against a real endpoint (or opens the relevant modal).
    // The phone's schedule editor. It is a sheet rather than an inline row: the
    // detail overlay has no spare width for a date field and its buttons.
    openScheduleSheet(t) {
      if (this._appDestroyed || !this.canSchedule(t)) return;
      this.closeSheet(false);
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

    // The current schedule as the sheet header states it: "starts Fri 09:00,
    // in 18h". Empty for a ticket with none, where the header has nothing to
    // report yet.
    mobileScheduleCurrent(t) {
      var chip = this.scheduleChip(t);
      if (!chip) return '';
      return 'starts ' + chip.abs + (chip.rel ? ', in ' + chip.rel : '');
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
      const result = await this._postSchedule(t, { scheduled_at: parsed.iso });
      if (!this._appDestroyed && this.sheet === sheet && result?.outcome === 'success') this.closeSheet();
      return result;
    },

    // Clearing from inside the sheet, where the error line is on screen and the
    // toast fallback the actions sheet needs is not wanted.
    async clearScheduleSheet(t) {
      if (this.scheduleSubmitting) return;
      const sheet = this.sheet;
      const result = await this.clearTicketSchedule(t);
      if (!this._appDestroyed && this.sheet === sheet && result?.outcome === 'success') this.closeSheet();
      return result;
    },

    // "run now instead": the daemon drops the timestamp in the same save that
    // queues the ticket, so this is the ordinary start action.
    async runNowFromSheet(t) {
      const command = this.lifecycleCommand(t, 'start');
      this.closeSheet();
      return this.dispatchLifecycle(this.ticketKey(t), command);
    },

    // The actions sheet closes before the clear is posted, and scheduleError
    // only renders in the detail rail and the schedule sheet, neither of which
    // is on screen. The toast is the surface a phone has left.
    async clearScheduleMobile(t) {
      const result = await this.clearTicketSchedule(t);
      if (result?.error) this.error = result.error;
      return result;
    },

    mobileSheetActions(t) {
      if (!t) return [];
      const key = this.ticketKey(t);
      const rows = [];
      const add = (label, kind, run) => rows.push({ label, kind, run: () => { this.closeSheet(); return run(); } });
      const addCommand = command => {
        if (!command) return;
        add(command.label, ['cancel', 'delete'].includes(command.kind) ? 'danger' : 'default', () => this.dispatchLifecycle(key, command));
      };
      if (t.kind === 'epic') add('Epic overview', 'default', () => this.openEpic(t.id));
      if (this.ticketCanControl(t)) {
        this.lifecycleActions(key).forEach(addCommand);
        if (this.lifecycleDestinations(key).length) add('Move to', 'default', () => this.openMoveSheet(t));
        for (const id of ['cancel', 'skip', 'review', 'annotate', 'archive', 'delete']) {
          const command = this.lifecycleCommand(key, id);
          if (this._lifecycleAllowed(this.lifecycleResolve(key), command)) addCommand(command);
        }
        if (this.canSchedule(t)) {
          add(t.scheduled_at ? 'Reschedule…' : 'Schedule…', 'default', () => this.openScheduleSheet(t));
          if (t.scheduled_at) add('Clear schedule', 'default', () => this.clearScheduleMobile(t));
        }
      } else if (this.ticketCanClaim(t)) add('Claim into primary', 'primary', () => this.claimTicket(t));
      if (t.branch) add('Copy branch', 'default', () => this.copyBranch(t.branch));
      addCommand(this.lifecycleCommand(key, 'copy-id'));
      addCommand(this.lifecycleCommand(key, 'copy-link'));
      return rows;
    },
    mobileActionColor(kind) {
      if (kind === 'danger') return 'rgba(var(--err),1)';
      if (kind === 'warn') return 'hsl(var(--st-paused))';
      if (kind === 'primary') return 'rgba(var(--accent),1)';
      return 'rgba(var(--tx-2),1)';
    },
  };
}
