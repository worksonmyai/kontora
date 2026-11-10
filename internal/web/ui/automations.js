export const automationDays = [
  { value: 1, label: 'Mon' }, { value: 2, label: 'Tue' },
  { value: 3, label: 'Wed' }, { value: 4, label: 'Thu' },
  { value: 5, label: 'Fri' }, { value: 6, label: 'Sat' },
  { value: 0, label: 'Sun' },
];

function blankAutomation() {
  return { id: null, revision: 0, schedule_revision: 0, name: '', path: '', agent: '', prompt: '', frequency: 'weekdays', time: '09:00', timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', days: [1], enabled: false, skipOverlap: true };
}

function automationDraft(row) {
  if (!row || typeof row.id !== 'string' || !row.id || !Number.isSafeInteger(row.revision) || row.revision < 1
    || !Number.isSafeInteger(row.schedule_revision) || row.schedule_revision < 1
    || !['name', 'path', 'agent', 'instructions'].every(key => typeof row[key] === 'string')
    || typeof row.enabled !== 'boolean' || typeof row.skip_overlap !== 'boolean'
    || !row.schedule || !['daily', 'weekdays', 'weekly'].includes(row.schedule.frequency)
    || typeof row.schedule.time !== 'string' || typeof row.schedule.timezone !== 'string'
    || (row.schedule.days != null && !Array.isArray(row.schedule.days))) {
    throw new Error('Invalid automation response.');
  }
  return { id: row.id, revision: row.revision, schedule_revision: row.schedule_revision,
    name: row.name, path: row.path, agent: row.agent, prompt: row.instructions,
    ...row.schedule, days: [...(row.schedule.days || [])], enabled: row.enabled, skipOverlap: row.skip_overlap };
}

function automationDefinition(draft) {
  return { ...(draft.id ? { id: draft.id } : {}), revision: draft.revision, schedule_revision: draft.schedule_revision,
    name: draft.name.trim(), path: draft.path.trim(), agent: draft.agent, instructions: draft.prompt.trim(),
    schedule: { frequency: draft.frequency, days: draft.frequency === 'weekly' ? [...draft.days] : [], time: draft.time, timezone: draft.timezone.trim() },
    enabled: draft.enabled, skip_overlap: draft.skipOverlap };
}

export function kontoraAutomations() {
  return {
    automations: [],
    automationsInitialized: false,
    automationsLoading: false,
    automationsAvailable: false,
    automationsError: '',
    automationsConflict: '',
    automationsPending: false,
    automationsToggling: {},
    automationsSelected: null,
    automationsEditing: false,
    automationsDraft: blankAutomation(),
    automationsErrors: {},
    automationsFilter: 'all',
    automationsQuery: '',
    automationsDays: automationDays,
    automationsNotice: '',
    automationsBaseline: '',
    automationsPreview: [],
    automationsPreviewLoading: false,
    automationsPreviewError: '',
    automationsHistory: [],
    automationsHistoryID: null,
    automationsHistoryOffset: 0,
    automationsHistoryTotal: 0,
    automationsHistoryLoading: false,
    automationsHistoryError: '',
    automationsRecovering: null,
    automationsTimezones: typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : ['UTC'],
    _automationsLoadSeq: 0,
    _automationsEditorSeq: 0,
    _automationsPreviewSeq: 0,
    _automationsPreviewTimer: null,
    _automationsPreviewKey: '',
    _automationsHistorySeq: 0,
    _automationsCreate: null,

    async automationsRequest(path = '', options = {}) {
      const res = await fetch('/api/automations' + path, options);
      if (res.status === 401) this.needsAuth = true;
      const data = await res.json().catch(error => {
        if (res.ok) throw error;
        return {};
      });
      if (!res.ok) {
        const error = new Error(data?.error || 'Automation request failed (' + res.status + ')');
        error.status = res.status;
        throw error;
      }
      return data;
    },

    async openAutomations() {
      this.automationsInitialized = true;
      await this.automationsRefresh();
    },

    async automationsRefresh() {
      if (!this.automationsInitialized || this._appDestroyed) return;
      const seq = ++this._automationsLoadSeq;
      this.automationsLoading = true;
      try {
        const data = await this.automationsRequest();
        if (seq !== this._automationsLoadSeq) return;
        this.automationsAvailable = data.available === true;
        this.automationsError = data.error || (data.available ? '' : 'Automations are unavailable.');
        this.automations = (data.definitions || []).map(row => {
          const previous = this.automations.find(existing => existing.id === row.id);
          return previous?.revision > row.revision ? previous : row;
        });
        const row = this.automations.find(row => row.id === this.automationsSelected);
        if (row && !this.automationsPending) {
          if (this.automationsDirty()) {
            if (row.revision !== this.automationsDraft.revision) this.automationsConflict = 'This automation changed on the server. Your unsaved changes are preserved. Reload the saved version before editing again.';
          } else this.automationsAdopt(row);
        } else if (this.automationsSelected && !row && data.available) {
          this.automationsConflict = 'This automation is no longer available. Your draft is preserved.';
        }
        if (this.automationsHistoryID) await this.automationsLoadHistory(this.automationsHistoryOffset);
      } catch (error) {
        if (seq === this._automationsLoadSeq) {
          this.automationsError = error.message;
          if (error.status === 503) this.automationsAvailable = false;
        }
      } finally {
        if (seq === this._automationsLoadSeq) this.automationsLoading = false;
      }
    },

    automationsDefaults() {
      const project = this.configCache?.projects?.[0];
      return { path: project?.path || '', agent: project?.agent || this.configCache?.agents?.[0] || '' };
    },

    automationsRows() {
      const query = this.automationsQuery.trim().toLowerCase();
      return this.automations.filter(row =>
        (this.automationsFilter === 'all' || (this.automationsFilter === 'enabled' ? row.enabled : !row.enabled))
        && `${row.name} ${row.path} ${row.agent}`.toLowerCase().includes(query));
    },

    automationsDirty() {
      return this.automationsEditing && JSON.stringify(this.automationsDraft) !== this.automationsBaseline;
    },

    automationsConfirmReplace() {
      if (this.automationsPending) return false;
      return !this.automationsDirty() || window.confirm('Discard unsaved changes to this automation?');
    },

    automationsAdopt(row) {
      const current = this.automations.find(existing => existing.id === row.id);
      if (current?.revision > row.revision) row = current;
      if (this.automationsDraft.id === row.id && this.automationsDraft.revision > row.revision) return;
      const draft = automationDraft(row);
      this.automations = this.automations.map(existing => existing.id === row.id && existing.revision < row.revision ? row : existing);
      this.automationsDraft = draft;
      this.automationsBaseline = JSON.stringify(this.automationsDraft);
      this.automationsConflict = '';
      this.automationsErrors = {};
      this.automationsSchedulePreview();
    },

    async automationsSelect(row) {
      if (!this.automationsConfirmReplace()) return false;
      const seq = ++this._automationsEditorSeq;
      this.automationsSelected = row.id;
      this.automationsEditing = true;
      this._automationsCreate = null;
      this.automationsNotice = '';
      this.automationsAdopt(row);
      this.automationsHistoryID = row.id;
      this.automationsHistory = [];
      this.automationsLoadHistory(0);
      const baseline = this.automationsBaseline;
      try {
        const full = await this.automationsRequest('/' + encodeURIComponent(row.id));
        if (seq !== this._automationsEditorSeq || this.automationsSelected !== row.id || full.revision < this.automationsDraft.revision) return;
        if (this.automationsBaseline === baseline && !this.automationsDirty()) this.automationsAdopt(full);
        else if (full.revision !== this.automationsDraft.revision) this.automationsConflict = 'This automation changed on the server. Your unsaved changes are preserved.';
      } catch (error) {
        if (seq === this._automationsEditorSeq) this.automationsNotice = error.message;
      }
    },

    async automationsReload() {
      const row = this.automations.find(row => row.id === this.automationsSelected);
      if (row) await this.automationsSelect(row);
    },

    automationsNew() {
      if (!this.automationsConfirmReplace()) return false;
      ++this._automationsEditorSeq;
      this.automationsSelected = null;
      this._automationsCreate = null;
      this.automationsDraft = { ...blankAutomation(), ...this.automationsDefaults() };
      this.automationsBaseline = JSON.stringify(this.automationsDraft);
      this.automationsEditing = true;
      this.automationsErrors = {};
      this.automationsNotice = '';
      this.automationsConflict = '';
      this.automationsHistoryID = null;
      ++this._automationsHistorySeq;
      this.automationsHistory = [];
      this.automationsSchedulePreview();
      this.$nextTick(() => this.$refs.automationName?.focus());
    },

    automationsCloseEditor() {
      if (!this.automationsConfirmReplace()) return false;
      ++this._automationsEditorSeq;
      this.automationsEditing = false;
      this.automationsSelected = null;
      this.automationsCancelPreview();
      this.$nextTick(() => this.$refs.automationNew?.focus());
    },

    automationsToggleDay(day) {
      const days = this.automationsDraft.days;
      this.automationsDraft.days = days.includes(day) ? days.filter(value => value !== day) : [...days, day];
      this.automationsSchedulePreview();
    },

    automationsSchedule(row) {
      const schedule = row.schedule || row;
      const frequency = schedule.frequency === 'daily' ? 'Every day'
        : schedule.frequency === 'weekdays' ? 'Weekdays'
          : automationDays.filter(day => (schedule.days || []).includes(day.value)).map(day => day.label).join(', ') || 'Choose days';
      return frequency + ' at ' + schedule.time + ' · ' + schedule.timezone;
    },

    automationsCancelPreview() {
      clearTimeout(this._automationsPreviewTimer);
      ++this._automationsPreviewSeq;
      this._automationsPreviewKey = '';
      this.automationsPreviewLoading = false;
      this.automationsPreview = [];
    },

    automationsSchedulePreview() {
      if (!this.automationsEditing) return;
      const schedule = automationDefinition(this.automationsDraft).schedule;
      const key = JSON.stringify(schedule);
      if (key === this._automationsPreviewKey) return;
      this.automationsCancelPreview();
      this._automationsPreviewKey = key;
      const seq = this._automationsPreviewSeq;
      this.automationsPreviewError = '';
      this.automationsPreviewLoading = true;
      this._automationsPreviewTimer = setTimeout(async () => {
        try {
          const data = await this.automationsRequest('/preview', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: key });
          if (seq !== this._automationsPreviewSeq) return;
          this.automationsPreview = data.occurrences || [];
        } catch (error) {
          if (seq === this._automationsPreviewSeq) this.automationsPreviewError = error.message;
        } finally {
          if (seq === this._automationsPreviewSeq) this.automationsPreviewLoading = false;
        }
      }, 300);
    },

    automationsDates() {
      return this.automationsPreview.map(value => new Date(value));
    },

    automationsDateLabel(date, timezone = this.automationsDraft.timezone) {
      if (!date || !Number.isFinite(new Date(date).getTime())) return '—';
      try {
        return new Intl.DateTimeFormat(undefined, { timeZone: timezone, timeZoneName: 'short', weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(new Date(date));
      } catch { return 'Invalid timezone'; }
    },

    automationsNextLabel(row) {
      return !row.enabled ? 'Paused' : row.next ? this.automationsDateLabel(row.next, row.schedule.timezone) : 'No next occurrence';
    },

    automationsRepository(row) {
      const project = (this.configCache?.projects || []).find(project => project.path === row.path);
      return project?.name || row.path.split('/').filter(Boolean).at(-1) || 'No repository selected';
    },

    async automationsToggle(row) {
      if (!this.automationsAvailable || this.automationsPending || this.automationsToggling[row.id]) return;
      this.automationsToggling[row.id] = true;
      this.automationsError = '';
      try {
        const full = await this.automationsRequest('/' + encodeURIComponent(row.id));
        if (full.revision !== row.revision) {
          const error = new Error('This automation changed on the server. Refresh before changing its state.');
          error.status = 409;
          throw error;
        }
        const saved = await this.automationsRequest('/' + encodeURIComponent(row.id), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...full, enabled: !row.enabled }) });
        const current = this.automationsUpsert(saved);
        if (this.automationsSelected === row.id) {
          if (!this.automationsDirty()) this.automationsAdopt(current);
          else this.automationsConflict = 'The saved state changed. Your unsaved changes are preserved. Reload the saved version before editing again.';
        }
      } catch (error) {
        this.automationsError = error.message;
        if (error.status === 409 && this.automationsSelected === row.id) this.automationsConflict = error.message;
      } finally {
        delete this.automationsToggling[row.id];
      }
    },

    automationsValidate() {
      const draft = this.automationsDraft;
      const errors = {};
      if (!draft.name.trim()) errors.name = 'Enter a name for this automation.';
      if (!draft.path.trim()) errors.path = 'Choose a repository or enter its path.';
      if (!draft.agent) errors.agent = 'Choose a configured agent.';
      if (!draft.prompt.trim()) errors.prompt = 'Enter the instructions for each run.';
      if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(draft.time) || (draft.frequency === 'weekly' && !draft.days.length) || !draft.timezone.trim()) errors.schedule = 'Choose a valid time and timezone. If Repeat is set to Selected days, choose at least one day.';
      this.automationsErrors = errors;
      return Object.keys(errors).length === 0;
    },

    automationsUpsert(row) {
      automationDraft(row);
      const current = this.automations.find(existing => existing.id === row.id);
      if (current?.revision > row.revision) return current;
      this.automations = [...this.automations.filter(existing => existing.id !== row.id), row];
      ++this._automationsLoadSeq;
      this.automationsLoading = false;
      return row;
    },

    async automationsSaveDraft() {
      if (this.automationsPending || !this.automationsAvailable || this.automationsConflict || this.automationsToggling[this.automationsSelected]) return;
      this.automationsNotice = '';
      if (!this._automationsCreate && !this.automationsValidate()) {
        this.$nextTick(() => document.querySelector('.automations-editor [aria-invalid="true"]')?.focus());
        return;
      }
      const seq = this._automationsEditorSeq;
      const snapshot = JSON.stringify(this.automationsDraft);
      const draft = this.automationsDraft;
      if (!draft.id && !this._automationsCreate) {
        this._automationsCreate = { request_id: typeof crypto.randomUUID === 'function' ? crypto.randomUUID() : Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, '0')).join(''), definition: { ...automationDefinition(draft), enabled: false } };
      }
      const create = this._automationsCreate;
      this.automationsPending = true;
      try {
        const saved = await this.automationsRequest(draft.id ? '/' + encodeURIComponent(draft.id) : '', {
          method: draft.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(draft.id ? automationDefinition(draft) : create),
        });
        const baseline = automationDraft(saved);
        const current = this.automationsUpsert(saved);
        if (seq !== this._automationsEditorSeq) return;
        this._automationsCreate = null;
        this.automationsSelected = saved.id;
        // A retried creation uses its original payload, even if the user edited the draft after a lost response.
        const matches = create ? JSON.stringify(automationDefinition(this.automationsDraft)) === JSON.stringify(create.definition) : JSON.stringify(this.automationsDraft) === snapshot;
        if (matches) this.automationsAdopt(current);
        else {
          if (this.automationsDraft.revision <= saved.revision) {
            this.automationsDraft = { ...this.automationsDraft, id: saved.id, revision: saved.revision, schedule_revision: saved.schedule_revision };
            this.automationsBaseline = JSON.stringify(baseline);
          }
          if (current.revision > this.automationsDraft.revision) this.automationsConflict = 'This automation changed on the server. Your unsaved changes are preserved. Reload the saved version before editing again.';
        }
        this.automationsNotice = this.automationsDirty() ? 'The original request was saved. Your later edits are still unsaved.' : 'Automation saved.';
        this.automationsHistoryID = saved.id;
        await this.automationsLoadHistory(0);
      } catch (error) {
        if (seq !== this._automationsEditorSeq) return;
        if (error.status === 400) this._automationsCreate = null;
        this.automationsNotice = error.message + (this._automationsCreate ? ' Retry save to confirm the original request without creating a duplicate.' : '');
        if (error.status === 409) this.automationsConflict = 'This automation changed on the server. Your draft is preserved. Reload the saved version before editing again.';
      } finally {
        this.automationsPending = false;
      }
    },

    automationsHistoryTimezone() {
      return this.automations.find(row => row.id === this.automationsHistoryID)?.schedule.timezone || 'UTC';
    },

    async automationsLoadHistory(offset = 0) {
      const id = this.automationsHistoryID;
      if (!id) return;
      const seq = ++this._automationsHistorySeq;
      this.automationsHistoryLoading = true;
      this.automationsHistoryError = '';
      try {
        const data = await this.automationsRequest('/' + encodeURIComponent(id) + '/occurrences?offset=' + offset + '&limit=20');
        if (seq !== this._automationsHistorySeq || id !== this.automationsHistoryID) return;
        this.automationsHistory = (data.occurrences || []).map(({ payload, ...row }) => ({ ...row, timezone: payload?.schedule?.timezone || this.automationsHistoryTimezone() }));
        this.automationsHistoryTotal = data.total;
        this.automationsHistoryOffset = offset;
      } catch (error) {
        if (seq === this._automationsHistorySeq) this.automationsHistoryError = error.message;
      } finally {
        if (seq === this._automationsHistorySeq) this.automationsHistoryLoading = false;
      }
    },

    async automationsSkip(row) {
      if (this.automationsRecovering || !this.automationsAvailable) return;
      if (!window.confirm('Mark this unresolved occurrence skipped? This does not create a replacement ticket or release any execution reservation.')) return;
      this.automationsRecovering = row.id;
      this.automationsHistoryError = '';
      try {
        await this.automationsRequest('/' + encodeURIComponent(row.automation_id) + '/occurrences/' + encodeURIComponent(row.id) + '/skip', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ revision: row.revision }) });
        await this.automationsRefresh();
      } catch (error) {
        this.automationsHistoryError = error.status === 409 ? 'This occurrence changed. Refresh history before acknowledging it.' : error.message;
      } finally {
        this.automationsRecovering = null;
      }
    },

    automationsDispose() {
      this.automationsCancelPreview();
      ++this._automationsLoadSeq;
      ++this._automationsHistorySeq;
      ++this._automationsEditorSeq;
    },
  };
}
