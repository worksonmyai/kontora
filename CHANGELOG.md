# Changelog

## [0.30.0](https://github.com/worksonmyai/kontora/compare/v0.29.0...v0.30.0) - 2026-08-13

- Rebuild start ticket modal on structured design
- Stop the watcher debounce test from splitting a write burst
- Give the config reload test one deadline for its six tickets
- Regenerate landing page screenshots
- Add lifecycle hooks: run user scripts at worktree and stage boundaries
- Show a ticket's sub-tickets and relations on the ticket page
- Rewrite landing page copy in plain terms
- Add time/tokens toggle to Stats stage breakdown panel
- Rebuild landing page on dashboard palette and rewrite copy
- Split Stats tokens by category and record pi usage
- Annotate a ticket kontora has not adopted

## [0.29.0](https://github.com/worksonmyai/kontora/compare/v0.28.0...v0.29.0) - 2026-08-12

- Add a Stats view for throughput, agent quality and live capacity
- Allow ticket annotation only in open
- Give a stage its own model, injected as --model
- Export daemon and pipeline metrics over OTLP
- Generate CHANGELOG.md and update it with each release
- Colour the [tag] prefix in a ticket's hover card
- Annotate a ticket in Plannotator and have an agent rewrite it
- Show a ticket's deps, links and parent in the details rail
- Make the wordmark a second way back to the board
- Add a setup command that can brief a coding agent
- Synthesize one ticket-level summary from the recorded run summaries
- Match a self-write by content instead of counting writes
- fix sidebar
- Start every tab pane on the rail's first label
- Start the commit rail at the first stage card
- Default branch naming to slug mode
- Put a dragged card back when the Start ticket form is dismissed
- Show the branch a ticket will run on before the run starts

## [0.28.0](https://github.com/worksonmyai/kontora/compare/v0.27.0...v0.28.0) - 2026-08-10

- Add readable ticket branch names
- Show the running stage's transcript live in the activity tab
- Show one header on the ticket page
- Link pull request numbers in summary prose
- Chip ticket ids in summary prose
- Chip summary entities by what the summaries say
- Show one summary card per stage run
- Hide the frontmatter separator in the body editor
- Give the xterm test double the key event handler
- Add an optional per-ticket base branch
- Make Escape leave read-write mode before closing the ticket
- Give the ticket body one look in both of its states
- Start the tickets watcher before the initial scan
- Highlight the ticket source and hold the body to a reading measure
- Widen the ticket tab column to 960px
- Focus the palette input once its panel is on screen
- Keep ticket bodies out of the board list

## [0.27.0](https://github.com/worksonmyai/kontora/compare/v0.26.0...v0.27.0) - 2026-08-08

- Cache rendered ticket markdown by its source
- Filter the board on the frame instead of after a delay
- Take the vendored libraries off the critical path
- Serve the web UI compressed and revalidated
- Keep the board rendered while the ticket page is open
- Keep the ticket body in place when edit mode ends
- Window the activity transcript to its newest events
- Show resolved values in the new-ticket form
- Show resolved values in the start-ticket modal
- Apply project defaults when the ticket path changes
- Drop the config reformat advisory from Settings
- Fix the Settings view failing to load with a clone error

## [0.26.0](https://github.com/worksonmyai/kontora/compare/v0.25.0...v0.26.0) - 2026-08-08

- Add a command palette to the web UI
- Replace the ticket drawer with a full-page ticket view
- Add per-run stage activity to the daemon and web API
- Resume an interrupted agent session after a daemon restart
- Add a Settings view to the web UI
- Show the ticket summary on its own tab
- Scope tmux window cleanup and give each daemon its own session
- Confirm before archiving tickets
- Filter archived tickets by repository and status
- Speed up the tmux terminal in the web UI
- Let a project override the branch prefix
- Stop the release from merging the formula PR

### Dependencies

