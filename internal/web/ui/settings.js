import { clockHM } from './format.js';

// Settings view. Merged into the kontora() component by index.js, so `this` here
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

// What a stage added here starts with. The daemon has no default: a stage with
// no timeout runs until the agent exits.
const SETTINGS_NEW_STAGE_TIMEOUT = '30m';

// The per-agent control: one value for every agent, or a map keyed by agent
// name or agent kind. It appears once per stage field and once per summary_*
// field, so it is held here as markup and rendered through x-html rather than
// pasted into index.html at each site, where the copies drifted. The scope it
// renders in supplies p (the flat path), label, labelClass, inputId and hint.
const SETTINGS_PER_AGENT_CONTROL = `
<div class="flex items-end gap-3 flex-wrap">
  <div>
    <label class="font-mono text-tx-3 mb-1 block" :class="labelClass" :for="inputId" x-text="label"></label>
    <input :id="inputId" type="text"
           x-model="settingsPerAgent(p).any" :disabled="settingsPerAgent(p).mode === 'per_agent'"
           class="w-56 bg-surface-800 border border-surface-700/50 rounded px-3 py-1.5 text-sm text-tx-2 font-mono focus:outline-none focus:border-accent/40 disabled:opacity-40"
           :placeholder="settingsPerAgent(p).mode === 'per_agent' ? 'set per agent below' : hint">
  </div>
  <div class="flex items-center gap-1 font-mono text-[11px]">
    <button type="button" @click="settingsSetPerAgentMode(p, 'any')"
            class="h-8 px-2.5 rounded border transition-colors"
            :class="settingsPerAgent(p).mode === 'any' ? 'border-accent/45 bg-surface-850 text-tx-2' : 'border-surface-700/60 text-surface-600 hover:text-tx-3'">one value</button>
    <button type="button" @click="settingsSetPerAgentMode(p, 'per_agent')"
            class="h-8 px-2.5 rounded border transition-colors"
            :class="settingsPerAgent(p).mode === 'per_agent' ? 'border-accent/45 bg-surface-850 text-tx-2' : 'border-surface-700/60 text-surface-600 hover:text-tx-3'">per agent</button>
  </div>
</div>

<div x-show="settingsPerAgent(p).mode === 'per_agent'" x-cloak class="flex flex-col gap-1.5 pl-3 mt-1.5">
  <template x-for="key in settingsPerAgentKeys(p)" :key="key">
    <div class="flex items-center gap-2">
      <input type="text" :value="key" readonly :aria-label="label + ' agent'"
             class="w-40 flex-none bg-surface-800 border rounded px-3 py-1.5 text-sm text-tx-2 font-mono focus:outline-none"
             :class="settingsAgentKeyValid(key) ? 'border-surface-700/50' : 'border-err'">
      <span class="text-surface-600 font-mono">=</span>
      <input type="text" x-model="settingsPerAgent(p).by[key]" :aria-label="key + ' ' + label"
             class="w-56 bg-surface-800 border border-surface-700/50 rounded px-3 py-1.5 text-sm text-tx-2 font-mono focus:outline-none focus:border-accent/40">
      <button type="button" @click="settingsRemovePerAgent(p, key)" :aria-label="'Remove ' + key"
              class="w-7 h-7 shrink-0 flex items-center justify-center border border-surface-700/50 rounded text-surface-600 hover:text-err hover:border-err/30 transition-colors">
        <svg class="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/></svg>
      </button>
    </div>
  </template>
  <button type="button" x-show="!settingsNewPerAgentOpen[p]" @click="settingsNewPerAgentOpen[p] = true"
          class="w-64 flex items-center justify-center gap-1.5 p-1.5 px-3 border border-dashed border-surface-700 rounded-lg text-surface-600 font-mono text-[12px] hover:text-tx-3 hover:border-edge-hover transition-colors">
    <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 12 12" fill="none"><path d="M6 2v8M2 6h8" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>
    add agent
  </button>
  <div x-show="settingsNewPerAgentOpen[p]" x-cloak class="flex items-center gap-2">
    <input type="text" x-model="settingsNewPerAgentKey[p]" placeholder="agent name or claude / pi"
           @keydown.enter.prevent="settingsAddPerAgent(p)" @keydown.escape="settingsNewPerAgentOpen[p] = false; settingsNewPerAgentKey[p] = ''"
           class="w-40 flex-none bg-surface-800 border rounded px-3 py-1.5 text-sm text-tx-2 font-mono focus:outline-none focus:border-accent/40"
           :class="settingsAgentKeyValid(settingsNewPerAgentKey[p]) ? 'border-surface-700/50' : 'border-err'">
    <button type="button" @click="settingsAddPerAgent(p)"
            :disabled="!(settingsNewPerAgentKey[p] || '').trim() || settingsPerAgentHasKey(p, settingsNewPerAgentKey[p]) || !settingsAgentKeyValid(settingsNewPerAgentKey[p])"
            class="font-mono text-[12px] h-8 px-4 rounded bg-accent border border-accent text-accent-fg hover:bg-accent-bright transition-colors disabled:opacity-50">add</button>
    <button type="button" @click="settingsNewPerAgentOpen[p] = false; settingsNewPerAgentKey[p] = ''"
            class="font-mono text-[12px] h-8 px-4 rounded bg-surface-900 border border-surface-700/60 text-tx-3">cancel</button>
  </div>
  <p class="text-[11px] text-surface-600">An agent name wins over the kind it runs (claude, pi). A key naming neither blocks the save.</p>
</div>`;

