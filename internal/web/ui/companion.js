// Attention is daemon-owned. Opening the inbox and refreshing it never starts a model turn.
const COMPANION_HEADER = `
<div class="companion-header" x-show="companionEnabled()" x-cloak>
  <span class="companion-cat" :data-state="companionState()" aria-hidden="true"></span>
  <div class="companion-status">
    <span x-text="companionName()"></span>
    <span role="status" x-text="companionState()"></span>
  </div>
  <span class="companion-count" x-text="companionCounts()"></span>
</div>
<nav class="companion-tabs" aria-label="Assistant views">
  <button type="button" x-show="companionEnabled()" x-cloak @click="companionOpenInbox()" :aria-pressed="assistantView === 'inbox'">Inbox</button>
  <button type="button" @click="assistantShowChat()" :aria-pressed="assistantView === 'thread'" x-text="companionEnabled() && companionThread() ? companionName() + ' chat' : 'Chat'"></button>
  <button type="button" @click="assistantShowHistory()" :aria-pressed="assistantView === 'history'">History</button>
  <button type="button" x-show="companionThread() && assistantStreaming" @click="stopAssistantTurn()">Stop</button>
</nav>
<p class="companion-chat-hint companion-note" x-show="companionEnabled() && companionThread() && !companionSnapshot?.ai_available" x-cloak x-text="companionSnapshot?.hint || 'Configure an assistant agent to ask for an explanation.'"></p>`;

const COMPANION_INBOX = `
<section class="companion-inbox" aria-label="Shared companion inbox">
  <p class="companion-note">Reading an item does not answer a question or change a ticket. Read state is shared.</p>
  <p x-show="!companionSnapshot && !companionError" role="status">Loading attention…</p>
  <p x-show="companionSnapshot && !companionSnapshot.enabled">Observation is disabled. Saved attention remains on disk. Re-enable the companion in Assistant settings to load it.</p>
  <p x-show="companionSnapshot && companionSnapshot.health === 'degraded'" role="status" x-text="'Degraded: ' + (companionSnapshot?.error || 'Companion storage is unavailable. Read state cannot be saved.')"></p>
  <p x-show="companionError" role="status" x-text="companionError"></p>
  <div class="companion-actions">
    <button type="button" @click="companionRefresh()">Refresh</button>
    <button type="button" @click="companionCatchUp()" :disabled="!companionCanWrite()">Set changes checkpoint</button>
    <button type="button" @click="companionSend('briefing', 'Summarize current attention and recent changes.', [])" :disabled="!companionCanAsk()">Brief me</button>
  </div>
  <p class="companion-note" x-show="!companionSnapshot?.ai_available" x-text="companionSnapshot?.hint || 'Configure an assistant agent for briefings and investigations. The inbox needs no agent.'"></p>
  <p class="companion-note" x-show="companionSnapshot?.observed_at" x-text="'Last observation: ' + companionTime(companionSnapshot?.observed_at)"></p>
  <p class="companion-note">Future explanations omit inactive changes at or before this checkpoint. Read state does not change.</p>
  <p class="companion-note" x-show="companionSnapshot?.caught_up" x-text="'Changes checkpoint: ' + companionCheckpoint()"></p>
  <p x-show="companionSnapshot && !companionItems().length">No recorded attention. This is an observation history, not a complete activity log.</p>
  <ol class="companion-items">
    <template x-for="item in companionItems()" :key="item.id">
      <li :class="item.read ? 'companion-read' : ''">
        <div class="companion-item-meta">
          <span x-text="companionCondition(item)"></span>
          <span x-text="item.read ? 'Read' : 'Unread'"></span>
        </div>
        <button type="button" class="companion-ticket" @click="companionOpenTicket(item)" :disabled="!item.ticket_id" x-text="(item.ticket_id ? item.ticket_id + ': ' : '') + (item.title || item.condition)"></button>
        <p class="companion-detail" x-show="item.detail" x-text="item.detail"></p>
        <p class="companion-note" x-show="!item.active" x-text="companionInactiveReason(item)"></p>
        <p class="companion-note" x-show="item.identity_limited">Source identity is limited. Recurring questions may not be distinguishable.</p>
        <time class="companion-note" x-text="companionTime(item.changed_at || item.first_seen)"></time>
        <div class="companion-actions">
          <button type="button" @click="companionRead(item, !item.read)" :disabled="!companionCanWrite()" x-text="item.read ? 'Mark unread' : 'Mark read'"></button>
          <button type="button" @click="companionSend('investigation', 'Explain the recorded work and current attention for this ticket.', [item.ticket_id])" :disabled="!item.ticket_id || !companionCanAsk()">Investigate</button>
        </div>
      </li>
    </template>
  </ol>
</section>`;

