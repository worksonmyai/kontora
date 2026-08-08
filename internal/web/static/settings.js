// Settings view. Merged into the kontora() component by app.js, so `this` here
// is the same Alpine object the board and the new-ticket form run on.
//
// The load-bearing constraint is that PUT /api/config/raw replaces the whole
// file. The user's config.yaml is heavily commented and holds keys this form
// does not model, so the document is never rebuilt from the form model: it is
// parsed once into a yaml Document, and a save mutates only the nodes whose
// paths changed.

// Restart-only paths. reloadConfig keeps the running value for these and only
// logs a warning (internal/daemon/reload.go, pinRestartOnly), so a save that
// touches one must not claim the change took effect.
const SETTINGS_RESTART_ONLY = [
  'tickets_dir', 'worktrees_dir', 'logs_dir', 'instance_name', 'max_concurrent_agents',
  'web.host', 'web.port', 'web.token',
];

// Mirrors config.DefaultFailurePatterns (internal/config/defaults.go). Shown
// read-only for agents that declare no failure_patterns of their own.
const SETTINGS_DEFAULT_FAILURE_PATTERNS = [
  '(?im)^\\s*API Error:',
  '(?i)Please run /login',
  '(?i)usage limit reached',
  "(?i)You've hit your (usage )?limit",
  '(?i)Prompt is too long',
  '(?i)insufficient_quota',
  '(?i)exceeded your current quota',
  '(?i)Rate limit reached for',
];

// config.defaultReworkPrompt (internal/config/defaults.go). applyDefaults
// injects this stage when the file has no stages.rework key.
const SETTINGS_REWORK_PROMPT = `Ticket: {{ .Ticket.Title }}

The reviewer requested changes. Their feedback:

{{ plannotatorReview }}

Apply the changes and continue the work.`;

const SETTINGS_REWORK_TIMEOUT = '30m';

const SETTINGS_SECTIONS = [
  { key: 'general', label: 'general', blurb: 'Where kontora keeps things, and how many agents it runs at once.' },
  { key: 'environment', label: 'environment', blurb: 'Injected into every agent process. Per-agent entries override these.' },
  { key: 'agents', label: 'agents', blurb: 'Binaries kontora spawns. Anything with a CLI works.' },
  { key: 'stages', label: 'stages', blurb: 'Prompt templates. A pipeline runs them in order, sharing one git worktree.' },
  { key: 'pipelines', label: 'pipelines', blurb: 'Stages wired to agents, with a routing rule per step.' },
  { key: 'projects', label: 'projects', blurb: 'Defaults a new ticket picks up from the repo it points at.' },
  { key: 'web', label: 'web', blurb: 'This dashboard and the HTTP API the CLI talks to.' },
  { key: 'plannotator', label: 'plannotator', blurb: 'Human review pass. Rework feeds its notes back into the built-in rework stage.' },
  { key: 'statuses', label: 'statuses', blurb: 'Extra board columns a pipeline step can route to.' },
  { key: 'display', label: 'display', blurb: 'Browser-local. Never written to config.yaml.' },
];

// Root scalar keys the general section edits, with the value applyDefaults
// would supply when the key is absent (internal/config/config.go).
const SETTINGS_GENERAL_FIELDS = [
  { key: 'tickets_dir', placeholder: '~/.kontora/tickets', help: 'Markdown tickets are read from and written to this directory.' },
  { key: 'worktrees_dir', placeholder: '~/.kontora/worktrees', help: 'One git worktree per ticket lives here.' },
  { key: 'logs_dir', placeholder: '~/.kontora/logs', help: 'Per-stage agent output. The LOGS pane reads from here.' },
  { key: 'branch_prefix', placeholder: 'kontora', help: "Fallback when the ticket's project sets none." },
  { key: 'default_agent', placeholder: 'claude', help: 'Used when a pipeline step or ticket names no agent.' },
  { key: 'max_concurrent_agents', placeholder: '3', help: 'Hard cap on tmux sessions running at once.' },
  { key: 'instance_name', placeholder: "this machine's hostname", help: 'Identifies this daemon when several share one tickets_dir.' },
];

