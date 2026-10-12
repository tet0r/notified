'use strict';

// Loaded in <head> so the theme is applied before the first paint.
// The preference is 'system', 'dark' or 'light' and lives in this browser.
const NotifiedTheme = (() => {
  const KEY = 'notified-theme';
  const media = window.matchMedia('(prefers-color-scheme: light)');
  const get = () => {
    try {
      const v = localStorage.getItem(KEY);
      return v === 'light' || v === 'dark' ? v : 'system';
    } catch (e) {
      return 'system';
    }
  };
  const apply = () => {
    const pref = get();
    document.documentElement.dataset.theme = pref === 'system' ? (media.matches ? 'light' : 'dark') : pref;
  };
  const set = (pref) => {
    try {
      if (pref === 'system') localStorage.removeItem(KEY); else localStorage.setItem(KEY, pref);
    } catch (e) { /* storage unavailable: theme lasts for this page only */ }
    apply();
  };
  media.addEventListener('change', apply);
  apply();
  return { get, set };
})();