const COMPANION_EVIDENCE = `
<details class="companion-evidence" x-show="companionEnabled() && companionThread()" x-cloak>
  <summary>Captured evidence <span x-show="companionEvidence().some(e => e.outdated)">(outdated)</span></summary>
  <p class="companion-note">Explanations describe captured records. They do not update current attention or change tickets.</p>
  <p x-show="!companionEvidence().length">Capture metadata is not available.</p>
  <template x-for="(packet, index) in companionEvidence()" :key="index">
    <div class="companion-evidence-packet">
      <p x-text="'Captured: ' + companionTime(packet.captured_at || packet.capture_time || packet.observed_at)"></p>
      <p x-show="packet.outdated">Outdated: sources changed after capture.</p>
      <p x-show="packet.omissions?.length" x-text="'Omissions: ' + companionMetadata(packet.omissions)"></p>
      <p x-show="packet.sources || packet.revisions || packet.source_revisions" x-text="'Source revisions: ' + companionMetadata(packet.sources || packet.revisions || packet.source_revisions)"></p>
      <p x-show="packet.revision !== undefined" x-text="'Attention revision: ' + packet.revision"></p>
      <p x-show="packet.window_end" x-text="'Observation window: ' + companionTime(packet.window_start) + ' to ' + companionTime(packet.window_end)"></p>
    </div>
  </template>
</details>`;

