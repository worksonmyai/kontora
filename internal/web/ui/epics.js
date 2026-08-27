// Epics: the rail section, the roll-up derivation behind it and the board card
// spine, and the epic page.
//
// Every key here is prefixed `epic` or `_epic`. merge() in index.js throws on a
// repeat, and activity.js already owns the unprefixed row helpers.

// Segments the rail meter draws. The row is one line in a 220px rail, and an
// epic can hold far more children than fit; the n/m count carries the true
// total. Same reason CHAIN_METER_CAP exists in relations.js.
var EPIC_METER_CAP = 14;

// The status hue each meter segment and group header takes. A status with no
// entry falls back to the surface tone, which is what an unknown custom status
// gets.
var EPIC_STATUS_HUE = {
  open: '--st-open',
  todo: '--st-progress',
  in_progress: '--st-progress',
  paused: '--st-paused',
  human_review: '--st-review',
  done: '--st-done',
  // A ticket carrying the legacy "closed" status counts as finished, which is
  // what ticket.DeriveEpicStatus does with it, so the meter has to say so too.
  closed: '--st-done',
  cancelled: '--st-cancel',
};

// The statuses the roll-up counts as finished work, the same set
// ticket.DeriveEpicStatus treats as done.
var EPIC_DONE_STATUSES = { done: true, closed: true };

// Group order on the epic page: what is moving, then what is waiting, then
// what is finished.
// dropStatus is what a drop into the group asks for, the same value the board's
// own columns carry: moveTask posts it to /move, and the daemon turns todo into
// a run or a retry.
var EPIC_GROUPS = [
  { key: 'in_progress', label: 'in flight', statuses: ['in_progress', 'todo', 'paused'], dropStatus: 'todo' },
  { key: 'human_review', label: 'review', statuses: ['human_review'], dropStatus: 'human_review' },
  { key: 'open', label: 'open', statuses: ['open'], dropStatus: 'open' },
  { key: 'done', label: 'done', statuses: ['done', 'closed', 'cancelled'], dropStatus: 'done' },
];

