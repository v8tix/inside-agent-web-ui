// eval:focus
// What has the keyboard focus: its test id, its accessible name, and the address
// when it is a link.
() => {
  const el = document.activeElement;
  if (!el || el === document.body) return {testid: null, name: '', href: null};
  return {
    testid: el.dataset ? (el.dataset.testid ?? null) : null,
    name: el.getAttribute('aria-label') || el.textContent.trim().slice(0, 80),
    href: el.getAttribute('href'),
  };
}
