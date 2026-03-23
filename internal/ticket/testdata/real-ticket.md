---
id: kon-zsqa
status: in_progress
deps: []
created: 2026-03-05T23:29:44Z
type: task
parent: kon-epic
---
# [kontora] Phase 1: Config + Task Parsing

## Goal

Scaffold the Go project and deliver library-only config parsing, ticket parsing with YAML frontmatter round-trip preservation, and pipeline evaluation with tests, without daemon code.

## Phases

- [ ] Phase 1: Project scaffold - create the module layout, placeholder binary, and shared build/lint tooling.
- [ ] Phase 2: Config parser - implement typed config loading, defaults, validation, and fixture-backed unit tests.
- [ ] Phase 3: Ticket parser - implement frontmatter parsing, `yaml.Node` round-trip mutation, ticket helpers, and unit tests.
- [ ] Phase 4: Pipeline evaluation - implement pure state machine for stage transitions with unit tests.
- [ ] Phase 5: Integration and verification - add a real-ticket round-trip test and finish with build/test/lint cleanup.

## Tasks

### Phase 1: Project scaffold
Files: Create `go.mod`, `cmd/kontora/main.go`, `Makefile`, `.golangci.yml`, `.gitignore`
- [ ] Initialize module `github.com/worksonmyai/kontora` with Go 1.26 and add tool dependencies for `govulncheck` and `deadcode`.
- [ ] Create `cmd/kontora/main.go` as a placeholder binary that prints version.
- [ ] Copy and adapt the `Makefile` from kontora; remove `fts5` tags and the `eval-test` target, and keep `build`, `test`, `test-race`, `lint`, `fmt`, `install`, and `clean`.
- [ ] Copy and adapt `.golangci.yml` from kontora; remove kontora-specific `treesitter` and `embed` exclusions.
- [ ] Add `.gitignore` for the new Go project layout.
- [ ] Verify `make build` and `make lint` pass on the scaffold before moving on.

### Phase 2: Config parser
Files: Create `internal/config/config.go`, `internal/config/config_test.go`, `internal/config/testdata/*.yaml`
- [ ] Create YAML fixtures in `internal/config/testdata/` for valid, minimal, and invalid configuration scenarios.
- [ ] Write `internal/config/config_test.go` cases for valid config, minimal config with defaults, unknown skill reference, unknown agent reference, `back` on the first stage, invalid `on_success`, invalid `on_failure`, missing `tasks_dir`, missing agent `binary`, duration parsing, file not found, and malformed YAML.
- [ ] Define `Config`, `Repo`, `Agent`, `AgentMode`, `Skill`, `Pipeline`, `Stage`, and `WebConfig`.
- [ ] Implement the custom `Duration` type with `yaml.Unmarshaler` using `time.ParseDuration`.
- [ ] Implement `Load(path)` and `LoadReader(io.Reader)`.
- [ ] Implement `Validate()` with referential integrity checks and defaults for `base_branch`, `max_concurrent_agents`, and `web.port`.
- [ ] Run the phase test suite and keep it green before the next phase.

### Phase 3: Ticket parser
Files: Create `internal/ticket/ticket.go`, `internal/ticket/ticket_test.go`, `internal/ticket/testdata/*.md`
- [ ] Create markdown fixtures in `internal/ticket/testdata/` for basic parse, minimal ticket, unknown fields, field order preservation, timestamp formats, history, empty body, `---` in body, and empty `deps: []`.
- [ ] Write `internal/ticket/ticket_test.go` cases for basic parse, minimal ticket, unknown field round-trip, field order preservation, body byte identity after round-trip, both timestamp formats (`Z` and `+01:00` with microseconds), statuses, no frontmatter error, empty body, `---` in body, `SetField` existing/new/preserves-others, marshal with history, title extraction, and empty `deps: []` round-trip.
- [ ] Implement frontmatter splitting to handle `\n`, `\r\n`, and only the first two `---` delimiters.
- [ ] Define `Ticket`, `Status`, `HistoryEntry` types.
- [ ] Implement `ParseFile`, `Parse`, and `ParseBytes` using a `yaml.Node` tree for typed access plus round-trip preservation.
- [ ] Implement `Marshal` to re-encode the patched `yaml.Node` and rejoin the original body without modifying it.
- [ ] Implement `SetField` to walk the mapping node, replace existing keys, or append new keys.
- [ ] Implement `Title()` to return the first `# heading` from the body.
- [ ] Run the phase test suite and keep it green before the next phase.

