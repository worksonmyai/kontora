// The notify row: which statuses ping, and which channels hear them. One
// implementation behind two surfaces — the init modal's row and the details
// rail's popover — so every method takes the form it edits rather than reading
// a fixed one. A form is `{ notify: [], notifyChannels: [], path }`; path is
// what resolves the channel, and in the modal it is still being typed.

// The three answers the row offers. `custom` is not among them: it is what the
// statuses read as when they match none of these, not a fourth thing to pick.
export const NOTIFY_MODES = [
  { key: 'off', label: 'off', statuses: [] },
  { key: 'needs', label: 'when it needs me', statuses: ['paused', 'human_review', 'waiting'] },
  { key: 'done', label: 'when it is finished', statuses: ['human_review', 'done'] },
];

const NOTIFY_ROUTE_ERROR = 'Choose a notification channel, configure an inherited route, or turn notifications off.';

// Chip order: what the two modes write first, then the rest of the board. The
// hues come from .notify-chip[data-status] in index.html, so a status with no
// rule (todo, a custom one) simply lifts to the surface tone.
const NOTIFY_STATUSES = [
  'paused', 'human_review', 'done', 'waiting',
  'open', 'todo', 'in_progress', 'cancelled',
];

// The channel list that silences a ticket. config.NoneSentinel, spelled here
// because the browser never sees the Go constant.
const NOTIFY_SILENCE = 'none';

function sameSet(a, b) {
  return a.length === b.length && a.every(x => b.includes(x));
}

export function notifyModeFor(statuses) {
  var mode = NOTIFY_MODES.find(m => sameSet(m.statuses, statuses || []));
  return mode ? mode.key : 'custom';
}

