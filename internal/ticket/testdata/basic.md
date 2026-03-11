---
id: kon-q88f
status: open
deps: []
created: 2026-02-25T19:39:45Z
type: task
pipeline: default
path: ~/projects/kontora
role: code
attempt: 1
started_at: 2026-02-25T20:00:00Z
branch: kon-q88f-work
---
# Fix the search index

The search index is broken when using special characters.

## Plan

Replace the naive string matching with proper escaping.
