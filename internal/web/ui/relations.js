// Relation chips one rail row shows before it has to be expanded. A ticket in
// the corpus carries up to 33 links, and the rail is 308px wide.
var RELATION_CAP = 8;
// Sub-tickets the tree shows before it has to be expanded. Higher than
// RELATION_CAP because the tree has the full 960px column, not a 308px rail.
var CHILDREN_CAP = 12;

// The relation rail and the sub-ticket tree.
export function kontoraRelations() {
  return {
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

    // The relations strip: deps, blocks and links, in that order and under the
    // strip's own wording. parent is in the breadcrumb, so relationRows' first
    // row is dropped here rather than reworded.
    _bandLabels: { deps: '⇠ waits on', blocks: 'blocks ⇢', links: 'related' },
    bandRows() {
      var self = this;
      return this.relationRows()
        .filter(function (r) { return r.key !== 'parent'; })
        .map(function (r) { return { key: r.key, label: self._bandLabels[r.key], refs: r.refs }; });
    },

    // The status hue as a background, for the 5px dots the strip, the crumb and
    // the tree draw. relationChipClass paints text; a dot has no text of its
    // own, so it takes the same class and fills from currentColor.
    relationDotClass(ref) {
      var mark = this._paletteStatusMarks[ref && ref.status];
      return (mark ? mark.cls : 'text-surface-600') + ' bg-current';
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
      if (c.status === 'in_progress' && c.stage_index && c.stage_count > 1) {
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
      if (c.status === 'in_progress') return this.formatDuration({ started_at: c.started_at }) || '—';
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
      var stages = t.stages || [];
      var i = stages.indexOf(t.stage);
      return {
        title: t.title,
        status: t.status,
        stage: t.stage,
        stage_index: i < 0 ? 0 : i + 1,
        stage_count: stages.length,
        started_at: (h.length && h[0].started_at) || t.started_at,
        completed_at: t.status === 'in_progress' ? null : (h.length ? h[h.length - 1].completed_at : t.updated_at),
      };
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
      span.setAttribute('data-tip-e-tag', tip.tag);
      span.setAttribute('data-tip-e-tag-color', tip.tagColor);
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
  };
}
