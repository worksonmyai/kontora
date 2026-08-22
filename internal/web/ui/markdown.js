import { reEscape } from './format.js';

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

// NodeFilter constants, spelled out rather than read off the global.
var SHOW_TEXT = 4;
var FILTER_ACCEPT = 1;
var FILTER_REJECT = 2;

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

export function highlightMarkdown(src) {
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

// Markdown rendering and the entity chips a rendered summary gets.
// The highlighter above the mixin is pure: it feeds the ticket editor's
// underlay, which must reproduce its source character for character.
export function kontoraMarkdown() {
  return {
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
      // Leading directories, so internal/web/ui/app.js chips as one path
      // rather than leaving internal/web/ui/ beside a chip. Dotted stems
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
  };
}
