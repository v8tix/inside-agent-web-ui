// eval:pageState
// What the evaluator needs to know about the page it is looking at.
() => ({
  contract: document.body ? (document.body.dataset.uiContract ?? null) : null,
  path: location.pathname,
  composer: !!document.querySelector('[data-testid=composer-input]'),
  login: !!document.querySelector('[data-testid=login-email]'),
  alert: (document.querySelector('[role=alert][data-testid=error]') || {}).innerText ?? null,
})
