// What a branch field says when no name can be shown: the ticket carries no
// branch and the daemon names one at pickup.
const BRANCH_PLACEHOLDER = 'daemon assigns branch when run starts';

// The create-ticket modal and the init-from-an-existing-file modal.
export function kontoraCreate() {
  return {
    async openCreateModal() {
      this.createForm = { title: '', path: '', pipeline: '', agent: '', status: 'todo', body: '', branch: '', base_branch: '' };
      this.createTouched = { pipeline: false, agent: false };
      this.currentView = 'new';
      this.writeHash();
      this.error = null;
      if (!this.configCache) {
        try {
          const res = await fetch('/api/config');
          if (res.ok) this.configCache = await res.json();
        } catch (e) {
          this.error = 'Failed to load config';
        }
      }
    },

    closeCreateModal() {
      this.currentView = 'board';
      this.createSubmitting = false;
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
          var norm = (raw || '').trim().replace(/\/+$/, '');
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
      var typed = (path || '').trim().replace(/\/+$/, '');
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

    // Prefill the selects the user has not touched from the project that owns
    // the typed path. cli.New applies the same defaults on submit, so this is a
    // preview of what the server will do, not the mechanism.
    //
    // Every untouched select is rewritten on each keystroke, including back to
    // blank once the path stops matching: leaving the previous project's values
    // in place would create the ticket with a pipeline picked for another repo.
    onCreatePathChange() {
      var project = this.createProject();
      if (!this.createTouched.pipeline) this.createForm.pipeline = (project && project.pipeline) || '';
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
      this.syncCreateAgent();
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
      if (f.title)    lines.push('title: ' + JSON.stringify(f.title));
      lines.push('status: ' + (f.status || 'todo'));
      if (f.pipeline) lines.push('pipeline: ' + f.pipeline);
      if (f.agent)    lines.push('agent: ' + f.agent);
      if (f.path)     lines.push('path: ' + f.path);
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

    async submitCreateTicket() {
      if (!this.createForm.title || !this.createForm.path) return;
      this.createSubmitting = true;
      this.error = null;
      try {
        const body = { title: this.createForm.title, path: this.createForm.path };
        // The selects carry the resolved values, so an empty one is a
        // deliberate opt-out. "none" says so; a blank field would make the
        // daemon inherit the project default the user just cleared.
        body.pipeline = this.createForm.pipeline || 'none';
        body.agent = this.createForm.agent || 'none';
        if (this.createForm.status) body.status = this.createForm.status;
        if (this.createForm.body) body.body = this.createForm.body;
        if (this.createForm.branch) body.branch = this.createForm.branch;
        if (this.createForm.base_branch) body.base_branch = this.createForm.base_branch;
        const res = await fetch('/api/tickets', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          this.error = data.error || 'Failed to create ticket';
          this.createSubmitting = false;
          return;
        }
        this.closeCreateModal();
      } catch (e) {
        this.error = 'Failed to create ticket: ' + e.message;
        this.createSubmitting = false;
      }
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

    async openInitModal(ticket) {
      var pt = this.parseTitleTag(ticket);
      this.initForm = {
        ticketId: ticket.id,
        status: ticket.status || '',
        tag: pt.tag || '',
        titleRest: pt.rest || '',
        pipeline: '',
        agent: '',
        path: ticket.path || '',
        branch: ticket.branch || '',
        autoBranch: ticket.auto_branch || '',
        ticketPath: ticket.path || '',
      };
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
      // The form shows what the ticket would run with, not a "project default"
      // placeholder: the fields the ticket leaves blank are filled from the
      // project that owns the path, the same values the daemon would resolve.
      this._initInherited = this.projectDefaultsFor(this.initForm.path);
      var pipeline = ticket.pipeline || this._initInherited.pipeline;
      var agent = ticket.agent || this._initInherited.agent;
      // Defer select values until after x-for has created <option> elements.
      await this.$nextTick();
      this.initForm.pipeline = pipeline;
      this.initForm.agent = agent;
      document.getElementById('init-pipeline')?.focus();
    },

    // Re-apply the project defaults after the path field changes. A value the
    // user chose is kept; one that only got there by inheriting from the
    // previous path is replaced, so retargeting the ticket cannot start it with
    // another repository's pipeline.
    onInitPathChange() {
      var prev = this._initInherited || { pipeline: '', agent: '' };
      var next = this.projectDefaultsFor(this.initForm.path);

      if (!this.initForm.pipeline || this.initForm.pipeline === prev.pipeline) {
        this.initForm.pipeline = next.pipeline;
      }
      if (!this.initForm.agent || this.initForm.agent === prev.agent) {
        this.initForm.agent = next.agent;
      }
      this._initInherited = next;
    },

    closeInitModal() {
      this.initModal = false;
      this.initSubmitting = false;
    },

    // The project the pipeline value came from, or null. The badge sits on the
    // pipeline label row and claims that provenance, so it tests the value: the
    // ticket's own field and the select both override the inherited one, and
    // the path resolving to a project says nothing about either.
    initPipelineProject() {
      if (!this.initForm.pipeline || this.initForm.pipeline !== this._initInherited.pipeline) return null;
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
      var infos = this.configCache?.pipeline_infos || [];
      var info = infos.find(i => i.name === this.initForm.pipeline);
      return (info && info.stages) || [];
    },

    async submitInitTicket() {
      if (!this.initForm.path) return;
      this.initSubmitting = true;
      this.initError = null;
      try {
        const res = await fetch('/api/tickets/' + this.initForm.ticketId + '/init', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          // The selects carry the resolved values, so an empty one is a
          // deliberate opt-out. "none" says so; a blank field would make the
          // daemon inherit the project default the user just cleared.
          body: JSON.stringify({
            pipeline: this.initForm.pipeline || 'none',
            path: this.initForm.path,
            agent: this.initForm.agent || 'none',
            branch: (this.initForm.branch || '').trim() || undefined,
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
