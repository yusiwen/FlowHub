// Asserts the document-start IME correction in `src-tauri/src/webview-ime.js`
// against a fake WebKit event object: the real engine's ordering cannot be
// provoked from a test runner, so what is pinned here is the report the script
// fabricates for the two shapes WebKit produces — a legacy `keyCode 229` and a
// keydown that lands just after `compositionend`.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const SOURCE = readFileSync(
  new URL("../src-tauri/src/webview-ime.js", import.meta.url),
  "utf8",
);

function harness({ withKeyboard = true } = {}) {
  const listeners = new Map();
  const window = {
    addEventListener(type, handler) {
      const list = listeners.get(type) ?? [];
      list.push(handler);
      listeners.set(type, list);
    },
  };
  if (withKeyboard) {
    window.KeyboardEvent = class KeyboardEvent {
      constructor(init = {}) {
        this.key = init.key ?? "";
        this.keyCode = init.keyCode ?? 0;
        this._composing = init.isComposing ?? false;
      }
      get isComposing() {
        return this._composing;
      }
    };
  }
  const clock = { now: 1_000 };
  const context = vm.createContext({ window, Date: { now: () => clock.now } });
  vm.runInContext(SOURCE, context);
  const fire = (type) => {
    for (const handler of listeners.get(type) ?? []) handler({ type });
  };
  return { window, clock, fire };
}

test("a plain Enter is not a composition", () => {
  const page = harness();
  assert.equal(
    new page.window.KeyboardEvent({ key: "Enter", keyCode: 13 }).isComposing,
    false,
  );
});

test("keyCode 229 marks a key the input method swallowed", () => {
  const page = harness();
  assert.equal(
    new page.window.KeyboardEvent({ key: "Enter", keyCode: 229 }).isComposing,
    true,
  );
});

test("the closing Enter stays composing through the grace window", () => {
  const page = harness();
  page.fire("compositionend");
  assert.equal(
    new page.window.KeyboardEvent({ key: "Enter", keyCode: 13 }).isComposing,
    true,
    "the keydown WebKit dispatches after compositionend must not submit",
  );

  page.clock.now += 10;
  assert.equal(
    new page.window.KeyboardEvent({ key: "Enter", keyCode: 13 }).isComposing,
    true,
    "the window is inclusive at 10 ms",
  );

  page.clock.now += 1;
  assert.equal(
    new page.window.KeyboardEvent({ key: "Enter", keyCode: 13 }).isComposing,
    false,
    "a second Enter 11 ms later belongs to the user, not to the input method",
  );
});

test("an engine that already reports composing is never downgraded", () => {
  const page = harness();
  assert.equal(
    new page.window.KeyboardEvent({ key: "Process", isComposing: true }).isComposing,
    true,
  );
});

test("a page without KeyboardEvent is left alone", () => {
  assert.doesNotThrow(() => harness({ withKeyboard: false }));
});
