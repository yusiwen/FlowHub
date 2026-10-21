# ADR 0011: The host corrects WebKit's IME composition order

- Status: accepted
- Date: 2026-10-21
- Issues: #85

## Context

Every tab is a page FlowHub does not own. Those pages decide for themselves whether
Enter sends a prompt, and the guard they use is
`event.key === "Enter" && !event.isComposing`.

That guard is written against Chromium's event order. WebKit — the engine behind
every webview this app creates — dispatches `compositionend` **before** the keydown
of the Enter that confirms an IME composition, so the keydown arrives with
`isComposing === false`, often carrying the legacy `keyCode === 229` that marks a key
the input method swallowed ([WebKit bug 165004](https://bugs.webkit.org/show_bug.cgi?id=165004),
[w3c/uievents#202](https://github.com/w3c/uievents/issues/202)).

Measured 2026-10-21 in the embedded `opencode@localhost` 2.0.32 session view: typing
`wenti` and pressing Enter both committed the composition and submitted the prompt, so
the message carried the raw pinyin instead of the candidate 问题. The composer's guard
is the one above (`packages/app/src/composer/editor/editor.tsx`), and the same code is
still on opencode's `dev` branch. Every other opencode input — rename, model search —
also checks `keyCode === 229`; only the composer misses it.

Three ways out were considered:

- **Fix each embedded page.** Correct, but it is not this repository, and the next
  embedded app will make the same mistake, because the host is what turns a browser
  page into an everyday editing surface.
- **Swallow the keydown in a capture listener** when `keyCode === 229`. Cheap, but it
  removes the event from pages that handle it deliberately (editors that track
  composition through it), so it can break what it does not understand.
- **Make `isComposing` truthful at document start.** The specification says a keydown
  generated during composition has `isComposing` set; Chromium does that. The host can
  restore the same report for the engine it ships, without hiding events.

## Decision

**One document-start script, added to every webview FlowHub creates** — the shell and
each tab — that redefines `KeyboardEvent.prototype.isComposing` as:

```
native.get.call(this) || this.keyCode === 229 || Date.now() - lastCompositionEnd <= 10
```

The three terms answer three different reports: the engine's own answer (which
Chromium gets right), the legacy swallowed-key signal, and the keydown WebKit
dispatches in the same runloop turn as `compositionend`. The window is 10 ms because
it only has to outlive that turn plus IPC jitter, and the typematic minimum is about
30 ms, so a genuine second Enter is never swallowed.

It is **additive**: it never reports `false` where the engine said `true`, and it
returns early when the engine exposes no `isComposing` accessor. A page that already
consults `keyCode === 229` therefore keeps its own behaviour; a page that consults
only `isComposing` starts behaving as it does in Chromium.

The script lives in `src-tauri/src/webview-ime.js` and reaches the builders through
`ime::SCRIPT` (`include_str!`), so a webview cannot be created without it. It is
main-frame only (`initialization_script`, not `_for_all_frames`): the pages this app
embeds render their composer in the main frame, and frames are a blast radius nobody
has asked to widen.

`src/main.ts`'s own Enter handler (the Hub log path) gained `!event.isComposing` in
the same change: the shell is a page too, and it should not be correct only because
the correction is installed.

## Consequences

- The composer behaves as it does in a browser: Enter confirms the candidate and
  commits the pinyin, a second Enter sends. Verified in a `WKWebView` probe against
  the real engine — without the script `{plain:false, legacy:false, closing:false}`,
  with it `{plain:false, legacy:true, closing:true}`, and `false` again 50 ms after
  `compositionend`.
- Every embedded page now sees a slightly different engine. That is the point, but it
  is a divergence from Safari, so the script must stay in step with the spec: it may
  only add `true`, never `false`.
- The 10 ms window can swallow an Enter pressed within 10 ms of a commit. Nothing
  measured gets close; if a real input method ever does, the number is the knob.
- Sub-frames are not covered. A composer rendered inside an iframe keeps the engine's
  report and the bug.
- Removal condition: when WebKit dispatches the closing keydown before
  `compositionend`, or when the embedded pages the app ships all consult
  `keyCode 229`. The script is one file and one `include_str!` away from deletion.
- The behaviour is pinned by `desktop/scripts/ime-shim.test.mjs` (five cases, run by
  `make desktop-check`), and both branches were mutation-checked: disabling the 229
  term fails the legacy case, removing the window fails the closing case.
