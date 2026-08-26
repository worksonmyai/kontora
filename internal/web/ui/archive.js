import { filterSplitTerms, filterUnquote } from './filter.js';

// Archive view: the list of archived tickets, its filters, and the read-only
// detail overlay one row opens. Merged into the kontora() component by
// index.js, so `this` here is the same Alpine object the board runs on. The
// overlay drives the board's own `selectedTicket`, which is what lets
// stageRibbon(), churn(), ticketWall() and tapeTokens() work on it untouched.
//
// Every key this mixin defines is prefixed `archive*` / `_archive*`: merge()
// throws on a repeat, and activity.js already owns the unprefixed helpers.

// The `key:value` terms the archive box understands. Wider than the board's
// two, because the archive's columns are what a reader narrows on.
const ARCHIVE_TOKEN_KEYS = ['project', 'agent', 'pipeline', 'status', 'branch'];

const ARCHIVE_RANGES = ['all', '7d', '30d', '90d'];
const ARCHIVE_RANGE_DAYS = { '7d': 7, '30d': 30, '90d': 90 };
const ARCHIVE_RANGE_LABELS = { all: 'All time', '7d': 'Last 7 days', '30d': 'Last 30 days', '90d': 'Last 90 days' };

// The sortable columns, in header order.
const ARCHIVE_SORT_KEYS = ['id', 'title', 'project', 'status', 'wall', 'archived'];

// Sorting these newest/largest first reads as the useful default; the string
// columns read the other way.
const ARCHIVE_SORT_DESC_FIRST = { archived: true, wall: true };

// A row whose ticket carries no archived_from — archived before that field
// existed — still needs a value to chip, group and filter by.
export function archiveRowStatus(row) {
  return (row && row.status) || 'unknown';
}

function archiveText(v) {
  return String(v == null ? '' : v).toLowerCase();
}

// Split the archive box into its `key:value` tokens and the free text left
// over. The tokens keep the term as typed, so removing a chip can cut that
// exact substring back out of the query. A key with nothing after the colon
// constrains nothing, so the list does not empty out between the colon and the
// first typed character.
export function archiveParseQuery(raw) {
  const tokens = [];
  const free = [];
  filterSplitTerms(raw).forEach(term => {
    const m = /^([a-zA-Z]+):(=?)(.*)$/.exec(term);
    const key = m ? m[1].toLowerCase() : '';
    if (!m || ARCHIVE_TOKEN_KEYS.indexOf(key) < 0) {
      free.push(archiveText(filterUnquote(term)));
      return;
    }
    const value = archiveText(filterUnquote(m[3]));
    if (value) tokens.push({ key: key, value: value, exact: m[2] === '=', term: term });
  });
  return { tokens: tokens, text: free.join(' ') };
}

// Tokens of different keys narrow together; repeats of one key widen it. A
// typed value matches as a substring; the `=` form matches the whole field.
// Same rule as the board box, so the syntax carries over.
function archiveTokenMatch(tokens, key, field) {
  const want = tokens.filter(t => t.key === key);
  if (!want.length) return true;
  const f = archiveText(field);
  return want.some(t => (t.exact ? f === t.value : f.indexOf(t.value) >= 0));
}

function archiveCutoff(range, nowMs) {
  const days = ARCHIVE_RANGE_DAYS[range];
  if (!days) return 0;
  return (Number(nowMs) || Date.now()) - days * 86400000;
}

function archiveRowMatches(row, parsed, state, cutoff) {
  const status = archiveRowStatus(row);
  if (state.status !== 'all' && status !== state.status) return false;
  if (state.project !== 'all' && (row.project || '') !== state.project) return false;
  if (state.pipeline !== 'all' && (row.pipeline || '') !== state.pipeline) return false;
  if (state.agent !== 'all' && (row.agent || '') !== state.agent) return false;
  if (cutoff) {
    const at = Date.parse(row.archived_at || '');
    if (isNaN(at) || at < cutoff) return false;
  }
  if (!archiveTokenMatch(parsed.tokens, 'project', row.project)) return false;
  if (!archiveTokenMatch(parsed.tokens, 'agent', row.agent)) return false;
  if (!archiveTokenMatch(parsed.tokens, 'pipeline', row.pipeline)) return false;
  if (!archiveTokenMatch(parsed.tokens, 'branch', row.branch)) return false;
  if (!archiveTokenMatch(parsed.tokens, 'status', status)) return false;
  if (!parsed.text) return true;
  return [row.id, row.title, row.branch, row.project, row.agent]
    .some(f => f && archiveText(f).indexOf(parsed.text) >= 0);
}

