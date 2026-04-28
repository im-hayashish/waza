# Decision: New feature types must update all onboarding entry points

**By:** Saul (Documentation Lead)
**Date:** 2025-07-15

**What:** When a new evaluation target type is added (like `.agent.md` in #226), the following entry points must all be checked and updated:
- `site/src/content/docs/quick-start.mdx`
- `site/src/content/docs/getting-started.mdx`
- `docs/GETTING-STARTED.md`
- `docs/GUIDE.md`
- `docs/TUTORIAL.md`
- `examples/README.md`

A dedicated guide page is not sufficient — users discover features through onboarding paths, not just reference docs.

**Why:** The #226 custom agent PR correctly added a dedicated guide and updated 5 files, but missed 6 onboarding entry points. Users reading quick-start or getting-started would not learn that `.agent.md` is a valid target. This audit pattern should be part of the Documentation Impact Matrix.
