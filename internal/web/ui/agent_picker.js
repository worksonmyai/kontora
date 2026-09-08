const AGENT_PICKER_HOSTS = ['init', 'create', 'edit', 'mobile-create'];
const AGENT_PICKER_RECENTS_KEY = 'kontora-recent-agents';
const AGENT_PICKER_RECENTS_MAX = 3;
const AGENT_PICKER_FILTER_MIN = 6;
const AGENT_PICKER_FOCUS_FRAMES = 6;

const AGENT_PICKER_CONTROL = `
<div class="relative" data-agent-picker-control>
  <button x-show="_agentPickerPopoverSupported" type="button"
          :id="agentPickerTriggerID(pickerHost)"
          :aria-labelledby="agentPickerLabelID(pickerHost) + ' ' + agentPickerValueID(pickerHost)"
          aria-haspopup="listbox"
          :aria-expanded="agentPickerIsOpen(pickerHost)"
          :aria-controls="agentPickerListID(pickerHost)"
          @click="toggleAgentPicker(pickerHost)"
          @keydown.enter.prevent="openAgentPicker(pickerHost)"
          @keydown.space.prevent="openAgentPicker(pickerHost)"
          @keydown.arrow-down.prevent="openAgentPicker(pickerHost)"
          class="w-full box-border flex items-center gap-2 bg-surface-800 border font-mono cursor-pointer text-left focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent"
          :class="agentPickerTriggerClass(pickerHost)">
    <span class="w-[5px] h-[5px] rounded-full shrink-0" :style="agentPickerDotStyle(agentPickerValue(pickerHost))"></span>
    <span :id="agentPickerValueID(pickerHost)" class="flex-1 min-w-0 truncate" :class="agentPickerValue(pickerHost) ? 'text-tx' : 'text-tx-3'" x-text="agentPickerLabel(pickerHost)"></span>
    <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" class="text-surface-600 shrink-0" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>
  </button>

  <select x-show="!_agentPickerPopoverSupported"
          :id="agentPickerFallbackID(pickerHost)"
          :aria-labelledby="agentPickerLabelID(pickerHost)"
          :value="agentPickerValue(pickerHost)"
          @change="agentPickerChoose(pickerHost, $event.target.value, false)"
          class="w-full box-border bg-surface-800 border font-mono focus:outline-none focus:border-accent"
          :class="agentPickerFallbackClass(pickerHost)">
    <option value="" x-text="agentPickerBlankLabel(pickerHost)"></option>
    <template x-for="agent in configCache?.agents || []" :key="agent">
      <option :value="agent" x-text="agent"></option>
    </template>
  </select>

  <div x-show="_agentPickerPopoverSupported" :id="agentPickerPanelID(pickerHost)" popover="manual" role="presentation"
       data-agent-picker-panel
       class="agent-picker-popover agent-picker-pop bg-surface-850 border border-surface-700 rounded-[10px] shadow-[0_22px_54px_-16px_rgba(0,0,0,.8)] overflow-hidden text-tx">
    <div x-show="agentPickerHasFilter()" class="flex items-center gap-2 px-[11px] py-[9px] border-b border-surface-700/70">
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" class="text-surface-600 shrink-0" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.6-3.6"/></svg>
      <input :id="agentPickerFilterID(pickerHost)" type="text"
             :value="agentPickerState(pickerHost).query"
             @input="agentPickerSetQuery(pickerHost, $event.target.value)"
             @keydown.arrow-down.prevent="agentPickerMove(pickerHost, 1)"
             @keydown.arrow-up.prevent="agentPickerMove(pickerHost, -1)"
             @keydown.enter.prevent="agentPickerPickActive(pickerHost)"
             @keydown.escape.prevent.stop="closeAgentPicker(true)"
             @keydown.tab="agentPickerTab(pickerHost)"
             role="combobox" aria-autocomplete="list" aria-expanded="true"
             :aria-controls="agentPickerListID(pickerHost)"
             :aria-activedescendant="agentPickerActiveID(pickerHost)"
             placeholder="filter agents" aria-label="Filter agents"
             class="flex-1 min-w-0 bg-transparent border-0 outline-none p-0 font-mono text-[12.5px] text-tx placeholder-surface-600">
      <span class="font-mono text-[10.5px] text-surface-600 shrink-0" x-text="agentPickerMatchCount(pickerHost)"></span>
    </div>

    <div :id="agentPickerListID(pickerHost)" role="listbox" tabindex="-1"
         :aria-label="'Agents for ' + agentPickerHostLabel(pickerHost)"
         :aria-activedescendant="agentPickerActiveID(pickerHost)"
         @keydown.arrow-down.prevent="agentPickerMove(pickerHost, 1)"
         @keydown.arrow-up.prevent="agentPickerMove(pickerHost, -1)"
         @keydown.enter.prevent="agentPickerPickActive(pickerHost)"
         @keydown.escape.prevent.stop="closeAgentPicker(true)"
         @keydown.tab="agentPickerTab(pickerHost)"
         class="agent-picker-scroll overflow-y-auto pt-[5px]">
      <template x-for="group in agentPickerGroups(pickerHost)" :key="group.name">
        <div role="presentation">
          <div role="presentation" class="font-mono text-[9.5px] font-bold tracking-[.13em] uppercase text-surface-600 px-3 pt-[6px] pb-1" x-text="group.name"></div>
          <template x-for="row in group.rows" :key="row.key">
            <div :id="agentPickerRowID(pickerHost, row)" role="option" :aria-selected="row.selected"
                 @click="agentPickerChoose(pickerHost, row.value, true)"
                 @mousemove="agentPickerHover(pickerHost, row.key)"
                 class="h-[34px] mx-[5px] mb-0.5 px-[9px] rounded-[7px] flex items-center gap-[9px] cursor-pointer"
                 :class="agentPickerRowClass(pickerHost, row)">
              <span class="w-[5px] h-[5px] rounded-full shrink-0" :style="agentPickerDotStyle(row.value)"></span>
              <span class="font-mono text-[12.5px] truncate" :class="row.selected ? 'text-tx-hi' : (row.inherit ? 'text-tx-3' : 'text-tx-2')" x-text="row.label"></span>
              <span class="ml-auto flex items-center gap-1.5 shrink-0 min-w-0">
                <span x-show="row.meta" class="font-mono text-[10px] text-tx-faint truncate max-w-[132px]" x-text="row.meta"></span>
                <span x-show="row.isDefault" class="font-mono text-[9px] font-bold tracking-[.08em] uppercase text-accent-bright bg-accent/[.12] border border-accent/[.34] rounded-[4px] px-1 py-px">default</span>
              </span>
            </div>
          </template>
        </div>
      </template>

      <div x-show="agentPickerRows(pickerHost).length === 0" role="presentation" class="px-[13px] pt-[14px] pb-4 flex flex-col gap-[5px]">
        <span class="font-mono text-xs text-tx-3">no agent matches that</span>
        <span class="font-mono text-[11px] text-surface-600">agents live in the config on the daemon host</span>
      </div>
    </div>

    <div x-show="pickerHost === 'init'" class="flex items-center gap-2.5 px-[11px] py-[7px] border-t border-surface-700/70 bg-surface-900/60">
      <span class="font-mono text-[10px] text-surface-600">&#8593;&#8595; move</span>
      <span class="font-mono text-[10px] text-surface-600">&#9166; pick</span>
      <span class="font-mono text-[10px] text-surface-600">esc close</span>
      <span class="ml-auto font-mono text-[10px] text-tx-faint" x-text="agentPickerRunningTotal() + ' running'"></span>
    </div>
  </div>
</div>`;

