import { termState } from './terminal.js';

const automationsFields = ['name', 'prompt', 'path', 'agent', 'model', 'schedule', 'timezone', 'workspace', 'overlap', 'timeout', 'enabled'];
const automationsBlank = () => ({ name: '', prompt: '', path: '', agent: '', model: '', schedule: '0 9 * * 1-5', timezone: 'UTC', workspace: 'worktree', overlap: 'skip', timeout: '30m', enabled: true });
const automationsSpec = draft => Object.fromEntries(automationsFields.map(key => [key, draft[key]]));
const automationsID = () => crypto.randomUUID?.() || Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, '0')).join('');

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
    automationsDetail: false,
    automationsEditing: false,
    automationsDraft: automationsBlank(),
    automationsBaseline: '',
    automationsErrors: {},
    automationsFilter: 'all',
    automationsQuery: '',
    automationsTab: 'runs',
    automationsMenu: false,
    automationsDeleteOpen: false,
    automationsDeleteTarget: null,
    automationsToast: '',
    automationsToastTimer: null,
    automationsNotice: '',
    automationsPreview: [],
    automationsPreviewHuman: '',
    automationsPreviewLoading: false,
    automationsPreviewError: '',
    automationsPreviewKey: '',
    automationsPreviewValidKey: '',
    automationsPreviewTimer: null,
    automationsYAML: '',
    automationsYAMLSeq: 0,
    automationsHistory: [],
    automationsHistoryTotal: 0,
    automationsHistoryLoading: false,
    automationsHistoryError: '',
    automationsLogRun: null,
    automationsLog: [],
    automationsLogLoading: false,
    automationsLogError: '',
    automationsLoadSeq: 0,
    automationsEditorSeq: 0,
    automationsPreviewSeq: 0,
    automationsHistorySeq: 0,
    automationsLogSeq: 0,
    automationsCreate: null,
    automationsRunReceipts: {},
    automationsBeforeUnload: null,
    automationsPresets: [
      { label: 'every hour', value: '0 * * * *' }, { label: 'every 6h', value: '0 */6 * * *' },
      { label: 'daily 09:00', value: '0 9 * * *' }, { label: 'weekdays 09:00', value: '0 9 * * 1-5' },
      { label: 'nightly 03:00', value: '0 3 * * *' }, { label: 'mondays 10:00', value: '0 10 * * 1' },
    ],
    automationsTimezones: typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : [],

    async automationsRequest(path = '', options = {}) {
      const res = await fetch('/api/automations' + path, options);
      if (res.status === 401) this.needsAuth = true;
      const data = res.status === 204 ? null : await res.json().catch(error => {
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

    automationsJSON(method, body) {
      return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
    },

    async automationsOpen() {
      this.automationsInitialized = true;
      if (!this.automationsBeforeUnload) {
        this.automationsBeforeUnload = event => {
          if (!this.automationsDirty() && !this.automationsPending && !this.automationsCreate) return;
          event.preventDefault();
          event.returnValue = '';
        };
        window.addEventListener('beforeunload', this.automationsBeforeUnload);
      }
      if (!this.configCache) {
        try {
          const res = await fetch('/api/config');
          if (res.ok) this.configCache = await res.json();
        } catch { /* The editor can still accept an explicit agent and path. */ }
      }
      await this.automationsRefresh();
    },

    async automationsRefresh() {
      if (this._appDestroyed) return;
      this.automationsInitialized = true;
      const seq = ++this.automationsLoadSeq;
      this.automationsLoading = true;
      try {
        const data = await this.automationsRequest();
        if (seq !== this.automationsLoadSeq) return;
        const previous = this.automationsRow();
        this.automationsAvailable = data.available === true;
        this.automationsError = data.error || (data.available ? '' : 'Automations are unavailable.');
        this.automations = (data.definitions || []).map(row => {
          const old = this.automations.find(item => item.id === row.id);
          return this.automationsDefinitionNewer(old, row) ? old : row;
        });
        const row = this.automationsRow();
        if (this.automationsEditing && this.automationsSelected && !this.automationsPending) {
          if (!row || row.revision !== this.automationsDraft.revision) {
            this.automationsConflict = 'The saved automation changed. Your draft is preserved. Reload the saved version before saving.';
          }
        }
        if (this.automationsDetail && !this.automationsEditing) {
          if (!row) this.automationsError = 'This automation is no longer available.';
          if (previous?.current_run?.n !== row?.current_run?.n || previous?.running !== row?.running) {
            this.closeTerminal();
            this.terminalRW = false;
          }
          if (!row?.running && this.automationsTab === 'session') this.automationsTab = 'runs';
          if (row?.running && this.automationsTab === 'session') this.$nextTick(() => this.openTerminal());
          await this.automationsLoadHistory(false);
        }
      } catch (error) {
        if (seq === this.automationsLoadSeq) {
          this.automationsError = error.message;
          if (error.status === 503) this.automationsAvailable = false;
        }
      } finally {
        if (seq === this.automationsLoadSeq) this.automationsLoading = false;
      }
    },

    automationsRow() { return this.automations.find(row => row.id === this.automationsSelected) || null; },
    automationsRunningCount() { return this.automations.filter(row => row.running).length; },
    automationsRows() {
      const tokens = this.automationsQuery.toLowerCase().match(/(?:[^\s"]+|"[^"]*")+/g) || [];
      return this.automations.filter(row => (this.automationsFilter === 'all' || (this.automationsFilter === 'enabled' ? row.enabled : !row.enabled))
        && tokens.every(token => {
          const [field, ...value] = token.split(':');
          const needle = value.join(':').replaceAll('"', '');
          if (field === 'project' && value.length) return this.automationsProject(row).toLowerCase().includes(needle);
          if (field === 'agent' && value.length) return (row.agent || '').toLowerCase().includes(needle);
          return [row.id, row.key, row.name, row.schedule, row.schedule_human, row.path, row.agent, this.automationsProject(row)].join(' ').toLowerCase().includes(token.replaceAll('"', ''));
        }));
    },
    automationsProject(row) {
      return (typeof row?.project === 'string' ? row.project : row?.project?.name) || (this.configCache?.projects || []).find(project => project.path === row?.path)?.name || '';
    },
    automationsProjectColor(row) {
      const project = (this.configCache?.projects || []).find(project => project.path === row?.path || project.name === this.automationsProject(row));
      return this.pipelineColorByName(project?.pipeline || this.pathBasename(row?.path || ''));
    },
    automationsKey() {
      if (this.automationsDraft.key) return this.automationsDraft.key;
      const slug = this.automationsDraft.name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'untitled';
      return (/^[a-z]/.test(slug) ? slug : 'automation-' + slug).slice(0, 100).replace(/-$/, '');
    },
    automationsDirty() { return this.automationsEditing && JSON.stringify(this.automationsDraft) !== this.automationsBaseline; },
    automationsConfirmReplace() {
      if (this.automationsPending) return false;
      return (!this.automationsDirty() && !this.automationsCreate) || window.confirm(this.automationsCreate
        ? 'The create request is unconfirmed. Leave without retrying? The automation may already exist.' : 'Discard unsaved automation changes?');
    },
    automationsAdopt(row) {
      this.automationsDraft = { ...automationsBlank(), ...automationsSpec(row), id: row.id, key: row.key, revision: row.revision };
      this.automationsBaseline = JSON.stringify(this.automationsDraft);
      this.automationsConflict = '';
      this.automationsErrors = {};
      this.automationsDraftChanged();
    },
    automationsDefinitionNewer(a, b) {
      if (!a) return false;
      if (a.revision !== b.revision) return a.revision > b.revision;
      const latest = row => [row.current_run, row.last_run].filter(Boolean).sort((x, y) => y.n - x.n || y.revision - x.revision)[0];
      const x = latest(a), y = latest(b);
      return !!x && (!y || x.n > y.n || (x.n === y.n && x.revision > y.revision));
    },
    automationsUpsert(row) {
      if (typeof row?.id !== 'string' || !row.id || !Number.isSafeInteger(row.revision) || row.revision < 1) throw new Error('Invalid automation response.');
      const old = this.automations.find(item => item.id === row.id);
      if (this.automationsDefinitionNewer(old, row)) return old;
      this.automations = old ? this.automations.map(item => item.id === row.id ? row : item) : [...this.automations, row];
      ++this.automationsLoadSeq;
      this.automationsLoading = false;
      return row;
    },

    async automationsSelect(row, writeRoute = true) {
      if (this.automationsSelected === row.id && this.automationsDetail && !this.automationsEditing) return true;
      if (!this.automationsConfirmReplace()) return false;
      const seq = ++this.automationsEditorSeq;
      this.closeAgentPicker(false);
      this.closeTerminal();
      this.terminalRW = false;
      this.automationsCancelPreview();
      this.automationsSelected = row.id;
      this.automationsDetail = true;
      this.automationsEditing = false;
      this.automationsMenu = false;
      this.automationsCreate = null;
      this.automationsConflict = '';
      this.automationsNotice = '';
      this.automationsHistory = [];
      this.automationsHistoryTotal = 0;
      this.automationsHistoryLoading = true;
      this.automationsLogRun = null;
      ++this.automationsLogSeq;
      this.automationsTab = row.running ? 'session' : 'runs';
      if (writeRoute) this.writeHash();
      try {
        const full = await this.automationsRequest('/' + encodeURIComponent(row.id));
        if (seq !== this.automationsEditorSeq || this.automationsSelected !== row.id) return false;
        if (full.id !== row.id) throw new Error('The server returned a different automation.');
        const current = this.automationsUpsert(full);
        this.automationsTab = current.running ? 'session' : 'runs';
        await this.automationsLoadHistory(false);
        if (seq !== this.automationsEditorSeq) return false;
        if (this.automationsTab === 'session') this.$nextTick(() => this.openTerminal());
        return true;
      } catch (error) {
        if (seq === this.automationsEditorSeq) {
          this.automationsError = error.message;
          this.automationsHistoryLoading = false;
        }
        return false;
      }
    },

    automationsNew(copy = null, writeRoute = true) {
      if (!this.automationsConfirmReplace()) return false;
      ++this.automationsEditorSeq;
      this.closeTerminal();
      this.terminalRW = false;
      this.closeAgentPicker(false);
      this.automationsCancelPreview();
      this.automationsSelected = null;
      this.automationsDetail = false;
      this.automationsEditing = true;
      this.automationsCreate = null;
      this.automationsMenu = false;
      const project = this.configCache?.projects?.[0];
      this.automationsDraft = copy ? { ...automationsSpec(copy), name: copy.name + ' (copy)', enabled: false }
        : { ...automationsBlank(), path: project?.path || '', agent: project?.agent || this.configCache?.default_agent || this.configCache?.agents?.[0] || '' };
      this.automationsBaseline = JSON.stringify(this.automationsDraft);
      this.automationsErrors = {};
      this.automationsConflict = '';
      this.automationsNotice = '';
      this.automationsDraftChanged();
      if (writeRoute) this.writeHash();
      this.$nextTick(() => this.$refs.automationName?.focus());
      return true;
    },
    automationsEdit(row = this.automationsRow(), writeRoute = true) {
      if (!row || !this.automationsConfirmReplace()) return false;
      ++this.automationsEditorSeq;
      this.closeTerminal();
      this.terminalRW = false;
      this.automationsSelected = row.id;
      this.automationsDetail = true;
      this.automationsEditing = true;
      this.automationsMenu = false;
      this.automationsCreate = null;
      this.automationsNotice = '';
      this.automationsAdopt(row);
      if (writeRoute) this.writeHash();
      return true;
    },
    automationsLeave() {
      ++this.automationsEditorSeq;
      ++this.automationsHistorySeq;
      ++this.automationsLogSeq;
      this.closeAgentPicker(false);
      this.closeTerminal();
      this.terminalRW = false;
      this.automationsEditing = false;
      this.automationsDetail = false;
      this.automationsSelected = null;
      this.automationsCreate = null;
      this.automationsDeleteOpen = false;
      this.automationsMenu = false;
      this.automationsCancelPreview();
    },
    async automationsBack(writeRoute = true, list = false) {
      if (!this.automationsConfirmReplace()) return false;
      const row = this.automationsRow();
      if (this.automationsEditing && row && !list) {
        this.automationsEditing = false;
        this.automationsDetail = false;
        await this.automationsSelect(row, false);
      } else this.automationsLeave();
      if (writeRoute) this.writeHash();
      return true;
    },
    async automationsApplyRoute(route, navigation) {
      const mode = route.automationsMode || 'list';
      if (mode === 'new') return this.automationsEditing && !this.automationsSelected ? true : this.automationsNew(null, false);
      if (mode === 'list') return !this.automationsDetail && !this.automationsEditing ? true : this.automationsBack(false, true);
      if (this.automationsSelected === route.automationsId && (mode === 'edit') === this.automationsEditing) return true;
      if (this.automationsSelected === route.automationsId && this.automationsEditing) return this.automationsBack(false);
      const row = this.automations.find(item => item.id === route.automationsId) || { id: route.automationsId };
      if (await this.automationsSelect(row, false) === false || navigation !== this._navigationSeq) return false;
      return mode === 'edit' ? this.automationsEdit(this.automationsRow(), false) : true;
    },
    async automationsReload() {
      if (!this.automationsConfirmReplace()) return false;
      const id = this.automationsSelected;
      const seq = ++this.automationsEditorSeq;
      try {
        const row = await this.automationsRequest('/' + encodeURIComponent(id));
        if (seq !== this.automationsEditorSeq || id !== this.automationsSelected) return false;
        this.automationsAdopt(this.automationsUpsert(row));
        return true;
      } catch (error) { if (seq === this.automationsEditorSeq) this.automationsNotice = error.message; return false; }
    },

    automationsCancelPreview() {
      clearTimeout(this.automationsPreviewTimer);
      ++this.automationsPreviewSeq;
      this.automationsPreviewKey = '';
      this.automationsPreviewValidKey = '';
      this.automationsPreviewLoading = false;
      this.automationsPreview = [];
    },
    automationsScheduleError() {
      const value = this.automationsDraft.schedule.trim();
      if (!value) return 'A schedule is required.';
      if (/^(?:\d+(?:h|m|s))+$/.test(value)) return '';
      const fields = value.split(/\s+/);
      if (fields.length !== 5) return 'cron takes 5 fields (minute hour day month weekday), got ' + fields.length;
      if (fields.some(field => !/^[\d*/,-]+$/.test(field))) return 'cron fields take digits, *, /, - and , only';
      return '';
    },
    automationsSchedulePreview(force = false) {
      if (!this.automationsEditing) return;
      const key = JSON.stringify({ schedule: this.automationsDraft.schedule, timezone: this.automationsDraft.timezone });
      if (!force && key === this.automationsPreviewKey) return;
      this.automationsCancelPreview();
      this.automationsPreviewKey = key;
      this.automationsPreviewHuman = '';
      this.automationsPreviewError = this.automationsScheduleError();
      if (this.automationsPreviewError) return;
      const seq = this.automationsPreviewSeq;
      this.automationsPreviewLoading = true;
      this.automationsPreviewTimer = setTimeout(async () => {
        try {
          const data = await this.automationsRequest('/preview', this.automationsJSON('POST', JSON.parse(key)));
          if (seq !== this.automationsPreviewSeq) return;
          this.automationsPreview = data.occurrences || [];
          this.automationsPreviewHuman = data.human || '';
          this.automationsPreviewValidKey = key;
        } catch (error) {
          if (seq === this.automationsPreviewSeq) this.automationsPreviewError = error.message;
        } finally { if (seq === this.automationsPreviewSeq) this.automationsPreviewLoading = false; }
      }, 250);
    },
    async automationsDraftChanged() {
      this.automationsSchedulePreview();
      const seq = ++this.automationsYAMLSeq;
      const key = this.automationsKey();
      const spec = automationsSpec(this.automationsDraft);
      try {
        const yaml = await import('/vendor/yaml@2.8.1/yaml.mjs');
        const text = yaml.stringify({ automations: { [key]: spec } }, { lineWidth: 0 });
        if (seq === this.automationsYAMLSeq) this.automationsYAML = text;
      } catch (error) { if (seq === this.automationsYAMLSeq) this.automationsYAML = 'Preview unavailable: ' + error.message; }
    },
    automationsYAMLLines() {
      return this.automationsYAML.trimEnd().split('\n').map(text => ({ text: text || ' ',
        class: text === 'automations:' ? 'text-tx-4' : /^ {2}\S/.test(text) ? 'text-st-open'
          : /^ {4}enabled: false$/.test(text) ? 'text-st-paused' : /^ {6}/.test(text) ? 'text-tx-2' : 'text-tx-3' }));
    },
    automationsCanSave() {
      return this.automationsAvailable && !this.automationsPending && !this.automationsConflict && !this.automationsToggling[this.automationsSelected]
        && (this.automationsCreate || (this.automationsDraft.name.trim() && this.automationsDraft.path.trim() && this.automationsDraft.agent.trim() && this.automationsDraft.prompt.trim()
          && this.automationsPreviewValidKey === JSON.stringify({ schedule: this.automationsDraft.schedule, timezone: this.automationsDraft.timezone })));
    },
    async automationsSaveDraft(runNow = false) {
      if (!this.automationsCanSave()) return;
      const draft = this.automationsDraft;
      const seq = this.automationsEditorSeq;
      if (!draft.id && !this.automationsCreate) this.automationsCreate = { request_id: automationsID(), key: this.automationsKey(), definition: automationsSpec(draft), runNow };
      const create = this.automationsCreate;
      this.automationsPending = true;
      this.automationsNotice = '';
      let saved;
      try {
        const payload = create ? { request_id: create.request_id, key: create.key, definition: create.definition }
          : { revision: draft.revision, definition: automationsSpec(draft) };
        saved = this.automationsUpsert(await this.automationsRequest(draft.id ? '/' + encodeURIComponent(draft.id) : '', this.automationsJSON(draft.id ? 'PUT' : 'POST', payload)));
        if (seq !== this.automationsEditorSeq) return;
        this.automationsCreate = null;
        this.automationsSelected = saved.id;
        this.automationsAdopt(saved);
        this.automationsNotify((create ? 'created' : 'saved') + ' automations.' + saved.key);
      } catch (error) {
        if (seq !== this.automationsEditorSeq) return;
        if (error.status >= 400 && error.status < 500) this.automationsCreate = null;
        this.automationsNotice = error.message + (this.automationsCreate ? ' Retry save to confirm the original request.' : '');
        if (error.status === 409) this.automationsConflict = 'The saved automation changed. Your draft is preserved. Reload the saved version before saving.';
      } finally { this.automationsPending = false; }
      if (!saved || seq !== this.automationsEditorSeq) return;
      if (await this.automationsSelect(saved)) {
        if (create ? create.runNow : runNow) await this.automationsRun(saved);
      }
    },

    async automationsToggle(row) {
      if (!row || !this.automationsAvailable || this.automationsPending || this.automationsToggling[row.id]) return;
      this.automationsToggling[row.id] = true;
      this.automationsError = '';
      try {
        const saved = await this.automationsRequest('/' + encodeURIComponent(row.id) + (row.enabled ? '/pause' : '/resume'), this.automationsJSON('POST', { revision: row.revision }));
        this.automationsUpsert(saved);
        if (this.automationsEditing && this.automationsSelected === row.id && this.automationsDraft.revision !== saved.revision) {
          this.automationsConflict = 'The schedule state changed. Your draft is preserved. Reload the saved version before saving.';
        }
        this.automationsNotify(row.id + ' schedule ' + (saved.enabled ? 'resumed' : 'paused'));
      } catch (error) { this.automationsError = error.message; }
      finally { delete this.automationsToggling[row.id]; }
    },
    async automationsRun(row = this.automationsRow()) {
      if (!row || !this.automationsAvailable || this.automationsPending) return;
      const request = this.automationsRunReceipts[row.id] ||= automationsID();
      this.automationsError = '';
      this.automationsPending = true;
      try {
        const run = this.automationsCheckRun(await this.automationsRequest('/' + encodeURIComponent(row.id) + '/run', this.automationsJSON('POST', { request_id: request })), row.id);
        delete this.automationsRunReceipts[row.id];
        this.automationsNotify(run.outcome === 'skipped' ? row.id + ' is already running · overlap: skip' : 'started ' + row.id + ' · run #' + run.n);
        await this.automationsRefresh();
        if (this.automationsSelected === row.id && this.automationsRow()?.running) this.automationsSwitchTab('session');
      } catch (error) {
        if (error.status >= 400 && error.status < 500) delete this.automationsRunReceipts[row.id];
        this.automationsError = error.message + (this.automationsRunReceipts[row.id] ? ' Retry run now to confirm the same request.' : '');
      } finally { this.automationsPending = false; }
    },
    automationsCheckRun(run, id) {
      if (run?.automation_id !== id || !Number.isSafeInteger(run.n) || run.n < 1) throw new Error('Invalid automation run response.');
      return run;
    },
    automationsCanStop(row = this.automationsRow()) { return !!row?.current_run && row.current_run.phase !== 'terminal'; },
    async automationsStop() {
      const row = this.automationsRow();
      if (!row?.current_run || this.automationsPending) return;
      this.automationsError = '';
      this.automationsPending = true;
      try {
        const run = await this.automationsRequest('/' + encodeURIComponent(row.id) + '/stop', this.automationsJSON('POST', { n: row.current_run.n }));
        this.automationsNotify((run.phase === 'terminal' ? 'stopped ' : 'stop requested for ') + row.id + ' · run #' + run.n);
        await this.automationsRefresh();
      } catch (error) { this.automationsError = error.message; }
      finally { this.automationsPending = false; }
    },
    automationsOpenDelete() {
      if (!this.automationsRow() || this.automationsPending) return;
      this.automationsDeleteTarget = { ...this.automationsRow() };
      this.automationsError = '';
      this.automationsDeleteOpen = true;
      this.automationsMenu = false;
      this.$nextTick(() => this.$refs.automationsDeleteCancel?.focus());
    },
    automationsCloseDelete() {
      if (this.automationsPending) return;
      this.automationsDeleteOpen = false;
      this.$nextTick(() => [...document.querySelectorAll('[data-automations-menu-trigger], [data-automations-delete-trigger]')].find(button => button.getClientRects().length)?.focus());
    },
    automationsTrapDelete(event) {
      const buttons = [...event.currentTarget.querySelectorAll('button:not(:disabled)')].filter(button => button.getClientRects().length);
      if (!buttons.length) { event.preventDefault(); return; }
      const index = buttons.indexOf(document.activeElement);
      if (event.shiftKey && index <= 0) { event.preventDefault(); buttons.at(-1).focus(); }
      else if (!event.shiftKey && (index < 0 || index === buttons.length - 1)) { event.preventDefault(); buttons[0].focus(); }
    },
    async automationsDelete() {
      const row = this.automationsDeleteTarget;
      if (!row || this.automationsPending) return;
      this.automationsError = '';
      this.automationsPending = true;
      try {
        await this.automationsRequest('/' + encodeURIComponent(row.id), this.automationsJSON('DELETE', { revision: row.revision }));
        this.automations = this.automations.filter(item => item.id !== row.id);
        ++this.automationsLoadSeq;
        this.automationsLoading = false;
        this.automationsLeave();
        this.writeHash();
        this.automationsNotify('deleted ' + row.id + ' · logs kept');
      } catch (error) { this.automationsError = error.message; }
      finally { this.automationsPending = false; }
    },
    automationsNotify(text) {
      clearTimeout(this.automationsToastTimer);
      this.automationsToast = text;
      this.automationsToastTimer = setTimeout(() => { this.automationsToast = ''; }, 2400);
    },

    async automationsLoadHistory(more = false) {
      const id = this.automationsSelected;
      if (!id || !this.automationsDetail) return;
      const seq = ++this.automationsHistorySeq;
      const offset = more ? this.automationsHistory.length : 0;
      const limit = more ? 20 : Math.min(200, Math.max(24, this.automationsHistory.length));
      this.automationsHistoryLoading = true;
      this.automationsHistoryError = '';
      try {
        const data = await this.automationsRequest('/' + encodeURIComponent(id) + '/runs?offset=' + offset + '&limit=' + limit);
        if (seq !== this.automationsHistorySeq || id !== this.automationsSelected) return;
        const page = data.runs || [];
        this.automationsHistory = more
          ? [...this.automationsHistory, ...page.filter(run => !this.automationsHistory.some(old => old.n === run.n))]
          : [...page, ...this.automationsHistory.filter(run => page.length && run.n < page.at(-1).n)];
        this.automationsHistoryTotal = data.total || 0;
      } catch (error) { if (seq === this.automationsHistorySeq) this.automationsHistoryError = error.message; }
      finally { if (seq === this.automationsHistorySeq) this.automationsHistoryLoading = false; }
    },
    automationsFollow() { termState.term?.scrollToBottom(); },
    automationsReconnect() {
      this.closeTerminal();
      this.$nextTick(() => this.openTerminal());
    },
    automationsSwitchTab(tab) {
      if (tab === 'session' && !this.automationsRow()?.running) tab = 'runs';
      this.automationsTab = tab;
      this.automationsMenu = false;
      if (tab === 'session') this.$nextTick(() => this.terminalOpen ? this.refitTerminal() : this.openTerminal());
    },
    automationsOutputSubject() {
      const row = this.automationsRow();
      const run = row?.current_run;
      if (this.currentView !== 'automations' || !this.automationsDetail || this.automationsEditing || !row?.running || !run) return null;
      return { ...row, kind: 'automation', status: 'in_progress', automation_id: row.id, n: run.n, run: run.n, current_run: run };
    },
    automationsRunActive(run) { return !!run && ['preparing', 'running', 'stopping'].includes(run.phase); },
    automationsOutcome(run) { return run?.outcome || run?.phase || '—'; },
    automationsOutcomeClass(run) {
      if (run?.outcome === 'complete') return 'text-ok';
      if (run?.outcome === 'failed' || run?.phase === 'recovery_required') return 'text-st-cancel';
      if (this.automationsRunActive(run)) return 'text-st-progress';
      return 'text-tx-faint';
    },
    automationsStrip() { return this.automationsHistory.slice(0, 24).slice().reverse(); },
    automationsStripSummary() {
      const runs = this.automationsHistory.slice(0, 24);
      return ['complete', 'failed', 'skipped'].map(outcome => runs.filter(run => run.outcome === outcome).length + ' ' + outcome).join(' · ');
    },
    automationsStripStyle(run) {
      const token = run.outcome === 'complete' ? 'hsl(var(--st-done) / .5)' : run.outcome === 'failed' ? 'hsl(var(--st-cancel) / .75)' : this.automationsRunActive(run) ? 'hsl(var(--st-progress) / .22)' : 'rgb(var(--surface-700))';
      return { background: token };
    },
    async automationsOpenRun(run) {
      if (this.automationsRunActive(run) && this.automationsRow()?.current_run?.n === run.n) { this.automationsSwitchTab('session'); return; }
      this.automationsTab = 'runs';
      this.automationsLogRun = run;
      this.automationsLog = [];
      this.automationsLogError = '';
      this.automationsLogLoading = true;
      const seq = ++this.automationsLogSeq;
      const id = this.automationsSelected;
      try {
        const data = await this.automationsRequest('/' + encodeURIComponent(id) + '/runs/' + run.n + '/log');
        if (seq === this.automationsLogSeq && id === this.automationsSelected) this.automationsLog = data.lines || [];
      } catch (error) { if (seq === this.automationsLogSeq) this.automationsLogError = error.message; }
      finally { if (seq === this.automationsLogSeq) this.automationsLogLoading = false; }
    },
    automationsAllRuns() { ++this.automationsLogSeq; this.automationsLogRun = null; this.automationsLogLoading = false; },
    automationsRunIndex() { return this.automationsHistory.findIndex(run => run.n === this.automationsLogRun?.n); },
    async automationsStepRun(delta) {
      const id = this.automationsSelected;
      const n = this.automationsLogRun?.n;
      let index = this.automationsRunIndex();
      if (index < 0) return;
      if (delta > 0 && index + 1 >= this.automationsHistory.length && this.automationsHistory.length < this.automationsHistoryTotal) await this.automationsLoadHistory(true);
      if (id !== this.automationsSelected || n !== this.automationsLogRun?.n) return;
      index = this.automationsRunIndex();
      const run = this.automationsHistory[index + delta];
      if (run) await this.automationsOpenRun(run);
    },
    automationsRawURL() { return '/api/automations/' + encodeURIComponent(this.automationsSelected) + '/runs/' + this.automationsLogRun?.n + '/log?raw=1'; },
    automationsLogClass(line) {
      return ({ tool_call: 'text-accent-bright', tool_result: 'text-tx-log', success: 'text-ok', warning: 'text-warn', error: 'text-err', lifecycle: 'text-tx-3' })[line.kind] || 'text-tx';
    },
    automationsLogTime(value) { return Date.parse(value) > 0 ? new Date(value).toLocaleTimeString([], { hour12: false }) : '—'; },
    automationsWall(run) {
      const start = Date.parse(run?.started_at);
      if (!Number.isFinite(start) || start <= 0) return '—';
      const completed = Date.parse(run.completed_at);
      const end = completed > 0 ? completed : (this.now || Date.now());
      return this.automationsDuration(Math.max(0, (Number(end) - start) / 1000));
    },
    automationsDuration(seconds) {
      if (seconds == null || !Number.isFinite(Number(seconds))) return '—';
      const s = Math.floor(Number(seconds));
      return s >= 3600 ? Math.floor(s / 3600) + 'h ' + Math.floor(s % 3600 / 60) + 'm' : Math.floor(s / 60) + 'm ' + s % 60 + 's';
    },
    automationsDateLabel(value, timezone = this.automationsRow()?.timezone || this.automationsDraft.timezone) {
      if (!value || !Number.isFinite(Date.parse(value)) || Date.parse(value) < 0) return '—';
      try { return new Intl.DateTimeFormat(undefined, { timeZone: timezone || 'UTC', weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(new Date(value)); }
      catch { return 'Invalid timezone'; }
    },
    automationsRunTimestamp(run, completed = false) {
      return (completed ? [run?.completed_at, run?.started_at, run?.accepted_at, run?.scheduled_for]
        : [run?.started_at, run?.accepted_at, run?.scheduled_for]).find(value => Date.parse(value) > 0) || '';
    },
    automationsRelative(value) {
      if (!(Date.parse(value) > 0)) return '—';
      const diff = Date.parse(value) - Number(this.now || Date.now());
      if (!Number.isFinite(diff)) return '—';
      const minutes = Math.max(1, Math.round(Math.abs(diff) / 60000));
      const label = minutes >= 1440 ? Math.floor(minutes / 1440) + 'd' : minutes >= 60 ? Math.floor(minutes / 60) + 'h ' + minutes % 60 + 'm' : minutes + 'm';
      return diff >= 0 ? 'in ' + label : label + ' ago';
    },
    automationsNextLabel(row) {
      if (!row.enabled) return 'paused';
      if (!(Date.parse(row.next) > 0)) return '—';
      try {
        const time = new Intl.DateTimeFormat(undefined, { timeZone: row.timezone || 'UTC', weekday: 'short', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(new Date(row.next));
        return time + ' · ' + this.automationsRelative(row.next);
      } catch { return 'Invalid timezone'; }
    },
    automationsCost(value) { return value == null || value === '' ? '—' : this.formatCostUSD(value) || '—'; },
    automationsConfigPath() { return this.configCache?.config_path || 'Config path unavailable'; },
    automationsLogDir(row) {
      const root = this.configCache?.logs_dir;
      return root && row?.key ? root.replace(/\/+$/, '') + '/automations/' + row.key + '/' : 'Log directory unavailable';
    },
    automationsRailDetails(row) {
      return [ ['id', row.id], ['key', row.key], ['agent', row.agent || 'default'], ['model', row.model || 'agent default'], ['path', row.path],
        ['workspace', row.workspace === 'repo' ? 'repo checkout' : 'fresh worktree per run'], ['overlap', row.overlap], ['timeout', row.timeout],
        ['logs', this.automationsLogDir(row)], ['created', this.automationsDateLabel(row.created_at, row.timezone)] ];
    },
    automationsDispose() {
      this.automationsCancelPreview();
      clearTimeout(this.automationsToastTimer);
      window.removeEventListener('beforeunload', this.automationsBeforeUnload);
      this.automationsBeforeUnload = null;
      ++this.automationsLoadSeq;
      ++this.automationsHistorySeq;
      ++this.automationsLogSeq;
      ++this.automationsEditorSeq;
      ++this.automationsYAMLSeq;
    },
  };
}