const SETTINGS_PLANNOTATOR_FIELDS = [
  { key: 'binary', placeholder: 'plannotator', help: 'Reviewer binary spawned for a human review pass.' },
  { key: 'timeout', placeholder: '30m', help: 'How long a review may stay open before it is cancelled.' },
  { key: 'reviews_dir', placeholder: '~/.kontora/plannotator-reviews', help: 'Where review transcripts are stored.' },
];

// config.builtinStatuses minus archived, which never reaches the board.
const SETTINGS_BUILTIN_STATUSES = ['open', 'todo', 'in_progress', 'paused', 'human_review', 'done', 'cancelled'];

const SETTINGS_TOKENS = [
  { token: '{{ .Ticket.ID }}', help: 'Short ticket id, e.g. kon-f7ha' },
  { token: '{{ .Ticket.Title }}', help: 'Ticket title line' },
  { token: '{{ .Ticket.Description }}', help: 'Full markdown body of the ticket' },
  { token: '{{ file "PLAN.md" }}', help: 'Reads a file from the shared worktree — how one stage hands work to the next' },
  { token: '{{ plannotatorReview }}', help: 'Reviewer feedback from the last plannotator run' },
];

// config.validStatusNameRe (internal/config/config.go).
const SETTINGS_STATUS_RE = /^[a-z][a-z0-9_]*$/;

