// Stats view. Merged into the kontora() component by app.js, so `this` here is
// the same Alpine object the board and Settings run on.
//
// Every figure arrives pre-aggregated from GET /api/stats. This file lays out
// and formats; it computes no rate, median or delta of its own. The one
// exception is bar geometry, which is a pixel height, not a measurement.

const STATS_RANGES = ['1d', '1w', '30d', '90d', 'all'];

// Chip labels. The long windows are cut to a whole number of weeks by the
// server — 35, 98 and 182 days — so a chip reading "30d" beside a caption
// reading "last 35 days" would contradict itself, and "all" would claim a
// lifetime total the 182-day cap does not deliver. The two short windows are
// cut to the day, so they are labelled as asked for.
const STATS_RANGE_LABELS = { '1d': '1d', '1w': '1w', '30d': '5w', '90d': '14w', all: '26w' };
const STATS_STAGE_MODES = ['time', 'tokens'];
const STATS_POLL_MS = 30000;

// Heat map geometry, mirrored by the cell classes in index.html: a 13px cell
// plus a 3px gap. Month ticks are positioned at weekIndex * this.
const STATS_CELL_PITCH = 16;
const STATS_WEEKLY_H = 86;
const STATS_TOKEN_H = 44;

const STATS_MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

// Heat levels 1-4 as accent alpha; level 0 is the empty-cell surface. Both come
// from theme tokens, so the light theme needs no rules of its own.
const STATS_HEAT_ALPHA = [0, 0.28, 0.5, 0.72, 1];

// Stage rows cycle this palette by position. Stage names are user-defined, so
// there is no fixed name-to-colour map to honour.
const STATS_STAGE_COLORS = [
  'hsl(var(--st-open))',
  'hsl(var(--st-progress))',
  'hsl(var(--st-done))',
  'hsl(var(--st-review))',
  'rgba(var(--surface-600),1)',
];

const STATS_UNITS = ['', 'k', 'M', 'B'];

function statsCompactPart(v) {
  return v.toFixed(1).replace(/\.0$/, '');
}

// statsCompact shortens a count for a 26px KPI value: 2.3k, 24M.
function statsCompact(n) {
  let v = Number(n) || 0;
  let unit = 0;
  while (Math.abs(v) >= 1000 && unit < STATS_UNITS.length - 1) { v /= 1000; unit++; }
  // Rounding to one decimal can carry the value into the next unit: 999999 is
  // 1M, not the '1000k' that overflows the value slot.
  if (Math.abs(Number(v.toFixed(1))) >= 1000 && unit < STATS_UNITS.length - 1) { v /= 1000; unit++; }
  return (unit === 0 ? String(Math.round(v)) : statsCompactPart(v)) + STATS_UNITS[unit];
}

// statsTokenTotal is the one figure behind a token count. tokens_in already
// contains both cache figures, so the total is in + out: summing all four
// would count the cached tokens twice.
function statsTokenTotal(rec) {
  return (Number(rec && rec.tokens_in) || 0) + (Number(rec && rec.tokens_out) || 0);
}

// statsTokenBreakdown names the four categories behind one token figure. Fresh
// input is what is left of tokens_in after subtracting both cache figures. The
// clamp guards the one case that subtraction cannot survive: a payload whose
// cache figures exceed the total they are supposed to be subsets of, which the
// page can detect no other way.
function statsTokenBreakdown(rec) {
  const tin = Number(rec && rec.tokens_in) || 0;
  const tout = Number(rec && rec.tokens_out) || 0;
  const create = Number(rec && rec.tokens_cache_create) || 0;
  const read = Number(rec && rec.tokens_cache_read) || 0;
  return statsCompact(Math.max(tin - create - read, 0)) + ' fresh in · ' +
    statsCompact(create) + ' cache write · ' + statsCompact(read) + ' cache read · ' +
    statsCompact(tout) + ' out';
}

