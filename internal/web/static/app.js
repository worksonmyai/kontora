// How long the first key of a two-key shortcut stays armed.
const KEY_SEQ_MS = 800;

// Ticket rows the command palette renders for one query. The rest are reachable
// by typing more, and the group hint says how many were left out.
const PALETTE_MAX_TICKETS = 6;

// Retries the palette makes to focus its input, one per frame. Alpine needs
// one frame, so the rest are slack for a dropped one.
const PALETTE_FOCUS_FRAMES = 10;

// Tape events the activity transcript renders at once, counted back from the
// newest. A whole 5000-event tape is 60,000 elements: 660ms to mount and 135ms
// to tear down, both on the main thread. 200 events cost 27ms and 9ms. The
// reader starts at the bottom, so the rest of the tape can wait for a click.
const TAPE_WINDOW_SIZE = 200;

// What a branch field says when no name can be shown: the ticket carries no
// branch and the daemon names one at pickup.
const BRANCH_PLACEHOLDER = 'daemon assigns branch when run starts';

// How often the activity tab re-reads a running stage's transcript. Fast enough
// that a tool call and its result land while the reader is still on the row,
// slow enough that a poll costs one stat on the daemon when nothing changed.
const ACTIVITY_POLL_MS = 2000;

// Rendered markdown, keyed by its source. Module scope rather than component
// state so Alpine's proxy never wraps it. The cap bounds the cache at roughly
// a megabyte of HTML, since ticket bodies run to tens of kilobytes.
var mdCache = new Map();
var MD_CACHE_MAX = 16;

// Entities one summary may chip. The patterns below are heuristics: a dotted
// lowercase run is an attribute path only most of the time, so the cap bounds a
// wrong guess as well as a long body. The widest summary in the ticket corpus
// matches nine.
var ENTITY_MAX = 40;
// File extensions a summary may name, as an allowlist. A bare \.\w+ tail would
// read github.com and every other domain as a file, and every method call as
// one too. The list covers what the ticket corpus writes; an extension missing
// from it falls through to the dotted-attribute pattern, which is how
// index_html.test.mjs used to render as an attribute path.
var ENTITY_EXT = 'go|ts|tsx|js|jsx|mjs|cjs|json|md|ya?ml|toml|lock|css|html?|lisp|asd|exs?|py|rb|rs|sh|sql|csv|log|txt|tmpl|proto';
// The shape of a ticket id, shared by the summary pass and the ticket-id-only
// pass authored prose gets. A match is checked against known tickets, never
// trusted from its shape alone.
var TICKET_ID_RE = '\\b[a-z]{2,8}-[a-z0-9]{4}\\b';
// Relation chips one rail row shows before it has to be expanded. A ticket in
// the corpus carries up to 33 links, and the rail is 308px wide.
var RELATION_CAP = 8;
// NodeFilter constants, spelled out rather than read off the global.
var SHOW_TEXT = 4;
var FILTER_ACCEPT = 1;
var FILTER_REJECT = 2;

function reEscape(s) {
  return String(s).replace(/[\\^$.*+?()[\]{}|]/g, '\\$&');
}

// Wall-clock seconds one history entry ran. Queue time sits between entries,
// so it never counts toward a stage.
function runSeconds(h) {
  if (!h || !h.started_at || !h.completed_at) return 0;
  var s = Math.floor((new Date(h.completed_at) - new Date(h.started_at)) / 1000);
  return s > 0 ? s : 0;
}

// Markdown source highlighting for the ticket editor. The result is painted
// under a transparent textarea, so it must reproduce the source character for
// character: only <span>s are added, and the whole line goes through the
// escaper before any markup does.
var MD_HL_SPECIAL = /[&<>]/g;
var MD_HL_ENTITY = { '&': '&amp;', '<': '&lt;', '>': '&gt;' };

function mdEscape(s) {
  return s.replace(MD_HL_SPECIAL, function (c) { return MD_HL_ENTITY[c]; });
}

function mdSpan(cls, text) {
  return '<span class="md-hl-' + cls + '">' + text + '</span>';
}

// Inline spans, in one left-to-right pass. Order matters: code comes first, so
// a `*` inside backticks is consumed as code and cannot also open emphasis.
var MD_HL_INLINE = /(`[^`\n]+`)|(\*\*[^\n]+?\*\*|__[^\n]+?__)|(\*[^*\n]+?\*)|(\[[^\]\n]*\]\([^)\n]*\))|(https?:\/\/[^\s)]+)/g;

function mdInline(raw) {
  return mdEscape(raw).replace(MD_HL_INLINE, function (m, code, strong, em, link, url) {
    if (code) return mdSpan('mark', '`') + mdSpan('code', code.slice(1, -1)) + mdSpan('mark', '`');
    if (strong) return mdSpan('mark', strong.slice(0, 2)) + mdSpan('strong', strong.slice(2, -2)) + mdSpan('mark', strong.slice(-2));
    if (em) return mdSpan('mark', '*') + mdSpan('em', em.slice(1, -1)) + mdSpan('mark', '*');
    if (link) {
      var cut = link.indexOf('](');
      return mdSpan('mark', '[') + mdSpan('link', link.slice(1, cut))
        + mdSpan('mark', '](') + mdSpan('url', link.slice(cut + 2, -1)) + mdSpan('mark', ')');
    }
    return mdSpan('url', url);
  });
}

var MD_HL_FENCE = /^\s*(```|~~~)/;
var MD_HL_RULE = /^\s*([-*_])(\s*\1){2,}\s*$/;
var MD_HL_HEADING = /^(\s*)(#{1,6} +)(.*)$/;
var MD_HL_QUOTE = /^(\s*>+ ?)(.*)$/;
var MD_HL_ITEM = /^(\s*)([-*+]|\d+[.)])( +)(.*)$/;
var MD_HL_TASK = /^(\[[ xX]\])( *)(.*)$/;
var MD_HL_ROW = /^\s*\|/;

function highlightMarkdown(src) {
  var lines = (src || '').split('\n');
  var out = [];
  var inFence = false;
  for (var i = 0; i < lines.length; i++) {
    var line = lines[i];
    var m;
    if (MD_HL_FENCE.test(line)) {
      inFence = !inFence;
      out.push(mdSpan('mark', mdEscape(line)));
    } else if (inFence) {
      out.push(mdSpan('code', mdEscape(line)));
    } else if (MD_HL_RULE.test(line)) {
      out.push(mdSpan('mark', mdEscape(line)));
    } else if ((m = MD_HL_HEADING.exec(line))) {
      // The heading text keeps one colour: nesting inline spans inside it would
      // repaint parts of it in the body palette.
      out.push(m[1] + mdSpan('mark', m[2]) + mdSpan('head', mdEscape(m[3])));
    } else if ((m = MD_HL_QUOTE.exec(line))) {
      out.push(mdSpan('mark', mdEscape(m[1])) + mdSpan('quote', mdInline(m[2])));
    } else if ((m = MD_HL_ITEM.exec(line))) {
      var rest = m[4];
      var task = MD_HL_TASK.exec(rest);
      var tail = task
        ? mdSpan(task[1][1] === ' ' ? 'mark' : 'done', task[1]) + task[2] + mdInline(task[3])
        : mdInline(rest);
      out.push(m[1] + mdSpan('mark', m[2]) + m[3] + tail);
    } else if (MD_HL_ROW.test(line)) {
      out.push(line.split('|').map(function (cell) { return mdInline(cell); }).join(mdSpan('mark', '|')));
    } else {
      out.push(mdInline(line));
    }
  }
  return out.join('\n');
}

// xterm handles live here, not on the Alpine component. Alpine wraps the whole
// x-data object in a deep reactive Proxy, so a Terminal stored there reads back
// proxied, and every internal property hop in xterm's parser, buffer, and
// renderer pays a trap; loadAddon hands the same proxy to the addons. Reach
// these through termState only: one hop through the component brings the proxy
// back. Module scope is safe because only one kontora() component exists.
var termState = {
  ws: null,
  term: null,
  fit: null,
  webgl: null,
  // Resolves once xterm's stylesheet is in the document.
  cssLoad: null,
  // Set once the vendored xterm modules have been imported.
  Terminal: null,
  FitAddon: null,
  Unicode11Addon: null,
  WebglAddon: null,
  inputDisposable: null,
  resizeObserver: null,
  resizeTimer: null,
  webglRetried: false,
};

