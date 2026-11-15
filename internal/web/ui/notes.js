// The notes tab: the ticket's conversation. Notes live in the ticket body's
// "## Notes" section, so a stage prompt that interpolates the body carries
// them; reactions live in a sidecar beside the ticket file.
//
// Every key here is prefixed `note*`, because ui/detail.js and ui/activity.js
// already own the unprefixed ticket helpers and merge() throws on a repeat.
export function kontoraNotes() {
  const requests = new Map();
  const drafts = new Map();
  return {
    _noteDraftEpoch: 0,
    _noteReplyVersion: 0,
    _noteEditVersion: 0,
    _noteDeleteVersion: 0,

    _noteDraftOwner(kind, text, target = null) {
      var owner = { key: this.ticketKey(this.selectedTicket), epoch: this._noteDraftEpoch, detail: this._ticketDetailSeq,
        kind, text, target, version: this['_note' + kind + 'Version'] };
      drafts.set(kind, owner);
      return owner;
    },

    _noteDraftCurrent(owner) {
      return drafts.get(owner.kind) === owner && owner.epoch === this._noteDraftEpoch && owner.detail === this._ticketDetailSeq &&
        owner.key === this.ticketKey(this.selectedTicket) &&
        owner.version === this['_note' + owner.kind + 'Version'];
    },
    // Which note the pointer is over, which one has an open reply or edit box,
    // and the drafts those boxes hold. One of each: opening a box closes the
    // one that was open.
    noteHover: null,
    noteReplyOn: null,
    noteReplyDraft: '',
    noteReplySubmitting: false,
    noteEditOn: null,
    noteEditDraft: '',
    noteEditSubmitting: false,
    noteDeleteTarget: null,
    noteDeleteSubmitting: false,
    // The pool the add-reaction button cycles. A picker would be the real
    // affordance; this is what the design ships with.
    noteEmojiPool: ['👍', '👀', '🎉', '🙏'],

    // ---- identity ----------------------------------------------------------

    // Who this browser is. The daemon signs a note from the composer with its
    // configured author, so the page has to name the same person to tell your
    // own notes apart from everyone else's.
    noteMe() {
      return this.configCache?.author || '';
    },

    noteIsMine(n) {
      var me = this.noteMe();
      return !!me && n?.author === me;
    },

    // An author with no name is a note written before notes carried one: the
    // neutral hue, no role chip.
    noteAuthorHue(n) {
      if (!n?.author) return 'none';
      if (n.author_kind === 'system') return 'none';
      if (n.author_kind === 'agent') return this.pipelineColorByName(n.author);
      return 'mauve';
    },

    noteInitial(n) {
      var name = n?.author || '';
      return name ? name.charAt(0) : '·';
    },

    noteRole(n) {
      return n?.author_kind === 'human' ? '' : (n?.author_kind || '');
    },

    // "2h ago" / "just now" — a note's relative time, with the absolute one in
    // the title. At is free-form, so an unparseable byline shows itself.
    noteAgo(at) {
      if (!at) return '';
      if (isNaN(Date.parse(at))) return at;
      var ago = this.timeAgo(at);
      return ago === 'just now' ? ago : ago + ' ago';
    },

    // ---- the feed ----------------------------------------------------------

    // Top-level notes and lifecycle rows on one ascending timeline, with each
    // note's replies hanging off it.
    //
    // A reply whose parent is not in the list — its parent was deleted out from
    // under a stale page — is drawn at the top level rather than dropped, so a
    // note never disappears without being deleted.
    notesFeed() {
      var notes = this.selectedTicket?.notes || [];
      var byID = {};
      notes.forEach(function (n) { byID[n.id] = n; });

      var items = [];
      notes.forEach((n) => {
        if (n.parent_id && byID[n.parent_id]) return;
        items.push({
          kind: 'note',
          key: 'n:' + n.id,
          at: n.at || '',
          note: n,
          replies: notes.filter(function (r) { return r.parent_id === n.id; }),
        });
      });
      if (notes.length) items = items.concat(this.noteLifecycleRows());

      // A note's `at` is free-form: a hand-written byline may not be a date at
      // all. Those sort to the front rather than to an arbitrary place.
      return items.sort(function (a, b) {
        var ta = Date.parse(a.at), tb = Date.parse(b.at);
        if (isNaN(ta) && isNaN(tb)) return 0;
        if (isNaN(ta)) return -1;
        if (isNaN(tb)) return 1;
        return ta - tb;
      });
    },

    // The lifecycle rows the history can support. Only completed agent runs are
    // recorded, so a manual status move draws nothing; a pause is not a row
    // either, but the daemon writes a note when it pauses, which is.
    noteLifecycleRows() {
      var t = this.selectedTicket;
      if (!t) return [];
      var rows = [];
      (t.history || []).forEach((h, i) => {
        var stage = h.stage || 'stage';
        if (h.started_at) {
          rows.push({
            kind: 'event',
            key: 'e:' + i + ':start',
            at: h.started_at,
            color: h.run > 0 ? 'amber' : 'indigo',
            label: (h.run > 0 ? 'retried · stage ' : 'picked up · stage ') + stage,
            meta: '',
          });
        }
        if (h.completed_at) {
          var outcome = this.runOutcomeLabel(h);
          var color = outcome === 'complete' ? 'green'
            : outcome === 'blocked' ? 'amber'
            : outcome === 'failed' || outcome === 'protocol_error' ? 'rose' : 'indigo';
          rows.push({
            kind: 'event',
            key: 'e:' + i + ':end',
            at: h.completed_at,
            color: color,
            label: stage + ' · ' + outcome,
            meta: this.noteRunMeta(h),
          });
        }
      });
      if (t.status === 'human_review' && t.finished_at) {
        rows.push({ kind: 'event', key: 'e:review', at: t.finished_at, color: 'mauve', label: 'moved to human review', meta: '' });
      }
      return rows;
    },

    noteRunMeta(h) {
      var meta = 'runner exit ' + (Number.isInteger(h.exit_code) ? h.exit_code : 'unknown');
      if (h.outcome_reason) meta += ' · ' + h.outcome_reason;
      if (h.invocation_id) meta += ' · invocation ' + h.invocation_id;
      if (h.started_at && h.completed_at) {
        var secs = Math.round((new Date(h.completed_at) - new Date(h.started_at)) / 1000);
        var dur = this.formatSeconds(secs);
        if (dur) meta += ' · ' + dur;
      }
      return meta;
    },

    noteCount() {
      return (this.selectedTicket?.notes || []).filter(function (n) { return !n.parent_id; }).length;
    },

    // The header's right-hand meta: how long ago the newest note landed.
    noteLastAgo() {
      var notes = this.selectedTicket?.notes || [];
      var newest = 0;
      notes.forEach(function (n) {
        var t = Date.parse(n.at);
        if (!isNaN(t) && t > newest) newest = t;
      });
      if (!newest) return 'nothing yet';
      var ago = this.noteAgo(new Date(newest).toISOString());
      return ago === 'just now' ? ago : 'last ' + ago;
    },

    // ---- writing -----------------------------------------------------------

    async submitNote() {
      var text = (this.noteDraft || '').trim();
      if (!text || !this.ticketCanEdit(this.selectedTicket) || this.noteSubmitting) return;
      var owner = this._noteDraftOwner('Add', this.noteDraft);
      this.noteSubmitting = true;
      var ok = await this._noteRequest('/note', 'POST', { text: text }, 'Failed to add note', owner);
      if (!this._noteDraftCurrent(owner)) return;
      if (ok && this.noteDraft === owner.text) this.noteDraft = '';
      this.noteSubmitting = false;
    },

    noteOpenReply(n) {
      if (!n || !this.ticketCanEdit(this.selectedTicket)) return;
      this.noteCancelEdit();
      this._noteReplyVersion++;
      this.noteReplySubmitting = false;
      this.noteReplyOn = n.id;
      this.noteReplyDraft = '';
    },

    noteCancelReply() {
      this._noteReplyVersion++;
      this.noteReplySubmitting = false;
      this.noteReplyOn = null;
      this.noteReplyDraft = '';
    },

    async submitReply() {
      var text = (this.noteReplyDraft || '').trim();
      var parent = this.noteReplyOn;
      if (!text || !parent || !this.ticketCanEdit(this.selectedTicket) || this.noteReplySubmitting) return;
      var owner = this._noteDraftOwner('Reply', this.noteReplyDraft, parent);
      this.noteReplySubmitting = true;
      var ok = await this._noteRequest('/note', 'POST', { text: text, parent: parent }, 'Failed to add reply', owner);
      if (!this._noteDraftCurrent(owner)) return;
      this.noteReplySubmitting = false;
      if (ok && this.noteReplyOn === parent && this.noteReplyDraft === owner.text) this.noteCancelReply();
    },

    noteOpenEdit(n) {
      if (!this.ticketCanEdit(this.selectedTicket) || !this.noteIsMine(n)) return;
      this.noteCancelReply();
      this._noteEditVersion++;
      this.noteEditSubmitting = false;
      this.noteEditOn = n.id;
      this.noteEditDraft = n.text || '';
    },

    noteCancelEdit() {
      this._noteEditVersion++;
      this.noteEditSubmitting = false;
      this.noteEditOn = null;
      this.noteEditDraft = '';
    },

    async saveNoteEdit() {
      var text = (this.noteEditDraft || '').trim();
      var id = this.noteEditOn;
      var note = this.selectedTicket?.notes?.find(n => n.id === id);
      if (!text || !id || !this.ticketCanEdit(this.selectedTicket) || !this.noteIsMine(note) || this.noteEditSubmitting) return;
      var owner = this._noteDraftOwner('Edit', this.noteEditDraft, id);
      this.noteEditSubmitting = true;
      var ok = await this._noteRequest('/notes/' + encodeURIComponent(id), 'PATCH', { text: text }, 'Failed to edit note', owner);
      if (!this._noteDraftCurrent(owner)) return;
      this.noteEditSubmitting = false;
      if (ok && this.noteEditOn === id && this.noteEditDraft === owner.text) this.noteCancelEdit();
    },

    noteAskDelete(n) {
      if (!this.ticketCanEdit(this.selectedTicket) || !this.noteIsMine(n)) return;
      this._noteDeleteVersion++;
      this.noteDeleteTarget = n;
    },

    noteCloseDelete() {
      if (this.noteDeleteSubmitting) return;
      this._noteDeleteVersion++;
      this.noteDeleteTarget = null;
    },

    async confirmDeleteNote() {
      var n = this.noteDeleteTarget;
      var current = this.selectedTicket?.notes?.find(note => note.id === n?.id);
      if (!current || !this.ticketCanEdit(this.selectedTicket) || !this.noteIsMine(current) || this.noteDeleteSubmitting) return;
      var owner = this._noteDraftOwner('Delete', null, n.id);
      var editVersion = this._noteEditVersion, replyVersion = this._noteReplyVersion;
      this.noteDeleteSubmitting = true;
      var ok = await this._noteRequest('/notes/' + encodeURIComponent(n.id), 'DELETE', null, 'Failed to delete note', owner);
      if (!this._noteDraftCurrent(owner)) return;
      this.noteDeleteSubmitting = false;
      if (ok) {
        this.noteDeleteTarget = null;
        if (this.noteEditOn === n.id && this._noteEditVersion === editVersion) this.noteCancelEdit();
        if (this.noteReplyOn === n.id && this._noteReplyVersion === replyVersion) this.noteCancelReply();
      }
    },

    // Whether you are on this chip. Drives its hue: yours read violet, everyone
    // else's read grey.
    noteReacted(chip) {
      var me = this.noteMe();
      return !!me && (chip?.actors || []).includes(me);
    },

    noteReactionHue(chip) {
      return this.noteReacted(chip) ? 'mauve' : 'none';
    },

    // The next emoji from the pool this note has no chip for, so the add button
    // does something visible until there is a picker.
    noteNextEmoji(n) {
      var used = new Set((n?.reactions || []).map(function (c) { return c.emoji; }));
      return this.noteEmojiPool.find(function (e) { return !used.has(e); }) || this.noteEmojiPool[0];
    },

    async toggleReaction(n, emoji) {
      if (!n || !emoji || !this.ticketCanEdit(this.selectedTicket)) return;
      var chip = (n.reactions || []).find(function (c) { return c.emoji === emoji; });
      var base = '/notes/' + encodeURIComponent(n.id) + '/reactions';
      if (this.noteReacted(chip)) {
        await this._noteRequest(base + '/' + encodeURIComponent(emoji), 'DELETE', null, 'Failed to drop reaction');
      } else {
        await this._noteRequest(base, 'POST', { emoji: emoji }, 'Failed to add reaction');
      }
    },

    // Serialize conversation writes per source-qualified ticket. Never replay
    // mutations; if a newer local snapshot overtakes an ack, re-read instead.
    async _noteRequest(path, method, body, failure, owner) {
      var t = this.selectedTicket;
      if (!this.ticketCanEdit(t)) return false;
      var id = this.ticketKey(t), epoch = this._noteDraftEpoch;
      var detail = this._ticketDetailSeq;
      var mine = () => this.ticketKey(this.selectedTicket) === id && epoch === this._noteDraftEpoch && detail === this._ticketDetailSeq;
      var previous = requests.get(id);
      var request = (async () => {
        if (previous) await previous;
        if (!mine() || !this.ticketCanEdit(this.selectedTicket)) return false;
        var feedback = () => mine() && (!owner || this._noteDraftCurrent(owner));
        return this._noteSend(t, path, method, body, failure, mine, feedback);
      })();
      requests.set(id, request);
      try { return await request; }
      finally { if (requests.get(id) === request) requests.delete(id); }
    },

    async _noteSend(t, path, method, body, failure, mine, feedback) {
      var id = this.ticketKey(t);
      var update = this._selectedTicketUpdateSeq;
      var acknowledged = this._editorSaves[id]?.ackSequence || 0;
      var snapshot = JSON.stringify(this.selectedTicket);
      var committed = false;
      var refreshFailure = 'Note saved, but the latest conversation could not be loaded. Reopen the ticket to refresh.';
      try {
        var init = { method: method };
        if (body) {
          init.headers = { 'Content-Type': 'application/json' };
          init.body = JSON.stringify(body);
        }
        var res = await fetch(this.ticketURL(t, path), init);
        if (!res.ok) {
          var err = await res.json().catch(function () { return {}; });
          if (feedback()) this.error = err.error || failure;
          return false;
        }
        var full = await res.json();
        full = { ...full, source: t.source || full.source };
        committed = true;
        if (!mine()) return true;
        if (this._noteApplyTicket(id, full, update, acknowledged, snapshot)) return true;
        // Re-read stale snapshots without replaying the successful write.
        for (var attempt = 0; attempt < 3 && mine(); attempt++) {
          update = this._selectedTicketUpdateSeq;
          acknowledged = this._editorSaves[id]?.ackSequence || 0;
          snapshot = JSON.stringify(this.selectedTicket);
          var fresh = await fetch(this.ticketURL(t));
          if (!mine()) return true;
          if (!fresh.ok) break;
          full = { ...await fresh.json(), source: t.source || full.source };
          if (!mine()) return true;
          if (this._noteApplyTicket(id, full, update, acknowledged, snapshot)) return true;
        }
        if (feedback()) this.error = refreshFailure;
        return true;
      } catch (e) {
        if (feedback()) this.error = committed ? refreshFailure : failure;
        return committed;
      }
    },

    _noteApplyTicket(id, full, update, acknowledged, snapshot) {
      var stale = (update !== undefined && update !== this._selectedTicketUpdateSeq) ||
        (acknowledged !== undefined && acknowledged !== (this._editorSaves[id]?.ackSequence || 0)) ||
        (snapshot !== undefined && snapshot !== JSON.stringify(this.selectedTicket));
      if (stale || this.ticketKey(this.selectedTicket) !== id) return false;
      if (this.ticketKey(this.selectedTicket) === id) {
        if (this.editing) {
          var description = this.descriptionText(this.selectedTicket);
          var tail = this.descriptionTail(full);
          this.selectedTicket = { ...this.selectedTicket, notes: full.notes,
            description: description, description_tail: tail, body: description + tail, revision: full.revision,
            can_edit: full.can_edit, can_claim: full.can_claim, refusal_reason: full.refusal_reason, claim_warnings: full.claim_warnings };
        } else {
          this.selectedTicket = full;
        }
        this._selectedTicketUpdateSeq++;
      }
      var entries = this.isSecondary(full) ? this.secondaryTickets : this.tickets;
      var idx = this.isSecondary(full) ? this.secondarySlot(full) : entries.findIndex(x => this.ticketKey(x) === id);
      if (idx >= 0 && !stale) {
        this._touchTicket(full);
        entries[idx] = this.boardEntry(full);
        this.recomputeBoard();
      }
      return true;
    },

    // Clear every open box. Called when the ticket page closes and when it
    // switches to another ticket, so a draft never reappears over a note it was
    // not written against.
    noteResetDrafts() {
      this._noteDraftEpoch++;
      drafts.clear();
      this.noteSubmitting = false;
      this._noteDeleteVersion++;
      this.noteHover = null;
      this.noteCancelReply();
      this.noteCancelEdit();
      this.noteReplySubmitting = false;
      this.noteEditSubmitting = false;
      this.noteDeleteTarget = null;
      this.noteDeleteSubmitting = false;
    },
  };
}
