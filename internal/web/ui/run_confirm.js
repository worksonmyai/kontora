const runKinds = {
  annotation: 'Apply Plannotator annotations',
  rework: 'Plannotator rework',
  graph: 'Workflow graph',
  check: 'Workflow graph · check (no agent)',
  wait: 'Workflow graph · wait',
  end: 'Workflow graph · finish without an agent',
};

function runReviewKey(ticket) {
  return JSON.stringify([ticket.run_preview, ticket.pipeline || '', ticket.stage || '',
    ticket.path || '', ticket.branch || '', ticket.base_branch || '',
    ticket.agent_override ? ticket.agent : '', [...(ticket.blockers || [])].sort()]);
}

export function kontoraRunConfirm() {
  let previewSequence = 0;
  return {
    runConfirmOpen: false,
    runConfirmLoading: false,
    runConfirmError: '',
    runConfirmPreview: null,

    async _readRunPreview(ticket) {
      return this.ticketRead(signal => this.readTicketJSON(this.ticketURL(ticket), signal, 'Could not read the saved run settings.'));
    },

    async openRunConfirm(ticket) {
      if (!this.lifecycleCanQueueSaved(ticket)) return this.dispatchLifecycle(ticket, 'start');
      const key = this.ticketKey(ticket);
      const seq = ++previewSequence;
      const origin = globalThis.document?.activeElement;
      const current = () => this.runConfirmOpen && seq === previewSequence && !this._appDestroyed && this.ticketKey(this.selectedTicket) === key;
      this.runConfirmOpen = true;
      this.runConfirmLoading = true;
      this.runConfirmError = '';
      this.runConfirmPreview = null;
      try {
        if (!await this.lifecycleDraftBarrier(key)) throw new Error('Save the latest ticket changes before reviewing the run.');
        if (!current()) return;
        const fresh = await this._readRunPreview(ticket);
        if (!current()) return;
        if (!this.sameTicket(fresh, ticket) || !this._lifecycleCanQueueSaved(fresh)) throw new Error('The ticket changed. Close this dialog and try again.');
        if (!fresh.run_preview) throw new Error('The daemon did not return a run preview.');
        this.runConfirmPreview = {
          ...fresh.run_preview,
          command: this.lifecycleCommand(key, 'queue-saved'),
          reviewKey: runReviewKey(fresh),
          blocker: this.ticketBlocker(fresh),
        };
      } catch (e) {
        if (current()) this.runConfirmError = e.message;
      } finally {
        if (current()) {
          this.runConfirmLoading = false;
          await this.$nextTick();
          if (current() && globalThis.document?.activeElement === origin) {
            const target = this.runConfirmError || this.runConfirmPreview?.error ? this.$refs.runConfirmCancel : this.$refs.runConfirmPrimary;
            target?.focus({ preventScroll: true });
          }
        }
      }
    },

    closeRunConfirm(restoreFocus) {
      if (!this.runConfirmOpen) return;
      ++previewSequence;
      this.runConfirmOpen = false;
      this.runConfirmLoading = false;
      this.runConfirmPreview = null;
      this.runConfirmError = '';
      if (restoreFocus) this.$refs.runConfirmTrigger?.focus({ preventScroll: true });
    },

    ticketBlocker(ticket) {
      const id = ticket?.blockers?.[0];
      if (!id) return null;
      const holder = this.sameTicket(ticket, this.selectedTicket) && this.upstreamHolder();
      if (holder) return { id: holder.id, status: holder.status, missing: !!holder.missing, ref: holder };
      // Blocker IDs are unqualified. Use the resolved dependency before looking on the board.
      const dep = ticket.deps?.find(d => d.id === id);
      const source = dep?.source || ticket.source || 'primary';
      const ref = dep?.status || dep?.missing ? dep : this.sourceTicketById(id, { source });
      return { id, status: ref?.status, missing: !!ref?.missing,
        ref: { id, source, title: ref?.title, status: ref?.status, missing: !!ref?.missing } };
    },

    queueReason(ticket) {
      if (ticket?.status !== 'todo' || this.executionActive(ticket)) return null;
      const blocker = this.ticketBlocker(ticket);
      return blocker ? { kind: 'blocked', ...blocker } : { kind: 'queued' };
    },

    runConfirmView() {
      const preview = this.runConfirmPreview;
      const blocker = preview?.blocker;
      const move = blocker && this.chainResumeMove();
      const error = this.runConfirmError || preview?.error || '';
      return {
        stages: preview?.stages || [],
        agent: preview?.agent || '',
        branch: preview?.branch || '',
        repo: preview?.repo || '',
        kind: preview && !error ? runKinds[preview.kind] || (preview.stages?.length ? '' : 'Single run, no stages') : '',
        error,
        blocker: blocker ? {
          id: blocker.id,
          body: blocker.missing
            ? 'Couldn’t resolve ticket ' + blocker.id + '. Check the ID and configured ticket folders.'
            : 'The ticket is blocked. Queueing it does not start work.',
          action: move && this.sameTicket(move.ref, blocker.ref)
            ? move.label.startsWith('Rearm wait') ? move.label : 'Run ' + blocker.id + ' first'
            : '',
        } : null,
      };
    },

    async confirmRun() {
      const preview = this.runConfirmPreview;
      if (!this.runConfirmOpen || !preview || this.runConfirmView().error) return;
      const ticket = this.selectedTicket;
      const seq = previewSequence;
      const result = await this.dispatchLifecycle(ticket, preview.command, {
        validateBeforeWrite: async current => {
          const fresh = await this._readRunPreview(current);
          return seq === previewSequence && this.sameTicket(fresh, current) &&
            this._lifecycleCanQueueSaved(fresh) && runReviewKey(fresh) === preview.reviewKey &&
            this.sameRunBlocker(preview.blocker, this.ticketBlocker(fresh));
        },
      });
      if (seq !== previewSequence || !this.runConfirmOpen) return result;
      if (result.outcome === 'success') this.closeRunConfirm(true);
      else {
        this.runConfirmError = result.error || 'Run was not requested. Close this dialog and try again.';
        await this.$nextTick();
        if (seq === previewSequence && this.runConfirmOpen) this.$refs.runConfirmCancel?.focus({ preventScroll: true });
      }
      return result;
    },

    sameRunBlocker(a, b) {
      return !a && !b || !!a && !!b && this.sameTicket(a.ref, b.ref) && a.missing === b.missing;
    },

    runBlockerFirst() {
      if (!this.runConfirmOpen || !this.runConfirmView().blocker?.action) return;
      const move = this.chainResumeMove();
      const origin = this.$refs.runConfirmTrigger;
      this.closeRunConfirm(true);
      return this.dispatchWithDialogOrigin('init', this.ticketKey(move.ref), origin, () =>
        this.lifecycleCanQueueSaved(move.ref)
          ? this.dispatchLifecycle(move.ref, 'queue-saved')
          : this.dispatchLifecycle(move.ref, this.lifecycleFromEndpoint(move.ref, move.endpoint)));
    },
  };
}
