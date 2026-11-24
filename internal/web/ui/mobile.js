// The phone ticket list, detail panels, and navigation and action sheets.
import { activeDialog, blockingInteraction, canReturnInteractionFocus, dialogFocusable, focusAvailable, stepDialogTab, syncInteractionCoverage } from './dialogs.js';
import { isoWithOffset, localScheduleText, parseScheduleInput, pickerToScheduleText, schedulePresets } from './format.js';

export function kontoraMobile() {
  // Keep DOM nodes and listener handles out of Alpine's reactive state.
  let lifecycle = null;
  let sheetFocus = null;
  let descriptionSequence = 0;
  let listProjection;
  let listPosition;
  const listPageSize = 100;
  let terminalFocusSequence = 0;

  return {
    _mobileWindowKey: '',
    mobileStatusFilter: ':attention',
    mobileProjectFilter: '',
    mobileAgentFilter: '',
    _mobileWindowSize: listPageSize,

    mobileFilterLabel() {
      if (this.mobileStatusFilter === ':attention') return 'Attention first';
      return this.columns.find(col => col.key === this.mobileStatusFilter)?.label || 'All statuses';
    },

    mobileFilterSummary() {
      return [this.mobileFilterLabel(), this.mobileProjectFilter && 'Project: ' + this.mobileProjectFilter,
        this.mobileAgentFilter && 'Agent: ' + this.mobileAgentFilter, this.needsInputOnly && 'Needs input']
        .filter(Boolean).join(' · ');
    },

    mobileFacetOptions(key) {
      const tickets = this.allBoardTickets();
      const configured = key === 'project' ? (this.configCache?.projects || []).map(project => project.name) : this.configCache?.agents || [];
      const names = tickets.map(ticket => key === 'project' ? this.ticketProjectName(ticket) : ticket.agent);
      const selected = key === 'project' ? this.mobileProjectFilter : this.mobileAgentFilter;
      return [...new Set([...configured, ...names, selected].filter(Boolean))].sort((a, b) => a.localeCompare(b));
    },

    mobileTicketCountKnown() {
      return !this.loading && (this.ticketsLastLoaded !== null || (this.ticketSourcesLoaded &&
        (this.secondaryTickets.length > 0 || this.ticketSources.some(source => source.id !== 'primary' && source.available))));
    },

    mobileIncompleteCount() {
      if (!this.ticketsLoadError && !this.ticketSourcesFailed()) return '';
      const count = this.mobileTicketList().rows.length;
      return count + (count === 1 ? ' match' : ' matches') + ' in loaded tickets';
    },

    mobileWindowKey() {
      return JSON.stringify([this.mobileStatusFilter, this.mobileProjectFilter, this.mobileAgentFilter,
        this.searchQuery, this.sourceFilter, this.needsInputOnly]);
    },

    mobileTicketList() {
      const filter = this.mobileStatusFilter;
      const board = this._board;
      const facets = this.mobileWindowKey();
      if (listProjection?.board === board && listProjection.facets === facets) return listProjection;
      const exact = value => value ? [{ value: value.toLowerCase(), exact: true }] : [];
      const query = { project: exact(this.mobileProjectFilter), agent: exact(this.mobileAgentFilter), epic: [], text: '' };
      const attention = [], inProgress = [], remaining = [];
      const cols = this.columns.filter(col => col.key === filter);
      for (const col of cols.length ? cols : this.columns) {
        for (const ticket of this.boardTickets(col.key)) {
          if (!this.ticketMatchesQuery(ticket, query)) continue;
          if (filter === ':attention' && (this.needsLiveInput(ticket) || ticket.status === 'human_review')) attention.push(ticket);
          else if (filter === ':attention' && this.executionActive(ticket)) inProgress.push(ticket);
          else remaining.push(ticket);
        }
      }
      const headings = {};
      if (attention.length) headings[0] = `Needs attention (${attention.length})`;
      if (inProgress.length) headings[attention.length] = `In progress (${inProgress.length})`;
      const otherStart = attention.length + inProgress.length;
      if (otherStart && remaining.length) headings[otherStart] = 'Other tickets';
      const search = this.parseFilterQuery(this.searchQuery);
      const epics = this.needsInputOnly || (this.sourceFilter && this.sourceFilter !== 'primary') ? [] : this.epics.filter(epic =>
        (!cols.length || cols.some(col => col.statuses.includes(epic.status))) &&
        this.ticketMatchesQuery(epic, search) && this.ticketMatchesQuery(epic, query));
      listProjection = { board, facets, rows: attention.concat(inProgress, remaining), headings, epics };
      return listProjection;
    },

    mobileTickets() {
      const size = this._mobileWindowKey === this.mobileWindowKey() ? this._mobileWindowSize : listPageSize;
      return this.mobileTicketList().rows.slice(0, size);
    },

    mobileLoadMore() {
      const key = this.mobileWindowKey();
      this._mobileWindowSize = (this._mobileWindowKey === key ? this._mobileWindowSize : listPageSize) + listPageSize;
      this._mobileWindowKey = key;
    },

    mobileListFilterChanged() {
      this._mobileWindowKey = '';
      if (this.$refs?.mobileCardList) this.$refs.mobileCardList.scrollTop = 0;
    },

    mobileAcceptDetail(ticket, kind = 'ticket') {
      if (!this.isMobile) return;
      const list = this.$refs?.mobileCardList;
      if (list) listPosition = { filter: this.mobileWindowKey(), top: list.scrollTop, key: this.ticketKey(ticket), kind };
      this.acceptMobileDetailRoute();
    },

    restoreMobileList() {
      const position = listPosition;
      if (!position || !this.isMobile || this.currentView !== 'board') return false;
      this.$nextTick(() => {
        if (this._appDestroyed || listPosition !== position || !this.isMobile || this.currentView !== 'board' ||
            this.selectedTicket || this.selectedEpic || blockingInteraction(this)) return;
        listPosition = null;
        const list = this.$refs?.mobileCardList;
        if (!list) return;
        if (position.filter === this.mobileWindowKey()) list.scrollTop = position.top;
        const bounds = list.getBoundingClientRect();
        const selector = position.kind === 'epic' ? '[data-mobile-epic-opener]' : '.mobile-card[data-mobile-ticket-key]';
        const opener = [...list.querySelectorAll(selector)].find(el => {
          if (el.dataset.mobileTicketKey !== position.key || !focusAvailable(el)) return false;
          const rect = el.getBoundingClientRect();
          return rect.bottom > bounds.top && rect.top < bounds.bottom && rect.right > bounds.left && rect.left < bounds.right;
        });
        const target = opener || this.$refs.mobileRefresh;
        if (focusAvailable(target)) target.focus({ preventScroll: true });
        else document.querySelector('[data-mobile-focus-fallback]')?.focus({ preventScroll: true });
      });
      return true;
    },

    mobileTicketStatus(ticket) {
      if (ticket.status === 'todo') return 'Queued';
      if (ticket.status === 'human_review') return 'Human review';
      if (ticket.status === 'paused') return 'Paused';
      return this.columns.find(col => col.key === ticket.status)?.label || this.statusLabel(ticket.status);
    },

    async mobileNavigate(view) {
      this.closeSheet();
      if (this.currentView === view && view !== 'board') return true;
      return this.navigateDestination({ view });
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
        update();
      }, true);
      listen(document, 'focusout', e => this._mobileTerminalFocusout(e), true);
      if (this.$watch) {
        const unwatch = this.$watch('sheet', () => {
          if (lifecycle === owner && !this._appDestroyed) this._syncMobileSheet();
        });
        if (typeof unwatch === 'function') owner.watches.push(unwatch);
        for (const key of ['searchQuery', 'mobileStatusFilter', 'mobileProjectFilter', 'mobileAgentFilter', 'needsInputOnly', 'sourceFilter']) {
          const stop = this.$watch(key, () => this.mobileListFilterChanged());
          if (typeof stop === 'function') owner.watches.push(stop);
        }
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
        if (lifecycle !== owner || this._appDestroyed) return;
        this._mobileResizeComposers();
        if (this.isMobile || window.matchMedia?.('(pointer: coarse)').matches) this._revealCompactField(geometry);
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

    _revealCompactField({ top, height, bottom }) {
      if (bottom < 1 && top < 1) return;
      const field = document.activeElement;
      if (!field?.matches('input:not(.sr-only), textarea:not(.xterm-helper-textarea), select, [contenteditable="true"]')) return;
      const scroller = field.closest('[data-compact-scroll], [data-dialog-panel]');
      if (!scroller) return;
      const bounds = scroller.getBoundingClientRect();
      const start = Math.max(top, bounds.top) + 8;
      const end = Math.min(top + height, bounds.bottom) - 8;
      if (end <= start) return;
      const rect = field.getBoundingClientRect();
      if (rect.top < start || rect.height > end - start) scroller.scrollTop += rect.top - start;
      else if (rect.bottom > end) scroller.scrollTop += rect.bottom - end;
    },

    mobileBreakpointChanged() {
      this.mobileResetTerminalInput();
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
      if (!blockingInteraction(this)) this.$refs.mobileDescriptionEditor?.focus({ preventScroll: true });
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

    // Native picker popovers stay in the sheet's DOM subtree.
    _syncMobileSheet() {
      if (!lifecycle || this._appDestroyed) return;
      const sheet = this.isMobile ? this.sheet : null;
      if (sheetFocus?.sheet === sheet) return;
      if (sheetFocus?.sheet?.type === 'assistant' && this.assistantOpen) this.closeAssistant();
      const returnFocus = sheetFocus?.returnFocus || document.activeElement;
      const keepCoverage = !!(sheet && sheetFocus?.dialog);
      this._releaseMobileSheetFocus(!sheet, keepCoverage);
      if (!sheet) return;
      const owner = lifecycle;
      const focus = sheetFocus = { sheet, returnFocus, dialog: null };
      this.$nextTick(() => {
        if (lifecycle !== owner || sheetFocus !== focus || this.sheet !== sheet || !this.isMobile || this._appDestroyed) return;
        const dialog = document.querySelector('[data-mobile-sheet-dialog]') || document.querySelector('.sheet-panel');
        if (!dialog) return;
        focus.dialog = dialog;
        syncInteractionCoverage(this);
        this._focusMobileSheet();
      });
    },

    _releaseMobileSheetFocus(restore, keepCoverage = false) {
      const focus = sheetFocus;
      sheetFocus = null;
      syncInteractionCoverage(this, keepCoverage);
      if (!focus || !restore || activeDialog(this)) return;
      const target = canReturnInteractionFocus(this, focus.returnFocus) ? focus.returnFocus
        : document.querySelector('[data-mobile-focus-fallback]');
      if (canReturnInteractionFocus(this, target)) target.focus({ preventScroll: true });
      else this.focusMobileDetail?.();
    },

    _focusMobileSheet() {
      const dialog = sheetFocus?.dialog;
      if (!dialog || activeDialog(this) || !focusAvailable(dialog)) return;
      const items = this._mobileSheetFocusable();
      const initial = items.find(el => el.hasAttribute('data-mobile-sheet-initial-focus'));
      (initial || items[0] || dialog).focus({ preventScroll: true });
    },

    _mobileFocusAvailable(el) {
      return focusAvailable(el);
    },

    _mobileSheetFocusable() {
      const dialog = sheetFocus?.dialog;
      return dialog ? dialogFocusable(dialog) : [];
    },

    // The picker calls this with its trigger on Tab, so closing the top-layer
    // panel advances within the sheet rather than escaping to the page behind.
    mobileSheetTab(event, from) {
      const dialog = sheetFocus?.dialog;
      if (!dialog || !this.sheet || !this.isMobile || activeDialog(this)) return false;
      return stepDialogTab(event, dialog, from);
    },

    _mobileSheetKeydown(event) {
      if (!sheetFocus?.dialog || !this.sheet || !this.isMobile || activeDialog(this) || event.defaultPrevented) return;
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
      if (!dialog || !this.sheet || !this.isMobile || activeDialog(this) || dialog.contains(event.target)) return;
      if (this.agentPickerOwnsTarget(event.target)) return;
      (this._mobileSheetFocusable()[0] || dialog).focus({ preventScroll: true });
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

    mobilePrimaryAction(t) {
      const key = this.ticketKey(t);
      const command = t?.status === 'human_review' && !this.executionActive(t)
        ? this.lifecycleCommand(key, 'move:done') : this.lifecycleActions(key)[0];
      if (!command) return { hidden: true };
      return { label: command.id === 'move:done' ? 'Mark done' : command.label,
        disabled: this.lifecycleBusy(key), run: () => this.dispatchLifecycle(key, command) };
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
      if (this.sameTicket(t, this.selectedTicket || this.selectedEpic)) add('Refresh ticket', 'default', () => this.reloadTicketDetail());
      if (t.kind === 'epic' && !this.sameTicket(t, this.selectedEpic) && !this.isSecondary(t)) add('Epic overview', 'default', () => this.openEpic(t.id));
      const current = this.lifecycleResolve(key) || t;
      if (this.ticketCanControl(current)) {
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
