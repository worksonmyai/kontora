import { runSeconds } from './format.js';
import { statsCompact } from './stats.js';

// Tape events the activity transcript renders at once, counted back from the
// newest. A whole 5000-event tape is 60,000 elements: 660ms to mount and 135ms
// to tear down, both on the main thread. 200 events cost 27ms and 9ms. The
// reader starts at the bottom, so the rest of the tape can wait for a click.
export const TAPE_WINDOW_SIZE = 200;

// How often the activity tab re-reads a running stage's transcript. Fast enough
// that a tool call and its result land while the reader is still on the row,
// slow enough that a poll costs one stat on the daemon when nothing changed.
const ACTIVITY_POLL_MS = 2000;

// The activity tab: the transcript tape, its poller, and the stage ribbon.
export function kontoraActivity() {
  return {
    // The newest run the transcript can show from history. Every history entry
    // describes a finished run, so the last one is the most recently completed
    // stage.
    latestCompletedRun() {
      var h = (this.selectedTicket && this.selectedTicket.history) || [];
      if (!h.length) return null;
      var last = h[h.length - 1];
      return { stage: last.stage, run: last.run || 0 };
    },

    // The run in flight, or null when nothing is running. The run number counts
    // that stage's history rows, as the daemon does: a history row is written
    // when a run ends, so the running one is the next index.
    runningRun() {
      var t = this.selectedTicket;
      if (!t || t.status !== 'in_progress' || !t.stage) return null;
      var run = 0;
      (t.history || []).forEach(function (h) { if (h.stage === t.stage) run++; });
      return { stage: t.stage, run: run };
    },

    // The run the activity tab shows: the one in flight, else the last finished.
    activityTarget() {
      return this.runningRun() || this.latestCompletedRun();
    },

    _resetActivity() {
      this._stopActivityPoll();
      this.activity = null;
      this.activityStage = null;
      this.activityRun = 0;
      this.activityLoading = false;
      this.activityError = null;
      this.expandedTools = {};
      this.tapeWindow = TAPE_WINDOW_SIZE;
    },

    _stopActivityPoll() {
      if (this._activityPoll !== null) clearTimeout(this._activityPoll);
      this._activityPoll = null;
      this._activityETag = null;
      this._activityFailures = 0;
    },

    // Load one run's transcript, replacing everything the pane holds.
    async fetchActivity(stage, run) {
      if (!this.selectedTicket) return;
      this._stopActivityPoll();
      this.activityStage = stage || '';
      this.activityRun = run || 0;
      this.activityLoading = true;
      this.activityError = null;
      this.activity = null;
      this.expandedTools = {};
      this.tapeWindow = TAPE_WINDOW_SIZE;
      await this._loadActivity(stage, run, { merge: false });
      this.activityLoading = false;
    },

    // Fetch one activity payload. merge=true is the polling path: only the
    // payload is replaced, so the expand map, the grown window and the scroll
    // offset all survive the tick.
    async _loadActivity(stage, run, { merge = false } = {}) {
      if (!this.selectedTicket) return;
      var id = this.selectedTicket.id;
      var seq = ++this._activityLoadSeq;
      var url = '/api/tickets/' + encodeURIComponent(id) + '/activity'
        + '?stage=' + encodeURIComponent(stage || '') + '&run=' + (run || 0);
      var headers = {};
      if (merge) {
        url += '&after=' + this.tapeEvents().length;
        if (this._activityETag) headers['If-None-Match'] = this._activityETag;
      }
      var data = null;
      var res = null;
      try {
        res = await fetch(url, { headers: headers });
        // A ribbon click or a ticket switch while this was in flight makes the
        // answer describe a run nobody is looking at.
        if (!this._activityCurrent(id, stage, run, seq)) return;
        if (res.status === 304) {
          this._activityFailures = 0;
          this._armActivityPoll(stage, run, true);
          return;
        }
        if (!res.ok) {
          var err = await res.json().catch(function () { return {}; });
          this._activityFailed(merge, err.error || 'Failed to load activity');
          this._armActivityPoll(stage, run, merge);
          return;
        }
        data = await res.json();
      } catch (e) {
        if (!this._activityCurrent(id, stage, run, seq)) return;
        this._activityFailed(merge, 'Failed to load activity');
        this._armActivityPoll(stage, run, merge);
        return;
      }
      if (!this._activityCurrent(id, stage, run, seq)) return;

      this._activityFailures = 0;
      this._activityETag = res.headers.get('ETag');
      if (merge) {
        this._mergeActivity(data);
      } else {
        this.activity = data;
        this.activityError = null;
      }
      this._armActivityPoll(stage, run, !!data.live);
    },

    // Whether a response still describes what the pane is showing. seq drops an
    // answer a later load has already superseded: the run ending starts a read
    // of the finished transcript while a poll is still in flight, and merging
    // that poll's partial tape afterwards would cut the transcript back down.
    _activityCurrent(id, stage, run, seq) {
      return seq === this._activityLoadSeq
        && !!this.selectedTicket && this.selectedTicket.id === id
        && this.activityStage === (stage || '') && this.activityRun === (run || 0);
    },

    // A poll that fails keeps the transcript on screen: a single dropped request
    // must not replace a good tape with an error. Three in a row is a daemon the
    // reader needs to know about.
    _activityFailed(merge, message) {
      if (!merge) {
        this.activityError = message;
        return;
      }
      this._activityFailures++;
      if (this._activityFailures >= 3) this.activityError = message;
    },

    // Splice the new suffix onto the events already held. The server sends its
    // own cursor, which can walk back over tool rows whose results arrived late.
    _mergeActivity(data) {
      // A run with no structured record tails the plaintext log, which carries
      // no cursor: the payload is whole every time.
      if (!data.tape) {
        this.activity = data;
        return;
      }
      var existing = this.tapeEvents();
      var offset = data.offset || 0;
      var events = existing.slice(0, offset).concat((data.tape && data.tape.events) || []);
      var added = events.length - existing.length;
      // A new object rather than a field write: the autoscroll effect watches
      // the activity property itself.
      this.activity = Object.assign({}, data, { tape: Object.assign({}, data.tape, { events: events }) });
      // With follow off the reader is holding a position, so the window grows by
      // what arrived and no row slides off the top. With follow on the window
      // stays put and the effect pins the view to the newest event.
      if (!this.logFollow && added > 0) this.tapeWindow += added;
    },

    // Re-arm the two-second poll while the run is live and its pane is visible.
    // A chained timeout, not an interval: it cannot stack requests on a slow
    // daemon. Once the run ends the payload stops saying live, nothing is armed,
    // and the completed transcript is what stays on screen.
    _armActivityPoll(stage, run, live) {
      if (this._activityPoll !== null) {
        clearTimeout(this._activityPoll);
        this._activityPoll = null;
      }
      if (!live || this.activeTab !== 'activity' || !this.selectedTicket) return;
      var self = this;
      this._activityPoll = setTimeout(function () {
        self._activityPoll = null;
        self._loadActivity(stage, run, { merge: true });
      }, ACTIVITY_POLL_MS);
    },

    // Show the transcript for one run and bring the activity tab forward.
    openActivity(stage, run) {
      this.activeTab = 'activity';
      this.fetchActivity(stage, run);
    },

    // Whether this run's session file left the dimension unfilled. The view
    // hides the affordance rather than showing a zero or the wrong colour.
    tapePartial(dim) {
      var p = this.activity && this.activity.tape && this.activity.tape.partial;
      return !!p && p.indexOf(dim) >= 0;
    },

    tapeEvents() {
      return (this.activity && this.activity.tape && this.activity.tape.events) || [];
    },

    // The rendered tail of the tape: the newest tapeWindow events, each paired
    // with its position in the full events array. Row identity (toolKey) and the
    // x-for key both read that position, so loading earlier events prepends rows
    // without renaming the ones already on screen.
    visibleTapeEvents() {
      var events = this.tapeEvents();
      var start = Math.max(0, events.length - this.tapeWindow);
      var out = [];
      for (var i = start; i < events.length; i++) out.push({ ev: events[i], idx: i });
      return out;
    },

    hiddenTapeEventCount() {
      return Math.max(0, this.tapeEvents().length - this.tapeWindow);
    },

    // Grow the window by one step. The rows appear above the viewport, so the
    // scroll offset moves down by the height they add; without that the reader
    // is thrown back to older events.
    loadEarlierTapeEvents() {
      var el = document.getElementById('activity-scroll');
      var height = el ? el.scrollHeight : 0;
      var top = el ? el.scrollTop : 0;
      this.tapeWindow = Math.min(this.tapeEvents().length, this.tapeWindow + TAPE_WINDOW_SIZE);
      this.$nextTick(function () {
        if (el) el.scrollTop = top + (el.scrollHeight - height);
      });
    },

    // Row identity for the expand map. Pi tool calls carry no id, so the index
    // stands in.
    toolKey(ev, i) {
      return ev.id || ('i' + i);
    },

    // A failure is expanded on first render: it is why the reader opened the
    // tape. Everything else starts collapsed.
    toolExpanded(ev, i) {
      if (ev.is_error && !this.tapePartial('is_error')) return true;
      return !!this.expandedTools[this.toolKey(ev, i)];
    },

    toolFailed(ev) {
      return !!ev.is_error && !this.tapePartial('is_error');
    },

    toggleTool(ev, i) {
      var k = this.toolKey(ev, i);
      this.expandedTools[k] = !this.expandedTools[k];
    },

    eventTime(ev) {
      if (this.tapePartial('time') || !ev || !ev.time) return '';
      var d = new Date(ev.time);
      if (isNaN(d.getTime())) return '';
      var pad = function (n) { return String(n).padStart(2, '0'); };
      return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
    },

    // Total tokens over the four categories the session record reports, or ''
    // when usage is unverified for this agent. There is no monetary figure:
    // neither the session record nor the config carries a price.
    tapeTokens(tape) {
      if (!tape || (tape.partial || []).indexOf('usage') >= 0) return '';
      var t = tape.totals || {};
      var n = (t.input || 0) + (t.output || 0) + (t.cache_create || 0) + (t.cache_read || 0);
      if (!n) return '';
      // The same shortening Stats uses, so one run's tokens read the same in
      // both places.
      return statsCompact(n);
    },

    // ---- stage ribbon ------------------------------------------------------

    // One segment per pipeline stage, sized by the summed duration of its runs.
    // Gaps between runs are queue time and do not count toward a stage.
    stageRibbon() {
      var t = this.selectedTicket;
      if (!t) return [];
      var self = this;
      var agg = Object.create(null);
      (t.history || []).forEach(function (h) {
        var e = agg[h.stage] || (agg[h.stage] = { seconds: 0, runs: 0, last: null });
        e.runs++;
        e.last = h;
        e.seconds += runSeconds(h);
      });

      var stages = t.stages || [];
      var currentIdx = stages.indexOf(t.stage);
      return stages.map(function (name, i) {
        var a = agg[name] || { seconds: 0, runs: 0, last: null };
        var running = t.status === 'in_progress' && name === t.stage;
        var done = !running && (t.status === 'done' || a.runs > 0 || (currentIdx >= 0 && i < currentIdx));
        var seconds = a.seconds;
        if (running && t.started_at) {
          var live = Math.floor((self.now - new Date(t.started_at)) / 1000);
          if (live > 0) seconds += live;
        }
        return {
          name: name,
          runs: a.runs,
          run: a.last ? (a.last.run || 0) : 0,
          seconds: seconds,
          state: running ? 'running' : (done ? 'done' : 'queued'),
          meta: running || done ? self.formatSeconds(seconds) : 'not started',
        };
      });
    },

    // "45s" / "2m 04s" / "1h 12m" — the ribbon's per-stage meta line.
    formatSeconds(secs) {
      if (typeof secs !== 'number' || isNaN(secs) || secs < 0) return '';
      if (secs < 60) return secs + 's';
      var m = Math.floor(secs / 60);
      if (m < 60) return m + 'm ' + String(secs % 60).padStart(2, '0') + 's';
      return Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
    },

    // Clicking a segment: the running stage returns to the live session, a
    // finished one loads its transcript, a queued one does nothing.
    clickRibbon(seg) {
      if (seg.state === 'running') {
        this.switchTab('session');
        return;
      }
      if (seg.state !== 'done' || !seg.runs) return;
      this.openActivity(seg.name, seg.run);
    },

    // Attempt denominator: the initial attempt plus the stage's max_retries.
    stageMaxAttempts() {
      var t = this.selectedTicket;
      if (!t || !t.pipeline || !t.stage) return 0;
      var infos = (this.configCache && this.configCache.pipeline_infos) || [];
      for (var i = 0; i < infos.length; i++) {
        if (infos[i].name !== t.pipeline) continue;
        var idx = (infos[i].stages || []).indexOf(t.stage);
        if (idx < 0) return 0;
        return ((infos[i].max_retries || [])[idx] || 0) + 1;
      }
      return 0;
    },

    // Wall time across the whole ticket: the first stage's pickup to the last
    // recorded exit, queue gaps included. The frontmatter's started_at is
    // rewritten at every stage spawn, so it holds the current stage's pickup
    // and would report only the last stage's duration.
    ticketWall() {
      var t = this.selectedTicket;
      if (!t) return '';
      var h = t.history || [];
      var start = h.length ? (h[0].started_at || t.started_at) : t.started_at;
      var end = h.length ? h[h.length - 1].completed_at : t.updated_at;
      return this.formatElapsed(start, end);
    },

    // Meters beside the ribbon. A meter with no verified data is left out
    // rather than rendered as a zero.
    ribbonMeters() {
      var t = this.selectedTicket;
      if (!t) return [];
      var out = [];
      var tokens = this.tapeTokens(this.activity && this.activity.tape);
      if (t.status === 'in_progress') {
        if (t.started_at) out.push({ k: 'elapsed', v: this.formatDuration(t) });
        if (tokens) out.push({ k: 'tokens', v: tokens });
        var max = this.stageMaxAttempts();
        var n = (t.attempt || 0) + 1;
        out.push({ k: 'attempt', v: max ? n + ' / ' + max : String(n) });
      } else if (t.status === 'human_review') {
        var wall = this.ticketWall();
        if (wall) out.push({ k: 'wall', v: wall });
        if (tokens) out.push({ k: 'tokens', v: tokens });
      }
      return out;
    },

    // ---- changed files -----------------------------------------------------

    // Bounded churn summary for the rail: the totals, the stacked bar's split,
    // and the three files with the most change. Equal churn breaks on path so
    // repeated renders agree on the same three.
    churn() {
      var files = (this.ticketChanges && this.ticketChanges.files) || [];
      var added = 0;
      var deleted = 0;
      files.forEach(function (f) { added += f.added || 0; deleted += f.deleted || 0; });
      var sorted = files.slice().sort(function (a, b) {
        var d = ((b.added || 0) + (b.deleted || 0)) - ((a.added || 0) + (a.deleted || 0));
        if (d !== 0) return d;
        return a.path < b.path ? -1 : (a.path > b.path ? 1 : 0);
      });
      var total = added + deleted;
      return {
        count: files.length,
        added: added,
        deleted: deleted,
        addedPct: total ? (added / total) * 100 : 0,
        top: sorted.slice(0, 3),
        more: Math.max(0, files.length - 3),
      };
    },
  };
}
