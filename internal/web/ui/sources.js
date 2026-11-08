export function kontoraSources() {
  return {
    ticketSources: [],
    secondaryTickets: [],
    sourceFilter: '',
    sourcesError: null,
    claimSubmitting: false,
    _claimKey: null,
    _sourceRelationDrafts: {},
    sourceRelationSubmitting: false,
    sourceRelationOptions: [
      { value: 'dep', label: 'Add dependency' }, { value: 'undep', label: 'Remove dependency' },
      { value: 'link', label: 'Link tickets' }, { value: 'unlink', label: 'Unlink tickets' },
      { value: 'parent', label: 'Set parent' }, { value: 'unparent', label: 'Clear parent' },
      { value: 'children', label: 'Set child order' },
    ],

    isSecondary(ticket) {
      return !!ticket?.source && ticket.source !== 'primary';
    },

    ticketKey(ticket) {
      if (typeof ticket === 'string') return ticket;
      return this.isSecondary(ticket) ? encodeURIComponent(ticket.source) + '/' + encodeURIComponent(ticket.id) : ticket?.id;
    },

    ticketIdentity(key) {
      if (typeof key !== 'string') return key;
      var split = key.indexOf('/');
      return split < 0 ? { id: key } : { source: decodeURIComponent(key.slice(0, split)), id: decodeURIComponent(key.slice(split + 1)) };
    },

    sameTicket(a, b) {
      return !!a && !!b && this.ticketKey(a) === this.ticketKey(b);
    },

    ticketURL(ticket, suffix = '') {
      var t = this.ticketIdentity(ticket);
      return (this.isSecondary(t) ? '/api/sources/' + encodeURIComponent(t.source) : '/api')
        + '/tickets/' + encodeURIComponent(t.id) + suffix;
    },

    sourceLabel(ticket) {
      if (!this.isSecondary(ticket)) return 'primary';
      return this.ticketSources.find(s => s.id === ticket.source)?.path || ticket.source;
    },

    sourceAvailable(ticket) {
      return !this.isSecondary(ticket) || this.ticketSources.find(s => s.id === ticket.source)?.available !== false;
    },

    ticketCanEdit(ticket) {
      return !!ticket && this.sourceAvailable(ticket) && (this.isSecondary(ticket) ? ticket.can_edit === true : ticket.can_edit !== false);
    },

    ticketCanControl(ticket) {
      return !!ticket && !this.isSecondary(ticket) && ticket.can_control !== false;
    },

    ticketCanClaim(ticket) {
      return this.isSecondary(ticket) && this.sourceAvailable(ticket) && !this.sourceRelationSubmitting && ticket.can_claim === true;
    },

    editorClaimLocked(ticket = this.selectedTicket) {
      return this.claimSubmitting && this._claimKey === this.ticketKey(ticket);
    },

    editorCanEdit(ticket = this.selectedTicket) {
      return this.ticketCanEdit(ticket) && (this.isSecondary(ticket) || ['open', 'todo', 'paused'].concat(this.configCache?.custom_statuses || []).includes(ticket.status));
    },

    allBoardTickets() {
      return this.tickets.concat(this.secondaryTickets);
    },

    sourceTickets(ticket = this.selectedTicket) {
      return this.isSecondary(ticket) ? this.secondaryTickets.filter(t => t.source === ticket.source) : this.tickets;
    },

    async fetchTicketSources() {
      try {
        var res = await fetch('/api/ticket-sources');
        if (!res.ok) throw new Error('Failed to load ticket sources');
        this.ticketSources = await res.json();
        this.sourcesError = null;
        var lists = await Promise.all(this.ticketSources.filter(s => s.id !== 'primary').map(async source => {
          if (!source.available) return [];
          try {
            var response = await fetch('/api/sources/' + encodeURIComponent(source.id) + '/tickets');
            if (!response.ok) throw new Error('Failed to load source tickets');
            return (await response.json()).map(t => this.boardEntry({ ...t, source: source.id }));
          } catch (e) {
            source.available = false;
            source.error = e.message;
            return [];
          }
        }));
        this.secondaryTickets = lists.flat();
      } catch (e) {
        this.sourcesError = e.message;
      }
      this.recomputeBoard();
    },

    applySecondaryUpdate(ticket) {
      if (!this.isSecondary(ticket)) return;
      var idx = this.secondaryTickets.findIndex(t => this.sameTicket(t, ticket));
      if (idx < 0) this.secondaryTickets.push(this.boardEntry(ticket));
      else this.secondaryTickets[idx] = this.boardEntry(ticket);
      if (this.sameTicket(this.selectedTicket, ticket)) {
        this._selectedTicketUpdateSeq++;
        this.selectedTicket = this._mergeDetailProjection(this.selectedTicket, ticket);
      }
    },

    deleteSecondaryTicket(ticket, close = true) {
      this._pendingTicketUpdates = this._pendingTicketUpdates.filter(t => !this.sameTicket(t, ticket));
      this.secondaryTickets = this.secondaryTickets.filter(t => !this.sameTicket(t, ticket));
      if (close && this._claimKey !== this.ticketKey(ticket) && this.sameTicket(this.selectedTicket, ticket)) this.closeDetail();
      this.recomputeBoard();
    },

    sourceRelationDraft(ticket = this.selectedTicket) {
      var key = this.ticketKey(ticket);
      return this._sourceRelationDrafts[key] ||= { action: 'dep', targets: '' };
    },

    async saveSourceRelation() {
      var ticket = this.selectedTicket;
      if (!this.isSecondary(ticket) || !this.ticketCanEdit(ticket) || this.sourceRelationSubmitting) return false;
      var draft = this.sourceRelationDraft(ticket);
      var ids = draft.targets.split(/[\s,]+/).filter(Boolean);
      var action = draft.action;
      var body = { related: ids };
      if (action === 'parent') body = { parent: ids[0] };
      if (action === 'unparent') body = {};
      if (action === 'children') body = { children: ids };
      if (['dep', 'undep', 'parent'].includes(action) && ids.length !== 1) {
        this.error = 'Enter one ticket ID for this relationship.';
        return false;
      }
      if (['link', 'unlink'].includes(action) && !ids.length) {
        this.error = 'Enter at least one ticket ID.';
        return false;
      }
      this.sourceRelationSubmitting = true;
      try {
        var res = await fetch(this.ticketURL(ticket, '/' + action), {
          method: action === 'children' ? 'PUT' : 'POST',
          headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        var full = await res.json();
        if (!res.ok) throw new Error(full.error || 'Relationship update failed');
        if (!this.sameTicket(full, ticket)) throw new Error('Invalid source ticket response');
        this.applySecondaryUpdate(full);
        this.recomputeBoard();
        draft.targets = '';
        return true;
      } catch (e) { this.error = e.message; return false; }
      finally { this.sourceRelationSubmitting = false; }
    },

    async claimTicket(ticket = this.selectedTicket) {
      if (!this.ticketCanClaim(ticket) || this.claimSubmitting) return false;
      var key = this.ticketKey(ticket);
      this.claimSubmitting = true;
      this._claimKey = key;
      try {
        this.closeAgentPickerHost('edit');
        if (!this.sameTicket(this.selectedTicket, ticket)) {
          if (this.isMobile) await this.openMobileTicket(ticket);
          else await this.selectTicket(ticket);
        }
        if (!this.sameTicket(this.selectedTicket, ticket)) return false;
        if (!await this.flushEditSave()) return false;
        await this._waitEditorSaves(key);
        var state = this._editorSaves[key];
        if (state && (state.pending || !this._sameEditorDraft(state.draft, state.ack))) return false;
        if (this.sameTicket(this.selectedTicket, ticket) && this.editing && state &&
            !this._sameEditorDraft(this._ordinaryEditorDraft(), state.ack)) return false;
        if (!this.sameTicket(this.selectedTicket, ticket) || !this.ticketCanClaim(this.selectedTicket)) return false;
        var current = this.selectedTicket;
        if (this.editing && state && this.descriptionText(current) !== state.ack.description) {
          this.error = 'The source description changed. Reopen the ticket before Claim.';
          return false;
        }
        var warnings = (current.claim_warnings || []).map(w => typeof w === 'string' ? w : JSON.stringify(w));
        if (!window.confirm('Claim ' + current.id + ' into primary without starting work? Related tickets are not moved; references resolve in primary.' + (warnings.length ? '\n\n' + warnings.join('\n') : ''))) return false;
        var res = await fetch(this.ticketURL(current, '/claim'), {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ revision: current.revision }),
        });
        var full = await res.json();
        if (!res.ok) throw new Error(full.error || 'Claim failed');
        if (this.isSecondary(full) || full.id !== ticket.id) throw new Error('Invalid claim response');
        this.deleteSecondaryTicket(ticket, false);
        delete this._editorSaves[key];
        this.applyTicketUpdate(full);
        this.recomputeBoard();
        if (this.sameTicket(this.selectedTicket, ticket)) {
          this.editing = false;
          this.closeDetail(false);
          if (this.isMobile) await this.openMobileTicket(full);
          else await this.selectTicket(full);
        }
        return true;
      } catch (e) {
        this.error = e.message;
        return false;
      } finally {
        this.claimSubmitting = false;
        this._claimKey = null;
      }
    },
  };
}