// statsDuration renders a span the way the design writes it: 22m, 1h 05m,
// 4h 12m, 3d 04h. Zero means "not measured" and renders as an em dash; a span
// too short to round to a minute is written as such, so a fast stage is not
// mistaken for one that recorded nothing.
function statsDuration(ms) {
  const raw = Number(ms) || 0;
  if (raw <= 0) return '—';
  const total = Math.round(raw / 60000);
  if (total <= 0) return '<1m';
  const days = Math.floor(total / 1440);
  const hours = Math.floor((total % 1440) / 60);
  const mins = total % 60;
  if (days > 0) return days + 'd ' + String(hours).padStart(2, '0') + 'h';
  if (hours > 0) return hours + 'h ' + String(mins).padStart(2, '0') + 'm';
  return total + 'm';
}

// statsWeekday is the day of week of a "YYYY-MM-DD" string. It goes through
// Date.UTC rather than parsing the string, because `new Date('2026-08-12')`
// reads UTC midnight and would shift the day for anyone west of Greenwich —
// exactly the users the server-side local-zone bucketing exists for.
function statsWeekday(iso) {
  const p = String(iso).split('-');
  return new Date(Date.UTC(Number(p[0]), Number(p[1]) - 1, Number(p[2]))).getUTCDay();
}

function statsDayLabel(iso) {
  const p = String(iso).split('-');
  return STATS_MONTHS[Number(p[1]) - 1] + ' ' + Number(p[2]);
}

function statsHeatColor(level) {
  if (!level) return 'rgba(var(--surface-800),1)';
  return 'rgba(var(--accent),' + STATS_HEAT_ALPHA[level] + ')';
}

// statsHeatWeeks turns the day series into Sunday-started columns, padding the
// first one with blanks so the weekday rows line up, and places one month tick
// per month change.
function statsHeatWeeks(days) {
  const list = Array.isArray(days) ? days : [];
  let max = 0;
  list.forEach(function(d) { max = Math.max(max, Number(d.runs) || 0); });

  const cells = list.map(function(d) {
    const n = Number(d.runs) || 0;
    // max + 0.001 keeps the busiest day inside level 4 without a divide by
    // zero on an empty window.
    const level = n === 0 ? 0 : Math.min(4, 1 + Math.floor((n / (max + 0.001)) * 3.999));
    return { date: d.date, runs: n, level: level, tip: n + (n === 1 ? ' run · ' : ' runs · ') + statsDayLabel(d.date) };
  });
  if (!cells.length) return { weeks: [], months: [], max: 0 };

  const padded = new Array(statsWeekday(cells[0].date)).fill(null).concat(cells);
  while (padded.length % 7 !== 0) padded.push(null);

  const weeks = [];
  for (let i = 0; i < padded.length; i += 7) {
    weeks.push({ index: weeks.length, days: padded.slice(i, i + 7) });
  }

  const months = [];
  let lastMonth = -1;
  weeks.forEach(function(w) {
    const first = w.days.find(function(c) { return c; });
    if (!first) return;
    const m = Number(String(first.date).split('-')[1]) - 1;
    if (m === lastMonth) return;
    lastMonth = m;
    months.push({ label: STATS_MONTHS[m], left: w.index * STATS_CELL_PITCH });
  });

  return { weeks: weeks, months: months, max: max };
}

// statsFirstPassColor names the token class a first-pass rate is drawn in. The
// boundaries are the design's: 78 and 70.
function statsFirstPassColor(pct) {
  const n = Number(pct) || 0;
  if (n >= 78) return 'ok';
  if (n >= 70) return 'warn';
  return 'err';
}

function statsPctLabel(pct) {
  const n = Number(pct) || 0;
  return Math.round(n) + '%';
}

function statsSigned(n) {
  return (n >= 0 ? '+' : '−') + Math.abs(n);
}

