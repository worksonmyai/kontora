// Semantic ticket commands. Transport adapters retain their existing workflows;
// this mixin owns subject identity, conflict ownership and draft acknowledgement.
const MOVE_STATUSES = ['open', 'todo', 'paused', 'human_review', 'done', 'cancelled'];
const labels = { open: 'Open', todo: 'Todo', paused: 'Paused', human_review: 'Human Review', done: 'Done', cancelled: 'Cancelled' };
const commands = {
  start: { endpoint: 'run', label: 'Request pickup' },
  retry: { endpoint: 'retry', label: 'Retry stage' },
  pause: { endpoint: 'pause', label: 'Pause' },
  cancel: { endpoint: 'move', status: 'cancelled', label: 'Cancel' },
  skip: { endpoint: 'skip', label: 'Skip stage' },
  archive: { endpoint: 'archive', label: 'Archive' },
  delete: { endpoint: '', label: 'Delete ticket' },
  annotate: { endpoint: 'plannotator-annotate', label: 'Annotate on daemon host' },
  review: { endpoint: 'plannotator-review', label: 'Review on daemon host' },
  initialize: { endpoint: 'init', label: 'Initialize' },
  schedule: { endpoint: 'schedule', label: 'Schedule ticket' },
  'workflow-restart': { endpoint: 'workflow-restart', label: 'Restart workflow' },
  'copy-id': { label: 'Copy ticket ID' },
  'copy-link': { label: 'Copy ticket link' },
};

