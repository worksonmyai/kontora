// The notes tab: the ticket's conversation. Notes live in the ticket body's
// "## Notes" section, so a stage prompt that interpolates the body carries
// them; reactions live in a sidecar beside the ticket file.
//
// Every key here is prefixed `note*`, because ui/detail.js and ui/activity.js
// already own the unprefixed ticket helpers and merge() throws on a repeat.
export function kontoraNotes() {
  const requests = new Map();
  const drafts = new Map();
  const snapshots = new Map();
  const targets = new Map();
  const editors = new Map();
  let recoveryID = 0;
  let targetID = 0;
  const legacy = id => /^#\d+$/.test(id || '');
  const signature = n => JSON.stringify([n.text || '', n.at || '', n.author || '', n.author_kind || '']);
  return {
    noteRecoveries: [],
    _noteRecoveryRevision: 0,
    _noteDraftEpoch: 0,
    _noteReplyVersion: 0,
    _noteEditVersion: 0,
    _noteDeleteVersion: 0,

    _noteDraftOwner(kind, text, target = null) {
      var owner = { key: this.ticketKey(this.selectedTicket), epoch: this._noteDraftEpoch, detail: this._ticketDetailSeq,
        kind, text, target, targetRef: target ? this._noteTrackTarget(this.selectedTicket, target) : null,
        pending: true, version: this['_note' + kind + 'Version'] };
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

    // ---- ownership and legacy identity ------------------------------------

    _noteTrackTarget(ticket, id) {
      var key = this.ticketKey(ticket);
      if (!snapshots.has(key)) snapshots.set(key, (ticket.notes || []).map(n => ({ ...n })));
      var ref = { token: ++targetID, id, unresolved: false, pending: 0 };
      if (!targets.has(key)) targets.set(key, new Set());
      targets.get(key).add(ref);
      return ref;
    },

    // #N is a parse-time position, never an identity. Accepted snapshots match
    // unchanged content/byline uniquely between surviving stable-note anchors.
    _noteReconcileTicket(full, unavailable = false) {
      if (!Array.isArray(full?.notes)) return;
      var key = this.ticketKey(full), before = snapshots.get(key);
      if (!before) return;
      var after = full.notes;
      var common = before.filter(n => !legacy(n.id) && before.filter(x => x.id === n.id).length === 1 && after.filter(x => x.id === n.id).length === 1);
      var anchorPositions = common.map(n => after.findIndex(x => x.id === n.id));
      var ordered = anchorPositions.every((position, i) => i === 0 || position > anchorPositions[i - 1]);
      var remap = id => {
        if ((unavailable || !ordered) && legacy(id)) return null;
        if (!legacy(id)) return after.filter(n => n.id === id).length === 1 ? id : null;
        var index = before.findIndex(n => n.id === id);
        if (index < 0) return null;
        var left = common.filter(n => before.indexOf(n) < index).at(-1);
        var right = common.find(n => before.indexOf(n) > index);
        var lo = left ? after.findIndex(n => n.id === left.id) : -1;
        var hi = right ? after.findIndex(n => n.id === right.id) : after.length;
        if (lo >= hi) return null;
        var fingerprint = signature(before[index]);
        var oldLo = left ? before.indexOf(left) : -1;
        var oldHi = right ? before.indexOf(right) : before.length;
        var oldMatches = before.slice(oldLo + 1, oldHi).filter(n => signature(n) === fingerprint);
        var matches = after.slice(lo + 1, hi).filter(n => signature(n) === fingerprint && !common.some(a => a.id === n.id));
        return oldMatches.length === 1 && matches.length === 1 ? matches[0].id : null;
      };
      var oldIDs = new Map();
      for (var ref of targets.get(key) || []) {
        oldIDs.set(ref, ref.id);
        if (ref.unresolved) continue;
        var next = remap(ref.id);
        if (next) ref.id = next;
        else { ref.id = null; ref.unresolved = true; }
      }
      snapshots.set(key, after.map(n => ({ ...n })));
      for (var [kind, editor] of editors) {
        if (editor.key !== key || key !== this.ticketKey(this.selectedTicket)) continue;
        var field = 'note' + kind + 'On';
        var current = kind === 'Delete' ? this.noteDeleteTarget?.id : this[field];
        if (current !== oldIDs.get(editor.ref)) continue;
        if (editor.ref.unresolved) {
          var text = kind === 'Delete' ? this.noteDeleteTarget?.text : this['note' + kind + 'Draft'];
          var owner = drafts.get(kind);
          if (!(owner?.pending && this._noteDraftCurrent(owner) && oldIDs.get(owner.targetRef) === oldIDs.get(editor.ref) && owner.text === text)) {
            this._noteRetain(key, kind, text, editor.ref, 'The original note cannot be identified uniquely. Choose the intended note again.');
          }
          if (kind === 'Delete') { this._noteDeleteVersion++; this.noteDeleteTarget = null; this.noteDeleteSubmitting = false; }
          else this['noteCancel' + kind]();
        } else if (kind === 'Delete') this.noteDeleteTarget = after.find(n => n.id === editor.ref.id);
        else this[field] = editor.ref.id;
      }
      this._noteRecoveryRevision++;
    },

    _notePruneTargets() {
      // Full note text is retained only while an editor, command or recovery
      // needs correspondence. Board/navigation history must not pin it forever.
      var live = new Set(this.noteRecoveries.map(r => r.ref?.token));
      for (var editor of editors.values()) live.add(editor.ref.token);
      for (var [key, refs] of targets) {
        for (var ref of refs) if (!ref.pending && !live.has(ref.token)) refs.delete(ref);
        if (!refs.size) { targets.delete(key); snapshots.delete(key); }
      }
    },

    _noteRetain(key, kind, text, ref, reason) {
      if (this.noteRecoveries.some(r => r.key === key && r.kind === kind && r.text === (text || '') &&
          r.ref?.id === ref?.id && !!r.ref?.unresolved === !!ref?.unresolved)) return;
      this.noteRecoveries.push({ id: ++recoveryID, key, kind, text: text || '', ref, reason });
    },

    noteRecoveryItems() {
      void this._noteRecoveryRevision;
      var key = this.ticketKey(this.selectedTicket);
      return this.noteRecoveries.filter(r => r.key === key).map(r => ({ ...r, target: r.ref?.id, unresolved: !!r.ref?.unresolved }));
    },

    noteDismissRecovery(id) {
      this.noteRecoveries = this.noteRecoveries.filter(r => r.id !== id);
      this._notePruneTargets();
    },

    // Restoring is explicit and never sends a mutation. Uncertain writes must
    // be checked in the feed before the user chooses whether to submit again.
    noteRestoreRecovery(id) {
      var r = this.noteRecoveryItems().find(r => r.id === id);
      if (!r || r.unresolved || !this.ticketCanEdit(this.selectedTicket)) return;
      if (r.kind === 'Add') {
        if (this.noteDraft) return;
        this.noteDraft = r.text;
      } else if (r.kind === 'Reply' || r.kind === 'Edit') {
        if (this.noteReplyDraft || this.noteEditDraft) return;
        var n = this.selectedTicket.notes?.find(n => n.id === r.target);
        if (!n || (r.kind === 'Edit' && !this.noteIsMine(n))) return;
        this['noteOpen' + r.kind](n);
        this['note' + r.kind + 'Draft'] = r.text;
      } else return;
      this.noteDismissRecovery(id);
    },

    _noteHasTail(key) {
      return requests.has(key);
    },

    async _noteWaitForTail(key) {
      while (requests.has(key)) await requests.get(key);
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
      editors.set('Reply', { key: this.ticketKey(this.selectedTicket), ref: this._noteTrackTarget(this.selectedTicket, n.id) });
      this.noteReplyDraft = '';
    },

    noteCancelReply() {
      this._noteReplyVersion++;
      this.noteReplySubmitting = false;
      this.noteReplyOn = null;
      editors.delete('Reply');
      this.noteReplyDraft = '';
      this._notePruneTargets();
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
      if (ok && this.noteReplyOn === owner.targetRef.id && this.noteReplyDraft === owner.text) this.noteCancelReply();
    },

    noteOpenEdit(n) {
      if (!this.ticketCanEdit(this.selectedTicket) || !this.noteIsMine(n)) return;
      this.noteCancelReply();
      this._noteEditVersion++;
      this.noteEditSubmitting = false;
      this.noteEditOn = n.id;
      editors.set('Edit', { key: this.ticketKey(this.selectedTicket), ref: this._noteTrackTarget(this.selectedTicket, n.id), initialText: n.text || '' });
      this.noteEditDraft = n.text || '';
    },

    noteCancelEdit() {
      this._noteEditVersion++;
      this.noteEditSubmitting = false;
      this.noteEditOn = null;
      editors.delete('Edit');
      this.noteEditDraft = '';
      this._notePruneTargets();
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
      if (ok && this.noteEditOn === owner.targetRef.id && this.noteEditDraft === owner.text) this.noteCancelEdit();
    },

    noteAskDelete(n) {
      if (!this.ticketCanEdit(this.selectedTicket) || !this.noteIsMine(n)) return;
      this._noteDeleteVersion++;
      this.noteDeleteTarget = n;
      editors.set('Delete', { key: this.ticketKey(this.selectedTicket), ref: this._noteTrackTarget(this.selectedTicket, n.id) });
    },

    noteCloseDelete() {
      if (this.noteDeleteSubmitting) return;
      this._noteDeleteVersion++;
      this.noteDeleteTarget = null;
      editors.delete('Delete');
      this._notePruneTargets();
    },

    async confirmDeleteNote() {
      var n = this.noteDeleteTarget;
      var current = this.selectedTicket?.notes?.find(note => note.id === n?.id);
      if (!current || !this.ticketCanEdit(this.selectedTicket) || !this.noteIsMine(current) || this.noteDeleteSubmitting) return;
      var owner = this._noteDraftOwner('Delete', n.text || '', n.id);
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
        await this._noteRequest(base + '/' + encodeURIComponent(emoji), 'DELETE', null, 'Failed to drop reaction', null, n.id);
      } else {
        await this._noteRequest(base, 'POST', { emoji: emoji }, 'Failed to add reaction', null, n.id);
      }
    },

    // Serialize conversation writes per source-qualified ticket. Never replay
    // mutations; if a newer local snapshot overtakes an ack, re-read instead.
    async _noteRequest(path, method, body, failure, owner, target) {
      var t = this.selectedTicket;
      if (!this.ticketCanEdit(t)) return false;
      t = { ...t };
      body = body ? { ...body } : null;
      var id = this.ticketKey(t), epoch = this._noteDraftEpoch;
      var detail = this._ticketDetailSeq;
      var ref = owner?.targetRef || (target ? this._noteTrackTarget(t, target) : null);
      if (ref) ref.pending++;
      var originalTarget = owner?.target || target;
      var kind = owner?.kind || 'Reaction';
      var mine = () => this.ticketKey(this.selectedTicket) === id && epoch === this._noteDraftEpoch && detail === this._ticketDetailSeq;
      var feedback = () => mine() && (!owner || this._noteDraftCurrent(owner));
      var retain = reason => this._noteRetain(id, kind, owner?.text || body?.emoji || '', ref, reason);
      var previous = requests.get(id);
      var request = (async () => {
        if (previous) await previous;
        // Accepted commands outlive selection; the server checks captured-ticket permissions.
        if (ref?.unresolved) {
          retain('Not sent: the original note cannot be identified uniquely. Choose the intended note again.');
          return false;
        }
        var resolvedPath = path;
        if (ref) {
          if (body?.parent) body.parent = ref.id;
          else resolvedPath = path.replace('/notes/' + encodeURIComponent(originalTarget), '/notes/' + encodeURIComponent(ref.id));
        }
        return this._noteSend(t, resolvedPath, method, body, failure, mine, feedback, retain);
      })();
      requests.set(id, request);
      try { return await request; }
      finally {
        if (owner) owner.pending = false;
        if (ref) ref.pending--;
        if (requests.get(id) === request) requests.delete(id);
        this._notePruneTargets();
      }
    },

    async _noteSend(t, path, method, body, failure, mine, feedback, retain) {
      var id = this.ticketKey(t);
      var update = this._selectedTicketUpdateSeq;
      var acknowledged = this._editorAckVersion(id);
      var snapshot = JSON.stringify(this.selectedTicket);
      var targetSnapshot = snapshots.get(id);
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
          var reason = err.error || failure;
          retain(reason);
          if (feedback()) this.error = reason;
          return false;
        }
        // A successful status confirms the mutation even if its body is lost.
        committed = true;
        var full = await res.json();
        if (full.id !== t.id || (full.source && full.source !== (t.source || 'primary'))) {
          committed = false;
          throw new Error('Invalid note acknowledgement');
        }
        full = { ...full, source: t.source || full.source };
        if (!mine()) {
          // With no selected owner, the accepted ack can still resolve the next
          // queued command. Never do so over a snapshot that overtook it.
          if (snapshots.get(id) === targetSnapshot) this._noteReconcileTicket(full);
          else await this._noteRefreshTargets(t);
          return true;
        }
        if (this._noteApplyTicket(id, full, update, acknowledged, snapshot)) return true;
        // Re-read stale snapshots without replaying the successful write.
        for (var attempt = 0; attempt < 3; attempt++) {
          update = this._selectedTicketUpdateSeq;
          acknowledged = this._editorAckVersion(id);
          snapshot = JSON.stringify(this.selectedTicket);
          targetSnapshot = snapshots.get(id);
          var fresh = await fetch(this.ticketURL(t));
          if (!fresh.ok) break;
          full = await fresh.json();
          if (full.id !== t.id || (full.source && full.source !== (t.source || 'primary'))) break;
          full = { ...full, source: t.source || full.source };
          if (mine() && this._noteApplyTicket(id, full, update, acknowledged, snapshot)) return true;
          if (!mine() && snapshots.get(id) === targetSnapshot) {
            this._noteReconcileTicket(full);
            return true;
          }
        }
        if (snapshots.has(id)) this._noteReconcileTicket({ ...t, notes: snapshots.get(id) }, true);
        if (feedback()) this.error = refreshFailure;
        return true;
      } catch (e) {
        // A lost ack/read cannot authorize use of an old positional reference.
        if (snapshots.has(id)) this._noteReconcileTicket({ ...t, notes: snapshots.get(id) }, true);
        if (!committed) retain('Outcome uncertain: ' + failure + '. Check the conversation before submitting again.');
        if (feedback()) this.error = committed ? refreshFailure : failure;
        return committed;
      }
    },

    async _noteRefreshTargets(ticket) {
      var key = this.ticketKey(ticket);
      for (var attempt = 0; attempt < 3; attempt++) {
        var snapshot = snapshots.get(key);
        var res = await fetch(this.ticketURL(ticket));
        if (!res.ok) throw new Error('Conversation refresh failed');
        var full = await res.json();
        if (full.id !== ticket.id || (full.source && full.source !== (ticket.source || 'primary'))) throw new Error('Invalid conversation response');
        if (snapshot !== snapshots.get(key)) continue;
        this._noteReconcileTicket({ ...full, source: ticket.source || full.source });
        return;
      }
      throw new Error('Conversation refresh overtaken');
    },

    _noteApplyTicket(id, full, update, acknowledged, snapshot) {
      var stale = (update !== undefined && update !== this._selectedTicketUpdateSeq) ||
        (acknowledged !== undefined && acknowledged !== this._editorAckVersion(id)) ||
        (snapshot !== undefined && snapshot !== JSON.stringify(this.selectedTicket));
      if (stale || this.ticketKey(this.selectedTicket) !== id || this.ticketKey(full) !== id) return false;
      this._noteReconcileTicket(full);
      if (this.ticketKey(this.selectedTicket) === id) {
        if (this.editing) {
          var description = this.descriptionText(this.selectedTicket);
          var tail = this.descriptionTail(full);
          // Claim must use the revision of the description the user reviewed.
          var revision = this.isSecondary(this.selectedTicket) && description !== this.descriptionText(full)
            ? this.selectedTicket.revision : full.revision;
          this.selectedTicket = { ...this.selectedTicket, notes: full.notes,
            description: description, description_tail: tail, body: description + tail, revision: revision,
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

    // Retain unsent/newer text before releasing the visible editor. Accepted
    // text is retained by its command on failure, not duplicated on navigation.
    noteResetDrafts() {
      var key = this.ticketKey(this.selectedTicket);
      if (key) for (var kind of ['Add', 'Reply', 'Edit']) {
        var text = this[kind === 'Add' ? 'noteDraft' : 'note' + kind + 'Draft'];
        var owner = drafts.get(kind);
        if (text && !(kind === 'Edit' && text === editors.get(kind)?.initialText) &&
            !(owner?.pending && this._noteDraftCurrent(owner) && owner.text === text &&
              (!owner.targetRef || owner.targetRef.id === editors.get(kind)?.ref.id))) {
          this._noteRetain(key, kind, text, editors.get(kind)?.ref || null, 'Unsent draft retained from this ticket.');
        }
      }
      this.noteDraft = '';
      editors.clear();
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
