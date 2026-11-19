# UI screenshot tests

The visual suite compares 16 Chromium screenshots with reviewed PNGs. It serves
embedded web assets on loopback and uses synthetic tickets, settings, Assistant
threads, statistics, and terminal output. It does not start a Kontora daemon or
call a model. The existing Chromium and WebKit behavior tests run separately.

## Run comparisons

With Docker running, choose one command to run from the repository root:

```sh
make test-visual         # Compare all scenes once.
make test-visual-repeat  # Compare every scene ten times without changing PNGs.
make test-visual-update  # Capture candidates locally for human review.
```

All three commands use the same pinned Linux x64 renderer. On an Arm host,
Docker needs amd64 emulation. The first run downloads the renderer and test
dependencies. The visual commands do not write to the host `node_modules`.
Ordinary Go tests do not require Docker or Playwright.

Expected PNGs live in `internal/web/testdata/visual-baselines/linux-x64-chromium/`.
A check fails if a PNG is missing. It never creates or updates an expected PNG.
Keep the default color threshold and zero allowed differing pixels. If an
unchanged scene fluctuates, identify the changing input before changing the
comparison settings.

## Review candidates

Save the failed check's pixel diff before updating, or rerun the check afterward.
Update mode replaces both local diagnostics directories and removes old candidates.
It publishes new candidate PNGs only after all captures pass. It does not change
tracked baselines.

1. Run `make test-visual-update` outside CI. Open each PNG under
   `test-results/visual-candidates/linux-x64-chromium/`. Check that the candidate
   filenames match the current scene names in `internal/web/testdata/visual.spec.mjs`.
   Account for each difference from the tracked baselines as an intended addition,
   rename, or removal.
2. For an existing baseline, compare the candidate with the tracked PNG and the
   saved pixel diff. Confirm that each change matches the intended UI change.
   For a new scene, confirm that the controls and data are visible. Check that
   the screenshot does not show loading or hover state.
3. If a candidate exposes an existing UI defect, record the defect and do not
   accept that image. Fix the defect separately before accepting a replacement.
4. Copy only human-approved PNGs into the expected image directory. Include the
   reviewed PNGs with the change. In the [Baseline review](#baseline-review)
   section of this file, record the reviewer, the date, and the scenes they approved.
5. Run `make test-visual` and `time make test-visual-repeat`. Expect ten passing
   comparisons per scene in the repeat run, with no skips or retries. Record
   the elapsed time when adding scenes.

The separate `visual` CI job only checks baselines. Leave existing PNGs
unchanged when adding scenes unless you are also reviewing an intended UI change.

## Refresh the README screenshots

Run `make test-browser-install` if you do not have the browser dependencies.
From the repository root, capture twice and keep both output directories:

```sh
capture_dir=$(mktemp -d /tmp/kontora-readme-captures.XXXXXX)
KONTORA_MOBILE_CAPTURE_DIR="$capture_dir" KONTORA_BROWSER_TEST_FILTER='^README screenshots$' make test-browser BROWSER_ENGINE=chromium
verify_dir=$(mktemp -d /tmp/kontora-readme-captures.XXXXXX)
KONTORA_MOBILE_CAPTURE_DIR="$verify_dir" KONTORA_BROWSER_TEST_FILTER='^README screenshots$' make test-browser BROWSER_ENGINE=chromium
```

Confirm that `README screenshots` passes in both runs. Compare the filenames,
dimensions, and visible ticket states in `$capture_dir` and `$verify_dir`.
Host Chromium output can vary by platform. The capture uses synthetic data
and does not start Kontora, an agent, or tmux. It does not update the docs
or visual regression baselines.

Review the four candidates at GitHub README width before copying them:

| Candidate under `$capture_dir` | Size | Destination |
| --- | --- | --- |
| `chromium/1280-dark/readme-board.png` | 1280 × 520 | `docs/dark.png` |
| `chromium/1280-light/readme-board.png` | 1280 × 520 | `docs/light.png` |
| `chromium/1280-dark/readme-summary.png` | 1280 × 820 | `docs/ticket-summary.png` |
| `chromium/1280-dark/readme-session.png` | 1280 × 660 | `docs/dark-tmux.png` |

Preview the shared images in a temporary copy of the landing page:

```sh
preview_dir=$(mktemp -d /tmp/kontora-landing-preview.XXXXXX)
cp docs/index.html "$preview_dir/index.html"
cp "$capture_dir/chromium/1280-dark/readme-board.png" "$preview_dir/dark.png"
cp "$capture_dir/chromium/1280-dark/readme-session.png" "$preview_dir/dark-tmux.png"
```

Open `index.html` from `$preview_dir` in a browser. Check the board and the
full session transcript at desktop, 899 px, and phone widths. The landing page
crops the session image from the top left. After review, copy the approved
candidates to the destinations in the table.

## Baseline review

Alexander Akhmetov approved the original 12 baseline images on November 18, 2026.

On November 19, 2026, Alexander Akhmetov approved four more scenes:

- Short Archive confirmation
- Mobile Notes recovery
- Mobile completed Automation log
- Mobile terminal composer

The ten-repeat check passed all 160 comparisons in 8 minutes 41 seconds on
an Arm Mac using Docker's amd64 emulation.

## Read failures

- Screenshot mismatches save actual, expected, and diff PNGs in `test-results/visual/`.
  Playwright retains recorded traces for failed scenes. If a failure is caught
  before unload and the page is still open, the test also tries to save
  `pre-cleanup.png`.
- Open `playwright-report/index.html`. If attachments do not load, use
  Playwright's `show-report` command if Node.js and the locked Playwright package
  are installed on the host. Otherwise, inspect `test-results/visual/` directly.
- Reports, traces, and screenshots stay local. The CI job must not upload them.

## Upgrade the renderer

Update the Playwright pin in `internal/web/testdata/package.json` and regenerate
its lockfile. In `hack/visual.Dockerfile`, update the Playwright image tag,
digest, and `KONTORA_VISUAL_PLAYWRIGHT_VERSION` together. Match the Go image tag,
digest, and `KONTORA_VISUAL_GO_VERSION` to `go.mod`. Capture and review candidates
with the upgraded renderer. Do not reuse images from a different renderer.
