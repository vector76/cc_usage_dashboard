'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { installVisibilitySpoof } = require('../lib/visibility');

function makeStubDocument(initial) {
    const listeners = [];
    const doc = {
        hidden: initial.hidden,
        visibilityState: initial.visibilityState,
        webkitHidden: initial.hidden,
        webkitVisibilityState: initial.visibilityState,
        addEventListener(type, fn, capture) {
            listeners.push({ type, fn, capture: !!capture });
        },
        // Helper for tests to dispatch a synthetic visibilitychange
        // through the registered listeners in the order they were
        // attached, respecting stopImmediatePropagation.
        _dispatchVisibilityChange(type) {
            type = type || 'visibilitychange';
            let stopped = false;
            const e = {
                type,
                stopImmediatePropagation() { stopped = true; },
                stopPropagation() { /* not load-bearing for this test */ },
            };
            for (const l of listeners) {
                if (l.type !== type) continue;
                l.fn(e);
                if (stopped) return { delivered: false };
            }
            return { delivered: true };
        },
        _listeners: listeners,
    };
    return doc;
}

test('hidden getter returns false after install', () => {
    const doc = makeStubDocument({ hidden: true, visibilityState: 'hidden' });
    installVisibilitySpoof(doc);
    assert.strictEqual(doc.hidden, false);
});

test('visibilityState getter returns "visible" after install', () => {
    const doc = makeStubDocument({ hidden: true, visibilityState: 'hidden' });
    installVisibilitySpoof(doc);
    assert.strictEqual(doc.visibilityState, 'visible');
});

test('webkit-prefixed mirrors are spoofed too', () => {
    const doc = makeStubDocument({ hidden: true, visibilityState: 'hidden' });
    installVisibilitySpoof(doc);
    assert.strictEqual(doc.webkitHidden, false);
    assert.strictEqual(doc.webkitVisibilityState, 'visible');
});

test('visibilitychange events are stopped before reaching application listeners', () => {
    const doc = makeStubDocument({ hidden: true, visibilityState: 'hidden' });
    installVisibilitySpoof(doc);

    let appHandlerCalled = false;
    doc.addEventListener('visibilitychange', () => { appHandlerCalled = true; }, false);

    const result = doc._dispatchVisibilityChange();
    assert.strictEqual(appHandlerCalled, false, 'application handler must not see the event');
    assert.strictEqual(result.delivered, false);
});

test('webkit-prefixed visibilitychange is also suppressed', () => {
    const doc = makeStubDocument({ hidden: true, visibilityState: 'hidden' });
    installVisibilitySpoof(doc);

    let appHandlerCalled = false;
    doc.addEventListener('webkitvisibilitychange', () => { appHandlerCalled = true; }, false);

    const result = doc._dispatchVisibilityChange('webkitvisibilitychange');
    assert.strictEqual(appHandlerCalled, false);
    assert.strictEqual(result.delivered, false);
});

test('install registers the suppressor in capture phase', () => {
    const doc = makeStubDocument({ hidden: true, visibilityState: 'hidden' });
    installVisibilitySpoof(doc);
    const captureSuppressors = doc._listeners.filter(l =>
        (l.type === 'visibilitychange' || l.type === 'webkitvisibilitychange') && l.capture);
    assert.strictEqual(captureSuppressors.length, 2);
});

test('install on a null/undefined document is a no-op (does not throw)', () => {
    assert.doesNotThrow(() => installVisibilitySpoof(null));
    assert.doesNotThrow(() => installVisibilitySpoof(undefined));
});

test('install survives a host that has locked the property as non-configurable', () => {
    const doc = makeStubDocument({ hidden: true, visibilityState: 'hidden' });
    // Lock `hidden` so defineProperty would throw.
    Object.defineProperty(doc, 'hidden', {
        configurable: false,
        writable: false,
        value: true,
    });
    // Should not throw; should still spoof the other properties.
    assert.doesNotThrow(() => installVisibilitySpoof(doc));
    assert.strictEqual(doc.visibilityState, 'visible');
});
