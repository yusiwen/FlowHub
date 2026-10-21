//! The document-start correction for WebKit's IME composition order.
//!
//! WebKit dispatches `compositionend` before the keydown of the Enter that
//! confirms a candidate, so an embedded page that guards with
//! `!event.isComposing` submits on the same keystroke that committed the
//! composition — measured in the opencode session view, whose prompt was sent
//! carrying the raw pinyin. The script in `webview-ime.js` runs before any page
//! script and reports `isComposing` the way the specification and Chromium do.
//! See `docs/adr/0011-webview-ime-composition.md`.

/// Document-start script for every webview this app creates.
pub const SCRIPT: &str = include_str!("webview-ime.js");

#[cfg(test)]
mod tests {
    use super::SCRIPT;

    // The behaviour is asserted by `desktop/scripts/ime-shim.test.mjs`; this is
    // the tripwire for the wiring, since a lost `include_str!` argument or an
    // emptied file would leave every gateway green and the bug in place.
    #[test]
    fn the_script_carries_both_web_kit_signals() {
        assert!(SCRIPT.contains("isComposing"), "the guard lost its reader");
        assert!(SCRIPT.contains("229"), "the legacy key code is gone");
        assert!(
            SCRIPT.contains("compositionend"),
            "the grace window is gone"
        );
    }
}