export function kontoraCompanion() {
  return {
    companionHeaderMarkup: COMPANION_HEADER,
    companionInboxMarkup: COMPANION_INBOX,
    companionEvidenceMarkup: COMPANION_EVIDENCE,
    companionSnapshot: null,
    companionError: '',
    companionDisconnected: false,
    companionBusy: false,
    companionWriting: false,
    companionTimer: null,
    companionFocus: null,
    companionFetchPending: false,
    companionFetchPromise: null,
    companionFetchAgain: false,
    companionSeq: 0,
    companionDestroyed: false,

    async companionInit() {
      this.companionDestroyed = false;
      this.companionFocus = () => this.companionRefresh();
      window.addEventListener('focus', this.companionFocus);
      this.companionTimer = setInterval(() => this.companionRefresh(), 30000);
      await this.companionRefresh();
      if (this.companionSnapshot?.enabled && !this.assistantThread) this.assistantView = 'inbox';
    },

    companionDestroy() {
      this.companionDestroyed = true;
      this.companionSeq++;
      clearInterval(this.companionTimer);
      this.companionTimer = null;
      if (this.companionFocus) window.removeEventListener('focus', this.companionFocus);
      this.companionFocus = null;
    },

    async companionRefresh() {
      if (this.companionDestroyed) return;
      if (this.companionFetchPending) { this.companionFetchAgain = true; return this.companionFetchPromise; }
      this.companionFetchPending = true;
      this.companionFetchPromise = this._companionFetchSnapshot();
      return this.companionFetchPromise;
    },

    async _companionFetchSnapshot() {
      const seq = ++this.companionSeq;
      try {
        const res = await this._assistantFetch('/api/assistant/companion');
        const snapshot = await res.json();
        if (seq !== this.companionSeq) return;
        this._companionAdoptSnapshot(snapshot);
        if (this.companionThread() && this.assistantOpen) {
          this._assistantETag = '';
          this._loadAssistantActivity();
        }
        this.companionDisconnected = false;
        this.companionError = '';
      } catch (e) {
        if (seq !== this.companionSeq) return;
        this.companionDisconnected = true;
        this.companionError = 'Disconnected. Saved attention may be stale. ' + String(e.message || e);
      } finally {
        this.companionFetchPending = false;
        if (this.companionFetchAgain && !this.companionDestroyed) {
          this.companionFetchAgain = false;
          this.companionRefresh();
        }
      }
    },

    companionEnabled() { return Boolean(this.companionSnapshot?.enabled); },
    _companionAdoptSnapshot(snapshot) {
      this.companionSnapshot = snapshot;
      if (!this.companionEnabled() && this.assistantView === 'inbox') this.assistantView = 'thread';
    },
    companionName() { return this.companionSnapshot?.name || 'Kotik'; },
    companionThread() { return this.assistantThread?.purpose === 'companion'; },
    companionItems() {
      return [...(this.companionSnapshot?.items || [])].sort((a, b) =>
        Number(b.active) - Number(a.active) || Number(a.read) - Number(b.read) ||
        String(b.changed_at || '').localeCompare(String(a.changed_at || '')));
    },
    companionCounts() {
      const items = this.companionSnapshot?.items || [];
      return items.filter(i => i.active).length + ' active / ' + items.filter(i => !i.read).length + ' unread';
    },
    companionState() {
      if (this.companionDisconnected) return 'disconnected';
      if (this.companionSnapshot?.health === 'degraded') return 'degraded';
      if (this.companionBusy || (this.companionThread() && this.assistantStreaming)) return 'thinking';
      if (!this.companionSnapshot?.enabled) return 'disabled';
      if ((this.companionSnapshot.items || []).some(i => i.active)) return 'needs you';
      return 'watching';
    },
    companionCondition(item) { return (item.condition || item.source || 'change').replaceAll('_', ' '); },
    companionInactiveReason(item) {
      return { completed: 'Completed', interrupted: 'Interrupted', no_longer_observed: 'No longer observed' }[item.inactive_reason] || 'No longer observed';
    },
    companionTime(value) {
      const d = value && new Date(value);
      return d && !isNaN(d.getTime()) ? d.toLocaleString() : 'not available';
    },
    companionMetadata(value) {
      if (value === undefined || value === null) return '';
      return typeof value === 'string' ? value : JSON.stringify(value);
    },
    companionCheckpoint() { return this.companionMetadata(this.companionSnapshot?.caught_up); },
    companionEvidence() {
      const evidence = this.companionThread() && this.assistantThread.evidence;
      return Array.isArray(evidence) ? evidence : (evidence ? [evidence] : []);
    },
    companionCanWrite() {
      return !!this.companionSnapshot?.enabled && !this.companionDisconnected &&
        this.companionSnapshot.health === 'ok' && !this.companionWriting;
    },
    companionCanAsk() {
      return !!(this.companionSnapshot?.enabled && this.companionSnapshot.ai_available &&
        !this.companionDisconnected && !this.companionBusy && !this.assistantStreaming);
    },
    companionOpenInbox() {
      if (!this.companionEnabled()) return;
      if (this.assistantView !== 'inbox') this._assistantSelectionSeq++;
      this.assistantView = 'inbox';
      this.companionRefresh();
    },
    async companionOpenTicket(item) {
      if (!item.ticket_id) return;
      const ticket = this.tickets.find(t => t.id === item.ticket_id) || { id: item.ticket_id };
      if (await this._paletteOpenTicket(ticket)) {
        if (this.isMobile) this.closeSheet();
      }
    },
    async companionPatch(path, body) {
      if (!this.companionCanWrite()) return;
      this.companionWriting = true;
      // Ignore pre-PATCH GET results; refetch after the write to include changes saved by other devices.
      const seq = ++this.companionSeq;
      try {
        const res = await this._assistantFetch(path, {
          method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        const snapshot = await res.json();
        if (seq === this.companionSeq) { this._companionAdoptSnapshot(snapshot); this.companionError = ''; }
      } catch (e) {
        await this.companionRefresh();
        this.companionError = 'Read state was not saved. ' + String(e.message || e);
      } finally {
        this.companionWriting = false;
        if (!this.companionError) this.companionRefresh();
      }
    },
    companionRead(item, read) {
      return this.companionPatch('/api/assistant/companion/items/' + encodeURIComponent(item.id), { revision: item.revision, read });
    },
    companionCatchUp() {
      return this.companionPatch('/api/assistant/companion', { revision: this.companionSnapshot?.revision });
    },

    async companionSend(kind, text, ticketIDs) {
      if (!this.companionCanAsk() || !text?.trim()) return;
      this.companionBusy = true;
      const requestedAt = new Date().toISOString();
      const selectionSeq = this._assistantSelectionSeq;
      const threadID = this.assistantThread?.id;
      const draft = this.assistantDraft;
      const stillSelected = () => !this.companionDestroyed && this._assistantSelectionSeq === selectionSeq &&
        this.assistantThread?.id === threadID;
      this.assistantError = null;
      this.companionError = '';
      try {
        const body = { kind, text: text.trim(), ticket_ids: ticketIDs || [] };
        if (kind === 'followup' && this.companionThread()) body.thread_id = this.assistantThread.id;
        const res = await this._assistantFetch('/api/assistant/companion/messages', {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        const thread = await res.json();
        this.assistantLoadThreads();
        if (!stillSelected()) return;
        this._assistantCloseStream();
        this._assistantClearPartial();
        this._stopAssistantPoll();
        this.assistantEvents = [];
        this._assistantCursor = 0;
        this._assistantETag = '';
        this._assistantLoadSeq++;
        this._assistantSetStreaming(false);
        const messages = thread.messages || [];
        const turn = thread.turns || ((messages[messages.length - 1]?.n || 0) + 1);
        const pending = messages.some(m => m.n === turn) ? messages : messages.concat([
          { n: turn, text: text.trim(), at: requestedAt },
        ]);
        this._assistantAdopt(thread, pending);
        if (kind === 'followup' && this.assistantDraft === draft && draft.trim() === text.trim()) {
          this.assistantDraft = '';
          this.assistantMenu = null;
        }
        this.assistantView = 'thread';
        this.assistantAutonomyOpen = false;
        await this._loadAssistantActivity();
        this._assistantScrollToEnd(true);
      } catch (e) {
        const error = String(e.message || e);
        if (stillSelected()) this.assistantError = error;
        if (!this.companionDestroyed) this.companionError = 'The explanation could not start. ' + error;
      } finally {
        this.companionBusy = false;
      }
    },
  };
}
