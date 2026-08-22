// Runs before first paint so the page never renders in the wrong palette.
// Both palettes live in app.css under :root[data-theme="..."], so setting the
// attribute is all this has to do.
function getStoredTheme() {
  try { return localStorage.getItem('kontora-theme'); } catch (e) { return null; }
}
function setStoredTheme(t) {
  try { localStorage.setItem('kontora-theme', t); } catch (e) {}
}
function applyTheme(t) {
  document.documentElement.dataset.theme = t;
  document.documentElement.style.colorScheme = t;
}
applyTheme(getStoredTheme() === 'light' ? 'light' : 'dark');