export function kontoraNotify() {
  return {
    _notifySaves: Object.create(null),
    _notifyEditorEpoch: 0,
    _notifySubmission: 0,
    _notifyDraftInitial: '',

    _notifySignature(form) {
      return JSON.stringify([form?.notify || [], form?.notifyChannels || [], !!form?.notifyUnset]);
    },

    _notifyOwns(owner) {
      return owner.epoch === this._notifyEditorEpoch && owner.detail === this._ticketDetailSeq &&
        owner.key === this.ticketKey(this.selectedTicket);
    },
    notifyModes() {
      return NOTIFY_MODES;
    },

    notifyHasChannels() {
      return (this.configCache?.channels || []).length > 0;
    },

    // Every status a chip is offered for: the board's, the configured custom
    // ones, and the pseudo-status `waiting`, which is in NOTIFY_STATUSES
    // because a blocked agent is the thing people most want to hear about.
    notifyStatuses() {
      var custom = (this.configCache?.custom_statuses || []).filter(s => s !== NOTIFY_SILENCE && !NOTIFY_STATUSES.includes(s));
      return NOTIFY_STATUSES.concat(custom);
    },

    // Which of the three answers a status list reads as, or 'custom'. Derived
    // rather than stored: a mode kept beside the list it describes goes stale
    // the moment a chip is toggled, and then the control claims one thing while
    // the echo line below it says another.
    notifyModeOf(form) {
      return notifyModeFor(form && form.notify);
    },

    // A mode replaces the status list outright. The channels are left alone, so
    // turning notifications off and on again does not re-ask where they go.
    notifyPickMode(form, key) {
      var mode = NOTIFY_MODES.find(m => m.key === key);
      if (!mode) return;
      form.notify = mode.statuses.slice();
      form.notifyFollowsDefaults = false;
      form.notifyUnset = false;
      this.notifyChanged(form);
    },

    notifyStatusOn(form, status) {
      return ((form && form.notify) || []).includes(status);
    },

    notifyToggleStatus(form, status) {
      form.notify = this.notifyStatusOn(form, status)
        ? form.notify.filter(s => s !== status)
        : (form.notify || []).concat([status]);
      form.notifyFollowsDefaults = false;
      form.notifyUnset = false;
      this.notifyChanged(form);
    },

    // The route follows config.NotifyChannelsFor, but keeps its source and an
    // explicit silence so the form can explain an empty result accurately.
    notifyRoute(form) {
      var project = this.projectForPath(form && form.path);
      var choices = [
        { channels: (form && form.notifyChannels) || [], source: 'ticket' },
      ];
      if (project) choices.push({ channels: project.notify_channels || [], source: 'project ' + project.name });
      choices.push({ channels: this.configCache?.default_channels || [], source: 'notifications.default' });

      var route = choices.find(c => c.channels.length) || { channels: [], source: '' };
      var silenced = route.channels.includes(NOTIFY_SILENCE);
      return {
        channels: silenced ? [] : route.channels.slice(),
        silenced,
        source: route.source,
      };
    },

    notifyResolve(form) {
      return this.notifyRoute(form).channels;
    },

    // The `→ tg` line beside the segmented control. Empty while no status is
    // picked, because there is then nothing to route. A ticket that asks for a
    // status and resolves nowhere says so: it is the likeliest way to configure
    // this and hear nothing, and the daemon's warning about it only reaches the
    // log.
    notifyWhere(form) {
      if (!((form && form.notify) || []).length) return '';
      var channels = this.notifyResolve(form);
      return '→ ' + (channels.length ? channels.join(', ') : 'no channel');
    },

    notifyRouteError(form) {
      var statuses = (form && form.notify) || [];
      var route = this.notifyRoute(form);
      if (!statuses.length || route.silenced || route.channels.length) return '';
      return NOTIFY_ROUTE_ERROR;
    },

    // The YAML this row is about to write, in the spelling the file gets. Same
    // convention as the new-ticket page's frontmatter preview.
    notifyEcho(form) {
      var list = (form && form.notify) || [];
      if (form?.notifyUnset) return 'no notify: field written';
      if (form?.notifyFollowsDefaults) return 'notify: resolved from defaults at start';
      return 'notify: [' + list.join(', ') + ']';
    },

    // The rail's one-line value. The two modes are named; anything else spells
    // its statuses out, because a list nobody can read back is a list nobody
    // can trust.
    notifyLabel(form) {
      var list = (form && form.notify) || [];
      if (!list.length) return 'off';
      var mode = this.notifyModeOf(form);
      var what = mode === 'needs' ? 'when it needs me'
        : mode === 'done' ? 'when finished'
        : list.join(', ');
      return what + ' ' + this.notifyWhere(form);
    },

    // An unrouted notification keeps the silence option available even when no
    // delivery channel exists.
    notifyPicksChannel(form) {
      var configured = this.configCache?.channels || [];
      if (configured.length > 1) return true;
      var own = (form && form.notifyChannels) || [];
      var active = ((form && form.notify) || []).length > 0;
      return own.length > 0 || (active && this.notifyResolve(form).length === 0);
    },

    // The channel chips, in the same vocabulary as the status ones: what the
    // ticket inherits, one chip per configured channel, and the opt-out.
    notifyChannelChips(form) {
      var own = (form && form.notifyChannels) || [];
      var silenced = own.includes(NOTIFY_SILENCE);
      var inherited = this.notifyResolve({ path: form && form.path, notifyChannels: [] });
      var chips = [{
        key: 'inherit',
        label: inherited.length ? 'inherit · ' + inherited.join(', ') : 'inherit · nothing',
        kind: 'inherit',
        on: !own.length,
      }];
      (this.configCache?.channels || []).forEach(name => {
        chips.push({ key: name, label: name, kind: 'channel', on: own.includes(name) });
      });
      chips.push({ key: NOTIFY_SILENCE, label: 'silence', kind: 'silence', on: silenced });
      return chips;
    },

    // Picking an explicit channel drops the inherit; unpicking the last one
    // falls back to it, so the row cannot reach a state that names nothing and
    // inherits nothing either.
    notifyToggleChannel(form, chip) {
      var own = (form.notifyChannels || []).filter(c => c !== NOTIFY_SILENCE);
      if (chip.kind === 'inherit') {
        form.notifyChannels = [];
      } else if (chip.kind === 'silence') {
        form.notifyChannels = chip.on ? [] : [NOTIFY_SILENCE];
      } else {
        form.notifyChannels = own.includes(chip.key) ? own.filter(c => c !== chip.key) : own.concat([chip.key]);
      }
      this.notifyChanged(form);
    },

    // One line under the channel row saying what this ticket will actually do.
    // The states are exclusive and this is their order of importance: a ticket
    // that names no status is silent however its channels are set.
    notifyChannelHint(form) {
      var own = (form && form.notifyChannels) || [];
      if (!((form && form.notify) || []).length) {
        return 'Silent. Only a transition the daemon decided on its own ever sends — your own drags and CLI moves never do.';
      }
      if (own.includes(NOTIFY_SILENCE)) {
        return 'Silenced for this ticket. The status list is kept, so removing the silence resumes it.';
      }
      var inherited = this.notifyRoute({ path: form && form.path, notifyChannels: [] });
      if (own.length) {
        if (!inherited.source) return 'Set for this ticket because no project or default route applies.';
        return 'Overrides ' + inherited.source + ' for this ticket. The same channel twice is one message.';
      }
      if (inherited.silenced) {
        return 'Inherited: silenced by ' + inherited.source + '. Pick a channel to override it for this ticket alone.';
      }
      if (!inherited.channels.length) {
        return 'Inherited: nothing is configured to receive it. Set notifications.default or the project\'s notify_channels.';
      }
      return 'Inherited: resolves to ' + inherited.channels.join(', ') + ', from ' + inherited.source + '. Pick a channel to override it for this ticket alone.';
    },

    // The autosave flag distinguishes the rail draft from the init form because
    // Alpine's proxies make object identity unreliable here.
    notifyChanged(form) {
      if (form && form.autosave) {
        this.saveNotify();
      } else {
        this.initError = null;
      }
    },

    // ─── the details rail row ───

    // Editing notify writes the ticket file, so it is offered exactly where the
    // API accepts a frontmatter edit — config.StatusAllowsEdit, which is open,
    // todo, paused, human_review and the custom statuses. A running ticket
    // shows the row and does not open it: an agent owns the file, and the write
    // would be lost when the run puts it back.
    notifyCanEdit() {
      if (!this.ticketCanEdit(this.selectedTicket)) return false;
      if (this.isSecondary(this.selectedTicket)) return true;
      if (this.executionActive(this.selectedTicket)) return false;
      var status = this.selectedTicket?.status;
      if (!status) return false;
      return ['open', 'todo', 'paused', 'human_review']
        .concat(this.configCache?.custom_statuses || [])
        .includes(status);
    },

    // The rail's read value, built from the open ticket rather than a draft, so
    // it follows an edit made anywhere else.
    notifyRailLabel() {
      return this.notifyLabel({
        notify: this.selectedTicket?.notify || [],
        notifyChannels: this.selectedTicket?.notify_channels || [],
        path: this.selectedTicket?.path || '',
      });
    },

    openNotifyEditor() {
      if (!this.notifyCanEdit()) return;
      this._notifyEditorEpoch++;
      this.notifySaving = false;
      this.notifySaved = false;
      this.notifyDraft = {
        notify: (this.selectedTicket.notify || []).slice(),
        notifyUnset: this.selectedTicket.notify == null,
        notifyChannels: (this.selectedTicket.notify_channels || []).slice(),
        path: this.selectedTicket.path || '',
        autosave: true,
      };
      this._notifyDraftInitial = this._notifySignature(this.notifyDraft);
      this.notifyError = null;
      this.notifyChips = false;
      this.notifyEditing = true;
    },

    closeNotifyEditor() {
      this._notifyEditorEpoch++;
      this.notifyEditing = false;
      this.notifySaving = false;
      this.notifySaved = false;
      this.notifyError = null;
    },

    // A channel edit must not turn an unset status preference into Off.
    async saveNotify() {
      var t = this.selectedTicket;
      if (!this.notifyCanEdit() || !this.notifyDraft) return false;
      this.notifyError = null;
      this.notifySaved = false;
      var routeError = this.notifyRouteError(this.notifyDraft);
      if (routeError) {
        this.notifyError = routeError;
        return false;
      }
      var key = this.ticketKey(t);
      var state = this._notifySaves[key] || (this._notifySaves[key] = { pending: null, promise: null, ok: true, ack: null, failed: null });
      var owner = { key, epoch: this._notifyEditorEpoch, detail: this._ticketDetailSeq,
        submission: ++this._notifySubmission, signature: this._notifySignature(this.notifyDraft) };
      state.pending = { t, owner, payload: {
        notify: this.notifyDraft.notifyUnset ? undefined : (this.notifyDraft.notify || []).slice(),
        notify_channels: (this.notifyDraft.notifyChannels || []).slice(),
      } };
      this.notifySaving = true;
      if (!state.promise) {
        state.promise = this._notifyDrain(state).finally(() => { state.promise = null; });
      }
      return state.promise;
    },

    async _notifyDrain(state) {
      while (state.pending) {
        const request = state.pending;
        state.pending = null;
        const { t, owner, payload } = request;
        const current = () => this._notifyOwns(owner) && owner.submission === this._notifySubmission &&
          owner.signature === this._notifySignature(this.notifyDraft);
        // Permission can be revoked while another notify write is pending.
        if (this._notifyOwns(owner) && !this.notifyCanEdit()) {
          state.ok = false;
          state.failed = owner.signature;
          state.pending = null;
          if (current()) {
            this.notifySaving = false;
            this.notifyError = 'Notifications can no longer be edited.';
          }
          break;
        }
        var before = this.sameTicket(this.selectedTicket, t) ? this.selectedTicket : t;
        try {
          var res = await fetch(this.ticketURL(t), {
            method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload),
          });
          if (!res.ok) {
            var data = await res.json().catch(() => ({}));
            throw new Error(data.error || 'Failed to save notify');
          }
          var updated = await res.json();
          // The endpoint returns a whole snapshot. Only acknowledge our fields;
          // even a fresh notification ack has no authority over notes or status.
          var fields = { notify: updated.notify || [], notify_channels: updated.notify_channels || [] };
          var entries = this.isSecondary(t) ? this.secondaryTickets : this.tickets;
          var idx = entries.findIndex(x => this.sameTicket(x, t));
          var unchanged = ticket => JSON.stringify([ticket?.notify || [], ticket?.notify_channels || []]) === JSON.stringify([before.notify || [], before.notify_channels || []]);
          if (idx >= 0 && unchanged(entries[idx])) entries[idx] = { ...entries[idx], ...fields };
          if (this.sameTicket(this.selectedTicket, t) && unchanged(this.selectedTicket)) {
            // A secondary revision authorizes the displayed document. Keep it
            // until a full, owned read rather than endorsing an unseen snapshot.
            this.selectedTicket = { ...this.selectedTicket, ...fields };
            this._selectedTicketUpdateSeq++;
          }
          state.ok = true;
          state.ack = owner.signature;
          state.failed = null;
          if (current() && !state.pending) {
            this.notifySaved = true;
            this.notifyError = null;
            setTimeout(() => { if (current() && state.ack === owner.signature) this.notifySaved = false; }, 1500);
          }
        } catch (e) {
          state.ok = false;
          const queued = state.pending;
          state.failed = queued?.owner.signature || owner.signature;
          if (current()) this.notifyError = e.message || 'Failed to save notify';
          else if (queued && this._notifyOwns(queued.owner) && queued.owner.submission === this._notifySubmission &&
              queued.owner.signature === this._notifySignature(this.notifyDraft)) {
            this.notifyError = 'Notification save failed. The newer draft has not been sent; save again to retry.';
          }
          // Keep the draft. No automatic mutation retry, including queued writes
          // after an uncertain network failure.
          state.pending = null;
        }
        if (this._notifyOwns(owner) && !state.pending) this.notifySaving = false;
      }
      return state.ok;
    },

    // Lifecycle barrier: drain the latest draft, not just the current promise.
    // A failed unchanged draft blocks execution without replaying a mutation.
    async flushNotifySave(ticket = this.selectedTicket) {
      var key = typeof ticket === 'string' ? ticket : this.ticketKey(ticket);
      if (!key) return true;
      for (;;) {
        var state = this._notifySaves[key];
        if (state?.promise) { await state.promise; continue; }
        if (state?.ok === false) return false;
        if (this.ticketKey(this.selectedTicket) === key && this.notifyEditing && this.notifyDraft) {
          var signature = this._notifySignature(this.notifyDraft);
          if (this.notifyRouteError(this.notifyDraft)) return false;
          if (state?.failed === signature) return false;
          if (signature !== (state?.ack ?? this._notifyDraftInitial)) {
            if (!await this.saveNotify()) return false;
            continue;
          }
        }
        return state?.ok !== false;
      }
    },
  };
}
