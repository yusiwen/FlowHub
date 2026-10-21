// WebKit dispatches `compositionend` BEFORE the keydown of the Enter that
// confirms an IME composition, so that keydown reaches the page with
// `isComposing === false` — and sometimes with the legacy `keyCode === 229`,
// which marks a key the input method swallowed. A page whose submit guard is
// `!event.isComposing` therefore acts on the same Enter that only committed the
// candidate: in the embedded opencode session view the prompt was sent carrying
// the raw pinyin instead of the chosen characters. Chromium keeps `isComposing`
// true for that keydown, which is why such pages work in a browser but not here.
//
// This script runs at document start in every webview FlowHub creates and makes
// `isComposing` truthful for the engine underneath: true for `keyCode 229`, and
// true for a keydown that lands right after `compositionend`. It is additive —
// it never reports false where the engine said true — so a page that already
// consults `keyCode 229` keeps its own behaviour.
(function () {
  var keyboard = window.KeyboardEvent;
  if (!keyboard || !keyboard.prototype) return;

  var native = Object.getOwnPropertyDescriptor(keyboard.prototype, "isComposing");
  if (!native || typeof native.get !== "function" || native.configurable === false) return;

  // WebKit emits both events in one runloop turn, so the window only has to
  // outlive that turn plus IPC jitter. 10 ms is well under any human key
  // repeat (typematic minimum is ~30 ms), so a real second Enter is never
  // swallowed.
  var GRACE_MS = 10;
  var endedAt = 0;

  window.addEventListener(
    "compositionend",
    function () {
      endedAt = Date.now();
    },
    true,
  );

  Object.defineProperty(keyboard.prototype, "isComposing", {
    configurable: true,
    get: function () {
      if (native.get.call(this)) return true;
      if (this.keyCode === 229) return true;
      return Date.now() - endedAt <= GRACE_MS;
    },
  });
})();
