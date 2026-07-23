/** @type {import('tailwindcss').Config} */
// Build-time Tailwind config. Mirrors the theme that used to live inline in
// static/index.html when the page pulled the Tailwind Play CDN. `make assets`
// runs the standalone Tailwind CLI against this to produce static/app.css, so
// the browser no longer compiles CSS at runtime.
module.exports = {
  content: [
    './internal/web/static/index.html',
    './internal/web/static/app.js',
  ],
  theme: {
    extend: {
      fontFamily: {
        mono: ['JetBrains Mono', 'monospace'],
        sans: ['DM Sans', 'sans-serif'],
      },
      colors: {
        surface: {
          deep: 'rgba(var(--surface-deep), <alpha-value>)',
          950: 'rgba(var(--surface-950), <alpha-value>)',
          frame: 'rgba(var(--surface-frame), <alpha-value>)',
          rail: 'rgba(var(--surface-rail), <alpha-value>)',
          900: 'rgba(var(--surface-900), <alpha-value>)',
          850: 'rgba(var(--surface-850), <alpha-value>)',
          800: 'rgba(var(--surface-800), <alpha-value>)',
          700: 'rgba(var(--surface-700), <alpha-value>)',
          600: 'rgba(var(--surface-600), <alpha-value>)',
        },
        edge: {
          card: 'rgba(var(--edge-card), <alpha-value>)',
          input: 'rgba(var(--edge-input), <alpha-value>)',
          hover: 'rgba(var(--edge-hover), <alpha-value>)',
          faint: 'rgba(var(--edge-faint), <alpha-value>)',
        },
        accent: {
          DEFAULT: 'rgba(var(--accent), <alpha-value>)',
          dim: 'rgba(var(--accent-dim), <alpha-value>)',
          bright: 'rgba(var(--accent-bright), <alpha-value>)',
          fg: 'rgba(var(--accent-fg), <alpha-value>)',
        },
        ok: 'rgba(var(--ok), <alpha-value>)',
        warn: 'rgba(var(--warn), <alpha-value>)',
        err: 'rgba(var(--err), <alpha-value>)',
        review: 'rgba(var(--review), <alpha-value>)',
        tx: {
          DEFAULT: 'rgba(var(--tx), <alpha-value>)',
          hi: 'rgba(var(--tx-hi), <alpha-value>)',
          2: 'rgba(var(--tx-2), <alpha-value>)',
          3: 'rgba(var(--tx-3), <alpha-value>)',
          4: 'rgba(var(--tx-4), <alpha-value>)',
          log: 'rgba(var(--tx-log), <alpha-value>)',
          faint: 'rgba(var(--tx-faint), <alpha-value>)',
        },
        st: {
          open: 'hsl(var(--st-open) / <alpha-value>)',
          progress: 'hsl(var(--st-progress) / <alpha-value>)',
          paused: 'hsl(var(--st-paused) / <alpha-value>)',
          review: 'hsl(var(--st-review) / <alpha-value>)',
          done: 'hsl(var(--st-done) / <alpha-value>)',
          cancel: 'hsl(var(--st-cancel) / <alpha-value>)',
        },
      },
    },
  },
};
