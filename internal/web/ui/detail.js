import { isoToLocalInput, parseScheduleInput, runSeconds, scheduleChipLabel, scheduleMinLocal } from './format.js';
import { highlightMarkdown } from './markdown.js';
import { termState } from './terminal.js';

const SETTINGS_ROUTE_KEYS = ['general', 'environment', 'agents', 'stages', 'pipelines', 'projects',
  'web', 'assistant', 'plannotator', 'notifications', 'statuses', 'display'];
const HISTORY_KEY = '__kontoraHistory';
const HISTORY_DESTINATION = Symbol('settings history destination');
const historyIdentity = () => Date.now().toString(36) + '-' + Math.random().toString(36).slice(2);
function historyMetadata(state) {
  const m = state?.[HISTORY_KEY];
  return m && typeof m.id === 'string' && typeof m.entry === 'string' && Number.isSafeInteger(m.index) ? m : null;
}
function historyState(metadata) {
  return { ...history.state, [HISTORY_KEY]: { id: metadata.id, entry: metadata.entry, index: metadata.index } };
}
function historySignature(entry) { return JSON.stringify([entry.id, entry.entry, entry.hash]); }
function historyPosition() {
  const index = window.navigation?.currentEntry?.index;
  return Number.isSafeInteger(index) && index >= 0 ? index : null;
}