// The grammar time.ParseDuration accepts: one or more signed decimal numbers,
// each with a unit. A narrower "digits + one unit" pattern would reject 1h30m
// and 1.5h, both of which the daemon takes.
const SETTINGS_DURATION_RE = /^[-+]?(\d+(\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)([-+]?(\d+(\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h))*$/;

function kontoraSettings() {
  return {
    settingsSection: 'stages',
    // The editable form model, and a deep clone of what was parsed from disk.
    // Dirty state and the diff compare the two.
    settingsConfig: null,
    settingsBaseline: null,
    settingsOpenStage: null,
    settingsOpenAgent: null,
    settingsErrors: [],
    settingsLoading: false,
    settingsSaving: false,
    settingsSavedAt: '',
    settingsSavedRestart: false,
    settingsDiffOpen: false,
    settingsGuard: false,
    settingsGuardTarget: null,
    // 'ok' once the file is parsed; 'unavailable' when the daemon has no config
    // path (501); 'parse-error' when the YAML itself does not parse.
    settingsState: 'idle',
    settingsLoadError: '',
    // True when re-serializing the freshly parsed document does not reproduce
    // the file byte for byte. Saving then reformats it whatever the form does.
    settingsReformats: false,
    settingsNewStage: '',
    settingsNewStageOpen: false,
    settingsNewEnvOpen: false,
    settingsNewEnvKey: '',
    settingsNewStatus: '',
    settingsNewStatusOpen: false,
    settingsShowToken: false,

    // The parsed Document, the text it came from, and the yaml module. Never
    // rendered and never part of the form model, so no template reads them.
    _settingsDoc: null,
    _settingsRawText: '',
    _settingsYAML: null,

    settingsSections: SETTINGS_SECTIONS,
    settingsGeneralFields: SETTINGS_GENERAL_FIELDS,
    settingsPlannotatorFields: SETTINGS_PLANNOTATOR_FIELDS,
    settingsBuiltinStatuses: SETTINGS_BUILTIN_STATUSES,
    settingsTokens: SETTINGS_TOKENS,

    // ---- loading -----------------------------------------------------------

    async openSettings() {
      this.currentView = 'settings';
      if (this.settingsConfig || this.settingsLoading) return;
      this.settingsLoading = true;
      this.settingsLoadError = '';
      try {
        const res = await fetch('/api/config/raw');
        if (res.status === 401) { this.needsAuth = true; this.settingsState = 'auth'; return; }
        if (res.status === 501) { this.settingsState = 'unavailable'; return; }
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const data = await res.json();
        await this._settingsParse(data.content || '');
      } catch (e) {
        this.settingsState = 'parse-error';
        this.settingsLoadError = e.message || String(e);
      } finally {
        this.settingsLoading = false;
      }
    },

    // Parse the raw file into a Document and derive the form model from it.
    // Split out from openSettings so a test can drive it with fixture text.
    async _settingsParse(text) {
      const yaml = await this._settingsLoadYAML();
      const doc = yaml.parseDocument(text);
      if (doc.errors && doc.errors.length) throw new Error(doc.errors[0].message);
      this._settingsDoc = doc;
      this._settingsRawText = text;
      this.settingsReformats = String(yaml.parseDocument(text)) !== text;
      this.settingsConfig = settingsModel(doc.toJS({ maxAliasCount: -1 }) || {});
      this.settingsBaseline = settingsClone(this.settingsConfig);
      this.settingsState = 'ok';
      this.settingsErrors = [];
      this.settingsSavedAt = '';
      this.settingsOpenStage = null;
      this.settingsOpenAgent = null;
    },

    async _settingsLoadYAML() {
      if (!this._settingsYAML) this._settingsYAML = await import('/vendor/yaml@2.8.1/yaml.mjs');
      return this._settingsYAML;
    },

    // ---- dirty state and diff ---------------------------------------------

    // Methods, not getters: kontora() merges this object with Object.assign,
    // which would call a getter once and copy the value it happened to return.
    settingsChangedPaths() {
      if (!this.settingsConfig || !this.settingsBaseline) return [];
      return settingsChangedPaths(this.settingsConfig, this.settingsBaseline);
    },

    settingsDirty() {
      return this.settingsChangedPaths().length > 0;
    },

    // Paths under a prefix, so a stage card can show its own dirty dot.
    settingsPathDirty(prefix) {
      return this.settingsChangedPaths().some(p => p === prefix || p.startsWith(prefix + '.'));
    },

    settingsDiffHunks() {
      const now = settingsFlatten(this.settingsConfig);
      const was = settingsFlatten(this.settingsBaseline);
      return this.settingsChangedPaths().map(path => ({
        path,
        lines: settingsDiffLines(was[path] === undefined ? '' : was[path], now[path] === undefined ? '' : now[path]),
      }));
    },

    // ---- validation --------------------------------------------------------

    // The checks whose wording can be guaranteed against config.Validate().
    // Anything else (durations, max_concurrent_agents) gets a live border but
    // no invented message: the daemon owns that text.
    settingsClientErrors() {
      const cfg = this.settingsConfig;
      if (!cfg) return [];
      const errs = [];
      for (const name of Object.keys(cfg.agents)) {
        if (!cfg.agents[name].binary.trim()) errs.push(`agent "${name}": binary is required`);
      }
      const wanted = cfg.general.default_agent.trim();
      if (wanted && !cfg.agents[wanted]) errs.push(`default_agent "${wanted}": not found in agents`);
      for (const name of Object.keys(cfg.projects)) {
        if (!cfg.projects[name].path.trim()) errs.push(`project "${name}": path is required`);
      }
      for (const s of cfg.statuses) {
        if (!SETTINGS_STATUS_RE.test(s)) errs.push(`custom status "${s}": must match [a-z][a-z0-9_]*`);
      }
      return errs;
    },

    // Non-blocking: flips a border while typing, never blocks the PUT.
    settingsDurationValid(value) {
      const v = (value || '').trim();
      return v === '' || SETTINGS_DURATION_RE.test(v);
    },

    settingsIntValid(value) {
      const v = (value || '').trim();
      return v === '' || /^\d+$/.test(v);
    },

    settingsIsRestartOnly(path) {
      return SETTINGS_RESTART_ONLY.includes(path);
    },

    // ---- saving ------------------------------------------------------------

    async saveSettings() {
      const errs = this.settingsClientErrors();
      if (errs.length) {
        this.settingsErrors = errs;
        this.settingsDiffOpen = true;
        return false;
      }
      this.settingsErrors = [];
      this.settingsSaving = true;
      const changed = this.settingsChangedPaths();
      try {
        if (await this._settingsStale()) {
          this.settingsErrors = ['config.yaml changed on disk after this page read it. Saving now would overwrite that change. Reload the page, then redo your edits.'];
          this.settingsDiffOpen = true;
          return false;
        }
        const content = await this._settingsWrite();
        const res = await fetch('/api/config/raw', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content }),
        });
        if (res.status === 401) { this.needsAuth = true; return false; }
        if (res.status === 204) {
          this._settingsRawText = content;
          this.settingsBaseline = settingsClone(this.settingsConfig);
          this.settingsSavedAt = settingsClock(new Date());
          this.settingsSavedRestart = changed.some(p => this.settingsIsRestartOnly(p));
          this.settingsDiffOpen = false;
          await this._settingsRefreshConfigCache();
          return true;
        }
        const data = await res.json().catch(() => ({}));
        this.settingsErrors = [data.error || 'HTTP ' + res.status];
        this.settingsDiffOpen = true;
        return false;
      } catch (e) {
        // settingsErrors only renders inside the diff panel, which is collapsed
        // by default. Without this a stopped daemon looks like a dead button.
        this.settingsErrors = [e.message || String(e)];
        this.settingsDiffOpen = true;
        return false;
      } finally {
        this.settingsSaving = false;
      }
    },

    // PUT replaces the whole file and the daemon takes no precondition, so a
    // Settings tab left open would overwrite an edit made meanwhile in $EDITOR,
    // by kontora init, or from a second tab. Re-read the file right before
    // writing it. That narrows the window rather than closing it; only an
    // If-Match on the daemon would close it.
    async _settingsStale() {
      const res = await fetch('/api/config/raw');
      if (!res.ok) return false;
      const data = await res.json();
      return (data.content || '') !== this._settingsRawText;
    },

    // The daemon reloaded, so the resolved config the rest of the UI reads is
    // stale: a new custom status is a new board column, a new project is a new
    // option in the new-ticket form. Nothing broadcasts a config reload.
    async _settingsRefreshConfigCache() {
      try {
        const res = await fetch('/api/config');
        if (res.ok) this.configCache = await res.json();
      } catch (e) {
        // The file was written. A stale cache clears on the next page load.
      }
    },

    // Apply the changed paths to the parsed document and return the text a save
    // would send. The document is re-parsed from the text on disk first: a
    // rejected save left its values in the old document, and those paths are no
    // longer changed paths, so nothing would overwrite them.
    async _settingsWrite() {
      const yaml = await this._settingsLoadYAML();
      this._settingsDoc = yaml.parseDocument(this._settingsRawText);
      settingsApply(yaml, this._settingsDoc, this.settingsConfig, this.settingsBaseline);
      return String(this._settingsDoc);
    },

    discardSettings() {
      this.settingsConfig = settingsClone(this.settingsBaseline);
      this.settingsErrors = [];
      this.settingsDiffOpen = false;
      this.settingsSavedAt = '';
    },

    // Restore one stage to what is on disk. A stage the baseline never had
    // (added here, or the built-in rework the file does not declare) is dropped
    // from the model instead, so reverting leaves no half-created key.
    revertSettingsStage(name) {
      const base = this.settingsBaseline.stages[name];
      if (base) this.settingsConfig.stages[name] = settingsClone(base);
      else {
        delete this.settingsConfig.stages[name];
        if (this.settingsOpenStage === name) this.settingsOpenStage = null;
      }
      this.settingsSavedAt = '';
    },

    // ---- editing helpers ---------------------------------------------------

    settingsStageNames() {
      return Object.keys(this.settingsConfig?.stages || {}).sort();
    },

    settingsAgentNames() {
      return Object.keys(this.settingsConfig?.agents || {}).sort();
    },

    settingsProjectNames() {
      return Object.keys(this.settingsConfig?.projects || {}).sort();
    },

    settingsPipelineNames() {
      return Object.keys(this.settingsConfig?.pipelines || {}).sort();
    },

    settingsEnvKeys() {
      return Object.keys(this.settingsConfig?.environment || {});
    },

    // Pipelines whose steps run this stage. Answers "if I edit this prompt,
    // what breaks?" without leaving the card.
    settingsStagePipelines(stage) {
      const pipes = this.settingsConfig?.pipelines || {};
      return Object.keys(pipes).filter(p => (pipes[p] || []).some(s => s.stage === stage)).sort();
    },

    settingsToggleStage(name) {
      this.settingsOpenStage = this.settingsOpenStage === name ? null : name;
    },

    settingsToggleAgent(name) {
      this.settingsOpenAgent = this.settingsOpenAgent === name ? null : name;
    },

    settingsAddStage() {
      const name = (this.settingsNewStage || '').trim();
      if (!name || this.settingsConfig.stages[name]) return;
      this.settingsConfig.stages[name] = { prompt: '', timeout: '', builtin: false };
      this.settingsOpenStage = name;
      this.settingsNewStage = '';
      this.settingsNewStageOpen = false;
      this.settingsSection = 'stages';
    },

    settingsAddEnv() {
      const key = (this.settingsNewEnvKey || '').trim();
      if (!key || key in this.settingsConfig.environment) return;
      this.settingsConfig.environment[key] = '';
      this.settingsNewEnvKey = '';
      this.settingsNewEnvOpen = false;
    },

    settingsRemoveEnv(key) {
      delete this.settingsConfig.environment[key];
    },

    settingsAddStatus() {
      const name = (this.settingsNewStatus || '').trim();
      if (!name || this.settingsConfig.statuses.includes(name)) return;
      this.settingsConfig.statuses.push(name);
      this.settingsNewStatus = '';
      this.settingsNewStatusOpen = false;
    },

    settingsRemoveStatus(name) {
      this.settingsConfig.statuses = this.settingsConfig.statuses.filter(s => s !== name);
    },

    // Splice a template token into the prompt at the caret, then put the caret
    // back after it. Without the refocus the user loses their place on every
    // chip click.
    insertTemplateToken(id, stage, token) {
      const el = document.getElementById(id);
      const cur = this.settingsConfig.stages[stage].prompt || '';
      const start = el ? el.selectionStart : cur.length;
      const end = el ? el.selectionEnd : cur.length;
      this.settingsConfig.stages[stage].prompt = cur.slice(0, start) + token + cur.slice(end);
      const caret = start + token.length;
      requestAnimationFrame(() => {
        const live = document.getElementById(id);
        if (!live) return;
        live.focus();
        live.setSelectionRange(caret, caret);
      });
      return caret;
    },

    // ---- read-only presentation -------------------------------------------

    // failure_patterns is three-valued: absent means the built-in defaults,
    // [] disables detection, and a list overrides.
    settingsFailurePatterns(name) {
      const agent = this.settingsConfig?.agents?.[name];
      if (!agent || agent.failure_patterns === null) {
        return { mode: 'default', patterns: SETTINGS_DEFAULT_FAILURE_PATTERNS };
      }
      if (agent.failure_patterns.length === 0) return { mode: 'disabled', patterns: [] };
      return { mode: 'override', patterns: agent.failure_patterns };
    },

    settingsAgentCommand(name) {
      const agent = this.settingsConfig?.agents?.[name];
      if (!agent) return '';
      return [agent.binary].concat(agent.args.split('\n').filter(Boolean)).join(' ');
    },

    settingsAgentSkipsPermissions(name) {
      return (this.settingsConfig?.agents?.[name]?.args || '').split('\n').includes('--dangerously-skip-permissions');
    },

    settingsPipelineEnds(name) {
      const steps = this.settingsConfig?.pipelines?.[name] || [];
      if (!steps.length) return '';
      return (steps[steps.length - 1].on_success || '').replace(/_/g, ' ');
    },

    settingsWebAdvisory() {
      const web = this.settingsConfig?.web;
      if (!web) return '';
      const host = web.host.trim() || '127.0.0.1';
      const port = web.port.trim() || '8080';
      if (!web.token.trim()) {
        return `No token. /api and /ws are open to anything that can reach ${host}:${port}. Set one before binding to anything but loopback.`;
      }
      if (host === '127.0.0.1' || host === 'localhost') {
        return `Token set. Open http://${host}:${port}/?token=… once to store the cookie. Plain HTTP sends it in clear — put the daemon behind TLS on any untrusted network.`;
      }
      return `Bound to ${host} with a token. The token is the only thing gating remote access, and agents run with --dangerously-skip-permissions. Fine on a tailnet; use TLS anywhere else.`;
    },

    settingsSectionCount(key) {
      const cfg = this.settingsConfig;
      if (!cfg) return '';
      if (key === 'environment') return Object.keys(cfg.environment).length;
      if (key === 'agents') return Object.keys(cfg.agents).length;
      if (key === 'stages') return Object.keys(cfg.stages).length;
      if (key === 'pipelines') return Object.keys(cfg.pipelines).length;
      if (key === 'projects') return Object.keys(cfg.projects).length;
      if (key === 'statuses') return cfg.statuses.length;
      return '';
    },

    // ---- navigation guard --------------------------------------------------

    // The one place currentView changes. Opening Settings loads it; leaving a
    // dirty Settings opens the guard instead of navigating.
    async gotoView(view) {
      if (this.currentView === 'settings' && view !== 'settings' && this.settingsDirty()) {
        this.settingsGuardTarget = view;
        this.settingsGuard = true;
        return;
      }
      if (view === 'settings') { await this.openSettings(); this.writeHash(); return; }
      if (view === 'new') { await this.openCreateModal(); this.writeHash(); return; }
      this.currentView = 'board';
      this.writeHash();
    },

    settingsGuardDiscard() {
      this.discardSettings();
      const target = this.settingsGuardTarget;
      this.settingsGuard = false;
      this.settingsGuardTarget = null;
      return this.gotoView(target || 'board');
    },

    async settingsGuardSave() {
      if (!(await this.saveSettings())) return;
      const target = this.settingsGuardTarget;
      this.settingsGuard = false;
      this.settingsGuardTarget = null;
      await this.gotoView(target || 'board');
    },
  };
}

