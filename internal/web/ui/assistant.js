// The assistant pane: a chat docked beside the board that answers questions
// about it and drives Kontora through the same verbs the CLI uses.
//
// Every key is prefixed. activity.js already owns toolKey, toolExpanded,
// toolFailed, toggleTool and eventTime for the ticket transcript, and merge()
// throws on a repeated key, so the pane carries its own copies rather than
// sharing state with the ticket detail view.

import { resourceOwner } from './board.js';

const ASSISTANT_POLL_MS = 1500;
const ASSISTANT_WINDOW = 200;

function assistantRaw(value) {
  return globalThis.Alpine?.raw ? globalThis.Alpine.raw(value) : value;
}

function assistantEqual(a, b) {
  if (a === b) return true;
  if (!a || !b || typeof a !== 'object' || typeof b !== 'object') return false;
  const keys = Object.keys(a);
  return keys.length === Object.keys(b).length &&
    keys.every(key => Object.hasOwn(b, key) && assistantEqual(a[key], b[key]));
}
// The wait before each reconnect, and the cap: four entries, so a turn gets
// five connections and the fifth failure opens nothing more.
const ASSISTANT_STREAM_BACKOFF = [800, 1600, 3200, 5000];
const ASSISTANT_WIDTH_MIN = 340;
const ASSISTANT_WIDTH_MAX = 640;
const ASSISTANT_WIDTH_DEFAULT = 420;
const ASSISTANT_MAIN_RESERVE = 320;

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

// Off the component, like termState: Alpine's deep proxy would wrap the
// EventSource and fire on every token appended to the buffer. Module scope is
// safe because only one kontora() component exists.
export const assistantStream = {
  es: null,
  // The thread the open connection belongs to, so a second call for it is a
  // no-op rather than a reconnect.
  threadID: '',
  // Monotonic, re-checked in every callback: a frame from a thread the user
  // switched away from must not paint into the new one.
  seq: 0,
  // The generation the client holds. A delta naming another one is ignored.
  gen: 0,
  // Deltas since the last paint.
  buf: '',
  // Separate from raf because the callback can run before raf is assigned,
  // which would leave a pending flag nothing clears.
  queued: false,
  raf: null,
  tries: 0,
  retry: null,
  retryAt: 0,
  exhausted: false,
};

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

function assistantFitWidth(preference, available) {
  if (available === null) return preference;
  return Math.min(preference, available, Math.max(ASSISTANT_WIDTH_MIN, available - ASSISTANT_MAIN_RESERVE));
}