function archiveSortValue(row, key) {
  switch (key) {
    case 'wall': return Number(row.wall_seconds) || 0;
    case 'archived': return Date.parse(row.archived_at || '') || 0;
    case 'status': return archiveText(archiveRowStatus(row));
    case 'title': return archiveText(row.title);
    case 'project': return archiveText(row.project);
    default: return archiveText(row.id);
  }
}

// One-entry memo for archiveView(). Module scope, not component state, for the
// reason markdown.js keeps mdCache out there: the template reads the derived
// list several times per render, and a cache written from inside a render
// effect would make every one of those reads a reactive write.
const archiveMemo = { key: null, rows: null, value: null };

// Filter and sort in one pass. Exported and driven directly by the node suite,
// the way statsDerive is.
export function archiveDerive(rows, state, nowMs) {
  const all = Array.isArray(rows) ? rows : [];
  const parsed = archiveParseQuery(state.query);
  const cutoff = archiveCutoff(state.range, nowMs);
  const out = all.filter(r => archiveRowMatches(r, parsed, state, cutoff));
  const key = ARCHIVE_SORT_KEYS.indexOf(state.sortKey) >= 0 ? state.sortKey : 'archived';
  const dir = state.sortDir === 'asc' ? 1 : -1;
  out.sort((a, b) => {
    const va = archiveSortValue(a, key);
    const vb = archiveSortValue(b, key);
    if (va < vb) return -dir;
    if (va > vb) return dir;
    // Ties break on id so repeated renders agree on the same order.
    return archiveText(a.id) < archiveText(b.id) ? -1 : (archiveText(a.id) > archiveText(b.id) ? 1 : 0);
  });
  return { rows: out, tokens: parsed.tokens, total: all.length, shown: out.length };
}

