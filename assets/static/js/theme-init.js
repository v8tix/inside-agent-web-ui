// Color theme: auto (follow the system), light or dark.
//
// Loaded from <head> without defer, so the theme is set before anything is
// painted and the page never flashes. It is a file of its own because the CSP
// forbids inline scripts. The choice is kept in localStorage, which can be
// unavailable (private windows, blocked storage): then it just is not kept.
(() => {
  'use strict';

  const KEY = 'theme';
  const NEXT = { auto: 'light', light: 'dark', dark: 'auto' };
  const NAME = { auto: 'Auto (system)', light: 'Light', dark: 'Dark' };
  const root = document.documentElement;
  const system = window.matchMedia('(prefers-color-scheme: dark)');

  function stored() {
    try {
      const value = localStorage.getItem(KEY);
      return value === 'light' || value === 'dark' ? value : 'auto';
    } catch {
      return 'auto';
    }
  }

  function save(mode) {
    try {
      if (mode === 'auto') localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, mode);
    } catch {
      // not kept; the theme still changes for this page
    }
  }

  // apply sets the theme on <html>: data-theme-mode is the choice, data-bs-theme
  // is what Bootstrap and our styles use.
  function apply(mode) {
    root.setAttribute('data-theme-mode', mode);
    root.setAttribute('data-bs-theme', mode === 'auto' ? (system.matches ? 'dark' : 'light') : mode);
    for (const button of document.querySelectorAll('[data-testid="theme-toggle"]')) {
      const label = `Theme: ${NAME[mode]}. Switch to ${NAME[NEXT[mode]]}.`;
      button.setAttribute('aria-label', label);
      button.setAttribute('title', label);
    }
  }

  apply(stored());
  system.addEventListener('change', () => apply(stored()));
  window.addEventListener('storage', (event) => {
    if (event.key === KEY || event.key === null) apply(stored());
  });
  document.addEventListener('DOMContentLoaded', () => apply(stored()));
  // A boosted page swap brings new buttons, which start with the plain label.
  document.addEventListener('htmx:afterSettle', () => apply(stored()));

  document.addEventListener('click', (event) => {
    if (!(event.target instanceof Element) || !event.target.closest('[data-testid="theme-toggle"]')) return;
    const mode = NEXT[stored()];
    save(mode);
    apply(mode);
  });
})();
