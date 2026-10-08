// eval:lastReply
// The newest assistant message: its status, its telemetry, and where its raw
// text is. The message element is replaced on every update, so it is looked up
// afresh each time.
() => {
  const conversation = (location.pathname.match(/^\/c\/([^/]+)/) || [])[1] ?? null;
  const replies = [...document.querySelectorAll('[data-testid=message][data-role=assistant]')];
  const m = replies.at(-1);
  if (!m) return {found: false, conversation};
  const d = m.dataset;
  const error = m.querySelector('[data-testid=error]');
  const copy = m.querySelector('[data-testid=copy-message]');
  const body = m.querySelector('[data-testid=message-body]');
  // include:visible
  return {
    found: true,
    conversation,
    id: d.id ?? null,
    status: d.status ?? null,
    ttft: d.ttftMs ?? null,
    duration: d.durationMs ?? null,
    promptTokens: d.promptTokens ?? null,
    completionTokens: d.completionTokens ?? null,
    toolCalls: d.toolCalls ?? null,
    error: error ? error.innerText : null,
    rawUrl: copy ? (copy.dataset.rawUrl ?? null) : null,
    chars: body ? visibleText(body).length : 0,
  };
}