export function kontoraArchive() {
  return {
    archiveRows: [],
    archiveLoading: false,
    archiveError: null,
    archiveQuery: '',
    archiveStatus: 'all',
    archiveProject: 'all',
    archivePipeline: 'all',
    archiveAgent: 'all',
    archiveRange: 'all',
    archiveSortKey: 'archived',
    archiveSortDir: 'desc',
    archiveTab: 'ticket',
    archiveDetailLoading: false,
    // The note prompt both entry points open. archivePromptId names the ticket
    // being archived, and is what makes the modal visible.
    archivePromptId: null,
    archiveNoteDraft: '',
    archiveSubmitting: false,
    archivePromptError: null,
    // The ticket the archive overlay put in selectedTicket. Tracked so leaving
    // the view only clears an overlay this view opened, never a board ticket a
    // route change opened on the way out.
    _archiveDetailId: null,

    archiveRanges: ARCHIVE_RANGES,
    archiveSortKeys: ARCHIVE_SORT_KEYS,
    archiveRowStatus: archiveRowStatus,

    // ---- the list ----------------------------------------------------------

    // openArchive is called by gotoView, the one place currentView changes.
    openArchive() {
      this.archiveLoad();
    },

    // Leaving the view drops the overlay, but only when the overlay is the one
    // this view opened: applyRoute opens a board ticket before it switches
    // views, and that ticket must survive the switch.
    closeArchive() {
      if (this._archiveDetailId && this.selectedTicket && this.selectedTicket.id === this._archiveDetailId) {
        this._archiveClearDetail();
      }
      this._archiveDetailId = null;
    },

    async archiveLoad() {
      if (!this.archiveRows.length) this.archiveLoading = true;
      try {
        const res = await fetch('/api/tickets/archived');
        // A session that expires while the archive is open shows the login
        // prompt, the way every other fetch in the app treats a 401.
        if (res.status === 401) {
          this.needsAuth = true;
          this.archiveError = null;
          return;
        }
        if (!res.ok) throw new Error('archive request failed (' + res.status + ')');
        const payload = await res.json();
        this.archiveRows = payload.tickets || [];
        this.archiveError = null;
      } catch (e) {
        this.archiveError = String((e && e.message) || e);
      } finally {
        this.archiveLoading = false;
      }
    },

    // The filtered and sorted list, memoized on the filter state and the row
    // array's identity. Every mutation replaces archiveRows rather than writing
    // into it, so identity alone catches a changed row.
    archiveView() {
      const state = {
        query: this.archiveQuery,
        status: this.archiveStatus,
        project: this.archiveProject,
        pipeline: this.archivePipeline,
        agent: this.archiveAgent,
        range: this.archiveRange,
        sortKey: this.archiveSortKey,
        sortDir: this.archiveSortDir,
      };
      // The clock only enters the key while a date range is set, so the 30s
      // tick does not re-derive a list no window applies to.
      const key = Object.values(state).concat(state.range === 'all' ? '' : this.now).join('\u0000');
      if (archiveMemo.key === key && archiveMemo.rows === this.archiveRows) return archiveMemo.value;
      archiveMemo.key = key;
      archiveMemo.rows = this.archiveRows;
      archiveMemo.value = archiveDerive(this.archiveRows, state, this.now);
      return archiveMemo.value;
    },

    archiveCountLine() {
      const v = this.archiveView();
      return v.total + ' archived · ' + v.shown + ' shown';
    },

    // Pill options are the values actually present, so a configured custom
    // status appears without being named here.
    archiveOptions(field) {
      const seen = Object.create(null);
      this.archiveRows.forEach(r => {
        const v = field === 'status' ? archiveRowStatus(r) : (r[field] || '');
        if (v) seen[v] = true;
      });
      return ['all'].concat(Object.keys(seen).sort());
    },

    archiveRangeLabel(range) {
      return ARCHIVE_RANGE_LABELS[range] || range;
    },

    archiveSetFilter(field, value) {
      const key = 'archive' + field.charAt(0).toUpperCase() + field.slice(1);
      this[key] = value;
    },

    archiveFilterValue(field) {
      return this['archive' + field.charAt(0).toUpperCase() + field.slice(1)];
    },

    archiveFiltersActive() {
      return !!this.archiveQuery || this.archiveStatus !== 'all' || this.archiveProject !== 'all'
        || this.archivePipeline !== 'all' || this.archiveAgent !== 'all' || this.archiveRange !== 'all';
    },

    archiveClearFilters() {
      this.archiveQuery = '';
      this.archiveStatus = 'all';
      this.archiveProject = 'all';
      this.archivePipeline = 'all';
      this.archiveAgent = 'all';
      this.archiveRange = 'all';
    },

    // Cut one parsed token back out of the query, by the substring it was
    // written as.
    archiveRemoveToken(term) {
      this.archiveQuery = filterSplitTerms(this.archiveQuery).filter(t => t !== term).join(' ');
    },

    archiveSort(key) {
      if (this.archiveSortKey === key) {
        this.archiveSortDir = this.archiveSortDir === 'asc' ? 'desc' : 'asc';
        return;
      }
      this.archiveSortKey = key;
      this.archiveSortDir = ARCHIVE_SORT_DESC_FIRST[key] ? 'desc' : 'asc';
    },

    archiveSortMark(key) {
      if (this.archiveSortKey !== key) return '';
      return this.archiveSortDir === 'asc' ? '▲' : '▼';
    },

    archiveToggleSortDir() {
      this.archiveSortDir = this.archiveSortDir === 'asc' ? 'desc' : 'asc';
    },

    // ---- sidebar facets ----------------------------------------------------

    // While the archive is open the sidebar's project and agent rows drive the
    // archive's own filters rather than writing a board filter token.
    archiveToggleFacet(field, value) {
      this.archiveSetFilter(field, this.archiveFilterValue(field) === value ? 'all' : value);
    },

    archiveFacetActive(field, value) {
      return this.archiveFilterValue(field) === value;
    },

    archiveFacetCount(field, value) {
      return this.archiveRows.filter(r => (r[field] || '') === value).length;
    },

    // ---- row rendering -----------------------------------------------------

    archiveWall(row) {
      const secs = Number(row && row.wall_seconds) || 0;
      return secs > 0 ? this.formatSeconds(secs) : '—';
    },

    archiveWhen(row) {
      return this.timeAgo(row && row.archived_at) || '—';
    },

    archiveCopyRow(row) {
      const text = [row.id, row.branch].filter(Boolean).join(' · ');
      navigator.clipboard.writeText(text);
      this.showToast('copied · ' + text);
    },

    // ---- the read-only detail overlay --------------------------------------

    // The overlay is open only for a ticket this view itself opened. Clicking
    // the sidebar's Archive item while a board ticket is on screen leaves that
    // ticket in selectedTicket, and rendering it here would stamp a running
    // ticket as archived.
    archiveDetailOpen() {
      return this.currentView === 'archive'
        && !!this.selectedTicket
        && this.selectedTicket.id === this._archiveDetailId;
    },

    async archiveOpenRow(row) {
      if (!row || !row.id) return;
      if (this.selectedTicket && this.selectedTicket.id === row.id) return;
      this.archiveTab = 'ticket';
      this._resetActivity();
      this.ticketChanges = null;
      this._archiveDetailId = row.id;
      // The row's fields are enough to draw the header while the body loads.
      this.selectedTicket = { id: row.id, title: row.title, status: 'archived', branch: row.branch };
      this.archiveDetailLoading = true;
      try {
        const res = await fetch('/api/tickets/' + encodeURIComponent(row.id));
        if (res.ok) {
          const full = await res.json();
          if (this._archiveDetailId === row.id) this.selectedTicket = full;
        }
      } catch (e) {
        this.error = 'Failed to load ticket details';
      }
      this.archiveDetailLoading = false;
      const t = this.selectedTicket;
      if (!t || t.id !== row.id) return;
      if (t.branch) this.fetchChanges(t.id);
      const latest = this.latestCompletedRun();
      if (latest) this.fetchActivity(latest.stage, latest.run);
    },

    archiveCloseDetail() {
      this._archiveClearDetail();
      this._archiveDetailId = null;
    },

    _archiveClearDetail() {
      this.selectedTicket = null;
      this.archiveTab = 'ticket';
      this.archiveDetailLoading = false;
      this._resetActivity();
      this.ticketChanges = null;
    },

    // Meters beside the ribbon. ribbonMeters() only produces meters for the two
    // live statuses, so the overlay has its own rather than teaching the board's
    // about a status that never runs.
    archiveMeters() {
      const t = this.selectedTicket;
      if (!t) return [];
      const out = [];
      const wall = this.ticketWall();
      if (wall) out.push({ k: 'wall', v: wall });
      const segs = this.stageRibbon();
      if (segs.length) {
        out.push({ k: 'stages', v: segs.filter(s => s.state === 'done').length + ' / ' + segs.length });
      }
      const tokens = this.tapeTokens(this.activity && this.activity.tape);
      if (tokens) out.push({ k: 'tokens', v: tokens });
      return out;
    },

    // The status the overlay's chip shows. Not archiveRowStatus(): a list row
    // carries archived_from in its own `status`, while GET /api/tickets/{id}
    // answers status: archived with archived_from beside it.
    archiveDetailStatus() {
      return (this.selectedTicket && this.selectedTicket.archived_from) || 'unknown';
    },

    // The archived stamp as the header pill writes it.
    archiveStamp() {
      const t = this.selectedTicket;
      if (!t || !t.archived_at) return 'archived';
      const parts = ['archived', this.timeAgo(t.archived_at)];
      if (t.archived_by) parts.push('by ' + t.archived_by);
      return parts.filter(Boolean).join(' · ');
    },

    // Churn is empty whenever the branch was deleted after the archive, which
    // is the common case. The rail names that rather than rendering blank.
    archiveChurnEmptyText() {
      const t = this.selectedTicket;
      if (!t || !t.branch) return 'This ticket never had a branch.';
      if (!this.ticketChanges) return 'Reading ' + t.branch + '…';
      return t.branch + ' is gone or carries no commits, so there is nothing left to diff.';
    },

    // ---- archive and restore -----------------------------------------------

    openArchivePrompt(id) {
      if (!id) return;
      this.archivePromptId = id;
      this.archiveNoteDraft = '';
      this.archivePromptError = null;
    },

    closeArchivePrompt() {
      this.archivePromptId = null;
      this.archiveNoteDraft = '';
      this.archivePromptError = null;
      this.archiveSubmitting = false;
    },

    async submitArchive() {
      const id = this.archivePromptId;
      if (!id || this.archiveSubmitting) return;
      this.archiveSubmitting = true;
      this.archivePromptError = null;
      try {
        const res = await fetch('/api/tickets/' + encodeURIComponent(id) + '/archive', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ note: (this.archiveNoteDraft || '').trim() }),
        });
        if (!res.ok) {
          const err = await res.json().catch(() => ({}));
          throw new Error(err.error || 'archive failed (' + res.status + ')');
        }
        const ticket = await res.json();
        this.closeArchivePrompt();
        this.showToast(id + ' archived');
        // The event carries status archived, so this is what drops the ticket
        // from the board and closes its detail panel.
        this.applyTicketUpdate(ticket);
        this.recomputeBoard();
        if (this.currentView === 'archive') this.archiveLoad();
      } catch (e) {
        this.archivePromptError = String((e && e.message) || e);
      } finally {
        this.archiveSubmitting = false;
      }
    },

    // Optimistic: the row goes as soon as the click lands, and comes back at
    // the position it held if the daemon refuses.
    async archiveRestore(id) {
      const idx = this.archiveRows.findIndex(r => r.id === id);
      if (idx < 0) return;
      const row = this.archiveRows[idx];
      this.archiveRows = this.archiveRows.filter(r => r.id !== id);
      if (this.selectedTicket && this.selectedTicket.id === id) this.archiveCloseDetail();
      try {
        const res = await fetch('/api/tickets/' + encodeURIComponent(id) + '/restore', { method: 'POST' });
        if (!res.ok) {
          const err = await res.json().catch(() => ({}));
          throw new Error(err.error || 'restore failed (' + res.status + ')');
        }
        const ticket = await res.json();
        this.applyTicketUpdate(ticket);
        this.recomputeBoard();
        this.showToast(id + ' restored to the board · ' + (ticket.status || ''));
      } catch (e) {
        const back = this.archiveRows.slice();
        back.splice(Math.min(idx, back.length), 0, row);
        this.archiveRows = back;
        this.error = String((e && e.message) || e);
      }
    },

    // A ticket that is no longer archived leaves the list, and takes the
    // overlay with it: a restore from another browser or a file edit would
    // otherwise leave a read-only page open on a ticket that is back on the
    // board.
    archiveDropRow(id) {
      if (this._archiveDetailId === id) this.archiveCloseDetail();
      if (!this.archiveRows.some(r => r.id === id)) return;
      this.archiveRows = this.archiveRows.filter(r => r.id !== id);
    },

    // Patch one row in place of a reload, for the SSE update that arrives when
    // a ticket is archived while this view is open.
    archivePatchRow(ticket) {
      const idx = this.archiveRows.findIndex(r => r.id === ticket.id);
      if (idx < 0) {
        // A ticket archived from somewhere else: the list has to be re-read,
        // because the row carries a wall time the event does not.
        this.archiveLoad();
        return;
      }
      const next = this.archiveRows.slice();
      next[idx] = Object.assign({}, next[idx], {
        title: ticket.title,
        status: ticket.archived_from || '',
        archived_at: ticket.archived_at,
        archived_by: ticket.archived_by,
      });
      this.archiveRows = next;
    },
  };
}
