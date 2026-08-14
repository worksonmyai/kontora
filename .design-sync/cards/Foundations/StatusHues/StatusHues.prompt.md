Reference card for the kontora status and pipeline hues. Not an importable component.

The `--st-*` tokens carry ticket lifecycle state: open, progress, paused,
review, done, cancel, error. They are HSL triples, so they take `hsl()` and not
`rgba()`:

```css
color: hsl(var(--st-done) / 1);
background: hsl(var(--st-done) / 0.12);
```

Tailwind exposes them as `text-st-done`, `bg-st-open/10` and so on.

For a per-pipeline tint, set `[data-pipe-color="indigo|cyan|amber|green|rose|mauve|none"]`
on a container. Anything inside it can then read `hsl(var(--pipe-h) / 0.4)`.

Definitions live in `tokens/status.css`.
