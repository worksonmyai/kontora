// The search-box filter grammar and the matcher the board runs it through.

// Split a filter box into terms. A double-quoted run counts as one term, which
// is how a project or agent name that has a space in it survives the split. An
// unclosed quote runs to the end of the box, so the term is whole while the
// closing quote is still being typed. Case is left alone: the archive box maps
// a chip back onto the substring the user typed, so it needs the terms as
// written.
export function filterSplitTerms(q) {
  return String(q == null ? '' : q).match(/(?:[^\s"]+|"[^"]*"?)+/g) || [];
}

export function filterUnquote(s) {
  return String(s || '').replace(/"/g, '');
}

export function kontoraFilter() {
  return {
    // Filter-box terms that address one field instead of the free-text fields.
    _filterTokenKeys: ['project', 'agent', 'epic'],

    _filterTerms(q) {
      return filterSplitTerms((q || '').toLowerCase());
    },

    // Split the filter box into its `<key>:<value>` tokens and the free text
    // left over, all lowercased. A term with an unknown key stays free text. A
    // known key with nothing after the colon constrains nothing, so the board
    // does not empty out between the colon and the first typed character.
    // `<key>:=<value>` asks for the whole field instead of a substring; that is
    // the form a sidebar click writes.
    //
    // Memoized on the raw string: the board parses once per pass, then every
    // sidebar row parses the same string again for its highlight. Callers read
    // the result and never write to it.
    parseFilterQuery(q) {
      var raw = q || '';
      if (this._parsedQuery && this._parsedQuerySrc === raw) return this._parsedQuery;
      var parsed = { text: '' };
      this._filterTokenKeys.forEach(k => { parsed[k] = []; });
      var free = [];
      this._filterTerms(raw).forEach(term => {
        var m = /^([a-z]+):(=?)(.*)$/.exec(term);
        if (!m || !this._filterTokenKeys.includes(m[1])) {
          free.push(this._unquote(term));
          return;
        }
        var value = this._unquote(m[3]);
        if (value) parsed[m[1]].push({ value: value, exact: m[2] === '=' });
      });
      // Free text on both sides of a token is rejoined, so `fonts project:x bar`
      // looks for the one substring "fonts bar" — the same single-substring
      // match a query with no token in it has always been.
      parsed.text = free.join(' ');
      this._parsedQuerySrc = raw;
      this._parsedQuery = parsed;
      return parsed;
    },

    _unquote: filterUnquote,

    // A value with a space in it has to be quoted to survive _filterTerms.
    _quoteFilterValue(value) {
      var v = this._unquote(value);
      return /\s/.test(v) ? '"' + v + '"' : v;
    },

    filterQueryEmpty(parsed) {
      return !parsed.text && this._filterTokenKeys.every(k => parsed[k].length === 0);
    },

    // Tokens of different keys narrow together; repeats of one key widen it. A
    // typed value matches as a substring, so a half-typed name still narrows;
    // the `=` form matches the whole field, so clicking `api` in the sidebar
    // does not also pull in `api-sdk`.
    _tokenMatches(values, field) {
      var f = (field || '').toLowerCase();
      return values.some(v => (v.exact ? f === v.value : f.includes(v.value)));
    },

    // Takes the raw filter string or an already parsed query, so a caller that
    // tests many tickets can parse once. extraTextFields names ticket fields to
    // search alongside the four the board searches; the palette passes its two.
    ticketMatchesQuery(ticket, q, extraTextFields) {
      var parsed = typeof q === 'object' && q !== null ? q : this.parseFilterQuery(q);
      if (parsed.project.length && !this._tokenMatches(parsed.project, this.ticketProjectName(ticket))) return false;
      if (parsed.agent.length && !this._tokenMatches(parsed.agent, ticket.agent)) return false;
      // epic:<id> narrows the board to one epic's children. The epic itself
      // draws no card, so it never has to match its own token.
      if (parsed.epic.length && !this._tokenMatches(parsed.epic, ticket.parent && ticket.parent.id)) return false;
      if (!parsed.text) return true;
      var fields = [ticket.title, ticket.id, this.pathBasename(ticket.path), ticket.pipeline];
      (extraTextFields || []).forEach(k => fields.push(ticket[k]));
      return fields.some(f => f && f.toLowerCase().includes(parsed.text));
    },

    // Put a single token in the filter box, or empty the box when that token is
    // already the whole query. Drives the sidebar project and agent rows. The
    // token is written in the `=` form: a row selects the name it shows, not
    // every name that contains it.
    toggleFilterToken(key, value) {
      this.searchQuery = this.filterTokenActive(key, value) ? '' : key + ':=' + this._quoteFilterValue(value);
    },

    // True only when the query is the token that row's click writes, so a row
    // stops looking active as soon as the query says more than the row does.
    filterTokenActive(key, value) {
      var parsed = this.parseFilterQuery(this.searchQuery);
      if (parsed.text) return false;
      var want = this._unquote(value).toLowerCase();
      return this._filterTokenKeys.every(k =>
        k === key
          ? parsed[k].length === 1 && parsed[k][0].exact && parsed[k][0].value === want
          : parsed[k].length === 0
      );
    },
  };
}