// Deep copy of the form model. Not structuredClone: settingsConfig and
// settingsBaseline live on the Alpine component, so every read hands back a
// reactive Proxy, and structuredClone throws DataCloneError on a Proxy. The
// model holds only strings, numbers, booleans, null, arrays and plain objects.
function settingsClone(value) {
  if (Array.isArray(value)) return value.map(settingsClone);
  if (value !== null && typeof value === 'object') {
    const out = {};
    for (const [k, v] of Object.entries(value)) out[k] = settingsClone(v);
    return out;
  }
  return value;
}

// Build the form model from the parsed document's plain-JS view. Values are
// strings so an input can round-trip them without a type coercion step;
// failure_patterns keeps its three-valued nil / [] / list shape.
function settingsModel(raw) {
  const str = v => (v === undefined || v === null ? '' : String(v));
  const model = {
    general: {},
    environment: {},
    agents: {},
    stages: {},
    pipelines: {},
    projects: {},
    statuses: (raw.statuses || []).map(String),
    web: { host: str(raw.web?.host), port: str(raw.web?.port), token: str(raw.web?.token) },
    plannotator: {
      binary: str(raw.plannotator?.binary),
      timeout: str(raw.plannotator?.timeout),
      reviews_dir: str(raw.plannotator?.reviews_dir),
    },
    auto_pick_up: raw.auto_pick_up === undefined || raw.auto_pick_up === null ? true : !!raw.auto_pick_up,
  };
  for (const f of SETTINGS_GENERAL_FIELDS) model.general[f.key] = str(raw[f.key]);
  for (const [k, v] of Object.entries(raw.environment || {})) model.environment[k] = str(v);
  for (const [name, agent] of Object.entries(raw.agents || {})) {
    model.agents[name] = {
      binary: str(agent?.binary),
      args: (agent?.args || []).map(String).join('\n'),
      failure_patterns: agent?.failure_patterns === undefined || agent?.failure_patterns === null
        ? null
        : agent.failure_patterns.map(String),
    };
  }
  for (const [name, stage] of Object.entries(raw.stages || {})) {
    model.stages[name] = { prompt: str(stage?.prompt), timeout: str(stage?.timeout), builtin: false };
  }
  // applyDefaults injects rework when the file declares no stages.rework, and
  // the daemon runs it. Show it, marked, rather than hiding a live stage.
  if (!model.stages.rework) {
    model.stages.rework = { prompt: SETTINGS_REWORK_PROMPT, timeout: SETTINGS_REWORK_TIMEOUT, builtin: true };
  }
  for (const [name, steps] of Object.entries(raw.pipelines || {})) {
    model.pipelines[name] = (steps || []).map(s => ({
      stage: str(s?.stage),
      agent: str(s?.agent),
      on_success: str(s?.on_success),
      on_failure: str(s?.on_failure),
      max_retries: Number(s?.max_retries) || 0,
    }));
  }
  for (const [name, project] of Object.entries(raw.projects || {})) {
    model.projects[name] = {
      path: str(project?.path),
      pipeline: str(project?.pipeline),
      agent: str(project?.agent),
      branch_prefix: str(project?.branch_prefix),
    };
  }
  return model;
}