function kontora() {
  // The Settings view lives in settings.js and is merged in here, so the
  // template sees one component. Its keys are distinct from everything below.
  return Object.assign({
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
    _searchRaf: null,
    // Reactive clock, advanced every 30s so relative durations ("Running for
    // 12m", card age timers) re-render without waiting for an SSE event.
    now: Date.now(),
    _nowTimer: null,
    selectedTicket: null,
    terminalOpen: false,
    terminalRW: false,
    activeTab: 'session',
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
    createForm: { title: '', path: '', pipeline: '', agent: '', status: 'todo', body: '', branch: '', base_branch: '' },
    createTouched: { pipeline: false, agent: false },
    // Paste-ready request the empty-board panel shows and copies. The brief
    // itself is printed by the daemon host's binary, so the UI hands over a
    // request rather than instructions of its own, which would drift from the
    // installed version.
    agentSetupRequest: 'Run `kontora setup --agent` on the Kontora daemon host and follow its instructions.',
    initModal: false,
    initSubmitting: false,
    initForm: { ticketId: '', title: '', pipeline: '', agent: '', path: '', branch: '', autoBranch: '', ticketPath: '' },
    actionLoading: null,
    deleteModal: false,
    detailMenuOpen: false,
    copiedId: false,
    copiedBranch: null,
    // Summary cards folded shut, keyed "stage#run". A stage can run more than
    // once, so the run is part of the key or two attempts collapse together.
    collapsedStages: {},
    copiedCmd: null,
    configCache: null,
    logViewContent: null,
    logViewStage: null,
    logViewLoading: false,
    // Structured transcript of one completed stage run: the /activity payload,
    // which stage and run it describes, and the tool rows opened by hand.
    activity: null,
    activityStage: null,
    activityRun: 0,
    activityLoading: false,
    activityError: null,
    expandedTools: {},
    // How many of the newest tape events the transcript renders. Grows one step
    // at a time through loadEarlierTapeEvents(); every activity load starts over
    // at one step.
    tapeWindow: TAPE_WINDOW_SIZE,
    // Refresh of a running stage's transcript: the pending timer, the validator
    // of the last payload, how many polls have failed in a row, and a counter
    // that tells the newest request from one already overtaken.
    _activityPoll: null,
    _activityETag: null,
    _activityFailures: 0,
    _activityLoadSeq: 0,
    noteDraft: '',
    noteSubmitting: false,
    detailLoading: false,
    // Commits and changed files for the open ticket's branch, from the changes
    // endpoint. Null until fetched; fetched for finished tickets only.
    ticketChanges: null,
    searchQuery: '',
    // Command palette (⌘K), desktop only. paletteGroups is the rendered model,
    // rebuilt by recomputePalette() at the mutation points — open, query, scope,
    // SSE flush — the same imperative-cache shape as _board.
    paletteOpen: false,
    paletteQuery: '',
    paletteSel: 0,
    // Ticket id the palette has drilled into, or null at the root.
    paletteScope: null,
    paletteGroups: [],
    _paletteRows: [],
    // Row id under the highlight. The selection follows the row rather than the
    // index, so an SSE re-rank does not move it under the user's finger.
    _paletteSelId: null,
    // Element that had focus when the palette opened, refocused on close.
    _paletteReturnFocus: null,
    // Last 3 opened ticket ids, most recent first.
    recentTickets: (function() {
      try {
        var v = JSON.parse(localStorage.getItem('kontora-recent-tickets'));
        return Array.isArray(v) ? v.slice(0, 3) : [];
      } catch (e) { return []; }
    })(),
    // Neutral confirmation for a palette command, unlike `error` (red, failures).
    toast: null,
    _toastTimer: null,
    // The only platform sniff in the app: the ⌘K keycap label.
    isMac: /Mac|iPhone|iPad/.test(navigator.platform || ''),
    _terminalSeq: 0,
    _terminalOpening: false,
    _eventSource: null,
    editing: false,
    editingBody: false,
    editForm: { body: '', pipeline: '', path: '', agent: '' },
    editSubmitting: false,
    editSaved: false,
    _editDebounce: null,
    // The pipeline and agent the edit form inherited from the project that owns
    // its current path. onEditPathChange compares against these values to keep
    // user-selected values.
    _editInherited: null,
    // The newline between the closing --- and the first line of the body, which
    // the file has and the source editor must not show as an empty first line.
    // Stripped when the form is filled and put back when it is saved, so a body
    // that came without it stays without it.
    _bodyLead: '',
    _blockOffsetsSrc: null,
    _blockOffsetsFor: null,
    setStageOpen: false,
    // Relation rows the user expanded past the first RELATION_CAP chips, keyed
    // by row. Cleared with the ticket, so opening another one starts collapsed.
    relExpanded: {},
    deleteSubmitting: false,
    uploadDragging: false,
    lightTheme: getStoredTheme() === 'light',
    sidebarHidden: (function() { try { return localStorage.getItem('kontora-sidebar-hidden') !== '0'; } catch (e) { return true; } })(),
    // Columns collapsed to a vertical rail. Cancelled starts collapsed unless
    // the user has saved their own set.
    collapsedCols: (function() {
      try {
        var v = localStorage.getItem('kontora-collapsed-cols');
        if (v === null) return ['cancelled'];
        var parsed = JSON.parse(v);
        return Array.isArray(parsed) ? parsed : ['cancelled'];
      } catch (e) { return ['cancelled']; }
    })(),
    // Card display toggles (column ⋯ menu): pipeline badge row and agent meta.
    showPipelineBadges: (function() { try { return localStorage.getItem('kontora-show-badges') !== '0'; } catch (e) { return true; } })(),
    showAgentMeta: (function() { try { return localStorage.getItem('kontora-show-agent-meta') !== '0'; } catch (e) { return true; } })(),
    colMenuOpen: null,
    currentView: 'board',
    // Set while applyRoute drives state from the hash, so the transitions it
    // calls do not write the hash back and stack up history entries.
    _applyingRoute: false,
    // Map of ticketId → true while a plannotator subprocess is in flight for it.
    plannotatorInFlight: {},
    // Board cards are rendered imperatively (not via Alpine), so the open card
    // menu and the per-column render state are plain (non-reactive) state.
    // _rendered[colKey] = { empty, ids, sigs }: the card ids in render order and
    // their signatures, so renderColumn can patch only the cards that changed.
    // _boardInit gates renderBoard until the column DOM exists (first paint).
    // Both maps are keyed by names that come from config (column keys) or from
    // ticket data (agent names), so they carry no prototype: a column called
    // "constructor" must not read a function where a record is expected.
    _openMenuId: null,
    // First key of a pending two-key shortcut, and when it was pressed.
    _keySeq: '',
    _keySeqAt: 0,
    _rendered: Object.create(null),
    _boardInit: false,
    // agent name -> running kontora ticket count, filled by recomputeBoard.
    _agentRunning: Object.create(null),

    _builtinColumns: [
      { key: 'open', statuses: ['open'], dropStatus: 'open', label: 'Open', color: 'bg-accent', tint: 'var(--st-open)', tip: 'Draft ticket, not running yet. Drag to In Progress or click Initialize to start.', emptyText: 'Create a ticket to get started',
        emptyIcon: '<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M9 15h6"/><path d="M12 18v-6"/>' },
      { key: 'in_progress', statuses: ['todo', 'in_progress', 'paused'], dropStatus: 'todo', label: 'In Progress', color: 'bg-ok', tint: 'var(--st-progress)', tip: 'Queued, running, or paused tickets. The daemon auto-promotes queued tickets when a worker is free.', emptyText: 'No active tickets',
        emptyIcon: '<path d="M12 8V4H8"/><rect width="16" height="12" x="4" y="8" rx="2"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/>' },
      { key: 'human_review', statuses: ['human_review'], dropStatus: 'human_review', label: 'Human Review', color: 'bg-review', tint: 'var(--st-review)', tip: 'Waiting for a human to look at the result.', emptyText: 'No tickets waiting for review',
        emptyIcon: '<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/>' },
      { key: 'done', statuses: ['done'], dropStatus: 'done', label: 'Done', color: 'bg-ok', tint: 'var(--st-done)', tip: 'Ticket completed successfully.', emptyText: 'No completed tickets yet',
        emptyIcon: '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>' },
      { key: 'cancelled', statuses: ['cancelled'], dropStatus: 'cancelled', label: 'Cancelled', color: 'bg-surface-600', tint: 'var(--st-cancel)', tip: 'Stopped manually. Drag to In Progress to run it again.', emptyText: 'No cancelled tickets',
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
      review: { label: 'Review', color: 'bg-review', tint: 'var(--st-review)', emptyIcon: '<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/>' },
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

    // Finish time of a ticket that waits in HUMAN REVIEW: the completed_at of
    // the last stage that ran. The stage that parks the ticket writes it, so it
    // stays put when the file is edited later. A manual "send to review" adds no
    // history entry, so fall back to the file mtime and then to creation.
    // Returns '' for any other status.
    reviewFinishedAt(ticket) {
      if (!ticket || ticket.status !== 'human_review') return '';
      var h = ticket.history;
      if (Array.isArray(h)) {
        for (var i = h.length - 1; i >= 0; i--) {
          if (h[i] && h[i].completed_at) return h[i].completed_at;
        }
      }
      return ticket.updated_at || ticket.created_at || '';
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

    // canAnnotateTicket reads the daemon's own answer. The rules (an initialized
    // ticket, an editable status, no annotation run already pending) live in the
    // daemon, so a button here can never offer a pass it refuses.
    canAnnotateTicket(ticket) {
      return !!ticket?.can_annotate;
    },

    historyLabel(h) {
      if (!h) return '';
      if (h.kind !== 'annotation') return h.stage;
      return h.stage + ' · annotation (' + (h.session_reused ? 'resumed' : 'fresh') + ')';
    },

    async openCreateModal() {
      this.createForm = { title: '', path: '', pipeline: '', agent: '', status: 'todo', body: '', branch: '', base_branch: '' };
      this.createTouched = { pipeline: false, agent: false };
      this.currentView = 'new';
      this.writeHash();
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
      this.writeHash();
    },

    // The project configured for a repository path. Both the configured form
    // (which may start with ~) and the resolved absolute form are compared,
    // since the browser cannot expand ~ itself.
    projectForPath(path) {
      var typed = (path || '').trim().replace(/\/+$/, '');
      if (!typed) return null;
      var projects = this.configCache?.projects || [];
      return projects.find(p =>
        typed === (p.path || '').replace(/\/+$/, '') ||
        typed === (p.resolved_path || '').replace(/\/+$/, '')
      ) || null;
    },

    // The project whose path matches what the create form's path field says.
    createProject() {
      return this.projectForPath(this.createForm.path);
    },

    // Prefill the selects the user has not touched from the project that owns
    // the typed path. cli.New applies the same defaults on submit, so this is a
    // preview of what the server will do, not the mechanism.
    //
    // Every untouched select is rewritten on each keystroke, including back to
    // blank once the path stops matching: leaving the previous project's values
    // in place would create the ticket with a pipeline picked for another repo.
    onCreatePathChange() {
      var project = this.createProject();
      if (!this.createTouched.pipeline) this.createForm.pipeline = (project && project.pipeline) || '';
      this.syncCreateAgent();
    },

    // The agent an untouched select shows: the one the project names, and
    // nothing else. Filling in the agent of the pipeline's first stage would
    // write that name onto the ticket, and a ticket agent overrides every
    // stage, so a two-stage pipeline would run stage two with the wrong agent.
    syncCreateAgent() {
      if (this.createTouched.agent) return;
      var project = this.createProject();
      this.createForm.agent = (project && project.agent) || '';
    },

    onPipelineChange() {
      this.createTouched.pipeline = true;
      this.syncCreateAgent();
    },

    toggleSidebar() {
      this.sidebarHidden = !this.sidebarHidden;
      try { localStorage.setItem('kontora-sidebar-hidden', this.sidebarHidden ? '1' : '0'); } catch (e) {}
    },

    isCollapsed(key) {
      return this.collapsedCols.includes(key);
    },

    toggleColumnCollapsed(key) {
      if (this.isCollapsed(key)) {
        this.collapsedCols = this.collapsedCols.filter(k => k !== key);
      } else {
        this.collapsedCols = this.collapsedCols.concat([key]);
      }
      try { localStorage.setItem('kontora-collapsed-cols', JSON.stringify(this.collapsedCols)); } catch (e) {}
      // Alpine recreates the column's card container on expand, so the cached
      // render state would make renderColumn skip filling the fresh (empty) element.
      delete this._rendered[key];
      this.$nextTick(() => this.renderColumn(key));
    },

    toggleShowBadges() {
      this.showPipelineBadges = !this.showPipelineBadges;
      try { localStorage.setItem('kontora-show-badges', this.showPipelineBadges ? '1' : '0'); } catch (e) {}
      this._rendered = Object.create(null);
      this.renderBoard();
    },

    toggleShowAgentMeta() {
      this.showAgentMeta = !this.showAgentMeta;
      try { localStorage.setItem('kontora-show-agent-meta', this.showAgentMeta ? '1' : '0'); } catch (e) {}
      this._rendered = Object.create(null);
      this.renderBoard();
    },

    // Number of tickets currently running on a given agent. Used by the sidebar,
    // once per agent row on every reactive read, so it reads the map that
    // recomputeBoard fills instead of filtering the whole ticket array.
    agentRunningCount(agent) {
      if (!agent) return 0;
      return this._agentRunning[agent] || 0;
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
      if (f.base_branch) lines.push('base_branch: ' + f.base_branch);
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
        // The selects carry the resolved values, so an empty one is a
        // deliberate opt-out. "none" says so; a blank field would make the
        // daemon inherit the project default the user just cleared.
        body.pipeline = this.createForm.pipeline || 'none';
        body.agent = this.createForm.agent || 'none';
        if (this.createForm.status) body.status = this.createForm.status;
        if (this.createForm.body) body.body = this.createForm.body;
        if (this.createForm.branch) body.branch = this.createForm.branch;
        if (this.createForm.base_branch) body.base_branch = this.createForm.base_branch;
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
        pipeline: '',
        agent: '',
        path: ticket.path || '',
        branch: ticket.branch || '',
        autoBranch: ticket.auto_branch || '',
        ticketPath: ticket.path || '',
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
      // The form shows what the ticket would run with, not a "project default"
      // placeholder: the fields the ticket leaves blank are filled from the
      // project that owns the path, the same values the daemon would resolve.
      this._initInherited = this.projectDefaultsFor(this.initForm.path);
      var pipeline = ticket.pipeline || this._initInherited.pipeline;
      var agent = ticket.agent || this._initInherited.agent;
      // Defer select values until after x-for has created <option> elements.
      await this.$nextTick();
      this.initForm.pipeline = pipeline;
      this.initForm.agent = agent;
    },

    // Re-apply the project defaults after the path field changes. A value the
    // user chose is kept; one that only got there by inheriting from the
    // previous path is replaced, so retargeting the ticket cannot start it with
    // another repository's pipeline.
    onInitPathChange() {
      var prev = this._initInherited || { pipeline: '', agent: '' };
      var next = this.projectDefaultsFor(this.initForm.path);

      if (!this.initForm.pipeline || this.initForm.pipeline === prev.pipeline) {
        this.initForm.pipeline = next.pipeline;
      }
      if (!this.initForm.agent || this.initForm.agent === prev.agent) {
        this.initForm.agent = next.agent;
      }
      this._initInherited = next;
    },

    closeInitModal() {
      this.initModal = false;
      this.initSubmitting = false;
    },

    // What an empty branch field shows: the name the daemon would assign, as
    // the server computed it, or a line saying it assigns one.
    branchPlaceholder(auto) {
      return auto || BRANCH_PLACEHOLDER;
    },

    // The init form's own placeholder. The server computed the name for the path
    // the ticket names, and a path the user retyped can resolve to a project
    // with another branch prefix or another naming mode. Rather than show a name
    // that would then be wrong, a diverged path falls back to the generic line.
    initBranchPlaceholder() {
      var trim = function(p) { return (p || '').trim().replace(/\/+$/, ''); };
      if (trim(this.initForm.path) !== trim(this.initForm.ticketPath)) return BRANCH_PLACEHOLDER;
      return this.branchPlaceholder(this.initForm.autoBranch);
    },

    async submitInitTicket() {
      if (!this.initForm.path) return;
      this.initSubmitting = true;
      this.error = null;
      try {
        const res = await fetch('/api/tickets/' + this.initForm.ticketId + '/init', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          // The selects carry the resolved values, so an empty one is a
          // deliberate opt-out. "none" says so; a blank field would make the
          // daemon inherit the project default the user just cleared.
          body: JSON.stringify({
            pipeline: this.initForm.pipeline || 'none',
            path: this.initForm.path,
            agent: this.initForm.agent || 'none',
            branch: (this.initForm.branch || '').trim() || undefined,
          }),
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
      if (h === '/settings') return { view: 'settings', ticketId: null };
      return { view: 'board', ticketId: null };
    },

    // Serialize the current view back to a hash. Inverse of parseHash.
    routeHash() {
      if (this.selectedTicket) return '#/t/' + encodeURIComponent(this.selectedTicket.id);
      if (this.currentView === 'new') return '#/new';
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
          if (t) { await this.selectTicket(t); return; }
          // The hash names a ticket this board does not have. Fall back to the
          // board rather than rendering an empty shell.
          if (this.selectedTicket) this.closeDetail();
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
      if (ticket && !ticket.kontora && ['todo', 'in_progress'].includes(newStatus)) {
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

    _escapeHtml(s) {
      if (s === null || s === undefined) return '';
      return String(s)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
    },

    // The relations behind a board card, as one line of ids. Board list payloads
    // carry the ids without titles, and an id is what fits on a card anyway.
    _cardRelationSummary(ticket) {
      var parts = [];
      var add = function (label, refs) {
        if (refs && refs.length) parts.push(label + ' ' + refs.map(function (r) { return r.id; }).join(', '));
      };
      add('under', ticket.parent ? [ticket.parent] : []);
      add('waits on', ticket.deps);
      add('related to', ticket.links);
      return parts.join(' \u00b7 ');
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

      var cls = ['kt-card group relative rounded-[9px] px-3 py-2.5 cursor-pointer border',
                 'bg-surface-900 border-edge-card hover:border-edge-hover hover:bg-surface-850',
                 'flex flex-col gap-1.5'];
      if (selected) cls.push('is-selected');
      if (!ticket.kontora) cls.push('border-dashed');
      if (ticket.status === 'cancelled') cls.push('opacity-60');
      if (ticket.status === 'in_progress') cls.push('card-state-running');
      if (ticket.status === 'paused') cls.push('card-state-paused');

      // Stage / status glyph (IN PROGRESS column only).
      var glyph = '';
      if (inProgressCol && ticket.status === 'in_progress') {
        glyph = '<span class="flex items-center gap-[5px] text-[10px] font-mono card-glyph-running">'
          + '<span class="w-[5px] h-[5px] rounded-full pulse-dot" style="background: hsl(var(--st-progress))"></span><span>' + esc(ticket.stage) + '</span></span>';
      } else if (inProgressCol && ticket.status === 'paused') {
        glyph = '<span class="flex items-center gap-1 text-[10px] font-mono card-glyph-paused"><span>⏸</span>'
          + (ticket.stage ? '<span>' + esc(ticket.stage) + '</span>' : '') + '</span>';
      } else if (inProgressCol && ticket.status === 'todo') {
        glyph = '<span class="flex items-center gap-1 text-[10px] font-mono text-surface-600"><span>◌</span>'
          + (ticket.stage ? '<span>' + esc(ticket.stage) + '</span>' : '') + '</span>';
      }

      var notKontoraBadge = (!ticket.kontora && ticket.status !== 'open')
        ? '<span class="px-1.5 py-px rounded-full border border-warn/20 bg-warn/10 text-warn text-[10px] font-mono shrink-0">not a kontora ticket</span>'
        : '';

      // Single-track progress bar: running tickets on multi-stage pipelines.
      // The current stage counts as half done — the stage index is the only
      // progress signal available.
      var progress = '';
      if (ticket.status === 'in_progress' && ticket.stages && ticket.stages.length > 1) {
        var ci = ticket.stages.indexOf(ticket.stage);
        if (ci >= 0) {
          var pct = Math.round(((ci + 0.5) / ticket.stages.length) * 100);
          progress = '<div class="card-progress" title="' + esc('stage ' + (ci + 1) + '/' + ticket.stages.length + ': ' + ticket.stage) + '"><div style="width:' + pct + '%"></div></div>';
        }
      }

      var agent = (this.showAgentMeta && ticket.agent)
        ? '<span class="flex items-center gap-1.5 min-w-0"><span class="text-edge-faint">·</span>'
          + '<span class="truncate">' + esc(ticket.agent) + '</span></span>'
        : '';

      var retry = (ticket.attempt > 0 && ticket.status !== 'done' && ticket.status !== 'cancelled')
        ? '<span class="px-1.5 py-px rounded text-[10px] bg-err/15 text-err">' + esc('retry ' + ticket.attempt) + '</span>'
        : '';
      // data-since / data-ago let _updateCardTimers patch the text in place on
      // the 30s tick without rebuilding the card.
      var timeSpan = '';
      var finishedAt = this.reviewFinishedAt(ticket);
      if (ticket.status === 'in_progress' && ticket.started_at) {
        timeSpan = '<span data-since="' + esc(ticket.started_at) + '" data-tip-t="' + esc('Started: ' + this.formatAbsDate(ticket.started_at)) + '">'
          + esc(this.formatDuration(ticket)) + '</span>';
      } else if (finishedAt) {
        timeSpan = '<span data-finished="' + esc(finishedAt) + '" data-tip-t="' + esc('Finished: ' + this.formatAbsDate(finishedAt)) + '">'
          + esc(this.finishedAgo(finishedAt)) + '</span>';
      } else if (ticket.created_at) {
        timeSpan = '<span data-ago="' + esc(ticket.created_at) + '" data-tip-t="' + esc(this.formatAbsDate(ticket.created_at)) + '">'
          + esc(this.timeAgo(ticket.created_at)) + '</span>';
      }

      // Badge row: pipeline badge (toggleable) + status glyph + import badge.
      // The kebab menu is absolutely positioned so the row can be omitted
      // entirely when it has no content.
      var badgeRow = '';
      var badgeParts = (this.showPipelineBadges
        ? '<span class="pipe-tag truncate">' + esc(this.ticketTagLabel(ticket)) + '</span>'
        : '') + notKontoraBadge + glyph;
      if (badgeParts) {
        badgeRow = '<div class="flex items-center gap-2 min-w-0 pr-5">' + badgeParts + '</div>';
      }

      // Only the kebab button ships with the card. The menu itself is built on
      // demand by _toggleCardMenu, which keeps ~9 nodes per card out of a board
      // that only ever shows one open menu.
      var menu = '<div class="absolute top-1.5 right-1.5 z-[2] flex items-center">'
        + '<button type="button" class="card-menu-btn w-6 h-6 rounded-md border border-surface-700/40 bg-surface-900/70 text-surface-600 hover:bg-surface-850 hover:text-tx-2 hover:border-edge-hover transition-colors flex items-center justify-center text-[13px] leading-none opacity-0 group-hover:opacity-100 focus:opacity-100" aria-haspopup="menu" aria-expanded="false" aria-label="More actions">'
        +   '⋯'
        + '</button>'
        + '</div>';

      // Colored mono [tag] prefix: taken from the title's own "[tag] ..."
      // prefix when present, otherwise the project basename.
      var pt = this.parseTitleTag(ticket);
      var tagSpan = pt.tag
        ? '<span class="title-tag" data-pipe-color="' + esc(this.pipelineColorByName(pt.tag)) + '">' + esc('[' + pt.tag + ']') + '</span> '
        : '';
      var titleCls = 'text-[13px] text-tx leading-[1.45]' + (ticket.status === 'cancelled' ? ' line-through decoration-surface-600/60' : '');

      // The card's id carries a hover card only when the frontmatter names a
      // relation: the title and the status are already on the card and in the
      // column, so a card repeating them would be noise.
      var rel = this._cardRelationSummary(ticket);
      var idTip = rel
        ? ' data-tip-e="' + esc(ticket.id) + '" data-tip-e-body="' + esc(rel) + '"'
        : '';

      return '<div class="' + cls.join(' ') + '"'
        + ' data-ticket-id="' + esc(ticket.id) + '"'
        + ' data-pipe-color="' + esc(this.ticketPipeColor(ticket)) + '"'
        + ' role="listitem" tabindex="0"'
        + ' aria-label="' + esc('Ticket ' + ticket.id + ': ' + (ticket.title || '')) + '">'
        + menu
        + badgeRow
        + '<p class="' + titleCls + '">' + tagSpan + esc(pt.rest) + '</p>'
        + progress
        + '<div class="flex items-center gap-2 text-[11px] font-mono text-surface-600 justify-between">'
        +   '<div class="flex items-center gap-1.5 min-w-0">'
        +     '<span class="group-hover:text-tx-3 transition-colors truncate"' + idTip + '>' + esc(ticket.id) + '</span>'
        +     agent
        +   '</div>'
        +   '<div class="flex items-center gap-2 shrink-0">' + retry + timeSpan + '</div>'
        + '</div>'
        + '</div>';
    },

    // Signature of everything _cardHTML renders except the relative time text,
    // which _updateCardTimers patches in place on the 30s tick. Leaving the clock
    // out is the point: it means a tick alone rebuilds no cards. Selection is a
    // class toggle (_markSelectedCard), so it stays out too. Keep this in sync
    // with _cardHTML — the "card signature covers every rendered field" test
    // guards each field.
    _cardSig(ticket, col) {
      // ticket.history and ticket.updated_at reach the card only through
      // reviewFinishedAt, so its result stands in for both here. ticket.parent,
      // ticket.deps and ticket.links reach it only through the relation line,
      // so that string stands in for the three of them.
      return [col.key, ticket.id, ticket.title, ticket.status, ticket.stage,
              ticket.pipeline, ticket.path, ticket.agent, ticket.attempt,
              ticket.kontora ? 1 : 0, ticket.started_at, ticket.created_at,
              this.reviewFinishedAt(ticket),
              (ticket.stages || []).join('>'),
              this._cardRelationSummary(ticket),
              this.showPipelineBadges ? 1 : 0, this.showAgentMeta ? 1 : 0].join('\u0001');
    },

    // The only place an HTML string becomes DOM. Tests replace it with a fake
    // node factory because their document stub cannot parse HTML.
    _nodeFromHTML(html) {
      var tpl = document.createElement('template');
      tpl.innerHTML = html;
      return tpl.content.firstElementChild;
    },

    _cardNode(ticket, col) {
      return this._nodeFromHTML(this._cardHTML(ticket, col));
    },

    // Markup for one card's action menu, built only while that menu is open:
    // Initialize (non-kontora) + valid moves + fallback. data-act carries the
    // endpoint ("init" for the init modal); data-status carries the target
    // status for move actions. The delegated handler matches .card-menu-item at
    // click time, so it needs no rebinding.
    _cardMenuHTML(ticket) {
      var esc = (s) => this._escapeHtml(s);
      var items = '';
      if (!ticket.kontora) {
        items += '<button type="button" class="card-menu-item w-full px-3 py-2 text-left text-[12px] font-mono text-warn hover:bg-surface-850 hover:text-warn transition-colors" data-act="init">Initialize</button>';
      }
      var moves = this.validMoves[ticket.status] || [];
      moves.forEach((mv) => {
        items += '<button type="button" class="card-menu-item w-full px-3 py-2 text-left text-[12px] font-mono text-tx-3 hover:bg-surface-850 hover:text-tx-2 transition-colors" data-act="'
          + esc(mv.endpoint) + '"' + (mv.status ? ' data-status="' + esc(mv.status) + '"' : '') + '>' + esc(mv.label) + '</button>';
      });
      if (!moves.length) {
        items += '<span class="block px-3 py-2 text-[12px] font-mono text-surface-600">No actions available</span>';
      }
      return '<div class="card-menu absolute right-0 top-7 min-w-[10rem] overflow-hidden rounded-lg border border-surface-700/60 bg-surface-900/95 shadow-lg shadow-black/30 z-20" role="menu">' + items + '</div>';
    },

    // Forget the cached render state for the column that holds a status, so the
    // next renderColumn rebuilds it from board data instead of skipping it as
    // unchanged. Needed whenever a card node moved without going through the
    // reconcile: the cache then describes a DOM that no longer exists.
    invalidateColumnFor(status) {
      var col = this.columns.find((c) => c.statuses.includes(status));
      if (col) delete this._rendered[col.key];
    },

    // Reconcile a single column's cards against the cached ids and signatures,
    // so an update touches only the cards that changed. Untouched columns keep
    // their scroll position, their open menu, and their DOM nodes.
    renderColumn(key) {
      var el = document.getElementById('col-' + key);
      if (!el) return;
      if (this.isCollapsed(key)) {
        // Collapsed rail: the container exists only as a drop target. Remove
        // any node Sortable dropped into it; moveTask re-buckets from data.
        if (el.firstChild) el.innerHTML = '';
        this._rendered[key] = { empty: true, ids: [], sigs: {} };
        return;
      }
      var col = this.columns.find((c) => c.key === key);
      if (!col) return;
      var list = this._board[key] || [];
      var prev = this._rendered[key];
      if (!list.length) {
        if (!prev || !prev.empty) el.innerHTML = this._emptyStateHTML(col);
        this._rendered[key] = { empty: true, ids: [], sigs: {} };
        return;
      }
      // Null-prototype so a ticket id like "constructor" cannot read a stale
      // signature off Object.prototype.
      var sigs = Object.create(null);
      var ids = [];
      for (var i = 0; i < list.length; i++) {
        ids.push(list[i].id);
        sigs[list[i].id] = this._cardSig(list[i], col);
      }
      if (prev && !prev.empty && this._sameCards(prev, ids, sigs)) return;
      this._patchColumn(el, list, col, prev, sigs);
      this._rendered[key] = { empty: false, ids: ids, sigs: sigs };
    },

    // True when the column renders the same cards, in the same order, with the
    // same content as the last render.
    _sameCards(prev, ids, sigs) {
      if (prev.ids.length !== ids.length) return false;
      for (var i = 0; i < ids.length; i++) {
        if (prev.ids[i] !== ids[i]) return false;
        if (prev.sigs[ids[i]] !== sigs[ids[i]]) return false;
      }
      return true;
    },

    // Keyed reconcile by ticket id: drop the cards that left, rebuild the ones
    // whose signature changed, and move the rest into the new order. A column
    // with no previous card state (first render, expand, empty state) is built
    // in one innerHTML write, which is cheaper than inserting node by node.
    _patchColumn(el, list, col, prev, sigs) {
      if (!prev || prev.empty) {
        el.innerHTML = list.map((t) => this._cardHTML(t, col)).join('');
        return;
      }
      var target = Object.create(null);
      list.forEach((t) => { target[t.id] = true; });
      var existing = Object.create(null);
      // Nodes Sortable dropped in, and cards whose ticket left the column, go
      // first so the ordering pass below sees only cards that belong here.
      Array.from(el.children).forEach(function (n) {
        var id = n.dataset && n.dataset.ticketId;
        if (!id || !target[id] || existing[id]) { n.remove(); return; }
        existing[id] = n;
      });
      for (var i = 0; i < list.length; i++) {
        var t = list[i];
        var node = existing[t.id];
        if (!node) {
          node = this._cardNode(t, col);
        } else if (prev.sigs[t.id] !== sigs[t.id]) {
          var fresh = this._cardNode(t, col);
          el.replaceChild(fresh, node);
          node = fresh;
        }
        var at = el.children[i];
        if (at !== node) el.insertBefore(node, at || null);
      }
    },

    // Render every current column. No-op until the column DOM exists (gated by
    // _boardInit, set in init's $nextTick).
    renderBoard() {
      if (!this._boardInit) return;
      this.columns.forEach((col) => this.renderColumn(col.key));
      this._dropStaleCardMenu();
    },

    // The menu node lives inside its card, so a card the reconcile removed or
    // rebuilt took the menu with it. Forget the open menu in that case, or the
    // next kebab click would toggle a menu that is no longer in the document.
    // A card the reconcile left alone keeps its menu open.
    _dropStaleCardMenu() {
      if (!this._openMenuId) return;
      if (!document.querySelector('#board-cols .card-menu')) this._openMenuId = null;
    },

    // Reset the render cache, render the cards, and bind the delegated handler
    // to the #board-cols that Alpine just mounted. Called once after the first
    // paint and again whenever the layout layer is rebuilt at the breakpoint,
    // because the previous board (and its listeners) went with the old layer.
    _mountBoard() {
      this._rendered = Object.create(null);
      this._boardInit = true;
      this.renderBoard();
      this._bindBoardEvents(document.getElementById('board-cols'));
    },

    // Crossing the 768px breakpoint destroys the layer that held the board and
    // the terminal container. This is the one path that disposes the terminal:
    // its container is going away, so the instance cannot be carried over.
    _onBreakpointChange() {
      // The palette has no phone layout, so a window narrowed while it is open
      // would leave it floating over the phone board.
      if (this.isMobile && this.paletteOpen) this.closePalette();
      this.closeTerminal();
      this._destroyTerminal();
      this.$nextTick(() => this._mountBoard());
    },

    // One delegated click/keydown handler for the whole board: menu toggle, menu
    // action, card select. Bound on #board-cols, a descendant of .board-scroll,
    // so stopPropagation here pre-empts the board background's closeDetail
    // handler. Re-bound per mounted board; the document-level listeners live in
    // _bindGlobalEvents so they are registered once.
    _bindBoardEvents(root) {
      var self = this;
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
    },

    // Close the menu on clicks outside the board, and on a window-level Escape.
    // Bound to document, so this runs once from init and survives a board remount.
    _bindGlobalEvents() {
      var self = this;
      // Capture phase on window: xterm reads keys from a hidden textarea and
      // forwards them to the agent, so a bubble listener never sees this one.
      // stopPropagation keeps the "k" out of the agent's stdin.
      //
      // One chord per platform, never both: on macOS Ctrl+K is readline's
      // kill-to-end-of-line, and swallowing it would break editing the agent's
      // command line in the terminal tab.
      window.addEventListener('keydown', function (e) {
        if (!(self.isMac ? e.metaKey : e.ctrlKey)) return;
        if ((e.key || '').toLowerCase() !== 'k') return;
        if (self.isMobile) return;
        e.preventDefault();
        e.stopPropagation();
        self.togglePalette();
      }, true);
      document.addEventListener('click', function (e) {
        if (!self._openMenuId) return;
        if (e.target.closest && e.target.closest('#board-cols')) return;
        self._closeCardMenu();
      });
      document.addEventListener('keydown', function (e) {
        if (e.key === 'Escape' && self._openMenuId) self._closeCardMenu();
        self._handleKeySequence(e);
      });
    },

    // Two-key sequences, vim style: "c" then "t" within KEY_SEQ_MS toggles the
    // theme. A key that starts no sequence, or arrives too late, resets the
    // buffer, so a stray "c" never leaves the next keystroke armed.
    _handleKeySequence(e) {
      if (e.ctrlKey || e.metaKey || e.altKey || this._isTypingTarget(e.target)) {
        this._keySeq = '';
        return;
      }
      var key = (e.key || '').toLowerCase();
      var now = Date.now();
      if (this._keySeq && now - this._keySeqAt > KEY_SEQ_MS) this._keySeq = '';
      if (this._keySeq === 'c' && key === 't') {
        this._keySeq = '';
        e.preventDefault();
        this.toggleTheme();
        return;
      }
      this._keySeq = key === 'c' ? 'c' : '';
      this._keySeqAt = now;
    },

    // True when the event target takes text input, so a bare shortcut key must
    // not swallow it. Includes the terminal: xterm reads keys from a hidden
    // textarea, and every keystroke there belongs to the agent.
    _isTypingTarget(el) {
      if (!el || !el.tagName) return false;
      var tag = el.tagName.toLowerCase();
      return tag === 'input' || tag === 'textarea' || tag === 'select' || el.isContentEditable === true;
    },

    // Build the clicked card's menu next to its kebab button. Closing the
    // previous menu removes its nodes, so at most one .card-menu exists.
    _toggleCardMenu(cardEl) {
      if (!cardEl) return;
      var id = cardEl.dataset.ticketId;
      if (this._openMenuId === id) { this._closeCardMenu(); return; }
      this._closeCardMenu();
      var ticket = this.tickets.find(function (t) { return t.id === id; });
      if (!ticket) return;
      var btn = cardEl.querySelector('.card-menu-btn');
      if (!btn) return;
      var menu = this._nodeFromHTML(this._cardMenuHTML(ticket));
      if (!menu) return;
      (btn.parentElement || cardEl).appendChild(menu);
      cardEl.classList.add('menu-open');
      btn.setAttribute('aria-expanded', 'true');
      this._openMenuId = id;
    },

    _closeCardMenu() {
      if (!this._openMenuId) return;
      document.querySelectorAll('#board-cols .kt-card.menu-open').forEach(function (el) {
        el.classList.remove('menu-open');
        var btn = el.querySelector('.card-menu-btn');
        if (btn) btn.setAttribute('aria-expanded', 'false');
      });
      document.querySelectorAll('#board-cols .card-menu').forEach(function (el) { el.remove(); });
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
      document.querySelectorAll('#board-cols [data-finished]').forEach(function (el) {
        el.textContent = self.finishedAgo(el.dataset.finished);
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
          // Sortable moved the card node behind the reconcile's back, so the
          // render cache for both columns is stale. Without dropping it, a
          // render that finds the same cards with the same signatures returns
          // early, and a moveTask that ends without a status change (an
          // uninitialized ticket opens the init modal instead) would leave the
          // card sitting in the column it was dropped on.
          self.invalidateColumnFor(fromDrop);
          self.invalidateColumnFor(toDrop);
          // No manual DOM restore: moveTask sets the status optimistically and
          // recomputeBoard → renderBoard rebuilds both columns from canonical
          // data, replacing the node Sortable moved (and reverting on failure).
          self.moveTask(ticketId, toDrop);
        }
      });
    },

    // xterm's stylesheet used to sit in <head>, where it blocked the first
    // paint of every board for a terminal most visits never open. It loads
    // here instead, alongside the xterm modules.
    _loadTerminalCSS() {
      if (termState.cssLoad) return termState.cssLoad;
      termState.cssLoad = new Promise(function (resolve, reject) {
        var link = document.createElement('link');
        link.rel = 'stylesheet';
        link.href = '/vendor/xterm@5.5.0/xterm.css';
        link.onload = resolve;
        link.onerror = reject;
        document.head.appendChild(link);
      });
      return termState.cssLoad;
    },

    async openTerminal() {
      if (!this.selectedTicket || this.terminalOpen) return;
      var seq = ++this._terminalSeq;
      var ticketId = this.selectedTicket.id;
      this.terminalOpen = true;
      this._terminalOpening = true;
      try {
        if (!termState.Terminal || !termState.FitAddon) {
          var [termMod, fitMod, unicodeMod, webglMod] = await Promise.all([
            import('/vendor/xterm@5.5.0/xterm.mjs'),
            import('/vendor/addon-fit@0.10.0/addon-fit.mjs'),
            import('/vendor/addon-unicode11@0.8.0/addon-unicode11.mjs'),
            // Optional: a failed load means the terminal falls back to the DOM renderer.
            import('/vendor/addon-webgl@0.18.0/addon-webgl.mjs').catch(function(e) {
              console.warn('webgl addon failed to load, using DOM renderer:', e);
              return null;
            }),
            // Last on purpose, after the four entries destructured above: it
            // resolves to nothing and only has to finish before the first paint.
            this._loadTerminalCSS(),
          ]);
          termState.Terminal = termMod.Terminal;
          termState.FitAddon = fitMod.FitAddon;
          termState.Unicode11Addon = unicodeMod.Unicode11Addon;
          termState.WebglAddon = webglMod ? webglMod.WebglAddon : null;
        }
        await this.$nextTick();
        if (!this.terminalOpen || this._terminalSeq !== seq) return;
        if (!this.terminalWanted() || this.selectedTicket?.id !== ticketId) {
          this.closeTerminal();
          return;
        }
        this._connectTerminal(seq);
      } catch (e) {
        console.error('terminal load error:', e);
        this.error = 'Failed to load terminal';
        // _connectTerminal may already have attached the resize observer.
        this.closeTerminal();
      } finally {
        if (this._terminalSeq === seq) this._terminalOpening = false;
      }
    },

    reconnectTerminal() {
      if (!this.selectedTicket || !this.terminalWanted() || this._terminalOpening) return;
      if (this.terminalOpen) this.closeTerminal();
      this.openTerminal();
    },

    _connectTerminal(seq) {
      if (this._terminalSeq !== seq || !this.terminalOpen) return;
      // On phone width the live terminal attaches into the mobile detail's own
      // container; on desktop into the panel container (fullscreen keeps the
      // same container — the panel just grows to fill the viewport).
      var container = document.getElementById(this.isMobile ? 'terminal-container-mobile' : 'terminal-session');
      // Returning with terminalOpen still set would strand the tab: switchTab
      // only refits a terminal it believes is open, so nothing would retry.
      if (!container) {
        this.closeTerminal();
        return;
      }

      // A terminal whose element sits somewhere else lost its layout layer when
      // the breakpoint changed, and cannot be reattached.
      if (termState.term && termState.term.element && termState.term.element.parentNode !== container) {
        this._destroyTerminal();
      }
      if (termState.term) {
        // The buffer still holds whatever the last stream wrote into it.
        termState.term.reset();
      } else {
        this._createTerminal(container);
      }

      var self = this;
      termState.resizeObserver = new ResizeObserver(function() {
        clearTimeout(termState.resizeTimer);
        termState.resizeTimer = setTimeout(function() { self.refitTerminal(); }, 100);
      });
      termState.resizeObserver.observe(container);

      requestAnimationFrame(function() {
        if (!termState.term || !self.terminalOpen || self._terminalSeq !== seq) return;
        termState.fit.fit();
        self._connectWs(seq);
      });
    },

    _createTerminal(container) {
      container.textContent = '';
      var fit = new termState.FitAddon();
      var term = new termState.Terminal({
        theme: this._getTerminalTheme(),
        fontSize: 13,
        fontFamily: "'JetBrains Mono', monospace",
        cursorBlink: this.terminalRW,
        disableStdin: false,
        scrollback: 5000,
        allowProposedApi: true,
      });
      term.loadAddon(fit);
      term.loadAddon(new termState.Unicode11Addon());
      term.unicode.activeVersion = '11';
      term.open(container);
      // Escape drops read-write mode instead of reaching the agent, and stops
      // there: the body Escape chain would otherwise see a read-only terminal
      // on the same keypress and close the ticket.
      var self = this;
      term.attachCustomKeyEventHandler(function(e) {
        if (e.type !== 'keydown' || e.key !== 'Escape' || !self.terminalRW) return true;
        e.preventDefault();
        e.stopPropagation();
        self.toggleTerminalRW();
        return false;
      });
      termState.term = term;
      termState.fit = fit;
      termState.webglRetried = false;
      this._loadWebgl();
    },

    // Must run after open(). Returns false when the DOM renderer stays active.
    _loadWebgl() {
      if (!termState.WebglAddon || !termState.term) return false;
      var self = this;
      try {
        var addon = new termState.WebglAddon();
        addon.onContextLoss(function() { self._onWebglContextLoss(addon); });
        termState.term.loadAddon(addon);
        termState.webgl = addon;
        return true;
      } catch (e) {
        console.warn('webgl renderer unavailable, using DOM renderer:', e);
        return false;
      }
    },

    // A lost context is recoverable, and the terminal outlives navigation, so
    // giving up on the first loss would pin the page to the DOM renderer for the
    // rest of its life. One retry only: retrying every loss would spin.
    _onWebglContextLoss(addon) {
      addon.dispose();
      if (termState.webgl !== addon) return;
      termState.webgl = null;
      if (termState.webglRetried || !this._loadWebgl()) {
        console.warn('webgl context lost, using DOM renderer');
        return;
      }
      termState.webglRetried = true;
    },

    _connectWs(seq) {
      if (termState.inputDisposable) {
        termState.inputDisposable.dispose();
        termState.inputDisposable = null;
      }
      if (this._terminalSeq !== seq || !termState.term) return;
      // Whoever asked for a stream gets exactly one. Without this, a read-write
      // toggle landing before _connectTerminal's requestAnimationFrame orphans
      // the socket it opened, and its kontora-view session outlives the page.
      if (termState.ws) {
        termState.ws.close();
        termState.ws = null;
      }
      var self = this;
      var term = termState.term;
      // A reused terminal keeps the cursor of whatever mode built it, and
      // terminalRW resets to false on ticket switch and detail close.
      term.options.cursorBlink = this.terminalRW;
      var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      var url = proto + '//' + location.host + '/ws/terminal/' + self.selectedTicket.id
        + '?cols=' + term.cols + '&rows=' + term.rows + (self.terminalRW ? '&rw=1' : '');
      var ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      termState.ws = ws;
      ws.onmessage = function(e) {
        if (self._terminalSeq !== seq) {
          ws.close();
          return;
        }
        term.write(new Uint8Array(e.data));
      };
      ws.onclose = function() { if (termState.ws === ws) termState.ws = null; };
      ws.onerror = function() { if (termState.ws === ws) termState.ws = null; };
      if (self.terminalRW) {
        termState.inputDisposable = term.onData(function(data) {
          if (self._terminalSeq === seq && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'input', data: data }));
          }
        });
      }
    },

    // Ends the stream and its listeners but keeps the terminal, so returning to
    // it costs no Terminal construction and no new WebGL context. _terminalSeq is
    // left alone: cancelling an in-flight openTerminal is the caller's call.
    _disconnectStream() {
      clearTimeout(termState.resizeTimer);
      termState.resizeTimer = null;
      if (termState.resizeObserver) {
        termState.resizeObserver.disconnect();
        termState.resizeObserver = null;
      }
      if (termState.inputDisposable) {
        termState.inputDisposable.dispose();
        termState.inputDisposable = null;
      }
      if (termState.ws) {
        termState.ws.close();
        termState.ws = null;
      }
    },

    // Browsers cap live WebGL contexts, so this runs only when the container
    // itself is going away: crossing the 768px breakpoint.
    _destroyTerminal() {
      this._disconnectStream();
      if (termState.term) {
        try { termState.term.dispose(); } catch (e) {}
      }
      termState.term = null;
      termState.fit = null;
      termState.webgl = null;
    },

    closeTerminal() {
      this._terminalSeq++;
      this._terminalOpening = false;
      this._disconnectStream();
      this.terminalOpen = false;
    },

    toggleTerminalRW() {
      this.terminalRW = !this.terminalRW;
      if (!this.terminalOpen || !termState.term) return;
      // Read-only is a flag on the tmux attach, so the mode change needs a fresh
      // stream. The terminal and its WebGL context stay.
      this._connectWs(this._terminalSeq);
    },

    refitTerminal() {
      if (!termState.term || !termState.fit || !this.terminalOpen) return;
      // A container hidden by x-show measures zero, and fitting to that would
      // reflow the whole scrollback twice: once down to nothing, once on return.
      var el = termState.term.element;
      if (!el || !el.clientWidth || !el.clientHeight) return;
      var oldCols = termState.term.cols;
      var oldRows = termState.term.rows;
      termState.fit.fit();
      if (termState.term.cols === oldCols && termState.term.rows === oldRows) return;
      if (termState.ws && termState.ws.readyState === WebSocket.OPEN) {
        termState.ws.send(JSON.stringify({ type: 'resize', cols: termState.term.cols, rows: termState.term.rows }));
      }
      // Clear viewport to remove reflow artifacts from cursor-positioned content.
      // tmux will redraw the screen after receiving the resize via SIGWINCH.
      termState.term.write('\x1b[2J\x1b[H');
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
      'widget-api':                 'indigo',
      'widget-api-sdk':             'cyan',
      'kontora':               'green',
      'acme-deploy':      'amber',
      'acme-assistant': 'rose',
      'acme-backend':    'amber',
      'acme-web':               'rose',
      'acm-0001':              'green',
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

    // Colored mono [tag] prefix for titles: a literal "[tag] ..." title prefix
    // wins; otherwise the project basename stands in (title left untouched).
    parseTitleTag(ticket) {
      var m = /^\[([^\]]+)\]\s*(.*)$/.exec(ticket.title || '');
      if (m) return { tag: m[1], rest: m[2] };
      var b = this.pathBasename(ticket.path);
      return { tag: b || null, rest: ticket.title || '' };
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
      // Per-agent running tally for the sidebar, filled in this same pass so
      // agentRunningCount doesn't filter the whole ticket array per agent row.
      var running = Object.create(null);
      for (var i = 0; i < this.tickets.length; i++) {
        var t = this.tickets[i];
        if (t.kontora && counts[t.status] !== undefined) counts[t.status]++;
        if (t.kontora && t.status === 'in_progress' && t.agent) {
          running[t.agent] = (running[t.agent] || 0) + 1;
        }
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
      this._agentRunning = running;
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

    // Re-filter on the next frame rather than after a fixed delay. Filtering
    // and reconciling the board costs a few milliseconds, so it fits in the
    // frame the keystroke already paints; the 150ms timer this replaces put
    // that much lag between a typed character and the board reacting to it.
    // Coalescing on the frame still collapses a fast burst of keystrokes into
    // one pass.
    debounceRecomputeBoard() {
      if (this._searchRaf !== null) return;
      this._searchRaf = requestAnimationFrame(() => {
        this._searchRaf = null;
        this.recomputeBoard();
      });
    },

    filteredTicketCount() {
      return this._boardTotal;
    },

    clearSearch() {
      this.searchQuery = '';
    },

    // ── Command palette (⌘K) ─────────────────────────────────────────────
    // Everything below derives from state already in memory (tickets,
    // selectedTicket, validMoves). The palette fetches nothing.

    // Tab targets inside a ticket scope. `tab` is the value switchTab() takes.
    // The session tab only exists while a stage runs; switchTab sends it to the
    // activity transcript on a ticket that is not running.
    _paletteTabs: [
      { tab: 'session', label: 'Open logs' },
      { tab: 'summary', label: 'Open summary' },
      { tab: 'ticket',  label: 'Open ticket body' },
    ],

    _paletteNav: [
      { id: 'nav-board',    view: 'board',    label: 'Go to board',    glyph: '▤' },
      { id: 'nav-new',      view: 'new',      label: 'New ticket',     glyph: '+' },
      { id: 'nav-settings', view: 'settings', label: 'Settings',       glyph: '⚙' },
      { id: 'nav-sidebar',  act: 'sidebar',   label: 'Toggle sidebar', glyph: '⌸' },
      { id: 'nav-theme',    act: 'theme',     label: 'Toggle theme',   glyph: '☀', meta: 'c t' },
    ],

    // Row glyph and hue per status: a mark for the three in-flight statuses, "#"
    // for the rest, each in that status's own colour.
    _paletteStatusMarks: {
      open:         { glyph: '#', cls: 'text-st-open' },
      todo:         { glyph: '◌', cls: 'text-surface-600' },
      in_progress:  { glyph: '●', cls: 'text-st-progress' },
      paused:       { glyph: '⏸', cls: 'text-st-paused' },
      human_review: { glyph: '#', cls: 'text-st-review' },
      done:         { glyph: '#', cls: 'text-st-done' },
      cancelled:    { glyph: '#', cls: 'text-st-cancel' },
    },

    // The palette's own status wording: running, not "in progress"; review, not
    // "human review". statusLabel() (detail header, cards) is left alone.
    _paletteStatusWords: { in_progress: 'running', human_review: 'review' },
    paletteStatusLabel(status) {
      return this._paletteStatusWords[status] || this.statusLabel(status);
    },

    // Past tense for the confirmation toast, keyed by validMoves label.
    _paletteVerbs: {
      'Queue': 'queued', 'Move to open': 'moved to open', 'Pause': 'paused',
      'Send to review': 'sent to review', 'Mark done': 'marked done',
      'Cancel': 'cancelled', 'Resume': 'resumed', 'Approve': 'approved',
      'Send back': 'sent back', 'Reopen': 'reopened', 'Initialize': 'initialized',
    },

    // validMoves for a ticket, with Initialize prepended for an unmanaged one.
    // moveTicketVia already routes a start of an unmanaged ticket to the init
    // modal, so Initialize is that same call under an explicit label.
    paletteMoves(t) {
      if (!t) return [];
      var moves = (this.validMoves[t.status] || []).slice();
      if (!t.kontora) moves.unshift({ label: 'Initialize', endpoint: 'run' });
      return moves;
    },

    // The board filter's fields plus stage and agent, which only the palette
    // searches. Extending ticketMatchesQuery would change the board filter too.
    _paletteMatches(t, q) {
      if (this.ticketMatchesQuery(t, q)) return true;
      return [t.stage, t.agent].some(f => f && f.toLowerCase().includes(q));
    },

    // Order the matches the way a multi-status board column is ordered:
    // in-flight first, then by activity. this.tickets is raw server order with
    // new tickets appended, so without this the six rendered rows would be six
    // arbitrary matches.
    _paletteRankStatuses: ['in_progress', 'paused', 'todo'],
    _paletteRank(list) {
      return this._sortColumn(list, this._paletteRankStatuses);
    },

    // Matches against the whole board, plus how many rows the list actually
    // holds when the rest were cut.
    _paletteTicketsHint(matched, shown) {
      var hint = matched + ' of ' + this.tickets.length;
      return shown < matched ? hint + ' · ' + shown + ' shown' : hint;
    },

    _paletteTicketRow(t) {
      var mark = this._paletteStatusMarks[t.status] || { glyph: '#', cls: 'text-surface-600' };
      var pt = this.parseTitleTag(t);
      return {
        id: 'ticket:' + t.id, kind: 'ticket', ticketId: t.id,
        move: null, tab: null, nav: null, fromStatus: t.status,
        glyph: mark.glyph, glyphClass: mark.cls,
        // Coloured from the tag itself, as the board card and the detail header
        // do. ticketPipeColor hashes the pipeline instead, so the same ticket
        // would carry two hues across the two places it appears.
        tag: pt.tag ? '[' + pt.tag + ']' : '', pipeColor: this.pipelineColorByName(pt.tag),
        title: pt.rest, sub: this._paletteTicketSub(t),
        status: t.status, statusText: this.paletteStatusLabel(t.status),
        meta: '→', metaClass: 'text-surface-600', danger: false,
      };
    },

    // Row line 2: id, stage position, agent, and whether kontora manages it.
    _paletteTicketSub(t) {
      var bits = [t.id];
      var stages = t.stages || [];
      var at = t.stage ? stages.indexOf(t.stage) : -1;
      if (at >= 0) bits.push(t.stage + ' ' + (at + 1) + '/' + stages.length);
      else if (stages.length) bits.push(stages.length + ' stages');
      if (t.agent) bits.push(t.agent);
      if (!t.kontora) bits.push('unmanaged');
      return bits.join('  ·  ');
    },

    // Command rows carry no project tag and no status pill: the title is the
    // whole row, and a destructive one is painted in the error colour.
    _paletteCmdRow(o) {
      return {
        id: o.id, kind: o.kind, ticketId: o.ticketId || null,
        move: o.move || null, tab: o.tab || null, nav: o.nav || null,
        fromStatus: o.fromStatus || '',
        glyph: o.glyph, glyphClass: o.danger ? 'text-err' : 'text-accent',
        tag: '', pipeColor: 'none',
        title: o.title, sub: '',
        status: '', statusText: '',
        meta: o.meta || '', metaClass: 'text-tx-faint', danger: !!o.danger,
      };
    },

    // A row id becomes an element id that aria-activedescendant points at, and
    // an IDREF may hold no whitespace, so a move label ("Send to review") has to
    // be slugged before it goes in one.
    _paletteSlug(s) {
      return String(s).toLowerCase().replace(/[^a-z0-9]+/g, '-');
    },

    _paletteActionRow(t, move) {
      return this._paletteCmdRow({
        id: 'action:' + t.id + ':' + this._paletteSlug(move.label), kind: 'action', ticketId: t.id,
        move: move, fromStatus: t.status, glyph: '▸', title: move.label,
        danger: move.label === 'Cancel',
      });
    },

    _paletteTabRow(t, tab) {
      return this._paletteCmdRow({
        id: 'tab:' + t.id + ':' + tab.tab, kind: 'tab', ticketId: t.id, tab: tab.tab,
        glyph: '❯', title: tab.label,
        meta: tab.tab === 'session' && t.status === 'in_progress' ? 'live' : '',
      });
    },

    _paletteDrillRow(t) {
      return this._paletteCmdRow({
        id: 'drill:' + t.id, kind: 'drill', ticketId: t.id, glyph: '▸',
        title: 'Actions for ' + t.id, meta: this.paletteMoves(t).length + ' →',
      });
    },

    _paletteNavRow(n) {
      return this._paletteCmdRow({
        id: n.id, kind: 'nav', nav: n, glyph: n.glyph, title: n.label, meta: n.meta || '',
      });
    },

    // Rebuild the palette model. Mirrors recomputeBoard: called at the mutation
    // points rather than derived on every reactive read, so reconciling the
    // highlight cannot loop inside a getter.
    recomputePalette() {
      if (!this.paletteOpen) { this.paletteGroups = []; this._paletteRows = []; return; }
      var q = (this.paletteQuery || '').trim().toLowerCase();
      var groups = [];
      var scoped = this.paletteScope ? this.tickets.find(t => t.id === this.paletteScope) : null;
      // The scoped ticket was deleted or archived while the palette was open.
      if (this.paletteScope && !scoped) this.paletteScope = null;

      if (scoped) {
        var legal = this.paletteMoves(scoped);
        var moves = legal.filter(m => !q || m.label.toLowerCase().includes(q));
        // A group with no surviving row is dropped, header and all: the root
        // branch below does the same, and a bare header above the no-match
        // message reads as a broken list.
        if (moves.length) {
          groups.push({
            label: 'Actions',
            // "legal" describes the ticket, so the count is of every legal move,
            // not of the ones the query left.
            hint: this.paletteStatusLabel(scoped.status) + ' · ' + legal.length + ' legal',
            items: moves.map(m => this._paletteActionRow(scoped, m)),
          });
        }
        var tabs = this._paletteTabs.filter(tb => !q || tb.label.toLowerCase().includes(q));
        if (tabs.length) {
          groups.push({ label: 'Open', hint: '', items: tabs.map(tb => this._paletteTabRow(scoped, tb)) });
        }
      } else {
        var open = this.selectedTicket ? this.tickets.find(t => t.id === this.selectedTicket.id) : null;
        if (open && !q) {
          var lead = [];
          if (this.paletteMoves(open).length) lead.push(this._paletteDrillRow(open));
          lead.push(this._paletteTabRow(open, this._paletteTabs[0]));
          groups.push({
            label: 'This ticket · ' + open.id,
            hint: this.parseTitleTag(open).rest,
            items: lead,
          });
        }
        // RECENT lists only ids still on the board, so a deleted ticket drops out.
        var ranked = q
          ? this._paletteRank(this.tickets.filter(t => this._paletteMatches(t, q)))
          : this.recentTickets.map(id => this.tickets.find(t => t.id === id)).filter(Boolean);
        if (ranked.length) {
          var shown = ranked.slice(0, PALETTE_MAX_TICKETS);
          groups.push({
            label: q ? 'Tickets' : 'Recent',
            hint: q ? this._paletteTicketsHint(ranked.length, shown.length) : '',
            items: shown.map(t => this._paletteTicketRow(t)),
          });
        }
        var nav = this._paletteNav.filter(n => !q || n.label.toLowerCase().includes(q));
        if (nav.length) groups.push({ label: 'Go to', hint: '', items: nav.map(n => this._paletteNavRow(n)) });
      }

      var rows = [];
      groups.forEach(g => g.items.forEach(r => rows.push(r)));
      this.paletteGroups = groups;
      this._paletteRows = rows;
      // Follow the row, not the index: an SSE re-rank must not move the
      // highlight under the user's finger.
      var at = this._paletteSelId ? rows.findIndex(r => r.id === this._paletteSelId) : -1;
      this.paletteSel = at >= 0 ? at : Math.min(this.paletteSel, Math.max(0, rows.length - 1));
      this._paletteSelId = rows.length ? rows[this.paletteSel].id : null;
    },

    togglePalette() {
      if (this.paletteOpen) { this.closePalette(); return; }
      this.openPalette();
    },

    openPalette() {
      // Below 768px the app runs its phone layout, which has its own sheets.
      if (this.isMobile) return;
      this._paletteReturnFocus = document.activeElement;
      this.paletteOpen = true;
      this._paletteReset();
      this._focusPaletteInput();
    },

    // Alpine defers an x-show reveal to the next animation frame, and focus on
    // a display:none element is a no-op that reports no error, so a single
    // attempt right after paletteOpen = true races the reveal and usually
    // loses. Retry per frame: the first attempt covers a panel already on
    // screen (drilling into a ticket scope), the loop covers the reveal.
    _focusPaletteInput() {
      var self = this;
      var tries = 0;
      var attempt = function () {
        // A palette closed again before this ran has already restored focus.
        if (!self.paletteOpen) return;
        var el = self.$refs && self.$refs.paletteInput;
        // offsetParent is null while the panel is still hidden.
        if (el && el.offsetParent) { el.focus(); return; }
        if (++tries > PALETTE_FOCUS_FRAMES) return;
        requestAnimationFrame(attempt);
      };
      attempt();
    },

    closePalette() {
      this.paletteOpen = false;
      this.paletteQuery = '';
      this.paletteScope = null;
      this.paletteGroups = [];
      this._paletteRows = [];
      this._paletteSelId = null;
      // Hiding the panel does not move focus off its input, and focus() on the
      // body below is a no-op while another element holds it. A hidden input
      // left focused is still a typing target, which kills every bare shortcut.
      var input = this.$refs && this.$refs.paletteInput;
      if (input) input.blur();
      var back = this._paletteReturnFocus;
      this._paletteReturnFocus = null;
      if (back && back.focus) back.focus();
    },

    // Pop one level: the ticket scope first, then the palette itself. The body
    // Escape stack calls this before its own chain. Backspace and ← reach it
    // only inside a scope, since at the root they are ordinary editing keys.
    palettePop() {
      if (!this.paletteScope) { this.closePalette(); return; }
      this.paletteScope = null;
      this._paletteReset();
    },

    palettePush(id) {
      this.paletteScope = id;
      this._paletteReset();
      this._focusPaletteInput();
    },

    _paletteReset() {
      this.paletteQuery = '';
      this.onPaletteQueryChanged();
    },

    // A new query is a new list, so the highlight starts at the top match.
    // recomputePalette instead follows the highlighted row across a rebuild,
    // which is right when SSE re-ranks the list under the user's finger and
    // wrong here: it would leave the highlight on a row the new query pushed
    // down, and Enter would run that row rather than the top match.
    onPaletteQueryChanged() {
      this.paletteSel = 0;
      this._paletteSelId = null;
      this.recomputePalette();
      this._scrollPaletteListTop();
    },

    paletteMove(delta) {
      var n = this._paletteRows.length;
      if (!n) return;
      this.paletteSel = ((this.paletteSel + delta) % n + n) % n;
      this._paletteSelId = this._paletteRows[this.paletteSel].id;
      this._scrollPaletteSelIntoView();
    },

    // The list scrolls inside a panel capped at the viewport height, so arrowing
    // past the fold would otherwise move the highlight out of sight and Enter
    // would run a row the user never saw.
    _scrollPaletteSelIntoView() {
      var el = this._paletteSelId && document.getElementById('palette-row-' + this._paletteSelId);
      if (el && el.scrollIntoView) el.scrollIntoView({ block: 'nearest' });
    },

    // A rebuilt list starts at row 0, and a leftover scroll offset would show it
    // part way down.
    _scrollPaletteListTop() {
      var list = document.getElementById('palette-list');
      if (list) list.scrollTop = 0;
    },

    // Takes a row id, not an index: the rows are rendered per group, and an id
    // keeps the highlight correct across a recompute that reordered them.
    paletteHover(id) {
      if (this._paletteSelId === id) return;
      var at = this._paletteRows.findIndex(r => r.id === id);
      if (at < 0) return;
      this.paletteSel = at;
      this._paletteSelId = id;
    },

    // → drills only from a ticket row, and only with the caret at the end of the
    // query, so it still moves the caret while typing. Tab is swallowed by the
    // template either way, which also traps focus in the dialog.
    paletteDrillFromKey(e) {
      var row = this._paletteRows[this.paletteSel];
      if (!row || row.kind !== 'ticket') return;
      if (e.key === 'ArrowRight') {
        var caret = e.target && typeof e.target.selectionStart === 'number' ? e.target.selectionStart : 0;
        if (caret < (this.paletteQuery || '').length) return;
        e.preventDefault();
      }
      this.palettePush(row.ticketId);
    },

    async paletteRun(row, withMeta) {
      if (!row) return;
      // ⌘Enter on a ticket drills instead of opening it.
      if (row.kind === 'drill' || (row.kind === 'ticket' && withMeta)) {
        this.palettePush(row.ticketId);
        return;
      }
      if (row.kind === 'action') { await this._paletteRunAction(row); return; }
      if (row.kind === 'nav') {
        var nav = row.nav;
        this.closePalette();
        if (nav.act === 'sidebar') { this.toggleSidebar(); return; }
        if (nav.act === 'theme') { this.toggleTheme(); return; }
        await this.gotoView(nav.view);
        return;
      }
      var t = this.tickets.find(x => x.id === row.ticketId);
      if (!t) return;
      this.closePalette();
      if (!await this._paletteOpenTicket(t)) return;
      if (row.kind === 'ticket') {
        this.showToast(t.status === 'in_progress' ? 'opened ' + t.id + ' · live logs' : 'opened ' + t.id);
        return;
      }
      // The log and summary tabs hide themselves when the ticket has nothing to
      // show, and activeTab pointing at a hidden tab leaves the panel blank.
      var tab = row.tab;
      if (tab === 'session' && !this.showTerminalTab()) tab = 'ticket';
      if (tab === 'summary' && !this.showSummaryTab()) tab = 'ticket';
      this.switchTab(tab);
      var target = this._paletteTabs.find(x => x.tab === tab);
      this.showToast(t.id + ' · ' + (target ? target.label : row.title).toLowerCase());
    },

    // selectTicket toggles the panel closed when handed the ticket already open,
    // so an already-open ticket only needs the board brought back in front.
    //
    // gotoView refuses to leave a dirty Settings form and opens its guard
    // instead, so the second check is not the first one repeated: selecting a
    // ticket the board never came back for attaches a live terminal to a hidden
    // container. Returns whether the ticket is on screen; the caller confirms
    // nothing when it is not.
    async _paletteOpenTicket(t) {
      if (this.currentView !== 'board') await this.gotoView('board');
      if (this.currentView !== 'board') return false;
      if (this.selectedTicket?.id === t.id) return true;
      await this.selectTicket(t);
      return true;
    },

    // The list may have re-derived over SSE since the row was built. A move that
    // is no longer in validMoves must not be sent.
    _paletteMoveStillLegal(row) {
      var t = this.tickets.find(x => x.id === row.ticketId);
      if (!t) return false;
      return this.paletteMoves(t).some(m => m.label === row.move.label);
    },

    async _paletteRunAction(row) {
      if (!this._paletteMoveStillLegal(row)) {
        this.closePalette();
        this.showToast(row.ticketId + ' is no longer ' + this.paletteStatusLabel(row.fromStatus));
        return;
      }
      var t = this.tickets.find(x => x.id === row.ticketId);
      var body = row.move.status ? { status: row.move.status } : null;
      // Starting an unmanaged ticket opens the init modal and posts nothing, so
      // there is no completed action to confirm.
      var opensInit = !t.kontora && this.ticketActionWouldStart(row.move.endpoint, body);
      this.closePalette();
      await this.moveTicketVia(t.id, row.move.endpoint, body);
      if (opensInit || this.error) return;
      var verb = this._paletteVerbs[row.move.label];
      this.showToast(t.id + ' ' + (typeof verb === 'string' ? verb : row.move.label.toLowerCase()));
    },

    palettePlaceholder() {
      return this.paletteScope ? 'Action for ' + this.paletteScope + '…' : 'Search tickets, or type a command…';
    },

    _paletteLegendRoot: [{ key: '↑↓', label: 'move' }, { key: '↵', label: 'open' },
                         { key: '→', label: 'actions' }, { key: 'esc', label: 'close' }],
    _paletteLegendScoped: [{ key: '↑↓', label: 'move' }, { key: '↵', label: 'run' },
                           { key: '⌫', label: 'back to search' }, { key: 'esc', label: 'close' }],
    paletteLegend() {
      return this.paletteScope ? this._paletteLegendScoped : this._paletteLegendRoot;
    },

    paletteScopePipeColor() {
      var t = this.paletteScope ? this.tickets.find(x => x.id === this.paletteScope) : null;
      return t ? this.ticketPipeColor(t) : 'none';
    },

    _pushRecentTicket(id) {
      if (!id) return;
      this.recentTickets = [id].concat(this.recentTickets.filter(x => x !== id)).slice(0, 3);
      try { localStorage.setItem('kontora-recent-tickets', JSON.stringify(this.recentTickets)); } catch (e) {}
    },

    showToast(msg) {
      this.toast = msg;
      clearTimeout(this._toastTimer);
      this._toastTimer = setTimeout(() => { this.toast = null; }, 2600);
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

    // Review-card label for a finish time: "finished 2h ago", "finished just now".
    finishedAgo(dateStr) {
      var ago = this.timeAgo(dateStr);
      if (!ago) return '';
      return ago === 'just now' ? 'finished just now' : 'finished ' + ago + ' ago';
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

    // Parsing and sanitising a ticket body costs 4-13ms at the sizes agents
    // write (30-50KB), and the same text is asked for more than once: the
    // editor preview and the read view render the same body, and stepping back
    // to a ticket renders it again. Keyed by the markdown itself, so a body
    // that changed is a miss and re-renders.
    renderMarkdown(md) {
      if (!md) return '';
      var hit = mdCache.get(md);
      if (hit !== undefined) return hit;
      var html;
      try { html = DOMPurify.sanitize(marked.parse(md)); } catch (e) { html = ''; }
      // Insertion-ordered, so the first key is the least recently added.
      if (mdCache.size >= MD_CACHE_MAX) mdCache.delete(mdCache.keys().next().value);
      mdCache.set(md, html);
      return html;
    },

    // x-html rewrites innerHTML on every effect run. An SSE refresh that leaves
    // the markdown untouched still tears the rendered body down and rebuilds
    // it, and the scroll container clamps to the top while it is empty. Write
    // only when the source changed. Plain innerHTML is enough here because
    // sanitized markdown carries no Alpine directives.
    setProse(el, md) {
      // The board size is in the key because a ticket id only chips once the
      // board holding that ticket has loaded, which is after the first paint.
      var src = (md || '') + '\u0000' + (this.tickets || []).length;
      if (el._proseSrc === src) return;
      el._proseSrc = src;
      el.innerHTML = this.renderMarkdown(md || '');
      this._markTicketIds(el);
    },

    // A note is plain text, not markdown: shown as the daemon or the agent
    // typed it. Ticket ids still become chips, so a note that answers "blocked
    // on kon-1234" points at that ticket.
    setNoteText(el, text) {
      var src = (text || '') + '\u0000' + (this.tickets || []).length;
      if (el._noteSrc === src) return;
      el._noteSrc = src;
      el.textContent = text || '';
      this._markTicketIds(el);
    },

    // The same write for stage summaries, with the full entity set on top. A
    // separate method rather than a flag on setProse: authored prose gets
    // ticket-id chips only, and chipping a sha or a filename inside a body
    // would rewrite what the reporter typed.
    //
    // The memo key adds the branch, the commit shas, the changed-file count and
    // the size of the board, all of which the chips read and none of which is
    // there on the first paint: fetchChanges resolves after it, and a ticket id
    // only chips once the board holding that ticket has loaded. A branch with
    // changed files but no commit yet has no sha to key on.
    setSummaryProse(el, md) {
      var shas = (this.ticketChanges?.commits || []).map(function (c) { return c.sha; }).join(',');
      var files = (this.ticketChanges?.files || []).length;
      var src = (md || '') + '\u0000' + (this.selectedTicket?.branch || '') + '\u0000' + shas
        + '\u0000' + files + '\u0000' + (this.tickets || []).length
        + '\u0000' + (this.ticketChanges?.remote || '');
      if (el._proseSrc === src) return;
      el._proseSrc = src;
      el.innerHTML = this.renderMarkdown(md || '');
      this._markEntities(el);
    },

    // Chip the ticket ids in already-sanitised prose and leave every other
    // pattern alone. This is the pass authored text gets: an id names a record
    // the reader can open, so it earns a chip, while a sha or a filename in a
    // body is only the words someone typed.
    _markTicketIds(root) {
      this._markEntities(root, this._ticketRe());
    },

    // Chip the entities in already-sanitised prose. One combined alternation,
    // so two patterns cannot claim overlapping text, and one pass.
    _markEntities(root, re) {
      re = re || this._entityRe();
      var walker = document.createTreeWalker(root, SHOW_TEXT, {
        acceptNode: function (node) {
          // Code and links own their text: a fence has to render character for
          // character, and a chip inside a link would eat the click.
          var p = node.parentElement;
          return p && p.closest('pre, code, a') ? FILTER_REJECT : FILTER_ACCEPT;
        },
      });
      // Collect before wrapping. A TreeWalker reads the live tree, and each
      // wrap replaces the node it is standing on.
      var targets = [];
      for (var n = walker.nextNode(); n; n = walker.nextNode()) targets.push(n);
      var budget = ENTITY_MAX;
      for (var i = 0; i < targets.length && budget > 0; i++) budget = this._wrapEntities(targets[i], re, budget);
    },

    // The ticket-id half of _entityRe on its own, for authored prose. Kept
    // beside the pattern it copies so the two shapes stay one shape.
    _ticketRe() {
      return new RegExp('(?<ticket>' + TICKET_ID_RE + ')', 'g');
    },

    _entityRe() {
      var parts = [];
      var branch = this.selectedTicket?.branch || '';
      // A branch name may hold a slash or a dash, so \b cannot bound it. Left
      // unbounded, a short name such as main chips the middle of domain and
      // maintenance, and takes those characters from the file pattern.
      if (branch) parts.push('(?<branch>(?<![\\w/-])' + reEscape(branch) + '(?![\\w/-]))');
      var shas = (this.ticketChanges?.commits || []).map(function (c) { return c.sha; }).filter(Boolean);
      // Only shas this branch produced. A bare \b[0-9a-f]{7,40}\b chips
      // deadbeef, feedface and every other hex-looking word in the prose. The
      // commit list holds short shas, so allow a longer form of one.
      if (shas.length) parts.push('(?<sha>\\b(?:' + shas.map(reEscape).join('|') + ')[0-9a-f]*\\b)');
      // A diff stat, either order: the added half green and the deleted half
      // red, so +750/-350 and -350/+750 read the same way.
      parts.push('(?<diff>(?<![\\w/])[+-]\\d+(?:,\\d{3})* ?/ ?[+-]\\d+(?:,\\d{3})*(?![\\w/]))');
      // The exit code decides whether the pipeline advanced, so it is coloured
      // by zero against everything else rather than left as prose.
      parts.push('(?<exit>\\bexits?(?:ed)?(?: code)? (?<code>\\d+)\\b)');
      // Leading directories, so internal/web/static/app.js chips as one path
      // rather than leaving internal/web/static/ beside a chip. Dotted stems
      // come with it, so contract.test.ts reads as a file: attr matches the
      // same run and, without them, claims it first.
      // The library names are prose, not files: a summary writes Alpine.js the
      // way it writes tmux, and the extension list cannot tell the two apart.
      parts.push('(?<file>(?<![\\w/.-])(?!(?:Alpine|Node|Next|Vue|React)\\.js\\b)/?(?:[\\w.-]+/)*[\\w-]+(?:\\.[\\w-]+)*\\.(?:' + ENTITY_EXT + ')\\b|\\bnode_modules\\b)');
      // An env var carries an underscore. Without that, the pattern claims
      // README, JSON, HTTP and every other shouted word: of 252 uppercase-word
      // matches across the ticket corpus, 24 were env vars.
      parts.push('(?<env>\\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\\b)');
      parts.push('(?<attr>\\b[a-z_]+(?:\\.[a-z_]+){2,}\\b)');
      // A ticket id is checked against the loaded board, not trusted from its
      // shape: of 164 words of this shape across the ticket corpus, 87 were
      // ordinary hyphenated words such as test-lisp and no-push.
      parts.push('(?<ticket>' + TICKET_ID_RE + ')');
      // A pull request or issue number. The link behind it needs the project's
      // origin, so this one is declined on a repository that has none.
      parts.push('(?<ref>(?<![\\w#])#\\d{1,7}\\b)');
      // One optional word between the number and the noun, for 239 node tests
      // and 22 modified files.
      parts.push('(?<count>\\b\\d+(?:,\\d{3})* (?:[a-z][\\w-]* )?(?:files?|insertions?|deletions?|tests?|cases?|checks?|assertions?|passed|failed|skipped)\\b)');
      return new RegExp(parts.join('|'), 'g');
    },

    // Split one text node around its matches. Returns what is left of the cap.
    _wrapEntities(node, re, budget) {
      var text = node.nodeValue;
      re.lastIndex = 0;
      var frag = null;
      var last = 0;
      var m;
      while (budget > 0 && (m = re.exec(text)) !== null) {
        // A pattern may decline the run it matched. Leaving last where it is
        // hands that text to the slice in front of the next chip, so the words
        // reach the fragment once, as prose.
        var chip = this._entityChip(m);
        if (!chip) continue;
        if (!frag) frag = document.createDocumentFragment();
        if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
        frag.appendChild(chip);
        last = m.index + m[0].length;
        budget--;
      }
      if (!frag) return budget;
      if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
      node.parentNode.replaceChild(frag, node);
      return budget;
    },

    _entityChip(m) {
      var g = m.groups;
      var text = m[0];
      if (g.diff) return this._diffChip(text);
      if (g.ref) return this._refChip(text);
      // A word shaped like a ticket id that names no ticket on the board is
      // prose, and declining it here is what keeps test-lisp a plain word.
      var t = g.ticket ? this._ticketById(text) : null;
      if (g.ticket && !t) return null;
      var span = document.createElement('span');
      span.textContent = text;
      if (t) return this._ticketChip(span, t);
      if (g.count) {
        span.className = 'ent-count';
        return span;
      }
      if (g.exit) {
        span.className = 'ent ent-' + (g.code === '0' ? 'ok' : 'bad');
        return span;
      }
      var kind = g.branch ? 'branch' : (g.sha ? 'sha' : (g.file ? 'file' : (g.env ? 'env' : 'attr')));
      span.className = 'ent ent-' + kind;
      // A sha, a branch and a file the branch touched have a record behind
      // them. An env var, a dotted attribute and a file this branch never
      // changed have none, so those are coloured and nothing more.
      var card = this._entityCard(kind, text);
      if (!card) return span;
      span.setAttribute('data-tip-e', text);
      span.setAttribute('data-tip-e-body', card);
      span.setAttribute('data-tip-e-hint', 'click to copy');
      var self = this;
      span.addEventListener('click', function () {
        self.copyBranch(text);
        span.classList.add('is-copied');
        setTimeout(function () { span.classList.remove('is-copied'); }, 1200);
      });
      return span;
    },

    // A chip needs a record behind the id. The board is the first place to
    // look; the open ticket's relations are the second, because the daemon
    // resolves those from every file on disk and so covers the tickets the
    // board hides (archived, or a status with no column).
    _ticketById(id) {
      var hit = (this.tickets || []).find(function (t) { return t.id === id; });
      return hit || this._relationRefById(id);
    },

    _relationRefById(id) {
      var rows = this.relationRows();
      for (var i = 0; i < rows.length; i++) {
        var hit = rows[i].refs.find(function (r) { return r.id === id && !!r.status; });
        if (hit) return hit;
      }
      return null;
    },

    // The frontmatter relations, in the order the rail lists them: the ticket
    // above this one, what it waits on, what waits on it, then the symmetric
    // set. Rows with nothing in them are dropped rather than shown empty.
    relationRows() {
      var t = this.selectedTicket;
      if (!t) return [];
      return [
        { key: 'parent', label: 'parent', refs: t.parent ? [t.parent] : [] },
        { key: 'deps', label: 'deps', refs: t.deps || [] },
        { key: 'blocks', label: 'blocks', refs: t.blocks || [] },
        { key: 'links', label: 'links', refs: t.links || [] },
      ].filter(function (r) { return r.refs.length > 0; });
    },

    // What one row shows: the first RELATION_CAP refs until the row is
    // expanded. A ticket can carry 30-odd links, and the rail is 308px wide.
    relationRefs(row) {
      if (this.relExpanded[row.key]) return row.refs;
      return row.refs.slice(0, RELATION_CAP);
    },

    relationHidden(row) {
      return this.relExpanded[row.key] ? 0 : Math.max(0, row.refs.length - RELATION_CAP);
    },

    // A ref the daemon could not resolve names a ticket that is no longer in
    // the tickets dir. It stays on screen, because the frontmatter still points
    // at it, but it is not a link.
    relationKnown(ref) {
      return !!(ref && ref.status);
    },

    relationChipClass(ref) {
      var mark = this._paletteStatusMarks[ref && ref.status];
      return 'ent ent-ticket ' + (mark ? mark.cls : 'text-surface-600')
        + (this.relationKnown(ref) ? '' : ' ent-ticket-gone');
    },

    // The hover card behind a ticket id: the title, which the id does not say,
    // the status word, and what a click does.
    ticketTip(ref) {
      var known = this.relationKnown(ref);
      return {
        title: (ref && (ref.title || ref.id)) || '',
        body: known ? this.paletteStatusLabel(ref.status) : 'not in the tickets dir',
        hint: known ? 'click to open' : '',
      };
    },

    // Open a ticket named by a relation. The board entry is preferred when there
    // is one, so the card behind the panel shows as selected; a ticket the board
    // hides is opened from the ref itself and the detail fetch fills it in.
    async openTicketRef(ref) {
      if (!this.relationKnown(ref)) return;
      var t = (this.tickets || []).find(function (x) { return x.id === ref.id; });
      await this._paletteOpenTicket(t || { id: ref.id, title: ref.title, status: ref.status });
    },

    // A pull request link, and the only chip that navigates. The path a number
    // sits under is the host's own convention (/pull on GitHub, /-/merge_requests
    // on GitLab), so anything but github.com is left as prose rather than
    // pointed at a URL that may not exist. #12 cannot say whether it is a pull
    // request or an issue; GitHub redirects /pull to the issue when it is one.
    _refChip(text) {
      var remote = this.ticketChanges?.remote || '';
      if (remote.indexOf('https://github.com/') !== 0) return null;
      var a = document.createElement('a');
      a.textContent = text;
      a.className = 'ent ent-ref';
      a.setAttribute('href', remote + '/pull/' + text.slice(1));
      a.setAttribute('target', '_blank');
      a.setAttribute('rel', 'noopener noreferrer');
      a.setAttribute('data-tip-e', text);
      a.setAttribute('data-tip-e-body', remote.slice('https://github.com/'.length));
      a.setAttribute('data-tip-e-hint', 'opens on GitHub');
      return a;
    },

    // A ticket chip wears the referenced ticket's own status colour, the same
    // hue the palette row and the board column use, so a summary that names a
    // done ticket reads apart from one that names a cancelled one. Clicking
    // opens that ticket rather than copying its id, and the card leads with the
    // title, which is the part the id does not say.
    _ticketChip(span, t) {
      var tip = this.ticketTip(t);
      span.className = this.relationChipClass(t);
      span.setAttribute('data-tip-e', tip.title);
      span.setAttribute('data-tip-e-body', tip.body);
      span.setAttribute('data-tip-e-hint', tip.hint);
      var self = this;
      span.addEventListener('click', function () { self.openTicketRef(t); });
      return span;
    },

    // One chip holding both halves of a diff stat, each in its own colour.
    _diffChip(text) {
      var span = document.createElement('span');
      span.className = 'ent ent-diff';
      var halves = text.split('/');
      for (var i = 0; i < halves.length; i++) {
        if (i) span.appendChild(document.createTextNode('/'));
        var half = document.createElement('span');
        half.className = halves[i].trim().charAt(0) === '-' ? 'ent-del' : 'ent-add';
        half.textContent = halves[i];
        span.appendChild(half);
      }
      return span;
    },

    _entityCard(kind, text) {
      if (kind === 'sha') {
        var hit = (this.ticketChanges?.commits || []).find(function (c) { return text.indexOf(c.sha) === 0; });
        return hit ? hit.subject : '';
      }
      if (kind === 'branch') {
        var base = this.ticketChanges?.base;
        return base ? 'Branched from ' + base : 'This ticket\u2019s branch';
      }
      if (kind === 'file') {
        // Summaries name a file by its basename as often as by its path, so
        // match either against the changed-file list.
        var f = (this.ticketChanges?.files || []).find(function (c) {
          return c.path === text || c.path.endsWith('/' + text);
        });
        return f ? '+' + f.added + '/-' + f.deleted + ' on this branch' : '';
      }
      return '';
    },

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

    // The newest run the transcript can show from history. Every history entry
    // describes a finished run, so the last one is the most recently completed
    // stage.
    latestCompletedRun() {
      var h = (this.selectedTicket && this.selectedTicket.history) || [];
      if (!h.length) return null;
      var last = h[h.length - 1];
      return { stage: last.stage, run: last.run || 0 };
    },

    // The run in flight, or null when nothing is running. The run number counts
    // that stage's history rows, as the daemon does: a history row is written
    // when a run ends, so the running one is the next index.
    runningRun() {
      var t = this.selectedTicket;
      if (!t || t.status !== 'in_progress' || !t.stage) return null;
      var run = 0;
      (t.history || []).forEach(function (h) { if (h.stage === t.stage) run++; });
      return { stage: t.stage, run: run };
    },

    // The run the activity tab shows: the one in flight, else the last finished.
    activityTarget() {
      return this.runningRun() || this.latestCompletedRun();
    },

    _resetActivity() {
      this._stopActivityPoll();
      this.activity = null;
      this.activityStage = null;
      this.activityRun = 0;
      this.activityLoading = false;
      this.activityError = null;
      this.expandedTools = {};
      this.tapeWindow = TAPE_WINDOW_SIZE;
    },

    _stopActivityPoll() {
      if (this._activityPoll !== null) clearTimeout(this._activityPoll);
      this._activityPoll = null;
      this._activityETag = null;
      this._activityFailures = 0;
    },

    // Load one run's transcript, replacing everything the pane holds.
    async fetchActivity(stage, run) {
      if (!this.selectedTicket) return;
      this._stopActivityPoll();
      this.activityStage = stage || '';
      this.activityRun = run || 0;
      this.activityLoading = true;
      this.activityError = null;
      this.activity = null;
      this.expandedTools = {};
      this.tapeWindow = TAPE_WINDOW_SIZE;
      await this._loadActivity(stage, run, { merge: false });
      this.activityLoading = false;
    },

    // Fetch one activity payload. merge=true is the polling path: only the
    // payload is replaced, so the expand map, the grown window and the scroll
    // offset all survive the tick.
    async _loadActivity(stage, run, { merge = false } = {}) {
      if (!this.selectedTicket) return;
      var id = this.selectedTicket.id;
      var seq = ++this._activityLoadSeq;
      var url = '/api/tickets/' + encodeURIComponent(id) + '/activity'
        + '?stage=' + encodeURIComponent(stage || '') + '&run=' + (run || 0);
      var headers = {};
      if (merge) {
        url += '&after=' + this.tapeEvents().length;
        if (this._activityETag) headers['If-None-Match'] = this._activityETag;
      }
      var data = null;
      var res = null;
      try {
        res = await fetch(url, { headers: headers });
        // A ribbon click or a ticket switch while this was in flight makes the
        // answer describe a run nobody is looking at.
        if (!this._activityCurrent(id, stage, run, seq)) return;
        if (res.status === 304) {
          this._activityFailures = 0;
          this._armActivityPoll(stage, run, true);
          return;
        }
        if (!res.ok) {
          var err = await res.json().catch(function () { return {}; });
          this._activityFailed(merge, err.error || 'Failed to load activity');
          this._armActivityPoll(stage, run, merge);
          return;
        }
        data = await res.json();
      } catch (e) {
        if (!this._activityCurrent(id, stage, run, seq)) return;
        this._activityFailed(merge, 'Failed to load activity');
        this._armActivityPoll(stage, run, merge);
        return;
      }
      if (!this._activityCurrent(id, stage, run, seq)) return;

      this._activityFailures = 0;
      this._activityETag = res.headers.get('ETag');
      if (merge) {
        this._mergeActivity(data);
      } else {
        this.activity = data;
        this.activityError = null;
      }
      this._armActivityPoll(stage, run, !!data.live);
    },

    // Whether a response still describes what the pane is showing. seq drops an
    // answer a later load has already superseded: the run ending starts a read
    // of the finished transcript while a poll is still in flight, and merging
    // that poll's partial tape afterwards would cut the transcript back down.
    _activityCurrent(id, stage, run, seq) {
      return seq === this._activityLoadSeq
        && !!this.selectedTicket && this.selectedTicket.id === id
        && this.activityStage === (stage || '') && this.activityRun === (run || 0);
    },

    // A poll that fails keeps the transcript on screen: a single dropped request
    // must not replace a good tape with an error. Three in a row is a daemon the
    // reader needs to know about.
    _activityFailed(merge, message) {
      if (!merge) {
        this.activityError = message;
        return;
      }
      this._activityFailures++;
      if (this._activityFailures >= 3) this.activityError = message;
    },

    // Splice the new suffix onto the events already held. The server sends its
    // own cursor, which can walk back over tool rows whose results arrived late.
    _mergeActivity(data) {
      // A run with no structured record tails the plaintext log, which carries
      // no cursor: the payload is whole every time.
      if (!data.tape) {
        this.activity = data;
        return;
      }
      var existing = this.tapeEvents();
      var offset = data.offset || 0;
      var events = existing.slice(0, offset).concat((data.tape && data.tape.events) || []);
      var added = events.length - existing.length;
      // A new object rather than a field write: the autoscroll effect watches
      // the activity property itself.
      this.activity = Object.assign({}, data, { tape: Object.assign({}, data.tape, { events: events }) });
      // With follow off the reader is holding a position, so the window grows by
      // what arrived and no row slides off the top. With follow on the window
      // stays put and the effect pins the view to the newest event.
      if (!this.logFollow && added > 0) this.tapeWindow += added;
    },

    // Re-arm the two-second poll while the run is live and its pane is visible.
    // A chained timeout, not an interval: it cannot stack requests on a slow
    // daemon. Once the run ends the payload stops saying live, nothing is armed,
    // and the completed transcript is what stays on screen.
    _armActivityPoll(stage, run, live) {
      if (this._activityPoll !== null) {
        clearTimeout(this._activityPoll);
        this._activityPoll = null;
      }
      if (!live || this.activeTab !== 'activity' || !this.selectedTicket) return;
      var self = this;
      this._activityPoll = setTimeout(function () {
        self._activityPoll = null;
        self._loadActivity(stage, run, { merge: true });
      }, ACTIVITY_POLL_MS);
    },

    // Show the transcript for one run and bring the activity tab forward.
    openActivity(stage, run) {
      this.activeTab = 'activity';
      this.fetchActivity(stage, run);
    },

    // Whether the agent's session format leaves this dimension unverified. The
    // view hides the affordance rather than showing a zero or the wrong colour.
    tapePartial(dim) {
      var p = this.activity && this.activity.tape && this.activity.tape.partial;
      return !!p && p.indexOf(dim) >= 0;
    },

    tapeEvents() {
      return (this.activity && this.activity.tape && this.activity.tape.events) || [];
    },

    // The rendered tail of the tape: the newest tapeWindow events, each paired
    // with its position in the full events array. Row identity (toolKey) and the
    // x-for key both read that position, so loading earlier events prepends rows
    // without renaming the ones already on screen.
    visibleTapeEvents() {
      var events = this.tapeEvents();
      var start = Math.max(0, events.length - this.tapeWindow);
      var out = [];
      for (var i = start; i < events.length; i++) out.push({ ev: events[i], idx: i });
      return out;
    },

    hiddenTapeEventCount() {
      return Math.max(0, this.tapeEvents().length - this.tapeWindow);
    },

    // Grow the window by one step. The rows appear above the viewport, so the
    // scroll offset moves down by the height they add; without that the reader
    // is thrown back to older events.
    loadEarlierTapeEvents() {
      var el = document.getElementById('activity-scroll');
      var height = el ? el.scrollHeight : 0;
      var top = el ? el.scrollTop : 0;
      this.tapeWindow = Math.min(this.tapeEvents().length, this.tapeWindow + TAPE_WINDOW_SIZE);
      this.$nextTick(function () {
        if (el) el.scrollTop = top + (el.scrollHeight - height);
      });
    },

    // Row identity for the expand map. Pi tool calls carry no id, so the index
    // stands in.
    toolKey(ev, i) {
      return ev.id || ('i' + i);
    },

    // A failure is expanded on first render: it is why the reader opened the
    // tape. Everything else starts collapsed.
    toolExpanded(ev, i) {
      if (ev.is_error && !this.tapePartial('is_error')) return true;
      return !!this.expandedTools[this.toolKey(ev, i)];
    },

    toolFailed(ev) {
      return !!ev.is_error && !this.tapePartial('is_error');
    },

    toggleTool(ev, i) {
      var k = this.toolKey(ev, i);
      this.expandedTools[k] = !this.expandedTools[k];
    },

    eventTime(ev) {
      if (this.tapePartial('time') || !ev || !ev.time) return '';
      var d = new Date(ev.time);
      if (isNaN(d.getTime())) return '';
      var pad = function (n) { return String(n).padStart(2, '0'); };
      return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
    },

    // Total tokens over the four categories the session record reports, or ''
    // when usage is unverified for this agent. There is no monetary figure:
    // neither the session record nor the config carries a price.
    tapeTokens(tape) {
      if (!tape || (tape.partial || []).indexOf('usage') >= 0) return '';
      var t = tape.totals || {};
      var n = (t.input || 0) + (t.output || 0) + (t.cache_create || 0) + (t.cache_read || 0);
      if (!n) return '';
      if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
      if (n >= 1000) return Math.round(n / 1000) + 'k';
      return String(n);
    },

    // ---- stage ribbon ------------------------------------------------------

    // One segment per pipeline stage, sized by the summed duration of its runs.
    // Gaps between runs are queue time and do not count toward a stage.
    stageRibbon() {
      var t = this.selectedTicket;
      if (!t) return [];
      var self = this;
      var agg = Object.create(null);
      (t.history || []).forEach(function (h) {
        var e = agg[h.stage] || (agg[h.stage] = { seconds: 0, runs: 0, last: null });
        e.runs++;
        e.last = h;
        e.seconds += runSeconds(h);
      });

      var stages = t.stages || [];
      var currentIdx = stages.indexOf(t.stage);
      return stages.map(function (name, i) {
        var a = agg[name] || { seconds: 0, runs: 0, last: null };
        var running = t.status === 'in_progress' && name === t.stage;
        var done = !running && (t.status === 'done' || a.runs > 0 || (currentIdx >= 0 && i < currentIdx));
        var seconds = a.seconds;
        if (running && t.started_at) {
          var live = Math.floor((self.now - new Date(t.started_at)) / 1000);
          if (live > 0) seconds += live;
        }
        return {
          name: name,
          runs: a.runs,
          run: a.last ? (a.last.run || 0) : 0,
          seconds: seconds,
          state: running ? 'running' : (done ? 'done' : 'queued'),
          meta: running || done ? self.formatSeconds(seconds) : 'not started',
        };
      });
    },

    // "45s" / "2m 04s" / "1h 12m" — the ribbon's per-stage meta line.
    formatSeconds(secs) {
      if (typeof secs !== 'number' || isNaN(secs) || secs < 0) return '';
      if (secs < 60) return secs + 's';
      var m = Math.floor(secs / 60);
      if (m < 60) return m + 'm ' + String(secs % 60).padStart(2, '0') + 's';
      return Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
    },

    // Clicking a segment: the running stage returns to the live session, a
    // finished one loads its transcript, a queued one does nothing.
    clickRibbon(seg) {
      if (seg.state === 'running') {
        this.switchTab('session');
        return;
      }
      if (seg.state !== 'done' || !seg.runs) return;
      this.openActivity(seg.name, seg.run);
    },

    // Attempt denominator: the initial attempt plus the stage's max_retries.
    stageMaxAttempts() {
      var t = this.selectedTicket;
      if (!t || !t.pipeline || !t.stage) return 0;
      var infos = (this.configCache && this.configCache.pipeline_infos) || [];
      for (var i = 0; i < infos.length; i++) {
        if (infos[i].name !== t.pipeline) continue;
        var idx = (infos[i].stages || []).indexOf(t.stage);
        if (idx < 0) return 0;
        return ((infos[i].max_retries || [])[idx] || 0) + 1;
      }
      return 0;
    },

    // Wall time across the whole ticket: the first stage's pickup to the last
    // recorded exit, queue gaps included. The frontmatter's started_at is
    // rewritten at every stage spawn, so it holds the current stage's pickup
    // and would report only the last stage's duration.
    ticketWall() {
      var t = this.selectedTicket;
      if (!t) return '';
      var h = t.history || [];
      var start = h.length ? (h[0].started_at || t.started_at) : t.started_at;
      var end = h.length ? h[h.length - 1].completed_at : t.updated_at;
      return this.formatElapsed(start, end);
    },

    // Meters beside the ribbon. A meter with no verified data is left out
    // rather than rendered as a zero.
    ribbonMeters() {
      var t = this.selectedTicket;
      if (!t) return [];
      var out = [];
      var tokens = this.tapeTokens(this.activity && this.activity.tape);
      if (t.status === 'in_progress') {
        if (t.started_at) out.push({ k: 'elapsed', v: this.formatDuration(t) });
        if (tokens) out.push({ k: 'tokens', v: tokens });
        var max = this.stageMaxAttempts();
        var n = (t.attempt || 0) + 1;
        out.push({ k: 'attempt', v: max ? n + ' / ' + max : String(n) });
      } else if (t.status === 'human_review') {
        var wall = this.ticketWall();
        if (wall) out.push({ k: 'wall', v: wall });
        if (tokens) out.push({ k: 'tokens', v: tokens });
      }
      return out;
    },

    // ---- changed files -----------------------------------------------------

    // Bounded churn summary for the rail: the totals, the stacked bar's split,
    // and the three files with the most change. Equal churn breaks on path so
    // repeated renders agree on the same three.
    churn() {
      var files = (this.ticketChanges && this.ticketChanges.files) || [];
      var added = 0;
      var deleted = 0;
      files.forEach(function (f) { added += f.added || 0; deleted += f.deleted || 0; });
      var sorted = files.slice().sort(function (a, b) {
        var d = ((b.added || 0) + (b.deleted || 0)) - ((a.added || 0) + (a.deleted || 0));
        if (d !== 0) return d;
        return a.path < b.path ? -1 : (a.path > b.path ? 1 : 0);
      });
      var total = added + deleted;
      return {
        count: files.length,
        added: added,
        deleted: deleted,
        addedPct: total ? (added / total) * 100 : 0,
        top: sorted.slice(0, 3),
        more: Math.max(0, files.length - 3),
      };
    },

    // ---- notes -------------------------------------------------------------

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

    toggleTheme() {
      this.lightTheme = !this.lightTheme;
      var t = this.lightTheme ? 'light' : 'dark';
      applyTheme(t);
      setStoredTheme(t);
      if (termState.term) this._applyTerminalTheme();
      this.updateFavicon();
    },

    _getTerminalTheme() {
      var s = getComputedStyle(document.documentElement);
      return { background: this._cssVar('--surface-deep', s), foreground: this._cssVar('--tx', s), cursor: this._cssVar('--accent', s), selectionBackground: this._cssVar('--surface-700', s) };
    },

    _applyTerminalTheme() {
      if (!termState.term) return;
      termState.term.options.theme = this._getTerminalTheme();
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
      // The board card lacks detail fields, so the summary check runs on the
      // full ticket selectTicket fetched.
      if (this.detailTab === 'logs' && this.summaryFirst(this.selectedTicket)) {
        this.detailTab = 'ticket';
      }
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
        if (this.selectedTicket.status === 'in_progress') this.openTerminal();
      } else {
        if (this.terminalOpen) this.closeTerminal();
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
      this.createForm = { title: '', path: '', pipeline: '', agent: '', status: 'todo', body: '', branch: '', base_branch: '' };
      this.createTouched = { pipeline: false, agent: false };
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
  }, kontoraSettings());
}
