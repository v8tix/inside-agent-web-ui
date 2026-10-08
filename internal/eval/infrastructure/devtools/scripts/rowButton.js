// eval:rowButton
// How to find the rename or delete button of one conversation's row in the
// sidebar: its accessible name, and how many buttons with that same name come
// before it. Rows can have the same title (every question of an evaluation
// opens with the same prompt), so their buttons can have the same name, and a
// name alone does not say which button is wanted.
() => {
  const { kind, id } = __ARG__;
  // Each hook is written out, so that the test of the UI contract finds it here.
  const selector = { rename: '[data-testid=rename]', delete: '[data-testid=delete]' }[kind];
  const row = [...document.querySelectorAll('[data-testid=conversation]')].find((r) => r.dataset.id === id);
  const button = selector && row && row.querySelector(selector);
  if (!button) return null;
  const name = button.getAttribute('aria-label');
  const all = [...document.querySelectorAll('[data-testid=conversation] ' + selector)];
  return { name, before: all.slice(0, all.indexOf(button)).filter((b) => b.getAttribute('aria-label') === name).length };
}
