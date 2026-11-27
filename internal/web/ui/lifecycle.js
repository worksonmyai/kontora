// Semantic ticket commands. Transport adapters retain their existing workflows;
// this mixin owns subject identity, conflict ownership and draft acknowledgement.
const MOVE_STATUSES = ['open', 'todo', 'paused', 'human_review', 'done', 'cancelled'];
const labels = { open: 'Open', todo: 'Queued', paused: 'Paused', human_review: 'Human review', done: 'Done', cancelled: 'Cancelled' };
const commands = {
  start: { endpoint: 'run', label: 'Run…' },
  'queue-saved': { endpoint: 'run', label: 'Queue' },
  retry: { endpoint: 'retry', label: 'Run again' },
  pause: { endpoint: 'pause', label: 'Pause' },
  cancel: { endpoint: 'move', status: 'cancelled', label: 'Cancel' },
  skip: { endpoint: 'skip', label: 'Skip stage' },
  archive: { endpoint: 'archive', label: 'Archive…' },
  delete: { endpoint: '', label: 'Delete ticket…' },
  annotate: { endpoint: 'plannotator-annotate', label: 'Annotate in Plannotator' },
  review: { endpoint: 'plannotator-review', label: 'Review diff in Plannotator' },
  initialize: { endpoint: 'init', label: 'Set up and run…' },
  schedule: { endpoint: 'schedule', label: 'Schedule ticket' },
  'workflow-restart': { endpoint: 'workflow-restart', label: 'Restart workflow' },
  'copy-id': { label: 'Copy ticket ID' },
  'copy-link': { label: 'Copy ticket link' },
};

const lifecycleReasons = {
  refused: 'Command unavailable for this ticket.',
  busy: 'Another command is pending for this ticket.',
  obsolete: 'This ticket changed. Review it before trying again.',
  cancelled: 'The command was cancelled before it was sent.',
  failed: 'The command failed.',
  uncertain: 'Result unknown.',
};
const pickupExpired = 'The pickup time passed. Choose a new time.';

function scheduleBodyError(body, futureOnly) {
  if (body?.clear === true && body.scheduled_at === undefined) return '';
  if (body?.clear !== undefined && body.clear !== false) return 'Specify a pickup time or clear the schedule, not both.';
  const iso = body?.scheduled_at;
  // The input parser has already resolved local times/delays; only an absolute
  // instant belongs in a reviewed descriptor. Date.parse alone normalizes bad days.
  const date = typeof iso === 'string' && /^(\d{4})-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$/.exec(iso);
  const time = date ? Date.parse(iso) : NaN;
  if (!Number.isFinite(time) || new Date(date[1] + '-' + date[2] + '-' + date[3] + 'T00:00:00Z').getUTCDate() !== +date[3]) {
    return 'Enter a valid absolute pickup time.';
  }
  if (futureOnly ? time <= Date.now() : time < Date.now() - 60_000) return pickupExpired;
  return '';
}

