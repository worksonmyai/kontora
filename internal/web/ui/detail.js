import { runSeconds } from './format.js';
import { highlightMarkdown } from './markdown.js';
import { termState } from './terminal.js';

// The ticket detail panel: summary, stage rail, inline editing, stage logs
// and the actions the panel offers.
export function kontoraDetail() {
  return {
    async selectTicket(ticket) {
      if (this.selectedTicket?.id === ticket.id) {
        this.closeDetail();
        return;
      }
      // The editor holds the previous ticket's body. Saving it before the swap,
      // then dropping it, keeps a pending debounce or a later flush from writing
      // that body onto the ticket being opened. startEditing re-arms it below.
      this.flushEditSave();
      this.editing = false;
      this.editingBody = false;
      this.closeTerminal();
      this.terminalRW = false;
      this.logViewContent = null;
      this.logViewStage = null;
      this.logViewLoading = false;
      this._resetActivity();
      this.setStageOpen = false;
      this.ticketChanges = null;
      this.collapsedStages = {};
      this.relExpanded = {};
      this.childrenCollapsed = false;
      this.childrenExpanded = false;
      this.selectedTicket = ticket;
      this._pushRecentTicket(ticket.id);
      this.detailLoading = true;
      try {
        var res = await fetch('/api/tickets/' + ticket.id);
        if (res.ok) {
          var full = await res.json();
          this.selectedTicket = full;
          var idx = this.tickets.findIndex(function(t) { return t.id === full.id; });
          if (idx >= 0) {
            this.tickets[idx] = this.boardEntry(full);
            // The replacement can change agent, status, or any rendered field,
            // so refresh the cached board and the agent tally from it.
            this.recomputeBoard();
          }
        }
      } catch (e) {
        this.error = 'Failed to load ticket details';
      }
      this.detailLoading = false;
      // The rail's churn block is always on, so the branch diff is read for
      // every ticket that has one rather than only for summarised runs.
      if (this.selectedTicket?.branch) {
        this.fetchChanges(this.selectedTicket.id);
      }
      if (this.selectedTicket?.status !== 'in_progress' && this.selectedTicket?.status !== 'todo' && this.selectedTicket?.status !== 'open'
          && this.selectedTicket?.history?.length > 0) {
        var latest = this.latestCompletedRun();
        if (latest) this.fetchActivity(latest.stage, latest.run);
        // Finished tickets with a summary open on the summary tab; the
        // transcript stays prefetched for the activity tab.
        this.activeTab = this.summaryFirst(this.selectedTicket) ? 'summary' : 'activity';
      } else if (this.selectedTicket?.status === 'in_progress') {
        this.activeTab = 'session';
        this.openTerminal();
      } else {
        this.activeTab = 'ticket';
        if (['open', 'todo', 'paused'].concat(this.configCache?.custom_statuses || []).includes(this.selectedTicket?.status)) this.startEditing();
      }
      this.writeHash();
    },

    // Whether the detail panel should open on the summary (ticket tab): the
    // ticket finished and a stage run recorded a summary.
    summaryFirst(t) {
      return !!t && ['human_review', 'done'].includes(t.status) && !!t.summary;
    },

    // The ticket-level outcome the daemon synthesized from every run summary.
    // It is rendered above the stage cards, but no stage produced it: it is
    // not counted as one and it cannot be collapsed.
    finalSummary() {
      return this.selectedTicket?.final_summary || '';
    },

    // The stage of the last history entry. It usually wrote the top-level
    // summary, but a manual SetSummary and the non-pipeline exit path do not
    // append history.
    summaryStage() {
      var h = this.selectedTicket?.history;
      return h && h.length ? h[h.length - 1].stage : '';
    },

    // One card per run that recorded a summary, newest first. Duration and run
    // count come from stageRibbon(), so a card and the ribbon segment above it
    // cannot disagree. Both are per stage, so two runs of one stage show the
    // same stage total.
    //
    // The run number counts the stage's earlier history rows, as the daemon
    // does, rather than reading h.run: rows written before that field existed
    // carry none, and two cards keyed stage#0 make Alpine throw on the
    // duplicate x-for key.
    summaryCards() {
      var t = this.selectedTicket;
      if (!t) return [];
      var segs = Object.create(null);
      this.stageRibbon().forEach(function (s) { segs[s.name] = s; });
      var hist = t.history || [];
      var totalRuns = Object.create(null);
      hist.forEach(function (h) { totalRuns[h.stage] = (totalRuns[h.stage] || 0) + 1; });
      var walked = Object.create(null);
      var cards = [];
      hist.forEach(function (h) {
        var run = walked[h.stage] || 0;
        walked[h.stage] = run + 1;
        if (!h.summary) return;
        // A stage the current pipeline no longer lists has no ribbon segment to
        // agree with, so the card reads this run's own clock.
        var seg = segs[h.stage];
        cards.push({
          key: h.stage + '#' + run,
          stage: h.stage,
          run: run,
          summary: h.summary,
          failed: h.exit_code !== 0,
          agent: h.agent || '',
          seconds: seg ? seg.seconds : runSeconds(h),
          runs: seg ? seg.runs : totalRuns[h.stage],
        });
      });
      cards.reverse();
      // Prepend the top-level field only when it says something the newest run
      // does not.
      var top = t.summary || '';
      if (top && (!cards.length || cards[0].summary !== top)) {
        cards.unshift({
          key: 'summary#top', stage: this.summaryStage(), run: -1,
          summary: top, failed: false, agent: '', seconds: 0, runs: 1,
        });
      }
      return cards;
    },

    // A failed run keeps the error hue wherever it sits. Otherwise the newest
    // card is the outcome and carries the done hue whatever stage produced it,
    // and an earlier card takes its hue from the stage's position in the
    // pipeline rather than from its name.
    stageHue(card, i) {
      if (card.failed) return '--st-error';
      if (i === 0) return '--st-done';
      var stages = this.selectedTicket?.stages || [];
      var idx = stages.indexOf(card.stage);
      var cycle = ['--st-open', '--st-review', '--st-paused'];
      return cycle[(idx >= 0 ? idx : i) % cycle.length];
    },

    // "50m 00s · ×2 · codex · failed" — the right side of a card header.
    stageCardMeta(card) {
      var parts = [];
      if (card.seconds > 0) parts.push(this.formatSeconds(card.seconds));
      if (card.runs > 1) parts.push('×' + card.runs);
      if (card.agent) parts.push(card.agent);
      if (card.failed) parts.push('failed');
      return parts.join(' · ');
    },

    toggleStageCard(card) {
      this.collapsedStages[card.key] = !this.collapsedStages[card.key];
    },

    // Collapse-all skips the newest card: it is the answer the reader came for.
    earlierAllCollapsed() {
      var self = this;
      var rest = this.summaryCards().slice(1);
      return rest.length > 0 && rest.every(function (c) { return !!self.collapsedStages[c.key]; });
    },

    toggleAllStages() {
      var collapse = !this.earlierAllCollapsed();
      var self = this;
      this.summaryCards().slice(1).forEach(function (c) { self.collapsedStages[c.key] = collapse; });
    },

    // "4 stages · 42 files · +14015 −0 · 1 commit" — the right side of the
    // outcome rule. Distinct stages rather than cards: a stage that ran twice
    // is one segment on the ribbon above and must not read as two here.
    summaryHeadline() {
      var stages = Object.create(null);
      this.summaryCards().forEach(function (c) { stages[c.stage] = true; });
      var n = Object.keys(stages).length;
      var parts = [n + ' stage' + (n === 1 ? '' : 's')];
      var c = this.churn();
      if (c.count) parts.push(c.count + ' file' + (c.count === 1 ? '' : 's'), '+' + c.added + ' −' + c.deleted);
      var commits = this.ticketChanges?.commits?.length || 0;
      if (commits) parts.push(commits + ' commit' + (commits === 1 ? '' : 's'));
      return parts.join(' · ');
    },

    async fetchChanges(id) {
      try {
        var res = await fetch('/api/tickets/' + id + '/changes');
        if (res.ok) {
          var changes = await res.json();
          if (this.selectedTicket?.id === id) this.ticketChanges = changes;
        }
      } catch (e) { /* the card simply shows no commit list */ }
    },

    switchTab(tab) {
      // The tab content uses x-show, so leaving it would otherwise keep a
      // focused textarea mounted and bring it back with stale geometry.
      if (tab !== 'ticket' && this.editingBody) {
        this.flushEditSave();
        this.editingBody = false;
      }
      // The live session only exists while a stage runs. Asking for it on a
      // finished ticket lands on that stage's transcript instead.
      if (tab === 'session' && this.selectedTicket?.status !== 'in_progress') tab = 'activity';
      this.activeTab = tab;
      // Leaving the session tab keeps the stream and the terminal: the tab
      // content is x-show, so the container survives, and output that arrives
      // while it is hidden is already in the buffer on return. Only the fit is
      // owed, because a hidden container measures zero.
      if (tab === 'session' && this.selectedTicket?.status === 'in_progress') {
        if (this.terminalOpen) {
          var self = this;
          requestAnimationFrame(function() { self.refitTerminal(); });
        } else {
          this.openTerminal();
        }
      }
      if (tab !== 'activity') {
        this._stopActivityPoll();
      } else if (!this.activity && !this.activityLoading) {
        var target = this.activityTarget();
        if (target) this.fetchActivity(target.stage, target.run);
      } else if (this.activity?.live) {
        this._armActivityPoll(this.activityStage, this.activityRun, true);
      }
      if (tab === 'ticket' && !this.editing) {
        this.startEditing();
      }
      // A running ticket keeps committing, so the commit list is re-read every
      // time a tab that shows it is opened rather than only once on select.
      if ((tab === 'summary' || tab === 'diff') && this.selectedTicket) {
        this.fetchChanges(this.selectedTicket.id);
      }
    },

    closeDetail() {
      this.flushEditSave();
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
      this._resetActivity();
      this.ticketChanges = null;
      this.collapsedStages = {};
      this.relExpanded = {};
      this.childrenCollapsed = false;
      this.childrenExpanded = false;
      this.noteDraft = '';
      this.writeHash();
    },

    // ---- hash routing ------------------------------------------------------

    // Parse a URL hash into {view, ticketId}. Anything unrecognised is the
    // board, so a stale or hand-typed link never leaves the app blank.
    parseHash(hash) {
      var h = String(hash == null ? '' : hash).replace(/^#/, '');
      if (h.indexOf('/t/') === 0 && h.length > 3) {
        var raw = h.slice(3);
        // A malformed escape ("#/t/%") makes decodeURIComponent throw. Left
        // uncaught it would reject applyRoute, and the await in init() would
        // skip the board render that follows it.
        var id;
        try { id = decodeURIComponent(raw); } catch (e) { id = raw; }
        return { view: 'board', ticketId: id };
      }
      if (h === '/new') return { view: 'new', ticketId: null };
      if (h === '/stats') return { view: 'stats', ticketId: null };
      if (h === '/settings') return { view: 'settings', ticketId: null };
      return { view: 'board', ticketId: null };
    },

    // Serialize the current view back to a hash. Inverse of parseHash.
    routeHash() {
      if (this.selectedTicket) return '#/t/' + encodeURIComponent(this.selectedTicket.id);
      if (this.currentView === 'new') return '#/new';
      if (this.currentView === 'stats') return '#/stats';
      if (this.currentView === 'settings') return '#/settings';
      return '#/';
    },

    writeHash() {
      if (this._applyingRoute) return;
      var next = this.routeHash();
      if (location.hash === next) return;
      location.hash = next;
    },

    // Drive state from the hash. Browser Back and a pasted link both land here.
    // Every branch is a no-op when the state already matches, so the hashchange
    // fired by our own writeHash costs nothing.
    async applyRoute() {
      var r = this.parseHash(location.hash);
      this._applyingRoute = true;
      try {
        if (r.ticketId) {
          if (this.selectedTicket && this.selectedTicket.id === r.ticketId) return;
          var t = this.tickets.find(function(x) { return x.id === r.ticketId; });
          // The detail rail covers whatever view is underneath, and selectTicket
          // does not touch currentView, so the switch below still has to run:
          // opening a ticket from Stats would otherwise leave its poll armed and
          // put the user back on Stats when the rail closes.
          if (!t && this.selectedTicket) {
            // The hash names a ticket this board does not have. Fall back to the
            // board rather than rendering an empty shell.
            this.closeDetail();
          }
          if (t) await this.selectTicket(t);
        } else if (this.selectedTicket) {
          this.closeDetail();
        }
        if (this.currentView !== r.view) await this.gotoView(r.view);
      } finally {
        this._applyingRoute = false;
      }
      this.writeHash();
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

    // One button, one endpoint. Only the word changes: retry reads right for a
    // ticket that stopped mid-pipeline, restart for one that already finished.
    // Both queue the stage the ticket currently sits on; neither rewinds it.
    retryLabel() {
      const restart = ['done', 'cancelled'].includes(this.selectedTicket?.status);
      if (this.actionLoading === 'retry') return restart ? 'restarting...' : 'retrying...';
      return restart ? 'restart' : 'retry';
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
        if (idx >= 0) this.tickets[idx] = this.boardEntry(updated);
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
      // Starting a ticket goes through the init modal, not /run, /retry, or
      // /move: an unmanaged one cannot execute at all, and a managed one in open
      // gets the form so the run's fields are confirmed before it is queued.
      if (ticket && this.needsInitModal(ticket) && this.ticketActionWouldStart(endpoint, body)) {
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
          if (idx >= 0) this.tickets[idx] = this.boardEntry(updated);
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
      if (ticket && this.needsInitModal(ticket) && ['todo', 'in_progress'].includes(newStatus)) {
        // A drag here has already moved the DOM node into the target column;
        // rebuild from canonical data so the card snaps back if the user
        // dismisses the init modal. The drag dropped the render cache for both
        // columns, so this render patches them even though no status changed.
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
      var body = this.selectedTicket.body || '';
      this._bodyLead = body.startsWith('\n') ? '\n' : '';
      this.editForm = {
        body: body.slice(this._bodyLead.length),
        pipeline: '',
        path: this.selectedTicket.path || '',
        agent: '',
        branch: this.selectedTicket.branch || '',
        base_branch: this.selectedTicket.base_branch || '',
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
      this._editInherited = this.projectDefaultsFor(this.editForm.path);
      // Defer select values until after x-for has created <option> elements.
      // Alpine's x-model effect on the <select> fires before x-for populates
      // options, so setting the value immediately would fail to match.
      await this.$nextTick();
      this.editForm.pipeline = pipeline;
      this.editForm.agent = agent;
    },

    // Pipeline and agent defaults for the project that owns path. The daemon
    // names an empty branch at pickup.
    projectDefaultsFor(path) {
      var project = this.projectForPath(path) || {};
      return {
        pipeline: project.pipeline || '',
        agent: project.agent || '',
      };
    },

    // Re-apply inherited pipeline and agent values after the path changes.
    // Keep values the user selected and leave the branch unchanged.
    onEditPathChange() {
      var prev = this._editInherited || { pipeline: '', agent: '' };
      var next = this.projectDefaultsFor(this.editForm.path);

      if (!this.editForm.pipeline || this.editForm.pipeline === prev.pipeline) {
        this.editForm.pipeline = next.pipeline;
      }
      if (!this.editForm.agent || this.editForm.agent === prev.agent) {
        this.editForm.agent = next.agent;
      }
      this._editInherited = next;

      this.saveEdit();
    },

    async saveEdit() {
      if (!this.selectedTicket || !this.editing) return;
      this.editSubmitting = true;
      this.editSaved = false;
      try {
        const body = {};
        const editedBody = this._bodyLead + this.editForm.body;
        if (editedBody !== (this.selectedTicket.body || '')) body.body = editedBody;
        if (this.editForm.pipeline !== (this.selectedTicket.pipeline || '')) body.pipeline = this.editForm.pipeline;
        if (this.editForm.path !== (this.selectedTicket.path || '')) body.path = this.editForm.path;
        if (this.editForm.agent !== (this.selectedTicket.agent || '')) body.agent = this.editForm.agent;
        if (this.editForm.branch !== (this.selectedTicket.branch || '')) body.branch = this.editForm.branch;
        if (this.editForm.base_branch !== (this.selectedTicket.base_branch || '')) body.base_branch = this.editForm.base_branch;
        if (Object.keys(body).length === 0) { this.editSubmitting = false; return; }
        const res = await fetch('/api/tickets/' + this.selectedTicket.id, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        if (res.ok) {
          const updated = await res.json();
          const idx = this.tickets.findIndex(t => t.id === updated.id);
          if (idx >= 0) this.tickets[idx] = this.boardEntry(updated);
          // A save flushed on the way out resolves after the panel closed or
          // selected another ticket, and an unguarded assignment reopens it.
          if (this.selectedTicket?.id === updated.id) {
            this.selectedTicket = updated;
            this.editSaved = true;
            setTimeout(() => { this.editSaved = false; }, 1500);
          }
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

    // Cancel the pending debounce and save now. saveEdit reads selectedTicket
    // and editForm before its first await, so a caller may clear editor state
    // as soon as this returns without dropping the in-flight request.
    flushEditSave() {
      if (this._editDebounce) {
        clearTimeout(this._editDebounce);
        this._editDebounce = null;
      }
      this.saveEdit();
    },

    // Enter body edit mode with the clicked block left where it is on screen.
    beginBodyEdit(event) {
      var anchor = this._blockAnchor(event.target);
      this.editingBody = true;
      // Alpine runs x-init before x-model, so the textarea carries no value
      // until the flush that $nextTick waits for.
      this.$nextTick(() => this._anchorEditor(anchor));
    },

    // Leave body edit mode with the block holding the caret left where it is.
    exitBodyEdit() {
      var anchor = this._caretAnchor(this.$refs.bodyEditor);
      this.flushEditSave();
      this.editingBody = false;
      this.$nextTick(() => this._anchorPreview(anchor));
    },

    // A blur while the window still has focus is a click elsewhere in the page:
    // save and collapse to the preview. Losing the window is an application
    // switch, so save but stay in edit mode and resume in place on return.
    blurBodyEdit() {
      if (!document.hasFocus()) {
        this.flushEditSave();
        return;
      }
      this.exitBodyEdit();
    },

    // Where the clicked rendered block sits, and which source offset produced
    // it. A click on the wrapper rather than on a block, or a body whose token
    // stream does not line up with the rendered elements, leaves offset null
    // and takes the ratio-preserving fallback.
    _blockAnchor(target) {
      var prose = this.$refs.bodyPreview;
      var scroller = this.closestScroller(prose || target);
      if (!scroller) return null;
      var anchor = { scroller: scroller, top: scroller.scrollTop, height: scroller.scrollHeight, offset: null, y: 0 };
      var offsets = this.bodyBlockOffsets(this.editForm.body);
      if (!prose || !offsets || offsets.length !== prose.children.length) return anchor;
      var block = target;
      while (block && block.parentElement !== prose) block = block.parentElement;
      var idx = block ? Array.prototype.indexOf.call(prose.children, block) : -1;
      if (idx < 0) return anchor;
      anchor.offset = offsets[idx];
      anchor.y = this.topWithin(block, scroller) - scroller.scrollTop;
      return anchor;
    },

    // Where the block holding the caret sits inside the textarea. count is the
    // number of source blocks, which the preview checks against its own child
    // count before trusting index.
    _caretAnchor(el) {
      var scroller = el && this.closestScroller(el);
      if (!scroller) return null;
      var anchor = { scroller: scroller, top: scroller.scrollTop, height: scroller.scrollHeight, index: -1, count: 0, y: 0 };
      var offsets = this.bodyBlockOffsets(this.editForm.body);
      if (!offsets || !offsets.length) return anchor;
      var caret = el.selectionStart || 0;
      var idx = 0;
      for (var i = 0; i < offsets.length && offsets[i] <= caret; i++) idx = i;
      anchor.index = idx;
      anchor.count = offsets.length;
      // topWithin adds the panel's scroll position and this subtracts it again,
      // so the two must be the same number: anchor.top, not a fresh read taken
      // after the measurement.
      anchor.y = this.topWithin(el, scroller) + this.caretLineTop(el, offsets[idx], scroller) - anchor.top;
      return anchor;
    },

    _anchorEditor(anchor) {
      var el = this.$refs.bodyEditor;
      if (!el) return;
      this.autoGrowBody(el);
      var offset = 0;
      if (anchor && anchor.offset === null) {
        this._restoreScrollRatio(anchor);
      } else if (anchor) {
        offset = anchor.offset;
        this._scrollTo(anchor.scroller, this.topWithin(el, anchor.scroller) + this.caretLineTop(el, offset, anchor.scroller) - anchor.y);
      }
      // caretLineTop assigns .value, which resets the selection and clears the
      // native undo stack, so the caret goes in last.
      el.focus({ preventScroll: true });
      el.setSelectionRange(offset, offset);
    },

    _anchorPreview(anchor) {
      if (!anchor) return;
      var prose = this.$refs.bodyPreview;
      // One offset per rendered child, or the indexes name the wrong elements:
      // sanitizing drops an HTML comment that the lexer counted as a block, and
      // every block after it shifts by one.
      var trusted = !!prose && anchor.count === prose.children.length;
      var block = trusted && anchor.index >= 0 ? prose.children[anchor.index] : null;
      if (!block) {
        this._restoreScrollRatio(anchor);
        return;
      }
      this._scrollTo(anchor.scroller, this.topWithin(block, anchor.scroller) - anchor.y);
    },

    // Fallback for every case with no exact anchor: hold the same fraction of
    // the document rather than letting the swap clamp the reader to the top.
    _restoreScrollRatio(anchor) {
      var height = anchor.scroller.scrollHeight;
      this._scrollTo(anchor.scroller, anchor.height ? anchor.top * height / anchor.height : anchor.top);
    },

    // Clamp here rather than leaving it to the browser. The textarea is shorter
    // than the rendered preview, so an entry target for a block near the end
    // lands past the maximum.
    _scrollTo(scroller, top) {
      scroller.scrollTop = Math.max(0, Math.min(top, scroller.scrollHeight - scroller.clientHeight));
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

    // Changing the stage is a bookkeeping edit on the ticket file, so it is
    // open on any parked ticket. in_progress is the exception: an agent owns
    // the worktree and the file. archived tickets never reach the board, and
    // legacy closed ones come from the foreign ticket CLI, so neither gets an
    // affordance.
    canSetStage() {
      const status = this.selectedTicket?.status;
      if (!status) return false;
      return ['open', 'todo', 'paused', 'human_review', 'done', 'cancelled']
        .concat(this.configCache?.custom_statuses || [])
        .includes(status);
    },

    // A ticket that gained a pipeline through the detail form has no stage yet.
    // The first stage is where a run would start, so show that, but leave the
    // file alone until the user picks a value.
    displayStage() {
      return this.selectedTicket?.stage || this.selectedTicket?.stages?.[0] || '';
    },

    async setStage(stage) {
      // The empty value is the picker's placeholder, not a stage.
      if (!this.selectedTicket || !stage) return;
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
        if (idx >= 0) this.tickets[idx] = this.boardEntry(updated);
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

    // Classes for one stepper pill in the stages row. Completed stages get the
    // green tint treatment, the active stage a solid status-colored pill with
    // dark text, queued stages stay dimmed.
    stageStyle(stage, ticket) {
      if (!ticket || !ticket.stages) return 'text-surface-600 border-surface-700/60';
      var stageIdx = ticket.stages.indexOf(stage);
      var currentIdx = ticket.stages.indexOf(ticket.stage);
      var doneCls = 'text-st-done bg-st-done/[0.08] border-st-done/[0.24]';
      if (ticket.status === 'done') return doneCls;
      if (stage === ticket.stage) {
        if (ticket.status === 'paused') return 'text-st-paused bg-st-paused/[0.12] border-st-paused/[0.3]';
        if (ticket.status === 'in_progress') return 'stage-active-progress';
        if (ticket.status === 'human_review') return 'stage-active-review';
        return 'text-tx-2 bg-surface-800 border-edge-input';
      }
      if (currentIdx >= 0 && stageIdx >= 0 && stageIdx < currentIdx) return doneCls;
      return 'text-surface-600 border-surface-700/60 opacity-70';
    },

    // Whether a stage is behind the current one (renders with a ✓ prefix).
    isStageDone(stage, ticket) {
      if (!ticket || !ticket.stages) return false;
      if (ticket.status === 'done') return true;
      var si = ticket.stages.indexOf(stage);
      var ci = ticket.stages.indexOf(ticket.stage);
      return ci >= 0 && si >= 0 && si < ci;
    },

    statusLabel(status) {
      return (status || '').replace(/_/g, ' ');
    },

    // Duration between two timestamps, for history rows ("38m", "1h 4m", "45s").
    formatElapsed(start, end) {
      if (!start || !end) return '';
      var secs = Math.floor((new Date(end) - new Date(start)) / 1000);
      if (isNaN(secs) || secs < 0) return '';
      if (secs < 60) return secs + 's';
      var mins = Math.floor(secs / 60);
      if (mins < 60) return mins + 'm';
      return Math.floor(mins / 60) + 'h ' + (mins % 60) + 'm';
    },

    // "⌁ attach tmux session": running tickets jump to the live terminal,
    // otherwise the attach command is copied.
    attachTerminal() {
      if (!this.selectedTicket) return;
      if (this.selectedTicket.status === 'in_progress') {
        this.logViewContent = null;
        this.logViewStage = null;
        this.switchTab('session');
      } else {
        this.copyCmd('kontora attach ' + this.selectedTicket.id);
      }
    },

    // Follow (autoscroll) toggle for the log pane. The live terminal already
    // sticks to the bottom while it is at the bottom; enabling re-pins it.
    logFollow: true,
    toggleLogFollow() {
      this.logFollow = !this.logFollow;
      if (this.logFollow) this.scrollLogToEnd();
    },
    scrollLogToEnd() {
      if (this.terminalOpen && termState.term) {
        try { termState.term.scrollToBottom(); } catch (e) {}
        return;
      }
      var el = document.getElementById('activity-scroll') || document.getElementById('stage-log-pre');
      if (el) el.scrollTop = el.scrollHeight;
    },

    // Open the currently viewed log as plain text in a new tab.
    async openRawLog() {
      if (!this.selectedTicket) return;
      var content = this.logViewContent;
      if (content === null && this.activity && !this.activity.tape) content = this.activity.content;
      if (content === null || content === undefined) {
        try {
          var url = '/api/tickets/' + this.selectedTicket.id + '/logs';
          var stage = this.activityStage || this.logViewStage || this.selectedTicket.stage;
          if (stage) url += '?stage=' + encodeURIComponent(stage);
          var res = await fetch(url);
          if (res.ok) {
            var data = await res.json();
            content = data.content || '';
          }
        } catch (e) { /* fall through to the guard below */ }
      }
      if (content === null || content === undefined) {
        this.error = 'No log available yet';
        return;
      }
      var blob = new Blob([content], { type: 'text/plain' });
      window.open(URL.createObjectURL(blob));
    },

    // ─── Imperative board card rendering ───
    // The desktop card list is built as HTML strings and injected with innerHTML
    // instead of an Alpine x-for, so a board with hundreds of tickets carries no
    // per-card reactive effects. One delegated handler (_bindBoardEvents) drives
    // all card interactions; selection and the open menu are class toggles.

    // Repaint the layer under the editor's transparent text. The typed
    // character only becomes visible when this runs, so it cannot be deferred
    // past the frame that draws it; one frame's worth of coalescing collapses
    // repeated effect runs into a single pass over the body, which costs 1.6ms
    // at 50KB. On a highlighter failure the text still has to be readable, so
    // fall back to the plain source rather than leaving the layer empty.
    setEditorHighlight(el, src) {
      var text = src || '';
      if (el._hlSrc === text) return;
      el._hlSrc = text;
      if (el._hlFrame) return;
      el._hlFrame = requestAnimationFrame(function () {
        el._hlFrame = 0;
        try {
          el.innerHTML = highlightMarkdown(el._hlSrc);
        } catch (e) {
          el.textContent = el._hlSrc;
        }
      });
    },

    // Height from content, so the panel stays the only scroller while editing.
    // Collapsing the box to its 'auto' floor shrinks the panel's content and
    // the browser clamps the panel's scrollTop to fit; growing the box back
    // does not undo the clamp. This runs on every keystroke, so without the
    // restore a single character sends the reader to the top of the ticket.
    autoGrowBody(el) {
      if (!el) return;
      var scroller = this.closestScroller(el);
      var savedScroll = scroller ? scroller.scrollTop : 0;
      el.style.height = 'auto';
      el.style.height = el.scrollHeight + 'px';
      if (scroller) scroller.scrollTop = savedScroll;
    },

    // The element that actually scrolls the ticket body. A stacked panel turns
    // an extra ancestor into a scroller, so resolve it from the node rather
    // than hardcoding one container. Do not also require that the ancestor
    // overflows: during the preview/editor swap it transiently does not, and
    // the walk would run past it to <body>.
    closestScroller(el) {
      for (var n = el && el.parentElement; n; n = n.parentElement) {
        var oy = window.getComputedStyle(n).overflowY;
        if (oy === 'auto' || oy === 'scroll') return n;
      }
      return null;
    },

    // Distance from the scroller's scrollable origin to el's top. Anchor and
    // target go through this one helper so border and padding cancel out.
    topWithin(el, scroller) {
      return el.getBoundingClientRect().top
        - scroller.getBoundingClientRect().top
        - scroller.clientTop
        + scroller.scrollTop;
    },

    // Source offset of each top-level rendered block, or null when the source
    // and the token stream cannot be lined up.
    //
    // Do not replace the cursor walk with a running sum of raw.length. marked
    // consumes a top-level link-reference definition ("[ref]: https://…")
    // without pushing a token, which silently shortens every later offset, and
    // it appends a synthetic newline to raw in some paragraph sequences, after
    // which raw is no longer a substring of the source. Re-finding each raw
    // heals the first case and gives up on the second.
    //
    // The caller must still compare the length against the rendered container's
    // children. DOMPurify drops comment nodes, so a markdown comment is one
    // token and no element, and every offset after it names the wrong block.
    bodyBlockOffsets(md) {
      var src = (md || '').replace(/\r\n|\r/g, '\n');
      if (this._blockOffsetsSrc === src) return this._blockOffsetsFor;
      var offsets = [];
      try {
        var tokens = marked.lexer(src).filter(function (t) { return t.type !== 'space'; });
        var cursor = 0;
        for (var i = 0; i < tokens.length; i++) {
          var raw = tokens[i].raw;
          var off = cursor;
          if (!raw) { offsets = null; break; }
          if (src.substr(cursor, raw.length) !== raw) {
            off = src.indexOf(raw, cursor);
            if (off < 0) { offsets = null; break; }
          }
          offsets.push(off);
          cursor = off + raw.length;
        }
      } catch (e) {
        offsets = null;
      }
      this._blockOffsetsSrc = src;
      this._blockOffsetsFor = offsets;
      return offsets;
    },

    // Y of the line holding source offset off, measured from the textarea's
    // border-box top. Callers that already resolved the panel pass it in, so
    // the measurement and the anchor arithmetic use the same node.
    caretLineTop(el, off, scroller) {
      var cs = window.getComputedStyle(el);
      var padTop = parseFloat(cs.paddingTop) || 0;
      var padBottom = parseFloat(cs.paddingBottom) || 0;
      var full = el.value;
      var savedHeight = el.style.height;
      var savedMinHeight = el.style.minHeight;
      // The textarea is what makes the panel scrollable, so zeroing its height
      // for the measurement shrinks the panel's content and the browser clamps
      // the panel's scrollTop to fit. Restoring the height grows the content
      // back but leaves the scroll position clamped.
      if (scroller === undefined) scroller = this.closestScroller(el);
      var savedScroll = scroller ? scroller.scrollTop : 0;
      // scrollHeight is max(contentHeight, clientHeight) and auto-grow has
      // already set the box to the full content height, so without zeroing it
      // every prefix measures the same number. The min-height floor has to go
      // with the height: it holds the box at 100px, which is taller than the
      // first few lines, so every short prefix measures the floor and the line
      // height comes out as the floor instead of one line. The sentinel
      // character matters because block offsets are line starts: the prefix
      // ends in "\n" and engines disagree on whether that trailing empty line
      // counts.
      el.style.height = '0px';
      el.style.minHeight = '0px';
      var measure = function (text) { el.value = text; return el.scrollHeight; };
      // getComputedStyle returns "normal" for an unset line-height, so derive
      // it from a single rendered line instead.
      var lineHeight = measure('x') - padTop - padBottom;
      var height = measure(full.slice(0, off) + 'x');
      el.value = full;
      el.style.height = savedHeight;
      el.style.minHeight = savedMinHeight;
      if (scroller) scroller.scrollTop = savedScroll;
      // scrollHeight includes both paddings and excludes the border.
      return (parseFloat(cs.borderTopWidth) || 0) + height - padBottom - lineHeight;
    },

    // Lexical colorizer for the stage-log pane: "> tool …" lines get an accent
    // marker and a highlighted tool name, "└ …" result lines and "[banner]"
    // lines dim. Every character passes through _escapeHtml; no parsing of
    // agent-specific formats beyond these three line shapes.
    renderLogHTML(content) {
      var esc = (s) => this._escapeHtml(s);
      return (content || '').split('\n').map(function (line) {
        if (/^>\s/.test(line)) {
          var rest = line.slice(2);
          var sp = rest.indexOf(' ');
          var tool = sp < 0 ? rest : rest.slice(0, sp);
          var tail = sp < 0 ? '' : rest.slice(sp);
          return '<span class="log-marker">&gt;</span> <span class="log-tool">' + esc(tool) + '</span>' + esc(tail);
        }
        if (/^\s*└/.test(line)) return '<span class="log-dim">' + esc(line) + '</span>';
        if (/^\[.*\]\s*$/.test(line)) return '<span class="log-banner">' + esc(line) + '</span>';
        return esc(line);
      }).join('\n');
    },

    // ---- activity tape -----------------------------------------------------

    async submitNote() {
      var text = (this.noteDraft || '').trim();
      if (!text || !this.selectedTicket || this.noteSubmitting) return;
      var id = this.selectedTicket.id;
      this.noteSubmitting = true;
      try {
        var res = await fetch('/api/tickets/' + encodeURIComponent(id) + '/note', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ text: text }),
        });
        if (!res.ok) {
          var err = await res.json().catch(function () { return {}; });
          this.error = err.error || 'Failed to add note';
        } else {
          var full = await res.json();
          this.noteDraft = '';
          if (this.selectedTicket && this.selectedTicket.id === id) this.selectedTicket = full;
          var idx = this.tickets.findIndex(function (x) { return x.id === id; });
          if (idx >= 0) { this.tickets[idx] = this.boardEntry(full); this.recomputeBoard(); }
        }
      } catch (e) {
        this.error = 'Failed to add note';
      }
      this.noteSubmitting = false;
    },
  };
}
