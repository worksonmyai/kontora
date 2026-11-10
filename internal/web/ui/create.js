import { localScheduleText, parseScheduleInput, pickerToScheduleText, scheduleEchoParts, schedulePresets } from './format.js';

// The empty create form. Three places reset it — the desktop modal, the phone
// sheet and the component's initial state — and a field added in only two of
// them is a field the third silently drops.
export function newCreateForm() {
  return { title: '', path: '', pipeline: '', pipelineChoice: 'inherit', agent: '', status: 'todo', body: '', branch: '', base_branch: '', scheduled_at: '', scheduleMode: 'now', kind: '', parent: '' };
}

// What a branch field says when no name can be shown: the ticket carries no
// branch and the daemon names one at pickup.
const BRANCH_PLACEHOLDER = 'daemon assigns branch when run starts';

// Project lookup has to clean paths the way filepath.Clean does in the daemon;
// otherwise a valid dot segment changes notification routing and defaults.
function cleanProjectPath(raw) {
  var path = (raw || '').trim();
  if (!path) return '';
  var rooted = path.startsWith('/');
  var home = path.startsWith('~/');
  var rest = rooted ? path.slice(1) : home ? path.slice(2) : path;
  var parts = [];
  rest.split('/').forEach(part => {
    if (!part || part === '.') return;
    if (part === '..') {
      if (parts.length && parts[parts.length - 1] !== '..') parts.pop();
      else if (!rooted && !home) parts.push(part);
      return;
    }
    parts.push(part);
  });
  if (rooted) return '/' + parts.join('/');
  if (home) return parts.length ? '~/' + parts.join('/') : '~';
  return parts.join('/') || '.';
}

