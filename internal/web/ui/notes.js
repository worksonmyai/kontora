// The notes tab: the ticket's conversation. Notes live in the ticket body's
// "## Notes" section, so a stage prompt that interpolates the body carries
// them; reactions live in a sidecar beside the ticket file.
//
// Every key here is prefixed `note*`, because ui/detail.js and ui/activity.js
// already own the unprefixed ticket helpers and merge() throws on a repeat.
export function kontoraNotes() {
  return {
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
          var ok = h.exit_code === 0;
          rows.push({
            kind: 'event',
            key: 'e:' + i + ':end',
            at: h.completed_at,
            color: ok ? 'green' : 'rose',
            label: stage + (ok ? ' done' : ' failed'),
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
      var meta = 'exit ' + (h.exit_code || 0);
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
      if (!text || !this.selectedTicket || this.noteSubmitting) return;
      this.noteSubmitting = true;
      var ok = await this._noteRequest('/note', 'POST', { text: text }, 'Failed to add note');
      if (ok) this.noteDraft = '';
      this.noteSubmitting = false;
    },

    noteOpenReply(n) {
      this.noteEditOn = null;
      this.noteReplyOn = n.id;
      this.noteReplyDraft = '';
    },

    noteCancelReply() {
      this.noteReplyOn = null;
      this.noteReplyDraft = '';
    },

    async submitReply() {
      var text = (this.noteReplyDraft || '').trim();
      var parent = this.noteReplyOn;
      if (!text || !parent || this.noteReplySubmitting) return;
      this.noteReplySubmitting = true;
      var ok = await this._noteRequest('/note', 'POST', { text: text, parent: parent }, 'Failed to add reply');
      if (ok) this.noteCancelReply();
      this.noteReplySubmitting = false;
    },

    noteOpenEdit(n) {
      this.noteReplyOn = null;
      this.noteEditOn = n.id;
      this.noteEditDraft = n.text || '';
    },

    noteCancelEdit() {
      this.noteEditOn = null;
      this.noteEditDraft = '';
    },

    async saveNoteEdit() {
      var text = (this.noteEditDraft || '').trim();
      var id = this.noteEditOn;
      if (!text || !id || this.noteEditSubmitting) return;
      this.noteEditSubmitting = true;
      var ok = await this._noteRequest('/notes/' + encodeURIComponent(id), 'PATCH', { text: text }, 'Failed to edit note');
      if (ok) this.noteCancelEdit();
      this.noteEditSubmitting = false;
    },

    noteAskDelete(n) {
      this.noteDeleteTarget = n;
    },

    noteCloseDelete() {
      if (this.noteDeleteSubmitting) return;
      this.noteDeleteTarget = null;
    },

    async confirmDeleteNote() {
      var n = this.noteDeleteTarget;
      if (!n || this.noteDeleteSubmitting) return;
      this.noteDeleteSubmitting = true;
      var ok = await this._noteRequest('/notes/' + encodeURIComponent(n.id), 'DELETE', null, 'Failed to delete note');
      this.noteDeleteSubmitting = false;
      if (ok) {
        this.noteDeleteTarget = null;
        if (this.noteEditOn === n.id) this.noteCancelEdit();
        if (this.noteReplyOn === n.id) this.noteCancelReply();
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
      if (!n || !emoji) return;
      var chip = (n.reactions || []).find(function (c) { return c.emoji === emoji; });
      var base = '/notes/' + encodeURIComponent(n.id) + '/reactions';
      if (this.noteReacted(chip)) {
        await this._noteRequest(base + '/' + encodeURIComponent(emoji), 'DELETE', null, 'Failed to drop reaction');
      } else {
        await this._noteRequest(base, 'POST', { emoji: emoji }, 'Failed to add reaction');
      }
    },

    // One request against a ticket's note routes. Every one of them answers
    // with the whole ticket, so the page replaces its copy rather than
    // reconciling: the reply, the edit and the reaction all land the same way.
    async _noteRequest(path, method, body, failure) {
      var t = this.selectedTicket;
      if (!t) return false;
      var id = t.id;
      try {
        var init = { method: method };
        if (body) {
          init.headers = { 'Content-Type': 'application/json' };
          init.body = JSON.stringify(body);
        }
        var res = await fetch('/api/tickets/' + encodeURIComponent(id) + path, init);
        if (!res.ok) {
          var err = await res.json().catch(function () { return {}; });
          this.error = err.error || failure;
          return false;
        }
        var full = await res.json();
        this._noteApplyTicket(id, full);
        return true;
      } catch (e) {
        this.error = failure;
        return false;
      }
    },

    // Replace the open ticket and its board row with what the mutation returned.
    // While the ticket tab is editing, only the notes are taken: the rest of the
    // payload would overwrite the fields being typed into.
    _noteApplyTicket(id, full) {
      if (this.selectedTicket && this.selectedTicket.id === id) {
        if (this.editing) {
          this.selectedTicket.notes = full.notes;
        } else {
          this.selectedTicket = full;
        }
      }
      var idx = this.tickets.findIndex(function (x) { return x.id === id; });
      if (idx >= 0) {
        this.tickets[idx] = this.boardEntry(full);
        this.recomputeBoard();
      }
    },

    // Clear every open box. Called when the ticket page closes and when it
    // switches to another ticket, so a draft never reappears over a note it was
    // not written against.
    noteResetDrafts() {
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
