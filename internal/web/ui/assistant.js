// The assistant pane: a chat docked beside the board that answers questions
// about it and drives Kontora through the same verbs the CLI uses.
//
// Every key is prefixed. activity.js already owns toolKey, toolExpanded,
// toolFailed, toggleTool and eventTime for the ticket transcript, and merge()
// throws on a repeated key, so the pane carries its own copies rather than
// sharing state with the ticket detail view.

const ASSISTANT_POLL_MS = 1500;
const ASSISTANT_WIDTH_MIN = 340;
const ASSISTANT_WIDTH_MAX = 640;
const ASSISTANT_WIDTH_DEFAULT = 420;

// The page-context cap the API enforces (assistantContextMax). Trimmed here so
// a message is never rejected for describing the page.
const ASSISTANT_CONTEXT_MAX = 2 * 1024;

// The slash commands the composer offers. They write a phrasing the agent can
// act on rather than being executed by the browser: the pane types for the
// user, it does not drive the daemon itself.
const ASSISTANT_SLASH = [
  { name: 'status', hint: 'what is running right now', text: 'What is running right now? Summarise the board.' },
  { name: 'stuck', hint: 'tickets that need a person', text: 'Which tickets are paused, failed, or waiting on a human? Say what each one needs.' },
  { name: 'run', hint: 'start a ticket', text: 'Run ticket ' },
  { name: 'move', hint: 'change a ticket status', text: 'Move ticket ' },
  { name: 'note', hint: 'append a note', text: 'Add a note to ticket ' },
  { name: 'logs', hint: 'read a run transcript', text: 'Show me the last run of ticket ' },
];

function assistantStored(key, fallback) {
  try {
    const v = localStorage.getItem(key);
    return v === null ? fallback : v;
  } catch (e) {
    return fallback;
  }
}

function assistantStore(key, value) {
  try { localStorage.setItem(key, value); } catch (e) { /* private mode */ }
}

// A recorded timestamp as milliseconds, or null when the field is missing or
// unreadable. A pi session this build cannot read the clock off leaves it null
// for the whole tape.
function assistantMs(value) {
  if (!value) return null;
  const t = Date.parse(value);
  return Number.isNaN(t) ? null : t;
}

function assistantClampWidth(px) {
  const n = Number(px);
  if (!Number.isFinite(n)) return ASSISTANT_WIDTH_DEFAULT;
  return Math.max(ASSISTANT_WIDTH_MIN, Math.min(ASSISTANT_WIDTH_MAX, Math.round(n)));
}

