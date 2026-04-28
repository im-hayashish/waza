# History — Saul

## Project Context
- **Project:** waza — CLI tool for evaluating Agent Skills
- **Stack:** Go (primary), React 19 + Tailwind CSS v4 (web UI)
- **User:** Shayne Boyer (spboyer)
- **Repo:** microsoft/waza
- **Universe:** The Usual Suspects

## Key Learnings

### Documentation Standards
- **Style guide:** TBD (to be established)
- **Markdown:** Consistent code block formatting, link structure
- **Versioning:** Track docs alongside code (update in same PR)
- **API docs:** Follow Go conventions (exported functions documented)

### Documentation Scope
- README (main entry point)
- docs/ directory (detailed guides)
- waza-go/README.md (Go implementation)
- Inline code comments (for complex logic)
- CHANGELOG.md (release tracking)

### Quality Gates
- All PRs must update relevant docs
- Livingston and Saul review doc changes
- Style consistency checked before merge

### Doc-Freshness Reviews (Added in #256)
- **Doc-review gate** triggered by changes to `cmd/waza/`, `internal/`, or `web/src/`
- **Doc-consistency gate** triggered by changes to `docs/`, `README.md`, `DEMO-SCRIPT.md`
- Saul now owns ongoing doc-freshness verification across all code PRs
- Documentation Impact Matrix maps code paths to required doc updates
- Screenshot maintenance automated via Playwright E2E tests in `web/`

📌 Team update (2026-02-19): Screenshot conventions formalized (viewport, paths, naming, mock reuse) — decided by Basher (#251)


## 📌 Team update (2026-02-20): Model policy overhaul

All code roles now use `claude-opus-4.6`. Docs/Scribe/diversity use `gemini-3-pro-preview`. Heavy code gen uses `gpt-5.2-codex`. Decided by Scott Boyer. See decisions.md for full details.

### Releases Page (#383, PR #384)

- Created `site/src/content/docs/reference/releases.mdx` — Starlight MDX format with Tabs component for install commands
- Used existing reference pages (cli.mdx, schema.mdx) as style baseline: frontmatter with title+description, horizontal rules between sections, consistent table formatting
- Changelog highlights extracted from CHANGELOG.md v0.8.0 — grouped by Added/Changed/Fixed, condensed to one-line summaries
- Download table covers all 6 platform binaries with absolute GitHub release URLs
- azd extension section includes full install command with versioned tar.gz URL
- Sidebar updated in astro.config.mjs under Reference group
- Pattern: release pages should link to GitHub Releases for history rather than duplicating old changelogs

### Cross-Reference Audit (#222/#226/#228)

- **#222 rename was thorough** — no stale `BenchmarkSpec`/`TestRunner` references found in any auditable docs. The rename PR updated all code-facing references correctly.
- **#226 custom agent coverage** had gaps in onboarding paths — quick-start, getting-started (both site and docs/), GUIDE, TUTORIAL, and examples/README all lacked `.agent.md` mentions. Pattern: when adding a new target type, check all "getting started" entry points, not just the feature-specific guide.
- **#228 mock engine** — the mock description in INTEGRATION-TESTING.md and eval-yaml.mdx was outdated (said "Simulates responses" when it now echoes task metadata and file content). Pattern: executor behavior changes need doc updates in the executor reference table.
- **Design docs** (`docs/design/194-baseline-skill-impact.md`) still reference `BenchmarkSpec`/`TestRunner` — correct, these are historical point-in-time design documents.
- **examples/README.md** was missing 3 example directories that had been added over time (custom-agent, required-skills-demo, rubrics).
