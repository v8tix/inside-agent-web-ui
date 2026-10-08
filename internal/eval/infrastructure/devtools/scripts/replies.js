// eval:replies
// Every message of the conversation, for the probes that check what was kept,
// replaced or repeated. The text is compared in the evaluator's memory and is
// never written to a result.
() => {
  // include:visible
  return {
    assistants: [...document.querySelectorAll('[data-testid=message][data-role=assistant]')].map((m) => {
      const body = m.querySelector('[data-testid=message-body]');
      const text = body ? visibleText(body) : '';
      return { id: m.dataset.id ?? null, status: m.dataset.status ?? null, chars: text.length, text };
    }),
    users: document.querySelectorAll('[data-testid=message][data-role=user]').length,
    regenerate: [...document.querySelectorAll('[data-testid=regenerate]')].filter((b) => b.offsetParent !== null).length,
    retry: [...document.querySelectorAll('[data-testid=retry]')].filter((b) => b.offsetParent !== null).length,
  };
}
