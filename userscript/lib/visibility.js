'use strict';

// Spoof the Page Visibility API so claude.ai's app code believes the
// tab is always visible, even when the OS screensaver kicks in or the
// browser window is minimized. This matters because claude.ai's poll
// loop appears to pause itself based on visibility — a frozen page
// produces no DOM updates, the userscript's MutationObserver and
// dedup correctly suppress sends, and the chart shows a (correct but
// undesired) gap.
//
// Three things are spoofed:
//   1. `document.hidden` — getter always returns false.
//   2. `document.visibilityState` — getter always returns 'visible'.
//      `webkitHidden` / `webkitVisibilityState` are covered too for
//      older Chromium/WebKit codepaths that still query them.
//   3. `visibilitychange` events — a capture-phase listener on the
//      document calls `stopImmediatePropagation()` so registered
//      handlers in the page never see the event fire when the OS
//      reports the tab as hidden.
//
// What this does NOT fix: browser-level throttling of timers, RAF, and
// task scheduling. Chromium decides those independently of what
// `document.hidden` reports to JavaScript. If claude.ai's poll cadence
// turns out to be throttled at that layer, the spoof will not help —
// in which case the audio-loop trick (Tier 2) is the next move.
//
// MUST run before any other script reads visibility. The userscript
// header is `@run-at document-start` for that reason; this function
// is called synchronously at the top of the IIFE.
//
// Parameterized on `doc` for testability — pass a stub document in
// unit tests; the userscript passes the real `document`.

function installVisibilitySpoof(doc) {
    if (!doc) return;

    function defineAlwaysVisible(propName, value) {
        try {
            Object.defineProperty(doc, propName, {
                configurable: true,
                get() { return value; },
            });
        } catch (_) {
            // Some hosts may have already locked the property as
            // non-configurable; we silently no-op rather than abort
            // the rest of the spoof.
        }
    }

    defineAlwaysVisible('hidden', false);
    defineAlwaysVisible('visibilityState', 'visible');
    defineAlwaysVisible('webkitHidden', false);
    defineAlwaysVisible('webkitVisibilityState', 'visible');

    // Suppress visibilitychange events at the capture phase before
    // any application-registered listener on `document` can observe
    // them. (visibilitychange does not bubble to window, so listeners
    // there are out of scope of this concern.) We also cover the
    // prefixed variant.
    if (typeof doc.addEventListener === 'function') {
        const swallow = (e) => {
            if (typeof e.stopImmediatePropagation === 'function') {
                e.stopImmediatePropagation();
            }
            if (typeof e.stopPropagation === 'function') {
                e.stopPropagation();
            }
        };
        doc.addEventListener('visibilitychange', swallow, true);
        doc.addEventListener('webkitvisibilitychange', swallow, true);
    }
}

if (typeof module !== 'undefined') {
    module.exports = { installVisibilitySpoof };
}
