# Private DNS diagrams

These SVGs are the editable sources displayed by the architecture and
qualification guides. Each file owns its layout, text, and styles. Edit the SVG
directly; no diagram renderer or generated image needs to stay in sync.

## Visual style

The diagrams follow the [Datum brand guidelines](https://www.datum.net/brand)
and the layout approach used in [Fleet PR #2](https://github.com/datum-cloud/fleet/pull/2).

- [Color](https://www.datum.net/brand/color): Midnight Fjord text and connectors
  on Glacier Mist backgrounds. Aurora Moss highlights intent and key notes;
  Ocean Swell distinguishes serving components and observations. Labels and
  boundaries carry meaning independently of color.
- [Typography](https://www.datum.net/brand/typography): Alliance No.1 for labels,
  with Arial and Helvetica fallbacks. DejaVu Sans Mono identifies addresses and
  section labels. Files reference fonts without embedding or downloading them,
  so GitHub can display them with installed fallbacks.
- [Graphics](https://www.datum.net/brand/graphics): a small square introduces each
  view. Fine connectors, aligned boxes, and open space keep attention on the
  architecture. Split wide flows into focused views instead of shrinking text.

Each SVG has a 960-unit viewBox, an explicit background, a title, and a text
description. Body labels use at least 17 units. Sequence diagrams use at most
four participants. Repeated components across views retain their names.

## Editing and review

Keep resource ownership, arrow directions, trust boundaries, transport labels,
and current-versus-proposed behavior consistent with the surrounding guide.
Check the image at documentation width as well as full size. Review text
wrapping, connector crossings, arrowheads, and spacing with fallback fonts.

Keep SVGs self-contained: no scripts, external images, font downloads, or
`foreignObject` elements. After editing, check XML syntax, Markdown image links,
and whitespace. The Markdown alt text and SVG description should explain the
view without relying on its colors.
