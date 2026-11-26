# A lighter dashboard look

## Why

The dashboard feels heavy. The sidebar is near-black (`#1f1f2b`) and the page is white, so the eye sees two
blocks fighting. The owner wants it to read as one light surface, like the two reference consoles the owner shared.

## Goals

- The sidebar, the phone top bar and the sign-in screen are light. The whole app reads as one surface.
- Look A from the mockups: the sidebar and the page share one light grey; content sits in white cards with a
  thin line and no shadow. The active menu item is a white pill with a thin line.
- Fewer type sizes and corner radii, so pages look the same as each other.

## Non-goals

- No change to the menu items, their order, the routes, or any page's content or copy.
- No new UI library, no Tailwind, no new font file.
- No dark mode.

## Anti-goals

- A page that loses information or a control because of the restyle.
- Text or a pill that falls below WCAG AA contrast on the new grey.
- A rewrite of the 85 page files.

## Approaches

| | Approach | Trade-off |
|---|---|---|
| **1 (picked)** | Reskin in place: change the tokens in `theme.css` and the shell, nav, card, stat and sign-in rules in `styles.css`; fold the type sizes and radii onto a small scale. | Pages stay untouched. The stylesheet keeps its current shape. |
| 2 | Rewrite `styles.css` from zero on a new token set. | Cleaner file, but every page must be checked again for a missed class, for a look the owner already picked. |
| 3 | Move to Tailwind or a component kit. | Touches all 85 page files. Out of proportion to "it feels heavy". |

The owner described the outcome and handed over the how; approach 1 is my call.

## Design

**Colours (`theme.css`).** The `--side*` tokens become light:

| Token | Today | New |
|---|---|---|
| `--bg` (page and sidebar) | `#f7f7f9` | `#f6f7f9` |
| `--side` | `#1f1f2b` | `#f6f7f9` |
| `--side-2` (hover, Admin-area box) | `#2a2a3a` | `#eceef2` |
| `--side-3` | `#3a3a4e` | `#e1e3ea` |
| `--side-text` | `#c9c9d6` | `#3b3e48` |
| `--side-active` | `#ffffff` | `--text` |
| `--line` | `#e6e6ee` | `#e7e8ec` |

The accent, bad and grey tokens stay. Three text colours get a little darker so every pill passes WCAG AA
(4.5:1) on its own background and on the page: `--ok` `#1f8a4c` → `#18773f` (was 3.9:1), `--warn` `#a86a00`
→ `#8a5800` (was 4.0:1), `--run` `#2f6fd6` → `#2459b8` (was 4.2:1).

**Shell.** The sidebar gets a 1px `--line` border on the right. The brand and logo draw in `--text`. The
active item is `#fff` with a 1px `--line` border and weight 600; hover is `--side-2`. The "Admin area" box and
"Back to" label use the light tokens. The account box at the bottom gets a top line.

**Phone top bar** and **sign-in background**: `--bg` with a bottom line, text in `--text`. The menu drawer on
a phone stays an overlay, now light.

**Cards.** Radius 12px → 10px, no shadow (there is one `box-shadow` today; it stays only if it marks a
popover). Stat tiles are cards, as now.

**Type scale.** 12 sizes today. New set: 11.5 (meta), 12.5 (small UI), 13.5 (body), 15 (card titles),
20 (page title), 26 (a big number). Each current size maps to the nearest one. 9px goes.

**Radii.** 4px (pills inside text, code), 7px (buttons, inputs, nav items), 10px (cards), 999px (pills).

## Left to the builder

- Mapping each rule's old size and radius to the scale.
- Contrast check on every pill and muted text on `#f6f7f9`.
- Updating any test that pins a dark-sidebar class or colour.
- Checking every page in the preview: Overview, Repositories, a repository's tabs, Reviews, a review,
  Skills, Rules, Teams, every admin page, sign-in, and a phone width.
