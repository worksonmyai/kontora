// How long the first key of a two-key shortcut stays armed.
const KEY_SEQ_MS = 800;

// The chip's leading clock, inline because the card is built as a string. Same
// lucide glyph the create form and the phone sheet draw at their own sizes.
const SCHED_CLOCK_SVG = '<svg width="9" height="9" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><polyline points="12 7 12 12 15.5 14"/></svg>';

// The board: card markup, the keyed column reconcile that patches only what
// changed, drag and drop, and the global key bindings.
export function kontoraBoard() {
  return {
    // What the assistant is told about the board: the view the user is on and
    // the filter narrowing it, which is what "these tickets" refers to.
    boardPageContext() {
      const lines = ['View: ' + this.currentView];
      const parsed = this.parseFilterQuery(this.searchQuery);
      if (!this.filterQueryEmpty(parsed)) {
        const parts = [];
        if (parsed.text) parts.push('text "' + parsed.text + '"');
        this._filterTokenKeys.forEach((key) => {
          parsed[key].forEach((v) => parts.push(key + ' ' + v.value));
        });
        lines.push('Board filter: ' + parts.join(', '));
      }
      return lines;
    },

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

      var cls = ['kt-card group relative rounded-[9px] px-3 py-2.5 cursor-pointer border overflow-hidden',
                 'bg-surface-900 border-edge-card hover:border-edge-hover hover:bg-surface-850',
                 'flex flex-col gap-1.5'];
      // The epic a child belongs to, if any. It paints a spine down the card's
      // left edge and a "3/6 epic" count in the footer, both in the epic's own
      // hue. data-pipe-color goes on those two spans and not on the card root:
      // the root's --pipe-h is the ticket's own pipeline, and reusing it would
      // repaint the pipeline tag in the epic's colour.
      var epic = this.epicCardInfo(ticket);
      var epicHue = epic ? this.pipelineColorByName(epic.id) : '';
      if (epic) cls.push('pl-[14px]');
      if (selected) cls.push('is-selected');
      if (!ticket.kontora) cls.push('border-dashed');
      if (ticket.status === 'cancelled') cls.push('opacity-60');
      else if (col.key === 'open' && ticket.scheduled_at) cls.push('kt-card-scheduled');
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

      // Nothing moves until a human answers in the terminal, so the badge
      // carries the wait. The nested data-since is what _updateCardTimers
      // patches on the tick, like the card's own clock.
      var waiting = ticket.waiting_for_input
        ? '<span class="waiting-chip" data-tip="' + esc('Waiting for input — ' + this.waitingLabel(ticket)) + '">'
          + '<span class="waiting-dot pulse-dot"></span>needs input '
          + '<span data-since="' + esc(ticket.waiting_since || '') + '">' + esc(this.waitingFor(ticket)) + '</span></span>'
        : '';

      // Open is the only column a schedule means anything in: the promotion is
      // what takes the ticket out of it.
      //
      // No pulse and no data-since: .pulse-dot and the 30s tick are for things
      // that are happening, and a schedule is not one. The distance is written
      // once per card build.
      var chip = col.key === 'open' ? this.scheduleChip(ticket) : null;
      var scheduled = chip
        ? '<span class="sched-chip" data-tip="' + esc('Starts ' + this.scheduleLabel(ticket)) + '">'
          + SCHED_CLOCK_SVG + esc(chip.abs)
          + (chip.rel ? '<span class="sched-chip-rel">' + esc('· ' + chip.rel) + '</span>' : '')
          + '</span>'
        : '';

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
        : '') + notKontoraBadge + glyph + scheduled + waiting;
      if (badgeParts) {
        // 34px, not the kebab's own width: it sits at right:6px and is 24px
        // wide, so anything less lets a long .pipe-tag push the chip under it.
        badgeRow = '<div class="flex items-center gap-2 min-w-0 pr-[34px]">' + badgeParts + '</div>';
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

      var epicSpine = epic
        ? '<span class="absolute left-0 top-0 bottom-0 w-[3px]" data-pipe-color="' + esc(epicHue) + '"'
          + ' style="background: hsl(var(--pipe-h) / 0.75)" aria-hidden="true"></span>'
        : '';
      var epicCount = epic
        ? '<span class="flex items-center gap-1.5 min-w-0"><span class="text-edge-faint">·</span>'
          + '<span class="truncate" data-pipe-color="' + esc(epicHue) + '" style="color: hsl(var(--pipe-h) / 0.9)"'
          + ' data-tip="' + esc(epic.title) + '">'
          + esc(epic.done + '/' + epic.total + ' epic') + '</span></span>'
        : '';

      return '<div class="' + cls.join(' ') + '"'
        + ' data-ticket-id="' + esc(ticket.id) + '"'
        + ' data-pipe-color="' + esc(this.ticketPipeColor(ticket)) + '"'
        + ' role="listitem" tabindex="0"'
        + ' aria-label="' + esc('Ticket ' + ticket.id + ': ' + (ticket.title || '')) + '">'
        + epicSpine
        + menu
        + badgeRow
        + '<p class="' + titleCls + '">' + tagSpan + esc(pt.rest) + '</p>'
        + progress
        + '<div class="flex items-center gap-2 text-[11px] font-mono text-surface-600 justify-between">'
        +   '<div class="flex items-center gap-1.5 min-w-0">'
        +     '<span class="group-hover:text-tx-3 transition-colors truncate"' + idTip + '>' + esc(ticket.id) + '</span>'
        +     epicCount
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
      // ticket.finished_at and ticket.updated_at reach the card only through
      // reviewFinishedAt, so its result stands in for both here. ticket.parent,
      // ticket.deps and ticket.links reach it only through the relation line
      // and, for parent, the epic spine, so those two strings stand in for
      // them. ticket.kind is in the signature although an epic draws no card:
      // a ticket that becomes one has to stop drawing the card it had.
      //
      // The spine and the "3/6 epic" footer are the one part of a card that is
      // board state rather than ticket state: a sibling finishing has to
      // repaint this card, and only _epicRollup knows that happened.
      var epic = this._epicRollup[(ticket.parent && ticket.parent.id) || ''];
      var epicSig = epic ? [epic.id, epic.title, epic.done, epic.total].join(':') : '';
      return [col.key, ticket.id, ticket.title, ticket.status, ticket.stage,
              ticket.pipeline, ticket.path, ticket.agent, ticket.attempt,
              ticket.kontora ? 1 : 0, ticket.started_at, ticket.created_at,
              ticket.waiting_for_input ? 1 : 0, ticket.waiting_since,
              ticket.waiting_tool, ticket.waiting_question,
              ticket.scheduled_at,
              this.reviewFinishedAt(ticket),
              (ticket.stages || []).join('>'),
              this._cardRelationSummary(ticket),
              ticket.kind, epicSig,
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
      // Archive is not a move: SetStatus refuses archived, so it has its own
      // verb and its own item rather than a row in validMoves.
      if (ticket.status === 'done' || ticket.status === 'cancelled') {
        items += '<button type="button" class="card-menu-item w-full px-3 py-2 text-left text-[12px] font-mono text-tx-3 hover:bg-surface-850 hover:text-tx-2 transition-colors" data-act="archive">Archive…</button>';
      } else if (!moves.length) {
        items += '<span class="block px-3 py-2 text-[12px] font-mono text-surface-600">No actions available</span>';
      }
      return '<div class="card-menu absolute right-0 top-7 min-w-[10rem] overflow-hidden rounded-lg border border-surface-700/60 bg-surface-900/95 shadow-lg shadow-black/30 z-20" role="menu">' + items + '</div>';
    },

    // The column a status is rendered in, or undefined for a status no column
    // claims (archived, tombstone).
    _columnForStatus(status) {
      return this.columns.find((c) => c.statuses.includes(status));
    },

    // Whether replacing a ticket in this.tickets changes anything the board
    // derives from it: which column it falls in, how it sorts, what the card
    // draws, and the tallies recomputeBoard keeps. A status with no column of
    // its own counts as changed, because the caller cannot show it either way.
    boardEntryChanged(before, after) {
      if (!before || !after || before.status !== after.status) return true;
      var col = this._columnForStatus(after.status);
      if (!col) return true;
      return this._cardSig(before, col) !== this._cardSig(after, col);
    },

    // Mark the column that holds a status as needing a patch on the next render.
    // Needed whenever a card node moved without going through the reconcile: the
    // cached ids and signatures then describe a DOM that no longer exists, and
    // renderColumn would skip the column as unchanged. The cache itself stays,
    // so _patchColumn still touches only the cards that moved or changed;
    // dropping it would rebuild every card in the column instead.
    invalidateColumnFor(status) {
      var col = this._columnForStatus(status);
      if (col) this._dirtyCols[col.key] = true;
    },

    // Reconcile a single column's cards against the cached ids and signatures,
    // so an update touches only the cards that changed. Untouched columns keep
    // their scroll position, their open menu, and their DOM nodes.
    renderColumn(key) {
      var el = document.getElementById('col-' + key);
      if (!el) return;
      // A dirty column is one a drag left out of sync with the board data, so
      // it is patched even when the data says nothing changed.
      var dirty = this._dirtyCols[key];
      delete this._dirtyCols[key];
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
        if (dirty || !prev || !prev.empty) el.innerHTML = this._emptyStateHTML(col);
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
      if (!dirty && prev && !prev.empty && this._sameCards(prev, ids, sigs)) return;
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
      if (this._dragging) { this._renderHeld = true; return; }
      this._renderHeld = false;
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
      this._dirtyCols = Object.create(null);
      this._dragging = false;
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
          } else if (act === 'archive') {
            self.openArchivePrompt(mid);
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
        var key = (e.key || '').toLowerCase();
        // ⌘J opens the assistant pane on the same terms as ⌘K opens the
        // palette, and from this one listener rather than a second: both have
        // to be swallowed before xterm forwards them.
        if (key !== 'k' && key !== 'j') return;
        if (self.isMobile) return;
        e.preventDefault();
        e.stopPropagation();
        if (key === 'k') self.togglePalette();
        else self.toggleAssistant();
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
      // Turn the FLIP animation off for anything but a short column: Sortable
      // measures and then transitions every card in a list it animates, on each
      // drag move, which is the drag stutter on a big board. A list animates its
      // own cards from its own instance option, so the column being dragged
      // *into* decides this as much as the one dragged from — set it on every
      // column at drag start rather than on the source alone.
      //
      // The cutoff is where the measurement put it. Dragging into a column on a
      // CPU throttled 4x, p90 of one drag move: 29ms at 30 cards, 59ms at 60,
      // 106ms at 90, 175ms at 150 — against a flat 14-16ms with the animation
      // off, at every size. A shorter duration does not help; the cost is the
      // per-card measuring, not the transition.
      var ANIM_THRESHOLD = 25;
      function tuneAnimations() {
        document.querySelectorAll('#board-cols [data-drop-status]').forEach(function (list) {
          var s = Sortable.get(list);
          if (s) s.option('animation', list.children.length > ANIM_THRESHOLD ? 0 : 150);
        });
      }
      new Sortable(el, {
        group: 'kanban',
        animation: 150,
        ghostClass: 'sortable-ghost',
        dragClass: 'sortable-drag',
        filter: '.empty-state',
        onStart: function(evt) {
          tuneAnimations();
          // Sortable owns the card nodes until the drop: a reconcile that runs
          // in between can move or replace the node under the cursor. Updates
          // that arrive during the drag land in _board, and the render they
          // asked for is held until onEnd.
          self._dragging = true;
          setDropTarget(evt.from);
        },
        onChange: function(evt) { setDropTarget(evt.to); },
        onEnd: function(evt) {
          self._dragging = false;
          clearDropTarget();
          // Everything the daemon reported while the drag held the board. Ahead
          // of the move below, so the optimistic status lands on fresh data.
          if (self._pendingTicketUpdates.length) self.flushTicketUpdates();
          var ticketId = evt.item.dataset.ticketId;
          var fromDrop = evt.from.dataset.dropStatus;
          var toDrop = evt.to.dataset.dropStatus;
          if (fromDrop === toDrop || !ticketId) {
            // No move to make, but a held render still has to land, and the
            // column the card was dropped back into needs the patch that puts
            // it back in sorted order.
            if (self._renderHeld) {
              self.invalidateColumnFor(fromDrop);
              self.renderBoard();
            }
            return;
          }
          // Sortable moved the card node behind the reconcile's back, so what
          // the render cache says about both columns no longer matches the DOM.
          // Without marking them, a render that finds the same cards with the
          // same signatures returns early, and a moveTask that ends without a
          // status change (an uninitialized ticket opens the init modal
          // instead) would leave the card sitting in the column it was dropped
          // on.
          self.invalidateColumnFor(fromDrop);
          self.invalidateColumnFor(toDrop);
          // No manual DOM restore: moveTask sets the status optimistically and
          // recomputeBoard → renderBoard patches both columns from canonical
          // data, moving the node Sortable moved (and reverting on failure).
          self.moveTask(ticketId, toDrop);
        }
      });
    },

    // Recompute every column's filtered+sorted list in one pass and cache it by
    // column key. Called imperatively at the few mutation points (load, batched
    // SSE flush, delete, debounced search) so the filter+sort runs once per
    // logical change rather than on every reactive template read.
    recomputeBoard() {
      var cols = this.columns;
      // Parsed once for the whole pass rather than per ticket.
      var query = this.parseFilterQuery(this.searchQuery);
      var filtering = !this.filterQueryEmpty(query);
      // status -> column key. Each status maps to exactly one column.
      var colOf = {};
      var board = {};
      cols.forEach(col => {
        board[col.key] = [];
        col.statuses.forEach(s => { colOf[s] = col.key; });
      });
      // Global kontora tallies for the favicon/running pill, computed ignoring
      // the search filter so they reflect all tickets.
      // needsInput is camelCase on purpose: the loop below writes any key a
      // ticket's status names, and "waiting" is a legal status.
      var counts = { in_progress: 0, paused: 0, todo: 0, done: 0, needsInput: 0 };
      // Per-agent running tally for the sidebar, filled in this same pass so
      // agentRunningCount doesn't filter the whole ticket array per agent row.
      var running = Object.create(null);
      // Epics collected on the way past. An epic is not work: it takes no
      // column, and its derived status must not reach the tallies behind the
      // favicon and the running pill, which would double-count its children.
      var epics = [];
      for (var i = 0; i < this.tickets.length; i++) {
        var t = this.tickets[i];
        if (t.kind === 'epic') {
          epics.push(t);
          continue;
        }
        if (t.kontora && counts[t.status] !== undefined) counts[t.status]++;
        if (t.kontora && t.waiting_for_input) counts.needsInput++;
        if (t.kontora && t.status === 'in_progress' && t.agent) {
          running[t.agent] = (running[t.agent] || 0) + 1;
        }
        var key = colOf[t.status];
        if (key === undefined) continue;            // no column -> not rendered
        if (filtering && !this.ticketMatchesQuery(t, query)) continue;
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
      // Before renderBoard: _cardHTML reads the roll-up for a child's spine.
      this._epicRecompute(epics);
      this.epicRefreshSelected();
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
  };
}
