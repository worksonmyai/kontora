function themeMediaQuery() {
  return typeof matchMedia === 'function' ? matchMedia('(prefers-color-scheme: dark)') : null;
}
function resolveTheme(preference, query = themeMediaQuery()) {
  if (preference === 'light' || preference === 'dark') return preference;
  return (query?.matches ?? true) ? 'dark' : 'light';
}
function applyTheme(palette) {
  document.documentElement.dataset.theme = palette;
  document.documentElement.style.colorScheme = palette;
}
applyTheme(resolveTheme(document.documentElement.dataset.themePreference));
