function kontora() {
  return {
    tickets: [],
    runningAgents: 0,
    selectedTicket: null,
    terminalOpen: false,
    terminalRW: false,
    terminalFullscreen: false,
    ticketFullscreen: false,
    activeTab: 'terminal',
    panelWidth: parseInt(localStorage.getItem('kontora-panel-width')) || Math.floor(window.innerWidth * 0.66),
    loading: true,
    error: null,
    isMobile: window.innerWidth < 768,
    createModal: false,
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
    _fitAddon: null,
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

    _builtinColumns: [
      { status: 'open', label: 'Open', color: 'bg-accent', tip: 'Draft ticket, not running yet. Drag to Todo or click Initialize to start.', emptyText: 'Create a ticket to get started', tint: '', glow: 'glow-top-accent',
        emptyIcon: '<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M9 15h6"/><path d="M12 18v-6"/>' },
      { status: 'todo', label: 'Todo', color: 'bg-tx-4', tip: 'Waiting to start. Will begin automatically when a slot is available.', emptyText: 'Move a ticket here to put it next in line', tint: '', glow: 'glow-top-muted',
        emptyIcon: '<polyline points="22 12 16 12 14 15 10 15 8 12 2 12"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>' },
      { status: 'in_progress', label: 'Running', color: 'bg-accent', tip: 'An agent is currently working on this ticket.', emptyText: 'No tickets are running right now', tint: '', glow: 'glow-top-accent',
        emptyIcon: '<path d="M12 8V4H8"/><rect width="16" height="12" x="4" y="8" rx="2"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/>' },
      { status: 'paused', label: 'Paused', color: 'bg-warn', tip: 'Stopped for now. Click Retry or drag to Todo to resume.', emptyText: 'No paused tickets', tint: '', glow: 'glow-top-warn',
        emptyIcon: '<rect x="14" y="4" width="4" height="16" rx="1"/><rect x="6" y="4" width="4" height="16" rx="1"/>' },
      { status: 'done', label: 'Done', color: 'bg-ok', tip: 'Ticket completed successfully.', emptyText: 'No completed tickets yet', tint: '', glow: 'glow-top-ok',
        emptyIcon: '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>' },
      { status: 'cancelled', label: 'Cancelled', color: 'bg-surface-600', tip: 'Stopped manually. Drag to Todo to run it again.', emptyText: 'No cancelled tickets', tint: '', glow: 'glow-top-muted',
        emptyIcon: '<path d="m15 9-6 6"/><path d="m9 9 6 6"/><circle cx="12" cy="12" r="10"/>' },
    ],

    get columns() {
      var cols = [...this._builtinColumns];
      var custom = this.configCache?.custom_statuses || [];
      if (custom.length > 0) {
        var doneIdx = cols.findIndex(c => c.status === 'done');
        if (doneIdx < 0) doneIdx = cols.length;
        var customCols = custom.map(s => ({
          status: s,
          label: s.charAt(0).toUpperCase() + s.slice(1).replace(/_/g, ' '),
          color: 'bg-surface-600',
          tip: 'Custom status: ' + s,
          emptyText: 'No ' + s + ' tickets',
          tint: '',
          glow: 'glow-top-muted',
          emptyIcon: '<circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/>',
        }));
        cols.splice(doneIdx, 0, ...customCols);
      }
      return cols;
    },

    async init() {
      window.addEventListener('resize', () => {
        this.isMobile = window.innerWidth < 768;
      });
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
        if (cfgRes.ok) this.configCache = await cfgRes.json();
      } catch (e) {
        this.error = 'Failed to load config';
      }
      try {
        await this.fetchTasks();
      } catch (e) {
        this.error = 'Failed to load tickets';
      }
      this.loading = false;
      this.connectSSE();
    },

    async fetchTasks() {
      const res = await fetch('/api/tickets');
      if (!res.ok) throw new Error('Failed to fetch tickets');
      const data = await res.json();
      this.tickets = data.tickets || [];
      this.runningAgents = data.running_agents || 0;
      this.updateFavicon();
    },

    _cssVar(name, styles) {
      var s = styles || getComputedStyle(document.documentElement);
      return 'rgb(' + s.getPropertyValue(name).trim() + ')';
    },

    updateFavicon() {
      const counts = { in_progress: 0, paused: 0, todo: 0, done: 0 };
      this.tickets.filter(t => t.kontora).forEach(t => { if (counts[t.status] !== undefined) counts[t.status]++; });

      var styles = getComputedStyle(document.documentElement);
      var v = (name) => this._cssVar(name, styles);
      let color, label;
      if (counts.in_progress > 0) {
        color = v('--accent'); label = counts.in_progress + ' running';
      } else if (counts.paused > 0) {
        color = v('--warn'); label = counts.paused + ' paused';
      } else if (counts.todo > 0) {
        color = v('--surface-600'); label = counts.todo + ' queued';
      } else if (counts.done > 0) {
        color = v('--ok'); label = 'all done';
      } else {
        color = v('--surface-600'); label = null;
      }

      document.title = label ? '(' + label + ') kontora' : 'kontora';
      const icon = document.querySelector('link[rel="icon"]');
      if (icon) icon.href = "data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><circle cx='8' cy='8' r='7' fill='" + encodeURIComponent(color) + "'/></svg>";
    },

    ticketsByStatus(status) {
      return this.tickets
        .filter(t => t.status === status && (status === 'open' || t.kontora))
        .sort((a, b) => {
          const ta = status === 'in_progress' && a.started_at ? a.started_at : (a.created_at || '');
          const tb = status === 'in_progress' && b.started_at ? b.started_at : (b.created_at || '');
          if (ta !== tb) return ta > tb ? -1 : 1;
          if (a.title !== b.title) return a.title < b.title ? -1 : 1;
          if (a.id !== b.id) return a.id < b.id ? -1 : 1;
          return 0;
        });
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

    connectSSE() {
      if (this._eventSource) this._eventSource.close();
      const es = new EventSource('/api/events');
      this._eventSource = es;
      es.addEventListener('ticket_updated', (e) => {
        const ticket = JSON.parse(e.data);
        const idx = this.tickets.findIndex(t => t.id === ticket.id);
        if (idx >= 0) {
          this.tickets[idx] = ticket;
        } else {
          this.tickets.push(ticket);
        }
        this.runningAgents = this.tickets.filter(t => t.status === 'in_progress' && t.kontora).length;
        this.updateFavicon();
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
      });
      es.addEventListener('ticket_deleted', (e) => {
        const ticket = JSON.parse(e.data);
        this.tickets = this.tickets.filter(t => t.id !== ticket.id);
        if (this.selectedTicket?.id === ticket.id) {
          this.closeDetail();
        }
        this.runningAgents = this.tickets.filter(t => t.status === 'in_progress' && t.kontora).length;
        this.updateFavicon();
      });
      es.addEventListener('terminal_ready', (e) => {
        const ticket = JSON.parse(e.data);
        if (this.selectedTicket?.id === ticket.id && this.activeTab === 'terminal') {
          this.reconnectTerminal();
        }
      });
      es.onerror = () => {
        es.close();
        setTimeout(() => this.connectSSE(), 3000);
      };
    },

    async openCreateModal() {
      this.createForm = { title: '', path: '', pipeline: '', agent: '', status: 'todo', body: '', branch: '' };
      this.createModal = true;
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
      this.createModal = false;
      this.createSubmitting = false;
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
        this.runningAgents = this.tickets.filter(t => t.status === 'in_progress' && t.kontora).length;
        this.updateFavicon();
      } catch (e) {
        this.error = 'Delete failed: ' + e.message;
      } finally {
        this.deleteSubmitting = false;
      }
    },

    async moveTask(ticketId, newStatus) {
      this.error = null;
      const ticket = this.tickets.find(t => t.id === ticketId);
      if (newStatus === 'in_progress' && ticket && ticket.status === 'open') {
        if (confirm("Tickets can't run directly from Open. Move to Todo instead?")) {
          this.moveTask(ticketId, 'todo');
        }
        return;
      }
      if (newStatus === 'todo' && ticket && !ticket.kontora) {
        this.openInitModal(ticket);
        return;
      }
      const oldStatus = ticket ? ticket.status : null;
      if (ticket) ticket.status = newStatus;
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
        }
      } catch (e) {
        this.error = 'Move failed: ' + e.message;
        if (ticket && oldStatus) ticket.status = oldStatus;
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

    statusStyle(status) {
      return {
        open: 'bg-accent/10 text-accent border-accent/20',
        todo: 'bg-surface-800 text-tx-3 border-surface-700/50',
        in_progress: 'bg-accent/10 text-accent border-accent/20',
        paused: 'bg-warn/10 text-warn border-warn/20',
        done: 'bg-ok/10 text-ok border-ok/20',
      }[status] || 'bg-surface-800 text-tx-3 border-surface-700/50';
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

    stageStyle(stage, ticket) {
      if (!ticket || !ticket.stages) return 'bg-surface-800 text-surface-600';
      var stageIdx = ticket.stages.indexOf(stage);
      var currentIdx = ticket.stages.indexOf(ticket.stage);
      if (ticket.status === 'done') return 'bg-ok/10 text-ok/60';
      if (stage === ticket.stage) {
        if (ticket.status === 'in_progress') return 'bg-accent/15 text-accent';
        if (ticket.status === 'paused') return 'bg-warn/15 text-warn';
        return 'bg-accent/15 text-accent';
      }
      if (currentIdx >= 0 && stageIdx >= 0 && stageIdx < currentIdx) return 'bg-ok/10 text-ok/60';
      return 'bg-surface-800 text-surface-600';
    },

    initSortable(el, status) {
      if (this.isMobile) return;
      var self = this;
      new Sortable(el, {
        group: 'kanban',
        animation: 150,
        ghostClass: 'sortable-ghost',
        dragClass: 'sortable-drag',
        filter: '.empty-state',
        onEnd: function(evt) {
          var ticketId = evt.item.dataset.ticketId;
          var fromStatus = evt.from.dataset.status;
          var toStatus = evt.to.dataset.status;
          if (fromStatus === toStatus || !ticketId) return;
          evt.item.remove();
          var ref = evt.from.children[evt.oldIndex];
          evt.from.insertBefore(evt.item, ref || null);
          self.moveTask(ticketId, toStatus);
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
          var [termMod, fitMod, unicodeMod] = await Promise.all([
            import('https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/+esm'),
            import('https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.10.0/+esm'),
            import('https://cdn.jsdelivr.net/npm/@xterm/addon-unicode11@0.8.0/+esm'),
          ]);
          this._TerminalClass = termMod.Terminal;
          this._FitAddonClass = fitMod.FitAddon;
          this._Unicode11AddonClass = unicodeMod.Unicode11Addon;
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
      var container = document.getElementById('terminal-container');
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

    ticketMatchesQuery(ticket, q) {
      var normalized = (q || '').trim().toLowerCase();
      if (!normalized) return true;
      var fields = [ticket.title, ticket.id, this.pathBasename(ticket.path), ticket.pipeline];
      return fields.some(f => f && f.toLowerCase().includes(normalized));
    },

    filteredTicketsByStatus(status) {
      var all = this.ticketsByStatus(status);
      if (!this.searchQuery) return all;
      return all.filter(t => this.ticketMatchesQuery(t, this.searchQuery));
    },

    filteredTicketCount() {
      return this.columns.reduce((n, col) => n + this.filteredTicketsByStatus(col.status).length, 0);
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
      var start = new Date(ticket.started_at);
      var now = new Date();
      var mins = Math.floor((now - start) / 60000);
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
      var diff = Math.floor((Date.now() - new Date(dateStr)) / 1000);
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
  };
}
