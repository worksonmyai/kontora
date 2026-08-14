Reference card for the kontora type system. Not an importable component.

Two families, both self-hosted:

- `font-sans` is DM Sans, weights 400 to 600. Body copy, labels, headings.
- `font-mono` is JetBrains Mono, weights 400 to 700. Ticket IDs, branch names,
  file paths, log lines, terminal output.

The root font size is 17px, set in `tokens/base.css`, so Tailwind size classes
resolve against that rather than the usual 16px.

Text color moves through tiers rather than opacity: `text-tx-hi` for emphasis,
`text-tx` for body, `text-tx-2` through `text-tx-4` stepping down to muted,
`text-tx-log` for terminal output, `text-tx-faint` for the quietest tier.

Font files and `@font-face` rules live in `fonts/`.