// The create-ticket modal and the init-from-an-existing-file modal.
export function kontoraCreate() {
  return {
    createError: null,
    _createGeneration: 0,

    resetCreateForm() {
      this.invalidateCreateForm();
      this.createForm = newCreateForm();
      this.createTouched = { pipeline: false, agent: false };
    },

    invalidateCreateForm() {
      this._createGeneration++;
      this.createError = null;
      this.createSubmitting = false;
    },

    _createContext() {
      var form = this.createForm;
      var generation = this._createGeneration;
      // Only the New sheet or routed New page owns creation. Background
      // selections and unrelated sheets can change while its request is pending.
      var owner = this.sheet?.type === 'new' ? this.sheet : 'new';
      return () => !this._appDestroyed && form === this.createForm && generation === this._createGeneration &&
        (owner === 'new' ? this.currentView === 'new' : this.sheet === owner);
    },

    async openCreateModal(navigation = ++this._navigationSeq) {
      this.closeAgentPicker(false);
      if (this.selectedEpic && await this.closeEpic(false) === false) return;
      if (navigation !== this._navigationSeq) return;
      if (this.selectedTicket) this.closeDetail(false);
      this.resetCreateForm();
      this.currentView = 'new';
      this.writeHash();
      await this.loadCreateConfig();
    },

    async loadCreateConfig() {
      var current = this._createContext();
      if (!this.configCache) {
        try {
          const res = await fetch('/api/config');
          if (!current()) return;
          if (!res.ok) throw new Error('Failed to load config');
          const config = await res.json();
          if (!current()) return;
          this.configCache = config;
        } catch (e) {
          if (!current()) return;
          this.createError = 'Failed to load config';
        }
      }
      if (current()) this.onCreatePathChange();
    },

    closeCreateModal() {
      this.closeAgentPicker(false);
      this.invalidateCreateForm();
      this.currentView = 'board';
      this.writeHash();
    },

    // Repository path -> project, under both the configured form (which may
    // start with ~) and the resolved absolute form, since the browser cannot
    // expand ~ itself. Keyed on the projects array it was built from: a config
    // reload replaces the array, and a miss only costs the linear scan over
    // the same list that this replaces.
    _projectIndex() {
      var projects = this.configCache?.projects || [];
      if (this._projectIndexSrc === projects) return this._projectByPath;
      var index = Object.create(null);
      projects.forEach(p => {
        [p.path, p.resolved_path].forEach(raw => {
          var norm = cleanProjectPath(raw);
          // First project wins, matching the find() this replaces.
          if (norm && index[norm] === undefined) index[norm] = p;
        });
      });
      this._projectIndexSrc = projects;
      this._projectByPath = index;
      return index;
    },

    // The project configured for a repository path.
    projectForPath(path) {
      var typed = cleanProjectPath(path);
      if (!typed) return null;
      return this._projectIndex()[typed] || null;
    },

    // What `project:` filters against. A repository outside the configured
    // projects still gets a name, so the token narrows those tickets too.
    ticketProjectName(ticket) {
      var project = this.projectForPath(ticket.path);
      return (project && project.name) || this.pathBasename(ticket.path);
    },

    // The project whose path matches what the create form's path field says.
    createProject() {
      return this.projectForPath(this.createForm.path);
    },

    pipelineOverride(choice) {
      if (!choice || choice === 'inherit') return undefined;
      return choice === 'none' ? 'none' : choice.slice('pipeline:'.length);
    },

    pipelinePreview(choice, defaults) {
      var override = this.pipelineOverride(choice);
      return override === undefined ? defaults.pipeline : override === 'none' ? '' : override;
    },

    inheritedPipelineLabel(defaults) {
      var source = defaults.pipelineSource;
      var label = source === 'default_pipeline' ? 'Use global default'
        : source === 'epic_child_pipeline' ? 'Use project epic-child default'
        : source === 'pipeline' ? 'Use project default' : 'Inherit';
      return label + ' (' + (defaults.pipeline || 'single run, no stages') + ')';
    },

    createPipelineDefaults() {
      var parentID = this.createForm.parent;
      var parent = (this.tickets || []).find(t => t.id === parentID);
      if (!parent && this.selectedEpic?.id === parentID) parent = this.selectedEpic;
      return this.projectDefaultsFor(this.createForm.path, parent?.kind === 'epic');
    },

    syncCreatePipeline() {
      this.createForm.pipeline = this.createForm.kind === 'epic' ? ''
        : this.pipelinePreview(this.createForm.pipelineChoice, this.createPipelineDefaults());
    },

    onCreatePathChange() {
      this.syncCreatePipeline();
      this.syncCreateAgent();
    },

    // The agent an untouched select shows: the one the project names, and
    // nothing else. Filling in the agent of the pipeline's first stage would
    // write that name onto the ticket, and a ticket agent overrides every
    // stage, so a two-stage pipeline would run stage two with the wrong agent.
    syncCreateAgent() {
      if (this.createTouched.agent) return;
      var project = this.createProject();
      this.createForm.agent = (project && project.agent) || '';
    },

    onPipelineChange() {
      this.createTouched.pipeline = true;
      this.syncCreatePipeline();
      this.syncCreateAgent();
    },

    // What the Start-at field currently means: the instant, or the reason it
    // means none. Read by the echo line, the preview, the submit guard and the
    // request, so all four agree by construction.
    //
    // "now" is not the absence of a schedule with leftover text — it is the
    // choice, so the text is ignored until the user switches back to "later".
    createSchedule() {
      if (this.createForm.kind === 'epic' || this.createForm.scheduleMode !== 'later') return { iso: '', at: null, error: '' };
      return parseScheduleInput(this.createForm.scheduled_at, new Date());
    },

    createScheduleISO() {
      return this.createSchedule().iso;
    },

    createScheduleError() {
      return this.createSchedule().error;
    },

    // The echo line: weekday, zone, distance and the exact instant. Null while
    // the field is empty or unreadable, when the error line takes its place.
    createScheduleEcho() {
      var iso = this.createScheduleISO();
      return iso ? scheduleEchoParts(iso, new Date()) : null;
    },

    // The presets, rebuilt per read so "tonight 18:00" stops being offered once
    // it is behind the clock.
    createSchedulePresets() {
      return schedulePresets(new Date());
    },

    // Whether a preset chip is the one currently in the field. Compared on the
    // instant, not the text: a preset writes a plain time, so a time typed by
    // hand that lands on the same minute lights the same chip.
    createPresetSelected(preset) {
      var iso = this.createScheduleISO();
      return !!iso && iso === preset.iso;
    },

    // A preset writes the time itself, not its label: the field is the one
    // input, and a value it cannot read back is a field the user cannot edit.
    pickCreatePreset(preset) {
      this.createForm.scheduled_at = localScheduleText(preset.at);
      this.onCreateScheduleChange();
    },

    // now | later. Leaving "later" drops the schedule rather than remembering
    // it: the status pin and the submit label both come off, and a ticket
    // created "now" carrying a hidden instant is the surprise that causes.
    setCreateScheduleMode(mode) {
      this.createForm.scheduleMode = mode;
      if (mode === 'now') this.createForm.scheduled_at = '';
      this.onCreateScheduleChange();
    },

    // A schedule is what moves the ticket out of open, so picking one pins the
    // status select there (the template disables it) rather than letting the
    // user build the --status todo + --at conflict the CLI rejects. Clearing
    // the field puts the select back.
    onCreateScheduleChange() {
      this.createForm.status = this.createForm.kind === 'epic' || this.createSchedulePinned() ? 'open' : 'todo';
    },

    // Whether the Status select is pinned open by the Start-at field. It is the
    // mode that pins it, not a parsed instant: leaving the select editable
    // while the user is mid-way through typing a time would flip it twice.
    createSchedulePinned() {
      return this.createForm.kind !== 'epic' && this.createForm.scheduleMode === 'later';
    },

    // The native picker writes a datetime-local value; the field itself holds
    // the grammar a person types, so the picked value is written back in the
    // spelling the parser and the CLI both take.
    onCreateSchedulePicked(value) {
      var text = pickerToScheduleText(value);
      if (!text) return;
      this.createForm.scheduled_at = text;
      this.onCreateScheduleChange();
    },

    // Submit is blocked while the field cannot be read: sending nothing would
    // create an unscheduled ticket, which is not what the form says it will do.
    createBlocked() {
      if (this.createForm.kind === 'epic') return false;
      return !!this.createScheduleError() || (this.createSchedulePinned() && !this.createScheduleISO());
    },

    toggleSidebar() {
      this.sidebarHidden = !this.sidebarHidden;
      try { localStorage.setItem('kontora-sidebar-hidden', this.sidebarHidden ? '1' : '0'); } catch (e) {}
    },

    isCollapsed(key) {
      return this.collapsedCols.includes(key);
    },

    toggleColumnCollapsed(key) {
      if (this.isCollapsed(key)) {
        this.collapsedCols = this.collapsedCols.filter(k => k !== key);
      } else {
        this.collapsedCols = this.collapsedCols.concat([key]);
      }
      try { localStorage.setItem('kontora-collapsed-cols', JSON.stringify(this.collapsedCols)); } catch (e) {}
      // Alpine recreates the column's card container on expand, so the cached
      // render state would make renderColumn skip filling the fresh (empty) element.
      delete this._rendered[key];
      this.$nextTick(() => this.renderColumn(key));
    },

    toggleShowBadges() {
      this.showPipelineBadges = !this.showPipelineBadges;
      try { localStorage.setItem('kontora-show-badges', this.showPipelineBadges ? '1' : '0'); } catch (e) {}
      this._rendered = Object.create(null);
      this.renderBoard();
    },

    toggleShowAgentMeta() {
      this.showAgentMeta = !this.showAgentMeta;
      try { localStorage.setItem('kontora-show-agent-meta', this.showAgentMeta ? '1' : '0'); } catch (e) {}
      this._rendered = Object.create(null);
      this.renderBoard();
    },

    // Number of tickets currently running on a given agent. Used by the sidebar,
    // once per agent row on every reactive read, so it reads the map that
    // recomputeBoard fills instead of filtering the whole ticket array.
    agentRunningCount(agent) {
      if (!agent) return 0;
      return this._agentRunning[agent] || 0;
    },

    // Live preview of the YAML frontmatter on the new-ticket page.
    // Mirrors the fields the server stores; not byte-for-byte, but close enough
    // to give a useful sense of what the markdown file will look like.
    get createPreviewYaml() {
      var f = this.createForm || {};
      var lines = ['---'];
      var scheduled = this.createScheduleISO();
      if (f.title)    lines.push('title: ' + JSON.stringify(f.title));
      // Creating an epic selects its agent but does not activate supervision.
      if (f.kind)     lines.push('kind: ' + f.kind);
      lines.push('status: ' + (f.kind === 'epic' ? 'open' : (scheduled ? 'open' : (f.status || 'todo'))));
      // Directly under status, because it is the field that moves it, and in
      // the spelling the daemon stores: UTC, quoted, second precision. The echo
      // line above the field shows the same instant in the local zone.
      if (f.kind !== 'epic' && scheduled) lines.push('scheduled_at: ' + JSON.stringify(scheduled));
      if (f.kind !== 'epic' && f.pipeline) lines.push('pipeline: ' + f.pipeline);
      if (f.agent)    lines.push('agent: ' + f.agent);
      if (f.path)     lines.push('path: ' + f.path);
      if (f.parent)   lines.push('parent: ' + f.parent);
      if (f.branch)   lines.push('branch: ' + f.branch);
      if (f.base_branch) lines.push('base_branch: ' + f.base_branch);
      lines.push('---');
      if (f.title) {
        lines.push('');
        lines.push('# ' + f.title);
      }
      if (f.body) {
        lines.push('');
        lines.push(f.body);
      }
      return lines.join('\n');
    },

    // The preview's lines, so the scheduled_at one can be painted in the open
    // hue. createPreviewYaml stays the single source of the text.
    get createPreviewLines() {
      return this.createPreviewYaml.split('\n').map(function (text, i) {
        return { key: i, text: text, hue: text.startsWith('scheduled_at:') };
      });
    },

    async submitCreateTicket() {
      if (this.createSubmitting) return false;
      if (!(this.createForm.title || '').trim() || !(this.createForm.path || '').trim() || this.createBlocked()) return false;
      var current = this._createContext();
      this.createSubmitting = true;
      this.createError = null;
      try {
        const body = { title: this.createForm.title, path: this.createForm.path };
        if (this.createForm.parent) body.parent = this.createForm.parent;
        // Activation belongs to the start form, not creation.
        if (this.createForm.kind === 'epic') {
          body.kind = 'epic';
          body.status = 'open';
          if (this.createForm.agent) body.agent = this.createForm.agent;
          if (this.createForm.branch) body.branch = this.createForm.branch;
          if (this.createForm.base_branch) body.base_branch = this.createForm.base_branch;
          if (this.createForm.body) body.body = this.createForm.body;
          return await this._submitCreate(body, current);
        }
        var pipeline = this.pipelineOverride(this.createForm.pipelineChoice);
        if (pipeline !== undefined) body.pipeline = pipeline;
        body.agent = this.createForm.agent || 'none';
        var scheduled = this.createScheduleISO();
        if (scheduled) {
          // A scheduled ticket waits in open; the timestamp is what moves it.
          body.scheduled_at = scheduled;
          body.status = 'open';
        } else if (this.createForm.status) {
          body.status = this.createForm.status;
        }
        if (this.createForm.body) body.body = this.createForm.body;
        if (this.createForm.branch) body.branch = this.createForm.branch;
        if (this.createForm.base_branch) body.base_branch = this.createForm.base_branch;
        return await this._submitCreate(body, current);
      } catch (e) {
        if (current()) this.createError = 'Failed to create ticket: ' + e.message;
        return false;
      } finally {
        if (current()) this.createSubmitting = false;
      }
    },

    // An obsolete response must not close another form or replace its error.
    async _submitCreate(body, current) {
      const res = await fetch('/api/tickets', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!current()) return false;
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        if (current()) this.createError = data?.error || 'Failed to create ticket';
        return false;
      }
      if (this.sheet?.type !== 'new') this.closeCreateModal();
      return true;
    },

    async handleUpload(fileList) {
      const mdFiles = [...fileList].filter(f => f.name.toLowerCase().endsWith('.md'));
      if (!mdFiles.length) {
        this.error = 'No .md files selected';
        return;
      }
      const form = new FormData();
      mdFiles.forEach(f => form.append('files', f));
      try {
        const res = await fetch('/api/tickets/upload', {
          method: 'POST',
          headers: { 'X-Kontora-Confirm': 'upload-tickets' },
          body: form,
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok && data.error) {
          this.error = data.error;
        } else if (data.errors && data.errors.length) {
          this.error = data.errors.map(e => e.file + ': ' + e.error).join('; ');
        }
      } catch (e) {
        this.error = 'Upload failed: ' + e.message;
      }
    },

    // Whether a start action goes through the init modal instead of posting
    // right away. An unmanaged ticket has nothing to run with yet; a managed one
    // still gets the form on its way out of open, so queueing it is a deliberate
    // step with the pipeline, path and branch of the run in front of the user.
    needsInitModal(ticket) {
      return this.ticketCanControl(ticket) && (this.epicCanStart(ticket) || !ticket.kontora || ticket.status === 'open');
    },

    async openInitModal(ticket) {
      if (!this.ticketCanControl(ticket)) return;
      this.closeAgentPicker(false);
      var seq = ++this._initContextSeq;
      var navigation = this._navigationSeq;
      var current = () => seq === this._initContextSeq && navigation === this._navigationSeq;
      this.initModal = false;
      this._initOpening = true;
      this.initSubmitting = false;
      try {
        var res = await fetch('/api/tickets/' + ticket.id);
        if (res.status === 404 && current()) {
          this.error = 'Ticket no longer exists';
          return;
        }
        if (res.ok) {
          var fresh = await res.json();
          if (fresh.id === ticket.id) ticket = fresh;
        }
      } catch (e) {
        // A failed preview refresh must not turn inheritance into a stored override.
      } finally {
        if (seq === this._initContextSeq) this._initOpening = false;
      }
      if (!current()) return;
      var pt = this.parseTitleTag(ticket);
      this.initForm = {
        ticketId: ticket.id,
        kind: ticket.kind || '',
        status: ticket.status || '',
        tag: pt.tag || '',
        titleRest: pt.rest || '',
        pipeline: '',
        pipelineChoice: '',
        canInheritPipeline: !ticket.pipeline,
        epicChild: !!ticket.epic_child,
        parentID: ticket.parent?.id || '',
        agent: '',
        path: ticket.path || '',
        branch: ticket.branch || '',
        autoBranch: ticket.auto_branch || '',
        ticketPath: ticket.path || '',
        // The ticket's own fields, not the resolved ones: the row edits what
        // gets written, and an empty channel list is the ticket deferring to
        // its project rather than naming nothing.
        notify: (ticket.notify || []).slice(),
        notifyChannels: (ticket.notify_channels || []).slice(),
      };
      var form = this.initForm;
      this.initNotifyOpen = false;
      this.initError = null;
      this.initModal = true;
      if (!this.configCache) {
        try {
          const res = await fetch('/api/config');
          if (res.ok) this.configCache = await res.json();
        } catch (e) {
          this.initError = 'Failed to load config';
        }
      }
      if (!this.initModal || form !== this.initForm) return;
      // ticket.agent can be a resolved stage value. Use it only for a stored
      // override; otherwise show the project agent Init will apply.
      this._initInherited = this.projectDefaultsFor(this.initForm.path, this.initForm.epicChild);
      this._initAgentFollowsProject = !ticket.agent_override;
      var agent = ticket.agent_override ? ticket.agent : this._initInherited.agent;
      await this.$nextTick();
      if (!this.initModal || form !== this.initForm) return;
      this.initForm.pipelineChoice = ticket.pipeline ? 'pipeline:' + ticket.pipeline : 'inherit';
      this.onInitPipelineChange();
      this.initForm.agent = agent;
      document.getElementById(ticket.kind === 'epic' ? 'init-agent' : 'init-pipeline')?.focus();
    },

    initPickNotifyMode(key) {
      if (!this.configCache || (!this.notifyHasChannels() && key !== 'off')) return;
      this.notifyPickMode(this.initForm, key);
      if (key === 'off' && !this.notifyHasChannels()) this.initForm.notifyChannels = [];
      this.initNotifyOpen = false;
    },

    onInitPipelineChange() {
      this.initForm.pipeline = this.initForm.kind === 'epic' ? ''
        : this.pipelinePreview(this.initForm.pipelineChoice, this._initInherited);
    },

    onInitPathChange() {
      this._initInherited = this.projectDefaultsFor(this.initForm.path, this.initForm.epicChild);
      this.onInitPipelineChange();
      if (this._initAgentFollowsProject) this.initForm.agent = this._initInherited.agent;
      this.initError = null;
    },

    async refreshInitContext(ticketID) {
      if (!this.initModal) return;
      if (ticketID && ticketID !== this.initForm.ticketId && ticketID !== this.initForm.parentID) return;
      var id = this.initForm.ticketId;
      var seq = ++this._initContextSeq;
      try {
        var res = await fetch('/api/tickets/' + id);
        if (!this.initModal || seq !== this._initContextSeq || this.initForm.ticketId !== id) return;
        if (res.status === 404) {
          this.closeInitModal();
          return;
        }
        if (!res.ok) return;
        var ticket = await res.json();
        if (!this.initModal || seq !== this._initContextSeq || ticket.id !== id) return;
        this.initForm.epicChild = !!ticket.epic_child;
        this.initForm.parentID = ticket.parent?.id || '';
        this._initInherited = this.projectDefaultsFor(this.initForm.path, this.initForm.epicChild);
        this.onInitPipelineChange();
      } catch (e) {
        // Keep the snapshot on network failure; submission still resolves inheritance on the server.
      }
    },

    closeInitModal() {
      this.closeAgentPicker(false);
      this.initModal = false;
      this._initOpening = false;
      this._initContextSeq++;
      this.initSubmitting = false;
    },

    initPipelineProject() {
      if (this.initForm.pipelineChoice !== 'inherit') return null;
      if (!['pipeline', 'epic_child_pipeline'].includes(this._initInherited.pipelineSource)) return null;
      return this.projectForPath(this.initForm.path);
    },

    // What an empty branch field shows: the name the daemon would assign, as
    // the server computed it, or a line saying it assigns one.
    branchPlaceholder(auto) {
      return auto || BRANCH_PLACEHOLDER;
    },

    // The init form's own placeholder. The server computed the name for the path
    // the ticket names, and a path the user retyped can resolve to a project
    // with another branch prefix or another naming mode. Rather than show a name
    // that would then be wrong, a diverged path falls back to the generic line.
    initBranchPlaceholder() {
      var trim = function(p) { return (p || '').trim().replace(/\/+$/, ''); };
      if (trim(this.initForm.path) !== trim(this.initForm.ticketPath)) return BRANCH_PLACEHOLDER;
      return this.branchPlaceholder(this.initForm.autoBranch);
    },

    // Whether initBranchPlaceholder() named a branch the modal can preview: the
    // field has to be empty for the automatic name to apply at all, and the
    // generic line is not a name.
    initBranchResolved() {
      return !(this.initForm.branch || '').trim() &&
        this.initBranchPlaceholder() !== BRANCH_PLACEHOLDER;
    },

    // Ordered stages of the pipeline the init form names. Empty for "none",
    // which runs the ticket once with no stage machine behind it.
    initStages() {
      if (this.initForm.kind === 'epic') return [];
      var infos = this.configCache?.pipeline_infos || [];
      var info = infos.find(i => i.name === this.initForm.pipeline);
      return (info && info.stages) || [];
    },

    async submitInitTicket() {
      if (!this.initForm.path || this.initSubmitting) return;
      this.initError = null;
      var routeError = this.notifyRouteError(this.initForm);
      if (routeError) {
        this.initError = routeError;
        this.initSubmitting = false;
        return;
      }
      this.initSubmitting = true;
      try {
        const res = await fetch('/api/tickets/' + this.initForm.ticketId + '/init', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            pipeline: this.initForm.kind === 'epic' ? undefined : this.pipelineOverride(this.initForm.pipelineChoice),
            status: this.initForm.kind === 'epic' ? 'todo' : undefined,
            path: this.initForm.path,
            agent: this.initForm.agent || 'none',
            branch: (this.initForm.branch || '').trim() || undefined,
            // Both always sent, empty included: the row shows what the ticket
            // will carry, so leaving them out would let a ticket keep a notify
            // list the user just switched off.
            notify: this.initForm.notify,
            notify_channels: this.initForm.notifyChannels,
          }),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.initError = data.error || 'Failed to start ticket';
          this.initSubmitting = false;
          return;
        }
        this.closeInitModal();
      } catch (e) {
        this.initError = 'Failed to start ticket: ' + e.message;
        this.initSubmitting = false;
      }
    },
  };
}
