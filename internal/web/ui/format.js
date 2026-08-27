// Small pure formatters shared across the mixins.

export function reEscape(s) {
  return String(s).replace(/[\\^$.*+?()[\]{}|]/g, '\\$&');
}

// Local hh:mm, for the "updated 09:41" stamps Settings and Stats both print.
export function clockHM(date) {
  return String(date.getHours()).padStart(2, '0') + ':' + String(date.getMinutes()).padStart(2, '0');
}

// Wall-clock seconds one history entry ran. Queue time sits between entries,
// so it never counts toward a stage.
export function runSeconds(h) {
  if (!h || !h.started_at || !h.completed_at) return 0;
  var s = Math.floor((new Date(h.completed_at) - new Date(h.started_at)) / 1000);
  return s > 0 ? s : 0;
}

// The earliest value the schedule inputs accept, as a datetime-local string.
// The daemon refuses a past instant, and a native min turns that refusal into a
// field the browser will not submit.
export function scheduleMinLocal(now) {
  var d = now || new Date();
  var pad = function (n) { return String(n).padStart(2, '0'); };
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate())
    + 'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}

// An RFC 3339 instant as the value a datetime-local input takes.
// The input has no zone, so the parts are read in local time.
export function isoToLocalInput(iso) {
  if (!iso) return '';
  var d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  var pad = function (n) { return String(n).padStart(2, '0'); };
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate())
    + 'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}

// Duration units the schedule field takes, in milliseconds. The set mirrors
// ticket.ParseScheduleDelay: Go's own units, plus d and w, so a delay learned
// from `kontora schedule --after` works here and the other way round.
const SCHEDULE_UNIT_MS = { ms: 1, s: 1000, m: 60000, h: 3600000, d: 86400000, w: 604800000 };

// One <number><unit> group. A delay is a run of them ("1w2d3h"), summed the way
// time.ParseDuration sums repeated units.
const SCHEDULE_DELAY_PART = /(\d+(?:\.\d+)?)(ms|s|m|h|d|w)/g;

// A delay such as "24h", "3d" or "1w2d3h" as milliseconds, or 0 when the text
// is not one. Anything outside the unit groups makes it not a delay, so a
// half-typed "3 days" is rejected rather than read as 3 of something.
export function parseScheduleDelay(text) {
  var s = String(text || '').trim();
  if (!s) return 0;
  SCHEDULE_DELAY_PART.lastIndex = 0;
  var total = 0, consumed = 0, m;
  while ((m = SCHEDULE_DELAY_PART.exec(s)) !== null) {
    total += parseFloat(m[1]) * SCHEDULE_UNIT_MS[m[2]];
    consumed += m[0].length;
  }
  if (!total || consumed !== s.length) return 0;
  return total;
}

// Local wall time with a space or a T between the date and the time, the
// spellings ticket.ParseScheduleFlex takes. Seconds are optional and dropped:
// the field stores second precision and nobody schedules to the second.
const SCHEDULE_LOCAL = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::\d{2})?$/;

// The one input on every schedule surface: an absolute local time or a delay
// from now, the two grammars `kontora schedule` takes. Returns the instant it
// means, or the reason it means none.
//
// A past instant is named here rather than sent: the daemon refuses one (with a
// minute of grace for the round trip), and a refusal the user reads before
// pressing the button is a correction rather than an error.
export function parseScheduleInput(text, now) {
  var s = String(text || '').trim();
  var at = now instanceof Date ? now : new Date();
  if (!s) return { iso: '', at: null, error: '' };

  var delay = parseScheduleDelay(s);
  var when = null;
  if (delay) {
    when = new Date(at.getTime() + delay);
  } else {
    var m = SCHEDULE_LOCAL.exec(s);
    // Not "not a date": a bare date is the common near-miss, and saying which
    // half is missing is what tells the user to add a time.
    if (!m) {
      if (/^\d{4}-\d{2}-\d{2}$/.test(s)) return { iso: '', at: null, error: 'add a time — a date alone is ambiguous' };
      return { iso: '', at: null, error: 'not a time or a duration' };
    }
    when = new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], 0, 0);
    if (isNaN(when.getTime())) return { iso: '', at: null, error: 'not a time or a duration' };
  }
  // ticket.ScheduleGrace: an instant a moment old was current when it was typed.
  if (when.getTime() < at.getTime() - 60000) return { iso: '', at: null, error: 'that time has already passed' };
  return { iso: storedISO(when), at: when, error: '' };
}

// A native datetime-local value ("2026-09-01T09:00") as the text the schedule
// fields hold. Empty for anything that is not a full local date and time, which
// the callers read as "the picker gave nothing to write".
export function pickerToScheduleText(value) {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(value || '')) return '';
  return value.slice(0, 16).replace('T', ' ');
}

// An instant as the text the schedule fields hold: local, to the minute, in the
// spelling parseScheduleInput and `kontora schedule --at` both read. What a
// preset or the native picker writes back into the one input.
export function localScheduleText(date) {
  var pad = function (n) { return String(n).padStart(2, '0'); };
  return date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate())
    + ' ' + pad(date.getHours()) + ':' + pad(date.getMinutes());
}

