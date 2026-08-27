// The phone-width layer, gated on isMobile: a status tab strip and card list
// for the board, a full-screen tabbed detail (terminal / logs / ticket), and
// bottom sheets for actions and the new-ticket form. It reuses the desktop
// board's ticket data and operations — only the layout and a few view-state
// fields (activeColumn, detailTab, sheet) are its own.
import { newCreateForm } from './create.js';

export function kontoraMobile() {
  return {
    // The status column currently shown on the board, clamped to a valid index.
    mobileColumn() {
      var cols = this.columns;
      var i = Math.max(0, Math.min(this.activeColumn, cols.length - 1));
      return cols[i];
    },

    setActiveColumn(i) {
      this.activeColumn = Math.max(0, Math.min(i, this.columns.length - 1));
    },

    // Horizontal swipe on the card list moves between status columns. Mirrors
    // the prototype's threshold: dominant horizontal travel of at least 55px.
    onBoardTouchStart(e) {
      var t = e.changedTouches[0];
      this._swipeX = t.clientX;
      this._swipeY = t.clientY;
    },
    onBoardTouchEnd(e) {
      if (this.selectedTicket || this.sheet) return;
      var t = e.changedTouches[0];
      var dx = t.clientX - (this._swipeX || 0);
      var dy = t.clientY - (this._swipeY || 0);
      if (Math.abs(dx) > 55 && Math.abs(dx) > Math.abs(dy) * 1.4) {
        this.setActiveColumn(this.activeColumn + (dx < 0 ? 1 : -1));
      }
    },

    // A ticket "has a run" once it leaves the draft/queued states; that's when
    // the terminal/logs tabs and the detail tab bar appear.
    mobileHasRun(t) {
      return !!t && !['open', 'todo'].includes(t.status);
    },
    // The first (non-ticket) detail tab: a live terminal while running, the
    // stage-log history otherwise.
    mobileFirstTab() {
      return this.selectedTicket && this.selectedTicket.status === 'in_progress' ? 'terminal' : 'logs';
    },
    mobileFirstTabLabel() {
      return this.selectedTicket && this.selectedTicket.status === 'in_progress' ? '›_ terminal' : '≡ logs';
    },

    async openMobileTicket(t) {
      // Pick the detail tab from the card's status before selectTicket() runs,
      // so the right pane is already visible when the live terminal attaches.
      if (t.status === 'in_progress') this.detailTab = 'terminal';
      else if (this.mobileHasRun(t)) this.detailTab = 'logs';
      else this.detailTab = 'ticket';
      await this.selectTicket(t);
      // The board card lacks detail fields, so the summary check runs on the
      // full ticket selectTicket fetched.
      if (this.detailTab === 'logs' && this.summaryFirst(this.selectedTicket)) {
        this.detailTab = 'ticket';
      }
      // selectTicket() applies the desktop tab heuristics, which only fetch logs
      // when the ticket carries history. The mobile logs tab needs them either
      // way, so load them here if nothing started (logViewLoading is set
      // synchronously when selectTicket did kick off a fetch).
      if (this.detailTab === 'logs' && this.logViewContent === null && !this.logViewLoading) {
        this.mobileSwitchTab('logs');
      }
    },

    mobileSwitchTab(tab) {
      if (!this.selectedTicket) return;
      this.detailTab = tab;
      if (tab === 'terminal') {
        this.logViewContent = null;
        this.logViewStage = null;
        if (this.selectedTicket.status === 'in_progress') this.openTerminal();
      } else {
        if (this.terminalOpen) this.closeTerminal();
        if (tab === 'logs') {
          var t = this.selectedTicket;
          var stage = t.stage;
          if (!stage && t.history && t.history.length) stage = t.history[t.history.length - 1].stage;
          this.fetchStageLogs(t.id, stage);
        }
      }
    },

    // Inline style for one chip in the logs-tab stage strip.
    mobileStageStyle(stage) {
      var base = "font-family:'JetBrains Mono',monospace;font-size:12px;padding:4px 10px;border-radius:6px;white-space:nowrap;flex:none;";
      var t = this.selectedTicket;
      var stages = (t && t.stages) || [];
      var cur = stages.indexOf(t && t.stage);
      var i = stages.indexOf(stage);
      if (i === cur) return base + 'background:rgba(var(--surface-800),1);color:rgba(var(--tx),1);border:1px solid rgba(var(--surface-700),1);';
      if (cur >= 0 && i < cur) return base + 'color:rgba(var(--surface-600),1);';
      return base + 'color:rgba(var(--surface-700),1);';
    },

    // Status-aware action-bar buttons. The primary performs the main transition
    // directly, the secondary the alternate one; unknown statuses fall back to
    // the actions sheet.
    mobilePrimaryAction(t) {
      var self = this, id = t && t.id;
      var run = function(ep) { return function() { self.moveTicketVia(id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(id, 'move', { status: st }); }; };
      var map = {
        open:         { label: 'queue',          run: run('run') },
        todo:         { label: 'queue',          run: run('run') },
        in_progress:  { label: 'send to review', run: move('human_review') },
        paused:       { label: 'resume',         run: run('retry') },
        human_review: { label: 'approve',        run: move('done') },
        done:         { label: 're-open',        run: run('retry') },
        cancelled:    { label: 're-open',        run: run('retry') },
      };
      return map[t && t.status] || { label: 'actions', run: function() { self.openActionsSheet(t); } };
    },
    mobileSecondaryAction(t) {
      var self = this, id = t && t.id;
      var run = function(ep) { return function() { self.moveTicketVia(id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(id, 'move', { status: st }); }; };
      var map = {
        open:         { label: 'cancel',    run: move('cancelled') },
        todo:         { label: 'cancel',    run: move('cancelled') },
        in_progress:  { label: 'pause',     run: function() { self.action('pause'); } },
        paused:       { label: 'cancel',    run: move('cancelled') },
        human_review: { label: 'send back', run: run('retry') },
        done:         { label: 'delete',    run: function() { self.openDeleteModal(); } },
        cancelled:    { label: 'delete',    run: function() { self.openDeleteModal(); } },
      };
      return map[t && t.status] || { label: 'more', run: function() { self.openActionsSheet(t); } };
    },

    openActionsSheet(t) {
      if (!t) return;
      this.sheet = { type: 'actions', ticket: t };
    },
    async openNewSheet() {
      this.createForm = newCreateForm();
      this.createTouched = { pipeline: false, agent: false };
      this.error = null;
      this.sheet = { type: 'new' };
      if (!this.configCache) {
        try {
          var res = await fetch('/api/config');
          if (res.ok) this.configCache = await res.json();
        } catch (e) { /* form still works with empty pipeline/agent lists */ }
      }
    },
    closeSheet() {
      this.sheet = null;
    },

    async submitCreateTicketMobile() {
      this.error = null;
      await this.submitCreateTicket();
      if (!this.error) this.closeSheet();
    },

    // Action rows for the actions sheet, status by status. Each row closes the
    // sheet, then runs against a real endpoint (or opens the relevant modal).
    // The phone's schedule editor. It is a sheet rather than an inline row: the
    // detail overlay has no spare width for a date field and its buttons.
    openScheduleSheet(t) {
      this.openScheduleEditor(t);
      this.sheet = { type: 'schedule', ticket: t };
    },

    async submitScheduleMobile(t) {
      await this.submitSchedule(t);
      if (!this.scheduleError) this.closeSheet();
    },

    // The actions sheet closes before the clear is posted, and scheduleError
    // only renders in the detail rail and the schedule sheet, neither of which
    // is on screen. The toast is the surface a phone has left.
    async clearScheduleMobile(t) {
      await this.clearTicketSchedule(t);
      if (this.scheduleError) this.error = this.scheduleError;
    },

    mobileSheetActions(t) {
      if (!t) return [];
      var self = this, rows = [];
      var add = function(label, kind, fn) { rows.push({ label: label, kind: kind, run: function() { self.closeSheet(); fn(); } }); };
      var run = function(ep) { return function() { self.moveTicketVia(t.id, ep, null); }; };
      var move = function(st) { return function() { self.moveTicketVia(t.id, 'move', { status: st }); }; };
      var s = t.status;
      if ((s === 'open' || s === 'todo') && !t.kontora) add('Initialize ticket', 'warn', function() { self.openInitModal(t); });
      // On a scheduled ticket "Queue agent" is the run-now action: the daemon
      // drops the timestamp in the same save.
      if (this.canSchedule(t)) {
        add(t.scheduled_at ? 'Reschedule' : 'Schedule for later', 'default', function() { self.openScheduleSheet(t); });
        if (t.scheduled_at) add('Clear schedule', 'default', function() { self.clearScheduleMobile(t); });
      }
      if (s === 'open') {
        add('Queue agent', 'primary', run('run'));
        add('Move to Human Review', 'default', move('human_review'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'todo') {
        add('Queue agent', 'primary', run('run'));
        add('Move to open', 'default', move('open'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'in_progress') {
        add('Pause agent', 'warn', function() { self.action('pause'); });
        add('Skip stage', 'default', function() { self.action('skip'); });
        add('Send to review', 'primary', move('human_review'));
        add('Mark done', 'default', move('done'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'paused') {
        add('Resume', 'primary', run('retry'));
        add('Mark done', 'default', move('done'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'human_review') {
        if (t.kontora) add('Plannotator review', 'default', function() { self.startPlannotatorReview(t); });
        add('Approve & merge', 'primary', move('done'));
        add('Send back', 'warn', run('retry'));
        add('Cancel ticket', 'danger', move('cancelled'));
      } else if (s === 'done') {
        add('Re-open', 'default', run('retry'));
        add('Send to review', 'default', move('human_review'));
        if (t.branch) add('Copy branch', 'default', function() { self.copyBranch(t.branch); });
        add('Delete file', 'danger', function() { self.openDeleteModal(); });
      } else if (s === 'cancelled') {
        add('Re-open', 'default', run('retry'));
        add('Delete file', 'danger', function() { self.openDeleteModal(); });
      } else {
        (this.validMoves[s] || []).forEach(function(mv) {
          add(mv.label, 'default', mv.status ? move(mv.status) : run(mv.endpoint));
        });
      }
      return rows;
    },
    mobileActionColor(kind) {
      if (kind === 'danger') return 'rgba(var(--err),1)';
      if (kind === 'warn') return 'hsl(var(--st-paused))';
      if (kind === 'primary') return 'rgba(var(--accent),1)';
      return 'rgba(var(--tx-2),1)';
    },
    mobileActionDotColor(kind) {
      if (kind === 'danger') return 'rgba(var(--err),1)';
      if (kind === 'warn') return 'hsl(var(--st-paused))';
      if (kind === 'primary') return 'rgba(var(--accent),1)';
      return 'rgba(var(--surface-600),1)';
    },
  };
}
