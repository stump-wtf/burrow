# Burrow design language — "ink on paper"

Burrow's web surfaces take OpenWork's own design values so the two read as one
family: near-black ink on white paper, cool slate grays, hairline borders, soft
ink-tinted shadows, quiet 150ms transitions. No accent colors anywhere — state
and hierarchy come from weight, size, and the gray ramp.

The source of truth is the token block in `internal/httpapi/web.go`
(`pageCSS`). Every web surface embeds that const rather than restyling from
scratch; this document explains the tokens and the rules for using them.

## Tokens

| Token | Light | Dark | Use |
|---|---|---|---|
| `--ink` | `#011627` | `#f5f7fa` | text, primary buttons, focus rings |
| `--body` | `#374151` | `#c3ccd6` | body copy |
| `--muted` | `#6b7684` | `#8a93a0` | secondary copy, meta, wordmark |
| `--faint` | `#8a93a0` | `#5c6b7a` | placeholders, disabled |
| `--border` | `#e8edf3` | `#1d2a3a` | hairlines |
| `--surface` | `#f7f8fa` | `#101b29` | code blocks, wells |
| `--hover` | `#f5f7fa` | `#152238` | hover wash |
| `--page` | `#fcfcfc` | `#0b1220` | page background, primary-button text |
| `--card` | `#ffffff` | `#0e1826` | card background |
| `--btn-fg` | `#ffffff` | `#0b1220` | text on the primary button |
| `--btn-hover` | `#000000` | `#dbe2ea` | primary-button hover |

Radii: cards 16px, controls (buttons, code blocks) 8px.
Shadow: `0 8px 24px rgba(1,22,39,.05)` in light; a plain dark shadow in dark
mode. Focus: 2px solid `--ink`, offset 2px, via `:focus-visible` only.

## Components

- **Card** — the one container. White (dark: raised), hairline border, 28px
  padding, 16px radius, soft shadow. Widths cap at 400px for auth surfaces.
- **Primary button** — ink background, `--btn-fg` text, full width, 8px
  radius, 14px/500. Hover darkens toward black in light mode (OpenWork's own
  hover). One per view.
- **Note** — 14px body copy in `--body` for explanatory lines.
- **Grant / code block** — `--surface` well, hairline border, monospace,
  `user-select: all` so a click selects the whole value.
- **Alt link** — 13px muted, breaks long URLs, darkens on hover.
- **Wordmark** — "burrow", 14px/600, muted, sits outside the card.
- **Row (console)**: the `/admin` list row. Cells are 14px on the card with a
  hairline bottom border (`--border`), the last row borderless, under a
  header of 12px uppercase muted labels. A row states itself only through
  `.badge` (muted pill; `.badge.warn` when the value needs the reader's
  attention, like an unset key or an org-wide audience) and `.sub` muted
  12.5px secondary text (the row id; a menu the cache has not produced yet,
  which reads `deriving…` rather than a count of zero). No hue, per rule 1.

## Rules

1. Grayscale hierarchy only: weight, size, and the gray ramp carry meaning;
   never introduce hue for state.
2. Dark mode is not inverted light — it re-tokenizes. New surfaces must set
   both token sets and use only tokens, never raw colors, past the token
   block.
3. 150ms transitions on color only; `prefers-reduced-motion` drops them.
4. One primary action per view.
5. Long values (grants, URLs) always render `word-break: break-all` in a
   `--surface` well, never inline in prose.
6. New pages embed `pageCSS` and compose from these components; a component
   this file does not list is a spec change, not a style tweak.
