# Ludock identity

The Ludock mark is an L made from four equal tiles: three vertically, with the
fourth to the right of the bottom tile. Use the authored vectors; keep the tiles
flat, upright, and the same color.

- **Inline mark:** `packages/frontend/src/components/LudockMark.tsx`. The component
  inherits `currentColor`; the interface uses warm orange `#e68a3a`. It is
  decorative by default. Supply `label="Ludock"` only when no adjacent text
  already names the application.
- **Standalone vector:** `packages/frontend/public/ludock-mark.svg`. The 64-unit
  canvas contains 14-unit squares, 2-unit gaps, and 1.5-unit corner radii. Use at
  24 pixels or larger so the four tiles remain distinct.
- **Browser tab:** `packages/frontend/public/favicon.svg` and `favicon-32.png`.
  This variant joins the squares into one L and aligns its edges to whole pixels
  at 16 pixels. A charcoal `#20211f` tile keeps the orange legible on both light
  and dark browser chrome.
- **Application artwork:** `packages/frontend/public/ludock-app.svg` is the source
  for `apple-touch-icon.png` (180 pixels).
  These use the four-tile mark on an opaque charcoal background. The square
  artwork allows a launcher to apply its own corner mask.

Keep the canvas padding when placing the mark beside the Ludock name. Do not add
outlines, shadows, bevels, or extra game pieces. The small solid favicon and the
larger four-tile mark share the same L silhouette.

Use the shared orange `--accent` for primary actions, links, keyboard focus,
selected tabs, and native checkbox/radio controls. Primary buttons use dark
`--on-accent` text and the lighter `--accent-hover` shade on hover. This includes
the console Send action. Secondary actions stay neutral unless explicitly
accented; outlined accent actions keep their orange hue on hover. Console tabs
use the same orange selected-state indicator as other tabs. Text selection and
the terminal cursor/selection use the shared tokens; ANSI output retains its
own palette.

Keep informational blue, success green, warning yellow, and danger red distinct
from the interaction accent. Do not add page-specific blue control styles.
