// The phone ticket list, detail panels, and navigation and action sheets.
import { activeDialog, blockingInteraction, canReturnInteractionFocus, dialogFocusable, focusAvailable, stepDialogTab, syncInteractionCoverage } from './dialogs.js';
import { isoWithOffset, localScheduleText, parseScheduleInput, pickerToScheduleText, schedulePresets } from './format.js';

export function kontoraMobile() {
  // Keep DOM nodes and listener handles out of Alpine's reactive state.
  let lifecycle = null;
  let sheetFocus = null;
  let descriptionSequence = 0;
  let listProjection;
  let columnProjection;
  let listRestoreSequence = 0;
  let restoringList = false;
  let rowGesture = null;
  let consumedClick = null;
  const listPageSize = 100;
  let terminalFocusSequence = 0;

  return {
    _mobileWindowKey: '',
    mobileStatusFilter: ':attention',
    mobileProjectFilter: '',
    mobileAgentFilter: '',
    _mobileWindowSize: listPageSize,
    mobileColumnPreferences: {},
    mobileSelecting: false,
    mobileSelection: [],
    mobileRevealedRow: null,

    mobileFilterLabel() {
      if (this.mobileStatusFilter === ':attention') return 'Default';
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

    mobileMatchedRows(text, { column = '', ignoreProject = false } = {}) {
      const query = this.parseFilterQuery(text);
      const exact = value => value ? [{ value: value.toLowerCase(), exact: true }] : [];
      const facets = { project: exact(ignoreProject ? '' : this.mobileProjectFilter), agent: exact(this.mobileAgentFilter), epic: [], text: '' };
      const statuses = this.columns.find(col => col.key === column)?.statuses;
      const matched = new Map();
      for (const ticket of this.allBoardTickets()) {
        if (statuses && !statuses.includes(ticket.status)) continue;
        if (this.sourceFilter && (ticket.source || 'primary') !== this.sourceFilter) continue;
        if (this.needsInputOnly && !this.needsLiveInput(ticket)) continue;
        if (!this.ticketMatchesQuery(ticket, query) || !this.ticketMatchesQuery(ticket, facets)) continue;
        matched.set(this.ticketKey(ticket), ticket);
      }
      return [...matched.values()];
    },

    mobileInProgressRows(rows) {
      const statuses = new Set(this.columns.find(col => col.key === 'in_progress')?.statuses || []);
      const ranked = [];
      for (const ticket of rows) {
        const active = !this.isSecondary(ticket) && this.executionActive(ticket);
        if (!statuses.has(ticket.status) && !active) continue;
        const rank = this.needsLiveInput(ticket) ? 0 : active ? 1
          : ticket.status === 'paused' ? 2 : ticket.status === 'todo' ? 3 : 4;
        ranked.push({ ticket, rank, started: rank < 2 ? (Date.parse(ticket.started_at) || 0) : 0 });
      }
      return ranked.sort((a, b) => a.rank - b.rank || b.started - a.started ||
        this.ticketKey(a.ticket).localeCompare(this.ticketKey(b.ticket))).map(row => row.ticket);
    },

    mobileHomeFiltered() {
      return !!this.searchQuery.trim() || this.mobileStatusFilter !== ':attention' || this.needsInputOnly;
    },

    mobileClearFilters() {
      this.searchQuery = '';
      this.mobileStatusFilter = ':attention';
      this.mobileProjectFilter = '';
      this.mobileAgentFilter = '';
      this.sourceFilter = '';
      if (this.needsInputOnly) this.toggleNeedsInput();
      else this.recomputeBoard();
    },

    mobileTicketList() {
      const board = this._board;
      const columns = this.columns;
      const facets = this.mobileWindowKey();
      if (listProjection?.board === board && listProjection.columns === columns && listProjection.facets === facets) return listProjection;
      const matched = this.mobileMatchedRows(this.searchQuery, { column: this.mobileStatusFilter });
      const epics = matched.filter(ticket => !this.isSecondary(ticket) && ticket.kind === 'epic');
      const ordinary = matched.filter(ticket => this.isSecondary(ticket) || ticket.kind !== 'epic' || this.epicIsAgent(ticket));
      const progress = this.mobileInProgressRows(ordinary);
      const byColumn = new Map(columns.map(col => [col.key, []]));
      const columnByStatus = new Map(columns.flatMap(col => col.statuses.map(status => [status, col.key])));
      for (const ticket of ordinary) byColumn.get(columnByStatus.get(ticket.status))?.push(ticket);
      for (const col of columns) this._sortColumn(byColumn.get(col.key), col.statuses);
      const open = byColumn.get('open') || [];
      const waiting = progress.filter(ticket => this.needsLiveInput(ticket));
      const results = columns.map(col => ({ ...col, rows: byColumn.get(col.key) })).filter(col => col.rows.length);
      listProjection = { board, columns, facets, progress, open, waiting, byColumn, results, rows: results.flatMap(col => col.rows), epics };
      return listProjection;
    },

    mobileBoardChanged() {
      if (listProjection) this.mobileTicketList();
      this.mobilePruneSelection();
    },

    mobileHomeGroups() {
      const projection = this.mobileTicketList();
      if (!this.mobileHomeFiltered()) return [
        { key: 'in_progress', label: 'In progress', rows: projection.progress },
        { key: 'open', label: 'Open', rows: projection.open.slice(0, 4) },
      ];
      let remaining = this._mobileWindowKey === this.mobileWindowKey() ? this._mobileWindowSize : listPageSize;
      return projection.results.flatMap(group => {
        const rows = group.rows.slice(0, remaining);
        remaining -= rows.length;
        return rows.length ? [{ ...group, rows }] : [];
      });
    },

    mobileListGroups() {
      if (this.mobileColumnKey) return this.mobileColumnGroups();
      return this.ticketBoardEmpty() ? [] : this.mobileHomeGroups();
    },

    mobileColumnDefinition() {
      return this.columns.find(col => col.key === this.mobileColumnKey);
    },

    mobileColumnPreference() {
      return Object.hasOwn(this.mobileColumnPreferences, this.mobileColumnKey) ? this.mobileColumnPreferences[this.mobileColumnKey] : null;
    },

    mobileSetColumnPreference(patch) {
      this.mobileColumnPreferences = { ...this.mobileColumnPreferences,
        [this.mobileColumnKey]: { ...this.mobileColumnPreference(), ...patch } };
    },

    mobileColumnQuery(value) {
      this.mobileSetColumnPreference({ query: value });
      this.mobileListFilterChanged();
    },

    mobileColumnMode() {
      return this.mobileColumnPreference()?.mode || 'day';
    },

    mobileColumnModeChange(mode) {
      if (mode === this.mobileColumnMode()) return;
      const position = this.mobileCaptureList();
      this.mobileSetColumnPreference({ mode });
      if (position) this.mobileRestoreList({ ...position, filter: this.mobileListSignature() }, false);
    },

    mobileClearColumnFilters() {
      this.mobileColumnQuery('');
      this.mobileProjectFilter = '';
      this.mobileAgentFilter = '';
      this.sourceFilter = '';
      if (this.needsInputOnly) this.toggleNeedsInput();
      else this.recomputeBoard();
    },

    mobileColumnRows() {
      const key = this.mobileColumnKey;
      const col = this.mobileColumnDefinition();
      const query = this.mobileColumnPreference()?.query || '';
      const signature = this.mobileListSignature();
      const board = this._board;
      if (columnProjection?.board === board && columnProjection.col === col && columnProjection.signature === signature) return columnProjection.rows;
      const rows = col ? this.mobileMatchedRows(query, { column: key })
        .filter(ticket => this.isSecondary(ticket) || ticket.kind !== 'epic' || this.epicIsAgent(ticket)) : [];
      if (col) this._sortColumn(rows, col.statuses);
      columnProjection = { board, col, signature, rows };
      return rows;
    },

    mobileColumnTime(ticket) {
      return this.mobileColumnKey === 'human_review' ? (Date.parse(this.reviewFinishedAt(ticket)) || 0) :
        (Date.parse(ticket.finished_at) || Date.parse(ticket.updated_at) || Date.parse(ticket.created_at) || 0);
    },

    mobileColumnCount() {
      const rows = this.mobileColumnRows();
      const incomplete = this.ticketsLoadError || this.ticketSourcesFailed();
      const oldest = this.mobileColumnKey === 'human_review' && rows.reduce((min, ticket) => {
        const time = this.mobileColumnTime(ticket);
        return time > 0 ? Math.min(time, min) : min;
      }, Infinity);
      return `${rows.length} ${rows.length === 1 ? 'ticket' : 'tickets'}${incomplete ? ' in loaded tickets' : ''}` +
        (oldest && Number.isFinite(oldest) ? ' · Oldest ' + this.noteAgo(new Date(oldest).toISOString()) : '');
    },

    mobileColumnGroups() {
      if (!this.mobileColumnDefinition() || this.mobileColumnRouteError) return [];
      const mode = this.mobileColumnMode();
      const now = new Date();
      const today = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
      const offset = days => new Date(now.getFullYear(), now.getMonth(), now.getDate() - days).getTime();
      const groups = new Map();
      for (const ticket of this.mobileColumnRows()) {
        const time = this.mobileColumnTime(ticket);
        const project = this.ticketProjectName(ticket);
        let key, label, order;
        if (mode === 'project') {
          key = 'project:' + (project || ''); label = project || 'No project'; order = 0;
        } else if (!time) {
          key = 'undated'; label = 'Undated'; order = -1;
        } else if (time >= today) {
          key = 'today'; label = 'Today'; order = today;
        } else if (time >= offset(1)) {
          key = 'yesterday'; label = 'Yesterday'; order = offset(1);
        } else if (time >= offset(6)) {
          key = 'week'; label = '2–6 days ago'; order = offset(6);
        } else if (this.mobileColumnKey === 'done') {
          const date = new Date(time);
          order = new Date(date.getFullYear(), date.getMonth(), 1).getTime();
          key = 'month:' + order;
          label = date.toLocaleDateString(undefined, { month: 'long', year: 'numeric' });
        } else {
          key = 'older'; label = 'Older'; order = 0;
        }
        if (!groups.has(key)) groups.set(key, { key, label, order, rows: [] });
        groups.get(key).rows.push(ticket);
      }
      return [...groups.values()].sort((a, b) => b.order - a.order || a.label.localeCompare(b.label)).map((group, index) => {
        const state = this.mobileColumnPreference()?.groups?.[mode]?.[group.key];
        const expanded = state?.expanded ?? index === 0;
        const size = state?.size || listPageSize;
        return { ...group, key: mode + '/' + group.key, groupKey: group.key, expanded,
          count: group.rows.length, size, rows: expanded ? group.rows.slice(0, size) : [] };
      });
    },

    mobileSetColumnGroup(group, patch) {
      const mode = this.mobileColumnMode();
      const groups = this.mobileColumnPreference()?.groups || {};
      this.mobileSetColumnPreference({ groups: { ...groups,
        [mode]: { ...groups[mode], [group.groupKey]: { expanded: group.expanded, size: group.size, ...patch } } } });
    },

    mobileShowMoreInGroup(group, event) {
      const list = event.currentTarget.closest('ul');
      const next = group.rows.length;
      this.mobileSetColumnGroup(group, { size: group.size + listPageSize });
      if (event.detail === 0) this.$nextTick(() => {
        const added = [...list.querySelectorAll('[data-mobile-ticket-key]')].slice(next);
        (added.find(focusAvailable) || list.previousElementSibling?.querySelector('button'))?.focus();
      });
    },

    mobileHomeLinks() {
      const { byColumn, epics } = this.mobileTicketList();
      return this.columns.filter(col => this.mobileHomeFiltered() || !['in_progress', 'open'].includes(col.key))
        .sort((a, b) => Number(!['human_review', 'done', 'cancelled'].includes(a.key)) - Number(!['human_review', 'done', 'cancelled'].includes(b.key)))
        .map(col => ({ key: col.key, label: col.label, count: byColumn.get(col.key)?.length || 0 }))
        .concat({ key: ':epics', label: 'Epics', count: epics.length }, { key: ':archive', label: 'Archive' });
    },

    mobileHomeLink(item) {
      if (item.key === ':epics') return this.mobileJumpTo('epics');
      if (item.key === ':archive') return this.mobileNavigate('archive');
      return this.openMobileColumn(item.key, false, item.key + '-list');
    },

    mobileListIcon(key) {
      const paths = {
        in_progress: '<path d="m9 5 10 7-10 7z"/>',
        input: '<path d="M20 11a8 8 0 0 1-8 8H5l1.3-3.5A8 8 0 1 1 20 11Z"/>',
        open: '<circle cx="12" cy="12" r="6"/>',
        human_review: '<path d="M2 12s4-6 10-6 10 6 10 6-4 6-10 6-10-6-10-6Z"/><circle cx="12" cy="12" r="2"/>',
        done: '<path d="m5 12 4 4 10-10"/>',
        cancelled: '<path d="m6 6 12 12M18 6 6 18"/>',
        ':epics': '<rect x="4" y="8" width="16" height="12" rx="3"/><path d="M8 4h8"/>',
        ':archive': '<rect x="3" y="4" width="18" height="5" rx="1"/><path d="M5 9v11h14V9M10 13h4"/>',
        board: '<rect x="3" y="4" width="7" height="16" rx="2"/><rect x="14" y="4" width="7" height="10" rx="2"/>',
        settings: '<path d="M3 7h5m4 0h9M3 17h10m4 0h4"/><circle cx="10" cy="7" r="2"/><circle cx="15" cy="17" r="2"/>',
        browse: '<circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/>',
      };
      const icon = Object.hasOwn(paths, key) ? paths[key] : paths.open;
      return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${icon}</svg>`;
    },

    mobileHomeTiles() {
      const { progress, waiting, open, byColumn } = this.mobileTicketList();
      return [
        { key: 'in_progress', label: 'In progress', count: progress.length },
        { key: 'input', label: 'Needs input', count: waiting.length },
        { key: 'open', label: 'Open', count: open.length },
        { key: 'human_review', label: 'Human review', count: byColumn.get('human_review')?.length || 0 },
      ];
    },

    mobileHomeTile(tile) {
      if (tile.key === 'human_review') return this.openMobileColumn(tile.key, false, tile.key);
      if (tile.key === 'input') {
        const { waiting } = this.mobileTicketList();
        if (waiting.length === 1) return this.openMobileTicket(waiting[0]);
        return this.toggleNeedsInput();
      }
      this.mobileJumpTo(tile.key);
    },

    mobileJumpTo(key) {
      const heading = this.$refs.mobileCardList?.querySelector('#mobile-section-' + key);
      heading?.scrollIntoView({ block: 'start' });
      heading?.focus({ preventScroll: true });
    },

    mobileRowMeta(ticket) {
      return [this.ticketProjectName(ticket), this.showAgentMeta && ticket.agent, this.isSecondary(ticket) && this.sourceLabel(ticket)].filter(Boolean).join(' · ');
    },

    mobileStageAge(ticket) {
      return [ticket?.stage, ticket?.started_at && this.noteAgo(ticket.started_at)].filter(Boolean).join(' · ');
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
      this.mobileClearGesture();
      this.mobilePruneSelection();
    },

    mobileListSignature() {
      return this.mobileColumnKey ? JSON.stringify([this.mobileColumnKey, this.mobileColumnPreference()?.query || '',
        this.mobileColumnMode(), this.mobileProjectFilter, this.mobileAgentFilter, this.sourceFilter, this.needsInputOnly]) : this.mobileWindowKey();
    },

    mobileAcceptDetail(ticket, kind = 'ticket') {
      if (this.isMobile) this.acceptMobileDetailRoute(ticket, kind);
    },

    mobileVisibleListControl(el, bounds) {
      if (!focusAvailable(el)) return false;
      const rect = el.getBoundingClientRect();
      return rect.bottom > bounds.top && rect.top < bounds.bottom && rect.right > bounds.left && rect.left < bounds.right;
    },

    mobileCaptureList(focusKey) {
      const list = this.$refs?.mobileCardList;
      if (!list) return null;
      const bounds = list.getBoundingClientRect();
      const rows = [...list.querySelectorAll('[data-mobile-ticket-key]')];
      const anchor = rows.find(el => this.mobileVisibleListControl(el, bounds));
      const active = document.activeElement;
      return { filter: this.mobileListSignature(), top: list.scrollTop,
        anchor: anchor?.dataset.mobileListKey || anchor?.dataset.mobileTicketKey, offset: anchor ? anchor.getBoundingClientRect().top - bounds.top : 0,
        opener: (list.contains(active) && active.dataset.mobileListKey) || focusKey || (list.contains(active) && active.dataset.mobileTicketKey) || '' };
    },

    restoreMobileList() {
      return restoringList;
    },

    mobileRestoreList(position, focus = true) {
      if (!this.isMobile) return;
      const sequence = ++listRestoreSequence;
      const column = this.mobileColumnKey;
      restoringList = true;
      // An SSE update can invalidate the opener before the pending board render.
      if (this._boardDirty) this.flushTicketUpdates();
      this.$nextTick(() => {
        if (sequence !== listRestoreSequence) return;
        restoringList = false;
        if (this._appDestroyed || !this.isMobile || this.currentView !== 'board' || this.mobileColumnKey !== column ||
            this.selectedTicket || this.selectedEpic || blockingInteraction(this)) return;
        const list = this.$refs?.mobileCardList;
        if (!list) return;
        const bounds = list.getBoundingClientRect();
        const rows = [...list.querySelectorAll('[data-mobile-ticket-key]')];
        if (!position || position.filter !== this.mobileListSignature()) list.scrollTop = 0;
        else {
          const anchor = rows.find(el => (el.dataset.mobileListKey || el.dataset.mobileTicketKey) === position.anchor && focusAvailable(el));
          list.scrollTop = anchor ? list.scrollTop + anchor.getBoundingClientRect().top - bounds.top - position.offset : position.top;
        }
        if (!focus) return;
        const controls = rows.concat([...list.querySelectorAll('[data-mobile-list-key]')]);
        const opener = position?.opener && controls.find(el => (el.dataset.mobileListKey || el.dataset.mobileTicketKey) === position.opener &&
          this.mobileVisibleListControl(el, bounds));
        const heading = list.querySelector('[data-mobile-list-heading]');
        const fallback = [heading, list.querySelector('[data-mobile-column-back]')]
          .find(el => this.mobileVisibleListControl(el, bounds)) ||
          [...list.querySelectorAll('button, input, select')].find(el => this.mobileVisibleListControl(el, bounds));
        (opener || fallback || list).focus({ preventScroll: true });
      });
    },

    mobileClearGesture() {
      rowGesture = null;
      this.mobileRevealedRow = null;
    },

    mobileRowPointerDown(event, ticket) {
      rowGesture = null;
      consumedClick = null;
      if (this.mobileSelecting || this.sheet || event.isPrimary === false || event.button !== 0 ||
          event.clientX < 24 || event.clientX > window.innerWidth - 24 || !event.target.closest('.mobile-ios-row-open') ||
          this.lifecycleBusy(ticket)) return;
      const key = this.ticketKey(ticket);
      const queue = this.lifecycleCanQueueSaved(key) && this.lifecycleCommand(key, 'queue-saved');
      const review = this.mobileReviewCommands(key);
      if (!queue && !review.length) return;
      if (this.mobileRevealedRow?.key !== key) this.mobileRevealedRow = null;
      rowGesture = { key, queue, review, pointer: event.pointerId, row: event.currentTarget, x: event.clientX, y: event.clientY, locked: false };
    },

    mobileRowPointerMove(event) {
      const gesture = rowGesture;
      if (!gesture || gesture.pointer !== event.pointerId || gesture.row !== event.currentTarget || gesture.locked) return;
      const dx = Math.abs(event.clientX - gesture.x);
      const dy = Math.abs(event.clientY - gesture.y);
      if (dy >= 12 && dy >= dx) rowGesture = null;
      else if (dx >= 12 && dx > dy * 1.35) {
        gesture.locked = true;
        gesture.row.setPointerCapture?.(gesture.pointer);
      }
    },

    mobileGestureCommandCurrent(key, command) {
      if (!command) return false;
      const ticket = this.lifecycleResolve(key);
      return !!ticket && !this.lifecycleBusy(key) && command.expected === this._lifecycleState(ticket) && this._lifecycleAllowed(ticket, command);
    },

    mobileRowPointerUp(event) {
      const gesture = rowGesture;
      if (!gesture || gesture.pointer !== event.pointerId || gesture.row !== event.currentTarget) return;
      rowGesture = null;
      if (!gesture.locked || !gesture.row.isConnected || this.sheet || this.mobileSelecting) return;
      consumedClick = { key: gesture.key, expires: Date.now() + 500 };
      const dx = event.clientX - gesture.x;
      if (dx >= 76 && this.mobileGestureCommandCurrent(gesture.key, gesture.queue)) {
        this.mobileQueueSaved(gesture.key);
      } else if (dx <= -36) {
        const commands = gesture.review.filter(command => this.mobileGestureCommandCurrent(gesture.key, command));
        const done = commands.find(command => command.id === 'move:done');
        if (dx <= -150 && done) this.dispatchLifecycle(gesture.key, done);
        else if (commands.length) this.mobileRevealedRow = { key: gesture.key, commands };
      }
    },

    mobileSuppressRowClick(event, ticket) {
      if (!event.detail || consumedClick?.key !== this.ticketKey(ticket) || consumedClick.expires < Date.now()) return;
      consumedClick = null;
      event.preventDefault();
      event.stopImmediatePropagation();
    },

    mobileSwipeActions(ticket) {
      const key = this.ticketKey(ticket);
      if (this.mobileRevealedRow?.key !== key) return [];
      return this.mobileRevealedRow.commands.filter(command => this.mobileGestureCommandCurrent(key, command));
    },

    mobileCommitSwipe(ticket, command) {
      const key = this.ticketKey(ticket);
      this.mobileClearGesture();
      if (this.mobileGestureCommandCurrent(key, command)) return this.dispatchLifecycle(key, command);
    },

    mobileResetListInteraction() {
      this.mobileSelecting = false;
      this.mobileSelection = [];
      this.mobileClearGesture();
      this.closeSheet(false);
    },

    async mobileQueueSaved(ticket) {
      if (!this.lifecycleCanQueueSaved(ticket)) return;
      const key = this.ticketKey(ticket);
      return this.dispatchLifecycle(key, this.lifecycleCommand(key, 'queue-saved'));
    },

    mobileReviewCommands(subject) {
      const ticket = this.lifecycleResolve(subject);
      if (ticket?.status !== 'human_review' || ticket.kontora !== true || this.executionActive(ticket) ||
          !this.ticketCanControl(ticket) || (ticket.kind === 'epic' && !this.epicIsAgent(ticket))) return [];
      const commands = this.lifecycleActions(ticket).filter(command => command.id === 'retry');
      const done = this.lifecycleDestinations(ticket).find(command => command.id === 'move:done');
      if (done) commands.push({ ...done, label: 'Mark done' });
      return commands.filter(command => this._lifecycleAllowed(ticket, command));
    },

    mobileSelectionAllowed(ticket) {
      if (this.mobileColumnKey === 'open') return this._lifecycleCanQueueSaved(ticket);
      return this.mobileColumnKey === 'human_review' && ticket.status === 'human_review' && ticket.kontora === true &&
        !this.executionActive(ticket) && this._lifecycleAllowed(ticket, { kind: 'move', status: 'done' });
    },

    mobileSelectionReason(ticket) {
      if (this.mobileSelectionAllowed(ticket)) return '';
      if (this.isSecondary(ticket)) return 'From another ticket folder';
      if (!this.ticketCanControl(ticket)) return 'Ticket actions unavailable';
      if (this.lifecycleNeedsReconciliation(ticket)) return 'Refresh ticket first';
      if (ticket.scheduled_at) return 'Already scheduled';
      if (!ticket.kontora) return 'Not initialized';
      if (this.executionActive(ticket)) return 'Agent running';
      return 'Unavailable for selection';
    },

    mobileEligibleSelection() {
      if (!this.mobileSelecting) return [];
      return this.mobileColumnRows().filter(ticket => this.mobileSelectionAllowed(ticket)).map(ticket => this.ticketKey(ticket));
    },

    mobileStartSelection() {
      if (!['open', 'human_review'].includes(this.mobileColumnKey)) return;
      this.mobileClearGesture();
      this.mobileSelection = [];
      this.mobileSelecting = true;
    },

    mobileEndSelection() {
      this.mobileSelecting = false;
      this.mobileSelection = [];
    },

    mobilePruneSelection() {
      if (!this.mobileSelecting || !this.mobileSelection.length) return;
      const eligible = new Set(this.mobileEligibleSelection());
      this.mobileSelection = this.mobileSelection.filter(key => eligible.has(key));
    },

    mobileAllSelected(eligible = this.mobileEligibleSelection()) {
      return eligible.length > 0 && eligible.length === this.mobileSelection.length;
    },

    mobileToggleSelectAll() {
      const eligible = this.mobileEligibleSelection();
      this.mobileSelection = this.mobileAllSelected(eligible) ? [] : eligible;
    },

    mobileToggleSelected(ticket) {
      if (!this.mobileSelectionAllowed(ticket)) return;
      const key = this.ticketKey(ticket);
      this.mobileSelection = this.mobileSelection.includes(key) ? this.mobileSelection.filter(selected => selected !== key) : [...this.mobileSelection, key];
    },

    mobileSelectionProjects() {
      const rows = this.mobileMatchedRows(this.mobileColumnPreference()?.query || '', { column: this.mobileColumnKey, ignoreProject: true })
        .filter(ticket => this.isSecondary(ticket) || ticket.kind !== 'epic' || this.epicIsAgent(ticket));
      const counts = new Map();
      for (const ticket of rows) {
        const name = this.ticketProjectName(ticket);
        if (name) counts.set(name, (counts.get(name) || 0) + 1);
      }
      return [{ name: '', label: 'All', count: rows.length }, ...[...counts].sort(([a], [b]) => a.localeCompare(b))
        .map(([name, count]) => ({ name, label: name, count }))];
    },

    mobileBulkDestinations() {
      const [first, ...rest] = this.mobileSelection;
      if (!first) return [];
      return this.lifecycleDestinations(first).filter(command => !['todo', 'cancelled', 'paused'].includes(command.status) &&
        rest.every(key => this.lifecycleDestinations(key).some(destination => destination.id === command.id)));
    },

    mobileOpenBatch(id = 'queue-saved', atTime = false) {
      this.mobilePruneSelection();
      if (!this.mobileSelection.length) return;
      this.mobileClearGesture();
      this.scheduleDraft = '';
      this.scheduleError = null;
      const items = this.mobileSelection.map(key => {
        const ticket = this.lifecycleResolve(key);
        return { key, id: ticket.id, title: ticket.title || ticket.id, pipeline: ticket.pipeline,
          command: this.lifecycleCommand(key, id), result: null };
      });
      const count = this.mobileBatchTicketCount(items);
      const title = id === 'queue-saved' ? 'Queue ' + count :
        id === 'cancel' ? 'Cancel ' + count :
          id === 'move:done' ? 'Mark ' + items.length + ' done' : (items[0].command?.label || 'Move tickets');
      this.sheet = { type: 'queue', id, atTime, title, items, phase: 'review' };
    },

    mobileBatchTicketCount(items) {
      return items.length + (items.length === 1 ? ' ticket' : ' tickets');
    },

    mobileBatchLabel(batch) {
      return batch.id === 'queue-saved' && batch.atTime ? 'Schedule ' + this.mobileBatchTicketCount(batch.items) : batch.title;
    },

    mobileBatchScheduleExpired(item) {
      return item.command.id === 'schedule' && Date.parse(item.command.body.scheduled_at) <= Date.now();
    },

    mobileBatchCanRetryCommand(item) {
      if (!item.result || ['success', 'cancelled', 'expired'].includes(item.result.outcome) ||
          (item.result.resolution && item.result.resolution !== 'retryable')) return false;
      const ticket = this.lifecycleResolve(item.key);
      return ticket && item.command.expected === this._lifecycleState(ticket) && this._lifecycleAllowed(ticket, item.command);
    },

    mobileBatchRetryAllowed(item) {
      return this.mobileBatchCanRetryCommand(item) && !this.mobileBatchScheduleExpired(item);
    },

    mobileBatchMessage(item) {
      const result = item.result;
      if (!result) return '';
      if (result.outcome === 'success') return 'Request accepted';
      if (result.resolution === 'refresh-required') return result.error;
      if (result.resolution === 'review-required') return result.error + ' Check the current ticket before making another request.';
      if (result.outcome === 'expired' || this.mobileBatchScheduleExpired(item)) return 'The pickup time passed. Choose a new time.';
      return result.error;
    },

    async mobileSubmitBatch(retryKey = '') {
      const batch = this.sheet;
      if (batch?.type !== 'queue' || batch.phase !== (retryKey ? 'results' : 'review')) return;
      let items = batch.items;
      if (retryKey) {
        items = items.filter(item => item.key === retryKey && this.mobileBatchCanRetryCommand(item));
        if (!items.length) return;
      } else if (batch.id === 'queue-saved' && batch.atTime) {
        const parsed = this.mobileScheduleParsed();
        if (!parsed.iso || Date.parse(parsed.iso) <= Date.now()) {
          this.scheduleError = parsed.error || 'The pickup time passed. Choose a new time.';
          return;
        }
        for (const item of items) {
          item.command = this.lifecycleCommand(item.key, { id: 'schedule', expected: item.command.expected,
            body: { scheduled_at: parsed.iso } });
        }
      }
      batch.phase = 'sending';
      const isOwner = () => this.isMobile && this.sheet === batch;
      try {
        for (const item of items) {
          if (!isOwner()) return;
          const result = await this.dispatchLifecycle(item.key, item.command, { isOwner, suppressToast: true });
          if (!isOwner()) return;
          item.result = result;
        }
        if (batch.items.every(item => item.result?.outcome === 'success')) {
          this.closeSheet();
          this.mobileEndSelection();
        }
      } finally { batch.phase = 'results'; }
    },

    async mobileReadBatchResult(item) {
      const batch = this.sheet;
      if (batch?.type !== 'queue' || batch.phase !== 'results' || item.result?.resolution !== 'refresh-required') return;
      batch.phase = 'refreshing';
      try {
        const resolution = await this.lifecycleReconcile(item.key, item.result.reconciliationId);
        if (this.sheet === batch) item.result = { ...item.result, resolution };
      } finally { batch.phase = 'results'; }
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
        if (this.isMobile && this.runConfirmOpen) this.closeRunConfirm(false);
        if (owner.frame !== null) return;
        owner.frame = requestAnimationFrame(() => {
          owner.frame = null;
          if (lifecycle === owner && !this._appDestroyed) this._updateMobileViewport();
        });
      };
      listen(window, 'resize', () => { this.mobileClearGesture(); update(); });
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
          if (lifecycle === owner && !this._appDestroyed) {
            if (this.sheet) this.mobileClearGesture();
            this._syncMobileSheet();
          }
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
      listProjection = null;
      columnProjection = null;
      this.mobileResetListInteraction();
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
    mobileReplyIntents: Object.create(null),
    mobileReplyMessages: Object.create(null),

    get mobileReplyMessage() {
      return this.mobileReplyMessages[this.outputSubjectKey()] || '';
    },

    mobileRunningOverview() {
      return !this.detailLoading && !!this.selectedTicket && !this.isSecondary(this.selectedTicket) &&
        this.selectedTicket.kind !== 'epic' && this.executionActive(this.selectedTicket);
    },

    mobileQuestionIdentity(ticket) {
      if (!this.needsLiveInput(ticket)) return null;
      return JSON.stringify([this.ticketKey(ticket), ticket.invocation_id || '', ticket.stage || '', ticket.attempt || 0,
        ticket.started_at || '', ticket.waiting_since || '', ticket.waiting_tool || '', ticket.waiting_question || '']);
    },

    async mobileReply(ticket) {
      const question = this.mobileQuestionIdentity(ticket);
      if (!question) return false;
      const ticketKey = this.ticketKey(ticket);
      const epic = this.epicIsAgent(ticket);
      let navigation = this._navigationSeq;
      if (ticketKey !== this.ticketKey(this.outputSubject())) {
        navigation = ++this._navigationSeq;
        if (epic) await this.openEpic(ticket.id, navigation);
        else await this.openMobileTicket(ticket, navigation);
      }
      if (navigation !== this._navigationSeq || this._appDestroyed || !this.isMobile || this.currentView !== 'board' ||
          this.ticketKey(this.outputSubject()) !== ticketKey || this.detailLoading) return false;
      const key = this.outputSubjectKey();
      if (this.mobileQuestionIdentity(this.outputSubject()) !== question) {
        this.showToast('The question changed. Check the ticket before replying.');
        return false;
      }
      const detail = this._ticketDetailSeq;
      if (this.mobileReplyIntents[key]?.sending) return false;
      this.mobileReplyIntents[key] = { key, question, navigation, detail, partial: false, sending: false };
      this.mobileReplyMessages[key] = '';
      if (epic) this.epicSwitchTab('session');
      else this.mobileSwitchTab('terminal');
      await this.$nextTick();
      let frames = 0;
      const focus = () => {
        if (navigation !== this._navigationSeq || detail !== this._ticketDetailSeq || key !== this.outputSubjectKey() ||
            this.mobileQuestionIdentity(this.outputSubject()) !== question || !this.mobileCanCompose() || blockingInteraction(this)) return;
        // Switching panels removes the Reply button and its Alpine scope.
        const composer = document.getElementById(epic ? 'mobile-epic-terminal-compose' : 'mobile-terminal-compose');
        if (focusAvailable(composer)) composer.focus({ preventScroll: true });
        if (document.activeElement !== composer && ++frames < 8) requestAnimationFrame(focus);
      };
      focus();
      return true;
    },

    async mobileSendReply(intent) {
      if (intent.sending || intent.partial) return false;
      const { key } = intent;
      const draft = this.mobileComposeDraft;
      const isCurrent = () => this.mobileReplyIntents[key] === intent && this.mobileCanCompose() &&
        this.mobileTerminalInputTarget === 'composer' && this.outputSubjectKey() === key &&
        this._navigationSeq === intent.navigation && this._ticketDetailSeq === intent.detail &&
        this.mobileQuestionIdentity(this.outputSubject()) === intent.question;
      intent.sending = true;
      try {
        const result = await this.sendGuardedTerminalCompose(draft, { subjectKey: key, isCurrent });
        if (this._appDestroyed) return result === 'sent';
        if (result === 'partial') {
          intent.partial = true;
          this.mobileReplyMessages[key] = 'Enter wasn’t sent. Your draft was kept. Check Terminal before opening Reply again.';
        } else if (result === 'blocked') {
          this.mobileReplyMessages[key] = 'Reply not sent. Check the current question before opening Reply again.';
        } else if (result === 'multiline-unavailable') {
          this.mobileReplyMessages[key] = 'No reply was sent. Use a single line or enter your reply directly in Terminal.';
        } else if (result === 'sent') {
          if (this.mobileComposeDrafts[key] === draft) {
            this.mobileComposeDrafts[key] = '';
            delete this.mobileReplyIntents[key];
          }
          this.mobileReplyMessages[key] = 'Sent to terminal';
          this.$nextTick(() => this._mobileResizeComposers());
        }
        return result === 'sent';
      } finally { intent.sending = false; }
    },

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
      if (!key) return;
      this.mobileComposeDrafts[key] = value;
      const intent = this.mobileReplyIntents[key];
      if (!value && intent && !intent.partial && !intent.sending) {
        delete this.mobileReplyIntents[key];
        this.mobileReplyMessages[key] = '';
      }
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
      const key = this.outputSubjectKey();
      const intent = this.mobileReplyIntents[key];
      if (intent) return this.mobileSendReply(intent);
      if (!this.mobileCanCompose() || this.terminalSubjectKey !== key ||
          !this.sendTerminalCompose(this.mobileComposeDraft)) return false;
      this.mobileReplyMessages[key] = '';
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
      return 'Sent as ' + isoWithOffset(parsed.at) + '. Moves to Queued when due. Automatic pickup must be on for the schedule to request a run.';
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
      const refreshRequired = this.lifecycleNeedsReconciliation(key);
      if (refreshRequired) add('Refresh ticket', 'default', () => this.lifecycleRefreshTicket(key));
      else if (this.sameTicket(t, this.selectedTicket || this.selectedEpic)) add('Refresh ticket', 'default', () => this.reloadTicketDetail());
      if (this.sameTicket(t, this.selectedTicket) && this.mobileRunningOverview()) {
        add('Navigation', 'default', () => { this.sheet = { type: 'navigation' }; });
        add('Assistant', 'default', () => this.openAssistantSheet());
      }
      if (t.kind === 'epic' && !this.sameTicket(t, this.selectedEpic) && !this.isSecondary(t)) add('Epic overview', 'default', () => this.openEpic(t.id));
      const current = this.lifecycleResolve(key) || t;
      if (this.ticketCanControl(current) && !refreshRequired) {
        this.lifecycleActions(key).forEach(addCommand);
        if (this.lifecycleDestinations(key).length) add('Set status', 'default', () => this.openMoveSheet(t));
        for (const id of ['cancel', 'skip', 'review', 'annotate', 'archive', 'delete']) {
          const command = this.lifecycleCommand(key, id);
          if (this._lifecycleAllowed(this.lifecycleResolve(key), command)) addCommand(command);
        }
        if (this.canSchedule(t)) {
          add(t.scheduled_at ? 'Reschedule…' : 'Schedule…', 'default', () => this.openScheduleSheet(t));
          if (t.scheduled_at) add('Clear schedule', 'default', () => this.clearScheduleMobile(t));
        }
      } else if (this.ticketCanClaim(t)) add('Move here', 'primary', () => this.claimTicket(t));
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
