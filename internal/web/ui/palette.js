import { localScheduleText, parseScheduleInput, schedulePresets, timeUntil } from './format.js';

// Ticket rows the command palette renders for one query. The rest are reachable
// by typing more, and the group hint says how many were left out.
const PALETTE_MAX_TICKETS = 6;

// Retries the palette makes to focus its input, one per frame. Alpine needs
// one frame, so the rest are slack for a dropped one.
const PALETTE_FOCUS_FRAMES = 10;

// The command palette: its rows, its ranking, and what running a row does.
// Everything here derives from state already in memory (tickets,
// selectedTicket, validMoves). The palette fetches nothing.
export function kontoraPalette() {
  return {
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
      { id: 'nav-stats',    view: 'stats',    label: 'Stats',          glyph: '▦' },
      { id: 'nav-archive',  view: 'archive',  label: 'Archive',        glyph: '▥' },
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

    // The board filter, token grammar included, plus stage and agent as free
    // text, which only the palette searches. Keeping the grammar shared is what
    // makes `project:` and `agent:` work in the palette as well.
    _paletteTextFields: ['stage', 'agent'],
    _paletteMatches(t, q) {
      return this.ticketMatchesQuery(t, q, this._paletteTextFields);
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

    // The row that drills from the ticket scope into the schedule one. Offered
    // only for a ticket the daemon would accept: the browser cannot see an open
    // Plannotator session or a pending annotation, so those refusals arrive
    // from the daemon and land in the error line.
    _paletteScheduleDrillRow(t) {
      return this._paletteCmdRow({
        id: 'scheduledrill:' + t.id, kind: 'scheduledrill', ticketId: t.id, glyph: '◷',
        title: t.scheduled_at ? 'Reschedule…' : 'Schedule…',
        meta: t.scheduled_at ? 'set' : '',
      });
    },

    // One preset or one parsed free-text time. iso is what the row sends.
    _paletteScheduleRow(t, o) {
      var row = this._paletteCmdRow({
        id: 'schedule:' + t.id + ':' + o.key, kind: 'schedule', ticketId: t.id,
        glyph: o.glyph || '◷', title: o.title, meta: o.meta || '',
      });
      row.glyphClass = o.glyph === '⌨' ? 'text-surface-600' : 'text-st-open';
      row.sub = o.sub || '';
      row.scheduleISO = o.iso || '';
      return row;
    },

    // The three presets, plus whatever the query parses to. A readable query
    // leads: it is what the user is typing, and Enter runs the top row.
    _paletteScheduleRows(t) {
      var q = (this.paletteQuery || '').trim();
      var now = new Date();
      var rows = [];
      var parsed = q ? parseScheduleInput(q, now) : null;
      if (parsed && parsed.iso) {
        rows.push(this._paletteScheduleRow(t, {
          key: 'typed', title: localScheduleText(parsed.at),
          sub: 'sets scheduled_at, keeps status open', meta: 'in ' + timeUntil(parsed.at, now),
          iso: parsed.iso,
        }));
      }
      var presets = schedulePresets(now).slice(0, 3)
        .filter(p => !q || parsed && parsed.iso || p.label.toLowerCase().includes(q.toLowerCase()));
      presets.forEach((p, i) => {
        rows.push(this._paletteScheduleRow(t, {
          key: p.key, title: p.label,
          // Only the first row spells out what scheduling does; repeating it on
          // every row would push the dates off the end of each line.
          sub: i === 0 && !rows.length ? p.date + ' · sets scheduled_at, keeps status open' : p.date,
          meta: p.rel, iso: p.iso,
        }));
      });
      // The typing row is a hint, not a command, so it carries no instant and
      // paletteRun refuses it. It drops out once the query parses.
      if (!parsed || !parsed.iso) {
        rows.push(this._paletteScheduleRow(t, {
          key: 'type', glyph: '⌨', title: parsed && parsed.error ? parsed.error : 'type a time or a duration',
          sub: '2026-09-01 09:00 · 24h · 3d · 2w',
        }));
      }
      return rows;
    },

    // The ticket's current pickup time in the card chip's short wording.
    _paletteScheduleCurrent(t) {
      var chip = this.scheduleChip(t);
      if (!chip) return this.scheduleLabel(t);
      return chip.abs + (chip.rel ? ' · in ' + chip.rel : '');
    },

    // clear and run-now: the two ways a schedule ends. Only for a ticket that
    // has one — there is nothing to clear otherwise.
    _paletteScheduleAlsoRows(t) {
      var clear = this._paletteCmdRow({
        id: 'schedclear:' + t.id, kind: 'scheduleclear', ticketId: t.id, glyph: '✕',
        title: 'clear the schedule',
      });
      clear.sub = 'removes scheduled_at · ticket stays open';
      clear.glyphClass = 'text-surface-600';
      var run = this._paletteCmdRow({
        id: 'schedrun:' + t.id, kind: 'schedulerun', ticketId: t.id, glyph: '▸',
        title: 'run now instead',
      });
      run.sub = 'clears scheduled_at and queues the ticket';
      run.glyphClass = 'text-ok';
      run.status = 'open';
      run.statusText = 'open';
      run.meta = '';
      return [clear, run];
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

      if (scoped && this.paletteScopeKind === 'schedule') {
        // A ticket that stopped being schedulable while the scope was open —
        // it started running, or another browser moved it — has no rows to
        // offer, so the scope pops back to the ticket rather than sitting empty.
        if (!this.canSchedule(scoped)) {
          this.paletteScopeKind = 'actions';
        } else {
          groups.push({
            label: 'when',
            hint: 'open ticket · stays open until due',
            items: this._paletteScheduleRows(scoped),
          });
          if (scoped.scheduled_at) {
            groups.push({
              label: 'also',
              // The chip's own wording, so the hint and the card the user just
              // came from name the time the same way.
              hint: 'currently starts ' + this._paletteScheduleCurrent(scoped),
              items: this._paletteScheduleAlsoRows(scoped),
            });
          }
        }
      }

      // Pulling a ticket into the open epic. The scope is the epic, and the
      // rows are the tickets that could join it: an epic cannot be a child, and
      // a ticket already in this epic has nothing to pull.
      if (scoped && this.paletteScopeKind === 'epic-pull') {
        var self = this;
        var candidates = this.tickets.filter(function (t) {
          if (t.kind === 'epic' || t.id === scoped.id) return false;
          if (t.parent && t.parent.id === scoped.id) return false;
          return !q || self._paletteMatches(t, q);
        });
        var pullable = this._paletteRank(candidates).slice(0, PALETTE_MAX_TICKETS);
        if (pullable.length) {
          groups.push({
            label: 'Pull into ' + scoped.id,
            hint: this.parseTitleTag(scoped).rest,
            items: pullable.map(function (t) {
              var row = self._paletteTicketRow(t);
              return Object.assign({}, row, { id: 'pull:' + t.id, kind: 'epicpull' });
            }),
          });
        }
      }

      if (scoped && this.paletteScopeKind !== 'schedule' && this.paletteScopeKind !== 'epic-pull') {
        var legal = this.paletteMoves(scoped);
        var moves = legal.filter(m => !q || m.label.toLowerCase().includes(q));
        var items = [];
        // A group with no surviving row is dropped, header and all: the root
        // branch below does the same, and a bare header above the no-match
        // message reads as a broken list.
        if (moves.length) {
          items = moves.map(m => this._paletteActionRow(scoped, m));
        }
        if (this.canSchedule(scoped) && (!q || 'schedule'.includes(q) || 'reschedule'.includes(q))) {
          items.unshift(this._paletteScheduleDrillRow(scoped));
        }
        if (items.length) {
          groups.push({
            label: 'Actions',
            // "legal" describes the ticket, so the count is of every legal move,
            // not of the ones the query left.
            hint: this.paletteStatusLabel(scoped.status) + ' · ' + legal.length + ' legal',
            items: items,
          });
        }
        var tabs = this._paletteTabs.filter(tb => !q || tb.label.toLowerCase().includes(q));
        if (tabs.length) {
          groups.push({ label: 'Open', hint: '', items: tabs.map(tb => this._paletteTabRow(scoped, tb)) });
        }
      } else if (!scoped) {
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
      this.paletteScopeKind = 'actions';
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

    // Pop one level: the schedule scope back to the ticket, the ticket scope
    // back to the root, then the palette itself. The body Escape stack calls
    // this before its own chain. Backspace and ← reach it only inside a scope,
    // since at the root they are ordinary editing keys.
    palettePop() {
      if (!this.paletteScope) { this.closePalette(); return; }
      if (this.paletteScopeKind === 'schedule') {
        this.paletteScopeKind = 'actions';
        this._paletteReset();
        return;
      }
      // The pull scope was entered from the epic page, not by drilling into a
      // ticket, so popping it closes the palette rather than landing on the
      // actions of a ticket the user never chose.
      if (this.paletteScopeKind === 'epic-pull') { this.closePalette(); return; }
      this.paletteScope = null;
      this._paletteReset();
    },

    // Open the palette straight into the pull-in scope for the open epic. It
    // fetches nothing: every row is derived from this.tickets, which the board
    // already holds.
    openPaletteScope(kind) {
      if (kind !== 'epic-pull' || !this.selectedEpic) return;
      this.openPalette();
      if (!this.paletteOpen) return;
      this.palettePush(this.selectedEpic.id, 'epic-pull');
    },

    palettePush(id, kind) {
      this.paletteScope = id;
      this.paletteScopeKind = kind || 'actions';
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
      if (row.kind === 'scheduledrill') { this.palettePush(row.ticketId, 'schedule'); return; }
      if (row.kind === 'epicpull') {
        this.closePalette();
        await this.epicPullIn(row.ticketId);
        return;
      }
      if (row.kind === 'schedule' || row.kind === 'scheduleclear' || row.kind === 'schedulerun') {
        await this._paletteRunSchedule(row);
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

    // The three schedule verbs. Each goes through the same calls the detail rail
    // and the phone sheet use, so a refusal is reported the one way.
    //
    // The palette closes first and confirms with a toast: its own error line
    // would go with it, and scheduleError only renders on surfaces that are not
    // on screen here.
    async _paletteRunSchedule(row) {
      // The hint row carries no instant. Enter on it is a no-op rather than a
      // silent close, so the palette stays open for the user to finish typing.
      if (row.kind === 'schedule' && !row.scheduleISO) return;
      var t = this.tickets.find(x => x.id === row.ticketId);
      if (!t) return;
      // The list may have re-derived over SSE since the row was built.
      if (!this.canSchedule(t)) {
        this.closePalette();
        this.showToast(t.id + ' can no longer be scheduled');
        return;
      }
      var label = this._paletteScheduleCurrent(t);
      this.closePalette();
      if (row.kind === 'schedulerun') {
        await this.moveTicketVia(t.id, 'run', null);
        if (!this.error) this.showToast(t.id + ' queued');
        return;
      }
      if (row.kind === 'scheduleclear') {
        await this.clearTicketSchedule(t);
        if (this.scheduleError) { this.error = this.scheduleError; return; }
        this.showToast(t.id + ' no longer starts ' + label);
        return;
      }
      await this._postSchedule(t, { scheduled_at: row.scheduleISO });
      if (this.scheduleError) { this.error = this.scheduleError; return; }
      this.showToast(t.id + ' starts ' + this._paletteScheduleCurrent(this.tickets.find(x => x.id === t.id) || t));
    },

    async _paletteRunAction(row) {
      if (!this._paletteMoveStillLegal(row)) {
        this.closePalette();
        this.showToast(row.ticketId + ' is no longer ' + this.paletteStatusLabel(row.fromStatus));
        return;
      }
      var t = this.tickets.find(x => x.id === row.ticketId);
      var body = row.move.status ? { status: row.move.status } : null;
      // A start that opens the init modal posts nothing, so there is no
      // completed action to confirm.
      var opensInit = this.needsInitModal(t) && this.ticketActionWouldStart(row.move.endpoint, body);
      this.closePalette();
      await this.moveTicketVia(t.id, row.move.endpoint, body);
      if (opensInit || this.error) return;
      var verb = this._paletteVerbs[row.move.label];
      this.showToast(t.id + ' ' + (typeof verb === 'string' ? verb : row.move.label.toLowerCase()));
    },

    palettePlaceholder() {
      if (!this.paletteScope) return 'Search tickets, or type a command…';
      if (this.paletteScopeKind === 'schedule') return 'A time or a duration: 2026-09-01 09:00, 24h, 3d…';
      return 'Action for ' + this.paletteScope + '…';
    },

    // What the scope chip reads. The schedule scope names itself, because its
    // rows are times rather than the ticket verbs the chip usually stands for.
    paletteScopeLabel() {
      if (!this.paletteScope) return '';
      return this.paletteScopeKind === 'schedule' ? 'schedule ' + this.paletteScope : this.paletteScope;
    },

    _paletteLegendRoot: [{ key: '↑↓', label: 'move' }, { key: '↵', label: 'open' },
                         { key: '→', label: 'actions' }, { key: 'esc', label: 'close' }],
    _paletteLegendScoped: [{ key: '↑↓', label: 'move' }, { key: '↵', label: 'run' },
                           { key: '⌫', label: 'back to search' }, { key: 'esc', label: 'close' }],
    _paletteLegendSchedule: [{ key: '↵', label: 'schedule' }, { key: '←', label: 'back to ticket' },
                             { key: '↑↓', label: 'move' }, { key: 'esc', label: 'close' }],
    paletteLegend() {
      if (!this.paletteScope) return this._paletteLegendRoot;
      return this.paletteScopeKind === 'schedule' ? this._paletteLegendSchedule : this._paletteLegendScoped;
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
  };
}
