import { resourceOwner, mountSortableResource } from './board.js';

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
// own columns carry: moveTask posts it to /move without a separate run/retry
// request or resetting the stage's attempt counter.
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
    epicCandidateQuery: '',
    epicCandidateOpen: false,
    epicRemoveTarget: null,
    epicManagementErrors: Object.create(null),
    epicMutationPending: Object.create(null),
    _epicMutationTails: Object.create(null),
    _epicNeedsRefresh: Object.create(null),
    epicPickerMarkup: `
      <div class="epic-management">
        <button type="button" @click="epicCandidateOpen = !epicCandidateOpen" :aria-expanded="epicCandidateOpen" :disabled="!ticketCanControl(selectedEpic)">Add existing ticket</button>
        <template x-if="epicCandidateOpen">
        <div class="epic-candidate-picker">
          <label>Search tickets
            <input type="search" x-model="epicCandidateQuery" @keydown.escape.stop="epicCandidateOpen = false" placeholder="Title, ID, stage or agent">
          </label>
          <p x-show="!epicCandidates(selectedEpic, epicCandidateQuery).length">No matching tickets.</p>
          <ul>
            <template x-for="candidate in epicCandidates(selectedEpic, epicCandidateQuery)" :key="candidate.id">
              <li><button type="button" @click="epicPullIn(candidate.id)" :disabled="epicMutationBusy(selectedEpic)">
                <span x-text="candidate.id + ': ' + (candidate.title || candidate.id)"></span>
                <span class="text-tx-3" x-text="statusLabel(candidate.status) + (candidate.parent?.id ? ' (currently in ' + candidate.parent.id + ')' : '')"></span>
              </button></li>
            </template>
          </ul>
        </div>
        </template>
        <p role="alert" x-show="epicManagementErrors[selectedEpic?.id]" x-text="epicManagementErrors[selectedEpic?.id]"></p>
        <p class="text-tx-3" x-show="epicRetainedOrderRefs().length" x-text="'Stored order also retains: ' + epicRetainedOrderRefs().join(', ')"></p>
      </div>`,
    epicChildActionsMarkup: `
      <div class="epic-child-controls" @click.stop x-show="ticketCanControl(r) && ticketCanControl(selectedEpic)">
        <label>Move to
          <select aria-label="Move child to status" :disabled="lifecycleBusy(r)" @change="epicChildCommand(r, $event.target.value); $event.target.value = ''">
            <option value="">Choose status</option>
            <template x-for="command in lifecycleDestinations(r)" :key="command.id">
              <option :value="command.id" x-text="command.label"></option>
            </template>
          </select>
        </label>
        <template x-for="command in lifecycleActions(r)" :key="command.id">
          <button type="button" @click="epicChildCommand(r, command)" :disabled="lifecycleBusy(r)" x-text="command.label"></button>
        </template>
        <button type="button" @click="epicMoveChild(r.id, -1)" :disabled="epicMutationBusy(selectedEpic) || r.ord === 1" :aria-label="'Move ' + r.id + ' up'">Move up</button>
        <button type="button" @click="epicMoveChild(r.id, 1)" :disabled="epicMutationBusy(selectedEpic) || r.ord === g.count" :aria-label="'Move ' + r.id + ' down'">Move down</button>
        <button type="button" @click="epicConfirmRemove(r.id)" :disabled="epicMutationBusy(selectedEpic)">Remove from epic</button>
        <div x-show="epicRemoveTarget?.id === r.id && epicRemoveTarget?.owner === selectedEpic?.id" role="group" aria-label="Confirm removal from epic">
          <p>Remove from this epic? The ticket itself is kept.</p>
          <button type="button" @click="epicRemoveConfirmed()" :disabled="epicMutationBusy(selectedEpic)">Remove membership</button>
          <button type="button" @click="epicRemoveTarget = null">Keep in epic</button>
        </div>
      </div>`,

    // The brief editor borrows editForm.body so _blockAnchor, _anchorEditor
    // and autoGrowBody work here unchanged rather than being written twice.
    // The two overlays never stand at once, and startEditing rebuilds editForm
    // from scratch whenever a ticket page opens, so neither can read the
    // other's draft.
    epicEditingBrief: false,
    epicBriefSaving: false,
    epicBriefSaved: false,
    _epicBriefLead: '',
    _epicBriefDebounce: null,
    _epicBriefSave: null,
    // The last detail fetch, which is where the body and the notes come from.
    // A board recompute rebuilds selectedEpic out of the list, which carries
    // neither, so the two are merged rather than one replacing the other.
    _epicDetail: null,
    _epicDetailSeq: 0,
    _epicUpdateSeq: 0,
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
        supervisorStatus: this.epicIsAgent(epic) ? epic.status : '',
        supervisorWaiting: this.epicIsAgent(epic) && epic.waiting_for_input,
        done: done,
        total: total,
        pct: total ? Math.round((done / total) * 100) : 0,
        counts: counts,
        activeCount: ordered.filter(c => this.executionActive(c)).length,
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
      return children.find(c => this.executionActive(c))
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
      if (r.supervisorWaiting) return 'supervisor needs input';
      if (r.supervisorStatus) return 'supervisor ' + (r.supervisorStatus === 'todo' ? 'queued' : this.statusLabel(r.supervisorStatus));
      if (r.holder) return r.holder.id + ' holds it';
      if (r.next) return r.next.id + ' is running';
      if (r.total && r.done + (r.counts.cancelled || 0) === r.total) return 'ready to close';
      if (!r.total) return 'no sub-tickets';
      return 'nothing running';
    },

    epicNoteClass(id) {
      var r = this.epicRollup(id);
      if (r.supervisorWaiting) return 'text-st-review';
      if (r.supervisorStatus) {
        return { in_progress: 'text-st-progress', todo: 'text-st-progress', paused: 'text-st-paused',
          human_review: 'text-st-review', done: 'text-st-done', cancelled: 'text-st-cancel' }[r.supervisorStatus] || 'text-surface-600';
      }
      if (r.holder) return 'text-st-review';
      if (r.next) return 'text-st-progress';
      if (r.total && r.done + (r.counts.cancelled || 0) === r.total) return 'text-st-done';
      return 'text-surface-600';
    },

    // "1 running · 1 review · 2 open" under the donut. Statuses with no child
    // are left out, so the line stays one row.
    epicBreakdown(id) {
      var rollup = this.epicRollup(id);
      var counts = rollup.counts;
      var parts = [];
      var say = function (n, word) { if (n) parts.push(n + ' ' + word); };
      say(rollup.activeCount, 'running');
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
      var known = new Set(EPIC_GROUPS.flatMap(g => g.statuses));
      var extra = [...new Set(children.map(c => c.status).filter(s => !known.has(s)))].sort();
      var groups = EPIC_GROUPS.concat(extra.map(s => ({ key: s, label: s, statuses: [s] })));
      return groups.map(function (g) {
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

    epicIsAgent(ticket) {
      return ticket?.kind === 'epic' && ticket.epic_execution === 'agent';
    },

    epicCanStart(ticket) {
      return this.ticketCanControl(ticket) && ticket.kind === 'epic' && ticket.status !== 'archived' && (!ticket.epic_execution ||
        (this.epicIsAgent(ticket) && ticket.status === 'open'));
    },

    epicBriefWritable(ticket = this.selectedEpic) {
      return this.ticketCanEdit(ticket) && (!this.epicIsAgent(ticket) ||
        ['open', 'todo', 'paused'].includes(ticket.status));
    },

    epicCanEditBrief() {
      return this.epicBriefWritable() && !this.lifecycleBusy(this.selectedEpic) && this._epicDetail?.id === this.selectedEpic.id;
    },

    _epicRetainBrief() {
      if (this._epicBriefDebounce) clearTimeout(this._epicBriefDebounce);
      this._epicBriefDebounce = null;
      var state = this._editorSaves[this.selectedEpic?.id];
      if (state && this._epicDetail?.id === this.selectedEpic?.id) {
        state.draft = { ...state.draft, description: this._epicBriefLead + this.editForm.body };
      }
      this.epicEditingBrief = false;
    },

    epicDescription(ticket) {
      if (!ticket) return '';
      return [ticket.path, this.epicIsAgent(ticket) ? 'supervisor · ' + this.statusLabel(ticket.status) : 'passive epic',
        ticket.agent, ticket.branch || 'no branch yet'].filter(Boolean).join(' · ');
    },

    epicSupervisorActions() {
      var epic = this.selectedEpic;
      if (!epic) return [];
      var self = this, actions = [];
      if (this.epicCanStart(epic)) actions.push({ label: 'start epic', run: () => self.epicStart() });
      if (!this.epicIsAgent(epic)) return actions;
      actions.push({ label: 'watch agent', run: () => self.epicWatchAgent() });
      actions.push({ label: 'run details', run: () => self.epicRunDetails() });
      if (this.executionActive(epic)) {
        actions.push({ label: 'pause supervisor', run: () => self.epicAction('pause') });
      }
      if (['paused', 'done', 'cancelled'].includes(epic.status)) {
        actions.push({ label: epic.status === 'paused' ? 'retry supervisor' : 'restart supervisor', run: () => self.epicAction('retry') });
      }
      if (epic.status === 'human_review') {
        actions.push({ label: 'review supervisor', run: () => self.startPlannotatorReview(epic) });
        actions.push({ label: 'approve', run: () => self.epicAction('move', 'done') });
        actions.push({ label: 'send back', run: () => self.epicAction('retry') });
      }
      return actions;
    },

    epicStart() {
      return this.dispatchLifecycle(this.ticketKey(this.selectedEpic), 'start');
    },

    epicWatchAgent() {
      if (!this.epicIsAgent(this.selectedEpic)) return;
      this.epicSwitchTab(this.executionActive(this.selectedEpic) ? 'session' : 'activity');
    },

    epicSwitchTab(tab, load = true) {
      if (['session', 'activity'].includes(tab) && !this.epicIsAgent(this.selectedEpic)) return;
      if (tab !== this.epicTab) this._invalidateRawLog();
      this.epicTab = tab;
      if (tab !== 'activity') {
        this._activityLoadSeq++;
        this.activityLoading = false;
        this._stopActivityPoll();
      }
      if (this.isMobile && tab !== 'session') this.closeTerminal();
      if (tab === 'session' && this.terminalWanted()) {
        this.$nextTick(() => {
          if (!this.terminalWanted()) return;
          if (this.terminalOpen) this.refitTerminal();
          else this.openTerminal();
        });
      }
      if (tab === 'activity' && load) {
        var target = this.activityStage !== null
          ? { stage: this.activityStage, run: this.activityRun } : this.activityTarget();
        if (target) this.fetchActivity(target.stage, target.run);
      }
    },

    async epicRunDetails() {
      var epic = this.selectedEpic;
      if (!this.epicIsAgent(epic)) return;
      var navigation = ++this._navigationSeq;
      if (await this.epicFlushBrief() === false) return;
      if (navigation !== this._navigationSeq || this.selectedEpic?.id !== epic.id) return;
      if (this.isMobile) await this.openMobileTicket(epic);
      else await this.selectTicket(epic);
    },

    async epicAction(endpoint, status) {
      var epic = this.selectedEpic;
      if (!this.epicIsAgent(epic)) return { outcome: 'refused' };
      return this.moveTicketVia(this.ticketKey(epic), endpoint, status ? { status: status } : null);
    },

    // The board list carries no body, and the brief is the body. The row is
    // shown first and the fetch fills the brief in, which is what selectTicket
    // does for a ticket page.
    async openEpic(id, navigation = ++this._navigationSeq) {
      var identity = this.ticketIdentity(id);
      if (this.isSecondary(identity)) {
        var secondary = this.secondaryTickets.find(t => this.sameTicket(t, identity));
        if (secondary) await this._paletteOpenTicket(secondary);
        return;
      }
      if (this.currentView !== 'board') await this.gotoView('board', navigation, { view: 'board', epicId: id });
      if (navigation !== this._navigationSeq || this.currentView !== 'board') return;
      var epic = (this.tickets || []).find(function (t) { return t.id === id && t.kind === 'epic'; });
      if (!epic) return;
      // The ticket page and the epic page are two overlays over one board, so
      // opening either closes the other.
      if (this.selectedTicket) this.closeDetail(false);
      if (this.selectedEpic) {
        if (await this.closeEpic(false) === false) return;
        if (navigation !== this._navigationSeq) return;
      }
      this.closeTerminal();
      this.terminalRW = false;
      this._resetActivity();
      this.logViewContent = null;
      this.logViewStage = null;
      this._epicDetail = null;
      this.epicBriefSaved = false;
      this.editForm.body = '';
      this.selectedEpic = epic;
      this.epicTab = 'children';
      this.epicAddDraft = '';
      this.epicCandidateQuery = '';
      this.epicCandidateOpen = false;
      this.epicRemoveTarget = null;
      this.epicEditingBrief = false;
      ++this._editorGeneration;
      this.writeHash();
      await this._waitEditorSaves(id);
      if (navigation !== this._navigationSeq || this.selectedEpic?.id !== id) return;
      await this.epicFetchDetail(id);
    },

    // Fetch the epic's own detail: the body the brief renders and the notes
    // the daemon parsed out of it. Kept in _epicDetail rather than written
    // onto selectedEpic, because every board recompute replaces that object
    // with the list entry, which has neither.
    async epicFetchDetail(id) {
      var seq = ++this._epicDetailSeq;
      var update = this._epicUpdateSeq;
      var mine = () => seq === this._epicDetailSeq && this.selectedEpic?.id === id;
      try {
        while (mine()) {
          var res = await fetch('/api/tickets/' + encodeURIComponent(id));
          if (!mine()) return;
          if (!res.ok) throw new Error('detail request failed');
          var full = await res.json();
          if (!mine()) return;
          if (update !== this._epicUpdateSeq) {
            update = this._epicUpdateSeq;
            continue;
          }
          if (full.id !== id) return;
          var previous = this.selectedEpic;
          var keepDraft = this._epicDetail?.id === id && (this.epicEditingBrief || this._epicBriefSave ||
            this._epicBriefLead + this.editForm.body !== this._editorSaves[id]?.ack.description);
          this._epicDetail = full;
          this.selectedEpic = full;
          if (!keepDraft) this._epicLoadBrief(full);
          if (!this.epicBriefWritable(full)) this._epicRetainBrief();
          var index = this.tickets.findIndex(t => t.id === id);
          if (index >= 0) {
            var before = this.tickets[index];
            this.tickets[index] = this.boardEntry(full);
            if (this.boardEntryChanged(before, this.tickets[index])) this.recomputeBoard();
          }
          this.reconcileOutput(previous);
          return;
        }
      } catch (e) {
        if (mine()) this.error = 'Failed to load epic details';
      }
    },

    closeEpic(writeRoute = true) {
      if (writeRoute) ++this._navigationSeq;
      if (!this.selectedEpic) return true;
      var id = this.selectedEpic.id;
      var navigation = this._navigationSeq;
      var close = (ok) => {
        if (navigation !== this._navigationSeq || this.selectedEpic?.id !== id) return false;
        if (!ok) {
          if (this.epicBriefWritable()) return false;
          this._epicRetainBrief();
        }
        this.closeTerminal();
        this.terminalRW = false;
        this._resetActivity();
        this.selectedEpic = null;
        this._releaseEditorBody();
        this._retireEditorState(id);
        ++this._editorGeneration;
        this._epicDetailSeq++;
        this._epicDetail = null;
        this.epicAddDraft = '';
        this.epicEditingBrief = false;
        if (writeRoute) this.writeHash();
        return true;
      };
      var saving = this.epicFlushBrief();
      return saving && typeof saving.then === 'function' ? saving.then(close) : close(saving);
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
      if (!this.epicBriefWritable(fresh)) this._epicRetainBrief();
      var detail = this._epicDetail && this._epicDetail.id === id ? this._epicDetail : null;
      this.selectedEpic = detail
        ? Object.assign({}, detail, fresh, { body: detail.body, notes: detail.notes,
          history: detail.history, last_error: detail.last_error, last_log: detail.last_log })
        : fresh;
    },

    // ---- writes -----------------------------------------------------------

    // Re-derived at click time rather than read off the rendered pill, the way
    // resumeChainHolder is: the stream can have moved the holder on since the
    // row was drawn.
    async epicApproveHolder() {
      var r = this.epicRollup(this.selectedEpic ? this.selectedEpic.id : '');
      if (!r.holder || r.holder.status !== 'human_review') return;
      return this.dispatchLifecycle(this.ticketKey(r.holder), 'move:done');
    },

    epicCandidates(epic, query = '') {
      if (!epic || this.isSecondary(epic) || !this.ticketCanControl(epic)) return [];
      var q = query.trim().toLowerCase();
      return this.tickets.filter(t => !this.isSecondary(t) && this.ticketCanControl(t) &&
        t.kind !== 'epic' && t.id !== epic.id && t.status !== 'archived' && t.parent?.id !== epic.id &&
        (!q || this.ticketMatchesQuery(t, q, ['stage', 'agent'])))
        .sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
    },

    epicChildCommand(child, command) {
      return this.dispatchLifecycle(this.ticketKey(child), command);
    },

    epicMutationBusy(epic) {
      return !!this.epicMutationPending[typeof epic === 'string' ? epic : epic?.id];
    },

    _epicSelectedOwner() {
      return this.ticketCanControl(this.selectedEpic) ? this.selectedEpic.id : null;
    },

    _epicOwner(id) {
      return this.tickets.find(t => t.id === id && t.kind === 'epic' && !this.isSecondary(t)) || null;
    },

    _epicManagementError(owner, message) {
      this.epicManagementErrors = { __proto__: null, ...this.epicManagementErrors, [owner]: message };
      if (this.selectedEpic?.id === owner) this.error = message;
    },

    _epicEnqueue(owner, work) {
      if (!owner) return Promise.resolve(false);
      this.epicMutationPending = { __proto__: null, ...this.epicMutationPending, [owner]: (this.epicMutationPending[owner] || 0) + 1 };
      var previous = this._epicMutationTails[owner] || Promise.resolve();
      var next = previous.catch(() => false).then(async () => {
        if (this._epicNeedsRefresh[owner]) {
          try { await this.fetchTasks(); delete this._epicNeedsRefresh[owner]; }
          catch (_) { return false; }
        }
        var epic = this._epicOwner(owner);
        if (!epic || !this.ticketCanControl(epic)) return false;
        return work(epic);
      }).finally(() => {
        this.epicMutationPending = { __proto__: null, ...this.epicMutationPending, [owner]: this.epicMutationPending[owner] - 1 };
        if (this._epicMutationTails[owner] === next) delete this._epicMutationTails[owner];
      });
      this._epicMutationTails[owner] = next;
      return next;
    },

    epicConfirmRemove(id) {
      var owner = this._epicSelectedOwner();
      if (owner) this.epicRemoveTarget = { owner: owner, id: id };
    },

    async epicRemoveConfirmed() {
      var target = this.epicRemoveTarget;
      if (!target) return false;
      var ok = await this.epicMoveOut(target.id, target.owner);
      if (ok && this.epicRemoveTarget === target) this.epicRemoveTarget = null;
      return ok;
    },

    epicRetainedOrderRefs() {
      var epic = this.selectedEpic;
      if (!epic) return [];
      var members = new Set(this.tickets.filter(t => t.parent?.id === epic.id && t.kind !== 'epic').map(t => t.id));
      return (epic.child_order || []).filter(id => !members.has(id));
    },

    // Replace only slots owned by this displayed group. Raw nonmembers and
    // other groups keep their positions; missing members are appended by created/id.
    _epicSafeOrder(epic, groupIDs) {
      var children = this._epicOrderChildren(epic, this.tickets.filter(t => t.parent?.id === epic.id && t.kind !== 'epic'));
      var raw = (epic.child_order || []).slice();
      if (new Set(raw).size !== raw.length) {
        this._epicManagementError(epic.id, 'Stored child order has duplicate references. Refresh or correct the stored order before reordering.');
        return null;
      }
      var present = new Set(raw);
      children.forEach(c => { if (!present.has(c.id)) { raw.push(c.id); present.add(c.id); } });
      var ids = new Set(groupIDs);
      if (ids.size !== groupIDs.length || groupIDs.some(id => !children.some(c => c.id === id))) return null;
      var at = 0;
      return raw.map(id => ids.has(id) ? groupIDs[at++] : id);
    },

    epicMoveChild(id, delta) {
      var owner = this._epicSelectedOwner();
      return this.epicSaveOrder(epic => {
        var children = this._epicOrderChildren(epic, this.tickets.filter(t => t.parent?.id === owner && t.kind !== 'epic'));
        var child = children.find(c => c.id === id);
        if (!child) return null;
        var statuses = EPIC_GROUPS.find(g => g.statuses.includes(child.status))?.statuses || [child.status];
        var ids = children.filter(c => statuses.includes(c.status)).map(c => c.id);
        var at = ids.indexOf(id), to = at + delta;
        if (to < 0 || to >= ids.length) return null;
        [ids[at], ids[to]] = [ids[to], ids[at]];
        return this._epicSafeOrder(epic, ids);
      }, owner);
    },

    // One JSON request, with the failure reported the way moveTicketVia
    // reports one: the daemon's own message on this.error, and no throw, so a
    // refused drag leaves the page usable. Returns whether it landed.
    async _epicRequest(method, url, body, owner = this._epicSelectedOwner()) {
      if (!owner || !this.ticketCanControl(this._epicOwner(owner))) return false;
      this._epicManagementError(owner, null);
      try {
        var opts = { method: method };
        if (body !== undefined) {
          opts.headers = { 'Content-Type': 'application/json' };
          opts.body = JSON.stringify(body);
        }
        const res = await fetch(url, opts);
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this._epicManagementError(owner, data.error || (method + ' ' + url + ' failed'));
          return false;
        }
        return true;
      } catch (e) {
        this._epicNeedsRefresh[owner] = true;
        this._epicManagementError(owner, 'Result uncertain; refresh tickets before another change: ' + e.message);
        // Never replay a mutation after a lost response.
        try { await this.fetchTasks(); delete this._epicNeedsRefresh[owner]; } catch (_) {
          this._epicManagementError(owner, 'Result uncertain; refresh tickets before trying again.');
        }
        return false;
      }
    },

    // Write the epic's child order. One file, the epic's: membership is what
    // each child's parent says, so no child is touched.
    epicSaveOrder(ids, owner = this._epicSelectedOwner(), group) {
      return this._epicEnqueue(owner, async epic => {
        var order = typeof ids === 'function' ? ids(epic)
          : group !== undefined ? this._epicDropOrder(epic, group, ids) : this._epicSafeOrder(epic, ids);
        if (!order) return false;
        var touched = this._beginSnapshot();
        try {
          var ok = await this._epicRequest('PUT', '/api/tickets/' + encodeURIComponent(owner) + '/children', { children: order }, owner);
          var current = this._epicOwner(owner);
          // A newer stream update wins over this acknowledgement.
          if (ok && current && !touched.has(owner)) {
            // Keep older ticket snapshots from replacing the acknowledged order before the stream update arrives.
            this._touchTicket(current);
            current.child_order = order;
            if (this.selectedEpic?.id === owner) {
              this.selectedEpic.child_order = order;
              ++this._epicUpdateSeq;
            }
            if (this._epicDetail?.id === owner) this._epicDetail.child_order = order;
            this.recomputeBoard();
          }
          return ok;
        } finally { this._endSnapshot(touched); }
      });
    },

    // File an existing ticket under the open epic.
    epicPullIn(ticketID, owner = this._epicSelectedOwner()) {
      return this._epicEnqueue(owner, epic => {
        if (!this.epicCandidates(epic).some(t => t.id === ticketID)) return false;
        return this._epicRequest('POST', '/api/tickets/' + encodeURIComponent(ticketID) + '/parent', { parent: owner }, owner);
      });
    },

    // Take one child out of the epic. The ticket survives; it stops being part
    // of this brief.
    epicMoveOut(ticketID, owner = this._epicSelectedOwner()) {
      return this._epicEnqueue(owner, () => {
        var child = this.tickets.find(t => t.id === ticketID && !this.isSecondary(t));
        if (!child || child.parent?.id !== owner || !this.ticketCanControl(child)) return false;
        return this._epicRequest('POST', '/api/tickets/' + encodeURIComponent(ticketID) + '/unparent', undefined, owner);
      });
    },

    // The inline add row and "+ sub-ticket" both land here: one POST
    // /api/tickets carrying the epic's parent and path, which is what
    // submitCreateTicket posts for an ordinary ticket.
    async epicAddChild() {
      var title = this.epicAddDraft.trim();
      if (!title || !this._epicSelectedOwner() || this.epicAddBusy) return;
      var owner = this._epicSelectedOwner(), path = this.selectedEpic.path;
      var navigation = this._navigationSeq;
      this.epicAddBusy = true;
      try {
        var ok = await this._epicEnqueue(owner, () => this._epicRequest('POST', '/api/tickets', {
          title: title,
          path: path,
          parent: owner,
          status: 'open',
        }, owner));
        if (ok && this.selectedEpic?.id === owner && navigation === this._navigationSeq && this.epicAddDraft.trim() === title) this.epicAddDraft = '';
      } finally {
        this.epicAddBusy = false;
      }
    },

    // ---- the brief ---------------------------------------------------------

    // Seed from the retained draft or fetched description. The leading newline is
    // held aside and put back on save, so a round-trip through the editor does
    // not move the first line.
    _epicLoadBrief(ticket) {
      var state = this._loadEditorDraft(ticket, { description: this.descriptionText(ticket) });
      var text = state.draft.description;
      this._epicBriefLead = text.startsWith('\n') ? '\n' : '';
      this.editForm.body = text.slice(this._epicBriefLead.length);
    },

    // A click on a rendered section opens the editor with that section left
    // where it is on screen. There are no per-section editors: the anchor is
    // the whole of what per-block means here.
    epicBeginBriefEdit(event) {
      if (!this.epicCanEditBrief()) return;
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
      this.epicBriefSaved = false;
      if (this._epicBriefDebounce) clearTimeout(this._epicBriefDebounce);
      this._epicBriefDebounce = setTimeout(() => this.epicSaveBrief(), 800);
    },

    epicFlushBrief() {
      if (this._epicBriefDebounce) {
        clearTimeout(this._epicBriefDebounce);
        this._epicBriefDebounce = null;
      }
      return this.epicSaveBrief();
    },

    epicSaveBrief() {
      if (this._epicBriefSave) return this._epicBriefSave;
      if (!this.selectedEpic || this._epicDetail?.id !== this.selectedEpic.id) return true;
      var id = this.selectedEpic.id;
      var generation = this._editorGeneration;
      var state = this._editorSaves[id];
      if (!state) return true;
      if (!state.failed && this._epicBriefLead + this.editForm.body === state.ack.description) return true;
      if (!this.epicBriefWritable()) {
        this._epicRetainBrief();
        this.error = 'The unsaved epic brief is retained. Editing is no longer permitted.';
        return false;
      }
      this.epicBriefSaving = true;
      this.epicBriefSaved = false;
      // A second PUT must not overtake the first or let navigation drop a newer draft.
      this._epicBriefSave = (async () => {
        while (this.selectedEpic?.id === id) {
          var body = this._epicBriefLead + this.editForm.body;
          if (!state.failed && body === state.ack.description) return true;
          if (!this.epicBriefWritable()) { this._epicRetainBrief(); return false; }
          if (!await this._queueEditorSave(id, { description: body }, generation)) return false;
        }
        return true;
      })().finally(() => {
        this._epicBriefSave = null;
        this.epicBriefSaving = false;
        this.epicBriefSaved = !state.failed && this.selectedEpic?.id === id &&
          this._epicBriefLead + this.editForm.body === state.ack.description;
      });
      return this._epicBriefSave;
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
    epicSortableOwner() {
      return resourceOwner(function () {
        return this.epicInitSortable(this.$el);
      });
    },

    // Restore Sortable's moved rows to Alpine's current keyed order, not a pre-SSE snapshot.
    // The template fields belong to shipped Alpine 3.14.8.
    _epicRestoreRows() {
      function restore() {
        document.querySelectorAll('[data-epic-group]').forEach(function (list) {
          var template = list.querySelector('template');
          if (!template || !template._x_lookup || !template._x_prevKeys) return;
          var rows = template._x_prevKeys.map((key) => template._x_lookup[key]).filter(Boolean);
          var at = template;
          rows.forEach(function (row) {
            if (at.nextElementSibling !== row) list.insertBefore(row, at.nextElementSibling);
            at = row;
          });
          // A removed source group can have left a foreign row in this list.
          Array.from(list.children).forEach(function (row) {
            if (row.matches('.epic-row:not(.epic-row-add)') && rows.indexOf(row) < 0) row.remove();
          });
        });
      }
      if (typeof Alpine !== 'undefined') Alpine.mutateDom(restore);
      else restore();
    },

    epicInitSortable(el) {
      if (this.isMobile) return;
      var self = this;
      return mountSortableResource(el, {
        group: { name: 'epic-children', put: to => !!self._epicDropStatus(to.el.dataset.epicGroup) },
        animation: 150,
        handle: '.epic-row-handle',
        draggable: '.epic-row:not(.epic-row-add)',
        ghostClass: 'sortable-ghost',
        dragClass: 'sortable-drag',
        onStart: function (evt) {
          self.epicDragging = true;
          if (evt?.item) evt.item._epicDragOwner = self.selectedEpic?.id;
          if (evt?.item) evt.item._lifecycleDragState = self.lifecycleCommand?.(evt.item.dataset.childId, 'move:open')?.expected;
        },
        onEnd: async function (evt) {
          self.epicDragging = false;
          var childID = evt.item.dataset.childId;
          if (!childID) return;
          var from = evt.from.dataset.epicGroup;
          var to = evt.to.dataset.epicGroup;
          var owner = evt.item._epicDragOwner;
          var order = from === to ? self._epicOrderAfterDrop(evt.to) : null;
          self._epicRestoreRows();
          if (from !== to) {
            if (self.selectedEpic?.id !== owner) return;
            var status = self._epicDropStatus(to);
            if (status) {
              if (evt.item._lifecycleDragState !== undefined) await self.moveTask(childID, status, evt.item._lifecycleDragState);
              else await self.moveTask(childID, status);
            }
            return;
          }
          await self.epicSaveOrder(order, owner, from);
        },
      }, function () {
        self.epicDragging = false;
        self._epicRestoreRows();
      });
    },

    _epicDropStatus(group) {
      var g = EPIC_GROUPS.find(function (x) { return x.key === group; });
      return g ? g.dropStatus : (this.configCache?.custom_statuses || []).includes(group) ? group : undefined;
    },

    _epicDropOrder(epic, group, ids) {
      var statuses = EPIC_GROUPS.find(g => g.key === group)?.statuses || [group];
      var members = this.tickets.filter(t => t.parent?.id === epic.id && t.kind !== 'epic' && statuses.includes(t.status));
      if (members.length !== ids.length || members.some(t => !ids.includes(t.id))) return null;
      return this._epicSafeOrder(epic, ids);
    },

    // Read only the moved group, then merge its permutation into the raw order.
    _epicOrderAfterDrop(list) {
      var ids = [];
      list.querySelectorAll('[data-child-id]').forEach(function (el) {
        var id = el.dataset.childId;
        if (id && ids.indexOf(id) < 0) ids.push(id);
      });
      return ids;
    },
  };
}
