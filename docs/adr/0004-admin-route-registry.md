# Admin Route Registry and Nested Navigation

Status: Accepted
Date: 2026-08-27

## Context

The admin shell had eight top-level tabs and one nested sub-tab bar inside
Forwarding. Four facts made that arrangement expensive to operate and to change:

- **Provider config and route config shared one page.** `#upstreamsList` and
  `#modelRoutesList` were two `form-group`s in a single card. Measured on a
  32-provider / 14-route config at 1440x900: the pane was 3707px (4.12 screens)
  and the Model Routes heading started at y=2213, so reaching routes meant
  scrolling past 15 provider cards.
- **The sub-tab bar scrolled away.** `.subtabs` was `display: inline-flex` with
  no `position: sticky`. From the route list the bar sat 2067px above the
  viewport; switching pane cost a 2075px scroll first.
- **No navigation state survived anything.** No hash routing, no persistence:
  reload always landed on Accounts, back/forward did nothing, and a jump to
  another tab and back lost the scroll offset (measured: 2213px -> 0).
- **Placement did not match ownership.** The Logs tab is fed only by the Kiro
  pool paths (`recordSuccessLogSplit`, `recordFailureWithDetails`); forwarded
  requests are recorded in a separate store. With the pool disabled it is
  permanently empty, which reads as broken. The Stats tab reads the same
  `/forward-stats` payload the Forwarding tab does — it is forwarding analytics
  sitting at the top level.

Adding a view meant editing the nav markup, the panel markup, an if-chain in
`switchTab`, and sometimes a sub-pane toggle — four places that could drift.

## Decision

1. **One route registry (`ROUTES` in `web/app.js`) is the source of truth** for
   the sidebar rows, which panel is visible, the URL, and which long-lived
   connection a view needs. Adding a view is one entry plus one panel element.
2. **Hierarchy is data, not DOM nesting.** A child route names any panel by id,
   so the request log moved under Accounts and the provider statistics under
   Forwarding without either block of markup moving. One visibility mechanism
   (`.tab-content` + `.hidden`) replaces the old tab layer plus pane layer.
3. **Sidebar navigation is a `<nav>` with a nested list of links**, active view
   marked `aria-current="page"` and the owning section marked with a class only
   (one element per set may be current). Not `role="menu"` (MDN: reserved for
   action menus, not site navigation) and not a tablist, because these are
   addressable views. Links also bring focus order, Enter and middle-click.
4. **Children stay visible for every section, not only the open one.** The point
   of the nesting is reaching another section's view in one click from wherever
   the operator is; an accordion would make that two.
5. **The URL is a hash (`#/forwarding/routes`).** `/admin` is one Go handler
   serving one file, so a real path 404s on reload, and a hash survives whatever
   prefix the panel is reverse-proxied under. Fragments carry a leading slash so
   they can never match an element id and trigger the browser's own
   scroll-to-fragment.
6. **Scroll position is remembered per route and restored on plain navigation**,
   not only on back/forward the way a browser does, because these views are
   switched between while editing. `history.scrollRestoration = 'manual'`, since
   panels are hidden at boot and a browser-restored offset would land in the
   wrong view. Offsets are clamped to the new document and applied twice, as a
   panel that renders in its enter hook can still be growing.
7. **Streams are declared by the route and reconciled by the router**
   (`stream: 'forward' | 'console'`), not opened and closed by hand per view.
   Moving between two routes that both want the forward stream keeps the one
   connection; leaving closes it. Both openers are idempotent, which is what
   makes a diff-on-every-navigation safe.
8. **Forwarding is four sibling panels** — Providers, Model Routes, Activity,
   Stats — and the sub-tab bar is gone.
9. **The request log explains its own empty state** when no account is enabled,
   and links to the forwarding activity view, instead of printing "no logs".

## Alternatives rejected

- **Keeping the sub-tab bar alongside the nav.** Two navigations for one set of
  views, two active states to keep in sync.
- **Making it sticky and leaving the rest alone.** Fixes the 2075px scroll but
  not the lost offsets, the missing deep links, or the four-place drift.
- **Moving the Stats and Logs markup into the Forwarding container.** Physical
  nesting to express a nav relationship; ~170 lines moved for no behaviour, and
  every id reference re-verified for nothing.
- **Path routing (`/admin/forwarding/routes`).** Needs the Go handler to serve
  index.html for a subtree, which is server surface added for a cosmetic URL.
- **Custom keyboard shortcuts (`Alt+N`, `g` then key).** Deferred, not rejected:
  letter-key shortcuts need an off or remap switch to satisfy WCAG 2.1.4, and
  modifier combinations collide with browser bindings that differ per platform.
  Hash routing already gives back/forward, which covers switching between two
  views.

## Consequences

- Sidebar rows are rendered from JS; a route whose panel is missing logs one
  `[nav]` warning at boot rather than silently showing a blank page.
- Panel ids (`tabStats`, `tabLogs`) are historical labels, not hierarchy. The
  registry says where a view belongs.
- The stats-window refresh driven by the forward stream is now reachable. It was
  dead code before: the stream only lived while the forwarding tab was open,
  which hid the stats tab by definition.
- Provider config drops from 3707px to 2235px; routes open at the top of their
  own 1751px view.
- Views are linkable and reload in place, so a support note can point at
  `#/forwarding/activity`.