export function kontoraEpics() {
  return {
    // Every epic in this.tickets, filled by recomputeBoard's one pass.
    epics: [],
    // The open epic page, or null. Like selectedTicket this is an overlay
    // driver, not a currentView.
    selectedEpic: null,
    epicTab: 'children',
    epicGroupCollapsed: {},
    epicAddDraft: '',
    epicAddBusy: false,
    epicDragging: false,
    // The brief editor. It is the whole body in one textarea, the way a
    // ticket's is, and it borrows editForm.body so _blockAnchor, _anchorEditor
    // and autoGrowBody work here unchanged rather than being written twice.
    // The two overlays never stand at once, and startEditing rebuilds editForm
    // from scratch whenever a ticket page opens, so neither can read the
    // other's draft.
    epicEditingBrief: false,
    epicBriefSaving: false,
    epicBriefSaved: false,
    _epicBriefLead: '',
    _epicBriefDebounce: null,
    // The last detail fetch, which is where the body and the notes come from.
    // A board recompute rebuilds selectedEpic out of the list, which carries
    // neither, so the two are merged rather than one replacing the other.
    _epicDetail: null,
    // Roll-up per epic id, rebuilt in the same pass. It is an imperative cache
    // for the reason _board is one: a template read has to be O(1).
    _epicRollup: {},

    // ---- derivation -------------------------------------------------------

    // Fill _epicRollup from the epics collected in this pass. Called from
    // recomputeBoard so the rail, the cards and the page all move on one
    // repaint.
    _epicRecompute(epics) {
      var byParent = Object.create(null);
      for (var i = 0; i < this.tickets.length; i++) {
        var t = this.tickets[i];
        var parent = t.parent && t.parent.id;
        if (!parent || t.kind === 'epic') continue;
        (byParent[parent] || (byParent[parent] = [])).push(t);
      }
      var rollup = {};
      for (var j = 0; j < epics.length; j++) {
        var epic = epics[j];
        rollup[epic.id] = this._epicRollupFor(epic, byParent[epic.id] || []);
      }
      this.epics = epics;
      this._epicRollup = rollup;
    },

    // One epic's roll-up. Archived children never reach this.tickets, so the
    // "ignore archived" half of the daemon's rule needs nothing here.
    _epicRollupFor(epic, children) {
      // childRowFromEvent derives what the sub-ticket table's stage and elapsed
      // columns read — stage position and the wall bounds of the whole run —
      // from a board ticket, which carries neither. Done once here so the
      // table, the "next up" band and the holder pill all get it.
      var self = this;
      var ordered = this._epicOrderChildren(epic, children).map(function (c) {
        return Object.assign({}, c, self.childRowFromEvent(c));
      });
      var counts = Object.create(null);
      var done = 0;
      for (var i = 0; i < ordered.length; i++) {
        var s = ordered[i].status;
        counts[s] = (counts[s] || 0) + 1;
        if (EPIC_DONE_STATUSES[s]) done++;
      }
      var total = ordered.length;
      return {
        id: epic.id,
        title: epic.title || epic.id,
        done: done,
        total: total,
        pct: total ? Math.round((done / total) * 100) : 0,
        counts: counts,
        children: ordered,
        segs: ordered.slice(0, EPIC_METER_CAP).map(function (c) {
          return { id: c.id, hue: EPIC_STATUS_HUE[c.status] || null };
        }),
        next: this._epicNext(ordered),
        holder: this._epicHolderIn(ordered),
      };
    },

    // The epic's children in the order the epic asks for: its child_order list
    // first, then whatever it does not name, by created. The same rule
    // ticket.EpicChildren applies on the server, so a drag and a reload agree.
    _epicOrderChildren(epic, children) {
      var rank = Object.create(null);
      var order = (epic && epic.child_order) || [];
      for (var i = 0; i < order.length; i++) {
        if (rank[order[i]] === undefined) rank[order[i]] = i;
      }
      return children.slice().sort(function (a, b) {
        var ra = rank[a.id], rb = rank[b.id];
        if (ra !== undefined && rb !== undefined && ra !== rb) return ra - rb;
        if ((ra !== undefined) !== (rb !== undefined)) return ra !== undefined ? -1 : 1;
        var ca = a.created_at || '', cb = b.created_at || '';
        if (ca !== cb) return ca < cb ? -1 : 1;
        return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
      });
    },

    // What is moving now: the running child, or the queued one behind it.
    _epicNext(children) {
      return children.find(function (c) { return c.status === 'in_progress'; })
        || children.find(function (c) { return c.status === 'todo'; })
        || null;
    },

    // The child that stops the epic closing: one waiting on a person first,
    // then one a person paused. A running child holds nothing — it is working.
    _epicHolderIn(children) {
      return children.find(function (c) { return c.status === 'human_review'; })
        || children.find(function (c) { return c.status === 'paused'; })
        || null;
    },

    // ---- reads ------------------------------------------------------------

    // O(1) roll-up read. Every template goes through here, so an epic that is
    // not loaded yet reads as an empty one rather than throwing.
    epicRollup(id) {
      return this._epicRollup[id] || { id: id, done: 0, total: 0, pct: 0, counts: {}, children: [], segs: [], next: null, holder: null };
    },

    // What a board card needs of the epic above it, or null when the ticket is
    // in none. It reads the roll-up cache rather than scanning tickets, because
    // _cardHTML calls it once per card. _cardSig reads the same cache, so a
    // sibling finishing repaints this card too.
    epicCardInfo(ticket) {
      var id = (ticket && ticket.parent && ticket.parent.id) || '';
      return this._epicRollup[id] || null;
    },

    // The rail's note line, one sentence about why the epic is not finished.
    epicNote(id) {
      var r = this.epicRollup(id);
      if (r.holder) return r.holder.id + ' holds it';
      if (r.next) return r.next.id + ' is running';
      if (r.total && r.done + (r.counts.cancelled || 0) === r.total) return 'ready to close';
      if (!r.total) return 'no sub-tickets';
      return 'nothing running';
    },

    epicNoteClass(id) {
      var r = this.epicRollup(id);
      if (r.holder) return 'text-st-review';
      if (r.next) return 'text-st-progress';
      if (r.total && r.done + (r.counts.cancelled || 0) === r.total) return 'text-st-done';
      return 'text-surface-600';
    },

    // "1 running · 1 review · 2 open" under the donut. Statuses with no child
    // are left out, so the line stays one row.
    epicBreakdown(id) {
      var counts = this.epicRollup(id).counts;
      var parts = [];
      var say = function (n, word) { if (n) parts.push(n + ' ' + word); };
      say(counts.in_progress, 'running');
      say(counts.todo, 'queued');
      say(counts.paused, 'paused');
      say(counts.human_review, 'review');
      say(counts.open, 'open');
      say(counts.cancelled, 'cancelled');
      return parts.join(' · ');
    },

    // The donut's conic-gradient. Written whole rather than assembled from a
    // Tailwind class, because Tailwind scans for literal class strings.
    epicDonutStyle(id) {
      var turn = (this.epicRollup(id).pct / 100).toFixed(4);
      return 'background: conic-gradient(hsl(var(--st-done)) 0turn ' + turn + 'turn, rgb(var(--surface-700)) ' + turn + 'turn 1turn)';
    },

    epicStatusHue(status) {
      return EPIC_STATUS_HUE[status] || null;
    },

    // The sub-ticket table's groups, empty ones dropped. The open group is the
    // one that takes the inline add row.
    epicGroups() {
      var children = this.epicRollup(this.selectedEpic ? this.selectedEpic.id : '').children;
      var self = this;
      return EPIC_GROUPS.map(function (g) {
        var rows = children.filter(function (c) { return g.statuses.indexOf(c.status) >= 0; });
        return {
          key: g.key,
          label: g.label,
          hue: EPIC_STATUS_HUE[g.key],
          count: rows.length,
          addRow: g.key === 'open',
          rows: rows.map(function (c, i) {
            return Object.assign({}, c, { ord: i + 1, titleColor: self._epicRowTitleClass(c) });
          }),
        };
      }).filter(function (g) { return g.count > 0 || g.addRow; });
    },

    _epicRowTitleClass(c) {
      if (c.status === 'cancelled') return 'text-surface-600 line-through decoration-surface-600/60';
      if (c.status === 'done') return 'text-tx-3';
      return 'text-tx';
    },

    // ---- the page ---------------------------------------------------------

    epicPageOpen() {
      return !!this.selectedEpic;
    },

    // The board list carries no body, and the brief is the body. The row is
    // shown first and the fetch fills the brief in, which is what selectTicket
    // does for a ticket page.
    async openEpic(id) {
      var epic = (this.tickets || []).find(function (t) { return t.id === id && t.kind === 'epic'; });
      if (!epic) return;
      // The ticket page and the epic page are two overlays over one board, so
      // opening either closes the other.
      if (this.selectedTicket) this.closeDetail();
      this.selectedEpic = epic;
      this.epicTab = 'children';
      this.epicAddDraft = '';
      this.epicEditingBrief = false;
      this.writeHash();
      await this.epicFetchDetail(id);
    },

    // Fetch the epic's own detail: the body the brief renders and the notes
    // the daemon parsed out of it. Kept in _epicDetail rather than written
    // onto selectedEpic, because every board recompute replaces that object
    // with the list entry, which has neither.
    async epicFetchDetail(id) {
      try {
        var res = await fetch('/api/tickets/' + encodeURIComponent(id));
        if (!res.ok) return;
        var full = await res.json();
        this._epicDetail = full;
        if (this.selectedEpic && this.selectedEpic.id === full.id) {
          this.selectedEpic = Object.assign({}, this.selectedEpic, { body: full.body, notes: full.notes });
          this._epicLoadBrief(full.body);
        }
      } catch (e) {
        this.error = 'Failed to load the epic brief';
      }
    },

    closeEpic() {
      if (!this.selectedEpic) return;
      // A brief edited up to the moment the page closes is saved, the way
      // blurBodyEdit saves a ticket's body on the way out.
      this.epicFlushBrief();
      this.selectedEpic = null;
      this._epicDetail = null;
      this.epicAddDraft = '';
      this.epicEditingBrief = false;
      this.writeHash();
    },

    // Re-read the open epic out of this.tickets after an SSE flush, so the page
    // moves with the rail rather than holding the object it opened with.
    epicRefreshSelected() {
      if (!this.selectedEpic) return;
      var id = this.selectedEpic.id;
      var fresh = (this.tickets || []).find(function (t) { return t.id === id; });
      if (!fresh) {
        this.closeEpic();
        return;
      }
      if (fresh.kind !== 'epic') {
        // The kind was edited away underneath the page. There is no epic left
        // to render, so the overlay closes rather than showing an empty one.
        this.closeEpic();
        return;
      }
      var detail = this._epicDetail && this._epicDetail.id === id ? this._epicDetail : null;
      this.selectedEpic = detail
        ? Object.assign({}, fresh, { body: detail.body, notes: detail.notes })
        : fresh;
    },

    // ---- writes -----------------------------------------------------------

    // Re-derived at click time rather than read off the rendered pill, the way
    // resumeChainHolder is: the stream can have moved the holder on since the
    // row was drawn.
    async epicApproveHolder() {
      var r = this.epicRollup(this.selectedEpic ? this.selectedEpic.id : '');
      if (!r.holder) return;
      await this.moveTicketVia(r.holder.id, 'done', null);
    },

    // Queue every open child of the epic. Each is one run request, the same one
    // the board's own move posts.
    async epicQueueOpen() {
      var r = this.epicRollup(this.selectedEpic ? this.selectedEpic.id : '');
      var open = r.children.filter(function (c) { return c.status === 'open'; });
      for (var i = 0; i < open.length; i++) {
        await this.moveTicketVia(open[i].id, 'run', null);
      }
    },

    // One JSON request, with the failure reported the way moveTicketVia
    // reports one: the daemon's own message on this.error, and no throw, so a
    // refused drag leaves the page usable. Returns whether it landed.
    async _epicRequest(method, url, body) {
      this.error = null;
      try {
        var opts = { method: method };
        if (body !== undefined) {
          opts.headers = { 'Content-Type': 'application/json' };
          opts.body = JSON.stringify(body);
        }
        const res = await fetch(url, opts);
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || (method + ' ' + url + ' failed');
          return false;
        }
        return true;
      } catch (e) {
        this.error = 'Request failed: ' + e.message;
        return false;
      }
    },

    // Write the epic's child order. One file, the epic's: membership is what
    // each child's parent says, so no child is touched.
    async epicSaveOrder(ids) {
      if (!this.selectedEpic) return false;
      return this._epicRequest('PUT', '/api/tickets/' + encodeURIComponent(this.selectedEpic.id) + '/children', { children: ids });
    },

    // File an existing ticket under the open epic.
    async epicPullIn(ticketID) {
      if (!this.selectedEpic) return false;
      return this._epicRequest('POST', '/api/tickets/' + encodeURIComponent(ticketID) + '/parent', { parent: this.selectedEpic.id });
    },

    // Take one child out of the epic. The ticket survives; it stops being part
    // of this brief.
    async epicMoveOut(ticketID) {
      return this._epicRequest('POST', '/api/tickets/' + encodeURIComponent(ticketID) + '/unparent');
    },

    // The inline add row and "+ sub-ticket" both land here: one POST
    // /api/tickets carrying the epic's parent and path, which is what
    // submitCreateTicket posts for an ordinary ticket.
    async epicAddChild() {
      var title = this.epicAddDraft.trim();
      if (!title || !this.selectedEpic || this.epicAddBusy) return;
      this.epicAddBusy = true;
      try {
        var ok = await this._epicRequest('POST', '/api/tickets', {
          title: title,
          path: this.selectedEpic.path,
          parent: this.selectedEpic.id,
          status: 'open',
        });
        if (ok) this.epicAddDraft = '';
      } finally {
        this.epicAddBusy = false;
      }
    },

    // ---- the brief ---------------------------------------------------------

    // Seed the editor from the body the fetch returned. The leading newline is
    // held aside and put back on save, so a round-trip through the editor does
    // not move the first line.
    _epicLoadBrief(body) {
      var text = body || '';
      this._epicBriefLead = text.startsWith('\n') ? '\n' : '';
      this.editForm.body = text.slice(this._epicBriefLead.length);
    },

    // A click on a rendered section opens the editor with that section left
    // where it is on screen. There are no per-section editors: the anchor is
    // the whole of what per-block means here.
    epicBeginBriefEdit(event) {
      if (!this.selectedEpic) return;
      var anchor = this._blockAnchor(event.target);
      this.epicEditingBrief = true;
      this.$nextTick(() => this._anchorEditor(anchor));
    },

    epicExitBriefEdit() {
      var anchor = this._caretAnchor(this.$refs.bodyEditor);
      this.epicFlushBrief();
      this.epicEditingBrief = false;
      this.$nextTick(() => this._anchorPreview(anchor));
    },

    // A blur while the window still has focus is a click elsewhere on the page.
    // Losing the window is an application switch, so the draft is saved but the
    // editor stays open, which is what blurBodyEdit does for a ticket.
    epicBlurBriefEdit() {
      if (!document.hasFocus()) {
        this.epicFlushBrief();
        return;
      }
      this.epicExitBriefEdit();
    },

    epicDebounceBrief() {
      if (this._epicBriefDebounce) clearTimeout(this._epicBriefDebounce);
      this._epicBriefDebounce = setTimeout(() => this.epicSaveBrief(), 800);
    },

    epicFlushBrief() {
      if (this._epicBriefDebounce) {
        clearTimeout(this._epicBriefDebounce);
        this._epicBriefDebounce = null;
      }
      if (this.epicEditingBrief) this.epicSaveBrief();
    },

    // One PUT carrying the body, which is the same request the ticket page's
    // saveEdit makes. The epic is editable in every status it can derive,
    // because no agent ever owns its file.
    async epicSaveBrief() {
      if (!this.selectedEpic) return;
      var id = this.selectedEpic.id;
      var body = this._epicBriefLead + this.editForm.body;
      if (this._epicDetail && this._epicDetail.id === id && this._epicDetail.body === body) return;
      this.epicBriefSaving = true;
      this.epicBriefSaved = false;
      var ok = await this._epicRequest('PUT', '/api/tickets/' + encodeURIComponent(id), { body: body });
      this.epicBriefSaving = false;
      if (!ok) return;
      this.epicBriefSaved = true;
      if (this._epicDetail && this._epicDetail.id === id) this._epicDetail.body = body;
      if (this.selectedEpic && this.selectedEpic.id === id) {
        this.selectedEpic = Object.assign({}, this.selectedEpic, { body: body });
      }
    },

    epicToggleGroup(key) {
      this.epicGroupCollapsed[key] = !this.epicGroupCollapsed[key];
    },

    // Sortable over one status group. A drop inside the group writes the epic's
    // children list; a drop across a group boundary is a status move, the same
    // thing the board's own drop posts. The two are exclusive, so a cross-group
    // drop writes no order: the move re-derives the list on the next pass.
    //
    // The animation is left on: a group is at most the epic's children, which
    // is far below the board's ANIM_THRESHOLD of 25 in any epic worth grouping.
    epicInitSortable(el) {
      if (this.isMobile) return;
      var self = this;
      new Sortable(el, {
        group: 'epic-children',
        animation: 150,
        handle: '.epic-row-handle',
        draggable: '.epic-row:not(.epic-row-add)',
        ghostClass: 'sortable-ghost',
        dragClass: 'sortable-drag',
        onStart: function () { self.epicDragging = true; },
        onEnd: async function (evt) {
          self.epicDragging = false;
          var childID = evt.item.dataset.childId;
          if (!childID) return;
          var from = evt.from.dataset.epicGroup;
          var to = evt.to.dataset.epicGroup;
          if (from !== to) {
            await self.moveTask(childID, self._epicDropStatus(to));
            return;
          }
          await self.epicSaveOrder(self._epicOrderAfterDrop());
        },
      });
    },

    _epicDropStatus(group) {
      var g = EPIC_GROUPS.find(function (x) { return x.key === group; });
      return g ? g.dropStatus : 'open';
    },

    // The order to write after an in-group drop: every group's rows in the
    // order they are now on screen, read out of the DOM rather than out of the
    // model, because Sortable moved the nodes and the model has not caught up.
    _epicOrderAfterDrop() {
      var ids = [];
      document.querySelectorAll('[data-epic-group] [data-child-id]').forEach(function (el) {
        var id = el.dataset.childId;
        if (id && ids.indexOf(id) < 0) ids.push(id);
      });
      return ids;
    },
  };
}