export function kontoraLifecycle() {
  let reconciliationSequence = 0;
  const reconciliationReads = new Map();
  return {
    lifecyclePending: Object.create(null),
    _lifecycleUncertain: Object.create(null),
    _lifecycleReviewEpochs: Object.create(null),
    _lifecycleDeleteCommand: null,
    _lifecycleArchiveCommand: null,
    _lifecycleInitCommand: null,

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

    lifecycleNeedsReconciliation(subject) {
      const key = this.ticketKey(subject);
      return !!key && key in this._lifecycleUncertain && Object.hasOwn(this._lifecycleUncertain, key);
    },

    _lifecycleState(ticket) {
      if (!ticket) return '';
      const key = this.ticketKey(ticket);
      return JSON.stringify([key, this._lifecycleReviewEpochs[key] || 0, ticket.status, this.executionActive(ticket),
        !!ticket.kontora, ticket.kind || '', ticket.stage || '', ticket.attempt || 0, ticket.pipeline || '',
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
        // Pickers name the destination; standalone commands name the action.
        const statusLabel = Object.hasOwn(labels, status) ? labels[status] : status.charAt(0).toUpperCase() + status.slice(1).replaceAll('_', ' ');
        value = { id, kind: 'move', endpoint: 'move', status, statusLabel, label: 'Set status to ' + statusLabel };
      } else if (id.startsWith('set-stage:')) {
        value = { id, kind: 'set-stage', endpoint: 'set-stage', stage: id.slice(10), label: 'Set stage' };
      } else if (commands[id]) {
        value = { ...commands[id], id, kind: id };
        if (id === 'retry' && this.workflowWaiting?.(ticket) && ticket.graph_execution.wait.disarmed) value.label = 'Rearm wait';
        else if (id === 'retry' && ticket?.stage) value.label = 'Run again from ' + ticket.stage;
        if (id === 'start' && ticket?.kind === 'epic') value.label = 'Start epic…';
        else if (id === 'start' && ticket && !this.needsInitModal(ticket)) value.label = 'Run';
      } else return null;
      // Capture the schedule payload with the descriptor, not a reference to a
      // live form. Queue always runs from the daemon's saved settings, bodyless.
      const body = id === 'queue-saved' ? undefined : id === 'schedule' && raw.body
        ? { ...raw.body } : raw.body;
      return { ...value, id, ticketKey: raw.ticketKey ?? this.ticketKey(subject),
        expected: raw.expected ?? this._lifecycleState(ticket),
        ...(raw.confirmed ? { confirmed: true } : {}),
        ...(body !== undefined ? { body } : {}),
        ...(raw.note !== undefined ? { note: raw.note } : {}) };
    },

    _lifecycleMoveStatuses(ticket) {
      if (!this.ticketCanControl(ticket) || ticket.status === 'archived') return [];
      if (ticket.kind === 'epic' && !this.epicIsAgent(ticket)) return [];
      const statuses = [...new Set(MOVE_STATUSES.concat(this.configCache?.custom_statuses || []))];
      return statuses.filter(status => status !== ticket.status &&
        (status !== 'paused' || this.executionActive(ticket) || this.workflowWaiting?.(ticket)));
    },

    lifecycleDestinations(subject) {
      return this._lifecycleMoveStatuses(this.lifecycleResolve(subject)).map(status => this.lifecycleCommand(subject, 'move:' + status));
    },

    _lifecycleActionID(ticket) {
      if (!this.ticketCanControl(ticket) || ticket.status === 'archived') return '';
      if (ticket.kind === 'epic' && !this.epicIsAgent(ticket)) return this.epicCanStart(ticket) ? 'start' : '';
      if (this.executionActive(ticket)) return 'pause';
      return !ticket.kontora || ['open', 'todo'].includes(ticket.status) ? 'start' : 'retry';
    },

    lifecycleActions(subject) {
      const id = this._lifecycleActionID(this.lifecycleResolve(subject));
      return id ? [this.lifecycleCommand(subject, id)] : [];
    },

    _lifecycleCanQueueSaved(ticket) {
      return !this.lifecycleNeedsReconciliation(ticket) && ticket?.kontora === true && ticket.kind !== 'epic' &&
        ticket.status === 'open' && !ticket.scheduled_at && this._lifecycleActionID(ticket) === 'start';
    },

    lifecycleCanQueueSaved(subject) {
      return this._lifecycleCanQueueSaved(this.lifecycleResolve(subject));
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
      if (this.lifecycleNeedsReconciliation(ticket) || !this.ticketCanControl(ticket) || ticket.status === 'archived') return false;
      if (ticket.id in (this.plannotatorInFlight || {}) && Object.hasOwn(this.plannotatorInFlight, ticket.id)) return false;
      switch (command.kind) {
        case 'move': return this._lifecycleMoveStatuses(ticket).includes(command.status);
        case 'start': return this._lifecycleActionID(ticket) === 'start';
        case 'queue-saved': return this._lifecycleCanQueueSaved(ticket);
        case 'retry': return ((!ticket.kontora && !this.executionActive(ticket)) || this._lifecycleActionID(ticket) === 'retry') &&
          (!this.workflowWaiting?.(ticket) || ticket.graph_execution.wait.disarmed);
        case 'pause': return this.executionActive(ticket) || this.workflowWaiting?.(ticket);
        case 'skip': return this.canSkipStage(ticket);
        case 'set-stage': return !!command.stage && this.canSetStage(ticket);
        case 'annotate': return this.canAnnotateTicket(ticket);
        case 'review': return ticket.status === 'human_review' && (ticket.kind !== 'epic' || this.epicIsAgent(ticket));
        case 'workflow-restart': return this.canRestartGraph(ticket);
        case 'cancel': return ticket.status !== 'cancelled' && (ticket.kind !== 'epic' || this.epicIsAgent(ticket));
        case 'initialize': return this.needsInitModal(ticket);
        case 'schedule': return this.canSchedule(ticket) && !this.executionActive(ticket);
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

    // The caller owns the batch and its result ledger. isOwner is synchronous;
    // losing it cancels unsent work, but never aborts or re-labels an in-flight write.
    async dispatchLifecycle(subject, request, { isOwner, suppressToast = false, validateBeforeWrite } = {}) {
      const navigation = this._navigationSeq;
      const key = this.ticketKey(subject);
      const command = this.lifecycleCommand(subject, request);
      const batch = !!isOwner;
      const owned = () => !batch || (!this._appDestroyed && isOwner());
      const reconcileAfterPost = batch || ['queue-saved', 'schedule'].includes(command?.kind);
      let transportStarted = false;
      const result = (outcome, error = lifecycleReasons[outcome]) => ({ outcome, ticketKey: key,
        command: command?.id, transportStarted, ...(error ? { error } : {}) });
      const refused = error => { if (!batch) this.error = error; return result('refused', error); };
      let ticket = this.lifecycleResolve(subject);
      if (!owned()) return result('cancelled');
      if (!command || command.ticketKey !== key) return result('refused');
      if (!command.kind.startsWith('copy-') && this.lifecycleNeedsReconciliation(key)) {
        const pending = this._lifecycleUncertain[key];
        if (!batch) this.error = pending.error;
        return { ...result('blocked', pending.error), resolution: 'refresh-required', reconciliationId: pending.id };
      }
      if (!ticket) return result('refused');
      if (this.lifecycleBusy(key)) return result('busy');
      if (command.expected !== this._lifecycleState(ticket)) return result('obsolete');
      if (!this._lifecycleAllowed(ticket, command)) {
        const guidance = this.workflowWaiting?.(ticket) && command.kind === 'retry' ? this.workflowReleaseInstructions(ticket)
          : ['skip', 'set-stage'].includes(command.kind) ? 'Pause first and wait for the agent and review to stop before changing stages.' : lifecycleReasons.refused;
        return refused(guidance);
      }
      const scheduleError = () => command.kind === 'schedule' && scheduleBodyError(command.body, batch);
      const invalidSchedule = error => batch && error === pickupExpired ? result('expired', error) : refused(error);
      const invalid = scheduleError();
      if (invalid) return invalidSchedule(invalid);
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
      if (!batch) this.error = null;
      try {
        const saved = await this.lifecycleDraftBarrier(key);
        if (!owned()) return result('cancelled');
        if (!saved) {
          const error = this.notifyError || this.error || 'Latest ticket changes have not been saved successfully.';
          if (!batch) this.error = error;
          return result('save-failed', error);
        }
        ticket = this.lifecycleResolve(key);
        const current = () => ticket && command.expected === this._lifecycleState(ticket) && this._lifecycleAllowed(ticket, command);
        if (!current()) return result('obsolete');
        if (validateBeforeWrite) {
          if (!await validateBeforeWrite(ticket)) return result('obsolete');
          if (!owned()) return result('cancelled');
          ticket = this.lifecycleResolve(key);
          if (navigation !== this._navigationSeq || !current()) return result('obsolete');
        }
        const expired = scheduleError();
        if (expired) return invalidSchedule(expired);
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
            const opened = await this._paletteOpenTicket(ticket);
            if (!owned()) return result('cancelled');
            if (!opened || this.ticketKey(this.selectedTicket) !== key) return result('refused');
            ticket = this.lifecycleResolve(key);
            if (command.expected !== this._lifecycleState(ticket) || !this._lifecycleAllowed(ticket, command)) return result('obsolete');
          }
          this.openDeleteModal();
          return result('confirmation');
        }
        if (!owned()) return result('cancelled');
        transportStarted = true;
        let outcome;
        const moves = command.kind === 'move' || command.kind === 'cancel';
        if (moves && !batch) {
          outcome = await this._moveTaskTransport(key, command.status);
        } else if (['annotate', 'review'].includes(command.kind)) {
          outcome = await this._startPlannotatorTransport(ticket, command.endpoint, command.label);
        } else {
          // A batch reports acknowledged writes without applying optimistic moves.
          // This also lets Move/Cancel classify post-write server errors like Queue.
          let body = moves ? { status: command.status } : command.kind === 'set-stage' ? { stage: command.stage } : command.body;
          if (command.kind === 'archive') body = { note: command.note || '' };
          outcome = await this._lifecyclePost(key, command.endpoint, body,
            command.kind === 'delete' ? 'DELETE' : 'POST', { reconcileAfterPost, reportError: !batch });
        }
        if (outcome?.outcome === 'uncertain' && reconcileAfterPost) {
          const reconciliationId = ++reconciliationSequence;
          const error = outcome.error ? 'Result unknown: ' + outcome.error : lifecycleReasons.uncertain;
          this._lifecycleUncertain[key] = { id: reconciliationId, expected: command.expected, error, reviewRequired: false };
          const resolution = await this.lifecycleReconcile(key, reconciliationId);
          // The individual Schedule editor still owns its existing inline error.
          const message = !batch && command.kind === 'schedule' && outcome.error
            ? 'Result uncertain; refresh before trying again: ' + outcome.error : error;
          if (!batch) this.error = message;
          return { ...result('uncertain', message), resolution, reconciliationId };
        }
        if (outcome?.outcome === 'success' && !suppressToast && !this._appDestroyed) {
          if (['start', 'queue-saved'].includes(command.kind) || (command.kind === 'initialize' && ticket.kind !== 'epic')) {
            this.showToast(ticket.id + ': ' + (ticket.blockers?.length ? 'Queued' : 'Run requested'));
          } else if (command.kind === 'retry') {
            const message = this.workflowWaiting?.(ticket) ? 'Wait re-arm requested' : 'Retry requested';
            this.showToast(ticket.id + ': ' + message);
          }
        }
        return result(outcome?.outcome || 'failed', outcome?.error);
      } catch (e) {
        if (!transportStarted && !owned()) return result('cancelled');
        if (!batch) this.error = e.message;
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

    async lifecycleRefreshTicket(subject) {
      const previousError = this.error;
      const resolution = await this.lifecycleReconcile(subject);
      if (this._appDestroyed) return;
      if (resolution === 'refresh-required') this.error = 'Could not verify the last request. Refresh this ticket before trying again.';
      else {
        if (this.error === previousError) this.error = null;
        this.showToast('Ticket refreshed');
      }
    },

    // Omit reconciliationId to refresh the current block from outside its sheet.
    // Only 'retryable' lets the original descriptor be reconsidered.
    async lifecycleReconcile(subject, reconciliationId) {
      const key = this.ticketKey(subject);
      if (!this.lifecycleNeedsReconciliation(key)) return 'review-required';
      const pending = this._lifecycleUncertain[key];
      if (reconciliationId !== undefined && reconciliationId !== pending.id) return 'review-required';
      if (this._appDestroyed || this.needsAuth) return 'refresh-required';
      if (reconciliationReads.has(key)) return reconciliationReads.get(key);
      const read = this._lifecycleReadUncertain(key, pending);
      reconciliationReads.set(key, read);
      try { return await read; }
      finally { reconciliationReads.delete(key); }
    },

    async _lifecycleReadUncertain(key, pending) {
      const touched = this._beginSnapshot();
      try {
        const ticket = await this.ticketRead(async signal => {
          const response = await fetch(this.ticketURL(key), { signal, cache: 'no-store' });
          signal.throwIfAborted();
          if (response.status === 401 && !this._appDestroyed) this.ticketReadUnauthorized();
          return response.ok ? response.json() : null;
        });
        if (this._appDestroyed || this.needsAuth || !this._lifecycleAcknowledges(key, ticket, true)) return 'refresh-required';
        const readState = this._lifecycleState(ticket);
        // Preserve contrary evidence from this GET even if its projection cannot
        // be applied. Arrival order cannot distinguish a delayed SSE from a newer update.
        pending.reviewRequired ||= readState !== pending.expected;
        if (touched.has(key)) {
          if (readState !== this._lifecycleState(this.lifecycleResolve(key))) return 'refresh-required';
        } else {
          this.applyTicketUpdate(ticket, { openTerminal: false });
          this.recomputeBoard();
        }
        if (pending.reviewRequired) this._lifecycleReviewEpochs[key] = pending.id;
        delete this._lifecycleUncertain[key];
        return pending.reviewRequired ? 'review-required' : 'retryable';
      } catch (_) { return 'refresh-required'; }
      finally { this._endSnapshot(touched); }
    },

    _lifecycleAcknowledges(key, updated, requireStatus = false) {
      return !!updated?.id && updated.id === this.ticketIdentity(key).id && !this.isSecondary(updated) &&
        (!requireStatus || (typeof updated.status === 'string' && !!updated.status));
    },

    async _lifecyclePost(key, endpoint, body, method = 'POST', { reconcileAfterPost = false, reportError = true } = {}) {
      const touched = this._beginSnapshot();
      try {
        const options = { method };
        if (body !== undefined) { options.headers = { 'Content-Type': 'application/json' }; options.body = JSON.stringify(body); }
        if (method === 'DELETE') options.headers = { 'X-Kontora-Confirm': 'delete-ticket-file' };
        const response = await fetch(this.ticketURL(key, endpoint ? '/' + endpoint : ''), options);
        if (!response.ok) {
          const failure = await response.json().catch(() => null);
          const message = failure?.error || (endpoint || 'Delete') + ' failed';
          // The write and its response are separate operations on the server.
          if (reconcileAfterPost && response.status >= 500) throw new Error(message);
          if (reportError) this.error = message;
          return { outcome: 'failed', error: message };
        }
        const updated = method === 'DELETE' && response.status === 204 ? null : await response.json();
        if (method !== 'DELETE' && !this._lifecycleAcknowledges(key, updated, reconcileAfterPost)) {
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
        if (reconcileAfterPost) return { outcome: 'uncertain', error: e.message };
        const error = 'Result uncertain; refresh before trying again: ' + e.message;
        if (reportError) this.error = error;
        await this._lifecycleRefresh();
        return { outcome: 'uncertain', error };
      } finally { this._endSnapshot(touched); }
    },
  };
}