// path -> string for every YAML-backed field. Display preferences are absent by
// construction: they live in localStorage and never enter the file.
function settingsFlatten(cfg) {
  const out = {};
  if (!cfg) return out;
  for (const f of SETTINGS_GENERAL_FIELDS) out[f.key] = cfg.general[f.key];
  out.auto_pick_up = cfg.auto_pick_up ? 'true' : 'false';
  for (const [k, v] of Object.entries(cfg.environment)) out[`environment.${k}`] = v;
  for (const [name, agent] of Object.entries(cfg.agents)) {
    out[`agents.${name}.binary`] = agent.binary;
    out[`agents.${name}.args`] = agent.args;
  }
  for (const [name, stage] of Object.entries(cfg.stages)) {
    out[`stages.${name}.prompt`] = stage.prompt;
    out[`stages.${name}.timeout`] = stage.timeout;
  }
  for (const [name, project] of Object.entries(cfg.projects)) {
    out[`projects.${name}.path`] = project.path;
    out[`projects.${name}.pipeline`] = project.pipeline;
    out[`projects.${name}.agent`] = project.agent;
    out[`projects.${name}.branch_prefix`] = project.branch_prefix;
  }
  out['web.host'] = cfg.web.host;
  out['web.port'] = cfg.web.port;
  out['web.token'] = cfg.web.token;
  out['plannotator.binary'] = cfg.plannotator.binary;
  out['plannotator.timeout'] = cfg.plannotator.timeout;
  out['plannotator.reviews_dir'] = cfg.plannotator.reviews_dir;
  out.statuses = cfg.statuses.join('\n');
  return out;
}

