// eval:rowExists
// Whether a conversation is still in the sidebar.
() => {
  const id = __ARG__;
  return [...document.querySelectorAll('[data-testid=conversation]')].some(r => r.dataset.id === id);
}