export function kontoraLifecycle() {
  return {
    lifecyclePending: Object.create(null),
    _lifecycleDeleteCommand: null,
    _lifecycleArchiveCommand: null,
    _lifecycleInitCommand: null,
    workflowReleases: Object.create(null),

    _humanWaitIdentity(ticket) {
      const x = ticket?.graph_execution, w = x?.wait;
      if (!this.ticketCanControl(ticket) || !this.workflowWaiting(ticket) || this.executionActive(ticket) ||
          ticket.invocation_id || w.kind !== 'human' || w.phase !== 'pending' || w.disarmed || w.receipt ||
          w.invocation_id || !x.execution_id || !w.id || w.execution_id !== x.execution_id ||
          w.visit_id !== x.visit_id || w.node_id !== x.current_node ||
          !Number.isSafeInteger(x.revision) || x.revision < 0) return '';
      return JSON.stringify([x.execution_id, w.id, x.revision]);
    },

    workflowInspectedWait() {
      const t = this.selectedTicket;
      if (this._appDestroyed || this.needsAuth || this.detailLoading || t?._graphDetailPending || this.currentView === 'archive') return '';
      const inspected = this._humanWaitIdentity(t);
      return inspected && inspected === this._humanWaitIdentity(this.lifecycleResolve(t)) ? inspected : '';
    },

    _workflowReleaseRecords() {
      return this.workflowReleases[this.ticketKey(this.selectedTicket)] || {};
    },

    workflowReleaseState() {
      const x = this.selectedTicket?.graph_execution, records = this._workflowReleaseRecords();
      return records[JSON.stringify([x?.execution_id, x?.wait?.id])] || Object.values(records).at(-1);
    },

    workflowReleaseForm() {
      const inspected = this.workflowInspectedWait();
      if (!inspected) return '';
      const state = this._workflowReleaseRecords()[JSON.stringify(JSON.parse(inspected).slice(0, 2))];
      return !state || state.outcome === 'failed' || state.outcome === 'pending' && state.inspected === inspected ? inspected : '';
    },

    _reconcileWorkflowReleaseReceipts(ticket) {
      const records = this.workflowReleases[this.ticketKey(ticket)], x = ticket.graph_execution;
      if (!records || !x) return;
      for (const run of [x, ...(x.previous_executions || [])]) {
        const accept = (wait, receipt) => {
          if (!receipt?.request_id) return;
          const state = records[JSON.stringify([run.execution_id, wait])];
          if (state?.requestID === receipt.request_id) state.outcome = 'accepted';
        };
        for (const w of [run.wait, ...(run.wait_history || [])]) {
          if (w) accept(w.id, w.receipt);
        }
        if (run.wake) accept(run.wake.wait_id, run.wake.receipt);
      }
    },

    workflowReleaseMessages() {
      const uncertain = Object.values(this._workflowReleaseRecords()).filter(state => state.outcome === 'uncertain');
      const messages = uncertain.map(state => 'Release acceptance is unknown. Request ID: ' + state.requestID +
        '. Refresh wait details to look for a matching receipt. A missing receipt does not mean the release failed. Do not submit a new release for this wait.');
      const state = this.workflowReleaseState();
      if (state?.outcome === 'accepted') {
        const [, wait] = JSON.parse(state.inspected);
        messages.push('Release accepted for ' + wait + '. Check the recorded execution for progress.');
      } else if (state?.message && state.outcome !== 'uncertain') {
        messages.push(state.message);
      }
      return messages;
    },

    workflowCanRefreshRelease() {
      return this.workflowReleaseState()?.outcome === 'conflict' ||
        Object.values(this._workflowReleaseRecords()).some(state => state.outcome === 'uncertain');
    },

    async workflowRefreshRelease() {
      const key = this.ticketKey(this.selectedTicket), detail = this._ticketDetailSeq;
      if (!this.workflowCanRefreshRelease() || this.lifecycleBusy(key)) return;
      const records = this._workflowReleaseRecords();
      const conflicts = Object.entries(records).filter(([, state]) => state.outcome === 'conflict');
      const response = await this.reloadTicketDetail({ fresh: true });
      if (response?.ok && detail === this._ticketDetailSeq && key === this.ticketKey(this.selectedTicket)) {
        for (const [wait, state] of conflicts) {
          if (records[wait] === state && state.outcome === 'conflict') delete records[wait];
        }
      }
    },

    async releaseWorkflowWait(inspected, reason, form) {
      const key = this.ticketKey(this.selectedTicket), detail = this._ticketDetailSeq, navigation = this._navigationSeq;
      reason = reason.trim();
      if (!key || !reason || !inspected || inspected !== this.workflowReleaseForm() || this.lifecycleBusy(key)) return;
      const ownsDetail = () => detail === this._ticketDetailSeq && navigation === this._navigationSeq && key === this.ticketKey(this.selectedTicket);
      const stillInspected = () => ownsDetail() && (!form || form.isConnected) && inspected === this.workflowReleaseForm();
      const [execution_id, wait_id, revision] = JSON.parse(inspected);
      const wait = JSON.stringify([execution_id, wait_id]);
      this.workflowReleases[key] ||= Object.create(null);
      const records = this.workflowReleases[key];
      this.lifecyclePending = { __proto__: null, ...this.lifecyclePending, [key]: 'workflow-release' };
      try {
        if (!await this.lifecycleDraftBarrier(key)) {
          if (stillInspected()) records[wait] = { inspected, outcome: 'failed', message: 'Save pending ticket changes before releasing this wait.' };
          return;
        }
        if (!stillInspected()) return;
        const requestID = crypto.randomUUID?.() || Array.from(crypto.getRandomValues(new Uint8Array(16)), n => n.toString(16).padStart(2, '0')).join('');
        records[wait] = { inspected, requestID, outcome: 'pending' };
        const state = records[wait];
        const result = await this._postWorkflowRelease(key, { execution_id, wait_id, revision, request_id: requestID, reason });
        if (state.outcome !== 'accepted') Object.assign(state, result);
      } catch (_) {
        if (stillInspected()) records[wait] = { inspected, outcome: 'failed', message: 'Could not prepare the release request.' };
        return;
      } finally {
        delete this.lifecyclePending[key];
      }
      if (ownsDetail()) await this.reloadTicketDetail({ fresh: true });
    },

    async _postWorkflowRelease(key, body) {
      try {
        return await this.ticketRead(async signal => {
          const response = await fetch(this.ticketURL(key, '/workflow-release'), {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body), signal,
          });
          if (response.ok) {
            const receipt = await response.json();
            if (receipt?.id && receipt.request_id === body.request_id) return { outcome: 'accepted' };
          } else if (response.status >= 400 && response.status < 500) {
            const failure = await response.json().catch(() => null);
            if (response.status === 409) {
              const message = 'Release refused. Refresh and inspect the current wait.';
              return { outcome: 'conflict', message: failure?.error ? message + ' ' + failure.error : message };
            }
            return { outcome: 'failed', message: failure?.error || 'Release request failed.' };
          }
          return { outcome: 'uncertain' };
        });
      } catch (_) {
        return { outcome: 'uncertain' };
      }
    },

    lifecycleResolve(subject) {
      const key = this.ticketKey(subject);
      if (!key) return null;
      const board = this.allBoardTickets().find(t => this.ticketKey(t) === key);
      const detail = [this.selectedTicket, this.selectedEpic].find(t => this.ticketKey(t) === key);
      // Board fields, including permissions, override detail; detail supplies the body.
      // A supplied object is an identity, not a fallback snapshot.
      return board ? { ...detail, ...board } : detail || null;
    },

    lifecycleBusy(subject) {
      const key = this.ticketKey(subject);
      // Alpine tracks `in`, not Object.hasOwn, so command completion re-enables controls.
      return key in this.lifecyclePending && Object.hasOwn(this.lifecyclePending, key);
    },

    _lifecycleState(ticket) {
      if (!ticket) return '';
      return JSON.stringify([this.ticketKey(ticket), ticket.status, this.executionActive(ticket),
        !!ticket.kontora, ticket.kind || '', ticket.stage || '', ticket.attempt || 0,
        ticket.can_control !== false, ticket.can_annotate === true,
        ticket.epic_execution || '', ticket.scheduled_at || '', ticket.invocation_id || '',
        ticket.claimed_by || '', ticket.running_status || '', ticket.graph_execution || null]);
    },

    lifecycleCommand(subject, command) {
      const ticket = this.lifecycleResolve(subject);
      const raw = typeof command === 'string' ? { id: command } : command || {};
      const id = raw.id || (raw.kind === 'move' ? 'move:' + raw.status : '');
      let value;
      if (id.startsWith('move:')) {
        const status = id.slice(5);
        value = { id, kind: 'move', endpoint: 'move', status, label: 'Move to ' + (labels[status] || status.replaceAll('_', ' ')) };
      } else if (id.startsWith('set-stage:')) {
        value = { id, kind: 'set-stage', endpoint: 'set-stage', stage: id.slice(10), label: 'Set stage' };
      } else if (commands[id]) {
        value = { ...commands[id], id, kind: id };
        if (id === 'retry' && ['done', 'cancelled'].includes(ticket?.status)) value.label = 'Run again';
        if (id === 'start' && ticket?.kind === 'epic') value.label = 'Start epic';
      } else return null;
      return { ...value, id, ticketKey: raw.ticketKey ?? this.ticketKey(subject),
        expected: raw.expected ?? this._lifecycleState(ticket),
        ...(raw.confirmed ? { confirmed: true } : {}),
        ...(raw.body !== undefined ? { body: raw.body } : {}),
        ...(raw.note !== undefined ? { note: raw.note } : {}) };
    },

    lifecycleDestinations(subject) {
      const ticket = this.lifecycleResolve(subject);
      if (!this.ticketCanControl(ticket) || ticket.status === 'archived') return [];
      if (ticket.kind === 'epic' && !this.epicIsAgent(ticket)) return [];
      const statuses = [...new Set(MOVE_STATUSES.concat(this.configCache?.custom_statuses || []))];
      return statuses.filter(status => status !== ticket.status &&
        (status !== 'paused' || this.executionActive(ticket) || this.workflowWaiting?.(ticket)))
        .map(status => this.lifecycleCommand(subject, 'move:' + status));
    },

    lifecycleActions(subject) {
      const ticket = this.lifecycleResolve(subject);
      if (!this.ticketCanControl(ticket) || ticket.status === 'archived') return [];
      if (ticket.kind === 'epic' && !this.epicIsAgent(ticket)) {
        return this.epicCanStart(ticket) ? [this.lifecycleCommand(subject, 'start')] : [];
      }
      const ids = [];
      if (this.executionActive(ticket)) ids.push('pause');
      else if (!ticket.kontora || ['open', 'todo'].includes(ticket.status)) ids.push('start');
      else ids.push('retry');
      return ids.map(id => this.lifecycleCommand(subject, id));
    },

    lifecycleFromEndpoint(subject, endpoint, body) {
      const id = endpoint === 'move' ? 'move:' + body?.status
        : endpoint === 'run' ? 'start'
        : endpoint === 'set-stage' ? 'set-stage:' + body?.stage
        : endpoint === 'plannotator-annotate' ? 'annotate'
        : endpoint === 'plannotator-review' ? 'review' : endpoint;
      return this.lifecycleCommand(subject, id);
    },

    _lifecycleAllowed(ticket, command) {
      if (command.kind.startsWith('copy-')) return !!ticket;
      if (!this.ticketCanControl(ticket) || ticket.status === 'archived') return false;
      if (ticket.id in (this.plannotatorInFlight || {}) && Object.hasOwn(this.plannotatorInFlight, ticket.id)) return false;
      switch (command.kind) {
        case 'move': return this.lifecycleDestinations(ticket).some(d => d.status === command.status);
        case 'start': return this.lifecycleActions(ticket).some(d => d.id === 'start');
        case 'retry': return ((!ticket.kontora && !this.executionActive(ticket)) || this.lifecycleActions(ticket).some(d => d.id === 'retry')) &&
          (!this.workflowWaiting?.(ticket) || ticket.graph_execution.wait.disarmed);
        case 'pause': return this.executionActive(ticket) || this.workflowWaiting?.(ticket);
        case 'skip': return this.canSkipStage(ticket);
        case 'set-stage': return !!command.stage && this.canSetStage(ticket);
        case 'annotate': return this.canAnnotateTicket(ticket);
        case 'review': return ticket.status === 'human_review' && (ticket.kind !== 'epic' || this.epicIsAgent(ticket));
        case 'workflow-restart': return this.canRestartGraph(ticket);
        case 'cancel': return ticket.status !== 'cancelled' && (ticket.kind !== 'epic' || this.epicIsAgent(ticket));
        case 'initialize': return this.needsInitModal(ticket);
        case 'schedule': return this.canSchedule(ticket);
        case 'archive': return ['done', 'cancelled'].includes(ticket.status);
        case 'delete': return true;
        default: return false;
      }
    },

    async lifecycleDraftBarrier(key) {
      // Re-read after every await. A queue tail can settle successfully while a
      // newer keystroke still lives only in the form/debounce.
      for (let pass = 0; pass < 20; pass++) {
        if (this.ticketKey(this.selectedTicket) === key && this.editing) {
          if (await this.flushEditSave() === false) return false;
        }
        if (this.ticketKey(this.selectedEpic) === key) {
          if (await this.epicFlushBrief() === false) return false;
        }
        const state = Object.hasOwn(this._editorSaves, key) ? this._editorSaves[key] : null;
        const tail = state?.tail;
        if (tail && await tail === false) return false;
        if (this.flushNotifySave) {
          if (await this.flushNotifySave(key) === false) return false;
        } else if (this.ticketKey(this.selectedTicket) === key && (this.notifySaving || this.notifyError)) {
          // Fail closed until the notification queue can attest acknowledgement.
          return false;
        }
        const current = Object.hasOwn(this._editorSaves, key) ? this._editorSaves[key] : null;
        if (tail !== current?.tail || state?.pending) continue;
        if (state && !this._sameEditorDraft(state.ack, state.draft)) return false;
        if (this.ticketKey(this.selectedTicket) === key && this.editing &&
            !this._sameEditorDraft(state?.ack || {}, this._ordinaryEditorDraft())) continue;
        return true;
      }
      return false;
    },

    async dispatchLifecycle(subject, request) {
      const navigation = this._navigationSeq;
      const key = this.ticketKey(subject);
      const command = this.lifecycleCommand(subject, request);
      const result = (outcome, error) => ({ outcome, ticketKey: key, command: command?.id, ...(error ? { error } : {}) });
      let ticket = this.lifecycleResolve(subject);
      if (!command || command.ticketKey !== key || !ticket) return result('refused');
      if (this.lifecycleBusy(key)) return result('busy');
      if (command.expected !== this._lifecycleState(ticket)) return result('obsolete');
      if (!this._lifecycleAllowed(ticket, command)) {
        const guidance = this.workflowWaiting?.(ticket) && command.kind === 'retry' ? this.workflowReleaseInstructions(ticket)
          : ['skip', 'set-stage'].includes(command.kind) ? 'Pause first and wait for the agent and review to stop before changing stages.' : 'Command unavailable for this ticket.';
        this.error = guidance;
        return result('refused', guidance);
      }
      if (command.kind.startsWith('copy-')) {
        try {
          const hash = this.isSecondary(ticket)
            ? '#/s/' + encodeURIComponent(ticket.source) + '/t/' + encodeURIComponent(ticket.id)
            : (ticket.status === 'archived' ? '#/archive/t/' : '#/t/') + encodeURIComponent(ticket.id);
          const text = command.kind === 'copy-id' ? ticket.id : new URL(hash, location.href).href;
          await navigator.clipboard.writeText(text);
          return result('success');
        } catch (e) { return result('failed', e.message); }
      }
      // A descriptor is owned until all its awaited work completes. The lock is
      // per source-qualified ticket, never actionLoading/global selection.
      this.lifecyclePending = { __proto__: null, ...this.lifecyclePending, [key]: command.id };
      this.error = null;
      try {
        if (!await this.lifecycleDraftBarrier(key)) {
          this.error = this.notifyError || this.error || 'Latest ticket changes have not been saved successfully.';
          return result('save-failed', this.error);
        }
        ticket = this.lifecycleResolve(key);
        if (!ticket || command.expected !== this._lifecycleState(ticket) || !this._lifecycleAllowed(ticket, command)) return result('obsolete');
        const starts = command.kind === 'start' || command.kind === 'retry' ||
          (command.kind === 'move' && command.status === 'todo');
        if (starts && this.needsInitModal(ticket)) {
          if (navigation !== this._navigationSeq) return result('obsolete');
          await this.openInitModal(ticket);
          return result(this.initModal ? 'initialized' : 'obsolete');
        }
        if (command.kind === 'archive' && !command.confirmed) {
          if (navigation !== this._navigationSeq) return result('obsolete');
          this.openArchivePrompt(key);
          return result('confirmation');
        }
        if (command.kind === 'delete' && !command.confirmed) {
          if (navigation !== this._navigationSeq) return result('obsolete');
          if (this.ticketKey(this.selectedTicket) !== key) {
            if (!await this._paletteOpenTicket(ticket) || this.ticketKey(this.selectedTicket) !== key) return result('refused');
            ticket = this.lifecycleResolve(key);
            if (command.expected !== this._lifecycleState(ticket) || !this._lifecycleAllowed(ticket, command)) return result('obsolete');
          }
          this.openDeleteModal();
          return result('confirmation');
        }
        let outcome;
        if (command.kind === 'move' || command.kind === 'cancel') {
          outcome = await this._moveTaskTransport(key, command.status);
        } else if (['annotate', 'review'].includes(command.kind)) {
          outcome = await this._startPlannotatorTransport(ticket, command.endpoint, command.label);
        } else {
          let body = command.kind === 'set-stage' ? { stage: command.stage } : command.body;
          if (command.kind === 'archive') body = { note: command.note || '' };
          outcome = await this._lifecyclePost(key, command.endpoint, body, command.kind === 'delete' ? 'DELETE' : 'POST');
        }
        if (outcome?.outcome === 'success' && !this._appDestroyed) {
          if (command.kind === 'start' || (command.kind === 'initialize' && ticket.kind !== 'epic')) {
            this.showToast(ticket.id + ': Pickup requested');
          } else if (command.kind === 'retry') {
            const message = this.workflowWaiting?.(ticket) ? 'Wait re-arm requested' : 'Retry requested';
            this.showToast(ticket.id + ': ' + message);
          }
        }
        return { ...result(outcome?.outcome || 'failed', outcome?.error), ...outcome, ticketKey: key, command: command.id };
      } catch (e) {
        this.error = e.message;
        return result('failed', e.message);
      } finally {
        delete this.lifecyclePending[key];
        // Sortable may already have moved a DOM node even on refusal/init.
        this.recomputeBoard();
      }
    },

    async _lifecycleRefresh() {
      const warning = this.error;
      try { await this.resyncTickets(); } catch (_) { /* retain the uncertain result */ }
      finally { if (warning) this.error = warning; }
    },

    async _lifecyclePost(key, endpoint, body, method = 'POST') {
      const touched = this._beginSnapshot();
      try {
        const options = { method };
        if (body !== undefined) { options.headers = { 'Content-Type': 'application/json' }; options.body = JSON.stringify(body); }
        if (method === 'DELETE') options.headers = { 'X-Kontora-Confirm': 'delete-ticket-file' };
        const response = await fetch(this.ticketURL(key, endpoint ? '/' + endpoint : ''), options);
        if (!response.ok) {
          const failure = await response.json().catch(() => null);
          this.error = failure?.error || (endpoint || 'Delete') + ' failed';
          return { outcome: 'failed', error: this.error };
        }
        const updated = method === 'DELETE' && response.status === 204 ? null : await response.json();
        if (method !== 'DELETE' && (!updated?.id || updated.id !== this.ticketIdentity(key).id || this.isSecondary(updated))) {
          throw new Error('Invalid ticket acknowledgement');
        }
        if (method === 'DELETE') {
          this.tickets = this.tickets.filter(t => this.ticketKey(t) !== key);
          if (this.ticketKey(this.selectedTicket) === key) this.closeDetail();
          this.updateFavicon();
        } else if (updated?.id === this.ticketIdentity(key).id && !touched.has(key) && !this._appDestroyed) {
          this.applyTicketUpdate(updated);
          this.recomputeBoard();
        } else if (touched.has(key)) {
          // SSE is newer than the HTTP response. Never overwrite it.
        } else if (endpoint !== 'init') {
          await this._lifecycleRefresh();
        }
        return { outcome: 'success' };
      } catch (e) {
        const error = this.error = 'Result uncertain; refresh before trying again: ' + e.message;
        await this._lifecycleRefresh();
        return { outcome: 'uncertain', error };
      } finally { this._endSnapshot(touched); }
    },
  };
}
