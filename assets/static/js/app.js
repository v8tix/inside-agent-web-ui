// Agent Web UI behavior: the composer, the stop button and autoscroll.
//
// Loaded from <head> with defer. There are no inline scripts (the CSP forbids
// them). The page works as plain forms without this file; this adds comfort.
//
// The server decides what is on the page. This file reads it back: a reply is
// in flight when its message has data-status pending or streaming.
(() => {
  'use strict';

  const NEAR_BOTTOM_PX = 80; // closer than this to the bottom counts as "following"
  const MAX_ROWS = 8; // the composer grows to this many lines, then scrolls

  const $ = (selector) => document.querySelector(selector);
  const input = () => $('#composer-input');
  const form = () => $('#composer');
  const sendButton = () => $('#composer [data-testid="send"]');
  const stopButton = () => $('#composer [data-testid="stop"]');
  const scroller = () => $('#messages');
  const jumpButton = () => $('#jump-latest');

  const inFlight = () => $(
    '#messages [data-role="assistant"]:is([data-status="pending"], [data-status="streaming"])');

  const isEditable = (el) => el instanceof HTMLElement &&
    (el.isContentEditable || el.matches('input, textarea, select'));

  // --- Composer ---------------------------------------------------------------

  // autosize fits the textarea to its text, up to MAX_ROWS lines.
  function autosize() {
    const box = input();
    if (!box) return;
    const style = getComputedStyle(box);
    const line = parseFloat(style.lineHeight) || parseFloat(style.fontSize) * 1.5;
    const frame = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom) +
      parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth);
    const max = line * MAX_ROWS + frame;
    box.style.height = 'auto';
    const wanted = box.scrollHeight + parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth);
    box.style.height = `${Math.min(wanted, max)}px`;
    box.style.overflowY = wanted > max ? 'auto' : 'hidden';
  }

  // Enter sends, Shift+Enter adds a line. Enter that confirms an IME candidate
  // (keyCode 229 in Safari) is left alone.
  document.addEventListener('keydown', (event) => {
    const box = input();
    if (box && event.target === box) {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && event.keyCode !== 229) {
        event.preventDefault();
        if (!inFlight() && box.value.trim() !== '') box.form.requestSubmit();
      }
      return;
    }
    if (event.key === '/' && !event.ctrlKey && !event.metaKey && !event.altKey && !isEditable(event.target)) {
      if (box) {
        event.preventDefault();
        box.focus();
      }
    }
  });

  document.addEventListener('input', (event) => {
    if (event.target === input()) autosize();
  });

  // After the server accepted the message, the composer starts fresh and the
  // conversation follows the new turn. A refusal (422, 409) keeps the text.
  document.body.addEventListener('htmx:beforeRequest', (event) => {
    if (event.target === form()) forceFollow = true;
  });
  document.body.addEventListener('htmx:afterRequest', (event) => {
    if (event.target !== form()) return;
    const status = event.detail.xhr ? event.detail.xhr.status : 0;
    if (status >= 200 && status < 300) {
      input().value = '';
      autosize();
    } else {
      forceFollow = false; // nothing was added, so nothing to bring into view
    }
  });

  // --- Requests that fail without an answer to show -----------------------------

  // The server explains a refusal itself (409, 422, 429 are swapped in). For a
  // dropped network, a timeout or any other error status htmx swaps nothing, so
  // without this the page would look dead. The composer's alert line is always
  // there, and screen readers hear it.
  const TROUBLE = {
    403: 'That request was blocked. Reload the page and try again.',
    404: 'That chat is gone. Reload the page.',
    413: 'That message is too big to send. Shorten it and try again.',
  };

  function say(message) {
    const line = $('#composer-error');
    if (line) line.textContent = message;
  }

  document.body.addEventListener('htmx:responseError', (event) => {
    const status = event.detail.xhr ? event.detail.xhr.status : 0;
    say(TROUBLE[status] || (status >= 500
      ? 'The server ran into a problem. Try again in a moment.'
      : "That didn't work. Reload the page and try again."));
  });
  document.body.addEventListener('htmx:sendError', () => {
    say("Couldn't reach the server. Check your connection and try again.");
  });
  document.body.addEventListener('htmx:timeout', () => {
    say('The server took too long to answer. Try again.');
  });

  // --- Streaming state --------------------------------------------------------

  let wasStreaming = false;

  // syncStreaming makes the composer match the page: while a reply is in
  // flight, Send is off and Stop is on; when it ends, the composer is ready.
  function syncStreaming() {
    const streaming = inFlight() !== null;
    const send = sendButton();
    const stop = stopButton();
    if (send) send.disabled = streaming;
    if (stop) {
      stop.hidden = !streaming;
      if (!streaming) stop.disabled = false;
    }
    const log = scroller();
    if (log) log.setAttribute('aria-busy', String(streaming));
    if (wasStreaming && !streaming) {
      announceOutcome();
      const active = document.activeElement;
      const box = input();
      if (box && (active === document.body || (form() && form().contains(active)) || active === stop)) {
        box.focus({ preventScroll: true });
      }
    }
    wasStreaming = streaming;
  }

  // announce says something to screen readers, politely and once.
  function announce(message) {
    const region = $('#live-status');
    if (!region) return;
    region.textContent = '';
    window.setTimeout(() => { region.textContent = message; }, 50);
  }

  const OUTCOMES = { complete: 'Reply complete.', stopped: 'Reply stopped.', failed: 'Reply failed.' };

  // announceOutcome is called once, when a reply stops being in flight.
  function announceOutcome() {
    const replies = document.querySelectorAll('#messages [data-role="assistant"]');
    const last = replies[replies.length - 1];
    const message = last && OUTCOMES[last.dataset.status];
    if (message) announce(message);
  }

  // Stop asks the server to end the reply. The answer is the reply as saved,
  // which replaces the streaming one and so closes its connection.
  function stopStreaming() {
    const article = inFlight();
    const reply = article && article.closest('.reply[data-stop-url]');
    const stop = stopButton();
    if (!reply || !stop || typeof htmx === 'undefined') return;
    stop.disabled = true; // one press is enough
    // htmx settles this for every status, not only for a dropped network. If the
    // reply ended, the page hides the button; if not, the press failed, and it can
    // be tried again.
    const settled = () => { stop.disabled = false; };
    htmx.ajax('POST', reply.dataset.stopUrl, { target: reply, swap: 'outerHTML' }).then(settled, settled);
  }

  // --- Autoscroll -------------------------------------------------------------
  //
  // The reader follows the conversation while they are at the end of it: new
  // text scrolls into view. Once they scroll up to read, the view stays put and
  // "Jump to latest" appears. Sending a message always brings the view back.
  //
  // An update makes the list taller before we get to look, so "at the end" is
  // judged against the height seen last time. The live scrollTop is used rather
  // than the last scroll event, which may not have been delivered yet.

  let following = true;
  let forceFollow = false; // the reader just sent something
  let seenHeight = 0; // the list's height when we last looked

  const atEnd = (log, height) => height - log.scrollTop - log.clientHeight <= NEAR_BOTTOM_PX;

  function scrollToEnd() {
    const log = scroller();
    if (log) log.scrollTop = log.scrollHeight;
  }

  function syncJumpButton() {
    const jump = jumpButton();
    if (jump) jump.hidden = following;
  }

  // onListChanged runs after the list gains or changes content.
  function onListChanged() {
    const log = scroller();
    syncStreaming();
    decorateCode();
    following = forceFollow || atEnd(log, seenHeight);
    forceFollow = false;
    if (following) scrollToEnd();
    seenHeight = log.scrollHeight;
    syncJumpButton();
  }

  function onScroll() {
    const log = scroller();
    seenHeight = log.scrollHeight;
    following = atEnd(log, seenHeight);
    syncJumpButton();
  }

  // --- Code blocks: copy -------------------------------------------------------

  // decorateCode puts a Copy button in each code block of a finished reply. The
  // server wraps every block in div.code-block. It waits for the end because a
  // streaming reply is replaced on every update.
  function decorateCode() {
    const blocks = document.querySelectorAll(
      '#messages [data-role="assistant"]:not([data-status="pending"]):not([data-status="streaming"]) .message-body .code-block');
    for (const block of blocks) {
      if (block.querySelector('.copy-code')) continue;
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'btn btn-sm btn-outline-secondary copy-code';
      button.dataset.testid = 'copy-code';
      button.setAttribute('aria-label', 'Copy code');
      button.textContent = 'Copy';
      block.append(button);
    }
  }

  // copyText uses the Clipboard API where the page may (https, localhost) and
  // the older way where it may not.
  async function copyText(text) {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        return true;
      }
    } catch {
      // fall through to the older way
    }
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.className = 'visually-hidden';
    document.body.append(area);
    area.select();
    let copied = false;
    try {
      copied = document.execCommand('copy');
    } catch {
      copied = false;
    }
    area.remove();
    return copied;
  }

  // copyMessage copies a reply as it is stored (Markdown), fetched from the
  // server, not the page's rendering of it.
  async function copyMessage(button) {
    let copied = false;
    try {
      const response = await fetch(button.dataset.rawUrl, { credentials: 'same-origin' });
      // The reply's text is plain text. Anything else that answers 200, such as
      // a sign-in page from a proxy, is not the reply.
      const plain = (response.headers.get('Content-Type') || '').startsWith('text/plain');
      if (response.ok && plain) copied = await copyText(await response.text());
    } catch {
      copied = false;
    }
    button.textContent = copied ? 'Copied' : 'Copy failed';
    announce(copied ? 'Reply copied.' : 'Copying failed.');
    window.setTimeout(() => { button.textContent = 'Copy'; }, 2000);
  }

  async function copyCode(button) {
    const pre = button.parentElement.querySelector('pre');
    const text = pre ? pre.textContent.replace(/\n$/, '') : '';
    const copied = await copyText(text);
    button.textContent = copied ? 'Copied' : 'Copy failed';
    announce(copied ? 'Code copied.' : 'Copying failed.');
    window.setTimeout(() => { button.textContent = 'Copy'; }, 2000);
  }

  // --- Sidebar: rename and delete ---------------------------------------------

  // A rename field takes focus with its text selected, so typing replaces the
  // title; Escape cancels. htmx may already have focused the field (autofocus),
  // so what matters is that this field is new, not whether it has focus.
  let renameField = null;
  document.body.addEventListener('htmx:afterSettle', () => {
    const field = $('[data-testid="rename-input"]');
    if (field && field !== renameField) {
      field.focus();
      field.select();
    }
    renameField = field;
  });

  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && event.target instanceof Element && event.target.matches('[data-testid="rename-input"]')) {
      const cancel = event.target.closest('form').querySelector('[data-testid="rename-cancel"]');
      if (cancel) cancel.click();
    }
  });

  // The delete confirmation arrives as HTML in #modal-root. Show it, and clean
  // up once it is closed, handing focus back to where the reader was.
  document.body.addEventListener('htmx:afterSwap', (event) => {
    if (!(event.target instanceof Element) || event.target.id !== 'modal-root' || typeof bootstrap === 'undefined') return;
    const dialog = event.target.querySelector('.modal');
    if (!dialog) return;
    const opener = document.activeElement;
    // On a small screen the list is a drawer; it makes way for the dialog. Focus
    // leaves it first: a hidden drawer must not hold focus.
    const drawer = document.getElementById('sidebar');
    const drawerWasOpen = drawer && drawer.classList.contains('show');
    if (drawerWasOpen) {
      if (drawer.contains(document.activeElement)) document.activeElement.blur();
      bootstrap.Offcanvas.getInstance(drawer)?.hide();
    }
    // Bootstrap marks the dialog aria-hidden once it has faded out, and Chrome
    // warns when that lands on something that still holds focus. A blur (unlike
    // a move) does not set off the focus trap.
    dialog.addEventListener('hide.bs.modal', () => {
      if (dialog.contains(document.activeElement)) document.activeElement.blur();
    }, { once: true });
    // Focus goes back once the dialog has finished hiding: until then the focus
    // trap pulls it back inside.
    dialog.addEventListener('hidden.bs.modal', () => {
      bootstrap.Modal.getInstance(dialog)?.dispose();
      event.target.replaceChildren();
      const toggle = $('[data-testid="sidebar-toggle"]');
      for (const candidate of [opener, drawerWasOpen ? toggle : null, input()]) {
        if (!(candidate instanceof HTMLElement) || !candidate.isConnected) continue;
        candidate.focus({ preventScroll: true });
        if (document.activeElement === candidate) return;
      }
    }, { once: true });
    bootstrap.Modal.getOrCreateInstance(dialog).show();
  });

  // --- Wiring -----------------------------------------------------------------

  document.addEventListener('click', (event) => {
    if (!(event.target instanceof Element)) return;
    const copy = event.target.closest('.copy-code');
    const copyReply = event.target.closest('[data-testid="copy-message"]');
    if (copy) {
      copyCode(copy);
    } else if (copyReply) {
      copyMessage(copyReply);
    } else if (event.target.closest('#composer [data-testid="stop"]')) {
      stopStreaming();
    } else if (event.target.closest('#jump-latest')) {
      following = true;
      scrollToEnd();
      syncJumpButton();
    }
  });

  // The list the observers are watching. A boosted page swap (starting a chat)
  // brings a new one, so binding is done again then.
  let bound = null;
  let observers = [];

  function bind() {
    const log = scroller();
    if (log === bound) return;
    for (const observer of observers) observer.disconnect();
    observers = [];
    bound = log;
    if (!log) return; // not a chat page
    log.addEventListener('scroll', onScroll, { passive: true });
    const changes = new MutationObserver(onListChanged);
    changes.observe(log, {
      childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['data-status'],
    });
    // The list's own size changes when the composer grows or the window resizes.
    const resizes = new ResizeObserver(() => { if (following) scrollToEnd(); });
    resizes.observe(log);
    observers.push(changes, resizes);
    following = true;
    forceFollow = false;
    decorateCode();
    scrollToEnd();
    seenHeight = log.scrollHeight;
    wasStreaming = false;
    syncStreaming();
    autosize();
  }

  document.body.addEventListener('htmx:afterSettle', bind);
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bind);
  } else {
    bind();
  }
})();
