# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## General Rules

When asked to plan, analyze, or create a ticket, do NOT start implementation unless explicitly asked. Planning and implementation are separate steps.

When investigating issues, do NOT fabricate answers. If search results or tool output don't contain the answer, say so honestly. Never fill in gaps from training knowledge when the user is asking about live/runtime data.

## Build & Test

```bash
make build          # Build binary to ./kontora
make install        # go install ./cmd/kontora
make test           # go test -timeout 5m ./...
make test-race      # go test -race -timeout 5m ./...
make lint           # golangci-lint + go mod tidy -diff + govulncheck + deadcode
make fmt            # golangci-lint fmt (gofmt + goimports)
make css            # Rebuild static/app.css after changing Tailwind classes
make assets         # Re-download vendored web assets + rebuild app.css
```

Single package: `go test ./internal/process/...`
Single test: `go test ./internal/pipeline/... -run TestEvaluate/advance_on_exit_0`

## Architecture

Kontora is an agent orchestration daemon that coordinates AI coding agents through multi-stage pipelines, managing ticket lifecycle with retries, rollbacks, and git worktree isolation. Inspired by [wedow/ticket](https://github.com/wedow/ticket) — tickets are markdown files, no database.

### How the pieces fit together

`cmd/kontora/main.go` dispatches on `os.Args[1]` (no framework, just stdlib `flag`). All commands are flat top-level verbs.

**Config** (`internal/config`) — YAML with `KnownFields(true)` (unknown fields rejected). Paths store `~` literally; tilde expansion is deferred to the daemon/CLI layer.

**Tickets** (`internal/ticket`) — markdown files with YAML frontmatter. Uses `yaml.Node` for round-trip preservation — the daemon updates status/stage/branch without clobbering user-added custom fields or field order.

**Pipeline engine** (`internal/pipeline`) — pure state machine: `Evaluate(ticket, pipeline, event) → Action`. No side effects — all state changes are expressed as field updates in the returned Action.

**Daemon** (`internal/daemon`) — acquires a file lock (single instance), recovers from crashes (resets `in_progress` → `todo`, cleans orphaned tmux windows), schedules via FIFO min-heap bounded by semaphore. Self-write tracking (`selfWrites`) skips its own file change events from the watcher.

**Runner abstraction** — `RunnerFunc` injected via `WithRunner()`. Production uses `tmuxRunner` (detached tmux sessions). Tests use `DirectRunner` (wraps `process.Run`).

**Inter-stage communication** — stages share a git worktree. Artifacts are passed as files (e.g., plan stage writes `PLAN.md`, code stage reads it via `{{ file "PLAN.md" }}` in its prompt template).

## Web UI assets

The web UI (`internal/web/static/`) is a single-page Alpine app served by the daemon. All third-party libraries and fonts are self-hosted and embedded into the binary via `//go:embed static` (`internal/web/server.go`), so the page makes no external CDN requests at runtime. This keeps the localhost UI fast and fully offline.

- **Vendored libs** live under `static/vendor/<name>@<version>/`. The version sits in the path so a bump is an obvious diff. `static/index.html` (and the xterm dynamic `import()`s in `static/app.js`) reference these local paths.
- **Tailwind is precompiled**, not loaded from the Play CDN. `make css` runs the standalone Tailwind CLI against `hack/tailwind.config.js` and `hack/tailwind.css` and writes the committed `static/app.css`. The browser does no runtime JIT compile. Run `make css` after changing Tailwind classes in `index.html`/`app.js`, otherwise the new classes have no styles.
- **Fonts** (DM Sans, JetBrains Mono) are self-hosted woff2 under `static/vendor/fonts/`, wired up with local `@font-face` in `fonts.css`.

Build inputs (Tailwind config, vendoring scripts) live in `hack/`; the downloaded Tailwind CLI is written to `bin/` (gitignored). To bump a version: edit `hack/vendor-assets.sh` (or `hack/build-css.sh` for the Tailwind CLI), run `make assets` to re-download and rebuild, then update the matching `/vendor/<name>@<version>/` paths in `index.html`/`app.js`.

These regeneration steps are NOT part of `go build`: the outputs are committed and embedded, so `go build` and `go install` stay offline and reproducible.

## Go Version

This project uses Go 1.26. Notable language features:
- `new(value)` is valid — returns a pointer to the given value (e.g., `new(true)` returns `*bool`).

## Git Operations

After resolving merge/rebase conflicts, always do a second pass grep for leftover conflict markers (<<<<<<, >>>>>>, ======) before committing.

## Workflow

When working on ticket workflows, follow the sequence: plan → create ticket → refine ticket → STOP. Do not proceed to implementation without explicit user instruction.

## Conventions

### Tests
- Use table test cases pattern as much as possible.
- Prefer high-level tests that cover behaviour over testing implementation details.
- Daemon tests use `testHarness` (`newHarness(t)` → `h.startDaemon(...)`) with `DirectRunner` to avoid tmux dependency.

### Code
- Leaf packages (`process`, `worktree`, `prompt`) accept primitives, not config types — the daemon wires them.
