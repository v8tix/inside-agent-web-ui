# Vendored frontend files

Pinned versions, licenses, and the SHA-256 of each file exactly as served. A test
(`TestVendoredFilesMatchVersions`) fails if a file and this table disagree.
Directory names carry the version, so these URLs can be cached as immutable.

To upgrade: add the new version directory, update this table and the template
references together, and delete the old directory. Re-verify the version on the
npm registry first; stay on htmx 2.x (htmx 4 is still pre-release).

| File | Package | License | SHA-256 | Notes |
|------|---------|---------|---------|-------|
| bootstrap-5.3.8/bootstrap.min.css | bootstrap 5.3.8 | MIT | 8f8173cb2d8f867274aeb0cb15328e60f490c7f272351e51a55f1dabb486e4ff | `sourceMappingURL` comment removed (original d85327d99c7a3ee1f9b5d0500d1370acea3ad2db39c163c2f51f232baedbdede) |
| bootstrap-5.3.8/bootstrap.bundle.min.js | bootstrap 5.3.8 | MIT | c6f670216aedd2c61ec83102f7e40c1c44114eecd7f37b2edc332757390c386a | `sourceMappingURL` comment removed (original e4fd49181388c48ec5040bd3fe66f57c29c8e67fcd8502b3354b96ec7ab47cc7) |
| htmx-2.0.11/htmx.min.js | htmx.org 2.0.11 | 0BSD | d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717 | unmodified |
| htmx-ext-sse-2.2.4/sse.min.js | htmx-ext-sse 2.2.4 | see LICENSE | 98a46496de0c3605fbffdce9167ba427bdd9553184f83f149c261891a92c0136 | unmodified |

Why the source map comments were removed: browsers and DevTools fetch the `.map`
file, which would show up as a failed request in the evaluator's network checks.

htmx contains `eval` and `new Function` code paths. They stay unused because the
page sets `allowEval: false` (see `base.html`), which matters under our CSP
(`script-src 'self'`, no `unsafe-eval`).