export function kontoraAssistant() {
  return {
    assistantOpen: assistantStored('kontora-assistant-open', '0') === '1',
    assistantRail: assistantStored('kontora-assistant-rail', '0') === '1',
    assistantWidth: assistantClampWidth(assistantStored('kontora-assistant-width', ASSISTANT_WIDTH_DEFAULT)),
    // 'thread' is the live chat; 'history' is the list of past ones.
    assistantView: 'thread',
    assistantAutonomy: assistantStored('kontora-assistant-autonomy', 'ask'),
    // The GET /api/assistant payload. enabled false is what drives the
    // configure hint in place of the composer.
    assistantAgent: null,
    assistantDraft: '',
    assistantStreaming: false,
    // Seconds the running turn has been going. 0 while the pane cannot tell,
    // which is a turn it did not start itself.
    assistantElapsed: 0,
    assistantThread: null,
    assistantThreads: [],
    assistantMessages: [],
    assistantEvents: [],
    assistantGate: null,
    assistantError: null,
    assistantContext: [],
    // The open slash or mention menu: { kind, rows, index, from }.
    assistantMenu: null,
    assistantAutonomyOpen: false,
    // Keyed by tool-call id, so a row stays open as the tape grows. Null
    // prototype: the keys come from the agent, and one called "constructor"
    // must not read a function where a boolean is expected.
    assistantExpanded: Object.create(null),

    _assistantPoll: null,
    _assistantCursor: 0,
    _assistantETag: '',
    _assistantLoadSeq: 0,
    _assistantResize: null,
    _assistantTurnStart: null,
    _assistantTick: null,
    _assistantStick: true,

    // A fetch that throws on a status the pane cannot use, so every caller can
    // catch one thing. 304 passes through: it is the poll's "nothing new",
    // which the ETag revalidation depends on.
    async _assistantFetch(path, options) {
      const res = await fetch(path, options);
      if (res.status === 401) {
        this.needsAuth = true;
        throw new Error('unauthorized');
      }
      if (res.status === 304 || res.ok) return res;
      const data = await res.json().catch(() => ({}));
      throw new Error(data.error || ('the daemon answered ' + res.status));
    },

    // --- the working state -----------------------------------------------

    // Every write of assistantStreaming goes through here, so the elapsed clock
    // and the poll cannot disagree about whether a turn is live. startedAt is
    // passed only by the pane that sent the message: a turn adopted from the
    // daemon has no start the pane can honestly count from, so it shows the
    // label with no number.
    _assistantSetStreaming(running, startedAt) {
      if (running && !this.assistantStreaming) {
        this._assistantTurnStart = startedAt || null;
        this.assistantElapsed = 0;
        this._startAssistantTick();
      }
      if (!running) {
        this._assistantTurnStart = null;
        this.assistantElapsed = 0;
        this._stopAssistantTick();
      }
      this.assistantStreaming = running;
    },

    _startAssistantTick() {
      this._stopAssistantTick();
      if (!this._assistantTurnStart) return;
      const self = this;
      this._assistantTick = setInterval(function () {
        if (!self._assistantTurnStart) { self._stopAssistantTick(); return; }
        self.assistantElapsed = Math.floor((Date.now() - self._assistantTurnStart) / 1000);
      }, 1000);
    },

    _stopAssistantTick() {
      if (this._assistantTick !== null) {
        clearInterval(this._assistantTick);
        this._assistantTick = null;
      }
    },

    // What the working row reads. The agent can take a while before it writes
    // anything, so the row carries the count rather than a bare spinner.
    assistantWorkingLabel() {
      if (!this._assistantTurnStart) return 'working…';
      const s = Math.max(0, this.assistantElapsed);
      if (s < 60) return 'working… ' + s + 's';
      const m = Math.floor(s / 60);
      return 'working… ' + m + 'm ' + String(s % 60).padStart(2, '0') + 's';
    },

    // --- scrolling -------------------------------------------------------

    _assistantScrollEl() {
      const sheet = this.sheet && this.sheet.type === 'assistant';
      return document.getElementById(sheet ? 'assistant-sheet-thread' : 'assistant-thread');
    },

    // Read before the tape is spliced: a reader who has scrolled up to a tool
    // row keeps their place, and everyone else is carried along.
    _assistantNoteScroll() {
      const el = this._assistantScrollEl();
      this._assistantStick = !el || (el.scrollHeight - el.scrollTop - el.clientHeight) < 60;
      return this._assistantStick;
    },

    _assistantScrollToEnd(force) {
      if (!force && !this._assistantStick) return;
      const self = this;
      this.$nextTick(function () {
        const el = self._assistantScrollEl();
        if (el) el.scrollTop = el.scrollHeight;
      });
    },

    // --- lifecycle -------------------------------------------------------

    // Fetch the assistant config and, when the pane was left open, the thread
    // it was left on. Called from the component's init.
    async initAssistant() {
      await this._assistantRefreshConfig();
      if (!this.assistantEnabled()) return;
      const last = assistantStored('kontora-assistant-thread', '');
      if (last) await this.assistantSelectThread(last, { quiet: true });
      if (this.assistantOpen) this.assistantLoadThreads();
    },

    // Re-read GET /api/assistant. Called from init and again on every
    // config_reloaded, so a change to the assistant block reaches the pane
    // without a page reload. It touches nothing about the open thread — not
    // the cursor, not the poll — because a reload does not end a chat.
    async _assistantRefreshConfig() {
      try {
        const res = await this._assistantFetch('/api/assistant');
        this.assistantAgent = await res.json();
      } catch (e) {
        // Only the first fetch turns into the hint. A later one failing is a
        // blip in a pane that is already working, and replacing the composer
        // with the configure hint over it loses the draft in it.
        if (!this.assistantAgent) {
          this.assistantAgent = { enabled: false, hint: 'The daemon did not answer.' };
        }
      }
      this._assistantSeedAutonomy();
    },

    // The picker is sticky per browser, but a change to assistant.autonomy in
    // the file has to win, or the settings field would do nothing for anyone
    // who has already touched the switch. The seed is the config value this
    // browser last took: while it matches the file, the user's pick stands.
    _assistantSeedAutonomy() {
      const mode = this.assistantAgent && this.assistantAgent.autonomy;
      if (!mode) return;
      if (assistantStored('kontora-assistant-autonomy-seed', null) === mode) return;
      assistantStore('kontora-assistant-autonomy-seed', mode);
      assistantStore('kontora-assistant-autonomy', mode);
      // An open chat owns the switch: the daemon holds a mode for that chat,
      // and moving the switch under it would send the next message at an
      // autonomy nobody chose. The seed still moves, so the next new chat
      // starts on the file's value.
      if (this.assistantThread) return;
      this.assistantAutonomy = mode;
    },

    assistantEnabled() {
      return !!(this.assistantAgent && this.assistantAgent.enabled);
    },

    toggleAssistant() {
      if (this.assistantOpen && !this.assistantRail) {
        this.closeAssistant();
        return;
      }
      this.assistantOpen = true;
      this.assistantRail = false;
      assistantStore('kontora-assistant-open', '1');
      assistantStore('kontora-assistant-rail', '0');
      this.assistantRecomputeContext();
      if (this.assistantEnabled()) {
        this.assistantLoadThreads();
        // One poll now rather than re-arming: opening onto a chat whose turn is
        // already running has nothing armed yet, and the payload is what says
        // it is running.
        this._loadAssistantActivity();
      }
    },

    closeAssistant() {
      this.assistantOpen = false;
      this.assistantMenu = null;
      this.assistantAutonomyOpen = false;
      assistantStore('kontora-assistant-open', '0');
      this._stopAssistantPoll();
      this._stopAssistantTick();
    },

    // The phone has no room for a docked pane, so the same chat opens in the
    // bottom sheet the rest of the mobile layer uses.
    openAssistantSheet() {
      this.assistantOpen = true;
      this.assistantView = 'thread';
      this.sheet = { type: 'assistant' };
      this.assistantRecomputeContext();
      if (this.assistantEnabled()) {
        this.assistantLoadThreads();
        this._loadAssistantActivity();
      }
    },

    // The rail keeps the pane mounted at icon width, so a running turn is still
    // visible as a pulsing dot without the board being squeezed.
    toggleAssistantRail() {
      this.assistantRail = !this.assistantRail;
      assistantStore('kontora-assistant-rail', this.assistantRail ? '1' : '0');
      if (!this.assistantRail) this.assistantRecomputeContext();
    },

    // --- width -----------------------------------------------------------

    setAssistantWidth(px) {
      this.assistantWidth = assistantClampWidth(px);
      assistantStore('kontora-assistant-width', String(this.assistantWidth));
      return this.assistantWidth;
    },

    // Drag the pane's left edge. The width is written on every move and
    // persisted once, so a drag does not hammer localStorage.
    startAssistantResize(event) {
      const startX = event.clientX;
      const startWidth = this.assistantWidth;
      const self = this;
      const move = (e) => {
        // The pane is on the right, so dragging left widens it.
        self.assistantWidth = assistantClampWidth(startWidth + (startX - e.clientX));
      };
      const up = () => {
        window.removeEventListener('mousemove', move);
        window.removeEventListener('mouseup', up);
        self._assistantResize = null;
        assistantStore('kontora-assistant-width', String(self.assistantWidth));
      };
      this._assistantResize = { move, up };
      window.addEventListener('mousemove', move);
      window.addEventListener('mouseup', up);
    },

    // --- threads ---------------------------------------------------------

    async assistantLoadThreads() {
      if (!this.assistantEnabled()) return;
      try {
        const res = await this._assistantFetch('/api/assistant/threads');
        const data = await res.json();
        this.assistantThreads = (data && data.threads) || [];
      } catch (e) {
        this.assistantThreads = [];
      }
    },

    async assistantNewThread() {
      if (!this.assistantEnabled()) return null;
      this.assistantError = null;
      try {
        const res = await this._assistantFetch('/api/assistant/threads', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ autonomy: this.assistantAutonomy }),
        });
        const thread = await res.json();
        this._assistantAdopt(thread, []);
        this.assistantEvents = [];
        this.assistantView = 'thread';
        this.assistantLoadThreads();
        return thread;
      } catch (e) {
        this.assistantError = String(e.message || e);
        return null;
      }
    },

    async assistantSelectThread(id, opts) {
      const quiet = !!(opts && opts.quiet);
      try {
        const res = await this._assistantFetch('/api/assistant/threads/' + encodeURIComponent(id));
        const thread = await res.json();
        this._assistantAdopt(thread, thread.messages || []);
        this.assistantEvents = [];
        this._assistantCursor = 0;
        this._assistantETag = '';
        this.assistantView = 'thread';
        await this._loadAssistantActivity();
        this._assistantScrollToEnd(true);
        return true;
      } catch (e) {
        // A thread the browser remembered but the daemon has dropped is not an
        // error worth showing: the pane simply opens empty.
        if (!quiet) this.assistantError = String(e.message || e);
        this.assistantThread = null;
        assistantStore('kontora-assistant-thread', '');
        return false;
      }
    },

    async assistantDeleteThread(id) {
      try {
        await this._assistantFetch('/api/assistant/threads/' + encodeURIComponent(id), { method: 'DELETE' });
      } catch (e) {
        this.assistantError = String(e.message || e);
        return;
      }
      if (this.assistantThread && this.assistantThread.id === id) {
        this.assistantThread = null;
        this.assistantMessages = [];
        this.assistantEvents = [];
        assistantStore('kontora-assistant-thread', '');
      }
      this.assistantLoadThreads();
    },

    _assistantAdopt(thread, messages) {
      this.assistantThread = thread;
      this.assistantMessages = messages || [];
      this._assistantSetStreaming(!!(thread && thread.running));
      if (thread && thread.autonomy) this.assistantAutonomy = thread.autonomy;
      if (thread && thread.id) assistantStore('kontora-assistant-thread', thread.id);
    },

    // --- page context ----------------------------------------------------

    // The mixins that describe what the user is looking at, in the order their
    // lines are sent. Each returns an array of lines or null; a name with no
    // method behind it is skipped, and ui_mixins.test.mjs fails the build when
    // one goes missing, because a renamed hook would otherwise go quiet.
    _assistantContextHooks: ['detailPageContext', 'boardPageContext', 'statsPageContext', 'settingsPageContext'],

    // Collected fresh for every message, so the agent is told the page as of
    // that message rather than the one the chat opened on.
    _assistantPageContext() {
      const lines = [];
      for (const name of this._assistantContextHooks) {
        if (typeof this[name] !== 'function') continue;
        // A hook that throws costs its own lines, not the message: the user
        // pressed send, and a broken description of one view must not stop it.
        let got = null;
        try { got = this[name](); } catch (e) { continue; }
        if (!got) continue;
        for (const line of got) {
          if (line) lines.push(String(line));
        }
      }
      // The API rejects anything longer, and the tail is the least specific.
      // Measured in bytes, because the handler caps len(req.Context): counting
      // UTF-16 units would let non-ASCII text through and cost the message a 400.
      const encoder = new TextEncoder();
      let out = lines.join('\n');
      while (encoder.encode(out).length > ASSISTANT_CONTEXT_MAX && lines.length) {
        lines.pop();
        out = lines.join('\n');
      }
      return out;
    },

    // --- sending ---------------------------------------------------------

    async sendAssistantMessage() {
      const text = (this.assistantDraft || '').trim();
      if (!text || !this.assistantEnabled() || this.assistantStreaming) return;
      let thread = this.assistantThread;
      if (!thread) {
        thread = await this.assistantNewThread();
        if (!thread) return;
      }
      this.assistantDraft = '';
      this.assistantMenu = null;
      this.assistantError = null;
      // Shown before the daemon answers, so the message does not vanish while
      // the turn is starting. The next poll replaces it with the recorded one.
      this.assistantMessages = this.assistantMessages.concat([{ n: this.assistantMessages.length + 1, text: text, at: new Date().toISOString() }]);
      this._assistantSetStreaming(true, Date.now());
      this._assistantScrollToEnd(true);
      try {
        await this._assistantFetch('/api/assistant/threads/' + encodeURIComponent(thread.id) + '/messages', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ text: text, autonomy: this.assistantAutonomy, context: this._assistantPageContext() }),
        });
      } catch (e) {
        this._assistantSetStreaming(false);
        this.assistantError = String(e.message || e);
        return;
      }
      this._armAssistantPoll(0);
    },

    async stopAssistantTurn() {
      if (!this.assistantThread) return;
      try {
        await this._assistantFetch('/api/assistant/threads/' + encodeURIComponent(this.assistantThread.id) + '/stop', { method: 'POST' });
      } catch (e) {
        this.assistantError = String(e.message || e);
      }
    },

    // --- autonomy --------------------------------------------------------

    assistantAutonomies() {
      return [
        { key: 'read', label: 'read only', hint: 'answers questions, changes nothing' },
        { key: 'ask', label: 'ask first', hint: 'holds every change for your approval' },
        { key: 'auto', label: 'auto', hint: 'makes changes without asking, except deletes' },
      ];
    },

    setAssistantAutonomy(mode) {
      this.assistantAutonomy = mode;
      this.assistantAutonomyOpen = false;
      assistantStore('kontora-assistant-autonomy', mode);
    },

    assistantAutonomyLabel() {
      const row = this.assistantAutonomies().find((a) => a.key === this.assistantAutonomy);
      return row ? row.label : this.assistantAutonomy;
    },

    // --- the parked write ------------------------------------------------

    async resolveAssistantGate(approve) {
      const gate = this.assistantGate;
      if (!gate) return;
      // Cleared first: the card must not stay on screen while the request is in
      // flight, or a second click sends a second answer for the same call.
      this.assistantGate = null;
      try {
        await this._assistantFetch('/api/assistant/gate/' + encodeURIComponent(gate.id), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ decision: approve ? 'approve' : 'skip' }),
        });
      } catch (e) {
        this.assistantError = String(e.message || e);
      }
      this._armAssistantPoll(0);
    },

    // --- the poll --------------------------------------------------------

    async _loadAssistantActivity() {
      const thread = this.assistantThread;
      if (!thread) return;
      const seq = ++this._assistantLoadSeq;
      let res;
      try {
        const headers = {};
        if (this._assistantETag) headers['If-None-Match'] = this._assistantETag;
        res = await this._assistantFetch(
          '/api/assistant/threads/' + encodeURIComponent(thread.id) + '/activity?after=' + this._assistantCursor,
          { headers: headers },
        );
      } catch (e) {
        this._armAssistantPoll();
        return;
      }
      if (seq !== this._assistantLoadSeq || !this.assistantThread || this.assistantThread.id !== thread.id) return;
      if (res.status === 304) {
        this._armAssistantPoll();
        return;
      }
      const data = await res.json();
      this._assistantETag = res.headers && res.headers.get ? (res.headers.get('ETag') || '') : '';
      this._mergeAssistantActivity(data);
      this._armAssistantPoll();
    },

    // Splice the new suffix onto the events already held. The daemon sends its
    // own cursor, which walks back over tool rows whose results arrived late.
    _mergeAssistantActivity(data) {
      this._assistantNoteScroll();
      const events = (data.tape && data.tape.events) || [];
      const offset = data.offset || 0;
      this.assistantEvents = this.assistantEvents.slice(0, offset).concat(events);
      this._assistantCursor = offset + events.length;
      this._assistantSetStreaming(!!data.running);
      this.assistantGate = data.gate || null;
      if (data.messages) this.assistantMessages = this._assistantMergeMessages(data.messages);
      if (data.autonomy) this.assistantAutonomy = data.autonomy;
      if (this.assistantThread) this.assistantThread.running = !!data.running;
      this._assistantScrollToEnd();
    },

    // The daemon only records a turn once the agent has exited, so from turn 2
    // on its list is one message short for the whole turn. The message just sent
    // is kept until the recorded list catches up with it.
    _assistantMergeMessages(recorded) {
      const highest = recorded.length ? recorded[recorded.length - 1].n : 0;
      const pending = this.assistantMessages.filter((m) => m.n > highest);
      return recorded.concat(pending);
    },

    // Re-arm while the turn is live or a write is waiting on the user. A chained
    // timeout, not an interval: it cannot stack requests on a slow daemon.
    _armAssistantPoll(delay) {
      this._stopAssistantPoll();
      if (!this.assistantOpen || !this.assistantThread) return;
      if (!this.assistantStreaming && !this.assistantGate && delay === undefined) return;
      const self = this;
      this._assistantPoll = setTimeout(function () {
        self._assistantPoll = null;
        self._loadAssistantActivity();
      }, delay === undefined ? ASSISTANT_POLL_MS : delay);
    },

    _stopAssistantPoll() {
      if (this._assistantPoll !== null) {
        clearTimeout(this._assistantPoll);
        this._assistantPoll = null;
      }
    },

    // --- the thread ------------------------------------------------------

    // The thread is two ordered streams: the messages the daemon recorded, and
    // the tape the agent wrote. They are interleaved on the clock, so a message
    // sits above the events its own turn produced rather than every message
    // stacking at the top of the pane. A turn's StartedAt precedes everything
    // its agent then writes, which is what makes the comparison hold.
    //
    // A tape with no readable timestamps keeps the messages ahead of the
    // events: nothing orders them, and dropping the first message to the bottom
    // would be the worse guess.
    assistantThreadRows() {
      const events = this.assistantEvents || [];
      const messages = this.assistantMessages || [];
      const rows = [];
      const message = (m, i) => ({ key: 'm' + m.n + '-' + i, msg: m });
      const event = (e, i) => ({ key: 'e' + i, index: i, event: e });
      let mi = 0;

      if (!events.some((e) => assistantMs(e.time) !== null)) {
        for (; mi < messages.length; mi++) rows.push(message(messages[mi], mi));
        for (let i = 0; i < events.length; i++) rows.push(event(events[i], i));
        return rows;
      }

      // An event carrying no time of its own is grouped with the one before it.
      let at = null;
      for (let i = 0; i < events.length; i++) {
        const t = assistantMs(events[i].time);
        if (t !== null) at = t;
        while (at !== null && mi < messages.length) {
          const started = assistantMs(messages[mi].at);
          if (started === null || started > at) break;
          rows.push(message(messages[mi], mi));
          mi++;
        }
        rows.push(event(events[i], i));
      }
      // The message just sent, whose turn has written nothing yet.
      for (; mi < messages.length; mi++) rows.push(message(messages[mi], mi));
      return rows;
    },

    // --- tool rows -------------------------------------------------------

    // A tool row is keyed by the agent's own call id so it stays open as the
    // tape grows. The index is the fallback for a tape that carries none.
    assistantToolKey(event, index) {
      return event && event.id ? event.id : 'i' + index;
    },

    assistantToolExpanded(event, index) {
      return !!this.assistantExpanded[this.assistantToolKey(event, index)];
    },

    toggleAssistantTool(event, index) {
      const key = this.assistantToolKey(event, index);
      if (this.assistantExpanded[key]) delete this.assistantExpanded[key];
      else this.assistantExpanded[key] = true;
    },

    assistantToolFailed(event) {
      return !!(event && event.is_error);
    },

    assistantEventTime(event) {
      if (!event || !event.time) return '';
      const d = new Date(event.time);
      if (isNaN(d.getTime())) return '';
      return d.toTimeString().slice(0, 8);
    },

    // --- context strip ---------------------------------------------------

    // What the pane says it can see, recomputed from the board rather than sent
    // to the agent: it tells the reader which ticket a bare "it" would mean.
    assistantRecomputeContext() {
      const chips = [];
      if (this.selectedTicket) {
        chips.push({ key: 'ticket', label: this.selectedTicket.id, kind: 'ticket' });
      }
      const query = (this.searchQuery || '').trim();
      if (query) chips.push({ key: 'filter', label: query, kind: 'filter' });
      if (this.isMobile && this.columns && this.columns.length) {
        const col = this.columns[Math.max(0, Math.min(this.activeColumn, this.columns.length - 1))];
        if (col) chips.push({ key: 'column', label: col.label, kind: 'column' });
      }
      this.assistantContext = chips;
      return chips;
    },

    // --- the slash and mention menus -------------------------------------

    // Open a menu when the caret sits on a "/" at the start of the draft or on
    // an "@" anywhere in it, and close it as soon as neither holds.
    assistantDraftChanged() {
      const draft = this.assistantDraft || '';
      if (draft.startsWith('/')) {
        const term = draft.slice(1).toLowerCase();
        const rows = ASSISTANT_SLASH
          .filter((c) => c.name.startsWith(term))
          .map((c) => ({ id: 'slash:' + c.name, label: '/' + c.name, sub: c.hint, insert: c.text }));
        this.assistantMenu = rows.length ? { kind: 'slash', rows: rows, index: 0, from: 0 } : null;
        return;
      }
      const at = draft.lastIndexOf('@');
      if (at >= 0 && !/\s/.test(draft.slice(at + 1))) {
        const term = draft.slice(at + 1);
        const parsed = this.parseFilterQuery(term);
        const rows = (this.tickets || [])
          .filter((t) => !term || this.ticketMatchesQuery(t, parsed))
          .slice(0, 8)
          .map((t) => ({ id: 'mention:' + t.id, label: t.id, sub: t.title, insert: t.id + ' ' }));
        this.assistantMenu = rows.length ? { kind: 'mention', rows: rows, index: 0, from: at + 1 } : null;
        return;
      }
      this.assistantMenu = null;
    },

    moveAssistantMenu(delta) {
      const menu = this.assistantMenu;
      if (!menu || !menu.rows.length) return;
      menu.index = (menu.index + delta + menu.rows.length) % menu.rows.length;
    },

    // Replace what the menu was opened on with the chosen row's text.
    acceptAssistantMenu(row) {
      const menu = this.assistantMenu;
      if (!menu) return false;
      const chosen = row || menu.rows[menu.index];
      if (!chosen) return false;
      this.assistantDraft = (this.assistantDraft || '').slice(0, menu.from) + chosen.insert;
      this.assistantMenu = null;
      return true;
    },

    // Enter sends the message unless a menu is open, in which case it picks the
    // highlighted row. Shift+Enter is always a newline.
    assistantComposerKey(event) {
      if (this.assistantMenu) {
        if (event.key === 'ArrowDown') { event.preventDefault(); this.moveAssistantMenu(1); return; }
        if (event.key === 'ArrowUp') { event.preventDefault(); this.moveAssistantMenu(-1); return; }
        if (event.key === 'Enter' || event.key === 'Tab') { event.preventDefault(); this.acceptAssistantMenu(); return; }
        if (event.key === 'Escape') { event.preventDefault(); this.assistantMenu = null; return; }
      }
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        this.sendAssistantMessage();
      }
    },

    // Escape closes whatever the pane has open, innermost first, and reports
    // whether it consumed the key. The body chain calls it after the palette
    // and the sheets, so those still win.
    assistantEscape() {
      if (this.assistantMenu) { this.assistantMenu = null; return true; }
      if (this.assistantAutonomyOpen) { this.assistantAutonomyOpen = false; return true; }
      if (this.assistantGate) { this.resolveAssistantGate(false); return true; }
      if (this.assistantView === 'history') { this.assistantView = 'thread'; return true; }
      if (this.assistantOpen) { this.closeAssistant(); return true; }
      return false;
    },
  };
}