### Phase 4: Pipeline evaluation
Files: Create `internal/pipeline/evaluate.go`, `internal/pipeline/evaluate_test.go`
- [ ] Write `internal/pipeline/evaluate_test.go` cases for advance on success, retry on failure, back on failure, pause on exhausted retries, and done on last stage.
- [ ] Implement `Evaluate(ticket, pipeline, event) → Action` as a pure state machine with no side effects.
- [ ] Run the phase test suite and keep it green before the next phase.

### Phase 5: Integration and verification
Files: Create `internal/ticket/integration_test.go`, Create `internal/ticket/testdata/<real-ticket>.md`
- [ ] Copy a real ticket file into `internal/ticket/testdata/<real-ticket>.md`.
- [ ] Write `internal/ticket/integration_test.go` to round-trip `ParseFile -> SetField(status change) -> Marshal -> ParseBytes` and verify unknown fields are preserved and the body is byte-identical.
- [ ] Run `make test` and fix any remaining test failures.
- [ ] Run `make lint` and fix any remaining lint, vulnerability, dead code, or tidy issues.
- [ ] Run `make build` and confirm the `kontora` binary is produced.

## Acceptance Criteria

- `make build` produces a `kontora` binary from `cmd/kontora`.
- `make test` passes for `internal/config`, `internal/ticket`, `internal/pipeline`, and the real-ticket integration test.
- `make lint` passes with `golangci-lint`, `govulncheck`, `deadcode`, and `go mod tidy -diff`.
- Ticket frontmatter round-trips unknown YAML fields without loss and preserves the markdown body byte-for-byte except for intentional `SetField` updates.

## Open Questions

- Which real ticket file should be copied into `internal/task/testdata/` for the integration test, and what source path should be treated as canonical?

## Plan

# Phase 1: Config + Task Parsing

## Context

Kontora is a new Go project (empty repo). Phase 1 builds the foundation: config parsing, task file parsing with round-trip preservation, smart stage detection, and repo prefix mapping. Pure library code with tests, no daemon.

Design doc: `~/projects/kontora/DESIGN.md`
Epic: `~/tickets/kon-epic.md`

## Project Layout

```
kontora/
├── go.mod                        # github.com/worksonmyai/kontora, go 1.26
├── Makefile                      # mirroring kontora's Makefile structure
├── .golangci.yml
├── .gitignore
├── cmd/
│   └── kontora/
│       └── main.go               # CLI dispatch
└── internal/
    ├── config/
    │   ├── config.go             # types, Load, Validate
    │   ├── config_test.go
    │   └── testdata/             # YAML fixtures
    ├── ticket/
    │   ├── ticket.go             # Ticket struct, Parse, Marshal, SetField
    │   ├── ticket_test.go
    │   └── testdata/             # .md fixtures
    └── pipeline/
        ├── evaluate.go           # pure state machine
        └── evaluate_test.go
```

Dependency graph (no cycles):
- `config` — no internal deps
- `ticket` — no internal deps
- `pipeline` — imports `config` and `ticket`

## Package: `internal/config`

Parse `config.yaml` into typed Go structs. `gopkg.in/yaml.v3` directly, no Viper.

**Types**: `Config`, `Agent`, `Pipeline` (= `[]PipelineStep`), `Stage`, `Web`, `Duration`.