function settingsChangedPaths(cfg, baseline) {
  const now = settingsFlatten(cfg);
  const was = settingsFlatten(baseline);
  const paths = new Set([...Object.keys(now), ...Object.keys(was)]);
  const changed = [];
  for (const p of paths) {
    // A key present on only one side counts as changed only when its value is
    // non-empty: adding a blank environment row is not an edit yet.
    const a = now[p] === undefined ? '' : now[p];
    const b = was[p] === undefined ? '' : was[p];
    if (a !== b) changed.push(p);
  }
  return changed.sort();
}

// Line diff for one path. Enough for prompt edits and needs no library: skip
// the shared head and tail, emit the differing middles, keep one context line
// on each side.
function settingsDiffLines(before, after) {
  const a = String(before).split('\n');
  const b = String(after).split('\n');
  let head = 0;
  while (head < a.length && head < b.length && a[head] === b[head]) head++;
  let tail = 0;
  while (tail < a.length - head && tail < b.length - head && a[a.length - 1 - tail] === b[b.length - 1 - tail]) tail++;

  const lines = [];
  if (head > 0) lines.push({ kind: 'context', text: a[head - 1] });
  for (let i = head; i < a.length - tail; i++) lines.push({ kind: 'del', text: a[i] });
  for (let i = head; i < b.length - tail; i++) lines.push({ kind: 'add', text: b[i] });
  if (tail > 0) lines.push({ kind: 'context', text: a[a.length - tail] });
  return lines;
}