// The ticket detail panel: summary, stage rail, inline editing, stage logs
// and the actions the panel offers.
export function kontoraDetail() {
  const moves = new Map();

  return {
    // What the assistant is told about the open ticket. Null when none is open,
    // so the pane leaves the line out rather than saying "no ticket". The panel
    // only renders on the board and gotoView never clears selectedTicket, so a
    // ticket left open on another view would otherwise be described as on screen.
    detailPageContext() {
      const t = this.selectedTicket;
      if (!t || this.currentView !== 'board') return null;
      return [
        'Open ticket: ' + t.id + ' (' + t.status + (t.stage ? ', stage ' + t.stage : '') + ')',
        t.title ? 'Title: ' + t.title : null,
        t.branch ? 'Branch: ' + t.branch : null,
      ];
    },

    async selectTicket(ticket, navigation = ++this._navigationSeq) {
      if (this.currentView !== 'board') {
        if (await this.gotoView('board', navigation, { view: 'board', ticketId: ticket.id, source: ticket.source }) === false) return;
        if (navigation !== this._navigationSeq) return;
      }
      if (this.sameTicket(this.selectedTicket, ticket)) {
        this.closeDetail();
        return;
      }
      // The epic page is the other overlay over this board. Opening a ticket
      // from it, a sub-ticket row for instance, replaces it rather than
      // stacking on it: both pages carry a body editor, and only one of them
      // can own the refs behind it.
      if (this.selectedEpic) {
        if (await this.closeEpic(false) === false) return;
        if (navigation !== this._navigationSeq) return;
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
      this._resetTicketCost();
      this.setStageOpen = false;
      this.scheduleEditing = false;
      this.scheduleError = null;
      // The draft holds the previous ticket's statuses, and the editor saves on
      // every click, so leaving it open would write them onto the new one.
      this.notifyEditing = false;
      this.notifyDraft = null;
      this.notifyError = null;
      this.ticketChanges = null;
      this.collapsedStages = {};
      this.relExpanded = {};
      this.chain = null;
      this.chainCollapsed = false;
      this.chainError = null;
      this.childrenCollapsed = false;
      this.childrenExpanded = false;
      this.noteDraft = '';
      this.noteResetDrafts();
      var detailSeq = ++this._ticketDetailSeq;
      var updateSeq = this._selectedTicketUpdateSeq;
      var key = this.ticketKey(ticket);
      var mine = () => this.sameTicket(this.selectedTicket, ticket) && detailSeq === this._ticketDetailSeq;
      this.selectedTicket = ticket;
      this._pushRecentTicket(key);
      this.fetchTicketCost(ticket.id);
      this.detailLoading = true;
      try {
        if (this._editorSaves[key]) await this._waitEditorSaves(key);
        while (mine()) {
          var res = await fetch(this.ticketURL(ticket));
          if (!mine()) return;
          if (!res.ok) {
            this.error = 'Failed to load ticket details';
            break;
          }
          var full = await res.json();
          if (!mine()) return;
          // Ticket SSE omits the body. Repeat a read that an SSE update
          // overtook instead of replacing newer ticket fields with the old response.
          if (updateSeq !== this._selectedTicketUpdateSeq) {
            updateSeq = this._selectedTicketUpdateSeq;
            continue;
          }
          if (full.id === ticket.id) {
            full = { ...full, source: ticket.source || full.source };
            this.selectedTicket = full;
            this._touchTicket(full);
            var entries = this.isSecondary(ticket) ? this.secondaryTickets : this.tickets;
            var idx = this.isSecondary(full) ? this.secondarySlot(full) : entries.findIndex(t => this.sameTicket(t, full));
            if (idx >= 0) {
              var before = entries[idx];
              var entry = this.boardEntry(full);
              entries[idx] = entry;
              // The replacement can change agent, status, or any rendered field,
              // so refresh the cached board and the agent tally from it. Usually
              // it changes none of them, and a recompute re-filters and re-sorts
              // every column, so opening a ticket pays for that only when the
              // board would draw something different.
              if (this.boardEntryChanged(before, entry)) this.recomputeBoard();
            }
          }
          break;
        }
      } catch (e) {
        if (mine()) this.error = 'Failed to load ticket details';
      }
      if (!mine()) return;
      this.detailLoading = false;
      if (this.isSecondary(this.selectedTicket)) {
        this.activeTab = 'ticket';
        this.detailTab = 'ticket';
        await this.startEditing();
        this.writeHash();
        return;
      }
      // The rail's churn block is always on, so the branch diff is read for
      // every ticket that has one rather than only for summarised runs.
      if (this.selectedTicket?.branch) {
        this.fetchChanges(this.selectedTicket.id);
      }
      // The ladder is derived from every ticket file on disk, so it cannot be
      // read off the board and has to be fetched for each ticket opened.
      if (this.selectedTicket?.id) {
        this.fetchChain(this.selectedTicket.id);
      }
      if (!this.executionActive(this.selectedTicket) && this.selectedTicket?.status !== 'todo' && this.selectedTicket?.status !== 'open'
          && this.selectedTicket?.history?.length > 0) {
        var latest = this.latestCompletedRun();
        if (latest) this.fetchActivity(latest.stage, latest.run);
        // Finished tickets with a summary open on the summary tab; the
        // transcript stays prefetched for the activity tab.
        this.activeTab = this.summaryFirst(this.selectedTicket) ? 'summary' : 'activity';
      } else if (this.executionActive(this.selectedTicket) &&
          (this.selectedTicket.kind !== 'epic' || this.epicIsAgent(this.selectedTicket))) {
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

    // Missing outcomes are legacy/unverified, regardless of runner exit code.
    runOutcomeLabel(h) {
      return ['complete', 'blocked', 'protocol_error', 'failed'].includes(h?.run_outcome)
        ? h.run_outcome : 'unknown/unverified';
    },

    runOutcomeClass(h) {
      var outcome = this.runOutcomeLabel(h);
      if (outcome === 'complete') return 'text-ok';
      if (outcome === 'failed' || outcome === 'protocol_error') return 'text-st-cancel';
      return 'text-tx-faint';
    },

    // One card per run with a summary or outcome reason, newest first. Duration and run
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
      hist.forEach((h, historyIndex) => {
        var run = walked[h.stage] || 0;
        walked[h.stage] = run + 1;
        var summary = h.summary || h.outcome_reason;
        if (!summary) return;
        // A stage the current pipeline no longer lists has no ribbon segment to
        // agree with, so the card reads this run's own clock.
        var seg = h.imported ? null : segs[h.stage];
        cards.push({
          key: h.stage + '#' + run,
          stage: h.stage,
          run: run,
          historyIndex: historyIndex,
          imported: !!h.imported,
          summary: summary,
          failed: h.run_outcome === 'failed' || h.run_outcome === 'protocol_error',
          outcome: this.runOutcomeLabel(h),
          outcomeReason: h.outcome_reason || '',
          invocationID: h.invocation_id || '',
          exitCode: h.exit_code,
          agent: h.agent || '',
          seconds: seg ? seg.seconds : runSeconds(h),
          runs: h.imported ? 1 : (seg ? seg.runs : totalRuns[h.stage]),
        });
      });
      cards.reverse();
      // Prepend the top-level field only when it says something the newest run
      // does not.
      var top = t.summary || '';
      if (top && (!cards.length || cards[0].summary !== top)) {
        cards.unshift({
          key: 'summary#top', stage: this.summaryStage(), run: -1,
          summary: top, failed: false, outcome: 'unknown/unverified', agent: '', seconds: 0, runs: 1,
        });
      }
      return cards;
    },

    // Only a verified complete run gets the newest card's done hue.
    // Earlier cards use the stage's pipeline position unless failed or blocked.
    stageHue(card, i) {
      if (card.failed) return '--st-error';
      if (i === 0 && card.outcome === 'complete') return '--st-done';
      if (card.outcome === 'blocked') return '--st-paused';
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
      var cost = this.costLabel(this.ticketRunCost(card.historyIndex));
      if (cost) parts.push(cost);
      parts.push('outcome: ' + card.outcome);
      if (Number.isInteger(card.exitCode)) parts.push('runner exit ' + card.exitCode);
      if (card.outcomeReason) parts.push(card.outcomeReason);
      if (card.invocationID) parts.push('invocation ' + card.invocationID);
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
      if (this.isSecondary(this.selectedTicket)) return;
      try {
        var res = await fetch('/api/tickets/' + id + '/changes');
        if (res.ok) {
          var changes = await res.json();
          if (!this.isSecondary(this.selectedTicket) && this.selectedTicket?.id === id) this.ticketChanges = changes;
        }
      } catch (e) { /* the card simply shows no commit list */ }
    },

    _resetTicketCost() {
      this._ticketCostSeq++;
      this.ticketCost = null;
    },

    async fetchTicketCost(id) {
      if (this.isSecondary(this.selectedTicket)) return;
      var seq = ++this._ticketCostSeq;
      var mine = () => !this.isSecondary(this.selectedTicket) && this.selectedTicket?.id === id && seq === this._ticketCostSeq;
      if (mine()) this.ticketCost = null;
      try {
        var res = await fetch('/api/tickets/' + encodeURIComponent(id) + '/cost');
        if (!mine() || !res.ok) return;
        var cost = await res.json();
        if (mine() && cost.id === id) this.ticketCost = cost;
      } catch (e) { /* an unavailable estimate stays absent */ }
    },

    formatCostUSD(value) {
      if (value === null || value === undefined || value === '') return '';
      var match = /^(\d+)(?:\.(\d+))?$/.exec(String(value));
      if (!match) return '';
      var whole = match[1].replace(/^0+(?=\d)/, '');
      var fraction = match[2] || '';
      if (whole === '0' && !/[1-9]/.test(fraction)) return '$0.00';
      if (whole === '0' && (fraction + '0000').slice(0, 4) === '0000') return '<$0.0001';

      var places = whole === '0' && (fraction + '00').slice(0, 2) === '00' ? 4 : 2;
      var scale = 10n ** BigInt(places);
      var kept = (fraction + '0'.repeat(places)).slice(0, places);
      var units = BigInt(whole) * scale + BigInt(kept || '0');
      if (fraction.length > places && fraction[places] >= '5') units++;
      var roundedWhole = units / scale;
      var roundedFraction = String(units % scale).padStart(places, '0');
      return '$' + roundedWhole + '.' + roundedFraction;
    },

    currentTicketCost() {
      var cost = this.ticketCost;
      return cost && cost.id === this.selectedTicket?.id ? cost : null;
    },

    costLabel(cost) {
      if (!cost) return '';
      var label = this.formatCostUSD(cost.cost_usd);
      if (!label) {
        return cost.cost_usd == null && cost.tracked_runs > 0 && cost.priced_runs === 0 ? 'unknown' : '';
      }
      return label + (cost.priced_runs < cost.tracked_runs ? '+' : '');
    },

    costTooltip(cost) {
      if (!cost) return '';
      if (cost.cost_usd != null && cost.tracked_runs === undefined) {
        return 'Estimated using the bundled snapshot of standard model rates';
      }
      if (!cost.tracked_runs) return '';
      var coverage = cost.priced_runs + ' of ' + cost.tracked_runs + ' runs priced';
      if (cost.priced_runs === 0) return 'No estimate using the bundled snapshot of standard model rates · ' + coverage;
      return (cost.priced_runs < cost.tracked_runs
        ? 'Known subtotal using the bundled snapshot of standard model rates · '
        : 'Estimated using the bundled snapshot of standard model rates · ') + coverage;
    },

    ticketStageCost(stage) {
      return (this.currentTicketCost()?.stages || []).find(function (cost) { return cost.name === stage; }) || null;
    },

    ticketRunCost(historyIndex) {
      if (!Number.isInteger(historyIndex)) return null;
      return (this.currentTicketCost()?.runs || []).find(function (cost) { return cost.history_index === historyIndex; }) || null;
    },

    // The chain behind the ladder. Guarded on the selected ticket the way
    // fetchChanges is, and on a sequence number as well: the SSE path refetches
    // the same ticket, so the id alone cannot tell two in-flight requests apart
    // and an older response could land last.
    async fetchChain(id) {
      if (this.isSecondary(this.selectedTicket)) return;
      var seq = ++this._chainSeq;
      var mine = () => !this.isSecondary(this.selectedTicket) && this.selectedTicket?.id === id && seq === this._chainSeq;
      this.chainError = null;
      try {
        var res = await fetch('/api/tickets/' + id + '/chain');
        if (!mine()) return;
        if (!res.ok) {
          this.chainError = 'Failed to load the chain';
          return;
        }
        var chain = await res.json();
        if (!mine()) return;
        this.chain = chain;
      } catch (e) {
        if (mine()) this.chainError = 'Failed to load the chain';
      }
    },

    switchTab(tab) {
      if (this.isSecondary(this.selectedTicket) && !['ticket', 'summary', 'notes'].includes(tab)) return;
      // The tab content uses x-show, so leaving it would otherwise keep a
      // focused textarea mounted and bring it back with stale geometry.
      if (tab !== 'ticket' && this.editingBody) {
        this.flushEditSave();
        this.editingBody = false;
      }
      // The live session only exists while a stage runs. Asking for it on a
      // finished ticket lands on that stage's transcript instead.
      if (tab === 'session' && !this.executionActive(this.selectedTicket)) tab = 'activity';
      if (tab !== this.activeTab) {
        this._invalidateStageLogs();
        this._invalidateRawLog();
      }
      this.activeTab = tab;
      // Leaving the session tab keeps the stream and the terminal: the tab
      // content is x-show, so the container survives, and output that arrives
      // while it is hidden is already in the buffer on return. Only the fit is
      // owed, because a hidden container measures zero.
      if (tab === 'session' && this.executionActive(this.selectedTicket)) {
        if (this.terminalOpen) {
          var self = this;
          requestAnimationFrame(function() { self.refitTerminal(); });
        } else {
          this.openTerminal();
        }
      }
      if (tab !== 'activity') {
        this._activityLoadSeq++;
        this.activityLoading = false;
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

    closeDetail(writeRoute = true) {
      if (writeRoute) ++this._navigationSeq;
      this.closeAgentPicker(false);
      this.flushEditSave();
      this.closeTerminal();
      this.terminalRW = false;
      this.detailMenuOpen = false;
      this.deleteModal = false;
      this.editing = false;
      this.editingBody = false;
      ++this._editorGeneration;
      this.editSubmitting = false;
      this.deleteSubmitting = false;
      this.selectedTicket = null;
      this.activeTab = 'ticket';
      this.logViewContent = null;
      this.logViewStage = null;
      this.logViewLoading = false;
      this._resetActivity();
      this._resetTicketCost();
      this.ticketChanges = null;
      this.collapsedStages = {};
      this.relExpanded = {};
      this.chain = null;
      this.chainCollapsed = false;
      this.chainError = null;
      this.childrenCollapsed = false;
      this.childrenExpanded = false;
      this.noteDraft = '';
      this.noteResetDrafts();
      this._ticketDetailSeq++;
      if (writeRoute) this.writeHash();
    },

    // Whether the board's own ticket page is what is open. The Archive view
    // drives the same selectedTicket state but brings its own header, so the
    // app header's breadcrumb and action bar must not follow it there.
    boardDetailOpen() {
      return !!this.selectedTicket && this.currentView === 'board';
    },

    // ---- hash routing ------------------------------------------------------

    // Parse a URL hash into {view, ticketId}. Anything unrecognised is the
    // board, so a stale or hand-typed link never leaves the app blank.
    parseHash(hash) {
      var h = String(hash == null ? '' : hash).replace(/^#/, '');
      // An epic page is an overlay over the board, like a ticket page, so it
      // reports the board view and names the epic separately.
      var secondary = h.match(/^\/s\/([^/]+)\/t\/(.+)$/);
      if (secondary) {
        try { return { view: 'board', source: decodeURIComponent(secondary[1]), ticketId: decodeURIComponent(secondary[2]) }; }
        catch (e) { return { view: 'board', ticketId: null }; }
      }
      if (h.indexOf('/e/') === 0 && h.length > 3) {
        var rawEpic = h.slice(3);
        var epicID;
        try { epicID = decodeURIComponent(rawEpic); } catch (e) { epicID = rawEpic; }
        return { view: 'board', ticketId: null, epicId: epicID };
      }
      if (h.indexOf('/t/') === 0 && h.length > 3) {
        var raw = h.slice(3);
        // A malformed escape ("#/t/%") makes decodeURIComponent throw. Left
        // uncaught it would reject applyRoute, and the await in init() would
        // skip the board render that follows it.
        var id;
        try { id = decodeURIComponent(raw); } catch (e) { id = raw; }
        return { view: 'board', ticketId: id };
      }
      const automationsRoute = h.match(/^\/automations(?:\/([^/?]+)(\/edit)?)?(?:\?(.*))?$/);
      if (automationsRoute) {
        const params = new URLSearchParams(automationsRoute[3] || '');
        const filter = params.get('state');
        let id;
        try { id = automationsRoute[1] ? decodeURIComponent(automationsRoute[1]) : null; }
        catch { return { view: 'automations', ticketId: null }; }
        return { view: 'automations', ticketId: null, automationsId: id === 'new' ? null : id,
          automationsMode: id === 'new' ? 'new' : automationsRoute[2] ? 'edit' : id ? 'detail' : 'list',
          automationsFilter: ['enabled', 'paused'].includes(filter) ? filter : 'all', automationsQuery: params.get('q') || '' };
      }
      if (h === '/new') return { view: 'new', ticketId: null };
      if (h === '/stats') return { view: 'stats', ticketId: null };
      if (h.startsWith('/archive/t/') && h.length > 11) {
        const raw = h.slice(11);
        let id;
        try { id = decodeURIComponent(raw); } catch { id = raw; }
        return { view: 'archive', ticketId: id, source: 'primary' };
      }
      if (h === '/archive') return { view: 'archive', ticketId: null };
      if (h === '/settings' || h.startsWith('/settings/') || h.startsWith('/settings?')) {
        const [path, query = ''] = h.split(/\?(.*)/s);
        let section;
        try { section = decodeURIComponent(path.slice('/settings/'.length)); } catch { section = ''; }
        const params = new URLSearchParams(query);
        return { view: 'settings', ticketId: null,
          settingsSection: SETTINGS_ROUTE_KEYS.includes(section) ? section : 'general',
          settingsQuery: params.get('q') || '', settingsField: params.get('field') || '' };
      }
      return { view: 'board', ticketId: null };
    },

    // Serialize the current view back to a hash. Inverse of parseHash.
    routeHash() {
      if (this.currentView === 'automations') {
        const params = new URLSearchParams();
        if (this.automationsFilter !== 'all') params.set('state', this.automationsFilter);
        if (this.automationsQuery) params.set('q', this.automationsQuery);
        const path = this.automationsEditing && !this.automationsSelected ? '/new'
          : this.automationsSelected && (this.automationsDetail || this.automationsEditing)
            ? '/' + encodeURIComponent(this.automationsSelected) + (this.automationsEditing ? '/edit' : '') : '';
        return '#/automations' + path + (params.size ? '?' + params.toString() : '');
      }
      if (this.currentView === 'archive') return this.archiveDetailOpen() ? '#/archive/t/' + encodeURIComponent(this.selectedTicket.id) : '#/archive';
      if (this.currentView === 'new') return '#/new';
      if (this.currentView === 'stats') return '#/stats';
      if (this.currentView === 'settings') {
        const params = new URLSearchParams();
        if (this.settingsQuery) params.set('q', this.settingsQuery);
        if (this.settingsField) params.set('field', this.settingsField);
        const section = SETTINGS_ROUTE_KEYS.includes(this.settingsSection) ? this.settingsSection : 'general';
        return '#/settings/' + section + (params.size ? '?' + params.toString() : '');
      }
      if (this.isSecondary(this.selectedTicket)) return '#/s/' + encodeURIComponent(this.selectedTicket.source) + '/t/' + encodeURIComponent(this.selectedTicket.id);
      if (this.selectedTicket) return '#/t/' + encodeURIComponent(this.selectedTicket.id);
      if (this.selectedEpic) return '#/e/' + encodeURIComponent(this.selectedEpic.id);
      return '#/';
    },

    writeHash({ replace = false } = {}) {
      const adapter = this._settingsHistory;
      if (this._applyingRoute === this._navigationSeq || adapter?.routing === this._navigationSeq) return;
      if (adapter?.motion) {
        adapter.deferredWrite = { navigation: this._navigationSeq, replace };
        return;
      }
      const next = this.routeHash();
      if (location.hash === next) return;
      if (!adapter) {
        if (replace && typeof history !== 'undefined' && history.replaceState) history.replaceState(history.state, '', next);
        else location.hash = next;
        return;
      }
      this.cancelSettingsHistory();
      // Another navigation may supersede an asynchronous route before it is
      // accepted. Index the physical entry, not that older accepted route.
      const physical = historyMetadata(history.state) || adapter.current;
      const entry = { id: physical.id, index: physical.index + (replace ? 0 : 1),
        entry: replace ? physical.entry : historyIdentity(), hash: next };
      history[replace ? 'replaceState' : 'pushState'](historyState(entry), '', next);
      adapter.length = history.length;
      if (!replace) adapter.tip = entry.index;
      adapter.current = entry;
      adapter.observed = entry;
      adapter.seen = historySignature(entry);
      this._settingsHistoryRemember();
    },

    // Own BOTH route listeners. App-created entries use the History API (which
    // emits neither event); a native traversal emits both events, handled once.
    setupSettingsHistory() {
      if (this._settingsHistory) return;
      const initial = historyMetadata(history.state) || { id: historyIdentity(), entry: historyIdentity(), index: 0 };
      history.replaceState(historyState(initial), '', location.hash || '#/');
      const current = { ...initial, hash: location.hash };
      const position = historyPosition();
      const adapter = { current, observed: current, seen: null, routing: null,
        length: history.length, tip: initial.index, offset: position === null ? null : position - initial.index,
        pending: null, motion: null, resuming: null, deferredWrite: null,
        listener: event => { void this._settingsHistoryChanged(false, event.type); } };
      try {
        const saved = JSON.parse(sessionStorage.getItem(HISTORY_KEY));
        if (saved?.id === initial.id && Number.isSafeInteger(saved.tip) && saved.tip >= initial.index) adapter.tip = saved.tip;
      } catch { /* Storage can be unavailable in private browsing. */ }
      this._settingsHistory = adapter;
      this._settingsHistoryRemember();
      window.addEventListener('popstate', adapter.listener);
      window.addEventListener('hashchange', adapter.listener);
    },

    _settingsHistoryRemember() {
      const adapter = this._settingsHistory;
      if (!adapter) return;
      try { sessionStorage.setItem(HISTORY_KEY, JSON.stringify({ id: adapter.current.id, tip: adapter.tip })); }
      catch { /* Storage can be unavailable in private browsing. */ }
    },

    destroySettingsHistory() {
      const adapter = this._settingsHistory;
      if (!adapter) return;
      window.removeEventListener('popstate', adapter.listener);
      window.removeEventListener('hashchange', adapter.listener);
      this.cancelSettingsHistory();
      adapter.resuming?.resolve(false);
      if (adapter.routing === this._navigationSeq) ++this._navigationSeq;
      this._settingsHistory = null;
    },

    // Keep editing must drop the continuation, not rewrite the hash. A queued
    // compensating go() still finishes, but does not create a navigation owner.
    cancelSettingsHistory() {
      const adapter = this._settingsHistory;
      if (!adapter) return;
      adapter.pending?.resolve?.(false);
      adapter.pending = null;
    },

    // Save/Discard passes the guard destination back through Alpine's proxy.
    // A primitive token survives that wrapping; object identity does not.
    // Resume by traversal, never a push that would destroy Forward history.
    resumeSettingsHistory(target) {
      const adapter = this._settingsHistory;
      const pending = adapter?.pending;
      if (!pending || pending.route[HISTORY_DESTINATION] !== target?.[HISTORY_DESTINATION] || this.settingsGuard) return null;
      if (!pending.promise) pending.promise = new Promise(resolve => { pending.resolve = resolve; });
      pending.resume = true;
      const promise = pending.promise;
      if (!adapter.motion) this._settingsHistoryContinue();
      return promise;
    },

    _settingsHistoryContinue() {
      const adapter = this._settingsHistory;
      const pending = adapter?.pending;
      if (!pending?.resume) return;
      adapter.pending = null;
      const delta = pending.to.index - adapter.current.index;
      if (!pending.traversable || !delta) {
        // A missing traversal delta must not become go(0), which reloads.
        void this.navigateDestination({ ...pending.route, replace: true }).then(pending.resolve);
        return;
      }
      adapter.resuming = pending;
      history.go(delta);
    },

    async _settingsHistoryChanged(force = false, eventType) {
      const adapter = this._settingsHistory;
      if (!adapter) return;
      let metadata = historyMetadata(history.state);
      const known = metadata?.id === adapter.current.id;
      // Hash assignments can emit popstate before hashchange, so waiting for
      // stack growth alone misses new entries that replace managed Forward.
      const position = historyPosition();
      const index = position !== null && adapter.offset !== null ? position - adapter.offset : null;
      const newBranch = !known && index === null && adapter.observed.index < adapter.tip;
      // An unknown popstate can also predate setup, so the Forward span alone
      // is not sufficient evidence to compensate a refused route by traversal.
      const forward = adapter.tip - adapter.observed.index;
      const truncated = newBranch && history.length < adapter.length && history.length === adapter.length - forward + 1;
      const traversable = known || index !== null || history.length > adapter.length || truncated || eventType !== 'popstate';
      if (!known) {
        metadata = { id: adapter.current.id, index: index ?? adapter.observed.index + (traversable || newBranch ? 1 : 0), entry: historyIdentity() };
        history.replaceState(historyState(metadata), '', location.hash);
        if ((traversable || newBranch) && (index === null || index > adapter.observed.index)) adapter.tip = metadata.index;
      }
      adapter.length = history.length;
      this._settingsHistoryRemember();
      const to = { ...metadata, hash: location.hash };
      const signature = historySignature(to);
      if (!force && signature === adapter.seen) return;
      adapter.seen = signature;
      const previous = adapter.observed;
      adapter.observed = to;
      if (adapter.motion) {
        const motion = adapter.motion;
        if (to.entry === motion.target.entry && to.id === motion.target.id) {
          adapter.current = to;
          adapter.motion = null;
          const deferred = adapter.deferredWrite;
          adapter.deferredWrite = null;
          if (deferred?.navigation === this._navigationSeq) this.writeHash({ replace: deferred.replace });
          else this._settingsHistoryContinue();
          return;
        }
        if (to.index - previous.index === motion.delta) {
          // A rapid second Back can run before our compensating go(). Its
          // relative delta then lands short of the origin. Correct only after
          // that go finishes; never queue competing compensating traversals.
          motion.delta = motion.target.index - to.index;
          if (motion.delta) history.go(motion.delta);
          return;
        }
      }
      const from = adapter.current;
      this.cancelSettingsHistory();
      const route = { ...this.parseHash(to.hash), [HISTORY_DESTINATION]: Symbol() };
      const navigation = ++this._navigationSeq;
      adapter.routing = navigation;
      const result = await this.navigateDestination(route, navigation);
      if (adapter.routing === navigation) adapter.routing = null;
      if (!this._settingsHistory || navigation !== this._navigationSeq) return false;
      if (result === false) {
        // A rejected route may already have changed its section or detail view.
        // Restore the accepted presentation without revealing a field behind
        // the leave dialog, and keep that restoration off the browser stack.
        adapter.routing = navigation;
        await this.navigateDestination({ ...this.parseHash(from.hash), restore: true }, navigation);
        if (adapter.routing === navigation) adapter.routing = null;
        if (!this._settingsHistory || navigation !== this._navigationSeq) return false;
        const delta = traversable ? from.index - to.index : 0;
        if (this.settingsGuard && this.settingsGuardTarget?.[HISTORY_DESTINATION] === route[HISTORY_DESTINATION]) {
          adapter.pending = { from, to, route, traversable: !!delta };
        }
        if (adapter.motion) {
          // The latest user traversal replaces the guard destination, but the
          // already queued compensation remains the sole owner of restoration.
        } else if (delta) {
          // A newer refusal can finish after the queued compensation has
          // already restored the origin. Restore from the observed physical
          // entry, not the destination captured before awaiting the guard.
          // Otherwise a second go(delta) can overshoot (or emit no event at
          // the stack boundary), leaving motion and its continuation stuck.
          const remaining = from.index - adapter.observed.index;
          if (remaining) {
            adapter.motion = { target: from, delta: remaining };
            history.go(remaining);
          } else {
            this._settingsHistoryContinue();
          }
        } else {
          // Unmanaged replaceState cannot be traversed. Restore in place, still
          // without pushing or cutting off the browser's Forward stack.
          history.replaceState(historyState(from), '', from.hash);
          adapter.observed = from;
          adapter.seen = historySignature(from);
        }
      } else {
        adapter.current = to;
        if (adapter.motion) adapter.motion.target = to;
        this.writeHash({ replace: true });
      }
      if (adapter.resuming) {
        adapter.resuming.resolve(result !== false);
        adapter.resuming = null;
      }
      return result;
    },

    // Apply the current hash. Managed history events call
    // _settingsHistoryChanged(); the fallback hashchange listener calls applyRoute().
    async applyRoute() {
      if (this._settingsHistory) return this._settingsHistoryChanged(true);
      return this.navigateDestination({ ...this.parseHash(location.hash), replace: true });
    },

    async navigateDestination(r, navigation) {
      const resumed = this.resumeSettingsHistory(r);
      if (resumed) return resumed;
      if (navigation == null) navigation = ++this._navigationSeq;
      var mine = () => navigation === this._navigationSeq;
      if (this.currentView === 'settings') ++this._settingsRevealSeq;
      this._applyingRoute = navigation;
      try {
        if (this.currentView !== r.view) {
          if (await this.gotoView(r.view, navigation, r) === false || !mine()) return false;
        }
        if (r.view === 'settings') {
          this.settingsSection = SETTINGS_ROUTE_KEYS.includes(r.settingsSection) ? r.settingsSection : 'general';
          this.settingsQuery = r.settingsQuery || '';
          this.settingsField = r.settingsField || '';
          // Restoring a guarded route must not move focus behind its dialog.
          if (!r.restore && this.settingsApplyRoute) await this.settingsApplyRoute(r, navigation);
          return mine();
        }
        if (r.view === 'automations') {
          if (this.selectedTicket) this.closeDetail(false);
          if (this.selectedEpic && await this.closeEpic(false) === false) return false;
          if (!mine()) return false;
          if (await this.automationsApplyRoute(r, navigation) === false || !mine()) return false;
          this.automationsFilter = r.automationsFilter || 'all';
          this.automationsQuery = r.automationsQuery || '';
          return true;
        }
        if (this.sheet && (r.ticketId || r.epicId)) this.closeSheet(false);
        // The two overlays are exclusive, so the epic branch closes whichever
        // one the hash does not name.
        if (r.epicId) {
          if (this.selectedTicket) this.closeDetail(false);
          if (!this.selectedEpic || this.selectedEpic.id !== r.epicId) await this.openEpic(r.epicId, navigation);
          if (!mine()) return;
        } else if (this.selectedEpic) {
          if (await this.closeEpic(false) === false) return;
          if (!mine()) return;
        }
        if (r.view === 'archive') {
          if (r.ticketId) await this.archiveOpenRow({ id: r.ticketId }, navigation);
          else if (this.archiveDetailOpen()) this.archiveCloseDetail(false);
          if (!mine()) return false;
        } else if (r.ticketId) {
          const identity = { source: r.source || 'primary', id: r.ticketId };
          const alreadySelected = this.sameTicket(this.selectedTicket, identity);
          let t = this.allBoardTickets().find(x => this.sameTicket(x, identity));
          if (!t && !this.isSecondary(identity)) {
            try {
              const response = await fetch('/api/tickets/' + encodeURIComponent(r.ticketId));
              if (!mine()) return false;
              if (response.status === 401) this.needsAuth = true;
              if (!response.ok) throw new Error(response.status === 404 ? 'Historical ticket ' + r.ticketId + ' is missing. It may have been deleted.' : 'Failed to look up ticket (' + response.status + ')');
              const full = await response.json();
              if (!mine()) return false;
              if (full.id !== r.ticketId || this.isSecondary(full)) throw new Error('The primary ticket lookup returned a different ticket.');
              t = { ...full, source: 'primary' };
            } catch (error) {
              if (!mine()) return false;
              this.error = error.message;
            }
          }
          if (t?.status === 'archived') {
            if (await this.gotoView('archive', navigation, { view: 'archive', ticketId: t.id }) === false || !mine()) return false;
            await this.archiveOpenRow(t, navigation);
            if (!mine()) return false;
          } else if (t && !alreadySelected) {
            if (this.isMobile) await this.openMobileTicket(t, navigation);
            else await this.selectTicket(t, navigation);
            if (!mine()) return false;
          } else if (!t && this.selectedTicket) this.closeDetail(false);
        } else if (this.selectedTicket) {
          this.closeDetail(false);
        }
        if (!mine()) return false;
        if (r.view === 'board' && this.terminalWanted()) this.openTerminal();
        return true;
      } finally {
        if (this._applyingRoute === navigation) this._applyingRoute = null;
        if (mine()) this.writeHash({ replace: !!r.replace });
      }
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
      if (!this.ticketCanControl(this.selectedTicket) || this.actionLoading) return;
      if (type === 'skip' && !this.canSkipStage()) {
        this.error = 'Pause first and wait for the agent and review to stop before changing stages.';
        return;
      }
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
        this.applyTicketUpdate(updated);
        this.recomputeBoard();
      } catch (e) {
        this.error = type + ' failed: ' + e.message;
      } finally {
        this.actionLoading = null;
      }
    },

    openDeleteModal() {
      if (!this.ticketCanControl(this.selectedTicket) || this.deleteSubmitting) return;
      this.deleteModal = true;
    },

    closeDeleteModal() {
      if (this.deleteSubmitting) return;
      this.deleteModal = false;
    },

    async deleteSelectedTicket() {
      if (!this.ticketCanControl(this.selectedTicket) || this.deleteSubmitting) return;
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

    // Whether the panel offers schedule controls. An uninitialized ticket has
    // no pickup to postpone, and a later status has already been picked up. The
    // daemon holds the rest of the rules, an open annotation pass among them,
    // and its refusal is what the editor shows.
    canSchedule(ticket) {
      return this.ticketCanControl(ticket) && ticket.kind !== 'epic' && !!ticket.kontora && ['open', 'todo'].includes(ticket.status);
    },

    // A ticket's scheduled pickup as local wall time, or "" when it has none.
    // A value the daemon could not parse is shown as it stands, so a bad hand
    // edit is visible rather than silently dropped.
    //
    // The pattern is the daemon's RFC 3339, not what Date accepts: Date reads
    // "2026-09-01" as a valid instant, and rendering a time for a value the
    // daemon will never fire is worse than showing the text.
    scheduleLabel(ticket) {
      var raw = ticket && ticket.scheduled_at;
      if (!raw) return '';
      var rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;
      if (!rfc3339.test(raw) || isNaN(new Date(raw).getTime())) return raw;
      return this.formatAbsDate(raw);
    },

    // The card chip's two halves, or null when there is nothing to show. A
    // value scheduleLabel could not read has no time to shorten either, so the
    // chip is left off and the rail is where the raw text shows up.
    scheduleChip(ticket) {
      return scheduleChipLabel(ticket && ticket.scheduled_at, this.now ? new Date(this.now) : new Date());
    },

    // The min the schedule inputs carry, recomputed whenever the editor opens so
    // it never sits behind the clock.
    scheduleMin() {
      return scheduleMinLocal();
    },

    // ticket defaults to the open one; the phone's sheet passes the card it was
    // opened from, which is not always what the detail overlay holds.
    openScheduleEditor(ticket) {
      var t = ticket || this.selectedTicket;
      if (!this.canSchedule(t)) return;
      this.scheduleDraft = isoToLocalInput(t.scheduled_at);
      this.scheduleError = null;
      this.scheduleEditing = true;
    },

    closeScheduleEditor() {
      if (this.scheduleSubmitting) return;
      this.scheduleEditing = false;
      this.scheduleError = null;
    },

    async submitSchedule(ticket) {
      var t = ticket || this.selectedTicket;
      // The same parser every schedule surface uses, so the native picker's
      // value and a time typed by hand are read the one way.
      var parsed = parseScheduleInput(this.scheduleDraft, new Date());
      var iso = parsed.iso;
      if (!iso) {
        this.scheduleError = parsed.error || 'Enter a date and a time.';
        return;
      }
      // A zoneless input cannot spell the repeated hour of a DST fall-back, so
      // saving an untouched draft would move such a pickup an hour. An
      // unchanged draft asks for nothing, so it sends nothing.
      if (t && this.scheduleDraft === isoToLocalInput(t.scheduled_at)) {
        this.scheduleEditing = false;
        this.scheduleError = null;
        return;
      }
      await this._postSchedule(ticket, { scheduled_at: iso });
    },

    async clearTicketSchedule(ticket) {
      await this._postSchedule(ticket, { clear: true });
    },

    async _postSchedule(ticket, body) {
      var t = ticket || this.selectedTicket;
      if (!this.ticketCanControl(t) || this.scheduleSubmitting) return;
      this.scheduleSubmitting = true;
      this.scheduleError = null;
      try {
        const res = await fetch('/api/tickets/' + t.id + '/schedule', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          this.scheduleError = data.error || 'Schedule failed';
          return;
        }
        this._applyScheduled(data);
        this.scheduleEditing = false;
      } catch (e) {
        this.scheduleError = 'Schedule failed: ' + e.message;
      } finally {
        this.scheduleSubmitting = false;
      }
    },

    // Fold the daemon's answer back into the board and the open ticket. The
    // response is the whole ticket, so it also carries the status change a
    // schedule makes to a todo ticket.
    _applyScheduled(updated) {
      if (!updated || !updated.id) return;
      var idx = this.tickets.findIndex(t => t.id === updated.id);
      if (idx >= 0) this.tickets[idx] = this.boardEntry(updated);
      // The schedule route answers with the detail projection, body included,
      // so the open ticket can be replaced whole.
      if (this.sameTicket(this.selectedTicket, updated)) {
        var state = this._editorSaves[this.ticketKey(updated)];
        if (this.editing && state) state.draft = this._ordinaryEditorDraft();
        this.selectedTicket = updated;
        if (this.editing && this.editorCanEdit(updated)) this.startEditing();
      }
      this.recomputeBoard();
    },

    ticketActionWouldStart(endpoint, body) {
      if (endpoint === 'run' || endpoint === 'retry') return true;
      if (endpoint === 'move' && body && ['todo', 'in_progress'].includes(body.status)) return true;
      return false;
    },

    async moveTicketVia(ticketId, endpoint, body) {
      this.error = null;
      var ticket = this.allBoardTickets().find(t => this.ticketKey(t) === ticketId);
      if ((ticket && !this.ticketCanControl(ticket)) || this.isSecondary(this.ticketIdentity(ticketId))) return;
      // Starting a ticket goes through the init modal, not /run, /retry, or
      // /move: an unmanaged one cannot execute at all, and a managed one in open
      // gets the form so the run's fields are confirmed before it is queued.
      if (ticket && this.needsInitModal(ticket) && this.ticketActionWouldStart(endpoint, body)) {
        return this.openInitModal(ticket);
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
          this.applyTicketUpdate(updated);
          this.recomputeBoard();
        }
      } catch (e) {
        this.error = endpoint + ' failed: ' + e.message;
      }
    },

    async moveTask(ticketId, newStatus) {
      this.error = null;
      const ticket = this.allBoardTickets().find(t => this.ticketKey(t) === ticketId);
      if ((ticket && !this.ticketCanControl(ticket)) || this.isSecondary(this.ticketIdentity(ticketId))) return;
      if (ticket && this.needsInitModal(ticket) && ['todo', 'in_progress'].includes(newStatus)) {
        // A drag here has already moved the DOM node into the target column;
        // rebuild from canonical data so the card snaps back if the user
        // dismisses the init modal. The drag dropped the render cache for both
        // columns, so this render patches them even though no status changed.
        this.recomputeBoard();
        return this.openInitModal(ticket);
      }
      if (!ticket) return;
      let chain = moves.get(ticketId);
      const mine = () => !this._appDestroyed && moves.get(ticketId) === chain &&
        !chain.touched.has(ticketId) && this.tickets.find(t => t.id === ticketId) === chain.current;
      if (!chain || !mine()) {
        chain = { previous: { ...ticket }, requests: [], pending: 0, touched: this._beginSnapshot(), current: ticket };
        moves.set(ticketId, chain);
      }
      // Rollback skips failed optimistic states and keeps the last successful response or the original ticket.
      // Ordinary moves park custom statuses; only a daemon claim activates them.
      const move = { result: { ...ticket, status: newStatus, execution_active: newStatus === 'in_progress' }, failed: false };
      chain.requests.push(move);
      chain.pending++;
      const reconcile = () => {
        if (mine()) {
          const latest = chain.requests.filter(request => !request.failed).pop();
          const value = latest ? latest.result : chain.previous;
          if (value !== chain.value) {
            this.applyTicketUpdate(value);
            chain.current = this.tickets.find(t => t.id === ticketId);
            chain.value = value;
            // Ignore our own writes, but retain invalidation from any intervening authoritative update.
            chain.touched.delete(ticketId);
          }
        }
        this.recomputeBoard();
      };
      reconcile();
      try {
        const res = await fetch('/api/tickets/' + ticketId + '/move', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ status: newStatus }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || 'Move failed';
          move.failed = true;
        } else {
          const updated = await res.json().catch(() => null);
          if (updated?.id === ticketId) move.result = updated;
        }
        reconcile();
      } catch (e) {
        this.error = 'Move failed: ' + e.message;
        move.failed = true;
        reconcile();
      } finally {
        if (--chain.pending === 0) {
          this._endSnapshot(chain.touched);
          if (moves.get(ticketId) === chain) moves.delete(ticketId);
        }
      }
    },

    async startEditing() {
      if (!this.editorCanEdit() || !this.ticketDescriptionLoaded()) return;
      this.editingBody = false;
      var pipeline = this.selectedTicket.pipeline || '';
      this._editAgentFollowsProject = !this.selectedTicket.agent_override;
      var agent = this.selectedTicket.agent_override ? (this.selectedTicket.agent || '') : '';
      var generation = ++this._editorGeneration;
      var id = this.ticketKey(this.selectedTicket);
      this._editorTicketKey = id;
      var state = this._loadEditorDraft(this.selectedTicket, {
        description: this.descriptionText(this.selectedTicket),
        pipeline: pipeline,
        path: this.selectedTicket.path || '',
        agent: agent,
        branch: this.selectedTicket.branch || '',
        base_branch: this.selectedTicket.base_branch || '',
      });
      if (this.selectedTicket.kind === 'epic') {
        delete state.ack.pipeline;
        delete state.draft.pipeline;
        delete state.target.pipeline;
      }
      var draft = state.draft;
      this._bodyLead = draft.description.startsWith('\n') ? '\n' : '';
      this.editForm = {
        body: draft.description.slice(this._bodyLead.length),
        pipeline: draft.pipeline,
        path: draft.path,
        agent: draft.agent,
        branch: draft.branch,
        base_branch: draft.base_branch,
      };
      this.editing = true;
      this.editSubmitting = false;
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
      // The native fallback still needs its x-for options before Alpine applies
      // the value, or an older browser would lose the current selection.
      await this.$nextTick();
      if (generation !== this._editorGeneration || this.ticketKey(this.selectedTicket) !== id) return;
      this.editForm.pipeline = draft.pipeline;
      this.editForm.agent = draft.agent;
    },

    // Membership is a preview snapshot. The backend resolves defaults again on submit.
    projectDefaultsFor(path, epicChild = false) {
      var project = this.projectForPath(path) || {};
      var source = epicChild && project.epic_child_pipeline ? 'epic_child_pipeline'
        : project.pipeline ? 'pipeline' : this.configCache?.default_pipeline ? 'default_pipeline' : '';
      return {
        pipeline: source === 'default_pipeline' ? this.configCache.default_pipeline : (project[source] || ''),
        pipelineSource: source,
        agent: project.agent || '',
      };
    },

    // Path edits preserve the stored workflow. Only the inherited agent follows the project.
    onEditPathChange() {
      var next = this.projectDefaultsFor(this.editForm.path);
      if (this._editAgentFollowsProject) this.editForm.agent = next.agent;
      this._editInherited = next;

      return this.saveEdit();
    },

    _ordinaryEditorDraft() {
      var draft = { description: this._bodyLead + this.editForm.body };
      ['path', 'agent', 'branch', 'base_branch'].forEach(key => { draft[key] = this.editForm[key] || ''; });
      if (this.selectedTicket?.kind !== 'epic') draft.pipeline = this.editForm.pipeline || '';
      return draft;
    },

    async saveEdit() {
      if (!this.selectedTicket || !this.editing) return true;
      var id = this.ticketKey(this.selectedTicket);
      if (id !== this._editorTicketKey) return false;
      var submitted = this._ordinaryEditorDraft();
      if (!this.ticketCanEdit(this.selectedTicket)) {
        if (this._editorSaves[id]) this._editorSaves[id].draft = { ...submitted };
        return false;
      }
      var generation = this._editorGeneration;
      this.editSubmitting = true;
      this.editSaved = false;
      var pending = this._queueEditorSave(id, submitted, generation);
      var ok = await pending;
      if (this.ticketKey(this.selectedTicket) === id && generation === this._editorGeneration && this._editorSaves[id]?.tail === pending) {
        this.editSubmitting = false;
        this.editSaved = ok && this._sameEditorDraft(this._ordinaryEditorDraft(), submitted);
      }
      return ok;
    },

    ticketDescriptionLoaded(ticket = this.selectedTicket) {
      // Board and SSE projections omit both fields, including for empty descriptions.
      return typeof ticket?.description === 'string' || typeof ticket?.body === 'string';
    },

    descriptionText(ticket) {
      if (typeof ticket?.description === 'string') return ticket.description;
      return (ticket?.body || '').split(/^[^\S\n]*## Notes[^\S\n]*$/m)[0];
    },

    descriptionTail(ticket) {
      if (typeof ticket?.description_tail === 'string') return ticket.description_tail;
      var body = ticket?.body || '';
      return body.slice(this.descriptionText(ticket).length);
    },

    _sameEditorDraft(a, b) {
      var keys = new Set([...Object.keys(a), ...Object.keys(b)]);
      return [...keys].every(key => a[key] === b[key]);
    },

    _loadEditorDraft(ticket, snapshot) {
      var key = this.ticketKey(ticket);
      var state = this._editorSaves[key];
      if (!state || (!state.pending && this._sameEditorDraft(state.draft, state.ack))) {
        state = this._editorSaves[key] = {
          ack: { ...snapshot }, draft: { ...snapshot }, target: { ...snapshot }, sequence: 0,
          pending: 0, ackSequence: state?.ackSequence || 0, tail: Promise.resolve(true),
        };
      } else {
        var ack = state.pending ? { ...state.ack } : {};
        var draft = state.pending ? { ...state.draft } : {};
        Object.keys(snapshot).forEach(key => {
          ack[key] = key in state.ack ? state.ack[key] : snapshot[key];
          draft[key] = key in state.draft ? state.draft[key] : snapshot[key];
        });
        state.ack = ack;
        state.draft = draft;
        if (!state.pending) state.target = { ...ack };
      }
      return state;
    },

    async _waitEditorSaves(id) {
      // A completion may enqueue another submission before its caller closes.
      var tail;
      do {
        tail = this._editorSaves[id]?.tail;
        if (tail) await tail;
      } while (tail !== this._editorSaves[id]?.tail);
    },

    _queueEditorSave(id, draft, generation) {
      var state = this._editorSaves[id];
      if (!state) return Promise.resolve(false);
      state.draft = { ...draft };
      if (this._sameEditorDraft(draft, state.target)) return state.tail;
      var submitted = { ...draft };
      var sequence = ++state.sequence;
      state.pending++;
      state.target = submitted;
      var save = async () => {
        var body = {};
        Object.keys(submitted).forEach(key => {
          if (submitted[key] !== state.ack[key]) body[key] = submitted[key];
        });
        try {
          if (Object.keys(body).length) {
            var update = this._selectedTicketUpdateSeq;
            var epicUpdate = this._epicUpdateSeq;
            var res = await fetch(this.ticketURL(id), {
              method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
            });
            if (!res.ok) {
              var failure = await res.json().catch(() => ({}));
              throw new Error(failure.error || 'Failed to save');
            }
            var full = await res.json();
            full = { ...full, source: this.ticketIdentity(id).source || full.source };
            if (this.ticketKey(full) !== id) throw new Error('Invalid ticket response');
            while (generation === this._editorGeneration &&
                ((this.ticketKey(this.selectedTicket) === id && update !== this._selectedTicketUpdateSeq) ||
                 (this.selectedEpic?.id === id && epicUpdate !== this._epicUpdateSeq))) {
              update = this._selectedTicketUpdateSeq;
              epicUpdate = this._epicUpdateSeq;
              var refresh = await fetch(this.ticketURL(id));
              if (!refresh.ok) throw new Error('Failed to refresh saved ticket');
              full = await refresh.json();
              full = { ...full, source: this.ticketIdentity(id).source || full.source };
              if (this.ticketKey(full) !== id) throw new Error('Invalid ticket response');
            }
            if (generation === this._editorGeneration) {
              if (this.ticketKey(this.selectedTicket) === id && update === this._selectedTicketUpdateSeq) this.selectedTicket = full;
              if (this.selectedEpic?.id === id && epicUpdate === this._epicUpdateSeq) {
                this.selectedEpic = full;
                this._epicDetail = full;
              }
              var entries = this.isSecondary(full) ? this.secondaryTickets : this.tickets;
              var idx = entries.findIndex(t => this.ticketKey(t) === id);
              if (idx >= 0 && update === this._selectedTicketUpdateSeq && epicUpdate === this._epicUpdateSeq) {
                entries[idx] = this.boardEntry(full);
                this.recomputeBoard();
              }
            }
          }
          state.ack = submitted;
          state.ackSequence++;
          return true;
        } catch (e) {
          this.error = e.message;
          return false;
        } finally {
          state.pending--;
          if (state.sequence === sequence) state.target = { ...state.ack };
        }
      };
      state.tail = state.tail.then(save);
      return state.tail;
    },

    debounceSaveEdit() {
      this.editSaved = false;
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
      return this.saveEdit();
    },

    // Enter body edit mode with the clicked block left where it is on screen.
    beginBodyEdit(event) {
      if (this.editorClaimLocked()) return;
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
      if (!ticket) return false;
      if ((ticket.history || []).some(function (run) { return run.stage === stage; })) return true;
      if (!ticket.stages || ticket.stages.length === 0) return false;
      if (ticket.status === 'todo' || ticket.status === 'open') return false;
      if (ticket.status === 'done') return true;
      var stageIdx = ticket.stages.indexOf(stage);
      var currentIdx = ticket.stages.indexOf(ticket.stage);
      if (currentIdx < 0) return false;
      return stageIdx >= 0 && stageIdx < currentIdx;
    },

    clickStage(stage) {
      if (!this.selectedTicket) return;
      if (stage === this.selectedTicket.stage && this.executionActive(this.selectedTicket)) {
        this._invalidateStageLogs();
        this.logViewContent = null;
        this.logViewStage = null;
        this.detailTab = 'terminal';
        this.openTerminal();
        return;
      }
      if (!this.isStageClickable(stage, this.selectedTicket)) return;
      this.detailTab = 'logs';
      this.closeTerminal();
      this.fetchStageLogs(this.selectedTicket.id, stage);
    },

    // Active agents and reviews own the stage until they stop. A queued ticket
    // stays queued after a stage edit; selecting a stage does not retry it.
    canSetStage(ticket = this.selectedTicket) {
      const status = ticket?.status;
      if (!this.ticketCanControl(ticket) || this.executionActive(ticket) || !status || ticket.kind === 'epic' || this.plannotatorInFlight?.[ticket.id]) return false;
      return ['open', 'todo', 'paused', 'human_review', 'done', 'cancelled']
        .concat(this.configCache?.custom_statuses || [])
        .includes(status);
    },

    canSkipStage(ticket = this.selectedTicket) {
      return !!(ticket?.kontora && ticket.stage && ticket.stages?.length && this.canSetStage(ticket));
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
      if (!this.canSetStage()) {
        this.error = 'Pause first and wait for the agent and review to stop before changing stages.';
        return;
      }
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

    _stageLogSeq: 0,

    _invalidateStageLogs() {
      ++this._stageLogSeq;
      this.logViewLoading = false;
    },

    async fetchStageLogs(ticketId, stage) {
      if (!this.selectedTicket || this.selectedTicket.id !== ticketId || this.isSecondary(this.selectedTicket) || this.currentView !== 'board') return;
      const key = this.ticketKey(this.selectedTicket);
      const sequence = ++this._stageLogSeq;
      const mine = () => sequence === this._stageLogSeq && !this._appDestroyed &&
        this.currentView === 'board' && this.ticketKey(this.selectedTicket) === key;
      this._invalidateRawLog();
      this.logViewStage = stage;
      this.logViewLoading = true;
      this.logViewContent = null;
      try {
        var url = '/api/tickets/' + ticketId + '/logs';
        if (stage) url += '?stage=' + encodeURIComponent(stage);
        var res = await fetch(url);
        if (!res.ok) {
          var errData = await res.json().catch(function() { return {}; });
          if (!mine()) return;
          this.error = errData.error || 'Failed to load stage logs';
          this.logViewContent = null;
          this.logViewLoading = false;
          return;
        }
        var data = await res.json();
        if (!mine()) return;
        this.logViewContent = data.content || '';
      } catch (e) {
        if (!mine()) return;
        this.error = 'Failed to load stage logs';
        this.logViewContent = null;
      }
      if (mine()) this.logViewLoading = false;
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
        if (this.executionActive(ticket)) return 'stage-active-progress';
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
      if (this.executionActive(this.selectedTicket)) {
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
      if (this.outputTab() === 'session' && this.terminalOpen && termState.term) {
        try { termState.term.scrollToBottom(); } catch (e) {}
        return;
      }
      var el = document.getElementById(this.outputContainerID('activity')) || document.getElementById('stage-log-pre');
      if (el) el.scrollTop = el.scrollHeight;
    },

    _rawLogEpoch: 0,

    // Polling must not cancel a pending raw-log request.
    _invalidateRawLog() {
      this._rawLogEpoch++;
    },

    // Open the currently viewed log as plain text in a new tab.
    async openRawLog() {
      var subject = this.outputSubject();
      if (!subject || this._appDestroyed) return;
      var generation = this._rawLogEpoch;
      var viewingActivity = this.outputTab() === 'activity';
      var content = this.selectedEpic || viewingActivity ? null : this.logViewContent;
      if (content === null && this.activity && !this.activity.tape) content = this.activity.content;
      if (content === null || content === undefined) {
        try {
          var url = '/api/tickets/' + encodeURIComponent(subject.id) + '/logs';
          var stage = viewingActivity ? this.activityStage : this.logViewStage;
          stage = stage || subject.stage;
          if (stage) url += '?stage=' + encodeURIComponent(stage);
          var res = await fetch(url);
          if (res.ok) {
            var data = await res.json();
            content = data.content || '';
          }
        } catch (e) { /* fall through to the guard below */ }
      }
      if (this._appDestroyed || generation !== this._rawLogEpoch || this.outputSubject()?.id !== subject.id) return;
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
  };
}