// statsKpis builds the six headline cards. Tone is 'ok', 'warn' or 'neutral',
// and a comparison the payload cannot make reads "no earlier window" rather
// than a zero.
function statsKpis(payload) {
  const t = payload.totals || {};
  const win = payload.window || {};
  const days = win.days || 1;

  const cycleDelta = t.median_cycle_delta_ms == null
    ? { text: 'no earlier window', tone: 'neutral' }
    : t.median_cycle_delta_ms === 0
      // statsDuration writes an em dash for a zero span, which here would read
      // as "not measured" rather than as the two windows agreeing.
      ? { text: 'no change vs prev', tone: 'neutral' }
      : {
          text: (t.median_cycle_delta_ms < 0 ? '−' : '+') + statsDuration(Math.abs(t.median_cycle_delta_ms)) + ' vs prev',
          tone: t.median_cycle_delta_ms < 0 ? 'ok' : 'warn',
        };
  const tokenDelta = t.tokens_delta_pct == null
    ? { text: 'no earlier window', tone: 'neutral' }
    : {
        text: statsSigned(Math.round(t.tokens_delta_pct)) + '% vs prev',
        tone: t.tokens_delta_pct <= 0 ? 'ok' : 'warn',
      };

  return [
    {
      label: 'shipped', value: String(t.shipped || 0), unit: 'tickets',
      // The server clamps this count to the window, so on the one-day window it
      // covers today rather than the week it is named after.
      delta: statsSigned(t.shipped_this_week || 0) + (days === 1 ? ' today' : ' this week'),
      tone: (t.shipped_this_week || 0) > 0 ? 'ok' : 'neutral',
    },
    {
      label: 'stage runs', value: statsCompact(t.runs || 0), unit: 'runs',
      delta: Math.round((t.runs || 0) / days) + '/day avg', tone: 'neutral',
    },
    {
      label: 'median cycle', value: statsDuration(t.median_cycle_ms), unit: 'open → done',
      delta: cycleDelta.text, tone: cycleDelta.tone,
    },
    // Every stage run keys a first-pass pair, so no runs means the rate is
    // unmeasured. Reporting the zero as a percentage would assert that nothing
    // passed first time, which is the opposite of what an empty window says.
    (t.runs || 0) === 0
      ? { label: 'first-pass', value: '—', unit: '', delta: 'no stage runs', tone: 'neutral' }
      : {
          label: 'first-pass', value: String(Math.round(t.first_pass_pct || 0)), unit: '%',
          delta: Math.round(100 - (t.first_pass_pct || 0)) + '% needed a retry',
          tone: statsFirstPassColor(t.first_pass_pct) === 'ok' ? 'ok' : 'warn',
        },
    {
      label: 'tokens', value: statsCompact(statsTokenTotal(t)), unit: 'in / out',
      delta: tokenDelta.text, tone: tokenDelta.tone,
      tip: statsCompact(statsTokenTotal(t)) + ' tokens · ' + statsTokenBreakdown(t),
    },
    {
      label: 'busiest day',
      value: t.busiest_day ? String(Number(String(t.busiest_day).split('-')[2])) : '—',
      unit: t.busiest_day ? STATS_MONTHS[Number(String(t.busiest_day).split('-')[1]) - 1] : '',
      delta: (t.busiest_day_runs || 0) + ' runs in a day', tone: 'neutral',
    },
  ];
}

