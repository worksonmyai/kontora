# design-sync notes for kontora

## Shape: off-script, tokens only

Kontora is a Go daemon, not a JS package. There is no `package.json`, no npm
project, no React, and no Storybook anywhere in the repo. Neither converter
shape applies, so `package-build.mjs` is never run here. `ds-bundle/` is
assembled by hand from the repo's own committed artifacts.

The UI is one 3.5k-line Alpine `internal/web/static/index.html` plus the ES
modules under `internal/web/ui/`, which esbuild bundles into the served
`/app.js`, styled by a precompiled Tailwind `internal/web/static/app.css`.
There are no exported, independently renderable components, so the
project carries **zero components** and `window.Kontora` is an empty namespace.
This is the documented "tokens-only DS" case in the package sub-skill.

## What maps to what

| Bundle path | Source |
|---|---|
| `_ds_bundle.css` | `internal/web/static/app.css` (copied as-is) |
| `tokens/colors.css` | the `kontoraThemeCSS` dark/light strings, `index.html` lines 33-36 |
| `tokens/status.css` | the `--st-*` and `[data-pipe-color]` blocks, `index.html` lines 278-312 |
| `tokens/base.css` | the inline `<style>` globals, `index.html` lines 44-47 |
| `fonts/` | `internal/web/static/vendor/fonts/` (copied as-is) |
| `README.md` | `.design-sync/conventions.md` plus a file index appended |

`--col-tint`, `--stage-h`, `--line` and `--measure` are set per element in the
markup, not globally, so they are deliberately not shipped as tokens.

## The compiled-CSS limitation

`_ds_bundle.css` is Tailwind output from a content scan of kontora's own
markup, so only the class + alpha combinations kontora actually uses exist.
`bg-accent-dim` and `text-surface-900` have no rule, for example. The README
says this and points at `var()` as the escape hatch. Verified at sync time by
grepping every class named in the README against `_ds_bundle.css`.
`border-edge-faint` was cut that way, since only `text-edge-faint` compiles.

## Re-sync

There is no converter to re-run. Re-sync is: `make css` if the markup changed,
re-copy `app.css` and `fonts/`, re-extract the three token files if the theme
blocks in `index.html` moved, copy the cards back in, then re-validate and
re-upload.

`ds-bundle/` is gitignored, so the cards are kept in `.design-sync/cards/` and
copied into place:

```sh
cp -r .design-sync/cards/. ds-bundle/components/
```

Edit them under `.design-sync/cards/`, never in `ds-bundle/components/`, or a
fresh clone loses the edit.

```sh
node <skill>/package-validate.mjs ./ds-bundle --no-render-check
```

`.ds-build-meta.json` is hand-written with `"shape": "tokens-only"`, which is
what makes the validator treat the absent `components/` as expected rather than
an error. Keep that field or validate fails.

`ds-bundle/.verify.html` is a local-only page (dot-prefixed, never uploaded)
that exercises surfaces, edges, status hues, both fonts, a pipeline hue and raw
token access. Serve the bundle and open it in both themes to confirm a rebuild:

```sh
node <skill>/storybook/http-serve.mjs ./ds-bundle
# then ?theme=dark and ?theme=light
```

## Cards

The first upload shipped tokens only, and the Design System pane came up empty:
the pane builds its index from card files, and there were none. Four static HTML
cards under `components/` fix that. They are plain HTML with a first-line
`<!-- @dsCard group="..." -->` marker, no `.jsx` and no `.d.ts`, so nothing
claims to be an importable component:

| Card | Group | Shows |
|---|---|---|
| `Palette` | Foundations | every color token in both themes, read live with `getComputedStyle` |
| `StatusHues` | Foundations | the `--st-*` lifecycle hues and the `[data-pipe-color]` hues |
| `Typography` | Foundations | DM Sans and JetBrains Mono specimens, and the text tiers |
| `TicketCard` | Patterns | the house composition, in dark and light |

Keep `componentCount` in `.ds-build-meta.json` equal to the number of cards.

Each card renders both themes at once by putting `data-theme="light"` on a
child element. That works because `tokens/colors.css` scopes the light values to
the `[data-theme='light']` attribute selector rather than to `:root`.

## Known warns (expected, not new)

- `_ds_sync.json absent`. Omitted deliberately. The anchor's hash recipe wants
  converter-produced facts, and with zero components there is nothing for a
  re-sync to skip, so fabricating one would buy nothing.
- `[RENDER_SKIPPED]`. There are no preview cards to render. Visual
  verification is `.verify.html` instead, done by screenshot in both themes.

## Re-sync risks

- **The token files are a hand extraction.** If someone edits `kontoraThemeCSS`
  in `index.html`, `tokens/colors.css` silently goes stale. Nothing detects
  it. Diff the two on every re-sync.
- **`app.css` is a build output.** Syncing without running `make css` first
  ships whatever utilities were committed, which may lag the markup.
- **The cards are hand-written HTML.** They live under `components/` and are
  not generated from anything, so a token rename does not update them. Grep the
  four card files for a token name before removing it.