export function kontoraAssistant() {
  let widthOwner = null, widthDrag = null;
  let presentationBound = false;
  let configSeq = 0;
  let autonomySeed = assistantStored('kontora-assistant-autonomy-seed', null);
  // Avoid reconnecting and replaying settled text after an empty partial with
  // no pending tool; polling can discover any later block.
  let partialSettled = false;
  // Native request handles must not pass through Alpine's deep proxy.
  const lane = { epoch: 0, owner: {}, sends: new Set(), flight: null, again: false, destroyed: false, resolved: new Map() };
  let history = { events: null, messages: null, rows: [], visible: null, start: -1 };
  return {
    assistantOpen: assistantStored('kontora-assistant-open', '0') === '1',
    assistantRail: assistantStored('kontora-assistant-rail', '0') === '1',
    // Saved preference; only an explicit resize updates it.
    assistantWidth: assistantClampWidth(assistantStored('kontora-assistant-width', ASSISTANT_WIDTH_DEFAULT)),
    _assistantAvailableWidth: null,
    get assistantRenderedWidth() {
      return assistantFitWidth(this.assistantWidth, this._assistantAvailableWidth);
    },
    // 'thread' is the live chat; 'history' is the list of past ones.
    assistantView: 'thread',
    assistantViewIntent: '',
    assistantAutonomy: assistantStored('kontora-assistant-autonomy', 'ask'),
    assistantFutureAutonomy: assistantStored('kontora-assistant-autonomy', 'ask'),
    // The GET /api/assistant payload. enabled false is what drives the
    // configure hint in place of the composer.
    assistantAgent: null,
    assistantDraft: '',
    _assistantDraftRevision: 0,
    assistantSending: false,
    assistantReadError: '',
    assistantReadRetries: 0,
    assistantReadRetryable: true,
    assistantStreaming: false,
    // The message being written. Plain text, not setProse: the 16-entry
    // markdown cache would evict per token, and an unterminated fence
    // mid-stream renders the rest of the reply as a code block.
    assistantPartial: '',
    // Bumped by a new block, so a reader replaces rather than appends.
    assistantPartialGen: 0,
    assistantPartialTool: '',
    // False once the block is sealed, which stops the caret.
    assistantPartialTyping: false,
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
    mobileAssistantContext: null,
    // The open slash or mention menu: { kind, rows, index, from }.
    assistantMenu: null,
    assistantAutonomyOpen: false,
    // Keyed by tool-call id, so a row stays open as the tape grows. Null
    // prototype: the keys come from the agent, and one called "constructor"
    // must not read a function where a boolean is expected.
    assistantExpanded: Object.create(null),
    assistantWindow: ASSISTANT_WINDOW,

    _assistantPoll: null,
    _assistantCursor: 0,
    _assistantActivityLoaded: false,
    _assistantETag: '',
    _assistantSelectionSeq: 0,
    _assistantSelecting: false,
    _assistantTurnStart: null,
    _assistantTick: null,
    _assistantStick: true,
    _assistantComposing: false,
    _assistantCompositionEnding: false,
    _assistantCompositionTimer: null,

    // A fetch that throws on a status the pane cannot use, so every caller can
    // catch one thing. 304 passes through: it is the poll's "nothing new",
    // which the ETag revalidation depends on.
    async _assistantFetch(path, options, current) {
      const res = await fetch(path, options);
      if (res.status === 401) {
        if (!current || current()) this.needsAuth = true;
        throw Object.assign(new Error('unauthorized'), { status: 401 });
      }
      if (res.status === 304 || res.ok) return res;
      const data = await res.json().catch(() => ({}));
      throw Object.assign(new Error(data?.error || ('the daemon answered ' + res.status)), { status: res.status });
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
        if (this.assistantOpen && !this._appDestroyed && !lane.destroyed) {
          this._startAssistantTick();
          this._assistantOpenStream();
        }
      }
      if (!running) {
        this._assistantTurnStart = null;
        this.assistantElapsed = 0;
        this._stopAssistantTick();
        this._assistantCloseStream();
        // The text stays: a turn that died before writing its session file
        // leaves it as the only record of what the agent said.
        // _assistantAdoptPartial owns the removal, in the response that carries
        // the settled row. The caret and the pending call go: neither outlives
        // the turn, and LiveText.End drops the call for the same reason.
        this.assistantPartialTyping = false;
        this.assistantPartialTool = '';
      }
      this.assistantStreaming = running;
    },

    _startAssistantTick() {
      this._stopAssistantTick();
      if (!this._assistantTurnStart || !this.assistantOpen || this._appDestroyed || lane.destroyed) return;
      const self = this;
      const context = this._assistantContextToken();
      this._assistantTick = setInterval(function () {
        if (!self._assistantContextCurrent(context)) return;
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

    // Presentation only. Opening or closing a shell never owns a server turn.
    assistantLaunchLabel() {
      if (this.assistantGate) return 'Approval';
      if (this.assistantStreaming) return 'Working';
      if (this.companionEnabled()) {
        const unread = this.assistantUnreadCount();
        if (unread) return 'Inbox ' + unread;
      }
      return '';
    },

    assistantLaunchHint() {
      const label = this.assistantLaunchLabel();
      if (label === 'Approval') return 'Assistant: waiting for your approval';
      if (label === 'Working') return 'Assistant: ' + this.assistantWorkingLabel();
      return label ? 'Assistant: ' + label + ' unread' : 'Open Assistant';
    },

    assistantUnreadCount() {
      return this.companionEnabled() ? (this.companionSnapshot?.items || []).filter(item => !item.read).length : 0;
    },

    assistantComposerID(name) {
      return (this.isMobile ? 'assistant-page-' : 'assistant-') + name;
    },

    // --- scrolling -------------------------------------------------------

    _assistantScrollEl() {
      return globalThis.document?.getElementById(this.assistantComposerID('thread'));
    },

    // Read before the tape is spliced: a reader who has scrolled up to a tool
    // row keeps their place, and everyone else is carried along.
    _assistantNoteScroll() {
      const el = this._assistantScrollEl();
      // Hidden or unmounted transcripts have no meaningful scroll metrics.
      if (this.assistantView === 'thread' && el?.clientHeight > 0) {
        this._assistantStick = (el.scrollHeight - el.scrollTop - el.clientHeight) < 60;
      }
    },

    // The latch, on the scroll event rather than on every frame. A per-frame
    // read sees the grown scrollHeight against a scrollTop $nextTick has not
    // written yet, and silently un-sticks the transcript mid-reply.
    assistantThreadScrolled() {
      this._assistantNoteScroll();
    },

    assistantPageScrollOwner() {
      const app = this;
      return resourceOwner(function (alive) {
        const thread = this.$el;
        const observer = new ResizeObserver(() => {
          // Keep the latest turn in view when the keyboard shrinks the transcript.
          if (alive() && app.assistantView === 'thread' && thread.clientHeight > 0 && app._assistantStick) {
            thread.scrollTop = thread.scrollHeight;
          }
        });
        observer.observe(thread);
        const transcript = thread.querySelector('[data-assistant-transcript]');
        if (transcript) observer.observe(transcript);
        return () => observer.disconnect();
      });
    },

    _assistantScrollToEnd(force) {
      if (!force && !this._assistantStick) return;
      const self = this;
      const context = this._assistantContextToken();
      this.$nextTick(function () {
        if (!self._assistantContextCurrent(context) || self.assistantView !== 'thread' || (!force && !self._assistantStick)) return;
        const el = self._assistantScrollEl();
        if (el?.clientHeight > 0) el.scrollTop = el.scrollHeight;
      });
    },

    // --- lifecycle -------------------------------------------------------

    // Fetch the assistant config and, when the pane was left open, the thread
    // it was left on. Called from the component's init.
    async initAssistant() {
      if (this._appDestroyed || lane.destroyed) return;
      if (this.isMobile && this.currentView !== 'assistant') this.assistantOpen = false;
      if (!presentationBound && this.$watch) {
        presentationBound = true;
        // Alpine can remove the outgoing composer's blur handler before teardown dispatches blur.
        this.$watch('isMobile', () => this.assistantCompositionReset());
      }
      const context = this._assistantContextToken();
      await this._assistantRefreshConfig();
      if (!this._assistantContextCurrent(context) || this.assistantViewIntent || this.assistantDraft) return;
      const last = assistantStored('kontora-assistant-thread', '');
      if (last && (this.assistantEnabled() || last.startsWith('k'))) {
        await this.assistantSelectThread(last, { quiet: true });
      }
      if (this.assistantOpen) this.assistantLoadThreads();
    },

    // Re-read GET /api/assistant. Called from init and again on every
    // config_reloaded, so a change to the assistant block reaches the pane
    // without a page reload. It touches nothing about the open thread — not
    // the cursor, not the poll — because a reload does not end a chat.
    async _assistantRefreshConfig() {
      const seq = ++configSeq;
      const current = () => seq === configSeq && !this._appDestroyed && !lane.destroyed;
      try {
        const res = await this._assistantFetch('/api/assistant', undefined, current);
        const agent = await res.json();
        if (!current()) return;
        this.assistantAgent = agent;
      } catch (e) {
        // Only the first fetch turns into the hint. A later one failing is a
        // blip in a pane that is already working, and replacing the composer
        // with the configure hint over it loses the draft in it.
        if (!current()) return;
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
      if (autonomySeed === mode) return;
      autonomySeed = mode;
      this.assistantFutureAutonomy = mode;
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
      this.assistantCompositionReset();
      this.assistantOpen = true;
      this.assistantRail = false;
      assistantStore('kontora-assistant-open', '1');
      assistantStore('kontora-assistant-rail', '0');
      this.assistantRecomputeContext();
      this.assistantLoadThreads();
      if (this.assistantEnabled() || this.companionThread()) {
        // One poll now rather than re-arming: opening onto a chat whose turn is
        // already running has nothing armed yet, and the payload is what says
        // it is running.
        this._assistantResume();
      }
    },

    _assistantContextToken() {
      return { epoch: lane.epoch, selection: this._assistantSelectionSeq, id: this.assistantThread?.id,
        owner: lane.owner, thread: assistantRaw(this.assistantThread) };
    },

    _assistantContextCurrent(context) {
      return !this._appDestroyed && !lane.destroyed && context.epoch === lane.epoch &&
        context.selection === this._assistantSelectionSeq && context.id === this.assistantThread?.id;
    },

    // Dismissal retires resources, but failed writes still own the retained thread state.
    _assistantRetainedContextCurrent(context) {
      return !this._appDestroyed && !lane.destroyed && context.owner === lane.owner &&
        context.thread === assistantRaw(this.assistantThread);
    },

    _assistantRetireActivity() {
      lane.epoch++;
      lane.again = false;
      this._assistantSelecting = false;
      this.companionBusy = false;
      const flight = lane.flight;
      lane.flight = null;
      if (flight) flight.controller.abort();
      this._stopAssistantPoll();
    },

    _assistantResume() {
      if (this._appDestroyed || lane.destroyed) return;
      if (this.assistantStreaming) this._startAssistantTick();
      return this._loadAssistantActivity({ fresh: true });
    },

    assistantDestroy() {
      lane.destroyed = true;
      this.closeAssistant();
      lane.resolved.clear();
      history = { events: null, messages: null, rows: [], visible: null, start: -1 };
    },

    closeAssistant({ persist = !this.isMobile } = {}) {
      this._assistantRetireWidth();
      this._assistantSelectionSeq++;
      this._assistantRetireActivity();
      this.assistantOpen = false;
      this.assistantMenu = null;
      this.assistantAutonomyOpen = false;
      this.assistantCompositionReset();
      if (persist) assistantStore('kontora-assistant-open', '0');
      // The pane's flight is retired above; only a new read-only badge poll
      // survives dismissal. Destruction disables even that background owner.
      this._armAssistantPoll();
      this._stopAssistantTick();
      this._assistantCloseStream();
    },

    _assistantDefaultInbox() {
      if (this.companionSnapshot?.enabled && !this.mobileAssistantContext && !this.assistantThread && !this.assistantDraft && !this.assistantViewIntent) {
        this.assistantView = 'inbox';
      }
    },

    mobileAssistantEnter(ticket, traversing, focus = true) {
      if (this.sheet) this.closeSheet(false);
      this.mobileAssistantContext = ticket || null;
      this.assistantCompositionReset();
      this.assistantOpen = true;
      this._assistantStick = true;
      if (ticket) {
        // A ticket menu opens actionable Chat, not the observation-only chat.
        if (this.companionThread() && !traversing) {
          this._assistantEmptyThread();
          this.assistantViewIntent = 'thread';
        }
        this.assistantView = 'thread';
      } else this._assistantDefaultInbox();
      this.assistantRecomputeContext();
      this.assistantLoadThreads();
      if (this.assistantEnabled() || this.companionThread()) this._assistantResume();
      if (!focus) return;
      const seq = this._navigationSeq;
      this.$nextTick(() => {
        if (seq === this._navigationSeq && this.isMobile && this.currentView === 'assistant' && !this.interactionOwner?.()) {
          this.$refs?.mobileAssistantTitle?.focus({ preventScroll: true });
        }
      });
    },

    mobileAssistantLeave() {
      this.mobileAssistantContext = null;
      if (this.assistantView === 'inbox' && !this.companionEnabled()) this.assistantView = 'thread';
      this.closeAssistant({ persist: false });
    },

    mobileAssistantBreakpointChanged() {
      if (this.isMobile) {
        if (this.currentView !== 'assistant' && this.assistantOpen) this.closeAssistant({ persist: false });
      } else if (this.currentView === 'assistant') {
        // A phone-only route must not strand a wide viewport without a board.
        void this.navigateDestination({ view: 'assistant', assistantTicket: this.mobileAssistantContext, replace: true });
      } else if (!this.assistantOpen && assistantStored('kontora-assistant-open', '0') === '1') {
        this.assistantOpen = true;
        this.assistantRecomputeContext();
        this.assistantLoadThreads();
        this._assistantResume();
      }
    },

    mobileAssistantTicketContext() {
      const t = this.mobileAssistantContext;
      return t && (this.lifecycleResolve(t) || t);
    },

    // The rail keeps the pane mounted at icon width, so a running turn is still
    // visible as a pulsing dot without the board being squeezed.
    toggleAssistantRail() {
      this.assistantRail = !this.assistantRail;
      assistantStore('kontora-assistant-rail', this.assistantRail ? '1' : '0');
      if (this.assistantRail) this._assistantRetireWidth();
      else this.assistantRecomputeContext();
    },

    // --- width -----------------------------------------------------------

    assistantPaneStyle() {
      if (this._assistantAvailableWidth !== null) return `width:${this.assistantRenderedWidth}px`;
      // Before the first layout read, constrain the dock against the host
      // using the same CSS sidebar width as the shell.
      const sidebar = this.sidebarHidden === false ? 'var(--assistant-sidebar-width, 220px)' : '0px';
      const available = `100% - ${sidebar}`;
      return `width:min(${this.assistantWidth}px, max(0px, calc(${available})), ` +
        `max(${ASSISTANT_WIDTH_MIN}px, calc(${available} - ${ASSISTANT_MAIN_RESERVE}px)))`;
    },

    assistantWidthOwner() {
      const app = this;
      const owner = resourceOwner(function (alive) {
        const pane = this.$el;
        const host = pane?.parentElement;
        if (!host || !app.assistantOpen || app.assistantRail || app.isMobile || app._appDestroyed || lane.destroyed) return;
        app._assistantRetireWidth();
        widthOwner = owner;
        const sidebar = host.querySelector('[data-assistant-sidebar]');
        let frame = null;
        const schedule = () => {
          if (!alive() || frame !== null) return;
          frame = requestAnimationFrame(() => {
            frame = null;
            if (!alive() || !pane.isConnected || app.isMobile || !app.assistantOpen || app.assistantRail) return;
            const hostWidth = host.getBoundingClientRect().width;
            // A display:none sidebar measures zero; observing its box also
            // catches visibility and responsive width changes independently.
            const sidebarWidth = sidebar?.getBoundingClientRect().width ?? 0;
            app._assistantAvailableWidth = Math.max(0, hostWidth - sidebarWidth);
          });
        };
        const observer = new ResizeObserver(schedule);
        observer.observe(host, { box: 'border-box' });
        if (sidebar) observer.observe(sidebar, { box: 'border-box' });
        schedule();
        return () => {
          observer.disconnect();
          if (frame !== null) cancelAnimationFrame(frame);
          if (widthOwner !== owner) return;
          widthOwner = null;
          widthDrag?.finish(false);
          app._assistantAvailableWidth = null;
        };
      });
      return owner;
    },

    _assistantRetireWidth() {
      widthOwner?.destroy();
      widthDrag?.finish(false);
    },

    setAssistantWidth(px) {
      widthDrag?.finish(false);
      this.assistantWidth = assistantClampWidth(px);
      assistantStore('kontora-assistant-width', String(this.assistantWidth));
      return this.assistantWidth;
    },

    // Drag the right-docked pane's left edge from where it is painted, not
    // from its saved width. Persist once on release, only if it was resized.
    startAssistantResize(event) {
      if (this.isMobile || this.assistantRail || this._appDestroyed || lane.destroyed) return;
      widthDrag?.finish(false);
      const startX = event.clientX;
      const startWidth = event.currentTarget?.parentElement?.getBoundingClientRect().width ?? this.assistantRenderedWidth;
      const preference = this.assistantWidth;
      const drag = widthDrag = { finish: commit => {
        if (widthDrag !== drag) return;
        widthDrag = null;
        window.removeEventListener('mousemove', move);
        window.removeEventListener('mouseup', up);
        if (!commit) this.assistantWidth = preference;
        else if (this.assistantWidth !== preference) assistantStore('kontora-assistant-width', String(this.assistantWidth));
      } };
      const move = e => {
        if (widthDrag !== drag || this.isMobile) return;
        const available = this._assistantAvailableWidth;
        const next = assistantFitWidth(assistantClampWidth(startWidth + startX - e.clientX), available);
        // Keep the saved width if the pointer returns to its start or the drag leaves the fitted width unchanged.
        this.assistantWidth = e.clientX === startX || next === assistantFitWidth(preference, available)
          ? preference : assistantClampWidth(next);
      };
      const up = () => drag.finish(!this.isMobile);
      window.addEventListener('mousemove', move);
      window.addEventListener('mouseup', up);
    },

    // --- threads ---------------------------------------------------------

    async assistantLoadThreads() {
      if (lane.destroyed) return;
      const context = this._assistantContextToken();
      const current = () => this._assistantContextCurrent(context);
      try {
        const res = await this._assistantFetch('/api/assistant/threads', undefined, current);
        const data = await res.json();
        if (current()) this.assistantThreads = (data && data.threads) || [];
      } catch (e) {
        if (current()) this.assistantThreads = [];
      }
    },

    assistantVisibleThreads() {
      return this.assistantThreads.filter(thread => thread.purpose !== 'companion' || this.companionEnabled());
    },

    assistantThreadVisible() {
      return !this.companionThread() || this.companionEnabled();
    },

    assistantShowHistory() {
      this._assistantNoteScroll();
      this.assistantViewIntent = 'history';
      this._assistantSelectionSeq++;
      this._assistantRetireActivity();
      this.assistantView = 'history';
      this._assistantResume();
      return this.assistantLoadThreads();
    },

    assistantShowChat() {
      this.assistantViewIntent = 'thread';
      if (this._assistantSelecting || this.assistantView !== 'thread' || !this.assistantThread || !this.assistantThreadVisible()) {
        this._assistantSelectionSeq++;
        this._assistantRetireActivity();
      }
      if (!this.assistantThreadVisible()) {
        this._assistantEmptyThread();
      }
      this.assistantView = 'thread';
      this._assistantResume();
    },

    async assistantNewThread(autonomy = this.assistantFutureAutonomy) {
      if (!this.assistantEnabled() || lane.destroyed) return null;
      this.assistantViewIntent = 'thread';
      this._assistantSelectionSeq++;
      this._assistantRetireActivity();
      this._stopAssistantTick();
      this._assistantCloseStream();
      const context = this._assistantContextToken();
      const current = () => this._assistantContextCurrent(context);
      this.assistantError = null;
      try {
        const res = await this._assistantFetch('/api/assistant/threads', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ autonomy }),
        }, current);
        const thread = await res.json();
        if (!current()) return null;
        this._assistantAdopt(thread, []);
        this.assistantEvents = [];
        this.assistantView = 'thread';
        this.assistantLoadThreads();
        return thread;
      } catch (e) {
        if (current()) {
          this.assistantError = String(e.message || e);
          if (this.assistantOpen) this._assistantResume();
        }
        return null;
      }
    },

    async assistantSelectThread(id, opts) {
      if (this._appDestroyed || lane.destroyed) return false;
      if (!opts?.quiet) this.assistantViewIntent = 'thread';
      lane.owner = {};
      this._assistantSelectionSeq++;
      this._assistantRetireActivity();
      this._assistantSelecting = true;
      this._stopAssistantTick();
      const context = this._assistantContextToken();
      const current = () => this._assistantContextCurrent(context);
      const quiet = !!(opts && opts.quiet);
      // Before the fetch: a frame in flight for the old thread must not paint
      // into the new one.
      this._assistantCloseStream();
      this._assistantClearPartial();
      try {
        const res = await this._assistantFetch('/api/assistant/threads/' + encodeURIComponent(id), undefined, current);
        const thread = await res.json();
        if (thread.purpose === 'companion') {
          if (quiet && this.mobileAssistantContext) {
            if (current()) this._assistantSelecting = false;
            return false;
          }
          if (!this.companionSnapshot) await this.companionRefresh();
          if (!this.companionEnabled()) {
            if (current()) {
              this._assistantSelecting = false;
              this._assistantResume();
            }
            return false;
          }
        }
        if (!current()) return false;
        this._assistantSelecting = false;
        this._assistantAdopt(thread, thread.messages || []);
        this.assistantEvents = [];
        this._assistantCursor = 0;
        this._assistantActivityLoaded = false;
        this._assistantETag = '';
        this.assistantView = 'thread';
        const adopted = this._assistantContextToken();
        await this._assistantResume();
        if (!this._assistantContextCurrent(adopted)) return false;
        this._assistantScrollToEnd(true);
        return true;
      } catch (e) {
        // A thread the browser remembered but the daemon has dropped is not an
        // error worth showing: the pane simply opens empty.
        if (!current()) return false;
        if (!quiet) this.assistantError = String(e.message || e);
        this._assistantEmptyThread();
        return false;
      }
    },

    async assistantDeleteThread(id) {
      const context = this._assistantContextToken();
      const current = () => this._assistantContextCurrent(context);
      try {
        await this._assistantFetch('/api/assistant/threads/' + encodeURIComponent(id), { method: 'DELETE' }, current);
      } catch (e) {
        if (current()) this.assistantError = String(e.message || e);
        return;
      }
      if (!current()) return;
      if (this.assistantThread && this.assistantThread.id === id) {
        this._assistantSelectionSeq++;
        this._assistantRetireActivity();
        this._assistantEmptyThread();
      }
      this.assistantLoadThreads();
    },

    // Transcript resets must preserve drafts and send/selection errors.
    _assistantEmptyThread() {
      lane.owner = {};
      this._assistantRetireActivity();
      this._assistantSetStreaming(false);
      this._assistantClearPartial();
      this.assistantThread = null;
      this._assistantSyncSending();
      this.assistantMessages = [];
      this.assistantEvents = [];
      this.assistantGate = null;
      this.assistantExpanded = Object.create(null);
      this.assistantWindow = ASSISTANT_WINDOW;
      this._assistantStick = true;
      this._assistantCursor = 0;
      this._assistantETag = '';
      this._assistantActivityLoaded = false;
      this._assistantResetReadRecovery();
      lane.resolved.clear();
      history = { events: null, messages: null, rows: [], visible: null, start: -1 };
      assistantStore('kontora-assistant-thread', '');
    },

    _assistantAdopt(thread, messages) {
      // A successful explicit selection/admission can establish a new read
      // owner even for the same ID (for example after authenticating again).
      this._assistantResetReadRecovery();
      if (thread?.id !== this.assistantThread?.id) {
        this._assistantRetireActivity();
        partialSettled = false;
        this._assistantSetStreaming(false);
        this._assistantClearPartial();
        this.assistantWindow = ASSISTANT_WINDOW;
        this._assistantStick = true;
        this._assistantActivityLoaded = false;
        this.assistantExpanded = Object.create(null);
        this.assistantGate = null;
        this._assistantCursor = 0;
        this._assistantETag = '';
        lane.resolved.clear();
      }
      this.assistantThread = thread;
      this._assistantSyncSending();
      this.assistantMessages = messages || [];
      this._assistantSetStreaming(!!(thread && thread.running));
      if (thread && thread.autonomy && thread.purpose !== 'companion') this.assistantAutonomy = thread.autonomy;
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

    _assistantClearSubmittedDraft(raw, revision) {
      if (this._assistantDraftRevision !== revision || this.assistantDraft !== raw) return;
      this.assistantDraft = '';
      this._assistantDraftRevision++;
      this.assistantMenu = null;
    },

    _assistantSyncSending() {
      this.assistantSending = [...lane.sends].some(send => send.id
        ? send.id === this.assistantThread?.id : send.owner === lane.owner);
    },

    _assistantStartSend() {
      const send = { id: this.assistantThread?.id, owner: lane.owner };
      lane.sends.add(send);
      this._assistantSyncSending();
      return send;
    },

    _assistantFinishSend(send) {
      lane.sends.delete(send);
      this._assistantSyncSending();
    },

    async sendAssistantMessage() {
      if (this.assistantSending) return;
      const mobilePage = this.currentView === 'assistant';
      if (this.isSecondary(mobilePage ? this.mobileAssistantContext : this.selectedTicket) && !(mobilePage && this.companionThread())) {
        this.assistantError = 'The assistant is unavailable for secondary tickets. Claim the ticket into primary first.';
        return;
      }
      const raw = this.assistantDraft || '';
      const revision = this._assistantDraftRevision;
      const text = raw.trim();
      if (this.companionThread()) return this.companionSend('followup', text, []);
      if (!text || !this.assistantEnabled() || this.assistantStreaming) return;
      // Identical retyping is a newer draft; text equality cannot authorize clearing.
      const autonomy = this.assistantEffectiveAutonomy();
      const payload = { text, autonomy, context: this._assistantPageContext() };
      const sending = this._assistantStartSend();
      try {
        let thread = this.assistantThread;
        if (!thread) {
          thread = await this.assistantNewThread(autonomy);
          if (!thread) return;
          sending.id = thread.id;
        }
        this._assistantClearSubmittedDraft(raw, revision);
        this.assistantError = null;
        this.assistantMessages = this.assistantMessages.concat([{ n: this.assistantMessages.length + 1, text, at: new Date().toISOString() }]);
        partialSettled = false;
        this._assistantResetReadRecovery();
        this._assistantSetStreaming(true, Date.now());
        this._assistantScrollToEnd(true);
        const context = this._assistantContextToken();
        const current = () => this._assistantContextCurrent(context);
        try {
          await this._assistantFetch('/api/assistant/threads/' + encodeURIComponent(thread.id) + '/messages', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
          }, current);
        } catch (e) {
          if (!this._assistantRetainedContextCurrent(context)) return;
          this._assistantSetStreaming(!!this.assistantThread?.running);
          this.assistantError = String(e.message || e);
          this._armAssistantPoll();
          return;
        }
        // A hidden badge may already have observed the pre-write idle state.
        if (current() || (!this.assistantOpen && this._assistantRetainedContextCurrent(context))) {
          await this._loadAssistantActivity({ fresh: true });
        }
      } finally {
        this._assistantFinishSend(sending);
      }
    },

    async stopAssistantTurn() {
      if (!this.assistantThread) return;
      this.assistantError = null;
      const context = this._assistantContextToken();
      const current = () => this._assistantContextCurrent(context);
      try {
        await this._assistantFetch('/api/assistant/threads/' + encodeURIComponent(context.id) + '/stop', { method: 'POST' }, current);
        if (current() || (!this.assistantOpen && this._assistantRetainedContextCurrent(context))) {
          await this._loadAssistantActivity({ fresh: true });
        }
      } catch (e) {
        if (current()) this.assistantError = String(e.message || e);
      }
    },

    // --- autonomy --------------------------------------------------------

    assistantAutonomies() {
      return [
        { key: 'read', label: 'read only', hint: 'answers questions, changes nothing' },
        { key: 'ask', label: 'ask first', hint: 'holds every change for your approval' },
        { key: 'auto', label: 'auto', hint: 'runs known reads and writes; asks for deletes and unclassified calls' },
      ];
    },

    setAssistantAutonomy(mode) {
      if (this.companionThread()) return;
      this.assistantAutonomy = mode;
      this.assistantFutureAutonomy = mode;
      this.assistantAutonomyOpen = false;
      assistantStore('kontora-assistant-autonomy', mode);
    },

    assistantEffectiveAutonomy() {
      return this.assistantThread ? this.assistantAutonomy : this.assistantFutureAutonomy;
    },

    assistantAutonomyLabel() {
      const mode = this.assistantEffectiveAutonomy();
      const row = this.assistantAutonomies().find((a) => a.key === mode);
      return row ? row.label : mode;
    },

    mobileAssistantComposerHint() {
      if (this.companionThread()) return 'Observation only';
      const mode = this.assistantEffectiveAutonomy();
      const hint = mode === 'read' ? 'Read only' : mode === 'auto' ? 'Can make changes' : 'Asks before changes';
      return hint + ' · type / or @';
    },

    // --- approval --------------------------------------------------------

    mobileAssistantProposal() {
      const { tool, arg } = this.assistantGate || {};
      const move = tool === 'move_ticket' ? arg?.match(/^([\w.-]+)\s*→\s*([a-z_]+)$/) :
        tool?.toLowerCase() === 'bash' ? arg?.match(/^kontora move ([\w][\w.-]*) ([a-z_]+)$/) : null;
      if (move && (move[2] === 'todo' || this.columns.some(col => col.statuses.includes(move[2])))) {
        const status = this.mobileTicketStatus({ status: move[2] });
        return { ticket: move[1], status };
      }
      // An arbitrary argument is not a status change. Show it unabridged.
      return { text: arg || tool || '' };
    },

    assistantApprovalLabel() {
      switch (this.assistantGate?.kind) {
        case 'delete': return 'delete';
        case 'write': return 'change';
        default: return 'unclassified call';
      }
    },

    async resolveAssistantGate(approve) {
      const gate = this.assistantGate;
      if (!gate) return;
      // Cleared first: the card must not stay on screen while the request is in
      // flight, or a second click sends a second answer for the same call.
      this.assistantGate = null;
      this.assistantError = null;
      const context = this._assistantContextToken();
      const resolution = { gate, context };
      lane.resolved.set(gate.id, resolution);
      const current = () => this._assistantContextCurrent(context);
      try {
        await this._assistantFetch('/api/assistant/gate/' + encodeURIComponent(gate.id), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ decision: approve ? 'approve' : 'skip' }),
        }, current);
      } catch (e) {
        if (lane.resolved.get(gate.id) !== resolution) return;
        lane.resolved.delete(gate.id);
        if (!this.assistantGate && this._assistantRetainedContextCurrent(resolution.context)) {
          this.assistantGate = resolution.gate;
        }
        if (!this._assistantRetainedContextCurrent(context)) return;
        this.assistantError = String(e.message || e);
        this._armAssistantPoll();
        return;
      }
      delete resolution.gate;
      delete resolution.context;
      if (current() || (!this.assistantOpen && this._assistantRetainedContextCurrent(context))) {
        await this._loadAssistantActivity({ fresh: true });
      }
    },

    // --- the poll --------------------------------------------------------

    _assistantResetReadRecovery() {
      this.assistantReadError = '';
      this.assistantReadRetries = 0;
      this.assistantReadRetryable = true;
    },

    assistantRetryActivity() {
      if (!this.assistantReadRetryable) return Promise.resolve();
      this._assistantResetReadRecovery();
      return this._loadAssistantActivity({ fresh: true });
    },

    _loadAssistantActivity(options) {
      if (!this.assistantThread || this._appDestroyed || lane.destroyed) return Promise.resolve();
      if (this.assistantReadError && (!this.assistantReadRetryable || this.assistantReadRetries > ASSISTANT_STREAM_BACKOFF.length || this._assistantPoll !== null)) return Promise.resolve();
      if (lane.flight) {
        if (options?.fresh) lane.again = true;
        return lane.flight.promise;
      }
      this._stopAssistantPoll();
      // There is no cross-source ordering in the SSE schema. Flush already
      // accepted tokens, then retire that writer until all reads have decoded.
      this._assistantFlushFrame();
      this._assistantCloseStream({ keepTries: true });
      const context = this._assistantContextToken();
      const flight = { controller: new AbortController(), promise: null };
      lane.flight = flight;
      const current = () => lane.flight === flight && this._assistantContextCurrent(context);
      flight.promise = (async () => {
        try {
          const headers = {};
          if (this._assistantETag) headers['If-None-Match'] = this._assistantETag;
          const res = await this._assistantFetch(
            '/api/assistant/threads/' + encodeURIComponent(context.id) + '/activity?after=' + this._assistantCursor,
            { headers, signal: flight.controller.signal }, current,
          );
          if (!current()) return;
          if (res.status === 304) {
            this._assistantResetReadRecovery();
            return;
          }
          const data = await res.json();
          if (!current()) return;
          this._mergeAssistantActivity(data);
          this._assistantETag = res.headers?.get?.('ETag') || '';
          this._assistantResetReadRecovery();
        } catch (e) {
          if (!current()) return;
          // Transcript failures retain the last good tape without replacing mutation feedback.
          this.assistantReadError = String(e.message || e);
          this.assistantReadRetryable = e.name !== 'AbortError' && ![401, 403, 404, 410].includes(e.status);
          this.assistantReadRetries++;
          lane.again = false;
        } finally {
          if (current()) {
            lane.flight = null;
            if (lane.again) {
              lane.again = false;
              await this._loadAssistantActivity();
            } else {
              if (this.assistantStreaming && this.assistantReadRetryable) this._assistantOpenStream();
              this._armAssistantPoll();
            }
          }
        }
      })();
      return flight.promise;
    },

    // Splice the new suffix onto the events already held. The daemon sends its
    // own cursor, which walks back over tool rows whose results arrived late.
    _mergeAssistantActivity(data) {
      const loaded = this._assistantActivityLoaded;
      this._assistantNoteScroll();
      const previousCount = loaded && !this._assistantStick ? this._assistantHistoryRows().length : 0;
      const events = (data.tape && data.tape.events) || [];
      const offset = data.offset || 0;
      // Partial-only responses can repeat the unchanged, unstable tool suffix.
      const unchanged = offset + events.length === this.assistantEvents.length &&
        events.every((event, i) => assistantEqual(event, this.assistantEvents[offset + i]));
      if (!unchanged) this.assistantEvents = this.assistantEvents.slice(0, offset).concat(events);
      this._assistantCursor = offset + events.length;
      // After the splice, so the swap is one paint. Before the setter, which
      // stops the caret once the turn is over.
      this._assistantAdoptPartial(data);
      this._assistantSetStreaming(!!data.running);
      // A hidden gate can share an accepted validator. A 304 cannot restore it after a failed decision.
      for (const [id, resolution] of lane.resolved) {
        if (Object.hasOwn(resolution, 'gate')) {
          resolution.gate = data.gate?.id === id ? data.gate : null;
          resolution.context = this._assistantContextToken();
        }
      }
      this.assistantGate = data.gate && !lane.resolved.has(data.gate.id) ? data.gate : null;
      if (data.messages) this.assistantMessages = this._assistantMergeMessages(data.messages);
      if (data.autonomy && !this.companionThread()) this.assistantAutonomy = data.autonomy;
      if (this.assistantThread) {
        this.assistantThread.running = !!data.running;
        if (Object.hasOwn(data, 'evidence')) this.assistantThread.evidence = data.evidence;
      }
      if (loaded && !this._assistantStick) {
        this.assistantWindow += Math.max(0, this._assistantHistoryRows().length - previousCount);
      }
      this._assistantActivityLoaded = true;
      this._assistantScrollToEnd();
    },

    // The daemon only records a turn once the agent has exited, so from turn 2
    // on its list is one message short for the whole turn. The message just sent
    // is kept until the recorded list catches up with it.
    _assistantMergeMessages(recorded) {
      const highest = recorded.length ? recorded[recorded.length - 1].n : 0;
      const pending = this.assistantMessages.filter((m) => m.n > highest);
      const merged = recorded.concat(pending);
      const previous = this.assistantMessages;
      if (merged.length === previous.length && merged.every((m, i) =>
        m.n === previous[i].n && m.text === previous[i].text && m.at === previous[i].at &&
        m.error === previous[i].error)) return previous;
      return merged;
    },

    _assistantAdoptPartial(data) {
      const text = data?.partial || '';
      const tool = data?.partial_tool || '';
      partialSettled = !text && !tool;
      if (partialSettled) {
        this._assistantClearPartial();
        return;
      }
      // The stream is closed for the whole read. This snapshot is authoritative,
      // including shorter text at a generation reused by a different turn.
      assistantStream.buf = '';
      this.assistantPartialGen = data?.partial_gen || 0;
      this.assistantPartial = text;
      this.assistantPartialTool = tool;
      this.assistantPartialTyping = !partialSettled;
    },

    _assistantClearPartial() {
      assistantStream.buf = '';
      this.assistantPartial = '';
      this.assistantPartialGen = 0;
      this.assistantPartialTool = '';
      this.assistantPartialTyping = false;
    },

    // --- the stream ------------------------------------------------------

    // The same text the poll carries, at ten frames a second. The poll stays
    // the source of truth: a stream that never opens costs only granularity.
    _assistantOpenStream() {
      if (typeof EventSource !== 'function') return;
      const thread = this.assistantThread;
      if (this._appDestroyed || !thread || !this.assistantOpen || lane.destroyed || lane.flight || partialSettled || !this.assistantReadRetryable) return;
      if (assistantStream.es && assistantStream.threadID === thread.id) return;
      if (assistantStream.exhausted || assistantStream.retry !== null) return;
      const wait = assistantStream.retryAt - Date.now();
      if (wait > 0) {
        const context = this._assistantContextToken();
        assistantStream.retry = setTimeout(() => {
          assistantStream.retry = null;
          assistantStream.retryAt = 0;
          if (this._assistantContextCurrent(context)) this._assistantOpenStream();
        }, wait);
        return;
      }
      this._assistantCloseStream({ keepTries: true });

      const seq = ++assistantStream.seq;
      const self = this;
      let es;
      try {
        es = new EventSource('/api/assistant/threads/' + encodeURIComponent(thread.id) + '/stream');
      } catch (e) {
        return;
      }
      assistantStream.es = es;
      assistantStream.threadID = thread.id;
      assistantStream.gen = this.assistantPartialGen;

      const guard = (fn) => (ev) => {
        if (seq !== assistantStream.seq) return;
        let data = {};
        if (ev && ev.data) { try { data = JSON.parse(ev.data); } catch (e) { return; } }
        fn(data);
      };

      es.addEventListener('reset', guard(function (d) {
        assistantStream.tries = 0;
        partialSettled = false;
        assistantStream.gen = d.gen || 0;
        assistantStream.buf = '';
        self.assistantPartialGen = assistantStream.gen;
        self.assistantPartial = d.text || '';
        self.assistantPartialTool = d.tool || '';
        self.assistantPartialTyping = !!(d.text || d.tool);
        self._assistantScrollToEnd();
      }));

      es.addEventListener('delta', guard(function (d) {
        assistantStream.tries = 0;
        if ((d.gen || 0) !== assistantStream.gen) return;
        assistantStream.buf += d.text || '';
        self._assistantQueueFrame(seq);
      }));

      es.addEventListener('tool', guard(function (d) {
        if ((d.gen || 0) !== assistantStream.gen) return;
        self.assistantPartialTool = d.name || '';
      }));

      es.addEventListener('end', guard(function () {
        self._assistantFlushFrame(seq);
        self.assistantPartialTyping = false;
      }));

      es.addEventListener('done', guard(function () {
        self._assistantFlushFrame(seq);
        self.assistantPartialTyping = false;
        partialSettled = true;
        // Bumps the sequence, so the error the EOF fires finds a stale one and
        // does not reconnect into a turn that is over.
        self._assistantCloseStream();
      }));

      es.onerror = function () {
        // Close first, unconditionally: a clean EOF also fires error, and the
        // browser reconnects on its own while we are still deciding.
        es.close();
        if (assistantStream.es === es) assistantStream.es = null;
        if (seq !== assistantStream.seq) return;
        if (!self.assistantOpen || !self.assistantStreaming) return;
        const wait = ASSISTANT_STREAM_BACKOFF[assistantStream.tries];
        if (wait === undefined) {
          assistantStream.exhausted = true;
          return;
        }
        assistantStream.tries++;
        assistantStream.retryAt = Date.now() + wait;
        assistantStream.retry = setTimeout(function () {
          if (seq !== assistantStream.seq) return;
          assistantStream.retry = null;
          assistantStream.retryAt = 0;
          if (!self.assistantOpen || !self.assistantStreaming) return;
          self._assistantOpenStream();
        }, wait);
      };
    },

    // One paint per burst of tokens, and no geometry read in it:
    // _assistantScrollToEnd only consults the latch.
    _assistantQueueFrame(seq) {
      if (assistantStream.queued) return;
      assistantStream.queued = true;
      const self = this;
      assistantStream.raf = requestAnimationFrame(function () {
        if (seq !== assistantStream.seq) return;
        assistantStream.queued = false;
        self._assistantFlushFrame(seq);
      });
    },

    // A no-op when nothing is buffered, so end and done can call it without
    // cancelling the frame already queued.
    _assistantFlushFrame(seq) {
      if (seq !== undefined && seq !== assistantStream.seq) return;
      if (!assistantStream.buf) return;
      this.assistantPartial += assistantStream.buf;
      assistantStream.buf = '';
      this.assistantPartialTyping = true;
      this._assistantScrollToEnd();
    },

    // opts.keepTries is for a reconnect, which must not reset its own counter.
    _assistantCloseStream(opts) {
      if (assistantStream.retry !== null) {
        clearTimeout(assistantStream.retry);
        assistantStream.retry = null;
      }
      if (assistantStream.queued) {
        cancelAnimationFrame(assistantStream.raf);
        assistantStream.queued = false;
      }
      assistantStream.raf = null;
      assistantStream.buf = '';
      if (assistantStream.es) {
        assistantStream.es.close();
        assistantStream.es = null;
      }
      assistantStream.threadID = '';
      // Preserving retry limits must still retire every callback from the old stream.
      assistantStream.seq++;
      if (!(opts && opts.keepTries)) {
        assistantStream.tries = 0;
        assistantStream.gen = 0;
        assistantStream.retryAt = 0;
        assistantStream.exhausted = false;
      }
    },

    // Closed panes still need approval and completion updates for the launch badge.
    // Chain timeouts so a slow daemon cannot accumulate status requests.
    _armAssistantPoll(delay) {
      this._stopAssistantPoll();
      if (this._appDestroyed || !this.assistantThread || lane.destroyed) return;
      if (lane.flight) {
        if (delay === undefined) return;
        // An explicitly scheduled refresh supersedes an older snapshot. Normal
        // activity notifications coalesce through _loadAssistantActivity instead.
        const flight = lane.flight;
        lane.flight = null;
        lane.again = false;
        flight.controller.abort();
      }
      if (this.assistantReadError) {
        if (!this.assistantReadRetryable || this.assistantReadRetries > ASSISTANT_STREAM_BACKOFF.length) return;
        delay = ASSISTANT_STREAM_BACKOFF[this.assistantReadRetries - 1];
      }
      if (!this.assistantStreaming && !this.assistantGate && delay === undefined) return;
      const self = this;
      const context = this._assistantContextToken();
      const poll = setTimeout(function () {
        if (self._assistantPoll !== poll) return;
        self._assistantPoll = null;
        if (!self._assistantContextCurrent(context)) return;
        self._loadAssistantActivity();
      }, delay === undefined ? ASSISTANT_POLL_MS : delay);
      this._assistantPoll = poll;
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
    _assistantHistoryRows() {
      const events = this.assistantEvents;
      const messages = this.assistantMessages;
      const rawEvents = assistantRaw(events);
      const rawMessages = assistantRaw(messages);
      if (history.events === rawEvents && history.messages === rawMessages) return history.rows;
      const rows = [];
      history = { events: rawEvents, messages: rawMessages, rows, visible: null, start: -1 };
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

    assistantThreadRows() {
      const rows = this._assistantHistoryRows();
      const start = Math.max(0, rows.length - this.assistantWindow);
      if (history.start !== start || !history.visible) {
        history.start = start;
        history.visible = rows.slice(start);
      }
      return history.visible;
    },

    assistantHiddenRows() {
      return Math.max(0, this._assistantHistoryRows().length - this.assistantWindow);
    },

    assistantLoadEarlier() {
      const el = this._assistantScrollEl();
      const height = el?.scrollHeight || 0;
      const top = el?.scrollTop || 0;
      const context = this._assistantContextToken();
      this.assistantWindow = Math.min(this._assistantHistoryRows().length, this.assistantWindow + ASSISTANT_WINDOW);
      this.$nextTick(() => {
        if (el && this.assistantOpen && this._assistantContextCurrent(context)) {
          el.scrollTop = top + el.scrollHeight - height;
        }
      });
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
      const ticket = this.currentView === 'assistant' ? this.mobileAssistantContext : this.selectedTicket;
      if (ticket) {
        chips.push({ key: 'ticket', label: ticket.id, kind: 'ticket' });
      }
      const query = (this.searchQuery || '').trim();
      if (query) chips.push({ key: 'filter', label: query, kind: 'filter' });
      if (this.isMobile && this.mobileFilterLabel) chips.push({ key: 'column', label: this.mobileFilterLabel(), kind: 'column' });
      this.assistantContext = chips;
      return chips;
    },

    // --- the slash and mention menus -------------------------------------

    // Open a menu when the caret sits on a "/" at the start of the draft or on
    // an "@" anywhere in it, and close it as soon as neither holds.
    assistantDraftChanged() {
      this._assistantDraftRevision++;
      if (this._assistantComposing) return;
      const draft = this.assistantDraft || '';
      if (this.companionThread()) { this.assistantMenu = null; return; }
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
      globalThis.document?.getElementById(this.assistantComposerID('suggestion-' + menu.index))?.scrollIntoView({ block: 'nearest' });
    },

    // Replace what the menu was opened on with the chosen row's text.
    acceptAssistantMenu(row) {
      const menu = this.assistantMenu;
      if (!menu) return false;
      const chosen = row || menu.rows[menu.index];
      if (!chosen) return false;
      this.assistantDraft = (this.assistantDraft || '').slice(0, menu.from) + chosen.insert;
      this._assistantDraftRevision++;
      this.assistantMenu = null;
      this.$nextTick?.(() => {
        document.getElementById(this.assistantComposerID('input'))?.focus();
      });
      return true;
    },

    assistantCompositionStart() {
      this.assistantCompositionReset();
      this._assistantComposing = true;
      this.assistantMenu = null;
    },

    assistantCompositionEnd() {
      this._assistantComposing = false;
      this._assistantCompositionEnding = true;
      // Safari can report compositionend before the confirming Enter keydown.
      // Keep that key out of the send and suggestion paths until this task ends.
      clearTimeout(this._assistantCompositionTimer);
      this._assistantCompositionTimer = setTimeout(() => {
        this._assistantCompositionTimer = null;
        this._assistantCompositionEnding = false;
        this.assistantDraftChanged();
      }, 0);
    },

    assistantCompositionReset() {
      clearTimeout(this._assistantCompositionTimer);
      this._assistantCompositionTimer = null;
      this._assistantComposing = false;
      this._assistantCompositionEnding = false;
    },

    // Enter sends unless a suggestion is open. Shift+Enter stays a newline.
    assistantComposerKey(event) {
      if (event.isComposing || event.keyCode === 229 || this._assistantComposing || this._assistantCompositionEnding) {
        event.stopPropagation?.();
        return;
      }
      if (event.key === 'Enter' && event.shiftKey) return;
      if (this.assistantMenu) {
        if (event.key === 'ArrowDown') { event.preventDefault(); this.moveAssistantMenu(1); return; }
        if (event.key === 'ArrowUp') { event.preventDefault(); this.moveAssistantMenu(-1); return; }
        if (event.key === 'Enter' || event.key === 'Tab') { event.preventDefault(); this.acceptAssistantMenu(); return; }
        if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation?.(); this.assistantMenu = null; return; }
      }
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        this.sendAssistantMessage();
      }
    },

    // Escape closes whatever the pane has open, innermost first, and reports
    // whether it consumed the key. The body chain calls it after the palette,
    // sheets and ticket details, so those still win.
    assistantEscape() {
      if (this.assistantMenu) { this.assistantMenu = null; return true; }
      if (this.assistantAutonomyOpen) { this.assistantAutonomyOpen = false; return true; }
      if (this.isMobile && this.currentView === 'assistant') {
        if (this.assistantView !== 'thread') this.assistantShowChat();
        else this.mobileAssistantBack();
        return true;
      }
      if (this.assistantView === 'history') { this.assistantShowChat(); return true; }
      if (this.assistantOpen) { this.closeAssistant(); return true; }
      return false;
    },
  };
}
