import { TAPE_WINDOW_SIZE } from './activity.js';
import { newCreateForm } from './create.js';
import { termState } from './terminal.js';

// Component core: the state every other mixin reads, the app-wide bits
// (auth, routing, toast, theme) and the date formatting the templates call.
export function kontoraApp() {
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
    // needsInput is camelCase because a status is [a-z][a-z0-9_]* and so
    // cannot collide with it.
    _statusCounts: { in_progress: 0, paused: 0, todo: 0, done: 0, needsInput: 0 },
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
    createForm: newCreateForm(),
    createTouched: { pipeline: false, agent: false },
    // Paste-ready request the empty-board panel shows and copies. The brief
    // itself is printed by the daemon host's binary, so the UI hands over a
    // request rather than instructions of its own, which would drift from the
    // installed version.
    agentSetupRequest: 'Run `kontora setup --agent` on the Kontora daemon host and follow its instructions.',
    initModal: false,
    initSubmitting: false,
    // The init modal's own error line. Separate from `error`, which drives the
    // global toast: the modal stays open across a failure, so it must not show
    // an unrelated earlier failure and must not lose its own to the toast's
    // 10s timer.
    initError: null,
    initForm: { ticketId: '', status: '', tag: '', titleRest: '', pipeline: '', agent: '', path: '', branch: '', autoBranch: '', ticketPath: '' },
    // Pipeline and agent the path's project supplies, tracked so a value that
    // only got there by inheritance can be told from one the user chose.
    // Declared here rather than assigned on first use because the modal reads
    // it while rendering, so it has to be reactive from the start.
    _initInherited: { pipeline: '', agent: '' },
    actionLoading: null,
    // The detail panel's schedule editor. scheduleDraft is a local wall time,
    // the value a datetime-local input holds; the API is sent the instant it
    // means. scheduleError is the editor's own line, kept out of `error` so the
    // global toast's timer cannot take it away mid-edit.
    scheduleEditing: false,
    scheduleDraft: '',
    scheduleSubmitting: false,
    scheduleError: null,
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
    // The dependency chain behind the ladder, fetched per ticket and cleared
    // with it.
    chain: null,
    chainCollapsed: false,
    chainError: null,
    // Bumped per chain fetch, so a response from an older one is dropped.
    _chainSeq: 0,
    // Sub-ticket tree folded shut, and the tree expanded past CHILDREN_CAP.
    // Both are cleared with the ticket, like relExpanded.
    childrenCollapsed: false,
    childrenExpanded: false,
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
    // Column keys a drag left out of sync with the board data, so the next
    // render patches them instead of skipping them as unchanged.
    _dirtyCols: Object.create(null),
    // Set while Sortable owns the card nodes. _renderHeld records that a render
    // was asked for during the drag, so onEnd can run it.
    _dragging: false,
    _renderHeld: false,
    _boardInit: false,
    // agent name -> running kontora ticket count, filled by recomputeBoard.
    _agentRunning: Object.create(null),
    // Memo behind _projectIndex(), with the projects array it was built from.
    _projectByPath: null,
    _projectIndexSrc: null,
    // Memo behind parseFilterQuery(), with the string it was parsed from.
    _parsedQuery: null,
    _parsedQuerySrc: null,

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
    // detail panel (Pause for in_progress, retry/restart for the parked
    // statuses), so the validMoves list rendered there doesn't duplicate them.
    _detailCoveredMoves: {
      in_progress: ['pause'],
      paused: ['retry'],
      done: ['retry'],
      cancelled: ['retry'],
    },

    // validMoves entries shown as action buttons in the detail panel sidebar.
    detailMoves(ticket) {
      if (!ticket) return [];
      var covered = this._detailCoveredMoves[ticket.status] || [];
      return (this.validMoves[ticket.status] || []).filter(mv => !covered.includes(mv.endpoint));
    },

    _knownCustomStatuses: {
      review: { label: 'Review', color: 'bg-review', tint: 'var(--st-review)', emptyIcon: '<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/>' },
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

    // How long a ticket's agent has been blocked on a question. Same shape as
    // formatDuration so the 30s tick patches it in place through data-since.
    waitingFor(ticket) {
      if (!ticket || !ticket.waiting_for_input) return '';
      return this.formatDuration({ started_at: ticket.waiting_since });
    },

    // The badge's hover text: the tool that blocked and the question it asked.
    // A tool whose arguments the extension could not read leaves the name alone.
    waitingLabel(ticket) {
      if (!ticket || !ticket.waiting_for_input) return '';
      var tool = ticket.waiting_tool || 'a question';
      return ticket.waiting_question ? tool + ': ' + ticket.waiting_question : tool;
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

    toggleTheme() {
      this.lightTheme = !this.lightTheme;
      var t = this.lightTheme ? 'light' : 'dark';
      applyTheme(t);
      setStoredTheme(t);
      if (termState.term) this._applyTerminalTheme();
      this.updateFavicon();
    },
  };
}