- Bump github.com/mattn/go-isatty from 0.0.21 to 0.0.24 (#46)
- Bump golang.org/x/term from 0.44.0 to 0.45.0 (#44)
- Bump github.com/coder/websocket from 1.8.14 to 1.8.15 (#37)

## [0.25.0](https://github.com/worksonmyai/kontora/compare/v0.24.0...v0.25.0) - 2026-08-07

- Default a new ticket's pipeline and agent from its project
- Keep the ticket body in place when clicking to edit
- Summarize each stage run and show it in the web UI
- Give each pi stage its own session directory
- Toggle the theme with the c-t key sequence
- Sort the Human Review column by finish time

## [0.24.0](https://github.com/worksonmyai/kontora/compare/v0.23.0...v0.24.0) - 2026-08-03

- Hot-reload prompts, agents, and pipelines without a restart (#48)
- Patch board cards instead of rebuilding whole columns (#49)

## [0.23.0](https://github.com/worksonmyai/kontora/compare/v0.22.0...v0.23.0) - 2026-07-24

- Wrap the sleep agent in sh -c in the claim test
- Bump Go to 1.26.5 for the crypto/tls ECH fix
- Detect agent type behind wrapper binaries
- redesign
- Guard tickets with per-instance claims for multi-machine safety
- Pause tickets when a clean agent exit hid a quota/API error

## [0.22.0](https://github.com/worksonmyai/kontora/compare/v0.21.0...v0.22.0) - 2026-06-22

- Persist the web auth cookie and mark it Secure over HTTPS
- Keep ticket body scroll position when entering edit mode

## [0.21.0](https://github.com/worksonmyai/kontora/compare/v0.20.0...v0.21.0) - 2026-06-21

- Fill remote-CLI gaps: delete, update, init, config edit
- Add a token login modal to the web UI
- Add openssh-client to docker image

## [0.20.0](https://github.com/worksonmyai/kontora/compare/v0.19.0...v0.20.0) - 2026-06-21

- Allow to pass web token via env
- De-Alpine the kanban card list for large-board performance

## [0.19.0](https://github.com/worksonmyai/kontora/compare/v0.18.0...v0.19.0) - 2026-06-21

- Add a phone-width mobile web UI

## [0.18.0](https://github.com/worksonmyai/kontora/compare/v0.17.0...v0.18.0) - 2026-06-20

- Fix remote client edge cases
- Add remote mode to drive a kontora daemon over the network (#38)
- Bump bundled agent and tool versions
- Apply the canonical filename guard to foreign tickets too
- Unify status colors and polish the ticket detail panel
- Ignore md files which are not matching kontora ticket filenames

## [0.17.0](https://github.com/worksonmyai/kontora/compare/v0.16.0...v0.17.0) - 2026-06-10

- Load the xterm.js WebGL renderer in the web terminal
- Optimize web board for large ticket counts (#35)
- Self-host and embed web UI assets
- go 1.26.4
- Stop the agent when moving a running ticket to open
- Cache per-column board lists and batch SSE updates

## [0.16.0](https://github.com/worksonmyai/kontora/compare/v0.15.0...v0.16.0) - 2026-05-21

- Highlight drop-target column while dragging a card
- fix test
- Add archive command for old closed tickets
- Sort HUMAN REVIEW column by last update time
- Hide non-Kontora badge for open tickets on board
- Bump Go to 1.26.3
- fix linter
- Show non-Kontora tickets in lists
- Drop 'in flight' phrasing in empty-state text
- Add stats bar above the board
- Auto-fill agent from picked pipeline
- Use inline ∅ dashed box for empty columns
- Add hideable sidebar and new-ticket page
- Restyle main board to match design handoff
- Decouple plannotator review from the branch worktree
- Improve kontora board UX (#30)
- Key worktrees by branch instead of ticket ID (#29)
- Pass passthrough agent lookup in remaining daemon tests
- Recognise plannotator approve and cancel markers
- Resolve agent binary before spawn
- Launch plannotator in a disposable review worktree
- Fall back to common install paths when locating plannotator
- Move plannotator button to detail sidebar
- Gate plannotator review on human_review status
- Surface agent log path when a run fails (#28)
- Add plannotator review integration for code review (#27)
- Promote human_review to a first-class status
- Park completed tickets in human_review by default (#26)
- go vendor
- Pluralize 'agent/agents' in web UI status bar

### Dependencies

- Bump github.com/mattn/go-isatty from 0.0.20 to 0.0.21 (#25)

## [0.15.0](https://github.com/worksonmyai/kontora/compare/v0.14.0...v0.15.0) - 2026-03-28

- Add auto_pick_up config option and kontora run command
- Add custom pipeline statuses (#23)
- Use static landing page with MkDocs for docs only
- Add MkDocs Material docs site with matching theme
- Add GitHub Pages landing page
- Fix stale terminology in docs and testdata
- Fix formula update workflow for retries and missing auto-merge (#22)

## [0.14.0](https://github.com/worksonmyai/kontora/compare/v0.13.0...v0.14.0) - 2026-03-23

- Kill agent when running ticket moved to open or done (#20)
- Rename role to stage across codebase (#19)
- README.md
- Open PR instead of pushing directly in formula update (#17)

### Dependencies

- Bump github.com/charmbracelet/log from 0.4.2 to 1.0.0 (#15)

## [0.13.0](https://github.com/worksonmyai/kontora/compare/v0.12.0...v0.13.0) - 2026-03-21

_No changes._

## [0.12.0](https://github.com/worksonmyai/kontora/compare/v0.11.0...v0.12.0) - 2026-03-21

- Add Docker build workflow pushing to GHCR (#16)
- Configure Dependabot for Go modules with daily updates (#14)
- Persist last_error in ticket YAML frontmatter (#13)
- Add llms.txt for agent-guided installation
- Add screenshots to README
- Add Lucide icon license notice
- Fix config path typo and document set-stage API endpoint

## [0.11.0](https://github.com/worksonmyai/kontora/compare/v0.10.0...v0.11.0) - 2026-03-20

- Dim kanban board when detail sidebar is open
- Add icons to form labels, replace tooltip buttons with inline hints
- Add repo badge and meta icons to kanban cards
- Add Lucide icons throughout the web UI
- Add glowing top border to kanban cards by column status (#12)
- Add empty-state icons and column separators to kanban board
- Improve detail sidebar readability
- Widen default detail panel to 66% of viewport
- Redesign card layout and split detail panel into two columns
- Sort tickets by started_at for in-progress, created_at for others
- Fix web terminal rendering corruption after browser resize

## [0.10.0](https://github.com/worksonmyai/kontora/compare/v0.9.0...v0.10.0) - 2026-03-19

- Set LANG=en_US.UTF-8 on tmux commands to fix Unicode rendering
- Add header row to ticket tab for consistent layout

## [0.9.0](https://github.com/worksonmyai/kontora/compare/v0.8.0...v0.9.0) - 2026-03-18

- Add fullscreen expand/collapse toggle for ticket details tab (#10)
- Show branch in detail panel metadata (#9)

## [0.8.0](https://github.com/worksonmyai/kontora/compare/v0.7.0...v0.8.0) - 2026-03-17

- Sort tickets by created date descending, then title, then ID
- Show confirm dialog instead of error when moving open ticket to running
- Remove background tint from running column
- Fix web terminal failing to load with Unicode11 addon
- Use linked tmux session for web terminal viewer
- Load Unicode11Addon in web terminal for correct glyph rendering
- Remove background tint from paused and done columns

## [0.7.0](https://github.com/worksonmyai/kontora/compare/v0.6.0...v0.7.0) - 2026-03-17

- Add dotted underline to clickable pipeline stage chips
- Add dashed underline to click-to-copy elements
- Standardize tooltip mechanism on pipeline stages
- Improve contrast of tx-4 text color
- Rename "Do ticket" modal to "Start ticket"
- Use h1 for page title to fix heading hierarchy
- Show loading indicator when fetching ticket details
- Add loading state to action buttons (pause/retry/skip)
- Improve contrast of surface-600 color for readability
- Improve visual contrast and distinction across UI

## [0.6.0](https://github.com/worksonmyai/kontora/compare/v0.5.0...v0.6.0) - 2026-03-17

- Make tooltip info buttons slightly more visible
- Standardize modal close buttons to SVG icons
- Add hover hint to click-to-edit ticket body
- Associate form labels with inputs via for/id
- Add role="alert" to error toast for screen readers
- Use <main> landmark for primary content area
- Respect prefers-reduced-motion for animations
- Increase error toast auto-dismiss from 5s to 10s
- Add focus-visible styles for keyboard navigation
- Make ticket cards keyboard-accessible
- Add ARIA dialog semantics to modals
- Switch dark theme from Catppuccin Mocha to Macchiato
- Extract Agent.IsClaude/IsPi to handle full binary paths
- Fix hook injection when agent binary is a full path
- Add missing matcher field to Stop hook settings
- Allow overriding the branch name for a ticket (#8)
- Fix lint issues in shared ticket service
- Add tests for configDirs() XDG_CONFIG_HOME behavior
- Fix PR review issues in shared ticket service
- Introduce shared ticket application service
- Fix pipeline and agent selects not restoring value on reopen
- Derive lock path from config dir and fix XDG_CONFIG_HOME support
- Make the interface slightly bigger
- fix linter
- Handle SetField error and verify stage forwarding in test
- Fix lastError preservation condition and error banner overflow
- Set kontora: true on ticket transitions in web UI
- Broadcast SSE on pause and preserve lastError across disk refreshes
- Add set-stage command to move tickets to any pipeline stage
- Fix Copilot terminal startup review
- Add last_error field to ticket info and display in web UI
- Extract web app JS and harden terminal reconnects
- Warn about defaults with --dangerously-skip-permissions
- Move to worksonmyai org

## [0.5.0] - 2026-03-11

- Move to worksonmyai org
- Initial commit
