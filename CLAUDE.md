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
make test-scripts   # shell tests for the hack/ changelog scripts
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

**Daemon** (`internal/daemon`) — acquires a file lock (single instance), recovers from crashes (resets `in_progress` → `todo`, cleans tmux windows orphaned by its own previous run), schedules via FIFO min-heap bounded by semaphore. Self-write tracking (`selfWrites`) skips its own file change events from the watcher.

**Runner abstraction** — `RunnerFunc` injected via `WithRunner()`. Production uses `tmuxRunner` (detached tmux sessions). Tests use `DirectRunner` (wraps `process.Run`).

**Inter-stage communication** — stages share a git worktree. Artifacts are passed as files (e.g., plan stage writes `PLAN.md`, code stage reads it via `{{ file "PLAN.md" }}` in its prompt template).

## Web UI assets

The web UI is a single-page Alpine app served by the daemon: markup and vendored assets in `internal/web/static/`, component sources in `internal/web/ui/`. Both are embedded into the binary (`//go:embed static`, `//go:embed ui` in `internal/web/server.go`), so the page makes no external CDN requests at runtime. This keeps the localhost UI fast and fully offline.

- **The JS is bundled, not served file by file.** `internal/web/ui/*.js` are ES modules; `bundle.go` compiles them with esbuild (linked as a Go library, so `go build` stays offline) into one IIFE that `buildAssets` inserts as the synthetic `/app.js`. Nothing under `ui/` is served raw. The output must stay an IIFE: `index.html` loads `/app.js` as a plain non-deferred tag and Alpine is `defer`, so a `type="module"` bundle would run after Alpine started. The bundle carries an inline sourcemap, so a browser stack trace points back at the module it came from. Set `KONTORA_WEB_DIR=<repo root>` to serve the UI from that working copy on every request instead of from the embed: `/app.js` is recompiled, and every other asset is re-read from `internal/web/static/`, so a markup edit or a `make css` run shows up on the same reload. Only names the embedded table already carries are overlaid. A dev build that fails answers 500 and writes the esbuild message to the daemon log.
- **The component is a set of mixins.** Each `ui/*.js` exports a factory returning part of one flat Alpine object; `ui/index.js` merges them and assigns `kontora`, `termState` and `statsDerive` to `globalThis`. No two mixins may define the same key — `merge()` throws on a repeat, and `testdata/ui_mixins.test.mjs` names both owners. A key repeated inside one mixin is an esbuild warning, which fails the build.
- **Vendored libs** live under `static/vendor/<name>@<version>/`. The version sits in the path so a bump is an obvious diff. `static/index.html` and the dynamic `import()`s in `ui/terminal.js` (xterm) and `ui/settings.js` (yaml) reference these local paths. Only a dynamic `import()` reaches them: `bundle.go` rejects a static one, which esbuild would compile into a `__require()` call that throws in the browser, and it rejects a path with no embedded file behind it.
- **Tailwind is precompiled**, not loaded from the Play CDN. `make css` runs the standalone Tailwind CLI against `hack/tailwind.config.js` and `hack/tailwind.css` and writes the committed `static/app.css`. The browser does no runtime JIT compile. Run `make css` after changing Tailwind classes in `index.html` or `ui/`, otherwise the new classes have no styles.
- **Fonts** (DM Sans, JetBrains Mono) are self-hosted woff2 under `static/vendor/fonts/`, wired up with local `@font-face` in `fonts.css`.

Build inputs (Tailwind config, vendoring scripts) live in `hack/`; the downloaded Tailwind CLI is written to `bin/` (gitignored). To bump a version: edit `hack/vendor-assets.sh` (or `hack/build-css.sh` for the Tailwind CLI), run `make assets` to re-download and rebuild, then update the matching `/vendor/<name>@<version>/` paths in `index.html`, `ui/terminal.js` and `ui/settings.js`.

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
- Daemon tests use `testHarness`: `newHarness(t)` → `h.newDaemon(cfg, opts...)` → `go func() { errCh <- d.Run(ctx) }()`, with `DirectRunner` to avoid a tmux dependency. End with `cancel(); require.NoError(t, <-errCh)`.
- To assert on metrics, use `h.newMetricsDaemon(cfg)`, which injects a `ManualReader`. Collect only after the daemon has stopped; reading while it runs races the goroutines still recording.

### Code
- Leaf packages (`process`, `worktree`, `prompt`) accept primitives, not config types — the daemon wires them.
