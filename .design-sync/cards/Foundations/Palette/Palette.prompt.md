Reference card for the kontora color tokens. Not an importable component.

Kontora ships no React components. This card renders every color token in both
themes so you can see the palette. Use the tokens through the Tailwind classes
listed in the project README, or directly:

```css
background: rgba(var(--surface-900), 1);
border-color: rgba(var(--edge-card), 1);
color: rgba(var(--tx-hi), 1);
```

Every value is a bare `r,g,b` channel list, so alpha works both in classes
(`bg-surface-900/70`) and in CSS (`rgba(var(--surface-900), 0.7)`).

Dark is the default. Light is selected by `[data-theme="light"]` on any
ancestor, so you can nest a light region inside a dark page.

Definitions live in `tokens/colors.css`.