**Custom duration type**: `Stage.Timeout` uses a `Duration` wrapper with `yaml.Unmarshaler` that calls `time.ParseDuration` (yaml.v3 doesn't handle Go durations natively).

**Functions**: `Load(path) (*Config, error)`, `LoadReader(r io.Reader) (*Config, error)`, `(c *Config) Validate() error`.

**Validation**: pipeline stages reference existing stages and agents; `on_success` ∈ {next, done}; `on_failure` ∈ {retry, back, pause}; `back` not allowed on first stage; agents have `binary` set; defaults: `branch_prefix`→"kontora", `max_concurrent_agents`→3, `web.port`→8080.

**Tests**: valid config, minimal config with defaults, unknown stage ref, unknown agent ref, back-on-first-stage, invalid on_success/on_failure, missing agent binary, duration parsing (valid + invalid), file not found, malformed YAML.

## Package: `internal/task`

Parse markdown files with YAML frontmatter. **Round-trip preserves all unknown fields** using `yaml.Node` tree.

**Approach**: Parse frontmatter into `*yaml.Node` (preserves field order, styles, unknown fields). Decode the node into the `Ticket` struct for typed access. On write-back, patch the node tree via `SetField` and re-encode. Body is stored separately, never modified.

**Ticket struct fields**:
- `ID`, `Status` (string typedef), `Pipeline`, `Path`, `Agent`, `Stage`, `Attempt`, `StartedAt` (`*time.Time`), `CompletedAt`, `Branch`, `History` (`[]HistoryEntry`), `Created`, `LastError`
- `Body` (markdown, preserved as-is), `FilePath`, `rawNode` (unexported)

**`stage` field**: matches stage names in the pipeline config.

**Status**: stored as-is, no mapping in the parser. Status interpretation (open→todo) is daemon logic.

**Frontmatter splitting**: split on `\n---\n` boundaries, handle `\r\n`, first two `---` markers only.

**Functions**: `ParseFile(path)`, `Parse(r io.Reader)`, `ParseBytes(data []byte)`, `(t *Ticket) Marshal() ([]byte, error)`, `(t *Ticket) SetField(key string, value any) error`, `(t *Ticket) Title() string` (first `# heading` from body).

**SetField**: walks `yaml.Node` mapping children, replaces existing key's value node or appends new key-value pair.

**Tests**: basic parse, minimal ticket, unknown fields round-trip, field order preservation, body byte-identical after round-trip, both timestamp formats (`Z` and `+01:00` with microseconds), all statuses, no frontmatter error, empty body, `---` in body not confused with delimiter, SetField existing/new/preserves-others, Marshal with history, Title extraction, empty deps `[]` round-trips as `[]`.

## Package: `internal/stage`

Pure state machine: `Evaluate(ticket, pipeline, event) → Action`. No side effects — all state changes are expressed as field updates in the returned Action.

**Policies**:
- `on_success: next` → advance to next stage; `on_success: done` → mark ticket complete
- `on_failure: retry` → re-run (up to max_retries, then pause); `on_failure: back` → previous stage; `on_failure: pause` → human review

**Tests**: advance on success, retry on failure, back on failure, pause on exhausted retries, done on last stage.

## Implementation Order

1. Scaffold: `go.mod`, `main.go`, `Makefile`, `.golangci.yml`, `.gitignore` → verify `make build` and `make lint`
2. `internal/config` — self-contained, no internal deps
3. `internal/ticket` — most complex package (`yaml.Node` round-trip), self-contained
4. `internal/pipeline` — depends on config + ticket types
5. Integration: round-trip test with a real ticket file copy

## Build Tooling

Makefile targets: `build`, `test`, `test-race`, `lint`, `fmt`, `install`, `clean`.
`govulncheck` and `deadcode` as go tool deps.

## External Dependencies

- `gopkg.in/yaml.v3`
- `github.com/stretchr/testify`
- `golang.org/x/vuln/cmd/govulncheck` (tool)
- `golang.org/x/tools/cmd/deadcode` (tool)

## Verification

```bash
make build    # compiles cmd/kontora
make test     # go test ./...
make lint     # golangci-lint run + go mod tidy -diff + govulncheck + deadcode
```

## Scope

This plan covers Phase 1 only. Phases 2–8 are planned separately as we go.

## Notes

**2026-03-05T23:43:33Z**

progress: Phase 1 scaffold done — go.mod, main.go, Makefile, .golangci.yml, .gitignore created. make build and make lint pass.

**2026-03-05T23:47:08Z**

progress: Phase 2 (config) and Phase 3 (task parser) complete — all tests pass.
