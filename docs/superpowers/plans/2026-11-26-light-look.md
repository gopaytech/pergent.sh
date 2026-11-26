# Light Look Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the dashboard read as one light surface (look A): a light sidebar, white cards on light grey, fewer type sizes and radii.

**Architecture:** A reskin in place. `web/src/theme.css` holds the tokens; `web/src/styles.css` holds every rule. No page file changes unless a test pins a dark class. A throwaway `preview.html` with a fake `fetch` (never committed) is the visual check.

**Tech Stack:** React 18, plain CSS, vitest + jsdom, Vite.

**Spec:** `docs/superpowers/specs/2026-11-26-light-look-design.md`

## Global Constraints

- New token values exactly as the spec table: `--bg #f6f7f9`, `--side #f6f7f9`, `--side-2 #eceef2`, `--side-3 #e1e3ea`, `--side-text #3b3e48`, `--side-active` = `--text`, `--line #e7e8ec`. Every other token stays, except the three pill colours Task 4 darkens for contrast.
- Type scale: 11.5, 12.5, 13.5, 15, 20, 26 px. Nothing else, and no 9 px.
- Radii: 4, 7, 10, 999 px. Nothing else.
- Cards: radius 10px, no shadow.
- No change to menu items, routes, copy, or page content. No new dependency.
- The server CSP forbids inline style and script: no `style=` strings, no `<style>` in components.
- Tests run per file only (`npx vitest run <file>`), never the whole suite locally. Never start a DB or containers.
- pergent.sh is public: no employer, product, team or colleague names in code, docs or commits.
- Commits use `git commit -s` and end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. Text on `--side` that was white (`#fff`) for the dark sidebar: brand, `.topbar`, badges, the Admin-area box. It must become dark, or it vanishes on the light grey.
2. Focus rings and hover states in the sidebar drawn for a dark background (`--side-2`, `--side-3`, white outlines). They must stay visible on light.
3. The phone drawer and its backdrop (`.menu-open`): a light drawer over a light page needs the right-hand line or a shadow to read as a layer.
4. Muted text (`--muted #6b6b7b`) and pills on `#f6f7f9`: each must stay at or above 4.5:1.
5. The sign-in, password-change and waiting screens (`CenteredCard`, `.signin`) used `--side` as the page: the card needs its line to stand out from a light page.

---

### Task 1: Light colours and shell

**Files:**
- Modify: `web/src/theme.css` (the `--side*`, `--bg`, `--line` tokens)
- Modify: `web/src/styles.css` (the shell section: `.topbar`, `aside`, `.brand`, `.org`, `.nav a`, `.nav a:hover`, `.nav a.active`, `.nav .icon`, `.section-label`, `.me`, `.badge` in the nav, `.menu-open` drawer and backdrop, `.signin`)

- [ ] **Step 1:** Set the tokens to the Global Constraints values.
- [ ] **Step 2:** In the shell rules, replace every hard `#fff` / white text and every dark-only colour with `--text` or `--side-text`. `aside` gets `border-right: 1px solid var(--line)`. `.nav a.active` is `background: var(--panel); border: 1px solid var(--line); color: var(--text); font-weight: 600` (keep its box size equal to the other items with a transparent 1px border on `.nav a`). Hover is `--side-2`. `.me` gets a top line. `.topbar` gets `border-bottom: 1px solid var(--line)`.
- [ ] **Step 3:** Review Focus 1-3, 5: check each in the preview (Task 4 builds it; until then, read the rules).
- [ ] **Step 4:** Run `npx vitest run src/layout/Layout.test.tsx src/layout/visual.test.tsx`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(web): a light sidebar, top bar and sign-in screen`.

### Task 2: Cards, type scale and radii

**Files:**
- Modify: `web/src/styles.css`

- [ ] **Step 1:** `.card` radius 10px; drop any `box-shadow` that is not on a popover or menu.
- [ ] **Step 2:** Map every `font-size` to the scale: 9, 11 → 11.5; 12 → 12.5; 12.5 stays; 13, 14 → 13.5; 15, 16 → 15; 18, 20, 22 → 20; 26 stays. Ems and `inherit` stay.
- [ ] **Step 3:** Map every `border-radius`: 4 stays; 6, 8 → 7; 11, 12 → 10; 999 stays; percentages stay.
- [ ] **Step 4:** Check: `grep -oE 'font-size: [0-9.]+px' web/src/styles.css | sort -u` prints only the 6 scale values, and `grep -oE 'border-radius: [0-9.]+px' web/src/styles.css | sort -u` only 4, 7, 10, 999.
- [ ] **Step 5:** Commit `style(web): one type scale and four corner sizes`.

### Task 3: Docs

**Files:**
- Modify: the comment at the top of `web/src/theme.css` (say the colours are the light look of this spec, not the October mockups).
- Modify: `docs/concepts/dashboard.md` only if it describes the dark sidebar (`grep -n -i 'dark\|sidebar' docs/concepts/dashboard.md`).

- [ ] **Step 1:** Update the comment and any doc line.
- [ ] **Step 2:** Commit `docs: the dashboard's light look` together with the spec and this plan.

### Task 4: Check every page

**Files:**
- Create (never commit): `web/preview.html`, `web/src/preview.tsx` — renders `<App />` in a `MemoryRouter` at the path in `location.hash`, with `window.fetch` answering from `src/test/fixtures.ts`.

- [ ] **Step 1:** Run `npx tsc --noEmit` and `npx eslint src/theme.css src/styles.css src/layout` (eslint ignores CSS; tsc must be clean).
- [ ] **Step 2:** In the preview, look at: Overview, Repositories, a repository (Overview, Settings, Setup checks tabs), Reviews, a review, Skills, Rules, Teams, a team, every admin page, sign-in, and the phone width (375 px) with the menu open. Check Review Focus 1-5 on each.
- [ ] **Step 3:** Measure contrast of `--muted` and each pill's text on its background and on `#f6f7f9` with a small script (WCAG formula). Expected: every pair ≥ 4.5:1.
- [ ] **Step 4:** Fix what the look turns up (commit `fix(web): ...`), then remove the preview files.
