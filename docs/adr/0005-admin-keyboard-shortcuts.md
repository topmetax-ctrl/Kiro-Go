# Admin Keyboard Shortcuts and Command Palette

Status: Accepted
Date: 2026-08-27

## Context

ADR 0004 turned the admin shell into ten views driven by one route registry, on
the strength of an operator workflow that switches between those views while
configuring: providers to routes to activity to statistics, repeatedly, in one
sitting. That ADR deferred keyboard shortcuts with a note that letter-key
shortcuts need an off or remap switch to satisfy WCAG 2.1.4 and that modifier
combinations collide per platform. This is that round.

Three facts shaped the design:

- **A two-key sequence is still a character-key shortcut.** The Understanding
  document for SC 2.1.4 is explicit: the criterion "also applies to situations
  where a shortcut is based on a sequence of character keys — for example,
  pressing G and then A in quick succession". A GitHub-style `g` chord does not
  escape the criterion by being two keystrokes, so it needs one of the three
  listed mechanisms: turn off, remap, or active-only-on-focus.
- **A menu opened with a non-printable key is exempt.** The same document notes
  that a component "opened with a single non-character shortcut (e.g., Alt or
  Alt+F) before pressing a single character key to select an item" makes the full
  path a shortcut that includes a non-printable key. A palette opened with
  Cmd/Ctrl+K and then filtered by typing is therefore outside the criterion on
  two counts: the opener carries a modifier, and the typing happens in a
  component that has focus.
- **Modifier combinations are the colliding ones, not the bare keys.**
  Alt+letter triggers accesskeys in Chrome, Ctrl/Cmd+digit switches browser tabs,
  and which combinations are free differs by platform and browser. GitHub ships
  Cmd/Ctrl+K for its palette and documents that it "may conflict with your
  default OS and browser keyboard shortcuts", offering an alternative binding.

## Decision

1. **Two tiers.** Cmd/Ctrl+K opens a command palette; `g`+letter, `[`, `]` and
   `?` are character-key shortcuts. The split is deliberate: the palette is the
   keyboard path that survives when the character shortcuts are switched off,
   which is what keeps the panel keyboard-navigable for speech-input users rather
   than leaving them with nothing.

