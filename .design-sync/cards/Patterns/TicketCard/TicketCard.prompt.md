The canonical kontora surface pattern. Not an importable component.

Kontora ships no React components, so copy this markup rather than importing
anything. It shows the house style: a token-backed surface, a 1px edge, a
strong title over a mono metadata line, and a status pill tinted from the
matching lifecycle hue.

```jsx
<div className="bg-surface-900 border border-edge-card rounded-lg p-4 flex items-center justify-between">
  <div>
    <div className="text-tx-hi text-sm font-medium">Rewrite the scheduler</div>
    <div className="text-tx-3 text-xs font-mono">ticket-142 · feat/scheduler</div>
  </div>
  <span className="text-st-done text-xs bg-st-done/10 rounded-md px-2 py-1">done</span>
</div>
```

Conventions worth keeping:

- Identifiers, branches and paths are always `font-mono`.
- A card sits on `bg-surface-900` or `bg-surface-850`, never on the page ground.
- A status pill takes its text and its tint from the same `--st-*` hue.
- To mark a card with its pipeline, set `data-pipe-color` on it and draw the
  accent from `hsl(var(--pipe-h) / 1)`.
