# ADR 0010: Role colour is identity, never state

- Status: accepted
- Date: 2026-10-21
- Issues: #81

## Context

Three views (upstream, instances, hub) already existed as sidebar scopes, but the
chrome did not show which one was on screen: the sidebar header and the tab strip
looked identical in all three, so the only cue was a 10px segmented control.

The tab strip is not per-view. One session can hold an `opencode` tab and a
`youtrack` tab at once — that is the point of #75 — and their labels carry a host
name, not a role. So the unit that needs a colour is the *tab*, not only the strip.

At the same time the app already spends colour on state: four health-dot hues
(green/yellow/orange/red), `--danger`, `--accent` for actions, and `.hub-warning`.
A role hue that drifts into that vocabulary would make "the Instances view" and
"this target is failing" look alike.

## Decision

**One hue per role.** Upstream violet, Instances amber, Hub teal, each with six
tokens per theme (`--<role>`, `-pill`, `-edge`, `-line`, `-band`, `-badge`). They are
written as explicit `rgba()`, not `color-mix()`: `vite.config.ts` builds for
`safari15`, which predates that function.

**Two scopes of application, on purpose.**

- `.layout[data-view="<role>"]` maps the *view*: the sidebar header band, the tab
  strip, the scope switch, the badges and the active row. A view switch repaints all
  of them from one attribute.
- `.tab[data-role="<role>"]` maps *one tab*. A strip holding three roles therefore
  shows three marker colours at once, and the active tab keeps its own title colour
  even when the band shows a different view.

The second scope allows a deliberate disagreement: when a scope has no page of its
own, the stage keeps the page being read (ADR 0009), so the band follows the scope
while the active tab follows its own role. The probe asserts exactly that state.

**The two header bands are one line.** The sidebar switch moves into a 40px
`.sidebar-top` band whose wash and 2px bottom line are the tab strip's, so the view
colour runs across the whole window at one y. The count/`Add` row moved below it
into the scrolling body. A 3px marker on the row backing the active tab repeats the
tab's marker, so the list and the strip name the same object.

**Semantic colour stays semantic.** Health dots, `--danger`, `--accent` and
`.hub-warning` are untouched; a role hue never encodes "healthy", "failed" or
"warning", and no state colour may be reused as a role. Adding a role means adding
six tokens per theme; adding a state means picking a hue outside the three.

## Consequences

- An operator learns one mapping (three hues) that is visible in the sidebar, in the
  strip, on a row and on a badge, and it never lies about state.
- Coloured text passes 4.5:1 in both themes: upstream 5.44/7.86 (light/dark),
  instances 5.37/10.82, hub 6.37/9.74 — measured against each theme's panel colour.
- The strip can show more than one hue at once, so a screenshot of "the strip" is not
  a screenshot of "the view"; both themes have to be reviewed.
- Amber is the closest role hue to a semantic one (`#ff9f0a` marks an HTTP error on a
  health dot). They are separated by shape and place — a 3px bar in a header, a 7px
  dot in a list row — and an HTTP error still leaves the header's role colour alone.