2. **SC 2.1.4 is satisfied by "Turn off", not by remapping.** One switch in the
   shortcut sheet disables every character shortcut and leaves Cmd/Ctrl+K
   working — the same division GitHub offers ("disable character key shortcuts,
   while still allowing shortcuts that use modifier keys"). A remap UI was
   rejected: it would introduce a second, user-editable source of truth in front
   of every chord, while the sheet and the palette render straight from the
   registry. The setting is client-side view state (`kiro_char_shortcuts` in
   localStorage), like the theme and the privacy toggle.

3. **The chord letter lives in the route registry.** `key: 'p'` sits beside
   `panel`, `stream` and `enter` on each leaf route, so one table still answers
   every question about a view, and `routeByKey` is derived from it. A letter
   claimed twice is a boot warning, not a silently shadowed shortcut.

4. **The sheet and the palette render from the registry.** Neither keeps its own
   list, so neither can advertise a chord the dispatcher does not honour — the
   standard failure mode of a hand-written shortcut list.

5. **`[` and `]` step through the sidebar order and wrap.** Chrome DevTools uses
   the same pair to cycle panels. This is the cheapest possible gesture for the
   workflow this design exists for: neighbouring views — providers/routes,
   activity/statistics — are one keystroke apart with nothing to aim at.

6. **`g g` returns to the previous view.** The A-B flip for a pair that is not
   adjacent. Because the view left behind becomes the new previous one, pressing
   it again comes back.

7. **One dispatcher, one guard chain.** A single `keydown` listener on
   `document`. It stands down for: the login screen, a composing IME (Vietnamese
   telex would otherwise fire chords mid-word), `defaultPrevented`, any
   input/textarea/select/contenteditable target, an open combobox popover, and —
   for the character tier — any open dialog. Character shortcuts act only on an
   unmodified key, so Cmd+G, Ctrl+[ and Alt+Arrow reach the browser untouched.

8. **The armed chord is echoed on screen.** A pill at top centre for the 1.5s the
   `g` prefix is live. Without it, a half-typed chord looks like a frozen app.
   Top centre because the bottom-left corner is the sidebar's logout button and
   the whole bottom edge belongs to the toasts, which go full-width when narrow.

9. **The palette is a combobox, not a menu.** DOM focus stays on the input and
   the highlighted row is named by `aria-activedescendant` — per MDN, "the
   browser keeps the DOM focus on the container element or on an input element
   that controls the container", and the active descendant has to be scrolled
   into view by the author. Moving real focus onto rows would stop the user
   typing. `role="listbox"` on the list, `role="option"` per row, `aria-controls`
   and `aria-autocomplete="list"` on the input.

10. **Palette matching is diacritic-blind, with an index map.** "thong ke" finds
    "Thống kê" and "dinh tuyen" finds "Định tuyến". NFD strips the combining
    marks; `đ` survives decomposition as a letter and is folded by hand. Folding
    changes length, so every folded position maps back to its source character —
    without that the match highlight drifts by the number of accents removed
    before it. Each whitespace-separated token must match, so the query can be
    typed in any order, and the route id joins the haystack so the English name
    still finds a view while the panel runs in Vietnamese or Chinese.

11. **Views outrank commands on a near-tie.** A 60-point bias. Measured without
    it, "dinh tuyen" ranked "Thêm định tuyến" above the Model Routes view purely
    because the command's label is shorter, so Enter opened a dialog when the
    user was navigating.

12. **Discovery has a mouse path.** A keyboard button in the sidebar footer opens
    the sheet, and the nav rows carry their chord as a hover hint. The switch that
    disables character shortcuts cannot itself sit behind a character shortcut,
    and the hints are removed rather than greyed when the shortcuts are off: a
    hint for a key that does nothing is worse than no hint.

## Alternatives rejected

- **Alt+letter or Ctrl+digit for navigation.** Collides with accesskeys and
  browser tab switching, differs per platform, and would still need the same off
  switch under 2.1.4 if any bare-letter fallback existed.
- **A remap UI.** See decision 2. It buys the same compliance as an off switch at
  the cost of a second source of truth per chord.
- **Single letters without a `g` prefix** (`p` for providers, `r` for routes).
  Ten bare letters is ten times the accidental-activation surface, and it forecloses
  every future single-key action in a view.
- **Putting the toggle in the Settings tab.** That tab is server configuration;
  client view preferences in this panel live next to the thing they affect (theme
  and language in the sidebar, privacy mode in the accounts card header). The
  shortcut switch belongs in the shortcut sheet, which is also where someone
  annoyed by a misfire will look.
- **A palette that also searches providers, keys and routes by name.** Deferred,
  not rejected: the action list is one array plus the registry, so entries can be
  added without touching the dispatcher. Kept to views plus seven commands here so
  the palette has no dependency on feature state that could be stale.

## Consequences

- The character tier is one guard chain away from every input in the panel. A new
  free-text surface that is not an input/textarea/select/contenteditable — a
  custom editor, say — would have to be added to `isTypingTarget` or it would eat
  `g` while typing.
- `Cmd/Ctrl+K` is `preventDefault`ed. In a browser that reserves it, the palette
  is still reachable from the sidebar button; no second binding was added.
- The nav rows now carry a `title`. It is supplementary — the accessible name
  still comes from the link text — and it disappears with the setting.
- `.switch input:focus-visible + .slider` was added while wiring the toggle: the
  native checkbox in this pattern is 0x0 and transparent, which keeps it in the tab
  order but left every switch in the panel with no visible focus indicator. Fixed
  for all of them, not just this one.
- Ten chord letters are spoken for (a l s i k p r e t c). An eleventh view needs
  a free letter or none; the registry warns on a collision at boot.
