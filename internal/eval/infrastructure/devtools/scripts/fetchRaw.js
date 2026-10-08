// eval:fetchRaw
// Reads a reply as it is stored, through the endpoint the Copy button uses,
// with the session the page already has.
async () => {
  const r = await fetch(__ARG__, {credentials: 'same-origin', headers: {Accept: 'text/plain'}});
  return {status: r.status, text: await r.text()};
}
