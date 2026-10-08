// eval:theme
// The theme the page is showing (the choice, and the colors it resolved to).
() => ({
  mode: document.documentElement.getAttribute('data-theme-mode'),
  applied: document.documentElement.getAttribute('data-bs-theme'),
})
