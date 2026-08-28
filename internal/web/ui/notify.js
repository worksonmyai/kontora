// The notify row: which statuses ping, and which channels hear them. One
// implementation behind two surfaces — the init modal's row and the details
// rail's popover — so every method takes the form it edits rather than reading
// a fixed one. A form is `{ notify: [], notifyChannels: [], path }`; path is
// what resolves the channel, and in the modal it is still being typed.

// The three answers the row offers. `custom` is not among them: it is what the
// statuses read as when they match none of these, not a fourth thing to pick.
const NOTIFY_MODES = [
  { key: 'off', label: 'off', statuses: [] },
  { key: 'needs', label: 'when it needs me', statuses: ['paused', 'human_review', 'waiting'] },
  { key: 'done', label: 'when it is finished', statuses: ['done'] },
];

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

export function kontoraNotify() {
  return {
    notifyModes() {
      return NOTIFY_MODES;
    },

    // Every status a chip is offered for: the board's, the configured custom
    // ones, and the pseudo-status `waiting`, which is in NOTIFY_STATUSES
    // because a blocked agent is the thing people most want to hear about.
    notifyStatuses() {
      var custom = (this.configCache?.custom_statuses || []).filter(s => !NOTIFY_STATUSES.includes(s));
      return NOTIFY_STATUSES.concat(custom);
    },

    // Which of the three answers a status list reads as, or 'custom'. Derived
    // rather than stored: a mode kept beside the list it describes goes stale
    // the moment a chip is toggled, and then the control claims one thing while
    // the echo line below it says another.
    notifyModeOf(form) {
      var list = (form && form.notify) || [];
      var mode = NOTIFY_MODES.find(m => sameSet(m.statuses, list));
      return mode ? mode.key : 'custom';
    },

    // A mode replaces the status list outright. The channels are left alone, so
    // turning notifications off and on again does not re-ask where they go.
    notifyPickMode(form, key) {
      var mode = NOTIFY_MODES.find(m => m.key === key);
      if (!mode) return;
      form.notify = mode.statuses.slice();
      this.notifyChanged(form);
    },

    notifyStatusOn(form, status) {
      return ((form && form.notify) || []).includes(status);
    },

    notifyToggleStatus(form, status) {
      form.notify = this.notifyStatusOn(form, status)
        ? form.notify.filter(s => s !== status)
        : form.notify.concat([status]);
      this.notifyChanged(form);
    },

    // The channels a ticket's notifications go to: its own list, else its
    // project's, else the global default, with `none` anywhere in the winning
    // list meaning nowhere. It mirrors config.NotifyChannelsFor rather than
    // reading a resolved field off the ticket, because the init modal states
    // the channel for a path the user is still typing.
    notifyResolve(form) {
      var project = this.projectForPath(form && form.path) || {};
      var lists = [
        (form && form.notifyChannels) || [],
        project.notify_channels || [],
        this.configCache?.default_channels || [],
      ];
      for (var i = 0; i < lists.length; i++) {
        if (!lists[i].length) continue;
        return lists[i].includes(NOTIFY_SILENCE) ? [] : lists[i].slice();
      }
      return [];
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

    // The YAML this row is about to write, in the spelling the file gets. Same
    // convention as the new-ticket page's frontmatter preview.
    notifyEcho(form) {
      var list = (form && form.notify) || [];
      if (!list.length) return 'no notify: field written';
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

    // Whether the row asks for a channel at all. With one channel configured
    // there is nothing to choose, so it states the resolved one instead.
    notifyPicksChannel() {
      return (this.configCache?.channels || []).length > 1;
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
      var project = this.projectForPath(form && form.path);
      var from = project ? 'project ' + project.name : 'notifications.default';
      if (own.length) {
        return 'Overrides ' + from + ' for this ticket. The same channel twice is one message.';
      }
      var inherited = this.notifyResolve({ path: form && form.path, notifyChannels: [] });
      if (!inherited.length) {
        return 'Inherited: nothing is configured to receive it. Set notifications.default or the project\'s notify_channels.';
      }
      return 'Inherited: resolves to ' + inherited.join(', ') + ', from ' + from + '. Pick a channel to override it for this ticket alone.';
    },

    // What a form does after any change to either field. The init modal submits
    // with the rest of the form and needs nothing; the rail saves at once, and
    // says so with a flag on the draft rather than an identity check, which
    // Alpine's proxies make an unreliable way to tell two objects apart.
    notifyChanged(form) {
      if (form && form.autosave) this.saveNotify();
    },

    // ─── the details rail row ───

    // Editing notify writes the ticket file, so it is offered exactly where the
    // API accepts a frontmatter edit — config.StatusAllowsEdit, which is open,
    // todo, paused, human_review and the custom statuses. A running ticket
    // shows the row and does not open it: an agent owns the file, and the write
    // would be lost when the run puts it back.
    notifyCanEdit() {
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
      this.notifyDraft = {
        notify: (this.selectedTicket.notify || []).slice(),
        notifyChannels: (this.selectedTicket.notify_channels || []).slice(),
        path: this.selectedTicket.path || '',
        autosave: true,
      };
      this.notifyError = null;
      this.notifyChips = false;
      this.notifyEditing = true;
    },

    closeNotifyEditor() {
      this.notifyEditing = false;
      this.notifyError = null;
    },

    // Every change PATCHes at once, the way the detail edit form's selects do.
    // Both fields go on every request: they are edited together, and sending
    // only the one that changed would need a second copy of the draft to
    // compare against.
    async saveNotify() {
      var t = this.selectedTicket;
      if (!t || !this.notifyDraft) return;
      this.notifySaving = true;
      this.notifyError = null;
      try {
        const res = await fetch('/api/tickets/' + t.id, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            notify: this.notifyDraft.notify,
            notify_channels: this.notifyDraft.notifyChannels,
          }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.notifyError = data.error || 'Failed to save notify';
          return;
        }
        const updated = await res.json();
        const idx = this.tickets.findIndex(x => x.id === updated.id);
        if (idx >= 0) this.tickets[idx] = this.boardEntry(updated);
        // The panel may have moved on while this was in flight, and an
        // unguarded assignment would reopen the ticket it left.
        if (this.selectedTicket?.id === updated.id) {
          this.selectedTicket = updated;
          this.notifySaved = true;
          setTimeout(() => { this.notifySaved = false; }, 1500);
        }
      } catch (e) {
        this.notifyError = 'Failed to save notify: ' + e.message;
      } finally {
        this.notifySaving = false;
      }
    },
  };
}
