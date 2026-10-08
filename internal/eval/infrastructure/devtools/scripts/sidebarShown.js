// eval:sidebarShown
// Whether the conversation list can be seen: on a phone it slides in from the left.
() => {
  const nav = document.querySelector('[data-testid=sidebar]');
  if (!nav) return false;
  const box = nav.getBoundingClientRect();
  const style = getComputedStyle(nav);
  return style.visibility !== 'hidden' && box.width > 0 && box.right > 0 && box.left < window.innerWidth;
}
