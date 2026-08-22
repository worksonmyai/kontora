// Ticket store and app lifecycle: the columns, the initial load, the SSE
// stream that keeps them current, and the per-ticket presentation helpers.
export function kontoraTickets() {
  return {
    get columns() {
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
      return cols;
    },

    async init() {
      window.addEventListener('resize', () => {
        this.isMobile = window.innerWidth < 768;
      });
      window.addEventListener('hashchange', () => { this.applyRoute(); });
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
      this.$watch('configCache', () => this.$nextTick(() => this.recomputeBoard()));
      // Desktop and mobile are complementary x-if layers, so only one board
      // exists at a time and crossing the breakpoint rebuilds it.
      this.$watch('isMobile', () => this._onBreakpointChange());
      this._bindGlobalEvents();
      try {
        var cfgRes = await fetch('/api/config');
        if (cfgRes.status === 401) this.needsAuth = true;
        else if (cfgRes.ok) this.configCache = await cfgRes.json();
      } catch (e) {
        this.error = 'Failed to load config';
      }
      try {
        await this.fetchTasks();
      } catch (e) {
        if (!this.needsAuth) this.error = 'Failed to load tickets';
      }
      this.loading = false;
      if (this.needsAuth) {
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
      this.connectSSE();
      // Open whatever the URL addresses. Tickets are loaded by now, so a
      // #/t/<id> link can resolve against the board.
      await this.applyRoute();
      // The board DOM (column containers) is created by Alpine once loading
      // flips false; render cards into it on the next tick, then bind the one
      // delegated handler that drives card select / menu interactions.
      this.$nextTick(() => this._mountBoard());
    },

    async fetchTasks() {
      const res = await fetch('/api/tickets');
      if (res.status === 401) { this.needsAuth = true; throw new Error('unauthorized'); }
      if (!res.ok) throw new Error('Failed to fetch tickets');
      const data = await res.json();
      this.tickets = (data.tickets || []).map(t => this.boardEntry(t));
      this.recomputeBoard();
      // recomputeBoard derives runningAgents from in_progress kontora tickets;
      // prefer the daemon's authoritative running count at load.
      this.runningAgents = data.running_agents || 0;
      this.updateFavicon();
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
      var self = this;
      return list.sort((a, b) => {
        if (statuses.length > 1) {
          var ra = self._inflightRank[a.status];
          var rb = self._inflightRank[b.status];
          if (ra === undefined) ra = 99;
          if (rb === undefined) rb = 99;
          if (ra !== rb) return ra - rb;
        }
        const isReview = statuses.length === 1 && statuses[0] === 'human_review';
        let ta, tb;
        if (isReview) {
          ta = self.reviewFinishedAt(a);
          tb = self.reviewFinishedAt(b);
        } else {
          ta = a.status === 'in_progress' && a.started_at ? a.started_at : (a.created_at || '');
          tb = b.status === 'in_progress' && b.started_at ? b.started_at : (b.created_at || '');
        }
        if (ta !== tb) return ta > tb ? -1 : 1;
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
      return this.activeTab === 'session' || this.activeTab === 'activity';
    },

    // Whether the live terminal should be attached right now. The desktop
    // reads the session tab; mobile has its own tab state and must not depend
    // on a desktop-only value.
    terminalWanted() {
      return this.isMobile ? this.detailTab === 'terminal' : this.activeTab === 'session';
    },

    showTerminalTab() {
      if (!this.selectedTicket) return false;
      if (this.selectedTicket.status === 'in_progress') return true;
      if (this.selectedTicket.history?.length > 0) return true;
      return false;
    },

    terminalTabLabel() {
      if (this.selectedTicket?.status === 'in_progress') return 'terminal';
      return 'logs';
    },

    // The summary tab appears as soon as any stage has recorded a summary, so a
    // running ticket can show what the stages before the current one did.
    showSummaryTab() {
      var t = this.selectedTicket;
      if (!t) return false;
      return !!t.final_summary || !!t.summary || (t.history || []).some(function (e) { return !!e.summary; });
    },

    // Board cards render neither the body nor the notes parsed out of it, and
    // an entry holding them pins that ticket's full text for the life of the
    // tab. Every ticket stored in this.tickets goes through here; the detail
    // panel reads selectedTicket, which keeps the whole ticket.
    boardEntry(ticket) {
      var { body, notes, ...rest } = ticket;
      return rest;
    },

    applyTicketUpdate(ticket) {
      if (ticket.status === 'archived') {
        // Archived tickets are hidden from the board: drop them from client
        // state and close the detail panel if the archived ticket was selected.
        this.tickets = this.tickets.filter(t => t.id !== ticket.id);
        if (this.selectedTicket?.id === ticket.id) {
          this.closeDetail();
        }
      } else {
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
        if (kids && ticket.parent?.id === this.selectedTicket.id) {
          var ci = kids.findIndex(function (c) { return c.id === ticket.id; });
          if (ci >= 0) Object.assign(kids[ci], this.childRowFromEvent(ticket));
        }
        if (this.selectedTicket?.id === ticket.id) {
          var prevSummary = this.selectedTicket.summary;
          if (this.editing && !['open', 'todo', 'paused'].concat(this.configCache?.custom_statuses || []).includes(ticket.status)) {
            this.flushEditSave();
            this.editing = false;
            this.editingBody = false;
          }
          if (this.editing) {
            this.selectedTicket.status = ticket.status;
            this.selectedTicket.stage = ticket.stage;
            this.selectedTicket.attempt = ticket.attempt;
          } else {
            var body = this.selectedTicket.body;
            this.selectedTicket = ticket;
            if (body) this.selectedTicket.body = body;
          }
          if (this.logTabActive() && !this.showTerminalTab()) {
            this.activeTab = 'ticket';
          }
          if (this.selectedTicket.status !== 'in_progress') {
            this._stopActivityPoll();
            // The stage that was live has exited and written its sidecar, so one
            // last read swaps the partial tape for the completed transcript.
            if (this.activity?.live && this.activeTab === 'activity') {
              this._loadActivity(this.activityStage, this.activityRun, { merge: true });
            }
          }
          // A stage that finished while its live session was open has no
          // terminal left to show, so the same column becomes the transcript.
          // Nothing has fetched that transcript yet, so load it here too:
          // switchTab is bypassed, and an unloaded activity tab reads as
          // "no completed stage yet" right after a run that produced one.
          if (this.activeTab === 'session' && this.selectedTicket.status !== 'in_progress') {
            this.activeTab = 'activity';
            if (!this.activity && !this.activityLoading) {
              var latestRun = this.latestCompletedRun();
              if (latestRun) this.fetchActivity(latestRun.stage, latestRun.run);
            }
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
    },

    // Buffer SSE ticket_updated payloads and flush them on a single animation
    // frame so a burst of agent updates collapses into one apply + recompute.
    queueTicketUpdate(ticket) {
      this._pendingTicketUpdates.push(ticket);
      if (this._boardRaf !== null) return;
      this._boardRaf = requestAnimationFrame(() => this.flushTicketUpdates());
    },

    flushTicketUpdates() {
      this._boardRaf = null;
      var pending = this._pendingTicketUpdates;
      this._pendingTicketUpdates = [];
      pending.forEach(t => this.applyTicketUpdate(t));
      // recomputeBoard rebuilds the board and refreshes runningAgents +
      // _statusCounts, which updateFavicon then reads.
      this.recomputeBoard();
      this.updateFavicon();
      if (this.paletteOpen) this.recomputePalette();
    },

    connectSSE() {
      if (this._eventSource) this._eventSource.close();
      const es = new EventSource('/api/events');
      this._eventSource = es;
      es.addEventListener('ticket_updated', (e) => {
        this.queueTicketUpdate(JSON.parse(e.data));
      });
      es.addEventListener('ticket_deleted', (e) => {
        const ticket = JSON.parse(e.data);
        this.tickets = this.tickets.filter(t => t.id !== ticket.id);
        if (this.selectedTicket?.id === ticket.id) {
          this.closeDetail();
        }
        this.recomputeBoard();
        this.updateFavicon();
        if (this.paletteOpen) this.recomputePalette();
      });
      es.addEventListener('terminal_ready', (e) => {
        const ticket = JSON.parse(e.data);
        if (this.selectedTicket?.id === ticket.id && this.terminalWanted()) {
          this.reconnectTerminal();
        }
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
        // Drop any in-flight markers: if a plannotator run completes while SSE
        // is disconnected, we'll never see the finished event and the button
        // would stay disabled until a full page refresh.
        this.plannotatorInFlight = {};
        setTimeout(() => this.connectSSE(), 3000);
      };
    },

    startPlannotatorReview(ticket) {
      return this.startPlannotator(ticket, 'plannotator-review', 'Plannotator review');
    },

    startPlannotatorAnnotate(ticket) {
      return this.startPlannotator(ticket, 'plannotator-annotate', 'Ticket annotation');
    },

    async startPlannotator(ticket, endpoint, label) {
      if (!ticket) return;
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
      var h = (this.selectedTicket && this.selectedTicket.history) || [];
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