// Write the changed paths back into the parsed document. An existing scalar is
// mutated in place so its parsed style survives — a | block prompt stays a |
// block and keeps its trailing comment. Everything else goes through setIn,
// which appends a new key or replaces the node in place.
function settingsApply(yaml, doc, cfg, baseline) {
  for (const path of settingsWritePaths(doc, cfg, baseline)) {
    const { keys, value } = settingsNodeFor(path, cfg);
    // deleteIn throws rather than no-ops when it cannot descend: a key the file
    // leaves empty parses as a null scalar, not a collection. That is the shape
    // of a new stage whose timeout is left blank under a bare `stages:` key.
    if (value === null) { if (doc.hasIn(keys)) doc.deleteIn(keys); continue; }
    settingsOpenPath(yaml, doc, keys);
    const node = doc.getIn(keys, true);
    // A list (statuses, args) cannot be assigned into a Scalar: the node's tag
    // never resolves and stringify throws instead of writing the file. A key
    // whose entries are all commented out parses as exactly that null Scalar.
    if (node && yaml.isScalar(node) && typeof value !== 'object') node.value = value;
    else doc.setIn(keys, value);
  }
}

// The paths one save writes. A stage the file does not declare — the built-in
// rework, or one added here — is written whole. Writing only the edited field
// would create `rework:` with a prompt and no timeout, and a stage with no
// timeout runs unbounded: internal/process starts the timer only above zero.
function settingsWritePaths(doc, cfg, baseline) {
  const paths = settingsChangedPaths(cfg, baseline);
  const extra = new Set();
  for (const path of paths) {
    const stage = /^stages\.(.+)\.(?:prompt|timeout)$/.exec(path);
    if (!stage || doc.hasIn(['stages', stage[1]])) continue;
    extra.add(`stages.${stage[1]}.prompt`);
    extra.add(`stages.${stage[1]}.timeout`);
  }
  for (const path of paths) extra.delete(path);
  return paths.concat([...extra]).sort();
}

