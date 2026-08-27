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

// A native datetime-local value ("2026-09-01T09:00") is a local wall time with
// no zone. The instant it means is what the API stores.
//
// The parts are pulled out and handed to the Date constructor rather than
// parsed from the string: Date reads a date-only string as UTC and a
// date-and-time one as local, so a field holding "2026-09-01" would silently
// become midnight in another zone. Anything that is not a full local
// date-and-time returns "", and the caller sends no schedule.
export function localInputToISO(value) {
  var m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(value || '');
  if (!m) return '';
  var d = new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], 0, 0);
  if (isNaN(d.getTime())) return '';
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

// The inverse: an RFC 3339 instant as the value a datetime-local input takes.
// The input has no zone, so the parts are read in local time.
export function isoToLocalInput(iso) {
  if (!iso) return '';
  var d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  var pad = function (n) { return String(n).padStart(2, '0'); };
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate())
    + 'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
}
