# Silo brand assets

Marks for Silo and SiloDrive, exported from the *Silo Marks* design canvas
(claude.ai/design project `a8fa1616-847a-4c26-a26a-dbbf7dec9966`).

The geometry is the **Platters** mark (turn 2b): three stacked bars where the
middle one is inset from the left, so it reads as an enclosure with a slot and
stays distinguishable from a generic three-bar list icon. Only the middle bar
ever carries the accent colour.

## Colours

| Role | Hex | Notes |
| --- | --- | --- |
| Ink | `#201e1d` | The bars, and the app-icon field |
| System red | `#ec3013` | Default accent (turn 4a) |
| Booko orange | `#f5821f` | Alternate accent (turn 4b) |
| Knockout | `#ffffff` | Bars on an ink or accent field |

## Files

All SVGs are 1:1 square artboards with no embedded metadata.

### The mark — `silo-mark-*.svg` (64 artboard)

| File | Bars | Middle bar |
| --- | --- | --- |
| `silo-mark-red.svg` | ink | red |
| `silo-mark-orange.svg` | ink | orange |
| `silo-mark-ink.svg` | ink | ink |
| `silo-mark-white.svg` | white | white |
| `silo-mark-mono.svg` | `currentColor` | `currentColor` |
| `silo-mark-knockout-red.svg` | white | red |
| `silo-mark-knockout-orange.svg` | white | orange |

Use `silo-mark-mono.svg` inline in HTML and let CSS `color` tint it.

### App icon — `silo-app-icon-*.svg`

Rounded-square tiles ready for a dock/launcher icon. `-inverse` puts the mark on
an accent field instead of an ink one. PNG renders live in `png/` at 128–1024.

### Small sizes — `silo-16-*.svg`, `silo-32-*.svg`

Redrawn on their own grids rather than scaled down, so the bars land on whole
pixels. Two-tone (`-red`, `-orange`) for favicons, `-mono` for anything the host
tints.

### Sidebar / menu bar — `silo-sidebar-16*.svg`

Turn 5a, the solid treatment, drawn a touch lighter than `silo-16-*` so it sits
with the outlined system glyphs around it. `silo-sidebar-16.svg` uses
`currentColor` for anywhere the host tints the glyph; the red and orange
variants are for where the tinting is ours.

Note that these are **not** what Finder reads for a File Provider extension —
that wants an SF Symbol name, not an image. See [`sf-symbol/`](sf-symbol/).

### SiloDrive — `silodrive-*.svg` (76 artboard)

The mark with a sync badge in the lower-right corner. The badge takes the accent;
the arrow knocks out of it.

### Raster — `png/`, `favicon.ico`

Generated from the SVGs with `rsvg-convert`. `favicon.ico` bundles 16/32/48.
Re-render with `./render.sh` after changing a source SVG.