const SETTINGS_SECTIONS = [
  { key: 'general', label: 'general', blurb: 'Where kontora keeps things, and how many agents it runs at once.' },
  { key: 'environment', label: 'environment', blurb: 'Injected into every agent process. Per-agent entries override these.' },
  { key: 'agents', label: 'agents', blurb: 'Binaries kontora spawns. Anything with a CLI works.' },
  { key: 'stages', label: 'stages', blurb: 'Prompt templates. A pipeline runs them in order, sharing one git worktree.' },
  { key: 'pipelines', label: 'pipelines', blurb: 'Stages wired to agents, with a routing rule per step.' },
  { key: 'projects', label: 'projects', blurb: 'Defaults a new ticket picks up from the repo it points at.' },
  { key: 'web', label: 'web', blurb: 'This dashboard and the HTTP API the CLI talks to.' },
  { key: 'assistant', label: 'assistant', blurb: 'The chat docked beside the board. An empty agent disables it.' },
  { key: 'plannotator', label: 'plannotator', blurb: 'Human review of the diff, and annotation of the ticket. Review notes feed the rework stage; ticket notes rewrite the ticket.' },
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

// config.Assistant, in file order.
const SETTINGS_ASSISTANT_KEYS = ['agent', 'model', 'effort', 'workdir', 'timeout', 'autonomy', 'prompt'];

// The assistant fields that are a plain text input with a daemon default
// behind them. The other five carry their own markup: agent and autonomy are
// selects, prompt is a textarea, and model and effort have no default to show.
const SETTINGS_ASSISTANT_FIELDS = [
  { key: 'workdir', placeholder: 'tickets_dir', help: 'Every turn of every thread runs here. A thread cannot resume if its cwd moves.' },
  { key: 'timeout', placeholder: '10m', help: 'Bounds one turn.' },
];

// config.builtinStatuses minus archived, which never reaches the board.
const SETTINGS_BUILTIN_STATUSES = ['open', 'todo', 'in_progress', 'paused', 'human_review', 'done', 'cancelled'];

const SETTINGS_TOKENS = [
  { token: '{{ .Ticket.ID }}', help: 'Short ticket id, e.g. kon-f7ha' },
  { token: '{{ .Ticket.Title }}', help: 'Ticket title line' },
  { token: '{{ .Ticket.Description }}', help: 'Full markdown body of the ticket' },
  { token: '{{ file "PLAN.md" }}', help: 'Reads a file from the shared worktree — how one stage hands work to the next' },
  { token: '{{ plannotatorReview }}', help: 'Reviewer feedback from the last plannotator run' },
  { token: '{{ plannotatorAnnotations }}', help: 'Pending ticket annotations, read by the run that rewrites the ticket' },
];

// config.validStatusNameRe (internal/config/config.go).
const SETTINGS_STATUS_RE = /^[a-z][a-z0-9_]*$/;

// The grammar time.ParseDuration accepts: one or more signed decimal numbers,
// each with a unit. A narrower "digits + one unit" pattern would reject 1h30m
// and 1.5h, both of which the daemon takes.
const SETTINGS_DURATION_RE = /^[-+]?(\d+(\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)([-+]?(\d+(\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h))*$/;

export function kontoraSettings() {
  return {
    // What the assistant is told about the Settings view. The section is what
    // "this" means in a question asked from here.
    settingsPageContext() {
      if (this.currentView !== 'settings') return null;
      const lines = ['Settings section: ' + this.settingsSection];
      if (this.settingsDirty()) lines.push('Settings have unsaved edits');
      return lines;
    },

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
    settingsNewStage: '',
    settingsNewStageOpen: false,
    settingsNewEnvOpen: false,
    settingsNewEnvKey: '',
    settingsNewStatus: '',
    settingsNewStatusOpen: false,
    // The per-agent control appears once per stage and twice in general, so its
    // open state and its draft key are keyed by field path rather than being one
    // boolean per control the way the three add/remove pairs above are.
    settingsNewPerAgentOpen: {},
    settingsNewPerAgentKey: {},
    settingsShowToken: false,

    // The parsed Document, the text it came from, and the yaml module. Never
    // rendered and never part of the form model, so no template reads them.
    _settingsDoc: null,
    _settingsRawText: '',
    _settingsYAML: null,

    settingsPerAgentControl: SETTINGS_PER_AGENT_CONTROL,
    settingsSections: SETTINGS_SECTIONS,
    settingsGeneralFields: SETTINGS_GENERAL_FIELDS,
    settingsPlannotatorFields: SETTINGS_PLANNOTATOR_FIELDS,
    settingsAssistantFields: SETTINGS_ASSISTANT_FIELDS,
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
      this.settingsConfig = settingsModel(doc.toJS({ maxAliasCount: -1 }) || {});
      this.settingsBaseline = settingsClone(this.settingsConfig);
      this.settingsState = 'ok';
      this.settingsErrors = [];
      this.settingsSavedAt = '';
      this.settingsOpenStage = null;
      this.settingsOpenAgent = null;
      this.settingsClearPerAgentDrafts('');
    },

    async _settingsLoadYAML() {
      if (!this._settingsYAML) this._settingsYAML = await import('/vendor/yaml@2.8.1/yaml.mjs');
      return this._settingsYAML;
    },

    // ---- dirty state and diff ---------------------------------------------

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
      // config.Validate: an effort on an agent whose CLI takes no flag for it.
      // An agent with no binary at all is already reported above, and so is one
      // the daemon would stop at first.
      for (const name of Object.keys(cfg.agents).sort()) {
        const agent = cfg.agents[name];
        if (agent.binary.trim() && agent.effort.trim() && !settingsAgentKind(agent)) {
          errs.push(`agent "${name}": sets effort "${agent.effort.trim()}", which ${agent.binary.trim()} takes no flag for`);
        }
      }
      errs.push(...this._settingsAssistantErrors());
      // config.validateAgentKeyedFields: a map key naming neither a configured
      // agent nor a kind. Only the keys a save would write are checked; a row
      // left blank writes nothing.
      const keyErr = (field, key) => `${field} "${key}": neither a configured agent nor an agent kind (claude, pi)`;
      for (const name of Object.keys(cfg.stages).sort()) {
        for (const field of ['model', 'effort']) {
          for (const key of Object.keys(settingsPerAgentEntries(cfg.stages[name][field]))) {
            if (!this.settingsAgentKeyValid(key)) errs.push(`stage "${name}": ${keyErr(field, key)}`);
          }
        }
      }
      for (const path of ['summary_model', 'summary_effort']) {
        for (const key of Object.keys(settingsPerAgentEntries(cfg[path]))) {
          if (!this.settingsAgentKeyValid(key)) errs.push(`${path}: ${keyErr(path.replace('summary_', ''), key)}`);
        }
      }
      // config.Validate: a step pairing a stage override with an agent whose CLI
      // takes no flag for it. The pair is only visible where both are named.
      for (const name of Object.keys(cfg.pipelines).sort()) {
        cfg.pipelines[name].forEach((step, i) => {
          const agent = cfg.agents[step.agent];
          if (!agent || !agent.binary.trim() || settingsAgentKind(agent)) return;
          const stage = cfg.stages[step.stage];
          if (!stage) return;
          for (const field of ['model', 'effort']) {
            const v = settingsPerAgentFor(stage[field], step.agent, '');
            if (v) {
              errs.push(`pipeline "${name}" stage ${i}: stage "${step.stage}" sets ${field} "${v}", ` +
                `which agent "${step.agent}" (${agent.binary.trim()}) takes no flag for`);
            }
          }
        });
      }
      return errs;
    },

    // config.validateAssistant. An empty agent disables the pane, and the
    // daemon then checks nothing else in the block, so neither does this.
    // Empty autonomy and timeout are left alone too: applyDefaults fills them.
    // Its effort check is not mirrored: effortFlag() is empty for exactly the
    // kinds the agent check rejects, so the daemon stops at that one first.
    _settingsAssistantErrors() {
      const a = this.settingsConfig.assistant;
      const name = a.agent.trim();
      if (!name) return [];
      // hasOwn, not a truth test: agents is a plain object, so an agent named
      // after an Object.prototype member would otherwise look configured and
      // the read of its binary below would throw.
      if (!Object.hasOwn(this.settingsConfig.agents, name)) {
        return [`assistant.agent "${name}": not found in agents`];
      }
      const agent = this.settingsConfig.agents[name];
      const binary = agent.binary.trim();
      const kind = settingsAgentKind(agent);
      const errs = [];
      if (binary && !kind) {
        errs.push(`assistant.agent "${name}": runs ${binary}, and the assistant only supports claude and pi ` +
          '(they are the CLIs that take a session id, which one chat needs)');
      }
      const autonomy = a.autonomy.trim();
      if (autonomy && !['read', 'ask', 'auto'].includes(autonomy)) {
        errs.push(`assistant.autonomy "${autonomy}": must be one of read, ask, auto`);
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

    // A per-agent map key: a configured agent name, or an agent kind. Mirrors
    // config.validateAgentKeys.
    settingsAgentKeyValid(key) {
      const k = (key || '').trim();
      if (k === '' || k === 'claude' || k === 'pi') return true;
      // hasOwn, not a lookup: an agent named after an Object.prototype member
      // would otherwise read as configured.
      return !!this.settingsConfig && Object.hasOwn(this.settingsConfig.agents, k);
    },

    // Which CLI a configured agent runs, or '' for one that takes no model or
    // effort flag. config.Validate rejects an effort on that agent.
    settingsAgentKindOf(name) {
      return settingsAgentKind(this.settingsConfig?.agents?.[name]);
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
          await this._settingsRebase(content);
          this.settingsSavedAt = clockHM(new Date());
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
    // option in the new-ticket form. Called after a save here, and from the
    // config_reloaded listener for a reload this page did not cause.
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
      try {
        return String(this._settingsDoc);
      } catch (e) {
        // A structure the writer cannot express — an anchor whose alias it
        // would strand — leaves the library's own message, which names nothing
        // the user can act on.
        throw new Error(`this edit cannot be written back into config.yaml (${e.message || e}). Edit the file directly and reload this page.`);
      }
    },

    // Take the text that was just written as the new starting point. The model
    // is rebuilt from it rather than kept, because a save drops what it does not
    // write — a per-agent row whose value was cleared — and the form would
    // otherwise go on showing a row config.yaml no longer has, and report clean.
    async _settingsRebase(content) {
      const yaml = await this._settingsLoadYAML();
      const doc = yaml.parseDocument(content);
      this._settingsRawText = content;
      this._settingsDoc = doc;
      this.settingsConfig = settingsModel(doc.toJS({ maxAliasCount: -1 }) || {});
      this.settingsBaseline = settingsClone(this.settingsConfig);
    },

    discardSettings() {
      this.settingsConfig = settingsClone(this.settingsBaseline);
      this.settingsErrors = [];
      this.settingsDiffOpen = false;
      this.settingsSavedAt = '';
      this.settingsClearPerAgentDrafts('');
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
      this.settingsClearPerAgentDrafts(`stages.${name}`);
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
      this.settingsConfig.stages[name] = {
        // A timeout up front rather than a blank one: a stage with none runs
        // until the agent exits, and a stage created here can now be written
        // from a model or an effort alone, without the prompt field ever being
        // visited. Clearing it is still a choice the form explains.
        prompt: '', timeout: SETTINGS_NEW_STAGE_TIMEOUT, builtin: false,
        model: settingsPerAgentModel(undefined),
        effort: settingsPerAgentModel(undefined),
      };
      this.settingsOpenStage = name;
      this.settingsNewStage = '';
      this.settingsNewStageOpen = false;
      this.settingsSection = 'stages';
    },

    settingsAddEnv() {
      const key = (this.settingsNewEnvKey || '').trim();
      // hasOwn, not `in`: `'constructor' in {}` is true, and the row could then
      // never be added, with nothing on screen explaining why.
      if (!key || Object.hasOwn(this.settingsConfig.environment, key)) return;
      this.settingsConfig.environment[key] = '';
      this.settingsNewEnvKey = '';
      this.settingsNewEnvOpen = false;
    },

    settingsRemoveEnv(key) {
      delete this.settingsConfig.environment[key];
    },

    // ---- the per-agent control ---------------------------------------------

    // The PerAgent sub-model behind a flat path, so one markup block serves the
    // stage fields and the two summary_* ones.
    // Either a root key (summary_model, summary_effort) or stages.<name>.<field>,
    // whose name can itself carry dots.
    settingsPerAgent(path) {
      const parts = path.split('.');
      if (parts.length === 1) return this.settingsConfig[path];
      return this.settingsConfig.stages[parts.slice(1, -1).join('.')][parts[parts.length - 1]];
    },

    settingsPerAgentKeys(path) {
      return Object.keys(this.settingsPerAgent(path).by).sort();
    },

    // One line for a collapsed card, so a value is visible without expanding.
    // Empty means the field is unset and the agent's own applies.
    settingsPerAgentLabel(path) {
      const field = this.settingsPerAgent(path);
      const by = settingsPerAgentEntries(field);
      const keys = Object.keys(by);
      if (!keys.length) return field.any.trim();
      return keys.map(k => `${k}: ${by[k]}`).join(', ');
    },

    settingsSetPerAgentMode(path, mode) {
      this.settingsPerAgent(path).mode = mode;
    },

    // A draft key already on the field, whatever it is named, cannot be added
    // twice. hasOwn rather than `in`, which is true for every Object.prototype
    // member and would leave the add button disabled with no explanation.
    settingsPerAgentHasKey(path, key) {
      return Object.hasOwn(this.settingsPerAgent(path).by, (key || '').trim());
    },

    settingsAddPerAgent(path) {
      const key = (this.settingsNewPerAgentKey[path] || '').trim();
      const field = this.settingsPerAgent(path);
      if (!key || Object.hasOwn(field.by, key)) return;
      field.by[key] = '';
      this.settingsNewPerAgentKey[path] = '';
      this.settingsNewPerAgentOpen[path] = false;
    },

    settingsRemovePerAgent(path, key) {
      delete this.settingsPerAgent(path).by[key];
    },

    // Drop the open state and the half-typed key of every per-agent control
    // under a path prefix. The model behind them is gone once the form is
    // reloaded, reverted or discarded, and a draft row left over would reappear
    // on a field that no longer has it.
    settingsClearPerAgentDrafts(prefix) {
      for (const key of Object.keys(this.settingsNewPerAgentOpen)) {
        if (!prefix || key === prefix || key.startsWith(prefix + '.')) delete this.settingsNewPerAgentOpen[key];
      }
      for (const key of Object.keys(this.settingsNewPerAgentKey)) {
        if (!prefix || key === prefix || key.startsWith(prefix + '.')) delete this.settingsNewPerAgentKey[key];
      }
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
      // currentView changes nowhere else, and applyRoute routes even a ticket
      // hash through here, so this one call covers every path out of Stats.
      if (this.currentView === 'stats' && view !== 'stats') this.closeStats();
      if (this.currentView === 'archive' && view !== 'archive') this.closeArchive();
      if (view === 'stats') { this.currentView = 'stats'; this.openStats(); this.writeHash(); return; }
      if (view === 'archive') { this.currentView = 'archive'; this.openArchive(); this.writeHash(); return; }
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
    // A map keyed by user-supplied names carries no prototype, and copying it
    // onto a plain object would drop a key named after one of its members.
    const out = Object.getPrototypeOf(value) === null ? Object.create(null) : {};
    for (const [k, v] of Object.entries(value)) out[k] = settingsClone(v);
    return out;
  }
  return value;
}

// Which CLI an agent runs, or '' for one kontora knows no flags of. Mirrors
// config.Agent.Kind: a wrapper (nono, op) runs the binary after its "--".
function settingsAgentKind(agent) {
  const base = p => String(p).trim().split('/').pop();
  let binary = base(agent?.binary || '');
  if (binary === 'nono' || binary === 'op') {
    const args = (agent?.args || '').split('\n').filter(a => a !== '');
    const sep = args.indexOf('--');
    if (sep >= 0 && sep + 1 < args.length) binary = base(args[sep + 1]);
  }
  return binary === 'claude' || binary === 'pi' ? binary : '';
}

// A config.PerAgent field — a stage model or effort, or one of the summary_*
// pair — as the form holds it: either one value for every agent, or a map from
// an agent name or an agent kind to a value. Both halves are kept, so flipping
// the toggle and back does not lose what was typed.
function settingsPerAgentModel(raw) {
  const str = v => (v === undefined || v === null ? '' : String(v));
  // A sequence is neither shape PerAgent reads, so it is left as unset rather
  // than rendered as rows keyed 0 and 1, which a save would then write back as
  // a mapping the daemon rejects.
  if (raw !== null && typeof raw === 'object' && !Array.isArray(raw)) {
    // Object.create(null): an agent named after an Object.prototype member is a
    // key like any other here, and `by.__proto__ = 'haiku'` on a plain object
    // reaches the prototype setter and drops the entry.
    const by = Object.create(null);
    for (const [k, v] of Object.entries(raw)) by[k] = str(v);
    return { mode: 'per_agent', any: '', by };
  }
  return { mode: 'any', any: Array.isArray(raw) ? '' : str(raw), by: Object.create(null) };
}

// The entries a save would write: trimmed, empty ones dropped. Per-agent mode
// with no filled row is not a map yet — the bare value still applies — so
// flipping the toggle before filling a row is not an edit and deletes nothing.
function settingsPerAgentEntries(v) {
  const out = Object.create(null);
  if (!v || v.mode !== 'per_agent') return out;
  for (const k of Object.keys(v.by).sort()) {
    const value = String(v.by[k]).trim();
    if (value !== '') out[k] = value;
  }
  return out;
}

// The value one agent resolves to. Mirrors config.PerAgent.For: an exact agent
// name wins over the agent's kind, and a bare value applies to every agent.
function settingsPerAgentFor(v, agentName, kind) {
  const by = settingsPerAgentEntries(v);
  if (Object.hasOwn(by, agentName)) return by[agentName];
  if (kind && Object.hasOwn(by, kind)) return by[kind];
  return Object.keys(by).length ? '' : (v ? v.any.trim() : '');
}

// The string the flat path holds. Change detection compares these strings, so
// the map form carries a header line: a one-key map and a bare value that reads
// like one would otherwise compare equal, and the edit would be reported as no
// change. The single-value input is one line by construction, so a two-line
// string can only come from the map form.
function settingsPerAgentFlat(v) {
  if (!v) return '';
  const by = settingsPerAgentEntries(v);
  const keys = Object.keys(by);
  if (!keys.length) return v.any.trim();
  return ['per agent'].concat(keys.map(k => `  ${k}: ${by[k]}`)).join('\n');
}

// What settingsNodeFor writes: a bare string, a map, or null to delete the key.
function settingsPerAgentValue(v) {
  if (!v) return null;
  const by = settingsPerAgentEntries(v);
  if (Object.keys(by).length) return by;
  return v.any.trim() === '' ? null : v.any.trim();
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
    assistant: {
      agent: str(raw.assistant?.agent),
      model: str(raw.assistant?.model),
      effort: str(raw.assistant?.effort),
      workdir: str(raw.assistant?.workdir),
      timeout: str(raw.assistant?.timeout),
      autonomy: str(raw.assistant?.autonomy),
      prompt: str(raw.assistant?.prompt),
    },
    auto_pick_up: raw.auto_pick_up === undefined || raw.auto_pick_up === null ? true : !!raw.auto_pick_up,
    summary_model: settingsPerAgentModel(raw.summary_model),
    summary_effort: settingsPerAgentModel(raw.summary_effort),
  };
  for (const f of SETTINGS_GENERAL_FIELDS) model.general[f.key] = str(raw[f.key]);
  for (const [k, v] of Object.entries(raw.environment || {})) model.environment[k] = str(v);
  for (const [name, agent] of Object.entries(raw.agents || {})) {
    model.agents[name] = {
      binary: str(agent?.binary),
      effort: str(agent?.effort),
      args: (agent?.args || []).map(String).join('\n'),
      failure_patterns: agent?.failure_patterns === undefined || agent?.failure_patterns === null
        ? null
        : agent.failure_patterns.map(String),
    };
  }
  for (const [name, stage] of Object.entries(raw.stages || {})) {
    model.stages[name] = {
      prompt: str(stage?.prompt),
      timeout: str(stage?.timeout),
      model: settingsPerAgentModel(stage?.model),
      effort: settingsPerAgentModel(stage?.effort),
      builtin: false,
    };
  }
  // applyDefaults injects rework when the file declares no stages.rework, and
  // the daemon runs it. Show it, marked, rather than hiding a live stage.
  if (!model.stages.rework) {
    model.stages.rework = {
      prompt: SETTINGS_REWORK_PROMPT,
      timeout: SETTINGS_REWORK_TIMEOUT,
      model: settingsPerAgentModel(undefined),
      effort: settingsPerAgentModel(undefined),
      builtin: true,
    };
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
  out.summary_model = settingsPerAgentFlat(cfg.summary_model);
  out.summary_effort = settingsPerAgentFlat(cfg.summary_effort);
  for (const [k, v] of Object.entries(cfg.environment)) out[`environment.${k}`] = v;
  for (const [name, agent] of Object.entries(cfg.agents)) {
    out[`agents.${name}.binary`] = agent.binary;
    out[`agents.${name}.effort`] = agent.effort;
    out[`agents.${name}.args`] = agent.args;
  }
  for (const [name, stage] of Object.entries(cfg.stages)) {
    out[`stages.${name}.prompt`] = stage.prompt;
    out[`stages.${name}.timeout`] = stage.timeout;
    out[`stages.${name}.model`] = settingsPerAgentFlat(stage.model);
    out[`stages.${name}.effort`] = settingsPerAgentFlat(stage.effort);
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
  for (const f of SETTINGS_ASSISTANT_KEYS) out[`assistant.${f}`] = cfg.assistant[f];
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
    if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
      settingsApplyMap(yaml, doc, keys, value);
      continue;
    }
    const node = doc.getIn(keys, true);
    // A list (statuses, args) cannot be assigned into a Scalar: the node's tag
    // never resolves and stringify throws instead of writing the file. A key
    // whose entries are all commented out parses as exactly that null Scalar.
    if (node && yaml.isScalar(node) && typeof value !== 'object') node.value = value;
    else doc.setIn(keys, value);
  }
}

// Write a map (a per-agent model or effort) entry by entry rather than through
// setIn, which would replace the whole mapping node: that drops the comments
// inside it, reorders its keys alphabetically, and — when the map carries an
// anchor another key aliases — leaves the alias dangling, so stringify throws
// and no save can succeed until the file is fixed by hand.
function settingsApplyMap(yaml, doc, keys, value) {
  const node = doc.getIn(keys, true);
  if (!node || !yaml.isMap(node)) { doc.setIn(keys, value); return; }
  for (const item of [...node.items]) {
    const key = yaml.isScalar(item.key) ? String(item.key.value) : String(item.key);
    if (!Object.hasOwn(value, key)) doc.deleteIn([...keys, key]);
  }
  for (const [key, v] of Object.entries(value)) {
    const cur = doc.getIn([...keys, key], true);
    if (cur && yaml.isScalar(cur)) cur.value = v;
    else doc.setIn([...keys, key], v);
  }
}

// The paths one save writes. A stage the file does not declare — the built-in
// rework, or one added here — is written whole. Writing only the edited field
// would create `rework:` with a prompt and no timeout, and a stage with no
// timeout runs unbounded: internal/process starts the timer only above zero.
// Editing only its model or effort is the same case, which is why those two
// trigger it as well.
function settingsWritePaths(doc, cfg, baseline) {
  const paths = settingsChangedPaths(cfg, baseline);
  const extra = new Set();
  for (const path of paths) {
    const stage = /^stages\.(.+)\.(?:prompt|timeout|model|effort)$/.exec(path);
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
  // Before the general fallthrough below, which reads cfg.general[path]: these
  // two live on the model's top level, not in general.
  if (path === 'summary_model' || path === 'summary_effort') {
    return { keys: [path], value: settingsPerAgentValue(cfg[path]) };
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
  if (group === 'assistant') {
    const field = rest[0];
    // The prompt is the one field whose whitespace can be deliberate, the same
    // rule stage prompts follow.
    const v = field === 'prompt' ? cfg.assistant.prompt : cfg.assistant[field].trim();
    return { keys: ['assistant', field], value: v === '' ? null : v };
  }
  const name = rest.slice(0, rest.length - 1).join('.');
  const field = rest[rest.length - 1];
  if (group === 'agents') {
    if (field === 'args') {
      const args = cfg.agents[name].args.split('\n').filter(l => l !== '');
      return { keys: ['agents', name, 'args'], value: args.length ? args : null };
    }
    if (field === 'effort') {
      const v = cfg.agents[name].effort.trim();
      return { keys: ['agents', name, 'effort'], value: v === '' ? null : v };
    }
    return { keys: ['agents', name, 'binary'], value: cfg.agents[name].binary.trim() };
  }
  if (group === 'stages') {
    if (field === 'model' || field === 'effort') {
      return { keys: ['stages', name, field], value: settingsPerAgentValue(cfg.stages[name][field]) };
    }
    const v = field === 'timeout' ? cfg.stages[name].timeout.trim() : cfg.stages[name].prompt;
    return { keys: ['stages', name, field], value: v === '' ? null : v };
  }
  const v = cfg.projects[name][field].trim();
  return { keys: ['projects', name, field], value: v === '' ? null : v };
}
