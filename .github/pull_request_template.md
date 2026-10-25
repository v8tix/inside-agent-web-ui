<!--
Title: "<prefix>: <what changed>", at most 80 characters. Prefixes: feat, fix, docs,
chore, breaking, refactor, test, ci. Check it before you open the pull request:
  scripts/pr-check.sh --title "fix: stop dropping the partial reply"
Delete these comments, and any section that does not apply.
-->

## Type of change
<!-- Pick the one matching the title's prefix. -->

- [ ] 🍕 feat — new functionality
- [ ] 🐛 fix — resolves a bug
- [ ] 📝 docs — documentation only
- [ ] 📦 chore — tooling, dependencies, release prep
- [ ] 💥 breaking — changes existing behavior
- [ ] 🧑‍💻 refactor — structural change, no behavior change
- [ ] ✅ test — adding or fixing tests
- [ ] 🔁 ci — CI/CD pipeline or workflow

## What is this?
<!-- One or two sentences on what changed and why — what it does, not which files:
"Stop now cancels the agent's run" reads better than "edited chat_handler.go". -->

## Why now?
<!-- The reason and the context: the decision behind it, what was tried and dropped. Use
`Fixes #123` (or `Closes #123`) if there is a GitHub issue, so it closes on merge;
otherwise link it and copy in the part that matters, since links go stale. Someone who
was not there should be able to follow. -->

## How does it work?
<!-- The flow in a few steps, or "no change in behavior". -->

## How to try it
<!-- Steps or commands a reviewer can run, and what they should see. -->

## Before and after
<!-- Screenshots, output, or a short diff of behavior. Delete if nothing visible changed. -->

## Risk
<!-- Could this break something that works today? What did you do about it? -->

## Checklist
- [ ] `scripts/check.sh` passes (and `scripts/check.sh --it` when the change touches the UI, the agent adapter, the scripts or Compose)
- [ ] Tests are new or changed, table-driven, and fail without the change
- [ ] A new or changed setting is read from `.env` and written down in `.env.example`
- [ ] The docs match: `README.md` when a user would notice
- [ ] No secrets, `.env` values, GAIA data, answers or results in the diff, the commits or the logs
- [ ] One thing, under 500 changed lines and 20 files (`scripts/pr-check.sh`), or the reason it is bigger is in the notes
- [ ] The title has a prefix and is at most 80 characters

## Notes
<!-- Where to start reading, what to skip, what is left for another pull request.
If an AI assistant wrote or reviewed part of this: which part, and how a person checked it. -->
