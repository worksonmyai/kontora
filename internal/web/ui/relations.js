// Sub-tickets the tree shows before it has to be expanded.
var CHILDREN_CAP = 12;
// Statuses that release dependents; mirrors internal/ticket/dependencies.go.
const RESOLVED_DEP_STATUSES = new Set(['human_review', 'done', 'cancelled', 'archived', 'closed']);

// The relation rail and the sub-ticket tree.
export function kontoraRelations() {
  return {
    // A chip needs a record behind the id. The board is the first place to
    // look; the open ticket's relations are the second, because the daemon
    // resolves those from every file on disk and so covers the tickets the
    // board hides (archived, or a status with no column).
    _ticketById(id) {
      var hit = this.sourceTicketById(id);
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

    // Lookup order for resolving ticket IDs in prose.
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

    // A dot shares the status text hue by filling from currentColor.
    relationDotClass(ref) {
      return this.relationHueClass(ref) + ' bg-current';
    },

    relationHueClass(ref) {
      var mark = this._paletteStatusMarks[ref && ref.status];
      return mark ? mark.cls : 'text-surface-600';
    },

    // Direct dependencies, and the upstream chain holder (which may be indirect).
    dependsOn() {
      var deps = this.selectedTicket?.deps || [];
      var holder = this.upstreamHolder();
      return {
        total: deps.length,
        done: deps.filter(ref => RESOLVED_DEP_STATUSES.has(ref.status)).length,
        holder,
        others: holder ? deps.filter(ref => !this.sameTicket(ref, holder)) : deps,
      };
    },

    relatedRefs() {
      var deps = this.selectedTicket?.deps || [];
      var holder = this.dependsOn().holder;
      return (this.selectedTicket?.links || []).filter(ref =>
        !deps.some(dep => this.sameTicket(dep, ref)) && !this.sameTicket(holder, ref));
    },

    chainVerdict() {
      return (this.chain && this.chain.verdict) || '';
    },

    // The one node the chain is waiting on. The daemon picks it, so the
    // tie-break between two equally deep blockers lives in one place.
    chainHolder() {
      return ((this.chain && this.chain.nodes) || []).find(function (n) { return n.holds_chain; }) || null;
    },

    upstreamHolder() {
      if (!this.selectedTicket || this.isSecondary(this.selectedTicket)) return null;
      const holder = this.chainHolder();
      return holder?.direction === 'upstream' ? holder : null;
    },

    chainResumeMove() {
      var holder = this.upstreamHolder();
      if (!holder || holder.missing || this.isSecondary(holder)) return null;
      var endpoint = holder.status === 'open' ? 'run' : holder.status === 'paused' ? 'retry' : '';
      if (!endpoint) return null;
      // Chain nodes are relation refs, not lifecycle subjects with permissions.
      // Recheck the live ticket rather than acting on a stale chain response.
      var ticket = this.lifecycleResolve(holder);
      if (ticket?.status !== holder.status) return null;
      var command = this.lifecycleFromEndpoint(holder, endpoint);
      if (!this._lifecycleAllowed(ticket, command)) return null;
      return { id: holder.id, ref: holder, endpoint, label: command.label === 'Rearm wait'
        ? 'Rearm wait for ' + holder.id : 'Run ' + holder.id + (endpoint === 'run' ? '…' : ' again') };
    },

    resumeChainHolder() {
      var move = this.chainResumeMove();
      if (move) return this.moveTicketVia(this.ticketKey(move.ref), move.endpoint);
    },

    // Sub-tickets, capped like a relation row. `last` drives the connector: the
    // stem stops at the elbow on the final row, and "final" means the last row
    // drawn, so a collapsed tail does not leave the stem running into nothing.
    childRows() {
      var all = this.selectedTicket?.children || [];
      var shown = this.childrenExpanded ? all : all.slice(0, CHILDREN_CAP);
      return shown.map(function (c, i) {
        return Object.assign({}, c, { last: i === shown.length - 1 });
      });
    },

    childrenHidden() {
      var n = (this.selectedTicket?.children || []).length;
      return this.childrenExpanded ? 0 : Math.max(0, n - CHILDREN_CAP);
    },

    // Derived, never stored: the rollup counts every child, not only the ones
    // the tree is currently showing.
    childRollup() {
      var all = this.selectedTicket?.children || [];
      var done = all.filter(function (c) { return c.status === 'done'; }).length;
      return { done: done, total: all.length, pct: all.length ? Math.round((done / all.length) * 100) : 0 };
    },

    // "implement 2/4" for a running child on a multi-stage pipeline, the stage
    // word alone otherwise. The position, not a percentage: the stage index is
    // the only progress signal a ticket carries, and the board card's tooltip
    // already says it this way.
    childStageLine(c) {
      if (!c || !c.stage) return '—';
      if (c.graph_execution || c.graph_definition) return c.stage + (c.graph_execution?.current_template ? ' · ' + c.graph_execution.current_template : '');
      if (this.executionActive(c) && c.stage_index && c.stage_count > 1) {
        return c.stage + ' ' + c.stage_index + '/' + c.stage_count;
      }
      return c.stage;
    },

    // A finished child reports the wall the daemon bounded; a running one is
    // clocked off the reactive `now`, so the column ticks with the 30s timer.
    // Only a running one: a child with no run left to make has no time to count
    // up to, and a live clock on it would climb forever.
    childElapsed(c) {
      if (!c || !c.started_at) return '—';
      if (this.executionActive(c)) return this.formatDuration({ started_at: c.started_at }) || '—';
      return this.formatElapsed(c.started_at, c.completed_at) || '—';
    },

    // One sub-ticket row off a ticket_updated event, derived the way the
    // daemon's childInfo derives it: the stage position from the ticket's own
    // pipeline, and the wall bounds from the first pickup to the last exit,
    // because started_at is rewritten at every stage spawn. A running child
    // gets no completion, which is what makes the page clock it live. The event
    // carries no completed_at of its own, so a child that finished without a
    // history row ends at its file's mtime, which is what ticketWall does with
    // the same gap.
    childRowFromEvent(t) {
      var h = t.history || [];
      var stages = (t.graph_definition || t.graph_execution) ? [] : t.stages || [];
      var i = stages.indexOf(t.stage);
      return {
        title: t.title,
        status: t.status,
        stage: t.stage,
        graph_definition: t.graph_definition,
        graph_execution: t.graph_execution,
        stage_index: i < 0 ? 0 : i + 1,
        stage_count: stages.length,
        started_at: (h.length && h[0].started_at) || t.started_at,
        execution_active: this.executionActive(t),
        completed_at: this.executionActive(t) ? null : (h.length ? h[h.length - 1].completed_at : t.updated_at),
      };
    },

    // A ref the daemon could not resolve names a ticket that is no longer in
    // the tickets dir. It stays on screen, because the frontmatter still points
    // at it, but it is not a link.
    relationKnown(ref) {
      return !!ref?.status && !ref.missing;
    },

    relationChipClass(ref) {
      var mark = this._paletteStatusMarks[ref && ref.status];
      return 'ent ent-ticket ' + (mark ? mark.cls : 'text-surface-600')
        + (this.relationKnown(ref) ? '' : ' ent-ticket-gone');
    },

    // The hover card behind a ticket id: the title, which the id does not say,
    // the status word, and what a click does. The [tag] prefix comes out of the
    // title so the card can paint it in the project's hue, as the board card
    // and the palette row do. Only a prefix the title wrote itself: a ref
    // carries no path, so there is no basename to stand in, and the same id
    // would otherwise read one way in the rail and another in prose.
    ticketTip(ref) {
      var known = this.relationKnown(ref);
      var pt = this.splitTitleTag(ref && ref.title);
      return {
        tag: pt.tag ? '[' + pt.tag + ']' : '',
        // The bare tag, the string every other site hashes.
        tagColor: this.pipelineColorByName(pt.tag),
        // A title that is a tag and nothing else leaves no title. The card is
        // suppressed on an empty one, taking the status and the hint with it.
        title: pt.rest || (ref && ref.id) || '',
        body: known ? this.paletteStatusLabel(ref.status) : 'not found',
        hint: known ? 'click to open' : '',
      };
    },

    // Open a ticket named by a relation. The board entry is preferred when there
    // is one, so the card behind the panel shows as selected; a ticket the board
    // hides is opened from the ref itself and the detail fetch fills it in.
    async openTicketRef(ref) {
      if (!this.relationKnown(ref)) return;
      var source = ref.source || this.selectedTicket?.source;
      var identity = { id: ref.id, ...(source ? { source: source } : {}) };
      this.rememberDetailView();
      await this.navigateDestination({ view: !this.isSecondary(identity) && ref.status === 'archived' ? 'archive' : 'board',
        ticketId: ref.id, ...(source ? { source } : {}) });
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
      span.setAttribute('data-tip-e-tag', tip.tag);
      span.setAttribute('data-tip-e-tag-color', tip.tagColor);
      span.setAttribute('data-tip-e-body', tip.body);
      span.setAttribute('data-tip-e-hint', tip.hint);
      var self = this;
      if (this.relationKnown(t)) {
        span.setAttribute('role', 'link');
        span.setAttribute('tabindex', '0');
        span.addEventListener('click', function () { self.openTicketRef(t); });
        span.addEventListener('keydown', function (event) {
          if (event.key !== 'Enter' && event.key !== ' ') return;
          event.preventDefault();
          self.openTicketRef(t);
        });
      }
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
  };
}
