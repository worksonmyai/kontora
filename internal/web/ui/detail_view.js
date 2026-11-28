import { blockingInteraction, canReturnInteractionFocus, focusAvailable, mobileDetailCovered, syncInteractionCoverage } from './dialogs.js';

const mobileDetailSelector = '.mobile-detail, .mobile-epic';

export function kontoraDetailView() {
  const views = new Map();
  const noteScrolls = new WeakMap();
  let returnFocus = null;
  let scrollSequence = 0;
  let cancelScrollRestore = null;

  return {
    detailMode(mobile = this.isMobile) {
      return mobile ? this.detailTab : this.activeTab === 'session' ? 'terminal'
        : this.activeTab === 'summary' ? 'ticket' : this.activeTab;
    },

    supportedDetailMode(mode, ticket = this.selectedTicket, mobile = this.isMobile) {
      if (mode === 'workflow') return !mobile && this.showWorkflowGraph(ticket) ? mode : 'ticket';
      if (this.isSecondary(ticket)) {
        if (!mobile && !['ticket', 'notes'].includes(mode)) return 'ticket';
        if (['terminal', 'logs'].includes(mode)) return 'activity';
      }
      if (mode === 'terminal' && !this.executionActive(ticket)) mode = this.mobileHasRun(ticket) ? 'activity' : 'ticket';
      if (['activity', 'logs'].includes(mode) && !this.isSecondary(ticket) && !this.mobileHasRun(ticket)) mode = 'ticket';
      return !mobile && mode === 'logs' ? 'activity' : mode;
    },

    detailViewRoot(mobile = this.isMobile) {
      return document.querySelector(mobile ? '.mobile-detail' : '[data-ticket-detail]');
    },

    detailViewScrollers(mobile = this.isMobile) {
      const root = this.detailViewRoot(mobile);
      if (!root) return [];
      return [...root.querySelectorAll('[data-detail-scroll], .overflow-y-auto')]
        .filter(el => el.getClientRects().length && getComputedStyle(el).visibility !== 'hidden');
    },

    rememberDetailView(mobile = this.isMobile) {
      const ticket = this.selectedTicket;
      if (!ticket || this.detailLoading) return;
      const key = this.ticketKey(ticket);
      const previous = views.get(key) || { scroll: {} };
      const mode = this.detailMode(mobile);
      previous.scroll[mode] = new Map(this.detailViewScrollers(mobile).map((el, i) =>
        [el.dataset?.detailScroll || i, el.scrollTop]));
      views.set(key, { ...previous, mode, stage: this.activityStage, run: this.activityRun,
        explicit: this._activityExplicit, logStage: this.logViewStage, follow: this.logFollow });
      if (views.size > 100) views.delete(views.keys().next().value);
    },

    detailNotesShouldFollow(el) {
      if (this.detailMode() !== 'notes' || !this.selectedTicket || this.detailLoading) return false;
      const key = this.ticketKey(this.selectedTicket);
      const revision = JSON.stringify(this.selectedTicket.notes || []);
      const previous = noteScrolls.get(el);
      noteScrolls.set(el, { key, revision });
      return previous?.key === key ? previous.revision !== revision : !views.get(key)?.scroll.notes;
    },

    restoreDetailScroll() {
      cancelScrollRestore?.();
      const sequence = ++scrollSequence;
      const key = this.ticketKey(this.selectedTicket);
      const mode = this.detailMode();
      const mobile = this.isMobile;
      const detailSequence = this._ticketDetailSeq;
      const positions = views.get(key)?.scroll[mode];
      if (!positions) return;
      let stop = null;
      let finished = false;
      const finish = () => {
        finished = true;
        stop?.();
        if (cancelScrollRestore === finish) cancelScrollRestore = null;
      };
      cancelScrollRestore = finish;
      const pending = () => mode === 'activity' ? this.activityLoading
        : mode === 'diff' ? this.ticketChangesLoading : mode === 'logs' ? this.logViewLoading : false;
      const restore = () => this.$nextTick(() => requestAnimationFrame(() => {
        if (finished) return;
        if (sequence !== scrollSequence || this._appDestroyed || this.currentView !== 'board' ||
            this.ticketKey(this.selectedTicket) !== key || this.detailMode() !== mode ||
            this.isMobile !== mobile || this._ticketDetailSeq !== detailSequence) {
          finish();
          return;
        }
        // Loading placeholders cannot hold the saved offset. Wait for the read
        // and its Alpine render before writing scrollTop.
        if (pending()) return;
        finish();
        this.detailViewScrollers().forEach((el, i) => { el.scrollTop = positions.get(el.dataset?.detailScroll || i) ?? 0; });
      }));
      // Alpine.watch returns a disposer; $watch only cleans up when the component is destroyed.
      stop = Alpine.watch(() => JSON.stringify([
        this.ticketKey(this.selectedTicket), this.detailMode(), this.isMobile,
        this._ticketDetailSeq, this.currentView, this._appDestroyed, pending(),
      ]), restore);
      restore();
    },

    async restoreDetailView() {
      const ticket = this.selectedTicket;
      const key = this.ticketKey(ticket);
      const saved = views.get(key);
      if (!saved || !ticket || this.detailLoading) return false;
      const mode = this.supportedDetailMode(saved.mode, ticket);
      const history = ticket.history || [];
      const sameStage = history.filter(h => h.stage === saved.stage);
      const live = this.runningRun();
      const available = saved.stage !== null && (sameStage[saved.run] || (live?.stage === saved.stage && live.run === saved.run));
      if (available) {
        this.activityStage = saved.stage;
        this.activityRun = saved.run;
        this._activityExplicit = saved.explicit;
      }
      this.logFollow = saved.follow;
      if (this.isMobile) this.mobileSwitchTab(mode, { logStage: saved.logStage });
      else {
        this.activeTab = mode === 'terminal' ? 'session' : mode;
        if (mode === 'activity' && available) this.openActivity(saved.stage, saved.run);
        else this.switchTab(this.activeTab);
      }
      if (mode === 'ticket' && !this.editing && this.editorCanEdit(ticket)) await this.startEditing();
      if (!this.sameTicket(this.selectedTicket, ticket)) return true;
      this.restoreDetailScroll();
      this.syncDetailCoverage();
      return true;
    },

    focusMobileDetail() {
      if (this._appDestroyed || !mobileDetailCovered(this) || blockingInteraction(this)) return;
      const root = this.selectedEpic ? document.querySelector('.mobile-epic') : this.detailViewRoot();
      if (!root || root.contains(document.activeElement)) return;
      const target = [...root.querySelectorAll('button')].find(focusAvailable) || root;
      if (focusAvailable(target)) target.focus({ preventScroll: true });
    },

    syncDetailCoverage() {
      if (typeof document === 'undefined' || this._appDestroyed) return;
      const covered = mobileDetailCovered(this);
      if (covered && !returnFocus && !blockingInteraction(this)) {
        const active = document.activeElement;
        if (!active?.closest(mobileDetailSelector)) {
          returnFocus = active;
          const key = this.ticketKey(this.selectedTicket || this.selectedEpic);
          this.$nextTick(() => {
            if (this.ticketKey(this.selectedTicket || this.selectedEpic) === key) this.focusMobileDetail();
          });
        }
      } else if (!covered) {
        const target = returnFocus;
        returnFocus = null;
        if (this.restoreMobileList?.() || !target) {
          syncInteractionCoverage(this);
          return;
        }
        this.$nextTick(() => {
          if (!mobileDetailCovered(this) && canReturnInteractionFocus(this, target)) target.focus({ preventScroll: true });
        });
      }
      syncInteractionCoverage(this);
    },
  };
}