// statsDerive turns one payload into everything the markup draws. It runs once
// per fetch rather than inside the template, so a re-render never re-buckets
// 182 days. Every division guards its denominator: an empty window must render,
// not produce NaN.
function statsDerive(payload) {
  if (!payload) return null;
  const weeksRaw = payload.weeks || [];
  const stagesRaw = payload.stages || [];
  const agentsRaw = payload.agents || [];
  const projectsRaw = payload.projects || [];
  const live = payload.live || {};
  const totals = payload.totals || {};

  let weeklyMax = 0;
  weeksRaw.forEach(function(w) { weeklyMax = Math.max(weeklyMax, (w.done || 0) + (w.cancelled || 0)); });
  const weekly = weeksRaw.map(function(w, i) {
    const done = w.done || 0, cancelled = w.cancelled || 0;
    return {
      week: w.week,
      latest: i === weeksRaw.length - 1,
      doneH: weeklyMax ? (done / weeklyMax) * STATS_WEEKLY_H : 0,
      cancelH: weeklyMax ? (cancelled / weeklyMax) * STATS_WEEKLY_H : 0,
      tip: done + ' shipped · week of ' + statsDayLabel(w.week) + (cancelled ? ' · ' + cancelled + ' cancelled' : ''),
    };
  });

  let tokenMax = 0;
  weeksRaw.forEach(function(w) { tokenMax = Math.max(tokenMax, statsTokenTotal(w)); });
  const tokens = weeksRaw.map(function(w, i) {
    const tin = w.tokens_in || 0, tout = w.tokens_out || 0;
    return {
      week: w.week,
      latest: i === weeksRaw.length - 1,
      inH: tokenMax ? (tin / tokenMax) * STATS_TOKEN_H : 0,
      outH: tokenMax ? (tout / tokenMax) * STATS_TOKEN_H : 0,
      tip: statsCompact(statsTokenTotal(w)) + ' tokens · week of ' + statsDayLabel(w.week) +
        ' · ' + statsTokenBreakdown(w),
    };
  });

  // Both modes of the stage panel are built here, so its header toggle is a
  // template switch over one payload rather than a second derivation. The
  // colour is taken from the server's time-share order before anything sorts
  // by tokens, so a stage keeps it in either mode.
  const stages = stagesRaw.map(function(s, i) {
    const runs = s.runs || 0;
    const tokenRuns = s.token_runs || 0;
    const measured = tokenRuns > 0;
    const failed = (s.failed || 0) + ' failed';
    return {
      name: s.name,
      color: STATS_STAGE_COLORS[i % STATS_STAGE_COLORS.length],
      retry: statsPctLabel(s.retry_pct) + ' retried',
      // A stage where a fifth of the runs are retries is the one to look at.
      hot: (s.retry_pct || 0) >= 15,
      time: {
        share: s.share || 0,
        value: statsDuration(s.p50_ms),
        sub: 'p90 ' + statsDuration(s.p90_ms),
        meta: runs + ' runs · ' + failed,
        tip: s.name + ' · ' + Math.round(s.share || 0) + '% of measured time · p50 ' +
          statsDuration(s.p50_ms) + ' · p90 ' + statsDuration(s.p90_ms),
      },
      // A stage none of whose runs recorded counts reads as unmeasured, not as
      // free: it takes no share of the bar and neither figure is a number.
      tokens: {
        share: measured ? (s.token_share || 0) : 0,
        value: measured ? statsCompact(s.tokens) : '—',
        // The bold figure is the stage total and this one is per run, so it
        // says which it is rather than letting the two read as one unit.
        sub: measured ? 'p90/run ' + statsCompact(s.tokens_p90) : 'no counts',
        // The total covers the runs that recorded counts. When that is fewer
        // than the stage ran, the row counts those instead of implying the
        // total spans them all.
        meta: (measured && tokenRuns < runs ? tokenRuns + ' of ' + runs + ' runs measured' : runs + ' runs') + ' · ' + failed,
        tip: measured
          ? s.name + ' · ' + Math.round(s.token_share || 0) + '% of stage tokens · ' +
            statsCompact(s.tokens) + ' over ' + tokenRuns + ' measured runs'
          : s.name + ' · no token counts recorded',
      },
    };
  });
  const stageTokens = stagesRaw.reduce(function(n, s) { return n + (Number(s.tokens) || 0); }, 0);
  // With no counts anywhere in the window there is no order to rank by and no
  // bar to fill, so tokens mode drops to the card's empty state rather than
  // drawing zero-width segments over rows sorted by name.
  const stagesByTokens = stageTokens > 0 ? stages.slice().sort(function(a, b) {
    if (b.tokens.share !== a.tokens.share) return b.tokens.share - a.tokens.share;
    return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
  }) : [];

  const busy = live.busy || [];
  const agents = agentsRaw.map(function(a) {
    const perRun = a.tokens_per_run == null ? '—' : statsCompact(a.tokens_per_run);
    const sub = [a.model, (a.retries_per_ticket || 0).toFixed(1) + ' retries/ticket'].filter(Boolean).join(' · ');
    return {
      name: a.name,
      sub: sub,
      running: busy.indexOf(a.name) >= 0,
      runs: statsCompact(a.runs || 0),
      pct: statsPctLabel(a.first_pass_pct),
      pctWidth: Math.max(0, Math.min(100, a.first_pass_pct || 0)),
      tone: statsFirstPassColor(a.first_pass_pct),
      median: statsDuration(a.median_ms),
      perRun: perRun,
      tip: a.name + ' · ' + (a.runs || 0) + ' runs · ' + statsPctLabel(a.first_pass_pct) + ' first-pass · ' +
        (a.tokens_per_run == null ? 'no token counts recorded' : perRun + ' tokens per run'),
    };
  });

  const projectMax = projectsRaw.reduce(function(m, p) { return Math.max(m, p.done || 0); }, 0);
  const projects = projectsRaw.map(function(p) {
    return {
      name: p.name,
      done: p.done || 0,
      width: projectMax ? ((p.done || 0) / projectMax) * 100 : 0,
      cycle: statsDuration(p.median_cycle_ms),
      tip: p.name + ' · ' + (p.done || 0) + ' shipped · median cycle ' + statsDuration(p.median_cycle_ms),
    };
  });

  const slots = [];
  for (let i = 0; i < (live.slots || 0); i++) {
    const isBusy = i < (live.running || 0);
    slots.push({
      busy: isBusy,
      tip: 'slot ' + (i + 1) + ' · ' + (isBusy ? (busy[i] || 'busy') : 'free'),
    });
  }

  return {
    heat: statsHeatWeeks(payload.days),
    heatCaption: statsCompact(totals.runs || 0) + ' runs · ' + (payload.days || []).length + ' days',
    weekly: weekly,
    tokens: tokens,
    tokenCaption: statsCompact(statsTokenTotal(totals)) + ' tokens',
    stages: stages,
    stagesByTokens: stagesByTokens,
    // "in stages" is what keeps this honest beside the KPI token card, which
    // also counts the annotation runs the stage figures leave out.
    stageTokens: statsCompact(stageTokens) + ' tokens in stages',
    agents: agents,
    projects: projects,
    slots: slots,
    live: {
      running: live.running || 0,
      slots: live.slots || 0,
      queued: live.queued || 0,
      oldest: live.queued ? statsDuration(live.oldest_wait_ms) : '—',
      inReview: live.in_review || 0,
    },
    kpis: statsKpis(payload),
    medianCycle: statsDuration(totals.median_cycle_ms),
    legend: [0, 1, 2, 3, 4],
  };
}