// An instant in the spelling the daemon stores: UTC, second precision. What
// every schedule request sends, so the preview of a ticket and the file written
// for it read the same.
export function storedISO(date) {
  var d = date instanceof Date ? date : new Date(date);
  if (isNaN(d.getTime())) return '';
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

// An instant as RFC 3339 in the local zone ("2026-09-01T09:00:00+02:00"). The
// daemon stores the UTC spelling of the same instant; this is the spelling a
// person reads and can paste into `kontora schedule --at`.
export function isoWithOffset(date) {
  var pad = function (n) { return String(n).padStart(2, '0'); };
  var offset = -date.getTimezoneOffset();
  var sign = offset < 0 ? '-' : '+';
  var abs = Math.abs(offset);
  return date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate())
    + 'T' + pad(date.getHours()) + ':' + pad(date.getMinutes()) + ':' + pad(date.getSeconds())
    + sign + pad(Math.floor(abs / 60)) + ':' + pad(abs % 60);
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

// How far ahead an instant is, in the buckets timeAgo uses backwards.
//
// Rounded, where timeAgo truncates. Elapsed time truncates naturally ("2h ago"
// covers the whole third hour), but a distance is read back against what the
// user asked for: a moment passes between typing "3d" and drawing the line, and
// truncation would answer "in 2d".
export function timeUntil(date, now) {
  var diff = Math.round((date - now) / 1000);
  if (diff < 60) return 'now';
  if (diff < 3600) return Math.round(diff / 60) + 'm';
  if (diff < 86400) return Math.round(diff / 3600) + 'h';
  if (diff < 604800) return Math.round(diff / 86400) + 'd';
  return Math.round(diff / 604800) + 'w';
}

// Days from ref to the coming Monday. Never 0: on a Monday the next one is a
// week out, which is what both callers mean by "Monday".
function daysToNextMonday(ref) {
  return (8 - ref.getDay()) % 7 || 7;
}

// Midnight on that Monday. A weekday name says which day only inside one
// calendar week: "Tue" five days out is next Tuesday, and reading it as this
// week's is a week-long mistake. Past that boundary the chip names the month
// instead.
function startOfNextWeek(ref) {
  return new Date(ref.getFullYear(), ref.getMonth(), ref.getDate() + daysToNextMonday(ref), 0, 0, 0, 0);
}

// The card chip's two halves: the absolute time, shortened by how far away it
// is, and the distance to it.
//
// The shortening is what keeps the chip beside a pipeline badge in a 280px
// column — a time today needs no date, and one this week needs no month.
//
// An instant already past is one the daemon has not woken for yet. It reads
// "due now" with no distance: "0m" would look like a clock that stopped.
export function scheduleChipLabel(iso, now) {
  if (!iso) return null;
  var at = new Date(iso);
  if (isNaN(at.getTime())) return null;
  var ref = now instanceof Date ? now : new Date();
  var pad = function (n) { return String(n).padStart(2, '0'); };
  var hm = pad(at.getHours()) + ':' + pad(at.getMinutes());
  if (at <= ref) return { abs: 'due now', rel: '' };

  var abs = at.toDateString() === ref.toDateString() ? hm
    : at < startOfNextWeek(ref) ? WEEKDAYS[at.getDay()] + ' ' + hm
    : MONTHS[at.getMonth()] + ' ' + at.getDate() + ', ' + hm;
  return { abs: abs, rel: timeUntil(at, ref) };
}

// The create form's echo line, the field's trust mechanism: the weekday, the
// zone, the distance and the exact instant, so a mistyped year or a time in
// yesterday's zone is visible before the ticket is written.
export function scheduleEchoParts(iso, now) {
  if (!iso) return null;
  var at = new Date(iso);
  if (isNaN(at.getTime())) return null;
  var ref = now instanceof Date ? now : new Date();
  var pad = function (n) { return String(n).padStart(2, '0'); };
  // The short zone name the runtime prints for this instant ("CEST"), which is
  // the half that says the reading is in the user's own zone, not UTC.
  var zone = '';
  try {
    var parts = new Intl.DateTimeFormat(undefined, { timeZoneName: 'short' }).formatToParts(at);
    zone = (parts.find(p => p.type === 'timeZoneName') || {}).value || '';
  } catch (e) {
    zone = '';
  }
  return {
    long: WEEKDAYS[at.getDay()] + ' ' + at.getDate() + ' ' + MONTHS[at.getMonth()] + ' ' + at.getFullYear()
      + ', ' + pad(at.getHours()) + ':' + pad(at.getMinutes()),
    zone: zone,
    distance: 'in ' + timeUntil(at, ref),
    rfc: isoWithOffset(at),
  };
}

// The presets every schedule surface offers, in one table so the create form,
// the palette scope and the phone sheet cannot drift apart. The create form
// renders all of them; the palette and the sheet render the first three.
//
// A preset already behind the clock is dropped rather than shown greyed: after
// 18:00 "tonight 18:00" is not a choice, and the daemon would refuse it.
export function schedulePresets(now) {
  var ref = now instanceof Date ? now : new Date();
  var atHour = function (dayOffset, hour) {
    var d = new Date(ref.getFullYear(), ref.getMonth(), ref.getDate() + dayOffset, hour, 0, 0, 0);
    return d;
  };
  var all = [
    { key: 'tonight', label: 'tonight 18:00', at: atHour(0, 18) },
    { key: 'tomorrow', label: 'tomorrow 09:00', at: atHour(1, 9) },
    { key: 'monday', label: 'Monday 09:00', at: atHour(daysToNextMonday(ref), 9) },
    { key: 'in24h', label: 'in 24h', at: new Date(ref.getTime() + 86400000) },
    { key: 'in3d', label: 'in 3d', at: new Date(ref.getTime() + 3 * 86400000) },
  ];
  return all.filter(p => p.at > ref).map(p => ({
    key: p.key,
    label: p.label,
    at: p.at,
    iso: storedISO(p.at),
    rel: 'in ' + timeUntil(p.at, ref),
    // The date the row's sub line carries: "Fri 28 Aug".
    date: WEEKDAYS[p.at.getDay()] + ' ' + p.at.getDate() + ' ' + MONTHS[p.at.getMonth()],
  }));
}
