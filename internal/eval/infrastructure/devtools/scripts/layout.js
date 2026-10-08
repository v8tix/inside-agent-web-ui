// eval:layout
// How the page fits the window: how far it spills sideways, whether the message
// field is on screen, and whether the button that opens the conversations (on a
// phone) is there.
() => {
  const root = document.documentElement;
  const field = document.querySelector('[data-testid=composer-input]');
  const box = field && field.getBoundingClientRect();
  return {
    overflow: Math.max(0, root.scrollWidth - root.clientWidth),
    width: root.clientWidth,
    composerVisible: !!box && box.width > 0 && box.height > 0 && box.top >= 0 && box.bottom <= window.innerHeight + 1,
    toggle: !!document.querySelector('[data-testid=sidebar-toggle]'),
  };
}
