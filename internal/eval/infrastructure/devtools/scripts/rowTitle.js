// eval:rowTitle
// The title a conversation has in the sidebar; null when it is not listed.
() => {
  const id = __ARG__;
  const row = [...document.querySelectorAll('[data-testid=conversation]')].find((r) => r.dataset.id === id);
  const link = row && row.querySelector('[data-testid=conversation-link]');
  return link ? link.textContent.trim() : null;
}