// setIn cannot descend through a key whose value is null, and `environment:`
// with every entry commented out parses as exactly that. Replace those with an
// empty map, which leaves the key and its comment where they already are.
function settingsOpenPath(yaml, doc, keys) {
  for (let i = 1; i < keys.length; i++) {
    const parent = keys.slice(0, i);
    const node = doc.getIn(parent, true);
    if (node && yaml.isScalar(node) && node.value === null) doc.setIn(parent, doc.createNode({}));
  }
}

// The document path and the value to write for one flattened path. Values are
// typed here rather than at the input, so a port stays a number in YAML.
//
// Paths, names, and durations are trimmed. A pasted trailing space survives
// yaml quoting, so an untrimmed `tickets_dir: "~/org/tickets "` reaches the
// daemon as a directory that does not exist. Prompts and environment values are
// left alone: their whitespace can be deliberate.
function settingsNodeFor(path, cfg) {
  const parts = path.split('.');
  if (path === 'statuses') return { keys: ['statuses'], value: cfg.statuses.slice() };
  if (path === 'auto_pick_up') return { keys: ['auto_pick_up'], value: cfg.auto_pick_up };
  if (path === 'max_concurrent_agents') {
    const raw = cfg.general.max_concurrent_agents.trim();
    // Non-numeric text is written through unchanged so the daemon reports it
    // rather than the client silently dropping the edit.
    return { keys: ['max_concurrent_agents'], value: raw === '' ? null : (/^\d+$/.test(raw) ? Number(raw) : raw) };
  }
  if (parts.length === 1) {
    const v = cfg.general[path].trim();
    return { keys: [path], value: v === '' ? null : v };
  }
  const [group, ...rest] = parts;
  if (group === 'environment') {
    const key = parts.slice(1).join('.');
    return { keys: ['environment', key], value: key in cfg.environment ? cfg.environment[key] : null };
  }
  if (group === 'web') {
    const field = rest[0];
    const v = cfg.web[field].trim();
    if (v === '') return { keys: ['web', field], value: null };
    return { keys: ['web', field], value: field === 'port' && /^\d+$/.test(v) ? Number(v) : v };
  }
  if (group === 'plannotator') {
    const v = cfg.plannotator[rest[0]].trim();
    return { keys: ['plannotator', rest[0]], value: v === '' ? null : v };
  }
  const name = rest.slice(0, rest.length - 1).join('.');
  const field = rest[rest.length - 1];
  if (group === 'agents') {
    if (field === 'args') {
      const args = cfg.agents[name].args.split('\n').filter(l => l !== '');
      return { keys: ['agents', name, 'args'], value: args.length ? args : null };
    }
    return { keys: ['agents', name, 'binary'], value: cfg.agents[name].binary.trim() };
  }
  if (group === 'stages') {
    const v = field === 'timeout' ? cfg.stages[name].timeout.trim() : cfg.stages[name].prompt;
    return { keys: ['stages', name, field], value: v === '' ? null : v };
  }
  const v = cfg.projects[name][field].trim();
  return { keys: ['projects', name, field], value: v === '' ? null : v };
}

function settingsClock(date) {
  return String(date.getHours()).padStart(2, '0') + ':' + String(date.getMinutes()).padStart(2, '0');
}
