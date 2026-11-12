// Ticket store and app lifecycle: the columns, the initial load, the SSE
// stream that keeps them current, and the per-ticket presentation helpers.

// How long the first load waits for the event stream before taking its
// snapshots anyway.
const SSE_OPEN_WAIT_MS = 1500;

export function kontoraTickets() {
  // Resolves the first load's wait for the event stream; null once released.
  let initialSnapshot = null;
  let primaryFlight = null;
  let primaryAgain = false;
  let detailFlight = null;

  return {
    get columns() {
      return this._columns || this._builtinColumns;
    },

    executionActive(ticket) {
      return !!ticket && (ticket.execution_active ?? (ticket.status === 'in_progress'));
    },

    ticketMoves(ticket) {
      var active = this.executionActive(ticket);
      var moves = this.validMoves[active ? 'in_progress' : ticket?.status] || [];
      return active ? moves : moves.filter(m => m.endpoint !== 'pause');
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
        var activeKey = this.mobileColumn()?.key;
        this._columns = cols;
        this._columnsSignature = signature;
        var active = cols.findIndex(c => c.key === activeKey);
        this.setActiveColumn(active < 0 ? this.activeColumn : active);
      }
      return this.columns;
    },

    async init() {
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
      // Selection highlight is a class toggle on the rendered card, not a
      // re-render, so changing the selected ticket doesn't rebuild a column.
      this.$watch('selectedTicket', () => this._markSelectedCard());
      // Custom statuses add columns; recompute so _board gains the new key,
      // then renderBoard (called by recomputeBoard) fills the new column DOM.
      this.$watch('configCache', () => this.$nextTick(() => {
        this.recomputeBoard();
        if (this.currentView === 'new' || this.sheet?.type === 'new') this.syncCreatePipeline();
        if (this.initModal) {
          this._initInherited = this.projectDefaultsFor(this.initForm.path, this.initForm.epicChild);
          this.onInitPipelineChange();
        }
      }));
      // Desktop and mobile are complementary x-if layers, so only one board
      // exists at a time and crossing the breakpoint rebuilds it.
      this.$watch('isMobile', () => this._onBreakpointChange());
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
      var subscribed = new Promise(resolve => { initialSnapshot = resolve; });
      this.connectSSE();
      var config = (async () => {
        try {
          var cfgRes = await fetch('/api/config');
          if (cfgRes.status === 401) this.needsAuth = true;
          else if (cfgRes.ok) this.configCache = await cfgRes.json();
        } catch (e) {
          this.error = 'Failed to load config';
        }
      })();
      await Promise.race([subscribed, new Promise(resolve => setTimeout(resolve, SSE_OPEN_WAIT_MS))]);
      initialSnapshot = null;
      try {
        await Promise.all([this.fetchTasks(), this.fetchTicketSources(), config]);
      } catch (e) {
        if (!this.needsAuth) this.error = 'Failed to load tickets';
      }
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
          try {
            const res = await fetch('/api/tickets');
            if (res.status === 401) { this.needsAuth = true; throw new Error('unauthorized'); }
            if (!res.ok) throw new Error('Failed to fetch tickets');
            const data = await res.json();
            if (this._appDestroyed) return;
            this.tickets = this._mergeSnapshot((data.tickets || []).map(t => this.boardEntry(t)), this.tickets, touched);
            this._reconcileSnapshotSelection(this.tickets, false, touched);
            this.recomputeBoard();
            // Exclude automation sessions, which have their own running count.
            this.runningAgents = Math.max(0, (data.running_agents || 0) - (data.automation_running_agents || 0));
            this.updateFavicon();
          } catch (e) {
            if (!primaryAgain || this._appDestroyed) throw e;
          } finally { this._endSnapshot(touched); }
        } while (primaryAgain && !this._appDestroyed);
      })().finally(() => { primaryFlight = null; });
      return primaryFlight;
    },

    _reconcileSnapshotSelection(tickets, secondary, touched) {
      var selected = this.selectedTicket;
      if (!selected || this.isSecondary(selected) !== secondary || touched.has(this.ticketKey(selected))) return;
      if (secondary && !this.sourceAvailable(selected)) return;
      var current = tickets.find(t => this.sameTicket(t, selected));
      if (current) this.queueTicketUpdate({ ...selected, ...current });
      else if (!this.archiveDetailOpen() && this._claimKey !== this.ticketKey(selected)) this.closeDetail();
    },

    _releaseInitialSnapshot() {
      var release = initialSnapshot;
      initialSnapshot = null;
      if (release) release();
    },

    async resyncTickets() {
      if (this.currentView === 'archive') this.archiveLoad();
      try {
        await Promise.all([this.fetchTasks(), this.fetchTicketSources()]);
        if (this._appDestroyed) return;
        if (this.selectedEpic) await this.epicFetchDetail(this.selectedEpic.id);
        else await this.refreshSelectedTicket();
      } catch (e) {
        if (!this._appDestroyed) this.error = e.message;
      }
    },

    refreshSelectedTicket() {
      var selected = this.selectedTicket;
      if (!selected || !this.sourceAvailable(selected)) return;
      var detail = this._ticketDetailSeq;
      if (detailFlight && detailFlight.detail === detail && this.sameTicket(detailFlight.selected, selected)) return detailFlight.promise;
      var flight = { selected, detail, promise: null };
      detailFlight = flight;
      flight.promise = (async () => {
        var update = this._selectedTicketUpdateSeq;
        var mine = () => !this._appDestroyed && detail === this._ticketDetailSeq &&
          this.sameTicket(this.selectedTicket, selected) && this.sourceAvailable(selected);
        while (mine()) {
          var response = await fetch(this.ticketURL(selected));
          if (!mine()) return;
          if (!response.ok) throw new Error('Failed to refresh ticket details');
          var full = await response.json();
          if (!mine()) return;
          if (update !== this._selectedTicketUpdateSeq) {
            update = this._selectedTicketUpdateSeq;
            continue;
          }
          if (!this.sameTicket(full, selected)) return;
          var previous = this.selectedTicket;
          this.applyTicketUpdate(full);
          if (!this.sameTicket(this.selectedTicket, selected)) return;
          if (!this.editing) {
            this.selectedTicket = this.isSecondary(previous) ? { ...full, revision: previous.revision } : full;
          }
          this.scheduleBoardFlush();
          return;
        }
      })().finally(() => { if (detailFlight === flight) detailFlight = null; });
      return flight.promise;
    },

    // Hand the token to the server via /?token=, which validates it, sets the
    // HttpOnly kontora_token cookie, and redirects back with the query param
    // stripped. Keeping the cookie server-set and HttpOnly means JS never holds
    // the token and it stays out of browser history.
    submitToken() {
      var t = (this.tokenInput || '').trim();
      if (!t) return;
      window.location.assign('/?token=' + encodeURIComponent(t));
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
        color = v('--surface-600'); label = counts.todo + ' queued';
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

    // The diff tab needs a branch to compare against the base. A ticket that
    // has not started one has nothing the changes endpoint could answer with.
    showDiffTab() {
      return !this.isSecondary(this.selectedTicket) && !!this.selectedTicket?.branch;
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

    applyTicketUpdate(ticket) {
      if (this.isSecondary(ticket)) { this.applySecondaryUpdate(ticket); return; }
      this._touchTicket(ticket);
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
            this.selectedTicket = this._mergeDetailProjection(this.selectedTicket, ticket);
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
          if (this.editing && !this.editorCanEdit(ticket)) {
            this.closeAgentPickerHost('edit');
            this.flushEditSave();
            this.editing = false;
            this.editingBody = false;
          }
          if (this.editing) {
            this.selectedTicket.status = ticket.status;
            this.selectedTicket.execution_active = ticket.execution_active;
            this.selectedTicket.kind = ticket.kind;
            this.selectedTicket.epic_execution = ticket.epic_execution;
            this.selectedTicket.epic_child = ticket.epic_child;
            this.selectedTicket.parent = ticket.parent;
            this.selectedTicket.stage = ticket.stage;
            this.selectedTicket.attempt = ticket.attempt;
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
          if (this.activeTab === 'summary' && !this.showSummaryTab()) {
            this.activeTab = 'ticket';
          }
          // A stage that just wrote a new summary usually also committed, so
          // the open summary tab re-reads the commit list.
          if (this.activeTab === 'summary' && ticket.summary !== prevSummary) {
            this.fetchChanges(ticket.id);
          }
        }
      }
      if (this.createForm.parent === ticket.id) this.syncCreatePipeline();
      if (previousOutput?.id === ticket.id) this.reconcileOutput(previousOutput);
    },

    queueTicketUpdate(ticket) {
      if (!this.isSecondary(ticket)) {
        if (this.sameTicket(this.selectedTicket, ticket) &&
            JSON.stringify([this.selectedTicket.status, this.executionActive(this.selectedTicket), this.selectedTicket.history || []]) !== JSON.stringify([ticket.status, this.executionActive(ticket), ticket.history || []])) {
          this._costDirtyKey = this.ticketKey(ticket);
        }
      }
      if (!this._chainDirtyKey && this._chainTouchedBy([ticket])) this._chainDirtyKey = this.ticketKey(this.selectedTicket);
      this.applyTicketUpdate(ticket);
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
      const es = new EventSource('/api/events');
      this._eventSource = es;
      es.addEventListener('open', () => {
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
        else this.resyncTickets();
      });
      es.addEventListener('automations_updated', () => { this.automationsRefresh(); });
      es.addEventListener('companion_updated', () => { this.companionRefresh(); });
      es.addEventListener('ticket_sources_changed', () => { this.fetchTicketSources(); });
      es.addEventListener('ticket_updated', (e) => {
        this.queueTicketUpdate(JSON.parse(e.data));
      });
      es.addEventListener('secondary_ticket_updated', (e) => {
        var payload = JSON.parse(e.data);
        this.queueTicketUpdate(payload.ticket || payload);
      });
      es.addEventListener('secondary_ticket_deleted', (e) => {
        var payload = JSON.parse(e.data);
        this.deleteSecondaryTicket(payload.ticket || payload);
        if (this.paletteOpen) this.recomputePalette();
      });
      es.addEventListener('ticket_deleted', (e) => {
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
      es.addEventListener('terminal_ready', (e) => {
        const ticket = JSON.parse(e.data);
        this.outputReady(ticket.id);
      });
      es.addEventListener('config_reloaded', () => {
        this._assistantRefreshConfig();
        this.companionRefresh();
        this._settingsRefreshConfigCache();
        this._settingsRefreshForm();
      });
      es.addEventListener('plannotator_started', (e) => {
        const payload = JSON.parse(e.data);
        if (payload.ticket_id) {
          // The entry a local start already made names which pass it is; the
          // event does not say, so it must not overwrite that.
          this.plannotatorInFlight[payload.ticket_id] ??= 'Plannotator';
        }
      });
      es.addEventListener('plannotator_finished', (e) => {
        const payload = JSON.parse(e.data);
        const label = (payload.ticket_id && this.plannotatorInFlight[payload.ticket_id]) || 'Plannotator';
        if (payload.ticket_id) {
          delete this.plannotatorInFlight[payload.ticket_id];
        }
        if (payload.outcome === 'error') {
          this.error = label + ' failed' + (payload.message ? ': ' + payload.message : '');
        }
      });
      es.onerror = () => {
        es.close();
        // Waiting for a stream that failed only delays the board.
        this._releaseInitialSnapshot();
        // Drop any in-flight markers: if a plannotator run completes while SSE
        // is disconnected, we'll never see the finished event and the button
        // would stay disabled until a full page refresh.
        this.plannotatorInFlight = {};
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

    async startPlannotator(ticket, endpoint, label) {
      if (!this.ticketCanControl(ticket)) return;
      const id = ticket.id;
      if (this.plannotatorInFlight[id]) return;
      // Optimistically reflect in-flight state; the SSE event confirms it. The
      // label is kept so a failure names the pass that failed.
      this.plannotatorInFlight[id] = label;
      this.error = null;
      try {
        const res = await fetch('/api/tickets/' + id + '/' + endpoint, { method: 'POST' });
        if (!res.ok) {
          delete this.plannotatorInFlight[id];
          const data = await res.json().catch(() => ({}));
          if (res.status === 409) {
            this.error = data.error || (label + ' cannot start in this state');
          } else if (res.status === 500) {
            this.error = data.error || ('Failed to start ' + label.toLowerCase());
          } else {
            this.error = data.error || (label + ' failed (' + res.status + ')');
          }
        }
      } catch (e) {
        delete this.plannotatorInFlight[id];
        this.error = label + ' failed: ' + e.message;
      }
    },

    // canAnnotateTicket reads the daemon's own answer. The rules (an open ticket,
    // no annotation run already pending) live in the daemon, so a button here can
    // never offer a pass it refuses.
    canAnnotateTicket(ticket) {
      return !!ticket?.can_annotate;
    },

    historyLabel(h) {
      if (!h) return '';
      if (h.kind !== 'annotation') return h.stage;
      return h.stage + ' · annotation (' + (h.session_reused ? 'resumed' : 'fresh') + ')';
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
      var h = this.outputSubject()?.history || [];
      var stage = this.activityStage;
      var run = this.activityRun || 0;
      for (var i = h.length - 1; i >= 0; i--) {
        if (h[i].stage === stage && (h[i].run || 0) === run) return h[i];
      }
      return null;
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
      if (info && info.stages.length) return name + '  (' + info.stages.join(' → ') + ')';
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
