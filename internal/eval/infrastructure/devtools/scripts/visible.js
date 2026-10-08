// The text a reader can see in a reply body. Decorative parts (the typing
// indicator) are marked aria-hidden and do not count: a reply has started when
// there is text, not when there is a spinner. Every script that counts the text
// of a reply counts it this way.
const visibleText = (body) => {
  if (!body.querySelector('[aria-hidden=true]')) return body.innerText;
  const copy = body.cloneNode(true);
  copy.querySelectorAll('[aria-hidden=true]').forEach((n) => n.remove());
  // innerText of a detached node ignores layout, so read it the plain way.
  return copy.textContent.trim();
};
