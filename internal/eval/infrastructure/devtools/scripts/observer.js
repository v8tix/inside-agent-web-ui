// agent-eval observer. It is run by Chrome in every new document, before any
// of the page's own code (navigate_page's initScript), and it only watches: it
// records when the reader pressed Enter, how the newest reply grew and changed
// status, and how far the list was from its end after each change. Nothing here
// is part of the Web UI, so the numbers are not produced by the code under test.
//
// All times are milliseconds on performance.timeOrigin + performance.now(),
// which is the same clock in every document and does not depend on how long a
// call to the browser takes.
(() => {
  if (window.__agentEval) return;

  const now = () => performance.timeOrigin + performance.now();
  const LIMIT = 20000; // a runaway page cannot make the record grow without end
  const push = (list, item) => { if (list.length < LIMIT) list.push(item); };

  const state = {
    keydownAt: null,
    stopAt: null,
    updates: [],
    statuses: [],
    scrolls: [],
    late: !!document.querySelector('[data-testid=message]'),
  };
  let lastChars = 0;
  let lastStatus = null;
  let lastId = null;

  const newestReply = () => {
    const all = document.querySelectorAll('[data-testid=message][data-role=assistant]');
    return all.length ? all[all.length - 1] : null;
  };

  // include:visible

  // The element that scrolls the conversation: the nearest ancestor of the reply
  // that can scroll, or the page. scrollable says whether anything is hidden
  // beyond the window; a list that fits is always at its end.
  const scroller = (el) => {
    for (let n = el.parentElement; n && n !== document.documentElement; n = n.parentElement) {
      if (/(auto|scroll)/.test(getComputedStyle(n).overflowY) && n.scrollHeight > n.clientHeight + 1) {
        return { el: n, scrollable: true };
      }
    }
    const page = document.scrollingElement || document.documentElement;
    return { el: page, scrollable: page.scrollHeight > page.clientHeight + 1 };
  };

  // Looks after the page has had its turn: the Web UI scrolls in its own
  // observer, which runs after this one, so the position is read a frame later.
  const sampleScroll = (at) => {
    let done = false;
    const sample = () => {
      if (done) return;
      done = true;
      const reply = newestReply();
      if (!reply) return;
      const { el, scrollable } = scroller(reply);
      push(state.scrolls, {
        at,
        fromBottomPx: Math.max(0, el.scrollHeight - el.scrollTop - el.clientHeight),
        scrollable,
      });
    };
    requestAnimationFrame(sample);
    setTimeout(sample, 80); // for a tab the browser does not draw
  };

  const look = () => {
    const reply = newestReply();
    if (!reply) return;
    const at = now();
    const status = reply.dataset.status ?? null;
    const id = reply.dataset.id ?? null;
    if (status !== lastStatus || id !== lastId) {
      lastStatus = status;
      push(state.statuses, { at, status });
    }
    const body = reply.querySelector('[data-testid=message-body]');
    const chars = body ? visibleText(body).length : 0;
    if (chars > 0 && (chars !== lastChars || id !== lastId)) {
      push(state.updates, { at, chars });
      sampleScroll(at);
    }
    lastChars = chars;
    lastId = id;
  };

  const watcher = new MutationObserver(look);
  watcher.observe(document, {
    childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['data-status'],
  });

  // Enter in the composer sends the message; the first one is the start of the
  // attempt.
  document.addEventListener('keydown', (e) => {
    if (state.keydownAt === null && e.key === 'Enter' && !e.shiftKey && !e.isComposing &&
        e.target && e.target.matches && e.target.matches('[data-testid=composer-input]')) {
      state.keydownAt = now();
    }
  }, true);

  // The first press of Stop, for measuring how quickly it takes effect.
  document.addEventListener('click', (e) => {
    if (state.stopAt === null && e.target && e.target.closest && e.target.closest('[data-testid=stop]')) {
      state.stopAt = now();
    }
  }, true);

  const findings = (root) => {
    let handlers = 0;
    let external = 0;
    for (const el of [root, ...root.querySelectorAll('*')]) {
      for (const a of el.attributes) if (/^on/i.test(a.name)) handlers++;
    }
    for (const img of root.querySelectorAll('img')) {
      try {
        const u = new URL(img.getAttribute('src') || '', location.href);
        if (/^https?:$/.test(u.protocol) && u.origin !== location.origin) external++;
      } catch { /* an address that cannot be read is not an external one */ }
    }
    return { scripts: root.querySelectorAll('script').length, handlers, external };
  };

  window.__agentEval = {
    version: 1,
    markLate() { state.late = true; },
    // collect is what the evaluator reads once the reply is over.
    collect() {
      look(); // in case the last change has not been seen yet
      const reply = newestReply();
      const body = reply && reply.querySelector('[data-testid=message-body]');
      const d = reply ? reply.dataset : {};
      return {
        version: 1,
        keydownAt: state.keydownAt,
        stopAt: state.stopAt,
        updates: state.updates,
        statuses: state.statuses,
        scrolls: state.scrolls,
        late: state.late,
        message: reply ? {
          id: d.id ?? null,
          status: d.status ?? null,
          ttft: d.ttftMs ?? null,
          duration: d.durationMs ?? null,
          promptTokens: d.promptTokens ?? null,
          completionTokens: d.completionTokens ?? null,
          toolCalls: d.toolCalls ?? null,
        } : null,
        visibleText: body ? body.innerText : '',
        unsafe: body ? findings(body) : null,
      };
    },
  };
})();
