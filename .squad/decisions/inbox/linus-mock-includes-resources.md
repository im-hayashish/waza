# Decision: Mock engine echoes full request context

**Date:** 2026-02-22
**Author:** Linus (Backend Developer)
**Issue:** #227

## Context

The `Run Waza Evaluation` CI job runs `examples/code-explainer/eval.yaml` with the mock engine. The mock returned only `"Mock response for: <prompt>"` plus a file count — a near-empty stub. This made every `_output_contains` grader fail because expected substrings (e.g., "async", "fetch", "recursive") never appeared in the output.

## Decision

The mock engine now echoes all available request data in its response:

1. **Task name and description** — via new `TaskName`/`TaskDescription` fields on `ExecutionRequest`
2. **Context metadata** — key/value pairs from the task's `context:` block
3. **File paths and content** — each resource file's path and up to 1KB of content

This makes the mock a "saw the files" echo agent rather than a no-op stub. The CI eval pipeline can now validate the full path (discovery → execution → grading) without a real model.

## Trade-offs

- **Pro:** CI evals pass without a real model; eval authors can write realistic `output_contains` expectations
- **Pro:** No changes needed to example eval files — the mock adapts
- **Con:** Mock output is now coupled to request shape; adding new `ExecutionRequest` fields may need mock updates
- **Con:** Content preview capped at 1KB per file — very large fixture tests may still need adjustment

## Alternatives considered

- **Relax eval expectations:** Rejected — the issue said "fix the mock, not the example"
- **Only echo file content:** Insufficient — some expected strings (e.g., "recursive") come from task descriptions, not file content
