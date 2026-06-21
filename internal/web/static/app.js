function kontora() {
  return {
    tickets: [],
    runningAgents: 0,
    // Cache of column key -> filtered+sorted ticket list, plus the total across
    // columns. recomputeBoard() must be called at every point that mutates
    // this.tickets or searchQuery (load, batched SSE flush, optimistic move,
    // delete, detail-panel actions, debounced search), so template reads are
    // O(1) lookups instead of re-filtering and re-sorting every column on every
    // reactive read.
    _board: {},
    _boardTotal: 0,
    // Global kontora status counts from the last recomputeBoard pass, used by
    // updateFavicon. Tallied ignoring searchQuery (the favicon reflects all
    // tickets, not the filtered view).
    _statusCounts: { in_progress: 0, paused: 0, todo: 0, done: 0 },
    // Buffer of ticket_updated payloads flushed once per animation frame, so a
    // burst of agent updates triggers a single recompute and repaint.
    _pendingTicketUpdates: [],
    _boardRaf: null,
    _searchDebounce: null,
    // Reactive clock, advanced every 30s so relative durations ("Running for
    // 12m", card age timers) re-render without waiting for an SSE event.
    now: Date.now(),
    _nowTimer: null,
    selectedTicket: null,
    terminalOpen: false,
    terminalRW: false,
    terminalFullscreen: false,
    ticketFullscreen: false,
    activeTab: 'terminal',
    panelWidth: parseInt(localStorage.getItem('kontora-panel-width')) || Math.floor(window.innerWidth * 0.66),
    loading: true,
    error: null,
    // Set when the daemon answers 401: the web token gate is on and this
    // browser has no valid kontora_token cookie yet. Drives the login modal.
    needsAuth: false,
    tokenInput: '',
    authError: null,
    isMobile: window.innerWidth < 768,
    // Mobile-only UI state (phone-width layer). activeColumn is which status
    // tab the board shows; detailTab is the open ticket's content tab; sheet is
    // the bottom sheet (actions / new ticket). Desktop ignores all three.
    activeColumn: 0,
    detailTab: 'ticket',
    sheet: null,
    createSubmitting: false,
    createForm: { title: '', path: '', pipeline: '', agent: '', status: 'todo', body: '', branch: '' },
    initModal: false,
    initSubmitting: false,
    initForm: { ticketId: '', title: '', pipeline: '', agent: '', path: '' },
    actionLoading: null,
    deleteModal: false,
    detailMenuOpen: false,
    copiedId: false,
    copiedBranch: null,
    copiedCmd: null,
    configCache: null,
    logViewContent: null,
    logViewStage: null,
    logViewLoading: false,
    detailLoading: false,
    searchQuery: '',
    searchOpen: false,
    suggestions: [],
    selectedIndex: -1,
    _termWs: null,
    _term: null,
    _TerminalClass: null,
    _FitAddonClass: null,
    _Unicode11AddonClass: null,
    _WebglAddonClass: null,
    _fitAddon: null,
    _webglAddon: null,
    _terminalSeq: 0,
    _terminalOpening: false,
    _resizeObserver: null,
    _resizeTimer: null,
    _eventSource: null,
    editing: false,
    editingBody: false,
    editForm: { body: '', pipeline: '', path: '', agent: '' },
    editSubmitting: false,
    editSaved: false,
    _editDebounce: null,
    setStageOpen: false,
    deleteSubmitting: false,
    uploadDragging: false,
    lightTheme: getStoredTheme() === 'light',
    sidebarHidden: (function() { try { return localStorage.getItem('kontora-sidebar-hidden') !== '0'; } catch (e) { return true; } })(),
    currentView: 'board',
    // Map of ticketId → true while a plannotator subprocess is in flight for it.
    plannotatorInFlight: {},
    // Board cards are rendered imperatively (not via Alpine), so the open card
    // menu and the last-rendered HTML per column are plain (non-reactive) state.
    // _boardInit gates renderBoard until the column DOM exists (first paint).
    _openMenuId: null,
    _renderedHTML: {},
    _boardInit: false,

    _builtinColumns: [
      { key: 'open', statuses: ['open'], dropStatus: 'open', label: 'Open', color: 'bg-accent', tint: '227 35% 80%', tip: 'Draft ticket, not running yet. Drag to In Progress or click Initialize to start.', emptyText: 'Create a ticket to get started', glow: 'glow-top-accent',
        emptyIcon: '<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M9 15h6"/><path d="M12 18v-6"/>' },
      { key: 'in_progress', statuses: ['todo', 'in_progress', 'paused'], dropStatus: 'todo', label: 'In Progress', color: 'bg-ok', tint: '41 86% 83%', tip: 'Queued, running, or paused tickets. The daemon auto-promotes queued tickets when a worker is free.', emptyText: 'No active tickets', glow: 'glow-top-ok',
        emptyIcon: '<path d="M12 8V4H8"/><rect width="16" height="12" x="4" y="8" rx="2"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/>' },
      { key: 'human_review', statuses: ['human_review'], dropStatus: 'human_review', label: 'Human Review', color: 'bg-review', tint: '267 84% 81%', tip: 'Waiting for a human to look at the result.', emptyText: 'No tickets waiting for review', glow: 'glow-top-review',
        emptyIcon: '<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/>' },
      { key: 'done', statuses: ['done'], dropStatus: 'done', label: 'Done', color: 'bg-ok', tint: '115 54% 76%', tip: 'Ticket completed successfully.', emptyText: 'No completed tickets yet', glow: 'glow-top-ok',
        emptyIcon: '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>' },
      { key: 'cancelled', statuses: ['cancelled'], dropStatus: 'cancelled', label: 'Cancelled', color: 'bg-surface-600', tint: '228 24% 72%', tip: 'Stopped manually. Drag to In Progress to run it again.', emptyText: 'No cancelled tickets', glow: 'glow-top-muted',
        emptyIcon: '<path d="m15 9-6 6"/><path d="m9 9 6 6"/><circle cx="12" cy="12" r="10"/>' },
    ],

    // In-flight sort order: in_progress > paused > todo. Used inside the IN PROGRESS column.
    _inflightRank: { in_progress: 0, paused: 1, todo: 2 },

    // Per-status list of valid transitions surfaced in the per-card action menu.
    // `endpoint` is the URL segment appended to /api/tickets/{id}/. When it is "move",
    // `status` must be supplied and is sent as the JSON body.
    validMoves: {
      open:         [{ label: 'Queue',          endpoint: 'run' },
                     { label: 'Cancel',         endpoint: 'move', status: 'cancelled' }],
      todo:         [{ label: 'Move to open',   endpoint: 'move', status: 'open' },
                     { label: 'Cancel',         endpoint: 'move', status: 'cancelled' }],
      in_progress:  [{ label: 'Pause',          endpoint: 'pause' },
                     { label: 'Send to review', endpoint: 'move', status: 'human_review' },
                     { label: 'Mark done',      endpoint: 'move', status: 'done' },
                     { label: 'Cancel',         endpoint: 'move', status: 'cancelled' }],
      paused:       [{ label: 'Resume',         endpoint: 'retry' },
                     { label: 'Mark done',      endpoint: 'move', status: 'done' },
                     { label: 'Cancel',         endpoint: 'move', status: 'cancelled' }],
      human_review: [{ label: 'Approve',        endpoint: 'move', status: 'done' },
                     { label: 'Send back',      endpoint: 'retry' },
                     { label: 'Cancel',         endpoint: 'move', status: 'cancelled' }],
      done:         [{ label: 'Reopen',         endpoint: 'retry' },
                     { label: 'Send to review', endpoint: 'move', status: 'human_review' }],
      cancelled:    [{ label: 'Reopen',         endpoint: 'retry' }],
    },

    // Endpoints already covered by the bespoke tooltip-bearing buttons in the
    // detail panel (Pause for in_progress, Resume/retry for paused), so the
    // validMoves list rendered there doesn't duplicate them.
    _detailCoveredMoves: { in_progress: ['pause'], paused: ['retry'] },

    // validMoves entries shown as action buttons in the detail panel sidebar.
    detailMoves(ticket) {
      if (!ticket) return [];
      var covered = this._detailCoveredMoves[ticket.status] || [];
      return (this.validMoves[ticket.status] || []).filter(mv => !covered.includes(mv.endpoint));
    },

    _knownCustomStatuses: {
      review: { label: 'Review', color: 'bg-review', tint: '267 84% 81%', glow: 'glow-top-review', emptyIcon: '<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/>' },
    },

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
            tint: known?.tint || '267 84% 81%',
            tip: 'Custom status: ' + s,
            emptyText: 'No ' + (known?.label || s).toLowerCase() + ' tickets',
            glow: known?.glow || 'glow-top-muted',
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
      // Advance the reactive clock (detail panel duration, mobile cards) and
      // patch the imperatively rendered card timers in place every 30s.
      this._nowTimer = setInterval(() => { this.now = Date.now(); this._updateCardTimers(); }, 30000);
      // Recompute the board when the search query changes. Debounced so typing
      // doesn't re-filter every column on each keystroke; updateSuggestions()
      // stays on @input for the autocomplete dropdown.
      this.$watch('searchQuery', () => this.debounceRecomputeBoard());
      // Selection highlight is a class toggle on the rendered card, not a
      // re-render, so changing the selected ticket doesn't rebuild a column.
      this.$watch('selectedTicket', () => this._markSelectedCard());
      // Custom statuses add columns; recompute so _board gains the new key,
      // then renderBoard (called by recomputeBoard) fills the new column DOM.
      this.$watch('configCache', () => this.$nextTick(() => this.recomputeBoard()));
      this.$watch('terminalFullscreen', (fs) => {
        if (!this._term?.element || !this.terminalOpen) return;
        var target = fs
          ? document.getElementById('terminal-container-fullscreen')
          : document.getElementById('terminal-container');
        if (!target) return;
        target.appendChild(this._term.element);
        if (this._resizeObserver) {
          this._resizeObserver.disconnect();
          this._resizeObserver.observe(target);
        }
        var self = this;
        requestAnimationFrame(function() { self.refitTerminal(); });
      });
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
      // The board DOM (column containers) is created by Alpine once loading
      // flips false; render cards into it on the next tick, then bind the one
      // delegated handler that drives card select / menu interactions.
      this.$nextTick(() => {
        this._boardInit = true;
        this.renderBoard();
        this._bindBoardEvents();
      });
    },

    async fetchTasks() {
      const res = await fetch('/api/tickets');
      if (res.status === 401) { this.needsAuth = true; throw new Error('unauthorized'); }
      if (!res.ok) throw new Error('Failed to fetch tickets');
      const data = await res.json();
      this.tickets = data.tickets || [];
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
      if (counts.in_progress > 0) {
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

    // Sort a column's ticket list in place. `statuses` is the column's status
    // list; multi-status columns (IN PROGRESS) rank by status first, the
    // human_review column sorts by updated_at, others by activity age.
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
          ta = a.updated_at || a.created_at || '';
          tb = b.updated_at || b.created_at || '';
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
          this.tickets[idx] = ticket;
        } else {
          this.tickets.push(ticket);
        }
        if (this.selectedTicket?.id === ticket.id) {
          if (this.editing && !['open', 'todo', 'paused'].concat(this.configCache?.custom_statuses || []).includes(ticket.status)) {
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
          if (this.activeTab === 'terminal' && !this.showTerminalTab()) {
            this.activeTab = 'ticket';
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
      });
      es.addEventListener('terminal_ready', (e) => {
        const ticket = JSON.parse(e.data);
        if (this.selectedTicket?.id === ticket.id && this.activeTab === 'terminal') {
          this.reconnectTerminal();
        }
      });
      es.addEventListener('plannotator_started', (e) => {
        const payload = JSON.parse(e.data);
        if (payload.ticket_id) {
          this.plannotatorInFlight[payload.ticket_id] = true;
        }
      });
      es.addEventListener('plannotator_finished', (e) => {
        const payload = JSON.parse(e.data);
        if (payload.ticket_id) {
          delete this.plannotatorInFlight[payload.ticket_id];
        }
        if (payload.outcome === 'error') {
          this.error = 'Plannotator review failed' + (payload.message ? ': ' + payload.message : '');
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

    async startPlannotatorReview(ticket) {
      if (!ticket) return;
      const id = ticket.id;
      if (this.plannotatorInFlight[id]) return;
      // Optimistically reflect in-flight state; the SSE event confirms it.
      this.plannotatorInFlight[id] = true;
      this.error = null;
      try {
        const res = await fetch('/api/tickets/' + id + '/plannotator-review', { method: 'POST' });
        if (!res.ok) {
          delete this.plannotatorInFlight[id];
          const data = await res.json().catch(() => ({}));
          if (res.status === 409) {
            this.error = data.error || 'Plannotator review already in progress';
          } else if (res.status === 500) {
            this.error = data.error || 'Failed to start plannotator review';
          } else {
            this.error = data.error || ('Plannotator review failed (' + res.status + ')');
          }
        }
      } catch (e) {
        delete this.plannotatorInFlight[id];
        this.error = 'Plannotator review failed: ' + e.message;
      }
    },

    async openCreateModal() {
      this.createForm = { title: '', path: '', pipeline: '', agent: '', status: 'todo', body: '', branch: '' };
      this.currentView = 'new';
      this.error = null;
      if (!this.configCache) {
        try {
          const res = await fetch('/api/config');
          if (res.ok) this.configCache = await res.json();
        } catch (e) {
          this.error = 'Failed to load config';
        }
      }
    },

    closeCreateModal() {
      this.currentView = 'board';
      this.createSubmitting = false;
    },

    // Auto-pick the agent that the picked pipeline runs first, with a fallback
    // to the global default. Mirrors what the daemon would resolve on submit
    // when agent is left blank.
    onPipelineChange() {
      var name = this.createForm.pipeline;
      if (!name) return;
      var infos = this.configCache?.pipeline_infos || [];
      var info = infos.find(i => i.name === name);
      var def = (info && info.default_agent) || this.configCache?.default_agent || '';
      if (def) this.createForm.agent = def;
    },

    toggleSidebar() {
      this.sidebarHidden = !this.sidebarHidden;
      try { localStorage.setItem('kontora-sidebar-hidden', this.sidebarHidden ? '1' : '0'); } catch (e) {}
    },

    // Number of tickets currently running on a given agent. Used by the sidebar.
    agentRunningCount(agent) {
      if (!agent) return 0;
      return this.tickets.filter(t => t.kontora && t.agent === agent && t.status === 'in_progress').length;
    },

    // Live preview of the YAML frontmatter on the new-ticket page.
    // Mirrors the fields the server stores; not byte-for-byte, but close enough
    // to give a useful sense of what the markdown file will look like.
    get createPreviewYaml() {
      var f = this.createForm || {};
      var lines = ['---'];
      if (f.title)    lines.push('title: ' + JSON.stringify(f.title));
      lines.push('status: ' + (f.status || 'todo'));
      if (f.pipeline) lines.push('pipeline: ' + f.pipeline);
      if (f.agent)    lines.push('agent: ' + f.agent);
      if (f.path)     lines.push('path: ' + f.path);
      if (f.branch)   lines.push('branch: ' + f.branch);
      lines.push('---');
      if (f.title) {
        lines.push('');
        lines.push('# ' + f.title);
      }
      if (f.body) {
        lines.push('');
        lines.push(f.body);
      }
      return lines.join('\n');
    },

    async submitCreateTicket() {
      if (!this.createForm.title || !this.createForm.path) return;
      this.createSubmitting = true;
      this.error = null;
      try {
        const body = { title: this.createForm.title, path: this.createForm.path };
        if (this.createForm.pipeline) body.pipeline = this.createForm.pipeline;
        if (this.createForm.agent) body.agent = this.createForm.agent;
        if (this.createForm.status) body.status = this.createForm.status;
        if (this.createForm.body) body.body = this.createForm.body;
        if (this.createForm.branch) body.branch = this.createForm.branch;
        const res = await fetch('/api/tickets', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || 'Failed to create ticket';
          this.createSubmitting = false;
          return;
        }
        this.closeCreateModal();
      } catch (e) {
        this.error = 'Failed to create ticket: ' + e.message;
        this.createSubmitting = false;
      }
    },

    async handleUpload(fileList) {
      const mdFiles = [...fileList].filter(f => f.name.toLowerCase().endsWith('.md'));
      if (!mdFiles.length) {
        this.error = 'No .md files selected';
        return;
      }
      const form = new FormData();
      mdFiles.forEach(f => form.append('files', f));
      try {
        const res = await fetch('/api/tickets/upload', { method: 'POST', body: form });
        const data = await res.json().catch(() => ({}));
        if (!res.ok && data.error) {
          this.error = data.error;
        } else if (data.errors && data.errors.length) {
          this.error = data.errors.map(e => e.file + ': ' + e.error).join('; ');
        }
      } catch (e) {
        this.error = 'Upload failed: ' + e.message;
      }
    },

    async openInitModal(ticket) {
      this.initForm = {
        ticketId: ticket.id,
        title: ticket.title || '',
        pipeline: ticket.pipeline || '',
        agent: ticket.agent || '',
        path: ticket.path || '',
      };
      this.initModal = true;
      if (!this.configCache) {
        try {
          const res = await fetch('/api/config');
          if (res.ok) this.configCache = await res.json();
        } catch (e) {
          this.error = 'Failed to load config';
        }
      }
    },

    closeInitModal() {
      this.initModal = false;
      this.initSubmitting = false;
    },

    async submitInitTicket() {
      if (!this.initForm.path) return;
      this.initSubmitting = true;
      this.error = null;
      try {
        const res = await fetch('/api/tickets/' + this.initForm.ticketId + '/init', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ pipeline: this.initForm.pipeline, path: this.initForm.path, agent: this.initForm.agent || undefined }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || 'Failed to start ticket';
          this.initSubmitting = false;
          return;
        }
        this.closeInitModal();
      } catch (e) {
        this.error = 'Failed to start ticket: ' + e.message;
        this.initSubmitting = false;
      }
    },

    async selectTicket(ticket) {
      if (this.selectedTicket?.id === ticket.id) {
        this.closeDetail();
        return;
      }
      this.closeTerminal();
      this.terminalRW = false;
      this.logViewContent = null;
      this.logViewStage = null;
      this.logViewLoading = false;
      this.setStageOpen = false;
      this.selectedTicket = ticket;
      this.detailLoading = true;
      try {
        var res = await fetch('/api/tickets/' + ticket.id);
        if (res.ok) {
          var full = await res.json();
          this.selectedTicket = full;
          var idx = this.tickets.findIndex(function(t) { return t.id === full.id; });
          if (idx >= 0) this.tickets[idx] = full;
        }
      } catch (e) {
        this.error = 'Failed to load ticket details';
      }
      this.detailLoading = false;
      if (this.selectedTicket?.status !== 'in_progress' && this.selectedTicket?.status !== 'todo' && this.selectedTicket?.status !== 'open'
          && this.selectedTicket?.history?.length > 0) {
        var lastStage = this.selectedTicket.history[this.selectedTicket.history.length - 1].stage;
        this.fetchStageLogs(this.selectedTicket.id, lastStage);
        this.activeTab = 'terminal';
      } else if (this.selectedTicket?.status === 'in_progress') {
        this.activeTab = 'terminal';
        this.openTerminal();
      } else {
        this.activeTab = 'ticket';
        if (['open', 'todo', 'paused'].concat(this.configCache?.custom_statuses || []).includes(this.selectedTicket?.status)) this.startEditing();
      }
    },

    switchTab(tab) {
      if (tab !== 'ticket') this.ticketFullscreen = false;
      this.activeTab = tab;
      if (tab === 'terminal' && this.selectedTicket?.status === 'in_progress' && !this.terminalOpen) {
        this.openTerminal();
      } else if (tab !== 'terminal' && this.terminalOpen) {
        this.closeTerminal();
      }
      if (tab === 'ticket' && !this.editing) {
        this.startEditing();
      }
    },

    closeDetail() {
      this.terminalFullscreen = false;
      this.ticketFullscreen = false;
      this.closeTerminal();
      this.terminalRW = false;
      this.detailMenuOpen = false;
      this.deleteModal = false;
      this.editing = false;
      this.editingBody = false;
      this.deleteSubmitting = false;
      this.selectedTicket = null;
      this.activeTab = 'ticket';
      this.logViewContent = null;
      this.logViewStage = null;
      this.logViewLoading = false;
    },

    copyTicketId(id) {
      if (!id) return;
      navigator.clipboard.writeText(id);
      this.copiedId = true;
      setTimeout(() => { this.copiedId = false; }, 1200);
    },

    copyBranch(branch) {
      if (!branch) return;
      navigator.clipboard.writeText(branch);
      this.copiedBranch = branch;
      setTimeout(() => { this.copiedBranch = null; }, 1200);
    },

    copyCmd(cmd) {
      if (!cmd) return;
      navigator.clipboard.writeText(cmd);
      this.copiedCmd = cmd;
      setTimeout(() => { this.copiedCmd = null; }, 1200);
    },

    async action(type) {
      if (!this.selectedTicket || this.actionLoading) return;
      this.actionLoading = type;
      this.error = null;
      try {
        const res = await fetch('/api/tickets/' + this.selectedTicket.id + '/' + type, { method: 'POST' });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || type + ' failed';
          return;
        }
        const updated = await res.json();
        const idx = this.tickets.findIndex(t => t.id === updated.id);
        if (idx >= 0) this.tickets[idx] = updated;
        this.selectedTicket = updated;
        if (type === 'pause' || type === 'skip') this.closeTerminal();
      } catch (e) {
        this.error = type + ' failed: ' + e.message;
      } finally {
        this.actionLoading = null;
      }
    },

    openDeleteModal() {
      if (!this.selectedTicket || this.deleteSubmitting) return;
      this.deleteModal = true;
    },

    closeDeleteModal() {
      if (this.deleteSubmitting) return;
      this.deleteModal = false;
    },

    async deleteSelectedTicket() {
      if (!this.selectedTicket || this.deleteSubmitting) return;
      const ticketId = this.selectedTicket.id;
      this.deleteSubmitting = true;
      this.error = null;
      try {
        const res = await fetch('/api/tickets/' + ticketId, {
          method: 'DELETE',
          headers: { 'X-Kontora-Confirm': 'delete-ticket-file' },
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || 'Delete failed';
          return;
        }
        this.deleteModal = false;
        this.tickets = this.tickets.filter(t => t.id !== ticketId);
        if (this.selectedTicket?.id === ticketId) this.closeDetail();
        this.recomputeBoard();
        this.updateFavicon();
      } catch (e) {
        this.error = 'Delete failed: ' + e.message;
      } finally {
        this.deleteSubmitting = false;
      }
    },

    ticketActionWouldStart(endpoint, body) {
      if (endpoint === 'run' || endpoint === 'retry') return true;
      if (endpoint === 'move' && body && ['todo', 'in_progress'].includes(body.status)) return true;
      return false;
    },

    async moveTicketVia(ticketId, endpoint, body) {
      this.error = null;
      var ticket = this.tickets.find(t => t.id === ticketId);
      // Starting or resuming a visible but unmanaged ticket needs the init modal,
      // not /run, /retry, or /move, because only kontora=true tickets may execute.
      if (ticket && !ticket.kontora && this.ticketActionWouldStart(endpoint, body)) {
        this.openInitModal(ticket);
        return;
      }
      try {
        var opts = { method: 'POST' };
        if (body) {
          opts.headers = { 'Content-Type': 'application/json' };
          opts.body = JSON.stringify(body);
        }
        const res = await fetch('/api/tickets/' + ticketId + '/' + endpoint, opts);
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || (endpoint + ' failed');
          return;
        }
        const updated = await res.json().catch(() => null);
        if (updated && updated.id) {
          const idx = this.tickets.findIndex(t => t.id === updated.id);
          if (idx >= 0) this.tickets[idx] = updated;
          if (this.selectedTicket?.id === updated.id) this.selectedTicket = updated;
          this.recomputeBoard();
        }
      } catch (e) {
        this.error = endpoint + ' failed: ' + e.message;
      }
    },

    async moveTask(ticketId, newStatus) {
      this.error = null;
      const ticket = this.tickets.find(t => t.id === ticketId);
      if (ticket && !ticket.kontora && ['todo', 'in_progress'].includes(newStatus)) {
        // A drag here has already moved the DOM node into the target column;
        // rebuild from canonical data so the card snaps back if the user
        // dismisses the init modal.
        this.recomputeBoard();
        this.openInitModal(ticket);
        return;
      }
      const oldStatus = ticket ? ticket.status : null;
      if (ticket) ticket.status = newStatus;
      this.recomputeBoard();
      try {
        const res = await fetch('/api/tickets/' + ticketId + '/move', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ status: newStatus }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || 'Move failed';
          if (ticket && oldStatus) ticket.status = oldStatus;
          this.recomputeBoard();
        }
      } catch (e) {
        this.error = 'Move failed: ' + e.message;
        if (ticket && oldStatus) ticket.status = oldStatus;
        this.recomputeBoard();
      }
    },

    async startEditing() {
      const editable = ['open', 'todo', 'paused'].concat(this.configCache?.custom_statuses || []);
      if (!this.selectedTicket || !editable.includes(this.selectedTicket.status)) return;
      this.editingBody = false;
      var pipeline = this.selectedTicket.pipeline || '';
      var agent = this.selectedTicket.agent || '';
      this.editForm = {
        body: this.selectedTicket.body || '',
        pipeline: '',
        path: this.selectedTicket.path || '',
        agent: '',
        branch: this.selectedTicket.branch || '',
      };
      this.editing = true;
      this.editSaved = false;
      if (!this.configCache) {
        try {
          const res = await fetch('/api/config');
          if (res.ok) {
            this.configCache = await res.json();
          } else {
            this.error = 'Failed to load configuration';
          }
        } catch (e) {
          this.error = 'Failed to load configuration: ' + (e.message || e);
        }
      }
      // Defer select values until after x-for has created <option> elements.
      // Alpine's x-model effect on the <select> fires before x-for populates
      // options, so setting the value immediately would fail to match.
      await this.$nextTick();
      this.editForm.pipeline = pipeline;
      this.editForm.agent = agent;
    },

    async saveEdit() {
      if (!this.selectedTicket || !this.editing) return;
      this.editSubmitting = true;
      this.editSaved = false;
      try {
        const body = {};
        if (this.editForm.body !== (this.selectedTicket.body || '')) body.body = this.editForm.body;
        if (this.editForm.pipeline !== (this.selectedTicket.pipeline || '')) body.pipeline = this.editForm.pipeline;
        if (this.editForm.path !== (this.selectedTicket.path || '')) body.path = this.editForm.path;
        if (this.editForm.agent !== (this.selectedTicket.agent || '')) body.agent = this.editForm.agent;
        if (this.editForm.branch !== (this.selectedTicket.branch || '')) body.branch = this.editForm.branch;
        if (Object.keys(body).length === 0) { this.editSubmitting = false; return; }
        const res = await fetch('/api/tickets/' + this.selectedTicket.id, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        if (res.ok) {
          const updated = await res.json();
          const idx = this.tickets.findIndex(t => t.id === updated.id);
          if (idx >= 0) this.tickets[idx] = updated;
          this.selectedTicket = updated;
          this.editSaved = true;
          setTimeout(() => { this.editSaved = false; }, 1500);
        } else {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || 'Failed to save';
        }
      } catch (e) {
        this.error = 'Failed to save: ' + e.message;
      }
      this.editSubmitting = false;
    },

    debounceSaveEdit() {
      if (this._editDebounce) clearTimeout(this._editDebounce);
      this._editDebounce = setTimeout(() => this.saveEdit(), 800);
    },

    isStageClickable(stage, ticket) {
      if (!ticket || !ticket.stages || ticket.stages.length === 0) return false;
      if (ticket.status === 'todo' || ticket.status === 'open') return false;
      if (ticket.status === 'done') return true;
      var stageIdx = ticket.stages.indexOf(stage);
      var currentIdx = ticket.stages.indexOf(ticket.stage);
      if (currentIdx < 0) return false;
      return stageIdx >= 0 && stageIdx < currentIdx;
    },

    clickStage(stage) {
      if (!this.selectedTicket) return;
      if (stage === this.selectedTicket.stage && this.selectedTicket.status === 'in_progress') {
        this.logViewContent = null;
        this.logViewStage = null;
        this.openTerminal();
        return;
      }
      if (!this.isStageClickable(stage, this.selectedTicket)) return;
      this.closeTerminal();
      this.fetchStageLogs(this.selectedTicket.id, stage);
    },

    async setStage(stage) {
      if (!this.selectedTicket) return;
      this.error = null;
      try {
        const res = await fetch('/api/tickets/' + this.selectedTicket.id + '/set-stage', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ stage: stage }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || 'set-stage failed';
          return;
        }
        const updated = await res.json();
        const idx = this.tickets.findIndex(t => t.id === updated.id);
        if (idx >= 0) this.tickets[idx] = updated;
        this.selectedTicket = updated;
      } catch (e) {
        this.error = 'set-stage failed: ' + e.message;
      }
    },

    async fetchStageLogs(ticketId, stage) {
      this.logViewStage = stage;
      this.logViewLoading = true;
      this.logViewContent = null;
      try {
        var url = '/api/tickets/' + ticketId + '/logs';
        if (stage) url += '?stage=' + encodeURIComponent(stage);
        var res = await fetch(url);
        if (!res.ok) {
          var errData = await res.json().catch(function() { return {}; });
          this.error = errData.error || 'Failed to load stage logs';
          this.logViewContent = null;
          this.logViewLoading = false;
          return;
        }
        var data = await res.json();
        this.logViewContent = data.content || '';
      } catch (e) {
        this.error = 'Failed to load stage logs';
        this.logViewContent = null;
      }
      this.logViewLoading = false;
    },

    stageDotClass(i, ticket) {
      if (!ticket || !ticket.stages) return 'stage-dot-todo';
      if (ticket.status === 'done') return 'stage-dot-done';
      var currentIdx = ticket.stages.indexOf(ticket.stage);
      if (currentIdx < 0) return 'stage-dot-todo';
      if (i < currentIdx) return 'stage-dot-done';
      if (i === currentIdx) return 'stage-dot-current';
      return 'stage-dot-todo';
    },

    stageStyle(stage, ticket) {
      if (!ticket || !ticket.stages) return 'bg-surface-800 text-surface-600';
      var stageIdx = ticket.stages.indexOf(stage);
      var currentIdx = ticket.stages.indexOf(ticket.stage);
      if (ticket.status === 'done') return 'bg-ok/10 text-ok/60';
      if (stage === ticket.stage) {
        if (ticket.status === 'paused') return 'stage-paused';
        return 'stage-current';
      }
      if (currentIdx >= 0 && stageIdx >= 0 && stageIdx < currentIdx) return 'bg-ok/10 text-ok/60';
      return 'bg-surface-800 text-surface-600';
    },

    // ─── Imperative board card rendering ───
    // The desktop card list is built as HTML strings and injected with innerHTML
    // instead of an Alpine x-for, so a board with hundreds of tickets carries no
    // per-card reactive effects. One delegated handler (_bindBoardEvents) drives
    // all card interactions; selection and the open menu are class toggles.

    _escapeHtml(s) {
      if (s === null || s === undefined) return '';
      return String(s)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
    },

    // Markup for a column with no matching tickets. Keeps the .empty-state class
    // so Sortable's filter still excludes it from dragging.
    _emptyStateHTML(col) {
      return '<div class="empty-state font-mono text-center text-[12px] text-surface-600 border border-dashed border-surface-700/60 rounded-lg py-7 px-4 mt-1">'
        + this._escapeHtml('∅ ' + col.emptyText.toLowerCase()) + '</div>';
    },

    // Build one card's HTML. Every interpolated string is escaped.
    _cardHTML(ticket, col) {
      var esc = (s) => this._escapeHtml(s);
      var inProgressCol = col.key === 'in_progress';
      var selected = this.selectedTicket && this.selectedTicket.id === ticket.id;

      var cls = ['kt-card group relative rounded-lg p-3 cursor-pointer border',
                 'bg-surface-900 border-surface-700/50 hover:bg-surface-850',
                 'flex flex-col gap-2'];
      if (selected) cls.push('is-selected');
      if (!ticket.kontora) cls.push('border-dashed');
      if (ticket.status === 'cancelled') cls.push('opacity-60');
      if (ticket.status === 'in_progress') cls.push('card-state-running');
      if (ticket.status === 'paused') cls.push('card-state-paused');

      // Stage / status glyph (IN PROGRESS column only).
      var glyph = '';
      if (inProgressCol && ticket.status === 'in_progress') {
        glyph = '<span class="flex items-center gap-1 text-[11px] font-mono card-glyph-running">'
          + '<span class="pulse-dot">●</span><span>' + esc(ticket.stage) + '</span></span>';
      } else if (inProgressCol && ticket.status === 'paused') {
        glyph = '<span class="flex items-center gap-1 text-[11px] font-mono card-glyph-paused"><span>⏸</span>'
          + (ticket.stage ? '<span>' + esc(ticket.stage) + '</span>' : '') + '</span>';
      } else if (inProgressCol && ticket.status === 'todo') {
        glyph = '<span class="flex items-center gap-1 text-[11px] font-mono text-surface-600"><span>◌</span>'
          + (ticket.stage ? '<span>' + esc(ticket.stage) + '</span>' : '') + '</span>';
      }

      var notKontoraBadge = (!ticket.kontora && ticket.status !== 'open')
        ? '<span class="px-1.5 py-px rounded-full border border-warn/20 bg-warn/10 text-warn text-[10px] font-mono shrink-0">not a kontora ticket</span>'
        : '';

      // Action menu items: Initialize (non-kontora) + valid moves + fallback.
      // data-act carries the endpoint ("init" for the init modal); data-status
      // carries the target status for move actions.
      var items = '';
      if (!ticket.kontora) {
        items += '<button type="button" class="card-menu-item w-full px-3 py-2 text-left text-[12px] font-mono text-warn hover:bg-surface-800 hover:text-warn transition-colors" data-act="init">Initialize</button>';
      }
      var moves = this.validMoves[ticket.status] || [];
      moves.forEach((mv) => {
        items += '<button type="button" class="card-menu-item w-full px-3 py-2 text-left text-[12px] font-mono text-tx-3 hover:bg-surface-800 hover:text-tx-2 transition-colors" data-act="'
          + esc(mv.endpoint) + '"' + (mv.status ? ' data-status="' + esc(mv.status) + '"' : '') + '>' + esc(mv.label) + '</button>';
      });
      if (!moves.length) {
        items += '<span class="block px-3 py-2 text-[12px] font-mono text-surface-600">No actions available</span>';
      }

      // Stage progress bars (IN PROGRESS column, multi-stage pipelines only).
      var stageBars = '';
      if (inProgressCol && ticket.stages && ticket.stages.length > 1) {
        var segs = ticket.stages.map((stage, i) =>
          '<span class="' + esc(this.stageBarClass(i, ticket)) + '" title="' + esc(stage) + '"></span>').join('');
        stageBars = '<div class="stage-bars">' + segs + '</div>';
      }

      var agent = ticket.agent
        ? '<span class="flex items-center gap-1.5 min-w-0"><span class="text-surface-700">·</span>'
          + '<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 10 10" fill="none" class="shrink-0 opacity-70"><circle cx="5" cy="5" r="4" stroke="currentColor" stroke-width="1"/><circle cx="5" cy="5" r="1.6" fill="currentColor"/></svg>'
          + '<span class="truncate">' + esc(ticket.agent) + '</span></span>'
        : '';

      var retry = (ticket.attempt > 0 && ticket.status !== 'done' && ticket.status !== 'cancelled')
        ? '<span class="px-1.5 py-px rounded text-[10px] bg-err/15 text-err">' + esc('retry ' + ticket.attempt) + '</span>'
        : '';
      // data-since / data-ago let _updateCardTimers patch the text in place on
      // the 30s tick without rebuilding the card.
      var timeSpan = '';
      if (ticket.status === 'in_progress' && ticket.started_at) {
        timeSpan = '<span data-since="' + esc(ticket.started_at) + '" data-tip-t="' + esc('Started: ' + this.formatAbsDate(ticket.started_at)) + '">'
          + esc(this.formatDuration(ticket)) + '</span>';
      } else if (ticket.created_at) {
        timeSpan = '<span data-ago="' + esc(ticket.created_at) + '" data-tip-t="' + esc(this.formatAbsDate(ticket.created_at)) + '">'
          + esc(this.timeAgo(ticket.created_at)) + '</span>';
      }

      var titleCls = 'text-[13px] text-tx leading-snug' + (ticket.status === 'cancelled' ? ' line-through decoration-surface-600/60' : '');

      return '<div class="' + cls.join(' ') + '"'
        + ' data-ticket-id="' + esc(ticket.id) + '"'
        + ' data-pipe-color="' + esc(this.ticketPipeColor(ticket)) + '"'
        + ' role="listitem" tabindex="0"'
        + ' aria-label="' + esc('Ticket ' + ticket.id + ': ' + (ticket.title || '')) + '">'
        + '<div class="flex items-center justify-between gap-2">'
        +   '<div class="flex items-center gap-2 min-w-0">'
        +     '<span class="pipe-tag truncate">' + esc('[' + this.ticketTagLabel(ticket) + ']') + '</span>'
        +     notKontoraBadge + glyph
        +   '</div>'
        +   '<div class="relative flex items-center shrink-0">'
        +     '<button type="button" class="card-menu-btn w-6 h-6 rounded-md border border-surface-700/40 bg-surface-900/70 text-surface-600 hover:bg-surface-800 hover:text-tx-2 hover:border-surface-600 transition-colors flex items-center justify-center opacity-0 group-hover:opacity-100 focus:opacity-100" aria-haspopup="menu" aria-expanded="false" aria-label="More actions">'
        +       '<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="currentColor"><circle cx="5" cy="12" r="2.2"></circle><circle cx="12" cy="12" r="2.2"></circle><circle cx="19" cy="12" r="2.2"></circle></svg>'
        +     '</button>'
        +     '<div class="card-menu absolute right-0 top-7 min-w-[10rem] overflow-hidden rounded-lg border border-surface-700/60 bg-surface-900/95 shadow-lg shadow-black/30 z-20" role="menu">' + items + '</div>'
        +   '</div>'
        + '</div>'
        + '<p class="' + titleCls + '">' + esc(ticket.title) + '</p>'
        + stageBars
        + '<div class="flex items-center gap-2 text-[11px] font-mono text-surface-600 justify-between">'
        +   '<div class="flex items-center gap-1.5 min-w-0">'
        +     '<span class="group-hover:text-tx-3 transition-colors truncate">' + esc(ticket.id) + '</span>'
        +     agent
        +   '</div>'
        +   '<div class="flex items-center gap-2 shrink-0">' + retry + timeSpan + '</div>'
        + '</div>'
        + '</div>';
    },

    // Replace a single column's cards, skipping the DOM write when the rendered
    // HTML is unchanged so untouched columns keep their scroll position and any
    // open menu.
    renderColumn(key) {
      var el = document.getElementById('col-' + key);
      if (!el) return;
      var col = this.columns.find((c) => c.key === key);
      if (!col) return;
      var list = this._board[key] || [];
      var html = list.length
        ? list.map((t) => this._cardHTML(t, col)).join('')
        : this._emptyStateHTML(col);
      if (this._renderedHTML[key] === html) return;
      this._renderedHTML[key] = html;
      el.innerHTML = html;
    },

    // Render every current column. No-op until the column DOM exists (gated by
    // _boardInit, set in init's $nextTick).
    renderBoard() {
      if (!this._boardInit) return;
      this._closeCardMenu();
      this.columns.forEach((col) => this.renderColumn(col.key));
    },

    // One delegated click/keydown handler for the whole board: menu toggle, menu
    // action, card select. Bound on #board-cols, a descendant of .board-scroll,
    // so stopPropagation here pre-empts the board background's closeDetail
    // handler.
    _bindBoardEvents() {
      var self = this;
      var root = document.getElementById('board-cols');
      if (!root) return;
      root.addEventListener('click', function (e) {
        var item = e.target.closest('.card-menu-item');
        if (item) {
          e.stopPropagation();
          var mcard = item.closest('.kt-card');
          var mid = mcard && mcard.dataset.ticketId;
          self._closeCardMenu();
          if (!mid) return;
          var act = item.dataset.act;
          if (act === 'init') {
            var it = self.tickets.find(function (t) { return t.id === mid; });
            if (it) self.openInitModal(it);
          } else {
            var status = item.dataset.status;
            self.moveTicketVia(mid, act, status ? { status: status } : null);
          }
          return;
        }
        var btn = e.target.closest('.card-menu-btn');
        if (btn) {
          e.stopPropagation();
          self._toggleCardMenu(btn.closest('.kt-card'));
          return;
        }
        var card = e.target.closest('.kt-card');
        if (card) {
          e.stopPropagation();
          self._closeCardMenu();
          var t = self.tickets.find(function (x) { return x.id === card.dataset.ticketId; });
          if (t) self.selectTicket(t);
          return;
        }
        // Click in the board gutter (not a card): close any menu and let the
        // event bubble to .board-scroll, which closes the detail panel.
        self._closeCardMenu();
      });
      root.addEventListener('keydown', function (e) {
        var card = e.target.closest('.kt-card');
        if (card && (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar')) {
          e.preventDefault();
          e.stopPropagation();
          var t = self.tickets.find(function (x) { return x.id === card.dataset.ticketId; });
          if (t) self.selectTicket(t);
        } else if (e.key === 'Escape') {
          self._closeCardMenu();
        }
      });
      // Close the menu on clicks outside the board, and on a window-level Escape.
      document.addEventListener('click', function (e) {
        if (!self._openMenuId) return;
        if (e.target.closest && e.target.closest('#board-cols')) return;
        self._closeCardMenu();
      });
      document.addEventListener('keydown', function (e) {
        if (e.key === 'Escape' && self._openMenuId) self._closeCardMenu();
      });
    },

    _toggleCardMenu(cardEl) {
      if (!cardEl) return;
      var id = cardEl.dataset.ticketId;
      if (this._openMenuId === id) { this._closeCardMenu(); return; }
      this._closeCardMenu();
      cardEl.classList.add('menu-open');
      var btn = cardEl.querySelector('.card-menu-btn');
      if (btn) btn.setAttribute('aria-expanded', 'true');
      this._openMenuId = id;
    },

    _closeCardMenu() {
      if (!this._openMenuId) return;
      document.querySelectorAll('#board-cols .kt-card.menu-open').forEach(function (el) {
        el.classList.remove('menu-open');
        var btn = el.querySelector('.card-menu-btn');
        if (btn) btn.setAttribute('aria-expanded', 'false');
      });
      this._openMenuId = null;
    },

    // Move the .is-selected highlight without re-rendering a column.
    _markSelectedCard() {
      var sel = this.selectedTicket ? this.selectedTicket.id : null;
      document.querySelectorAll('#board-cols .kt-card').forEach(function (el) {
        el.classList.toggle('is-selected', el.dataset.ticketId === sel);
      });
    },

    // Patch the running-duration / age text on the 30s tick, in place, so the
    // clock doesn't trigger a full board re-render.
    _updateCardTimers() {
      var self = this;
      document.querySelectorAll('#board-cols [data-since]').forEach(function (el) {
        el.textContent = self.formatDuration({ started_at: el.dataset.since });
      });
      document.querySelectorAll('#board-cols [data-ago]').forEach(function (el) {
        el.textContent = self.timeAgo(el.dataset.ago);
      });
    },

    initSortable(el) {
      if (this.isMobile) return;
      var self = this;
      // Mark the column wrapper that currently holds the dragged card so it
      // lights up with the column's own tint. Cleared on drag end.
      function setDropTarget(listEl) {
        document.querySelectorAll('.kanban-col.is-drop-target').forEach(function(c) {
          c.classList.remove('is-drop-target');
        });
        if (!listEl) return;
        var wrapper = listEl.closest('.kanban-col');
        if (wrapper) wrapper.classList.add('is-drop-target');
      }
      function clearDropTarget() {
        document.querySelectorAll('.kanban-col.is-drop-target').forEach(function(c) {
          c.classList.remove('is-drop-target');
        });
      }
      // Disable the FLIP animation when the source column is large: animating
      // every sibling on each drag move is the main drag stutter on big boards.
      var ANIM_THRESHOLD = 60;
      var sortable = new Sortable(el, {
        group: 'kanban',
        animation: 150,
        ghostClass: 'sortable-ghost',
        dragClass: 'sortable-drag',
        filter: '.empty-state',
        onStart: function(evt) {
          sortable.option('animation', evt.from.children.length > ANIM_THRESHOLD ? 0 : 150);
          setDropTarget(evt.from);
        },
        onChange: function(evt) { setDropTarget(evt.to); },
        onEnd: function(evt) {
          clearDropTarget();
          var ticketId = evt.item.dataset.ticketId;
          var fromDrop = evt.from.dataset.dropStatus;
          var toDrop = evt.to.dataset.dropStatus;
          if (fromDrop === toDrop || !ticketId) return;
          // No manual DOM restore: moveTask sets the status optimistically and
          // recomputeBoard → renderBoard rebuilds both columns from canonical
          // data, replacing the node Sortable moved (and reverting on failure).
          self.moveTask(ticketId, toDrop);
        }
      });
    },

    async openTerminal() {
      if (!this.selectedTicket || this.terminalOpen) return;
      var seq = ++this._terminalSeq;
      var ticketId = this.selectedTicket.id;
      this.terminalOpen = true;
      this._terminalOpening = true;
      try {
        if (!this._TerminalClass || !this._FitAddonClass) {
          var [termMod, fitMod, unicodeMod, webglMod] = await Promise.all([
            import('/vendor/xterm@5.5.0/xterm.mjs'),
            import('/vendor/addon-fit@0.10.0/addon-fit.mjs'),
            import('/vendor/addon-unicode11@0.8.0/addon-unicode11.mjs'),
            // Optional: a failed load means the terminal falls back to the DOM renderer.
            import('/vendor/addon-webgl@0.18.0/addon-webgl.mjs').catch(function(e) {
              console.warn('webgl addon failed to load, using DOM renderer:', e);
              return null;
            }),
          ]);
          this._TerminalClass = termMod.Terminal;
          this._FitAddonClass = fitMod.FitAddon;
          this._Unicode11AddonClass = unicodeMod.Unicode11Addon;
          this._WebglAddonClass = webglMod ? webglMod.WebglAddon : null;
        }
        await this.$nextTick();
        if (!this.terminalOpen || this._terminalSeq !== seq) return;
        if (this.activeTab !== 'terminal' || this.selectedTicket?.id !== ticketId) {
          this._teardownTransport();
          this.terminalOpen = false;
          return;
        }
        this._connectTerminal(seq);
      } catch (e) {
        console.error('terminal load error:', e);
        this.error = 'Failed to load terminal';
        this.terminalOpen = false;
      } finally {
        if (this._terminalSeq === seq) this._terminalOpening = false;
      }
    },

    reconnectTerminal() {
      if (!this.selectedTicket || this.activeTab !== 'terminal' || this._terminalOpening) return;
      if (this.terminalOpen) {
        this._teardownTransport();
        this.terminalOpen = false;
      }
      this.openTerminal();
    },

    _connectTerminal(seq) {
      // On phone width the live terminal attaches into the mobile detail's own
      // container; on desktop into the panel container (fullscreen moves are
      // handled by the terminalFullscreen watcher).
      var container = document.getElementById(this.isMobile ? 'terminal-container-mobile' : 'terminal-container');
      if (!container) return;
      if (this._terminalSeq !== seq || !this.terminalOpen) return;
      container.textContent = '';

      this._fitAddon = new this._FitAddonClass();
      this._term = new this._TerminalClass({
        theme: this._getTerminalTheme(),
        fontSize: 13,
        fontFamily: "'JetBrains Mono', monospace",
        cursorBlink: this.terminalRW,
        disableStdin: false,
        scrollback: 5000,
        allowProposedApi: true,
      });
      this._term.loadAddon(this._fitAddon);
      this._term.loadAddon(new this._Unicode11AddonClass());
      this._term.unicode.activeVersion = '11';
      this._term.open(container);

      // Must load after open(); on any failure the DOM renderer stays active.
      if (this._WebglAddonClass) {
        try {
          var webgl = new this._WebglAddonClass();
          webgl.onContextLoss(function() { webgl.dispose(); });
          this._term.loadAddon(webgl);
          this._webglAddon = webgl;
        } catch (e) {
          console.warn('webgl renderer unavailable, using DOM renderer:', e);
        }
      }

      var self = this;
      self._resizeObserver = new ResizeObserver(function() {
        clearTimeout(self._resizeTimer);
        self._resizeTimer = setTimeout(function() { self.refitTerminal(); }, 100);
      });
      self._resizeObserver.observe(container);

      requestAnimationFrame(function() {
        if (!self._term || !self.terminalOpen || self._terminalSeq !== seq) return;
        self._fitAddon.fit();
        self._connectWs(seq);
      });
    },

    _connectWs(seq) {
      if (this._termInputDisposable) {
        this._termInputDisposable.dispose();
        this._termInputDisposable = null;
      }
      if (this._terminalSeq !== seq || !this._term) return;
      var self = this;
      var cols = self._term.cols;
      var rows = self._term.rows;
      var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      var url = proto + '//' + location.host + '/ws/terminal/' + self.selectedTicket.id
        + '?cols=' + cols + '&rows=' + rows + (self.terminalRW ? '&rw=1' : '');
      var ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      self._termWs = ws;
      var term = self._term;
      ws.onmessage = function(e) {
        if (self._terminalSeq !== seq) {
          ws.close();
          return;
        }
        if (term) term.write(new Uint8Array(e.data));
      };
      ws.onclose = function() { if (self._termWs === ws) self._termWs = null; };
      ws.onerror = function() { if (self._termWs === ws) self._termWs = null; };
      if (self.terminalRW) {
        self._termInputDisposable = term.onData(function(data) {
          if (self._terminalSeq === seq && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'input', data: data }));
          }
        });
      }
    },

    _teardownTransport() {
      this._terminalSeq++;
      this._terminalOpening = false;
      clearTimeout(this._resizeTimer);
      if (this._resizeObserver) {
        this._resizeObserver.disconnect();
        this._resizeObserver = null;
      }
      if (this._termInputDisposable) {
        this._termInputDisposable.dispose();
        this._termInputDisposable = null;
      }
      if (this._termWs) {
        this._termWs.close();
        this._termWs = null;
      }
      if (this._term) {
        try { this._term.dispose(); } catch (e) {}
        this._term = null;
      }
      this._fitAddon = null;
      this._webglAddon = null;
    },

    closeTerminal() {
      this._teardownTransport();
      this.terminalOpen = false;
    },

    toggleTerminalRW() {
      this.terminalRW = !this.terminalRW;
      if (!this.terminalOpen || !this._term) return;
      if (this._termWs) this._termWs.close();
      this._connectWs(this._terminalSeq);
    },

    refitTerminal() {
      if (!this._term || !this._fitAddon || !this.terminalOpen) return;
      var oldCols = this._term.cols;
      var oldRows = this._term.rows;
      this._fitAddon.fit();
      if (this._term.cols === oldCols && this._term.rows === oldRows) return;
      if (this._termWs && this._termWs.readyState === WebSocket.OPEN) {
        this._termWs.send(JSON.stringify({ type: 'resize', cols: this._term.cols, rows: this._term.rows }));
      }
      // Clear viewport to remove reflow artifacts from cursor-positioned content.
      // tmux will redraw the screen after receiving the resize via SIGWINCH.
      this._term.write('\x1b[2J\x1b[H');
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
    _knownPipeColors: {
      'sigil':                 'indigo',
      'sigil-sdk':             'cyan',
      'kontora':               'green',
      'deployment_tools':      'amber',
      'grafana-assistant-app': 'rose',
      'backend-enterprise':    'amber',
      'astra-l':               'rose',
      'sig-vmwa':              'green',
    },
    _pipeColorPalette: ['indigo', 'cyan', 'amber', 'green', 'rose', 'mauve'],
    pipelineColorByName(name) {
      var n = (name || '').toLowerCase();
      if (!n) return 'none';
      if (this._knownPipeColors[n]) return this._knownPipeColors[n];
      var h = 0;
      for (var i = 0; i < n.length; i++) h = (h * 31 + n.charCodeAt(i)) | 0;
      return this._pipeColorPalette[Math.abs(h) % this._pipeColorPalette.length];
    },
    ticketPipeColor(ticket) {
      return this.pipelineColorByName(ticket.pipeline || this.pathBasename(ticket.path));
    },

    // Class for a single segment of the slim stage progress bar.
    stageBarClass(i, ticket) {
      if (!ticket || !ticket.stages) return '';
      if (ticket.status === 'done') return 'done';
      var currentIdx = ticket.stages.indexOf(ticket.stage);
      if (currentIdx < 0) return '';
      if (i < currentIdx) return 'done';
      if (i === currentIdx) return 'current';
      return '';
    },

    ticketMatchesQuery(ticket, q) {
      var normalized = (q || '').trim().toLowerCase();
      if (!normalized) return true;
      var fields = [ticket.title, ticket.id, this.pathBasename(ticket.path), ticket.pipeline];
      return fields.some(f => f && f.toLowerCase().includes(normalized));
    },

    // Recompute every column's filtered+sorted list in one pass and cache it by
    // column key. Called imperatively at the few mutation points (load, batched
    // SSE flush, delete, debounced search) so the filter+sort runs once per
    // logical change rather than on every reactive template read.
    recomputeBoard() {
      var cols = this.columns;
      var q = (this.searchQuery || '').trim().toLowerCase();
      // status -> column key. Each status maps to exactly one column.
      var colOf = {};
      var board = {};
      cols.forEach(col => {
        board[col.key] = [];
        col.statuses.forEach(s => { colOf[s] = col.key; });
      });
      // Global kontora tallies for the favicon/running pill, computed ignoring
      // the search filter so they reflect all tickets.
      var counts = { in_progress: 0, paused: 0, todo: 0, done: 0 };
      for (var i = 0; i < this.tickets.length; i++) {
        var t = this.tickets[i];
        if (t.kontora && counts[t.status] !== undefined) counts[t.status]++;
        var key = colOf[t.status];
        if (key === undefined) continue;            // no column -> not rendered
        if (q && !this.ticketMatchesQuery(t, q)) continue;
        board[key].push(t);
      }
      var total = 0;
      cols.forEach(col => {
        this._sortColumn(board[col.key], col.statuses);
        total += board[col.key].length;
      });
      this._board = board;
      this._boardTotal = total;
      this._statusCounts = counts;
      this.runningAgents = counts.in_progress;
      // Repaint the imperatively rendered cards from the fresh board data. Guarded
      // until the first post-load render (init's $nextTick) so calls during the
      // initial fetch, before the column DOM exists, are no-ops.
      this.renderBoard();
    },

    // O(1) lookup of a column's cached list. The board header count and the
    // mobile board still read this reactively; the desktop card list is rendered
    // from it imperatively by renderColumn/renderBoard.
    boardTickets(key) {
      return this._board[key] || [];
    },

    debounceRecomputeBoard() {
      if (this._searchDebounce) clearTimeout(this._searchDebounce);
      this._searchDebounce = setTimeout(() => this.recomputeBoard(), 150);
    },

    filteredTicketCount() {
      return this._boardTotal;
    },

    updateSuggestions() {
      var q = this.searchQuery.trim().toLowerCase();
      if (!q) { this.suggestions = []; this.searchOpen = false; this.selectedIndex = -1; return; }
      var projects = {}, pipelines = {};
      var matchingTickets = [];
      this.tickets.forEach(t => {
        var proj = this.pathBasename(t.path);
        if (proj) { if (!projects[proj]) projects[proj] = 0; projects[proj]++; }
        if (t.pipeline) { if (!pipelines[t.pipeline]) pipelines[t.pipeline] = 0; pipelines[t.pipeline]++; }
        if ((t.title && t.title.toLowerCase().includes(q)) || (t.id && t.id.toLowerCase().includes(q))) {
          matchingTickets.push(t);
        }
      });
      var groups = [];
      var projItems = Object.keys(projects).filter(p => p.toLowerCase().includes(q))
        .sort().slice(0, 5).map(p => ({ display: p, value: p, count: projects[p] }));
      if (projItems.length) groups.push({ label: 'Projects', items: projItems });
      var pipItems = Object.keys(pipelines).filter(p => p.toLowerCase().includes(q))
        .sort().slice(0, 5).map(p => ({ display: p, value: p, count: pipelines[p] }));
      if (pipItems.length) groups.push({ label: 'Pipelines', items: pipItems });
      var ticketItems = matchingTickets.slice(0, 5).map(t => ({ display: t.id + ' ' + t.title, value: t.id, count: null }));
      if (ticketItems.length) groups.push({ label: 'Tickets', items: ticketItems });
      this.suggestions = groups;
      this.selectedIndex = -1;
      this.searchOpen = groups.length > 0;
    },

    applySuggestion(item) {
      this.searchQuery = item.value;
      this.searchOpen = false;
      this.selectedIndex = -1;
      this.suggestions = [];
    },

    clearSearch() {
      this.searchQuery = '';
      this.searchOpen = false;
      this.suggestions = [];
      this.selectedIndex = -1;
    },

    flatIndex(gi, ii) {
      var idx = 0;
      for (var g = 0; g < this.suggestions.length; g++) {
        for (var i = 0; i < this.suggestions[g].items.length; i++) {
          if (g === gi && i === ii) return idx;
          idx++;
        }
      }
      return -1;
    },

    totalSuggestions() {
      return this.suggestions.reduce((n, g) => n + g.items.length, 0);
    },

    moveSelection(delta) {
      var total = this.totalSuggestions();
      if (!total) return;
      if (!this.searchOpen) this.searchOpen = true;
      this.selectedIndex = ((this.selectedIndex + delta) % total + total) % total;
    },

    acceptSelection() {
      if (this.selectedIndex < 0) return;
      var idx = 0;
      for (var g = 0; g < this.suggestions.length; g++) {
        for (var i = 0; i < this.suggestions[g].items.length; i++) {
          if (idx === this.selectedIndex) { this.applySuggestion(this.suggestions[g].items[i]); return; }
          idx++;
        }
      }
    },

    formatDuration(ticket) {
      if (!ticket || !ticket.started_at) return '';
      var mins = Math.floor((this.now - new Date(ticket.started_at)) / 60000);
      if (mins < 1) return '<1m';
      if (mins < 60) return mins + 'm';
      return Math.floor(mins / 60) + 'h ' + (mins % 60) + 'm';
    },

    formatAbsDate(dateStr) {
      if (!dateStr) return '';
      var d = new Date(dateStr);
      var pad = function(n) { return n < 10 ? '0' + n : '' + n; };
      return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
    },

    timeAgo(dateStr) {
      if (!dateStr) return '';
      var diff = Math.floor((this.now - new Date(dateStr)) / 1000);
      if (diff < 60) return 'just now';
      if (diff < 3600) return Math.floor(diff / 60) + 'm';
      if (diff < 86400) return Math.floor(diff / 3600) + 'h';
      if (diff < 604800) return Math.floor(diff / 86400) + 'd';
      return Math.floor(diff / 604800) + 'w';
    },

    renderMarkdown(md) {
      if (!md) return '';
      try { return DOMPurify.sanitize(marked.parse(md)); } catch (e) { return ''; }
    },

    startResize(e) {
      var self = this;
      var startX = e.clientX;
      var startW = self.panelWidth;
      var handle = e.target;
      handle.classList.add('active');
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';

      function onMove(ev) {
        var delta = startX - ev.clientX;
        var maxW = Math.floor(window.innerWidth * 0.9);
        self.panelWidth = Math.max(320, Math.min(maxW, startW + delta));
      }
      function onUp() {
        document.removeEventListener('mousemove', onMove);
        document.removeEventListener('mouseup', onUp);
        handle.classList.remove('active');
        document.body.style.cursor = '';
        document.body.style.userSelect = '';
        localStorage.setItem('kontora-panel-width', self.panelWidth);
      }
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    },

    toggleTheme() {
      this.lightTheme = !this.lightTheme;
      var t = this.lightTheme ? 'light' : 'dark';
      applyTheme(t);
      setStoredTheme(t);
      if (this._term) this._applyTerminalTheme();
      this.updateFavicon();
    },

    _getTerminalTheme() {
      var s = getComputedStyle(document.documentElement);
      return { background: this._cssVar('--surface-deep', s), foreground: this._cssVar('--tx', s), cursor: this._cssVar('--accent', s), selectionBackground: this._cssVar('--surface-700', s) };
    },

    _applyTerminalTheme() {
      if (!this._term) return;
      var theme = this._getTerminalTheme();
      this._term.options.theme = theme;
    },

    // ── Mobile UI ────────────────────────────────────────────────────────
    // The phone-width experience is a separate layer gated on isMobile: a status
    // tab strip + card list (board), a full-screen tabbed detail (terminal /
    // logs / ticket), and bottom sheets (actions / new ticket). It reuses the
    // same ticket data and operations as the desktop board — only the layout
    // and a few view-state fields (activeColumn, detailTab, sheet) are new.

    // The status column currently shown on the board, clamped to a valid index.
    mobileColumn() {
      var cols = this.columns;
      var i = Math.max(0, Math.min(this.activeColumn, cols.length - 1));
      return cols[i];
    },

    setActiveColumn(i) {
      this.activeColumn = Math.max(0, Math.min(i, this.columns.length - 1));
    },

    // Horizontal swipe on the card list moves between status columns. Mirrors
    // the prototype's threshold: dominant horizontal travel of at least 55px.
    onBoardTouchStart(e) {
      var t = e.changedTouches[0];
      this._swipeX = t.clientX;
      this._swipeY = t.clientY;
    },
    onBoardTouchEnd(e) {
      if (this.selectedTicket || this.sheet) return;
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
      return !!t && !['open', 'todo'].includes(t.status);
    },
    // The first (non-ticket) detail tab: a live terminal while running, the
    // stage-log history otherwise.
    mobileFirstTab() {
      return this.selectedTicket && this.selectedTicket.status === 'in_progress' ? 'terminal' : 'logs';
    },
    mobileFirstTabLabel() {
      return this.selectedTicket && this.selectedTicket.status === 'in_progress' ? '›_ terminal' : '≡ logs';
    },

    async openMobileTicket(t) {
      // Pick the detail tab from the card's status before selectTicket() runs,
      // so the right pane is already visible when the live terminal attaches.
      if (t.status === 'in_progress') this.detailTab = 'terminal';
      else if (this.mobileHasRun(t)) this.detailTab = 'logs';
      else this.detailTab = 'ticket';
      await this.selectTicket(t);
      // selectTicket() applies the desktop tab heuristics, which only fetch logs
      // when the ticket carries history. The mobile logs tab needs them either
      // way, so load them here if nothing started (logViewLoading is set
      // synchronously when selectTicket did kick off a fetch).
      if (this.detailTab === 'logs' && this.logViewContent === null && !this.logViewLoading) {
        this.mobileSwitchTab('logs');
      }
    },

    mobileSwitchTab(tab) {
      if (!this.selectedTicket) return;
      this.detailTab = tab;
      if (tab === 'terminal') {
        this.logViewContent = null;
        this.logViewStage = null;
        this.activeTab = 'terminal';
        if (this.selectedTicket.status === 'in_progress') this.openTerminal();
      } else {
        if (this.terminalOpen) this.closeTerminal();
        this.activeTab = 'ticket';
        if (tab === 'logs') {
          var t = this.selectedTicket;
          var stage = t.stage;
          if (!stage && t.history && t.history.length) stage = t.history[t.history.length - 1].stage;
          this.fetchStageLogs(t.id, stage);
        }
      }
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
      var self = this, id = t && t.id;
      var run = function(ep) { return function() { self.moveTicketVia(id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(id, 'move', { status: st }); }; };
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
      var self = this, id = t && t.id;
      var run = function(ep) { return function() { self.moveTicketVia(id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(id, 'move', { status: st }); }; };
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
      if (!t) return;
      this.sheet = { type: 'actions', ticket: t };
    },
    async openNewSheet() {
      this.createForm = { title: '', path: '', pipeline: '', agent: '', status: 'todo', body: '', branch: '' };
      this.error = null;
      this.sheet = { type: 'new' };
      if (!this.configCache) {
        try {
          var res = await fetch('/api/config');
          if (res.ok) this.configCache = await res.json();
        } catch (e) { /* form still works with empty pipeline/agent lists */ }
      }
    },
    closeSheet() {
      this.sheet = null;
    },

    async submitCreateTicketMobile() {
      this.error = null;
      await this.submitCreateTicket();
      if (!this.error) this.closeSheet();
    },

    // Action rows for the actions sheet, status by status. Each row closes the
    // sheet, then runs against a real endpoint (or opens the relevant modal).
    mobileSheetActions(t) {
      if (!t) return [];
      var self = this, rows = [];
      var add = function(label, kind, fn) { rows.push({ label: label, kind: kind, run: function() { self.closeSheet(); fn(); } }); };
      var run = function(ep) { return function() { self.moveTicketVia(t.id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(t.id, 'move', { status: st }); }; };
      var s = t.status;
      if ((s === 'open' || s === 'todo') && !t.kontora) add('Initialize ticket', 'warn', function() { self.openInitModal(t); });
      if (s === 'open') {
        add('Queue agent', 'primary', run('run'));
        add('Move to Human Review', 'default', move('human_review'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'todo') {
        add('Queue agent', 'primary', run('run'));
        add('Move to open', 'default', move('open'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'in_progress') {
        add('Pause agent', 'warn', function() { self.action('pause'); });
        add('Skip stage', 'default', function() { self.action('skip'); });
        add('Send to review', 'primary', move('human_review'));
        add('Mark done', 'default', move('done'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'paused') {
        add('Resume', 'primary', run('retry'));
        add('Mark done', 'default', move('done'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'human_review') {
        if (t.kontora) add('Plannotator review', 'default', function() { self.startPlannotatorReview(t); });
        add('Approve & merge', 'primary', move('done'));
        add('Send back', 'warn', run('retry'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'done') {
        add('Re-open', 'default', run('retry'));
        add('Send to review', 'default', move('human_review'));
        if (t.branch) add('Copy branch', 'default', function() { self.copyBranch(t.branch); });
        add('Delete file', 'danger', function() { self.openDeleteModal(); });
      } else if (s === 'cancelled') {
        add('Re-open', 'default', run('retry'));
        add('Delete file', 'danger', function() { self.openDeleteModal(); });
      } else {
        (this.validMoves[s] || []).forEach(function(mv) {
          add(mv.label, 'default', mv.status ? move(mv.status) : run(mv.endpoint));
        });
      }
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