function recentAgents() {
  try {
    const value = JSON.parse(localStorage.getItem(AGENT_PICKER_RECENTS_KEY));
    return Array.isArray(value) ? value.filter(v => typeof v === 'string').slice(0, AGENT_PICKER_RECENTS_MAX) : [];
  } catch (e) {
    return [];
  }
}

function pickerStates() {
  const states = {};
  AGENT_PICKER_HOSTS.forEach(host => {
    states[host] = { open: false, query: '', active: 0 };
  });
  return states;
}

export function kontoraAgentPicker() {
  return {
    agentPickerControl: AGENT_PICKER_CONTROL,
    recentAgents: recentAgents(),
    _agentPickerStates: pickerStates(),
    _agentPickerOpenHost: null,
    _agentPickerEventsBound: false,
    _agentPickerResizeFrame: null,
    _agentPickerPopoverSupported: typeof HTMLElement !== 'undefined' && typeof HTMLElement.prototype.showPopover === 'function',

    setupAgentPicker() {
      if (this._agentPickerEventsBound) return;
      this._agentPickerEventsBound = true;
      const self = this;
      document.addEventListener('mousedown', function(e) {
        if (!self._agentPickerOpenHost) return;
        if (e.target.closest && e.target.closest('[data-agent-picker-control]')) return;
        self.closeAgentPicker(false);
      });
      document.addEventListener('scroll', function(e) {
        if (!self._agentPickerOpenHost) return;
        if (e.target.closest && e.target.closest('[data-agent-picker-panel]')) return;
        self.closeAgentPicker(false);
      }, true);
      const reposition = function() {
        if (!self._agentPickerOpenHost || self._agentPickerResizeFrame !== null) return;
        self._agentPickerResizeFrame = requestAnimationFrame(function() {
          self._agentPickerResizeFrame = null;
          self.positionAgentPicker();
        });
      };
      window.addEventListener('resize', reposition);
      if (window.visualViewport) {
        window.visualViewport.addEventListener('resize', reposition);
        window.visualViewport.addEventListener('scroll', reposition);
      }
    },

    agentPickerState(host) {
      return this._agentPickerStates[host] || this._agentPickerStates.create;
    },

    agentPickerIsOpen(host) {
      return this._agentPickerOpenHost === host && this.agentPickerState(host).open;
    },

    agentPickerTriggerID(host) {
      return ({ init: 'init-agent', create: 'new-agent', edit: 'edit-agent', 'mobile-create': 'mobile-new-agent' })[host];
    },

    agentPickerFallbackID(host) {
      return this.agentPickerTriggerID(host) + '-fallback';
    },

    agentPickerValueID(host) {
      return this.agentPickerTriggerID(host) + '-value';
    },

    agentPickerLabelID(host) {
      return this.agentPickerTriggerID(host) + '-label';
    },

    agentPickerPanelID(host) {
      return 'agent-picker-' + host;
    },

    agentPickerListID(host) {
      return this.agentPickerPanelID(host) + '-list';
    },

    agentPickerFilterID(host) {
      return this.agentPickerPanelID(host) + '-filter';
    },

    agentPickerHostLabel(host) {
      return ({ init: 'start ticket', create: 'new ticket', edit: 'ticket edit', 'mobile-create': 'new ticket' })[host] || 'ticket';
    },

    agentPickerValue(host) {
      if (host === 'init') return this.initForm.agent || '';
      if (host === 'edit') return this.editForm.agent || '';
      return this.createForm.agent || '';
    },

    agentPickerPipeline(host) {
      if (host === 'init') return this.initForm.pipeline || '';
      if (host === 'edit') return this.editForm.pipeline || '';
      return this.createForm.pipeline || '';
    },

    agentPickerBlankLabel(host) {
      if (!this.agentPickerPipeline(host)) return 'default agent';
      return host === 'create' || host === 'mobile-create'
        ? 'default (each stage picks its own)'
        : 'default (per stage)';
    },

    agentPickerLabel(host) {
      return this.agentPickerValue(host) || this.agentPickerBlankLabel(host);
    },

    agentPickerAgents() {
      const metadata = Object.create(null);
      (this.configCache?.agent_infos || []).forEach(info => {
        if (info && typeof info.name === 'string') metadata[info.name] = info;
      });
      return (this.configCache?.agents || []).map(name => {
        const info = metadata[name] || {};
        return { name, model: info.model || '', effort: info.effort || '' };
      });
    },

    agentPickerMeta(agent) {
      return [agent?.model || '', agent?.effort || ''].filter(Boolean).join(' · ');
    },

    agentPickerInheritMeta(host) {
      const pipeline = this.agentPickerPipeline(host);
      const info = (this.configCache?.pipeline_infos || []).find(p => p.name === pipeline);
      if (info && info.stages?.length && info.default_agent) {
        return info.stages[0] + ' → ' + info.default_agent;
      }
      const fallback = this.configCache?.default_agent || '';
      return fallback ? 'single run → ' + fallback : '';
    },

    agentPickerHint(host) {
      const value = this.agentPickerValue(host);
      if (value) {
        const agent = this.agentPickerAgents().find(a => a.name === value);
        return this.agentPickerMeta(agent) || '\u00a0';
      }
      const first = this.agentPickerInheritMeta(host);
      if (!this.agentPickerPipeline(host)) return first || 'single run';
      return 'each stage picks its own' + (first ? ' · ' + first : '');
    },

    agentPickerHasFilter() {
      return this.agentPickerAgents().length >= AGENT_PICKER_FILTER_MIN;
    },

    agentPickerRows(host) {
      const state = this.agentPickerState(host);
      const query = (state.query || '').trim().toLowerCase();
      const agents = this.agentPickerAgents();
      const matches = agent => !query || agent.name.toLowerCase().includes(query) || agent.model.toLowerCase().includes(query);
      const byName = Object.create(null);
      agents.forEach(agent => { byName[agent.name] = agent; });
      const recent = this.recentAgents.map(name => byName[name]).filter(agent => agent && matches(agent));
      const recentNames = new Set(recent.map(agent => agent.name));
      const remaining = agents.filter(agent => matches(agent) && !recentNames.has(agent.name));
      const selected = this.agentPickerValue(host);
      const rows = [];
      if (!query) {
        rows.push({
          key: 'inherit', group: 'inherit', value: '', label: this.agentPickerBlankLabel(host),
          meta: this.agentPickerInheritMeta(host), inherit: true, selected: selected === '', isDefault: false,
        });
      }
      const append = (group, agent) => rows.push({
        key: 'agent:' + agent.name,
        group,
        value: agent.name,
        label: agent.name,
        meta: this.agentPickerMeta(agent),
        inherit: false,
        selected: selected === agent.name,
        isDefault: agent.name === (this.configCache?.default_agent || ''),
      });
      recent.forEach(agent => append('recent', agent));
      remaining.forEach(agent => append('all agents', agent));
      return rows;
    },

    agentPickerGroups(host) {
      const groups = [];
      this.agentPickerRows(host).forEach(row => {
        const last = groups[groups.length - 1];
        if (last && last.name === row.group) last.rows.push(row);
        else groups.push({ name: row.group, rows: [row] });
      });
      return groups;
    },

    agentPickerMatchCount(host) {
      return String(this.agentPickerRows(host).filter(row => !row.inherit).length);
    },

    agentPickerRunningTotal() {
      return this.agentPickerAgents().reduce((total, agent) => total + this.agentRunningCount(agent.name), 0);
    },

    agentPickerRowID(host, row) {
      const safe = encodeURIComponent(row.key).replace(/%/g, '_');
      return this.agentPickerPanelID(host) + '-row-' + safe;
    },

    agentPickerActiveIndex(host) {
      const rows = this.agentPickerRows(host);
      if (!rows.length) return -1;
      return Math.min(Math.max(this.agentPickerState(host).active, 0), rows.length - 1);
    },

    agentPickerActiveID(host) {
      const rows = this.agentPickerRows(host);
      const index = this.agentPickerActiveIndex(host);
      return index >= 0 ? this.agentPickerRowID(host, rows[index]) : null;
    },

    agentPickerDotStyle(value) {
      if (!value) return 'background:transparent;box-shadow:inset 0 0 0 1px rgba(var(--surface-600),1)';
      if (this.agentRunningCount(value) > 0) {
        return 'background:hsl(var(--st-progress));box-shadow:0 0 0 2px hsl(var(--st-progress) / .2)';
      }
      return 'background:rgba(var(--surface-600),1);box-shadow:none';
    },

    agentPickerRowClass(host, row) {
      if (row.selected) return 'bg-accent/[.14] shadow-[inset_0_0_0_1px_rgba(var(--accent),.5)]';
      const rows = this.agentPickerRows(host);
      const active = this.agentPickerActiveIndex(host);
      if (active >= 0 && rows[active]?.key === row.key) {
        return 'bg-surface-800 shadow-[inset_0_0_0_1px_rgba(var(--edge-hover),.7)]';
      }
      return 'bg-transparent shadow-none';
    },

    agentPickerTriggerClass(host) {
      const open = this.agentPickerIsOpen(host);
      const classes = {
        init: 'h-[39px] rounded-[7px] pl-3 pr-2.5 text-[13px] text-tx',
        create: 'h-[36px] rounded px-2 text-sm text-tx-2',
        edit: 'h-8 rounded px-2 text-[13px] text-tx-2',
        'mobile-create': 'h-[42px] rounded-[10px] px-2.5 text-[13px] text-tx-3 bg-surface-950',
      };
      const border = host === 'init'
        ? (open ? 'border-accent hover:border-accent' : 'border-edge-input hover:border-edge-hover')
        : (open ? 'border-accent/40 hover:border-accent/40' : 'border-surface-700/50 hover:border-edge-hover');
      return classes[host] + ' ' + border;
    },

    agentPickerFallbackClass(host) {
      const classes = {
        init: 'h-[39px] rounded-[7px] px-3 text-[13px] text-tx border-edge-input',
        create: 'h-[36px] rounded px-2 text-sm text-tx-2 border-surface-700/50',
        edit: 'h-8 rounded px-2 text-[13px] text-tx-2 border-surface-700/50',
        'mobile-create': 'h-[42px] rounded-[10px] px-2.5 text-[13px] text-tx-3 bg-surface-950 border-surface-700/50',
      };
      return classes[host];
    },

    agentPickerPanelWidth(host) {
      return host === 'init' ? 328 : 300;
    },

    agentPickerListLimit(host) {
      return host === 'init' ? 296 : 270;
    },

    async toggleAgentPicker(host) {
      if (this.agentPickerIsOpen(host)) {
        this.closeAgentPicker(true);
        return;
      }
      await this.openAgentPicker(host);
    },

    async openAgentPicker(host) {
      if (!this._agentPickerPopoverSupported) return;
      this.closeAgentPicker(false);
      const state = this.agentPickerState(host);
      state.query = '';
      const value = this.agentPickerValue(host);
      const rows = this.agentPickerRows(host);
      const selected = rows.findIndex(row => row.value === value);
      state.active = selected >= 0 ? selected : 0;
      state.open = true;
      this._agentPickerOpenHost = host;
      await this.$nextTick();
      const panel = document.getElementById(this.agentPickerPanelID(host));
      if (!panel || typeof panel.showPopover !== 'function') {
        state.open = false;
        this._agentPickerOpenHost = null;
        this._agentPickerPopoverSupported = false;
        return;
      }
      try {
        panel.showPopover();
      } catch (e) {
        state.open = false;
        this._agentPickerOpenHost = null;
        this._agentPickerPopoverSupported = false;
        return;
      }
      this.positionAgentPicker();
      this.focusAgentPicker(host);
      this.$nextTick(() => this.scrollAgentPickerActive(host));
    },

    closeAgentPicker(restoreFocus) {
      const host = this._agentPickerOpenHost;
      if (!host) return;
      const state = this.agentPickerState(host);
      state.open = false;
      state.query = '';
      state.active = 0;
      const panel = document.getElementById(this.agentPickerPanelID(host));
      if (panel && typeof panel.hidePopover === 'function') {
        try { panel.hidePopover(); } catch (e) {}
      }
      const filter = document.getElementById(this.agentPickerFilterID(host));
      const list = document.getElementById(this.agentPickerListID(host));
      if (filter?.blur) filter.blur();
      if (list?.blur) list.blur();
      this._agentPickerOpenHost = null;
      if (restoreFocus) document.getElementById(this.agentPickerTriggerID(host))?.focus();
    },

    closeAgentPickerHost(host) {
      if (this._agentPickerOpenHost === host) this.closeAgentPicker(false);
    },

    focusAgentPicker(host) {
      const self = this;
      let frames = 0;
      const focus = function() {
        if (!self.agentPickerIsOpen(host)) return;
        const id = self.agentPickerHasFilter() ? self.agentPickerFilterID(host) : self.agentPickerListID(host);
        const target = document.getElementById(id);
        if (target && target.offsetParent) {
          target.focus();
          return;
        }
        frames += 1;
        if (frames <= AGENT_PICKER_FOCUS_FRAMES) requestAnimationFrame(focus);
      };
      focus();
    },

    agentPickerTab(host) {
      setTimeout(() => {
        if (this._agentPickerOpenHost === host) this.closeAgentPicker(false);
      }, 0);
    },

    agentPickerSetQuery(host, query) {
      const state = this.agentPickerState(host);
      state.query = query;
      state.active = 0;
      this.$nextTick(() => {
        const list = document.getElementById(this.agentPickerListID(host));
        if (list) list.scrollTop = 0;
        this.positionAgentPicker();
      });
    },

    agentPickerMove(host, delta) {
      const rows = this.agentPickerRows(host);
      if (!rows.length) return;
      const state = this.agentPickerState(host);
      state.active = ((this.agentPickerActiveIndex(host) + delta) % rows.length + rows.length) % rows.length;
      this.$nextTick(() => this.scrollAgentPickerActive(host));
    },

    agentPickerHover(host, key) {
      const index = this.agentPickerRows(host).findIndex(row => row.key === key);
      if (index >= 0) this.agentPickerState(host).active = index;
    },

    agentPickerPickActive(host) {
      const rows = this.agentPickerRows(host);
      const index = this.agentPickerActiveIndex(host);
      if (index >= 0) this.agentPickerChoose(host, rows[index].value, true);
    },

    agentPickerChoose(host, value, restoreFocus) {
      if (host === 'init') {
        this.initForm.agent = value;
        this._initAgentFollowsProject = false;
      } else if (host === 'edit') {
        this.editForm.agent = value;
        this._editAgentFollowsProject = false;
      } else {
        this.createForm.agent = value;
        this.createTouched.agent = true;
      }
      if (value) this.rememberAgent(value);
      this.closeAgentPicker(restoreFocus !== false);
      if (host === 'edit') this.saveEdit();
    },

    rememberAgent(name) {
      this.recentAgents = [name, ...this.recentAgents.filter(agent => agent !== name)].slice(0, AGENT_PICKER_RECENTS_MAX);
      try { localStorage.setItem(AGENT_PICKER_RECENTS_KEY, JSON.stringify(this.recentAgents)); } catch (e) {}
    },

    scrollAgentPickerActive(host) {
      const list = document.getElementById(this.agentPickerListID(host));
      const id = this.agentPickerActiveID(host);
      const row = id && document.getElementById(id);
      if (!list || !row || !list.getBoundingClientRect || !row.getBoundingClientRect) return;
      const listRect = list.getBoundingClientRect();
      const rowRect = row.getBoundingClientRect();
      if (rowRect.top < listRect.top) list.scrollTop -= listRect.top - rowRect.top;
      else if (rowRect.bottom > listRect.bottom) list.scrollTop += rowRect.bottom - listRect.bottom;
    },

    agentPickerViewport() {
      const viewport = window.visualViewport;
      return viewport
        ? { left: viewport.offsetLeft, top: viewport.offsetTop, width: viewport.width, height: viewport.height }
        : { left: 0, top: 0, width: window.innerWidth, height: window.innerHeight };
    },

    agentPickerPlacement(rect, panelSize, viewport, gap = 6, gutter = 8) {
      const right = viewport.left + viewport.width;
      const bottom = viewport.top + viewport.height;
      const left = Math.min(Math.max(rect.left, viewport.left + gutter), right - gutter - panelSize.width);
      const below = bottom - gutter - rect.bottom - gap;
      const above = rect.top - viewport.top - gutter - gap;
      const placeAbove = below < panelSize.height && above > below;
      const wantedTop = placeAbove ? rect.top - gap - panelSize.height : rect.bottom + gap;
      const top = Math.min(Math.max(wantedTop, viewport.top + gutter), bottom - gutter - panelSize.height);
      return { left, top, placeAbove, available: Math.max(0, placeAbove ? above : below) };
    },

    positionAgentPicker() {
      const host = this._agentPickerOpenHost;
      if (!host) return;
      const trigger = document.getElementById(this.agentPickerTriggerID(host));
      const panel = document.getElementById(this.agentPickerPanelID(host));
      const list = document.getElementById(this.agentPickerListID(host));
      if (!trigger || !panel || !list || !trigger.getBoundingClientRect) return;
      const viewport = this.agentPickerViewport();
      const width = Math.max(180, Math.min(this.agentPickerPanelWidth(host), viewport.width - 16));
      panel.style.width = width + 'px';
      list.style.maxHeight = this.agentPickerListLimit(host) + 'px';
      const rect = trigger.getBoundingClientRect();
      const initial = this.agentPickerPlacement(rect, { width, height: panel.offsetHeight }, viewport);
      const chrome = Math.max(0, panel.offsetHeight - list.offsetHeight);
      list.style.maxHeight = Math.max(34, Math.min(this.agentPickerListLimit(host), initial.available - chrome)) + 'px';
      const placed = this.agentPickerPlacement(rect, { width, height: panel.offsetHeight }, viewport);
      panel.style.left = placed.left + 'px';
      panel.style.top = placed.top + 'px';
    },
  };
}