function kontoraStats() {
  return {
    statsRange: (function() {
      try {
        const saved = localStorage.getItem('kontora-stats-range');
        return STATS_RANGES.indexOf(saved) >= 0 ? saved : '90d';
      } catch (e) { return '90d'; }
    })(),
    statsStageMode: (function() {
      try {
        const saved = localStorage.getItem('kontora-stats-mode');
        return STATS_STAGE_MODES.indexOf(saved) >= 0 ? saved : 'time';
      } catch (e) { return 'time'; }
    })(),
    statsProject: 'all',
    statsPipeline: 'all',
    statsRanges: STATS_RANGES,
    statsStageModes: STATS_STAGE_MODES,
    stats: null,
    statsDerived: null,
    statsLoading: false,
    statsError: null,
    statsUpdated: '',
    _statsTimer: null,
    _statsSeq: 0,

    statsCompact: statsCompact,
    statsDuration: statsDuration,
    statsHeatWeeks: statsHeatWeeks,
    statsFirstPassColor: statsFirstPassColor,
    statsHeatColor: statsHeatColor,

    // statsCards keeps the KPI strip six cards wide before the first payload
    // arrives, so the first paint is the real layout, not a blank frame.
    statsCards() {
      return this.statsDerived ? this.statsDerived.kpis : statsKpis({});
    },

    // openStats is called by gotoView, the one place currentView changes.
    openStats() {
      this.closeStats();
      this.fetchStats();
      this._statsTimer = setInterval(() => {
        // gotoView clears the timer on the way out. This is the backstop for
        // a timer that somehow outlives the view.
        if (this.currentView !== 'stats') { this.closeStats(); return; }
        this.fetchStats();
      }, STATS_POLL_MS);
    },

    closeStats() {
      if (this._statsTimer === null) return;
      clearInterval(this._statsTimer);
      this._statsTimer = null;
    },

    statsQuery() {
      let q = 'range=' + encodeURIComponent(this.statsRange);
      if (this.statsProject !== 'all') q += '&project=' + encodeURIComponent(this.statsProject);
      if (this.statsPipeline !== 'all') q += '&pipeline=' + encodeURIComponent(this.statsPipeline);
      return q;
    },

    async fetchStats() {
      if (this.currentView !== 'stats') return;
      if (!this.stats) this.statsLoading = true;
      // Two filter changes in a row leave two requests in flight, and the
      // first can answer last. Only the newest one is allowed to land.
      const seq = ++this._statsSeq;
      try {
        const res = await fetch('/api/stats?' + this.statsQuery());
        // A session that expires while Stats is open shows the login prompt,
        // the way every other fetch in the app treats a 401. Polling a dead
        // session behind a red error string would never recover.
        if (res.status === 401) {
          if (seq !== this._statsSeq) return;
          this.closeStats();
          this.needsAuth = true;
          this.statsError = null;
          return;
        }
        if (!res.ok) throw new Error('stats request failed (' + res.status + ')');
        const payload = await res.json();
        if (seq !== this._statsSeq) return;
        this.stats = payload;
        this.statsDerived = statsDerive(payload);
        this.statsUpdated = clockHM(new Date());
        this.statsError = null;
      } catch (e) {
        if (seq !== this._statsSeq) return;
        this.statsError = String((e && e.message) || e);
      } finally {
        if (seq === this._statsSeq) this.statsLoading = false;
      }
    },

    setStatsRange(range) {
      if (this.statsRange === range) return;
      this.statsRange = range;
      try { localStorage.setItem('kontora-stats-range', range); } catch (e) { /* private mode */ }
      this.fetchStats();
    },

    // Both orders are already in the payload the page holds, so switching mode
    // redraws the card and asks the daemon for nothing.
    setStatsStageMode(mode) {
      if (this.statsStageMode === mode) return;
      this.statsStageMode = mode;
      try { localStorage.setItem('kontora-stats-mode', mode); } catch (e) { /* private mode */ }
    },

    statsStageRows() {
      if (!this.statsDerived) return [];
      return this.statsStageMode === 'tokens' ? this.statsDerived.stagesByTokens : this.statsDerived.stages;
    },

    // Tokens mode has rows to show only where something recorded counts, so an
    // empty card there means the window went unmeasured, not that it was idle.
    statsStageEmpty() {
      if (this.statsStageMode === 'tokens' && this.statsDerived && this.statsDerived.stages.length) {
        return 'no token counts in this window';
      }
      return 'no data in this window';
    },

    // The median cycle is a ticket figure rather than a stage one, so it stands
    // even when no stage ran; a stage-token total with no stages does not.
    statsStageCaption() {
      if (!this.statsDerived) return '';
      if (this.statsStageMode !== 'tokens') return 'median cycle ' + this.statsDerived.medianCycle;
      return this.statsDerived.stagesByTokens.length ? this.statsDerived.stageTokens : '';
    },

    setStatsProject(name) {
      if (this.statsProject === name) return;
      this.statsProject = name;
      this.fetchStats();
    },

    setStatsPipeline(name) {
      if (this.statsPipeline === name) return;
      this.statsPipeline = name;
      this.fetchStats();
    },

    statsRangeLabel(range) {
      return STATS_RANGE_LABELS[range] || range;
    },

    statsWindowLabel() {
      const win = (this.stats && this.stats.window) || null;
      if (!win) return '';
      // Days, not weeks: the window spans one more Sunday bucket than its length
      // whenever it opens mid-week, and the chip beside this would disagree.
      const days = win.days || 0;
      const span = 'last ' + days + (days === 1 ? ' day' : ' days');
      return this.statsUpdated ? span + ' · updated ' + this.statsUpdated : span;
    },

    statsProjectOptions() {
      return ['all'].concat(((this.configCache && this.configCache.projects) || []).map(function(p) { return p.name; }));
    },

    statsPipelineOptions() {
      return ['all'].concat((this.configCache && this.configCache.pipelines) || []);
    },
  };
}
