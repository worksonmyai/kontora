import { buildWorkflowGraph } from './workflow_graph.js';

// Ticket store and app lifecycle: the columns, the initial load, the SSE
// stream that keeps them current, and the per-ticket presentation helpers.

// How long the first load waits for the event stream before taking its
// snapshots anyway.
const SSE_OPEN_WAIT_MS = 1500;
const TICKET_READ_TIMEOUT_MS = 10000;
const FOREGROUND_GAP_MS = 15000;

function quoteShellArg(s) {
  return "'" + s.replaceAll("'", "'\\''") + "'";
}

export function kontoraTickets() {
  // Resolves the first load's wait for the event stream; null once released.
  let initialSnapshot = null;
  let primaryFlight = null;
  let primaryAgain = false;
  let detailFlight = null;
  let detailRefreshSeq = 0;
  let refreshFlight = null;
  let refreshAgain = false;
  let streamGeneration = 0;
  let initialWaitTimer;
  let refreshListeners;
  const reads = new Set();
  const plannotatorStarts = new Map();
  let configGeneration = 0;
  let graphMemo;
  let workflowCopyTimer;

  return {
    ticketsRefreshing: false,
    ticketsLastLoaded: null,
    ticketsLoadError: null,
    ticketDetailRefreshError: null,
    workflowSelectedNode: Object.create(null),
    workflowEvidenceKey: '',
    workflowCopiedCommand: '',

    ticketRead(run, controller = new AbortController()) {
      reads.add(controller);
      let timer;
      let abort;
      const stopped = new Promise((_, reject) => {
        timer = setTimeout(() => controller.abort(new Error('Request timed out')), TICKET_READ_TIMEOUT_MS);
        abort = () => reject(controller.signal.reason);
        controller.signal.addEventListener('abort', abort, { once: true });
      });
      return Promise.race([stopped, run(controller.signal)]).finally(() => {
        controller.signal.removeEventListener('abort', abort);
        clearTimeout(timer);
        reads.delete(controller);
      });
    },

    async readTicketJSON(url, signal, message, current = () => !this._appDestroyed) {
      signal.throwIfAborted();
      const response = await fetch(url, { signal });
      signal.throwIfAborted();
      if (!response.ok) {
        if (response.status === 401 && current()) this.ticketReadUnauthorized();
        throw new Error(message);
      }
      const data = await response.json();
      signal.throwIfAborted();
      return data;
    },

    ticketReadUnauthorized() {
      this.needsAuth = true;
      clearTimeout(this._sseReconnect);
      this._eventSource?.close();
      this._eventSource = null;
      this._releaseInitialSnapshot();
      for (const controller of reads) controller.abort(new Error('Sign-in required'));
    },

    setupTicketRefresh() {
      let hiddenAt = document.hidden ? Date.now() : null;
      let refreshOnReturn = false;
      const resume = () => {
        if (document.hidden) refreshOnReturn = true;
        else {
          refreshOnReturn = false;
          void this.refreshTickets({ afterEvent: true });
        }
      };
      const visibility = () => {
        if (document.hidden) hiddenAt = Date.now();
        else {
          if (refreshOnReturn || (hiddenAt !== null && Date.now() - hiddenAt >= FOREGROUND_GAP_MS)) resume();
          hiddenAt = null;
        }
      };
      const show = event => { if (event.persisted) resume(); };
      refreshListeners = { resume, visibility, show };
      document.addEventListener?.('visibilitychange', visibility);
      window.addEventListener('pageshow', show);
      window.addEventListener('online', resume);
    },

    destroyTicketRefresh() {
      this._releaseInitialSnapshot();
      for (const controller of reads) controller.abort(new Error('App closed'));
      if (!refreshListeners) return;
      const { resume, visibility, show } = refreshListeners;
      document.removeEventListener?.('visibilitychange', visibility);
      window.removeEventListener('pageshow', show);
      window.removeEventListener('online', resume);
      refreshListeners = null;
    },

    ticketSourcesFailed() {
      return !!this.sourcesError || this.ticketSources.some(source => source.id !== 'primary' && source.available === false);
    },

    ticketBoardEmpty() {
      return this.ticketsLastLoaded !== null && this.ticketSourcesLoaded && !this.ticketsLoadError && !this.ticketSourcesFailed() && this.allBoardTickets().length === 0;
    },

    invalidateConfigReads() {
      ++configGeneration;
    },

    // Startup, dialogs and reloads share one request order. A dialog guard alone
    // cannot stop its older response from replacing a newer reload in that dialog.
    async loadConfigCache({ current = () => true, required = true, signal } = {}) {
      const generation = ++configGeneration;
      const previous = this.configCache;
      const fresh = () => !this._appDestroyed && current() && generation === configGeneration && this.configCache === previous;
      try {
        const res = await fetch('/api/config', signal ? { signal } : undefined);
        if (!fresh()) return;
        if (!res.ok) {
          if (!required) return res;
          throw new Error('Failed to load config');
        }
        const config = await res.json();
        if (fresh()) {
          this.configCache = config;
          if (this.activeTab === 'workflow' && !this.showWorkflowGraph(this.selectedTicket)) this.switchTab('ticket');
          this.applyConfigTheme(config);
        }
      } catch (e) {
        if (fresh()) throw e;
      }
    },

    async refreshConfigCache() {
      try {
        await this.loadConfigCache({ required: false });
      } catch (e) {
        // A reload failure leaves the last usable configuration in the cache.
      }
    },

    get columns() {
      return this._columns || this._builtinColumns;
    },

    executionActive(ticket) {
      return !!ticket && (ticket.execution_active ?? (ticket.status === 'in_progress'));
    },

    ticketMoves(ticket) {
      return this.lifecycleActions(ticket).concat(this.lifecycleDestinations(ticket));
    },

    _deriveColumns() {
      var cols = [...this._builtinColumns];
      var custom = this.configCache?.custom_statuses || [];
      if (custom.length > 0) {
        var doneIdx = cols.findIndex(c => c.key === 'done');
        if (doneIdx < 0) doneIdx = cols.length;
        var customCols = custom.map(s => {
          var known = this._knownCustomStatuses[s];
          return {
            key: s,
            statuses: [s],
            dropStatus: s,
            label: known?.label || s.charAt(0).toUpperCase() + s.slice(1).replace(/_/g, ' '),
            color: known?.color || 'bg-surface-600',
            tint: known?.tint || 'var(--st-review)',
            tip: 'Custom status: ' + s,
            emptyText: 'No ' + (known?.label || s).toLowerCase() + ' tickets',
            emptyIcon: known?.emptyIcon || '<circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/>',
          };
        });
        cols.splice(doneIdx, 0, ...customCols);
      }
      var known = new Set(cols.flatMap(c => c.statuses));
      var extra = new Set();
      for (var t of this.allBoardTickets()) {
        if (!known.has(t.status) && (this.isSecondary(t) || this.executionActive(t))) extra.add(t.status);
      }
      for (var status of [...extra].sort()) {
        cols.push({ key: status, statuses: [status], label: status, color: 'bg-surface-600', emptyText: 'No tickets', tip: 'Undeclared status (display only)' });
      }
      var signature = JSON.stringify(cols);
      if (signature !== this._columnsSignature) {
        this._columns = cols;
        this._columnsSignature = signature;
        if (![':all', ':attention'].includes(this.mobileStatusFilter) && !cols.some(col => col.key === this.mobileStatusFilter)) {
          this.mobileStatusFilter = ':all';
        }
      }
      return this.columns;
    },

    async init() {
      this.initTheme();
      this.initDialogs?.();
      this.initMobile();
      this.setupSettingsLifecycle?.();
      this.setupSettingsHistory?.();
      if (!this._settingsHistory) {
        this._routeListener = () => { this.applyRoute(); };
        window.addEventListener('hashchange', this._routeListener);
      }
      // Advance the reactive clock (detail panel duration, mobile cards) and
      // patch the imperatively rendered card timers in place every 30s.
      this._nowTimer = setInterval(() => { this.now = Date.now(); this._updateCardTimers(); }, 30000);
      // Recompute the board when the filter query changes. Debounced so typing
      // doesn't re-filter every column on each keystroke.
      this.$watch('searchQuery', () => this.debounceRecomputeBoard());
      // The palette model is small and fully in memory, so it rebuilds on every
      // keystroke without a debounce.
      this.$watch('paletteQuery', () => this.onPaletteQueryChanged());
      // Detail covers the board without rebuilding the column DOM.
      const dismissCoveredMenu = () => {
        if (this.selectedTicket || this.selectedEpic) this._closeCardMenu();
      };
      this.$watch('selectedTicket', () => {
        dismissCoveredMenu();
        this._markSelectedCard();
      });
      this.$watch('selectedEpic', dismissCoveredMenu);
      // Custom statuses add columns; recompute so _board gains the new key,
      // then renderBoard (called by recomputeBoard) fills the new column DOM.
      this.$watch('configCache', () => this.$nextTick(() => {
        this.recomputeBoard();
        if (this.currentView === 'new' || this.sheet?.type === 'new') this.syncCreatePipeline();
        if (this.initModal) {
          this.syncInitNotifyDefaults();
          this._initInherited = this.projectDefaultsFor(this.initForm.path, this.initForm.epicChild);
          this.onInitPipelineChange();
        }
      }));
      // Desktop and mobile are complementary x-if layers, so only one board
      // exists at a time and crossing the breakpoint rebuilds it.
      this.$watch('isMobile', () => this._onBreakpointChange());
      this.$watch('currentView', view => { if (view !== 'board') this._closeCardMenu(); });
      const syncAutomationTerminal = () => {
        if (this.currentView !== 'automations') return;
        if (this.terminalOpen && (!this.terminalWanted() || this.terminalSubjectKey !== this.outputSubjectKey())) {
          this.closeTerminal();
          this.terminalRW = false;
        }
        if (this.terminalWanted() && !this.terminalOpen) this.$nextTick(() => this.openTerminal());
      };
      this.$watch('automations', syncAutomationTerminal);
      this.$watch('automationsTab', syncAutomationTerminal);
      this.$watch('automationsEditing', syncAutomationTerminal);
      this._bindGlobalEvents();
      this.setupAgentPicker();
      // Every startup request is a round trip, which costs most on a phone
      // talking to a remote daemon, so they overlap. The ticket snapshots wait
      // for the stream to open: a snapshot taken after the subscription misses
      // no event, so the first open needs no second copy of every list. A
      // stream that does not open in time (a proxy holding its headers) must
      // not hold the board; its late open resynchronizes as any reconnect does.
      var subscribed = new Promise(resolve => {
        initialSnapshot = resolve;
        initialWaitTimer = setTimeout(() => this._releaseInitialSnapshot(), SSE_OPEN_WAIT_MS);
      });
      this.connectSSE();
      var config = (async () => {
        try {
          const res = await this.ticketRead(signal => this.loadConfigCache({ required: false, signal, current: () => !signal.aborted }));
          if (res?.status === 401) this.ticketReadUnauthorized();
        } catch (e) {
          this.error = 'Failed to load config';
        }
      })();
      await subscribed;
      if (this._appDestroyed) return;
      this.setupTicketRefresh();
      await Promise.all([this.refreshTickets(), config]);
      if (this._appDestroyed) return;
      this.loading = false;
      if (this.needsAuth) {
        // The stream answers 401 too; retrying it would only repeat that.
        clearTimeout(this._sseReconnect);
        this._eventSource?.close();
        this._eventSource = null;
        // Show the login modal instead of a board with a generic error toast.
        // A leftover ?token= in the URL means the previous attempt's token was
        // rejected (a valid one would have been consumed and stripped by the
        // server redirect), so surface that.
        this.error = null;
        if (new URLSearchParams(location.search).has('token')) {
          this.authError = 'That token was rejected. Check it and try again.';
        }
        return;
      }
      this.automationsRefresh();
      // After the auth check, so a pane on a tokenless session does not fire a
      // second 401 of its own.
      this.initAssistant();
      this.companionInit();
      // Open whatever the URL addresses. Tickets are loaded by now, so a
      // #/t/<id> link can resolve against the board.
      await this.applyRoute();
      // The board DOM (column containers) is created by Alpine once loading
      // flips false; render cards into it on the next tick, then bind the one
      // delegated handler that drives card select / menu interactions.
      this.$nextTick(() => this._mountBoard());
    },

    fetchTasks() {
      if (primaryFlight) { primaryAgain = true; return primaryFlight; }
      primaryFlight = (async () => {
        do {
          primaryAgain = false;
          var touched = this._beginSnapshot();
          const uncertainStarts = [...plannotatorStarts].filter(([, start]) => start.uncertain && !start.confirmed);
          try {
            const data = await this.ticketRead(signal => this.readTicketJSON('/api/tickets', signal, 'Failed to fetch tickets'));
            if (this._appDestroyed || this.needsAuth) return;
            const incoming = (data.tickets || []).map(t => {
              this._reconcileWorkflowReleaseReceipts?.(t);
              return this.boardEntry(this.isGraphTicket(t) ? { ...t, _graphDetailPending: true } : t);
            });
            this._protectPendingMoves?.(incoming, touched);
            this.tickets = this._mergeSnapshot(incoming, this.tickets, touched);
            this._reconcileSnapshotSelection(this.tickets, false, touched);
            this.epicAcceptListSnapshot(touched);
            // A refresh releases failed, unconfirmed starts. SSE owns confirmed
            // sessions; the daemon still rejects duplicate starts.
            for (const [id, start] of uncertainStarts) {
              if (plannotatorStarts.get(id) !== start || start.confirmed) continue;
              delete this.plannotatorInFlight[id];
              plannotatorStarts.delete(id);
            }
            this.recomputeBoard();
            // Exclude automation sessions, which have their own running count.
            this.runningAgents = Math.max(0, (data.running_agents || 0) - (data.automation_running_agents || 0));
            this.updateFavicon();
            this.ticketsLastLoaded = Date.now();
          } catch (e) {
            if (!primaryAgain || this._appDestroyed) throw e;
          } finally { this._endSnapshot(touched); }
        } while (primaryAgain && !this._appDestroyed && !this.needsAuth);
      })().finally(() => { primaryFlight = null; });
      return primaryFlight;
    },

    _reconcileSnapshotSelection(tickets, secondary, touched) {
      var selected = this.selectedTicket;
      if (!selected || this.isSecondary(selected) !== secondary || touched.has(this.ticketKey(selected))) return;
      if (secondary && !this.sourceAvailable(selected)) return;
      var current = tickets.find(t => this.sameTicket(t, selected));
      if (current) {
        const projection = { ...selected, ...current };
        if (!secondary) {
          // The list omits empty refs; a missing field clears the stored value.
          Object.assign(projection, this.liveWaitFields(current), this.changesRefs(current));
          if (this.isGraphTicket(selected) || this.isGraphTicket(current)) {
            const before = selected.graph_execution, after = current.graph_execution;
            const sameRun = before && after && before.execution_id === after.execution_id &&
              before.revision === after.revision && before.definition_fingerprint === after.definition_fingerprint;
            if (sameRun && !selected._graphDetailPending) {
              const { transitions, wait_history, previous_executions } = before;
              projection.graph_execution = { ...after, transitions, wait_history, previous_executions };
              projection.last_error = current.last_error ?? selected.last_error;
              delete projection._graphDetailPending;
            } else {
              // Thin lists omit history. Keep known artifact occurrences, but don't treat them as a complete trace.
              projection.graph_execution = after;
              projection.history = selected.history;
              projection.last_error = current.last_error;
              projection._graphDetailPending = true;
            }
            projection.graph_definition = current.graph_definition;
          }
        }
        this.queueTicketUpdate(projection, { openTerminal: false });
      }
      else if (!this.archiveDetailOpen() && this._claimKey !== this.ticketKey(selected)) this.closeDetail();
    },

    liveWaitFields(ticket) {
      const { waiting_for_input, waiting_since, waiting_tool, waiting_question } = ticket;
      return { waiting_for_input, waiting_since, waiting_tool, waiting_question };
    },

    _releaseInitialSnapshot() {
      var release = initialSnapshot;
      clearTimeout(initialWaitTimer);
      initialSnapshot = null;
      if (release) release();
    },

    resyncTickets() {
      return this.refreshTickets({ afterEvent: true });
    },

    refreshTickets({ afterEvent = false } = {}) {
      if (this._appDestroyed || this.needsAuth) return;
      if (refreshFlight) {
        refreshAgain ||= afterEvent;
        return refreshFlight;
      }
      this.ticketsRefreshing = true;
      refreshFlight = (async () => {
        let outcome;
        do {
          refreshAgain = false;
          const [primary, sources] = await Promise.allSettled([this.fetchTasks(), this.fetchTicketSources()]);
          if (this._appDestroyed || this.needsAuth) return;
          if (refreshAgain) continue;
          this.ticketsLoadError = primary.status === 'rejected' ? primary.reason.message : null;
          const detail = await this.reloadTicketDetail();
          outcome = {
            primary: { ok: !this.ticketsLoadError, error: this.ticketsLoadError },
            sources: sources.status === 'fulfilled' ? sources.value : { ok: false, error: sources.reason.message },
            detail,
          };
        } while (refreshAgain && !this._appDestroyed && !this.needsAuth);
        return outcome;
      })().finally(() => {
        refreshFlight = null;
        this.ticketsRefreshing = false;
      });
      return refreshFlight;
    },

    async reloadTicketDetail({ fresh = false } = {}) {
      if (this.currentView === 'archive') return null;
      const ticket = this.selectedTicket || this.selectedEpic;
      if (!ticket) return null;
      const navigation = this._navigationSeq;
      const seq = ++detailRefreshSeq;
      this.ticketDetailRefreshError = null;
      const failed = (message, error = message) => {
        if (!this.needsAuth && !this._appDestroyed && seq === detailRefreshSeq && navigation === this._navigationSeq &&
            this.sameTicket(ticket, this.selectedTicket || this.selectedEpic)) this.ticketDetailRefreshError = message;
        return { ok: false, error };
      };
      if (!this.sourceAvailable(ticket)) return failed('The ticket folder is unavailable. Return to Tickets and tap Refresh.');
      try {
        if (this.selectedEpic) await this.epicFetchDetail(ticket.id, { refreshing: true });
        else await this.refreshSelectedTicket({ fresh });
        return { ok: true };
      } catch (e) {
        return failed('Could not refresh this ticket.', e.message);
      }
    },

    refreshSelectedTicket({ fresh = false } = {}) {
      var selected = this.selectedTicket;
      if (!selected || !this.sourceAvailable(selected)) return;
      var detail = this._ticketDetailSeq;
      if (!fresh && detailFlight && detailFlight.detail === detail && this.sameTicket(detailFlight.selected, selected)) return detailFlight.promise;
      var flight = { selected, detail, promise: null };
      detailFlight = flight;
      flight.promise = this.ticketRead(async signal => {
        var mine = () => detailFlight === flight && !signal.aborted && !this._appDestroyed && detail === this._ticketDetailSeq &&
          this.sameTicket(this.selectedTicket, selected) && this.sourceAvailable(selected);
        const key = this.ticketKey(selected);
        if (this._editorSaves[key]?.pending) await this._waitEditorSaves(key);
        if (this._noteHasTail?.(key)) await this._noteWaitForTail(key);
        while (mine()) {
          const update = this._selectedTicketUpdateSeq;
          const ackVersion = this._editorAckVersion(key);
          const superseded = () => update !== this._selectedTicketUpdateSeq || ackVersion !== this._editorAckVersion(key);
          const full = await this.readTicketJSON(this.ticketURL(selected), signal, 'Failed to refresh ticket details',
            () => mine() && !superseded());
          if (!mine()) return;
          if (superseded()) continue;
          if (!this.sameTicket(full, selected)) throw new Error('Invalid ticket detail');
          if (this._isPreMoveSnapshot(full)) return;
          var previous = this.selectedTicket;
          this.applyTicketUpdate(full, { openTerminal: false });
          if (!this.sameTicket(this.selectedTicket, selected)) return;
          if (!this.editing) {
            this.selectedTicket = this.isSecondary(previous) && !this.detailLoading ? { ...full, revision: previous.revision } : full;
          }
          this.supersedeTicketHydration();
          this.scheduleBoardFlush();
          return;
        }
      }).finally(() => { if (detailFlight === flight) detailFlight = null; });
      return flight.promise;
    },

    // Hand the token to the server via /?token=, which validates it, sets the
    // HttpOnly kontora_token cookie, and redirects back with the query param
    // stripped. A redirect with no fragment inherits the requested fragment;
    // use the actual URL here because the initial route may not have run yet.
    submitToken() {
      var t = (this.tokenInput || '').trim();
      if (!t) return;
      window.location.assign('/?token=' + encodeURIComponent(t) + (location.hash || ''));
    },

    _cssVar(name, styles) {
      var s = styles || getComputedStyle(document.documentElement);
      return 'rgb(' + s.getPropertyValue(name).trim() + ')';
    },

    // For the --st-* status hues, which are HSL triplets (the theme vars
    // _cssVar reads are RGB triplets).
    _cssHsl(name, styles) {
      var s = styles || getComputedStyle(document.documentElement);
      return 'hsl(' + s.getPropertyValue(name).trim() + ')';
    },

    updateFavicon() {
      const counts = this._statusCounts;

      var styles = getComputedStyle(document.documentElement);
      var v = (name) => this._cssVar(name, styles);
      var h = (name) => this._cssHsl(name, styles);
      let color, label;
      // A waiting agent outranks a running one: it is the only state that
      // needs the person at the keyboard, and an unfocused tab is where they
      // will see it.
      if (counts.needsInput > 0) {
        color = h('--st-review'); label = counts.needsInput + ' needs input';
      } else if (counts.in_progress > 0) {
        color = h('--st-progress'); label = counts.in_progress + ' running';
      } else if (counts.paused > 0) {
        color = h('--st-paused'); label = counts.paused + ' paused';
      } else if (counts.todo > 0) {
        color = v('--surface-600'); label = counts.todo + ' Todo';
      } else if (counts.done > 0) {
        color = h('--st-done'); label = 'all done';
      } else {
        color = v('--surface-600'); label = null;
      }

      document.title = label ? '(' + label + ') kontora' : 'kontora';
      const icon = document.querySelector('link[rel="icon"]');
      if (icon) icon.href = "data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><circle cx='8' cy='8' r='7' fill='" + encodeURIComponent(color) + "'/></svg>";
    },

    // Finish time of a ticket that waits in HUMAN REVIEW: finished_at, when the
    // last stage run completed. The daemon derives it in every projection, so a
    // board card and an SSE update agree, and it stays put when the file is
    // edited later. A manual "send to review" runs no stage and has none, so
    // fall back to the file mtime and then to creation.
    // Returns '' for any other status.
    reviewFinishedAt(ticket) {
      if (!ticket || ticket.status !== 'human_review') return '';
      return ticket.finished_at || ticket.updated_at || ticket.created_at || '';
    },

    // Sort a column's ticket list in place. `statuses` is the column's status
    // list; multi-status columns (IN PROGRESS) rank by status first, the
    // human_review column sorts by finish time, others by activity age.
    _sortColumn(list, statuses) {
      var ranked = statuses.length > 1;
      var isReview = statuses.length === 1 && statuses[0] === 'human_review';
      // Rank and timestamp per ticket rather than per comparison: a column of a
      // few hundred cards runs thousands of comparisons, and every one of them
      // was doing the status lookup and the reviewFinishedAt fallback again.
      var keys = new Map();
      for (var i = 0; i < list.length; i++) {
        var t = list[i];
        var rank = 0;
        if (ranked) {
          rank = this._inflightRank[t.status];
          if (rank === undefined) rank = 99;
        }
        keys.set(t, {
          rank: rank,
          time: isReview
            ? this.reviewFinishedAt(t)
            : (this.executionActive(t) && t.started_at ? t.started_at : (t.created_at || '')),
        });
      }
      return list.sort(function (a, b) {
        var ka = keys.get(a);
        var kb = keys.get(b);
        if (ka.rank !== kb.rank) return ka.rank - kb.rank;
        if (ka.time !== kb.time) return ka.time > kb.time ? -1 : 1;
        if (a.title !== b.title) return a.title < b.title ? -1 : 1;
        if (a.id !== b.id) return a.id < b.id ? -1 : 1;
        return 0;
      });
    },

    ticketsByStatuses(statuses) {
      var list = Array.isArray(statuses) ? statuses : [statuses];
      var set = new Set(list);
      return this._sortColumn(this.tickets.filter(t => set.has(t.status)), list);
    },

    // Whether the content column is showing agent output: the live session
    // while a stage runs, or a finished stage's activity.
    logTabActive() {
      return this.outputTab() === 'session' || this.outputTab() === 'activity';
    },

    // Whether the live terminal should be attached right now. The desktop
    // reads the session tab; mobile has its own tab state and must not depend
    // on a desktop-only value.
    terminalWanted() {
      return ['board', 'automations'].includes(this.currentView) && this.executionActive(this.outputSubject()) && this.outputTab() === 'session';
    },

    showTerminalTab() {
      if (!this.selectedTicket || this.isSecondary(this.selectedTicket)) return false;
      if (this.executionActive(this.selectedTicket)) return true;
      if (this.selectedTicket.history?.length > 0) return true;
      return false;
    },

    terminalTabLabel() {
      if (this.executionActive(this.selectedTicket)) return 'terminal';
      return 'logs';
    },

    // Running tickets can show summaries and outcome reasons from earlier stages.
    showSummaryTab() {
      var t = this.selectedTicket;
      if (!t) return false;
      return !!t.final_summary || !!t.summary || (t.history || []).some(function (e) { return !!(e.summary || e.outcome_reason); });
    },

    showReviewWorktree() {
      return this.selectedTicket?.status === 'human_review' && !this.isSecondary(this.selectedTicket);
    },

    showDiffTab() {
      return !this.isSecondary(this.selectedTicket) && (!!this.selectedTicket?.branch || this.showReviewWorktree());
    },

    // Board cards render neither the body nor the notes parsed out of it, and
    // an entry holding them pins that ticket's full text for the life of the
    // tab. Every ticket stored in this.tickets goes through here; the detail
    // panel reads selectedTicket, which keeps the whole ticket.
    boardEntry(ticket) {
      var { body, notes, description, description_tail, ...rest } = ticket;
      return rest;
    },

    _mergeDetailProjection(previous, update) {
      var merged = { ...update };
      ['body', 'description', 'description_tail'].forEach(key => {
        if (key in previous) merged[key] = previous[key];
      });
      // A secondary revision authorizes Claim of the displayed document, not an unseen SSE update.
      if (this.isSecondary(previous)) merged.revision = previous.revision;
      return merged;
    },

    applyTicketUpdate(ticket, outputOptions) {
      if (this.isSecondary(ticket)) { this.applySecondaryUpdate(ticket); return; }
      this._reconcileWorkflowReleaseReceipts?.(ticket);
      if (!outputOptions?.optimisticMove) this._touchTicket(ticket);
      this._noteReconcileTicket?.(ticket);
      this.refreshInitContext(ticket.id);
      var output = this.outputSubject();
      var previousOutput = output ? { ...output } : null;
      if (this.selectedEpic?.id === ticket.id) {
        this._epicUpdateSeq++;
        var detail = this._epicDetail;
        this.selectedEpic = Object.assign({}, detail, ticket, { body: detail?.body,
          description: detail?.description, description_tail: detail?.description_tail, history: ticket.history,
          last_error: ticket.last_error, last_log: ticket.last_log });
        if (this._epicDetail) this._epicDetail = this.selectedEpic;
      }
      if (this.sameTicket(this.selectedTicket, ticket)) this._selectedTicketUpdateSeq++;
      if (ticket.status === 'archived') {
        // Archived tickets are hidden from the board: drop them from client
        // state and close the detail panel if the archived ticket was selected.
        this.tickets = this.tickets.filter(t => t.id !== ticket.id);
        if (this.currentView === 'archive') this.archivePatchRow(ticket);
        if (this.sameTicket(this.selectedTicket, ticket)) {
          // The Archive view is where an archived ticket belongs, so its own
          // overlay stays open and takes the update. Any other open page is the
          // board's, which cannot hold a ticket the board no longer lists.
          if (this.archiveDetailOpen()) {
            // broadcastTicketUpdate strips the body from the event, so the copy
            // the overlay fetched has to survive the swap.
            const previousOwner = this.changesOwner(this.selectedTicket);
            this.selectedTicket = this._mergeDetailProjection(this.selectedTicket, ticket);
            this.reconcileChanges(previousOwner);
          } else {
            this.closeDetail();
          }
        }
      } else {
        // A ticket that left archived — restored here, or from another browser
        // — has to leave the archive list too.
        if (this.currentView === 'archive') this.archiveDropRow(ticket.id);
        const idx = this.tickets.findIndex(t => t.id === ticket.id);
        if (idx >= 0) {
          this.tickets[idx] = this.boardEntry(ticket);
        } else {
          this.tickets.push(this.boardEntry(ticket));
        }
        // A child finishing broadcasts the child, not the epic above it, so an
        // open tree would sit on what the detail fetch left behind. Patch the
        // one row rather than re-fetch the parent on every sub-ticket update.
        // The event carries every field the daemon built the row from, so the
        // stage position and the wall bounds are recomputed rather than left
        // stale beside a stage name that has moved on.
        var kids = this.selectedTicket?.children;
        if (kids && !this.isSecondary(this.selectedTicket) && ticket.parent?.id === this.selectedTicket.id) {
          var ci = kids.findIndex(function (c) { return c.id === ticket.id; });
          if (ci >= 0) Object.assign(kids[ci], this.childRowFromEvent(ticket));
        }
        if (this.sameTicket(this.selectedTicket, ticket)) {
          var prevSummary = this.selectedTicket.summary;
          var previousOwner = this.changesOwner(this.selectedTicket);
          if (this.editing && !this.editorCanEdit(ticket)) {
            this.closeAgentPickerHost('edit');
            this.flushEditSave();
            this.editing = false;
            this.editingBody = false;
          }
          if (this.editing) {
            Object.assign(this.selectedTicket, this.liveWaitFields(ticket), this.changesRefs(ticket));
            this.selectedTicket.status = ticket.status;
            this.selectedTicket.execution_active = ticket.execution_active;
            this.selectedTicket.kind = ticket.kind;
            this.selectedTicket.epic_execution = ticket.epic_execution;
            this.selectedTicket.epic_child = ticket.epic_child;
            this.selectedTicket.parent = ticket.parent;
            this.selectedTicket.stage = ticket.stage;
            this.selectedTicket.attempt = ticket.attempt;
            this.selectedTicket.graph_definition = ticket.graph_definition;
            this.selectedTicket.graph_execution = ticket.graph_execution;
            this.selectedTicket._graphDetailPending = ticket._graphDetailPending;
            this.selectedTicket.last_error = ticket.last_error;
            this.selectedTicket.can_control = ticket.can_control;
            this.selectedTicket.invocation_id = ticket.invocation_id;
            this.selectedTicket.started_at = ticket.started_at;
            this.selectedTicket.finished_at = ticket.finished_at;
            // A schedule set or cleared by the CLI, by another browser, or by
            // the daemon's own promotion has to reach the panel's controls even
            // while the body is being typed into. It is not an edited field.
            this.selectedTicket.scheduled_at = ticket.scheduled_at;
            // The notes tab reads the same ticket, so a note arriving from
            // `kontora note` or a daemon pause has to land even while the body
            // is being typed into. Notes are not an edited field.
            this.selectedTicket.notes = ticket.notes;
            this.selectedTicket.history = ticket.history;
          } else {
            this.selectedTicket = this._mergeDetailProjection(this.selectedTicket, ticket);
          }
          if (this.logTabActive() && !this.showTerminalTab()) {
            this.activeTab = 'ticket';
          }
          if ((this.activeTab === 'summary' && !this.showSummaryTab()) ||
              (this.activeTab === 'workflow' && !this.showWorkflowGraph(this.selectedTicket))) {
            this.activeTab = 'ticket';
          }
          // A stage that just wrote a new summary usually also committed, so
          // the open summary tab re-reads the commit list.
          this.reconcileChanges(previousOwner, this.activeTab === 'summary' && ticket.summary !== prevSummary);
        }
      }
      if (this.createForm.parent === ticket.id) this.syncCreatePipeline();
      if (previousOutput?.id === ticket.id) this.reconcileOutput(previousOutput, outputOptions);
    },

    queueTicketUpdate(ticket, outputOptions) {
      if (this.isSecondary(ticket) && !this.acceptSecondaryEvent(ticket)) return;
      if (!this.isSecondary(ticket)) {
        if (this.sameTicket(this.selectedTicket, ticket) &&
            JSON.stringify([this.selectedTicket.status, this.executionActive(this.selectedTicket), this.selectedTicket.history || []]) !== JSON.stringify([ticket.status, this.executionActive(ticket), ticket.history || []])) {
          this._costDirtyKey = this.ticketKey(ticket);
        }
      }
      if (!this._chainDirtyKey && this._chainTouchedBy([ticket])) this._chainDirtyKey = this.ticketKey(this.selectedTicket);
      this.applyTicketUpdate(ticket, outputOptions);
      this.scheduleBoardFlush();
    },

    scheduleBoardFlush() {
      this._boardDirty = true;
      if (this._boardRaf !== null || this._appDestroyed) return;
      this._boardRaf = requestAnimationFrame(() => this.flushTicketUpdates());
    },

    flushTicketUpdates() {
      if (this._boardRaf !== null) cancelAnimationFrame(this._boardRaf);
      this._boardRaf = null;
      if (this._dragging || this._appDestroyed) return;
      var key = this.ticketKey(this.selectedTicket);
      if (key && this._costDirtyKey === key && !this.isSecondary(this.selectedTicket)) this.fetchTicketCost(this.selectedTicket.id);
      if (key && this._chainDirtyKey === key && !this.isSecondary(this.selectedTicket)) this.fetchChain(this.selectedTicket.id);
      this._costDirtyKey = null;
      this._chainDirtyKey = null;
      this._boardDirty = false;
      this.recomputeBoard();
      this.updateFavicon();
      if (this.paletteOpen) this.recomputePalette();
    },

    // Whether a batch of ticket updates changes the ladder. A refetch rather
    // than a patch: one dep going done moves holds_chain, every waits_on behind
    // it and the verdict at once, which is the whole graph re-derived, and the
    // event carries nothing like enough to do that here.
    //
    // The second clause is what catches a new dependent: a ticket that adds a
    // dep on the open one becomes a downstream row, and its id is not in the
    // chain yet. The event is built with the body, so its deps are resolved refs.
    _chainTouchedBy(pending) {
      if (!this.chain || !this.selectedTicket || this.isSecondary(this.selectedTicket)) return false;
      var nodes = this.chain.nodes || [];
      var ids = new Set(nodes.map(function (n) { return n.id; }));
      return pending.some(t => {
        if (nodes.some(n => this.sameTicket(n, t))) return true;
        if (this.isSecondary(t)) return false;
        return (t.deps || []).some(function (d) { return ids.has(d.id || d); });
      });
    },

    _chainHasNode(id) {
      if (!this.chain || !this.selectedTicket || this.isSecondary(this.selectedTicket)) return false;
      return (this.chain.nodes || []).some(function (n) { return n.id === id; });
    },

    connectSSE() {
      if (this._appDestroyed || this.needsAuth) return;
      if (this._eventSource) this._eventSource.close();
      const generation = ++streamGeneration;
      const es = new EventSource('/api/events');
      this._eventSource = es;
      const on = (type, fn) => es.addEventListener(type, event => {
        if (generation === streamGeneration && !this.needsAuth && !this._appDestroyed) fn(event);
      });
      on('open', () => {
        this.companionRefresh();
        this.automationsRefresh();
        if (this.currentView === 'archive') {
          this.archiveLoad();
          if (this.archiveDetailOpen()) {
            const row = this.selectedTicket;
            this._archiveDetailId = null;
            this.archiveOpenRow(row);
          }
        }
        if (initialSnapshot) this._releaseInitialSnapshot();
        else {
          void this.resyncTickets();
          this.refreshConfigCache();
          this._settingsRefreshForm();
        }
      });
      on('automations_updated', () => { this.automationsRefresh(); });
      on('companion_updated', () => { this.companionRefresh(); });
      on('ticket_sources_changed', () => { this.resyncTickets(); });
      on('ticket_updated', (e) => {
        this.queueTicketUpdate(JSON.parse(e.data));
      });
      on('secondary_ticket_updated', (e) => {
        var payload = JSON.parse(e.data);
        this.queueTicketUpdate(payload.ticket || payload);
      });
      on('secondary_ticket_deleted', (e) => {
        var payload = JSON.parse(e.data);
        this.deleteSecondaryTicket(payload.ticket || payload);
        if (this.paletteOpen) this.recomputePalette();
      });
      on('ticket_deleted', (e) => {
        const ticket = JSON.parse(e.data);
        this._touchTicket(ticket);
        const wasSelected = this.sameTicket(this.selectedTicket, ticket);
        this.tickets = this.tickets.filter(t => t.id !== ticket.id);
        this.archiveDropRow(ticket.id);
        if (this.initModal && this.initForm.ticketId === ticket.id) this.closeInitModal();
        else this.refreshInitContext(ticket.id);
        if (this.createForm.parent === ticket.id) this.syncCreatePipeline();
        if (wasSelected) {
          this.closeDetail();
          this.error = 'Historical ticket ' + ticket.id + ' is missing. It may have been deleted.';
        } else if (this._chainHasNode(ticket.id)) {
          // A dep or a dependent of the open ticket is gone: the ladder would
          // keep drawing it, holder and resume pill included.
          this.fetchChain(this.selectedTicket.id);
        }
        this.scheduleBoardFlush();
      });
      on('terminal_ready', (e) => {
        const ticket = JSON.parse(e.data);
        this.outputReady(ticket.id);
      });
      on('config_reloaded', () => {
        this._assistantRefreshConfig();
        this.companionRefresh();
        this.refreshConfigCache();
        this._settingsRefreshForm();
      });
      on('plannotator_started', (e) => {
        const payload = JSON.parse(e.data);
        if (payload.ticket_id) {
          // The entry a local start already made names which pass it is; the
          // event does not say, so it must not overwrite that.
          const start = plannotatorStarts.get(payload.ticket_id);
          if (start) start.confirmed = true;
          if (!Object.hasOwn(this.plannotatorInFlight, payload.ticket_id)) {
            this.plannotatorInFlight = { ...this.plannotatorInFlight, [payload.ticket_id]: 'Plannotator' };
          }
        }
      });
      on('plannotator_finished', (e) => {
        const payload = JSON.parse(e.data);
        const label = Object.hasOwn(this.plannotatorInFlight, payload.ticket_id) ? this.plannotatorInFlight[payload.ticket_id] : 'Plannotator';
        if (payload.ticket_id) {
          delete this.plannotatorInFlight[payload.ticket_id];
          plannotatorStarts.delete(payload.ticket_id);
        }
        if (payload.outcome === 'error') {
          this.error = label + ' failed' + (payload.message ? ': ' + payload.message : '');
        }
      });
      es.onerror = () => {
        if (generation !== streamGeneration || this.needsAuth || this._appDestroyed) return;
        es.close();
        // Waiting for a stream that failed only delays the board.
        this._releaseInitialSnapshot();
        // Drop any in-flight markers: if a plannotator run completes while SSE
        // is disconnected, we'll never see the finished event and the button
        // would stay disabled until a full page refresh.
        this.plannotatorInFlight = {};
        plannotatorStarts.clear();
        this.companionDisconnected = true;
        this.companionError = 'Disconnected. Attention may be stale until the daemon reconnects.';
        clearTimeout(this._sseReconnect);
        this._sseReconnect = setTimeout(() => this.connectSSE(), 3000);
      };
    },

    startPlannotatorReview(ticket) {
      return this.startPlannotator(ticket, 'plannotator-review', 'Plannotator review');
    },

    startPlannotatorAnnotate(ticket) {
      return this.startPlannotator(ticket, 'plannotator-annotate', 'Ticket annotation');
    },

    startPlannotator(ticket, endpoint) {
      return this.dispatchLifecycle(this.ticketKey(ticket), this.lifecycleFromEndpoint(ticket, endpoint));
    },

    async _startPlannotatorTransport(ticket, endpoint, label) {
      if (!this.ticketCanControl(ticket)) return { outcome: 'refused' };
      const id = ticket.id;
      if (Object.hasOwn(this.plannotatorInFlight, id)) return { outcome: 'busy' };
      // Optimistically reflect in-flight state; the SSE event confirms it. The
      // label is kept so a failure names the pass that failed.
      const start = { confirmed: false, uncertain: false };
      plannotatorStarts.set(id, start);
      this.plannotatorInFlight = { ...this.plannotatorInFlight, [id]: label };
      this.error = null;
      try {
        const res = await fetch('/api/tickets/' + id + '/' + endpoint, { method: 'POST' });
        if (!res.ok) {
          if (plannotatorStarts.get(id) === start && !start.confirmed) {
            delete this.plannotatorInFlight[id];
            plannotatorStarts.delete(id);
          }
          const data = await res.json().catch(() => ({}));
          if (res.status === 409) {
            this.error = data.error || (label + ' cannot start in this state');
          } else if (res.status === 500) {
            this.error = data.error || ('Failed to start ' + label.toLowerCase());
          } else {
            this.error = data.error || (label + ' failed (' + res.status + ')');
          }
          return { outcome: 'failed', error: this.error };
        }
        return { outcome: 'success' };
      } catch (e) {
        // The host may have started the session. Refresh without replaying;
        // a confirmed SSE session must retain ownership.
        start.uncertain = true;
        const error = this.error = label + ' result uncertain: ' + e.message;
        await this._lifecycleRefresh();
        return { outcome: 'uncertain', error };
      }
    },

    // canAnnotateTicket reads the daemon's own answer. The rules (an open ticket,
    // no annotation run already pending) live in the daemon, so a button here can
    // never offer a pass it refuses.
    canAnnotateTicket(ticket) {
      return !!ticket?.can_annotate;
    },

    isGraphTicket(t) {
      return !!(t?.graph_definition || t?.graph_execution);
    },

    workflowWaiting(t) {
      return t?.status === 'paused' && !!t.graph_execution?.wait;
    },

    workflowStatusLabel(t) {
      return this.workflowWaiting(t) ? 'Waiting' : this.statusLabel(t?.status);
    },

    workflowWaitText(w, label = 'Waiting') {
      if (!w) return '';
      const lines = [label + ': ' + w.kind + ' · ' + w.id + ' · phase ' + w.phase + (w.disarmed ? ' · disarmed' : ''),
        'visit: ' + (w.visit_id || '') + ' · invocation: ' + (w.invocation_id || '') + ' · polls: ' + (w.poll_count || 0)];
      for (const key of ['until', 'deadline', 'next_poll']) {
        if (w[key] && !w[key].startsWith('0001-')) lines.push(key + ': ' + w[key]);
      }
      if (w.poll) lines.push('poll interval: ' + (w.poll / 1e9) + 's');
      for (const key of ['command', 'next', 'output', 'log_path', 'repository', 'branch', 'environment', 'required_commit']) {
        if (w[key]) lines.push(key + ': ' + w[key]);
      }
      if (w.output_truncated) lines.push('output truncated');
      if (w.receipt) lines.push('receipt: ' + w.receipt.id + ' · request ' + w.receipt.request_id + ' · ' + w.receipt.reason + ' · commit ' + (w.receipt.commit || '') + ' · accepted ' + w.receipt.accepted_at);
      return lines.join('\n');
    },

    workflowReleaseInstructions(t) {
      const x = t?.graph_execution, w = x?.wait;
      if (!this.workflowWaiting(t)) return '';
      if (w.disarmed) return 'Wait is disarmed. Inspect diagnostics, then explicitly retry to rearm; retry does not release the wait.';
      if (w.kind === 'human') {
        if (t._graphDetailPending) return 'Refresh and inspect the current wait before releasing it.';
        if (!Number.isSafeInteger(x.revision)) return 'Use the CLI to inspect the exact revision before releasing this wait.';
        if (w.phase !== 'pending' || t.invocation_id || w.invocation_id) return 'This wait is not ready for release. Inspect its diagnostics.';
        return 'kontora release ' + t.id + ' --execution-id ' + x.execution_id + ' --wait-id ' + w.id + ' --revision ' + x.revision + ' --request-id <unique-id> --reason <reason>';
      }
      if (w.kind === 'deployment') return 'Report the actual full deployed commit (required: ' + w.required_commit + '): kontora release --repo ' + quoteShellArg(w.worktree) + ' --environment ' + quoteShellArg(w.environment) + ' --rev <full-deployed-commit> --event-id <unique-id>';
      return 'The daemon resumes this visit automatically when the timer or check is satisfied, or its deadline expires.';
    },

    workflowTransitionText(edge) {
      return edge ? 'r' + edge.revision + ': ' + edge.from + '.' + edge.port + ' -> ' + edge.to : '';
    },

    workflowCursorText(t) {
      const x = t?.graph_execution;
      if (!x) return '';
      const lines = ['Current node: ' + x.current_node + (x.current_template ? ' · template ' + x.current_template : ''),
        'Execution ' + x.execution_id + ' · revision ' + x.revision + ' · visit ' + x.visit + ' · attempt ' + ((x.attempt || 0) + 1)];
      const last = x.transitions?.at(-1);
      if (last) lines.push('Latest transition: ' + this.workflowTransitionText(last));
      const result = x.last_result;
      if (result) {
        lines.push('Last result: ' + result.node_id + ' · ' + result.outcome + (result.verdict ? ' · verdict ' + result.verdict : '') + (result.exit_code !== undefined ? ' · exit ' + result.exit_code : ''));
        const reason = result.invocation_id && t.history?.find(row => row.workflow_execution_id === x.execution_id &&
          row.node_visit_id === result.visit_id && row.invocation_id === result.invocation_id)?.outcome_reason;
        if (reason) lines.push('Result reason: ' + reason);
      }
      for (const loop of x.loops || []) lines.push('Loop ' + loop.node_id + ': ' + loop.used + ' / ' + (loop.max > 0 ? loop.max : 'unknown') + (loop.exhausted ? ' · exhausted' : ''));
      return lines.join('\n');
    },

    workflowTraceText(x) {
      if (!x) return '';
      const lines = (x.transitions || []).map(e => this.workflowTransitionText(e));
      if (x.wake) lines.push('Wake: ' + x.wake.reason + ' · wait ' + x.wake.wait_id + ' · count ' + x.wake.count + (x.continuation ? ' · continuation (not a retry)' : '') + '\n' + (x.wake.output || '') + '\nNext: ' + (x.wake.next || ''));
      for (const w of x.wait_history || []) lines.push(this.workflowWaitText(w, 'Historical wait'));
      return lines.join('\n');
    },

    workflowCurrentDefinition(t) {
      const infos = this.configCache?.pipeline_infos;
      if (!t?.pipeline || this.isSecondary(t) || !Array.isArray(infos)) return t?.graph_definition;
      return infos.find(p => p.name === t.pipeline)?.graph_definition;
    },

    showWorkflowGraph(t) {
      return !!(t?.graph_execution || this.workflowCurrentDefinition(t));
    },

    workflowPortTarget(t, port) {
      const wait = t?.graph_execution?.wait;
      return this.workflowCurrentDefinition(t)?.edges?.find(e => e.from.node === wait?.node_id && e.from.port === port)?.to || '';
    },

    workflowGraph(t) {
      const definition = this.workflowCurrentDefinition(t);
      const x = t?._graphDetailPending ? null : t?.graph_execution;
      const running = !!x && this.executionActive(t);
      const cost = this.currentTicketCost();
      const releaseAvailable = !this.isMobile && !!this.workflowReleaseForm?.();
      const now = x?.wait ? this.now : null;
      const inputs = [definition, x, t?.history, running, cost, releaseAvailable, now];
      if (graphMemo && inputs.every((value, i) => value === graphMemo.inputs[i])) return graphMemo.value;
      const value = buildWorkflowGraph(definition, x, this.workflowRunRows(t, x), {
        now: now || Date.now(), running, releaseAvailable, runCost: index => this.ticketRunCost(index),
      });
      graphMemo = { inputs, value };
      return value;
    },

    workflowInspectedNode(graph) {
      const selected = this.workflowSelectedNode[this.ticketKey(this.selectedTicket)];
      return graph.nodes.find(n => n.id === selected) || graph.nodes.find(n => n.id === graph.current) ||
        graph.nodes.find(n => n.id === graph.entry) || graph.nodes[0];
    },

    workflowSelectNode(id) {
      this.workflowSelectedNode[this.ticketKey(this.selectedTicket)] = id;
    },

    workflowWaitEvidenceOpen() {
      const x = this.selectedTicket?.graph_execution;
      return !!x?.wait && this.workflowEvidenceKey === JSON.stringify([this.ticketKey(this.selectedTicket), x.execution_id, x.wait.id]);
    },

    workflowToggleWaitEvidence() {
      const x = this.selectedTicket?.graph_execution;
      if (!x?.wait || this.selectedTicket._graphDetailPending) return;
      if (this.workflowWaitEvidenceOpen()) { this.workflowEvidenceKey = ''; return; }
      this.workflowEvidenceKey = JSON.stringify([this.ticketKey(this.selectedTicket), x.execution_id, x.wait.id]);
      this.workflowSelectNode(x.wait.node_id);
      this.switchTab('workflow');
    },

    async workflowCopyReleaseCommand(command) {
      if (!command || command !== this.workflowReleaseBand()?.command) return;
      try {
        await navigator.clipboard.writeText(command);
      } catch (_) {
        this.showToast('Could not copy the release command.');
        return;
      }
      clearTimeout(workflowCopyTimer);
      this.workflowCopiedCommand = command;
      workflowCopyTimer = setTimeout(() => { this.workflowCopiedCommand = ''; }, 1400);
    },

    workflowConfigText(d) {
      if (!d) return 'No graph is configured for this pipeline.';
      const lines = ['Entry: ' + d.entry];
      for (const n of d.nodes || []) lines.push(n.id + ' [' + n.type + ']' + (n.template ? ' · template ' + n.template : '') + (n.command ? ' · ' + n.command : '') + (n.when ? ' · ' + n.when.field + ' = ' + JSON.stringify(n.when.equals) : '') + (n.type === 'loop' ? ' · max ' + n.max : '') + (n.mode ? ' · mode ' + n.mode : '') + (n.environment ? ' · environment ' + n.environment : '') + (n.timeout ? ' · timeout ' + n.timeout : '') + (n.wake ? ' · wake max ' + n.wake.max + ' · check timeout ' + n.wake.check_timeout : '') + (n.status ? ' · ' + n.status : ''));
      for (const e of d.edges || []) lines.push(e.from.node + '.' + e.from.port + ' -> ' + e.to);
      return lines.join('\n');
    },

    workflowRunRows(t, execution) {
      if (!execution?.execution_id || t?._graphDetailPending) return [];
      return this.historyArtifactRuns(t).filter(({ history }) => history.workflow_execution_id === execution.execution_id && history.node_visit_id && !history.kind);
    },

    workflowCanOpenRun(t, row) {
      return !!t && !this.isSecondary(t) && t.status !== 'archived' && !row.imported &&
        !!row.history.completed_at && !row.history.completed_at.startsWith('0001-');
    },

    openWorkflowRun(row) {
      const t = this.selectedTicket;
      if (!this.workflowCanOpenRun(t, row) || t?._graphDetailPending || t.history?.[row.historyIndex] !== row.history) return;
      const run = this.historyArtifactRun(row.historyIndex, t);
      if (run !== null) this.openActivity(row.stage, run);
    },

    canRestartGraph(ticket = this.selectedTicket) {
      return this.isGraphTicket(ticket) && this.ticketCanControl(ticket) && !this.executionActive(ticket) &&
        ticket.status !== 'archived' && !(ticket.id in (this.plannotatorInFlight || {}) && Object.hasOwn(this.plannotatorInFlight, ticket.id));
    },

    async restartGraphWorkflow() {
      const t = this.selectedTicket;
      if (!this.canRestartGraph(t)) return;
      if (!confirm('Restart at the configured graph entry with a new execution and fresh loop counters? History and worktree are retained.')) return;
      return this.dispatchLifecycle(this.ticketKey(t), 'workflow-restart');
    },

    historyLabel(h) {
      if (!h) return '';
      if (h.node_visit_id) return h.stage + (h.template ? ' · ' + h.template : '') + ' · visit ' + h.node_visit_id + ' · attempt ' + (h.visit_attempt || 1) + (h.continuation ? ' · continuation' : '') + (h.run_outcome === 'waiting' ? ' · Waiting' : '');
      if (h.kind !== 'annotation') return h.stage;
      return h.stage + ' · annotation (' + (h.session_reused ? 'resumed' : 'fresh') + ')';
    },

    historyProvenance(h) {
      const lines = [];
      if (h?.verdict) lines.push('verdict: ' + h.verdict);
      if (h?.next_step) lines.push('next: ' + h.next_step);
      if (h?.continuation) lines.push('continuation (not a retry)');
      if (h?.wake) {
        lines.push('wake: ' + h.wake.reason + ' · wait ' + h.wake.wait_id + ' · count ' + h.wake.count);
        if (h.wake.output) lines.push(h.wake.output);
        if (h.wake.receipt) lines.push('wake receipt: ' + h.wake.receipt.id + ' · ' + h.wake.receipt.reason);
      }
      if (h?.wait) lines.push(this.workflowWaitText(h.wait, 'historical wait'));
      return lines.join('\n');
    },

    // What Kontora passed the agent for a run, as one chip. Either half can be
    // absent, which means no flag was passed and the agent used its own default.
    historySettings(h) {
      if (!h) return '';
      return [h.model, h.effort].filter(Boolean).join(' · ');
    },

    // The history entry the activity tab is showing, or null when the run has
    // not finished and so has no entry yet.
    activityHistoryEntry() {
      return this.activityHistoryRecord();
    },

    // The effort chip in the activity header. Nothing on a stale payload: the
    // daemon found no sidecar for the run that was asked for and sent the
    // stage's shared log instead, so the transcript on screen is another run's
    // and this run's effort would label it wrongly.
    activityEffort() {
      if (this.activity && this.activity.stale) return '';
      var entry = this.activityHistoryEntry();
      return (entry && entry.effort) || '';
    },

    pathBasename(p) {
      if (!p) return '';
      return p.split('/').filter(Boolean).pop() || p;
    },

    pipelineLabel(name) {
      var infos = this.configCache?.pipeline_infos || [];
      var info = infos.find(i => i.name === name);
      if (info?.graph_definition) return name + '  (graph · entry ' + info.graph_definition.entry + ')';
      if (info?.stages?.length) return name + '  (' + info.stages.join(' → ') + ')';
      return name;
    },

    // Label rendered in the [PIPELINE] tag at the top of a card.
    // Prefers the pipeline name; falls back to the project basename.
    ticketTagLabel(ticket) {
      if (ticket.pipeline) return ticket.pipeline.toUpperCase();
      var b = this.pathBasename(ticket.path);
      if (b) return b.toUpperCase();
      return '—';
    },

    // Returns one of: indigo|cyan|amber|green|rose|mauve|none. Used as the
    // [data-pipe-color] attribute that drives the card left-border tint and
    // the pipeline tag chip color via --pipe-h.
    //
    // The name is hashed rather than looked up, so every install gets stable
    // colors for its own projects without this file naming any of them.
    _pipeColorPalette: ['indigo', 'cyan', 'amber', 'green', 'rose', 'mauve'],
    pipelineColorByName(name) {
      var n = (name || '').toLowerCase();
      if (!n) return 'none';
      var h = 0;
      for (var i = 0; i < n.length; i++) h = (h * 31 + n.charCodeAt(i)) | 0;
      return this._pipeColorPalette[Math.abs(h) % this._pipeColorPalette.length];
    },
    ticketPipeColor(ticket) {
      return this.pipelineColorByName(ticket.pipeline || this.pathBasename(ticket.path));
    },

    // The [tag] prefix a title writes itself, bare and without the brackets.
    splitTitleTag(title) {
      var m = /^\[([^\]]+)\]\s*(.*)$/.exec(title || '');
      return m ? { tag: m[1], rest: m[2] } : { tag: null, rest: title || '' };
    },

    // Colored mono [tag] prefix for titles: a literal "[tag] ..." title prefix
    // wins; otherwise the project basename stands in (title left untouched).
    // Both ticket reads stay here: the _cardSig scan derives the card's field
    // set from this body and does not follow splitTitleTag.
    parseTitleTag(ticket) {
      var pt = this.splitTitleTag(ticket.title);
      if (pt.tag) return pt;
      var b = this.pathBasename(ticket.path);
      return { tag: b || null, rest: ticket.title || '' };
    },
  };
}
