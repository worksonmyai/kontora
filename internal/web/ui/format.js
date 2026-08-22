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
