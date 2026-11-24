import { localScheduleText, parseScheduleInput, pickerToScheduleText, scheduleEchoParts, schedulePresets } from './format.js';

// Shared by the routed New page and the component's initial state.
export function newCreateForm() {
  return { title: '', path: '', pipeline: '', pipelineChoice: 'inherit', agent: '', status: 'open', body: '', branch: '', base_branch: '', scheduled_at: '', scheduleMode: 'now', kind: '', parent: '' };
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

// The full New page and the init-from-an-existing-file modal.
export function kontoraCreate() {
  return {
    createError: null,
    createUploadReceipt: '',
    _createGeneration: 0,
    _createDraftActive: false,
    _createRequest: null,
    _createRequestSeq: 0,

    resetCreateForm() {
      if (!this.invalidateCreateForm()) return false;
      this.createForm = newCreateForm();
      this.createTouched = { pipeline: false, agent: false };
      this._createDraftActive = true;
      return true;
    },

    invalidateCreateForm() {
      // Leaving New cannot cancel a POST that the server may already accept.
      if (this._createRequest || this.createSubmitting) return false;
      this._createGeneration++;
      this.createError = null;
      this.createUploadReceipt = '';
      this.createSubmitting = false;
      return true;
    },

    _createContext() {
      var form = this.createForm;
      var generation = this._createGeneration;
      return () => !this._appDestroyed && form === this.createForm && generation === this._createGeneration;
    },

    async openCreateModal(navigation = ++this._navigationSeq) {
      if (this._appDestroyed || navigation !== this._navigationSeq) return false;
      if (this.currentView === 'new' && (this._createDraftActive || this._createRequest)) return true;
      this.closeAgentPicker(false);
      if (this.selectedEpic && await this.closeEpic(false) === false) return;
      if (navigation !== this._navigationSeq) return;
      if (this.selectedTicket) this.closeDetail(false);
      if (!this._createDraftActive && !this._createRequest) this.resetCreateForm();
      this.currentView = 'new';
      this.writeHash();
      await this.loadCreateConfig();
      return true;
    },

    async loadCreateConfig() {
      var current = this._createContext();
      if (!this.configCache) {
        try {
          await this.loadConfigCache({ current });
        } catch (e) {
          if (!current()) return;
          this.createError = 'Failed to load config';
        }
      }
      if (current() && !this._createRequest) this.onCreatePathChange();
    },

    closeCreateModal() {
      this.closeAgentPicker(false);
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

    createSchedule(now = new Date()) {
      if (!this.createSchedulePinned()) return { iso: '', error: '' };
      var text = this.createForm.scheduled_at.trim();
      if (!text) return { iso: '', error: 'Enter a future date and local time (YYYY-MM-DD HH:MM), or a delay such as 24h.' };
      return parseScheduleInput(text, now, { futureOnly: true });
    },

    createScheduleISO() {
      return this.createSchedule().iso;
    },

    createScheduleError() {
      return this.createSchedule().error;
    },

    createScheduleEcho() {
      var now = new Date();
      var { iso, delay } = this.createSchedule(now);
      if (delay) return { long: delay + ' after submission' };
      return iso ? scheduleEchoParts(iso, now) : null;
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
    },

    // now | later. Leaving "later" drops the schedule rather than remembering
    // it: the status pin and the submit label both come off, and a ticket
    // created "now" carrying a hidden instant is the surprise that causes.
    setCreateScheduleMode(mode) {
      this.createForm.scheduleMode = mode;
      if (mode === 'now') this.createForm.scheduled_at = '';
    },

    createEffectiveStatus() {
      if (this.createForm.kind === 'epic' || this.createSchedulePinned()) return 'open';
      return this.createForm.status === 'todo' ? 'todo' : 'open';
    },
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
    },

    // Submit is blocked while the field cannot be read: sending nothing would
    // create an unscheduled ticket, which is not what the form says it will do.
    createBlocked() {
      return this.createSchedulePinned() && !this.createScheduleISO();
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
      var schedule = this.createSchedule();
      if (f.title)    lines.push('title: ' + JSON.stringify(f.title));
      // Creating an epic selects its agent but does not activate supervision.
      if (f.kind)     lines.push('kind: ' + f.kind);
      lines.push('status: ' + this.createEffectiveStatus());
      // Relative input has no fixed instant until submission.
      if (schedule.delay) lines.push('# scheduled_at: ' + schedule.delay + ' after submission');
      else if (schedule.iso) lines.push('scheduled_at: ' + JSON.stringify(schedule.iso));
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
        return { key: i, text: text, hue: text.startsWith('scheduled_at:') || text.startsWith('# scheduled_at:') };
      });
    },

    async submitCreateTicket() {
      if (this._createRequest || this.createSubmitting) return false;
      if (!(this.createForm.title || '').trim() || !(this.createForm.path || '').trim()) return false;
      const schedule = this.createSchedule();
      if (schedule.error) {
        this.createError = schedule.error;
        return false;
      }
      this.closeAgentPicker(false);
      var current = this._createContext();
      var request = this._createRequest = ++this._createRequestSeq;
      var created = false;
      this._createDraftActive = true;
      this.createSubmitting = true;
      this.createError = null;
      try {
        const body = { title: this.createForm.title, path: this.createForm.path, status: this.createEffectiveStatus() };
        if (this.createForm.parent) body.parent = this.createForm.parent;
        // Activation belongs to the start form, not creation.
        if (this.createForm.kind === 'epic') {
          body.kind = 'epic';
          if (this.createForm.agent) body.agent = this.createForm.agent;
          if (this.createForm.branch) body.branch = this.createForm.branch;
          if (this.createForm.base_branch) body.base_branch = this.createForm.base_branch;
          if (this.createForm.body) body.body = this.createForm.body;
          created = await this._submitCreate(body, current);
          return created;
        }
        var pipeline = this.pipelineOverride(this.createForm.pipelineChoice);
        if (pipeline !== undefined) body.pipeline = pipeline;
        body.agent = this.createForm.agent || 'none';
        if (schedule.iso) body.scheduled_at = schedule.iso;
        if (this.createForm.body) body.body = this.createForm.body;
        if (this.createForm.branch) body.branch = this.createForm.branch;
        if (this.createForm.base_branch) body.base_branch = this.createForm.base_branch;
        created = await this._submitCreate(body, current);
        return created;
      } catch (e) {
        if (current()) this.createError = 'Creation result unknown: ' + e.message + '. Check the board before submitting again.';
        return false;
      } finally {
        if (this._createRequest === request) {
          this._createRequest = null;
          this.createSubmitting = false;
          if (created && current()) {
            this.resetCreateForm();
            this._createDraftActive = false;
            if (this.currentView === 'new') this.closeCreateModal();
          }
        }
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
      return true;
    },

    onUploadDragEnter(event) {
      if (!event.dataTransfer?.types.includes('Files')) return;
      event.preventDefault();
      this.uploadDragging = !this.interactionOwner();
    },

    onUploadDragOver(event) {
      if (!event.dataTransfer?.types.includes('Files')) return;
      event.preventDefault();
      if (this.interactionOwner()) event.dataTransfer.dropEffect = 'none';
    },

    onUploadDrop(event) {
      const files = event.dataTransfer?.files;
      if (!files?.length && !event.dataTransfer?.types.includes('Files')) return;
      // Cancel even a blocked file drop so the browser cannot open the file.
      event.preventDefault();
      this.uploadDragging = false;
      if (files?.length && !this.interactionOwner()) return this.handleUpload(files);
    },

    async handleUpload(fileList) {
      const newDraft = this.currentView === 'new' && this._createContext();
      if (newDraft) this.createUploadReceipt = '';
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
        const data = await res.json().catch(() => null);
        const details = [typeof data?.error === 'string' ? data.error.trim() : ''];
        if (Array.isArray(data?.errors)) {
          for (const entry of data.errors) {
            if (entry?.file && entry?.error) details.push(entry.file + ': ' + entry.error);
          }
        }
        const message = details.filter(Boolean).join('; ');
        if (!res.ok) {
          this.error = 'Upload failed (HTTP ' + res.status + ')' + (message ? ': ' + message : '');
          return;
        }
        const ids = Array.isArray(data?.tickets) ? data.tickets.map(ticket => ticket?.id).filter(Boolean) : [];
        if (message) {
          this.error = ids.length ? 'Created tickets: ' + ids.join(', ') + '. Upload errors: ' + message : message;
        } else if (ids.length && !this._appDestroyed) {
          const receipt = (ids.length === 1 ? 'Imported ticket: ' : 'Imported tickets: ') + ids.join(', ') + '.';
          if (this.currentView === 'new' && newDraft && newDraft()) this.createUploadReceipt = receipt;
          else this.showToast(receipt);
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
      const origin = this.dialogTrigger?.('init', this.ticketKey(ticket));
      const command = this.lifecycleCommand(ticket, 'initialize');
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
      if (!command || command.expected !== this._lifecycleState(ticket) || !this.needsInitModal(ticket)) {
        this.error = 'Ticket changed while opening initialization; refresh before starting it.';
        return;
      }
      this._lifecycleInitCommand = command;
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
        notify: (ticket.notify || []).slice(),
        notifyFollowsDefaults: ticket.notify == null,
        notifyChannels: (ticket.notify_channels || []).slice(),
      };
      var form = this.initForm;
      var currentForm = () => this.initModal && form === this.initForm && navigation === this._navigationSeq;
      this.initNotifyOpen = false;
      this.initError = null;
      this.rememberDialogTrigger?.('init', origin);
      this.initModal = true;
      if (!this.configCache) {
        try {
          await this.loadConfigCache({ current: currentForm });
        } catch (e) {
          if (currentForm()) this.initError = 'Failed to load config';
        }
      }
      if (!currentForm()) return;
      this.syncInitNotifyDefaults();
      // ticket.agent can be a resolved stage value. Use it only for a stored
      // override; otherwise show the project agent Init will apply.
      this._initInherited = this.projectDefaultsFor(this.initForm.path, this.initForm.epicChild);
      this._initAgentFollowsProject = !ticket.agent_override;
      var agent = ticket.agent_override ? ticket.agent : this._initInherited.agent;
      await this.$nextTick();
      if (!currentForm()) return;
      // Creation stamps inherited names but stores a standalone run with no
      // pipeline. Preserve that for managed drafts; unmanaged blanks can inherit.
      this.initForm.pipelineChoice = ticket.pipeline === 'none' ? 'none'
        : ticket.pipeline ? 'pipeline:' + ticket.pipeline
        : ticket.kontora && ticket.status === 'open' ? 'none' : 'inherit';
      this.onInitPipelineChange();
      this.initForm.agent = agent;
      if (!this.dialogFocusManaged?.()) document.getElementById(ticket.kind === 'epic' ? 'init-agent' : 'init-pipeline')?.focus();
    },

    syncInitNotifyDefaults() {
      if (!this.configCache || !this.initForm.notifyFollowsDefaults) return;
      var project = this.projectForPath(this.initForm.path);
      this.initForm.notify = (project?.notify ?? this.configCache.default_notify ?? []).slice();
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
      this.syncInitNotifyDefaults();
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
      if (!this.configCache) {
        this.initError = 'Waiting for notification configuration. Close and reopen this dialog if loading does not finish.';
        return;
      }
      this.syncInitNotifyDefaults();
      var routeError = this.notifyRouteError(this.initForm);
      if (routeError) {
        this.initError = routeError;
        this.initSubmitting = false;
        return;
      }
      const form = this.initForm;
      const command = this._lifecycleInitCommand;
      if (!command || command.ticketKey !== form.ticketId) return { outcome: 'refused' };
      this.initSubmitting = true;
      try {
        const result = await this.dispatchLifecycle(form.ticketId, { ...command, body: {
          pipeline: form.kind === 'epic' ? undefined : this.pipelineOverride(form.pipelineChoice),
          status: form.kind === 'epic' ? 'todo' : undefined,
          path: form.path,
          agent: form.agent || 'none',
          branch: (form.branch || '').trim() || undefined,
          notify: form.notifyFollowsDefaults ? undefined : (form.notify || []).slice(),
          notify_channels: (form.notifyChannels || []).slice(),
        } });
        if (form === this.initForm) {
          if (result.outcome === 'success') this.closeInitModal();
          else this.initError = result.error || 'Ticket could not start (' + result.outcome + ').';
        }
        return result;
      } finally {
        if (form === this.initForm) this.initSubmitting = false;
      }
    },
  };
}
