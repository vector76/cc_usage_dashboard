'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

// Tests for userscript code that has no lib/ mirror (diagnostics,
// transport, bootstrap). Each function is extracted from the shipped
// script and evaluated against stubs passed in as its free variables.

const USERSCRIPT = fs.readFileSync(
    path.join(__dirname, '..', 'claude-usage-snapshot.user.js'),
    'utf8',
);

function extractFunction(name) {
    const m = USERSCRIPT.match(new RegExp(`^    function ${name}\\([^)]*\\) \\{[\\s\\S]*?\\n {4}\\}`, 'm'));
    assert.ok(m, `function ${name} not found in the userscript`);
    return m[0];
}

// Evaluate the named functions with `scope` supplying every free variable
// they read, and return the function called `exportName`.
function load(functions, exportName, scope) {
    const names = Object.keys(scope);
    const src = functions.map(extractFunction).join('\n');
    // eslint-disable-next-line no-new-func
    return new Function(...names, `${src}\nreturn ${exportName};`)(...names.map(n => scope[n]));
}

// ---------- buildFingerprint ----------

function stubNode(text) {
    return { textContent: text, getAttribute: () => null };
}

function stubContainer(headings) {
    return {
        querySelectorAll: sel => (sel === 'h2, h3' ? headings : []),
    };
}

function fingerprintFor({ pathname, hash, documentHeadings, dialogHeadings }) {
    const dialog = dialogHeadings ? stubContainer(dialogHeadings) : null;
    const document = {
        querySelector: sel => (dialog && /dialog/.test(sel) ? dialog : null),
        querySelectorAll: sel => (sel === 'h2, h3' ? documentHeadings : []),
        getElementById: () => null,
    };
    const buildFingerprint = load(['resolveBarLabel', 'buildFingerprint'], 'buildFingerprint', {
        document,
        location: { pathname, hash },
        navigator: { userAgent: 'test-agent' },
        USAGE_BAR_SELECTOR: '[role="meter"][aria-valuenow]',
        USAGE_PATH: '/settings/usage',
    });
    return JSON.parse(buildFingerprint());
}

test('fingerprint: conversation headings under the settings modal are not shipped', () => {
    const usage = stubNode('Your usage');
    const fp = fingerprintFor({
        pathname: '/chat/0b9f3c2e-1111-2222-3333-444455556666',
        hash: '#settings/usage',
        documentHeadings: [stubNode('Draft of my resignation letter'), usage],
        dialogHeadings: [usage],
    });
    assert.deepStrictEqual(fp.heading_texts, ['Your usage']);
    assert.strictEqual(fp.heading_count, 1);
    assert.ok(!JSON.stringify(fp).includes('resignation'));
});

test('fingerprint: the conversation id in the pathname is not shipped', () => {
    const fp = fingerprintFor({
        pathname: '/chat/0b9f3c2e-1111-2222-3333-444455556666',
        hash: '#settings/usage',
        documentHeadings: [],
        dialogHeadings: [],
    });
    assert.strictEqual(fp.pathname, '/chat');
});

test('fingerprint: modal route with no dialog found ships no headings', () => {
    const fp = fingerprintFor({
        pathname: '/new',
        hash: '#settings/usage',
        documentHeadings: [stubNode('Some chat heading')],
        dialogHeadings: null,
    });
    assert.deepStrictEqual(fp.heading_texts, []);
    assert.strictEqual(fp.pathname, '/new');
});

test('fingerprint: the legacy full-page route still reports document headings', () => {
    // No conversation is mounted under /settings/usage, so the document is
    // the settings page itself.
    const fp = fingerprintFor({
        pathname: '/settings/usage',
        hash: '',
        documentHeadings: [stubNode('Plan usage limits'), stubNode('Weekly limits')],
        dialogHeadings: null,
    });
    assert.deepStrictEqual(fp.heading_texts, ['Plan usage limits', 'Weekly limits']);
    assert.strictEqual(fp.pathname, '/settings');
});

// ---------- start / scheduleDispatch ----------

function bootstrapHarness({ dispatchThrows = false, observerThrows = false } = {}) {
    const calls = { intervals: [], observer: 0, dispatch: 0, warns: [] };
    const scope = {
        tryDispatch: () => {
            calls.dispatch++;
            if (dispatchThrows) throw new RangeError('Invalid time value');
        },
        startChangeObserver: () => {
            calls.observer++;
            if (observerThrows) throw new TypeError('document.body is null');
        },
        setInterval: (fn, ms) => { calls.intervals.push({ fn, ms }); return 1; },
        warn: (...args) => calls.warns.push(args),
        POST_INTERVAL_MS: 60000,
    };
    return { start: load(['start'], 'start', scope), calls };
}

test('start: a throwing initial sample still arms the observer and the interval', () => {
    const { start, calls } = bootstrapHarness({ dispatchThrows: true });
    try { start(); } catch (_) { /* asserted below */ }
    assert.strictEqual(calls.dispatch, 1);
    assert.strictEqual(calls.observer, 1);
    assert.strictEqual(calls.intervals.length, 1);
    assert.strictEqual(calls.intervals[0].ms, 60000);
});

test('start: a throwing observer setup still arms the interval and samples', () => {
    const { start, calls } = bootstrapHarness({ observerThrows: true });
    try { start(); } catch (_) { /* asserted below */ }
    assert.strictEqual(calls.intervals.length, 1);
    assert.strictEqual(calls.dispatch, 1);
});

test('start: failures are logged, not thrown, and the interval tick is guarded', () => {
    const { start, calls } = bootstrapHarness({ dispatchThrows: true });
    assert.doesNotThrow(() => start());
    assert.ok(calls.warns.length >= 1);
    // The backstop tick must survive the same throw.
    assert.doesNotThrow(() => calls.intervals[0].fn());
    assert.strictEqual(calls.dispatch, 2);
});

test('start: happy path samples once and arms both triggers', () => {
    const { start, calls } = bootstrapHarness();
    start();
    assert.strictEqual(calls.dispatch, 1);
    assert.strictEqual(calls.observer, 1);
    assert.strictEqual(calls.intervals.length, 1);
    assert.deepStrictEqual(calls.warns, []);
});

test('scheduleDispatch: a throwing debounced dispatch is logged and does not wedge the debounce', () => {
    const timers = [];
    const warns = [];
    let dispatches = 0;
    const src = extractFunction('scheduleDispatch');
    // eslint-disable-next-line no-new-func
    const scheduleDispatch = new Function('setTimeout', 'tryDispatch', 'warn', 'DISPATCH_DEBOUNCE_MS',
        `let dispatchTimer = null;\n${src}\nreturn scheduleDispatch;`)(
        (fn) => { timers.push(fn); return timers.length; },
        () => { dispatches++; throw new Error('boom'); },
        (...args) => warns.push(args),
        250,
    );
    scheduleDispatch();
    assert.doesNotThrow(() => timers[0]());
    assert.strictEqual(warns.length, 1);
    scheduleDispatch();
    assert.strictEqual(timers.length, 2, 'debounce re-arms after a failed dispatch');
    assert.strictEqual(dispatches, 1);
});

// ---------- postJSON ----------

function postJSONWith(returnValue) {
    const warns = [];
    const GM = { xmlHttpRequest: () => returnValue };
    const postJSON = load(['postJSON'], 'postJSON', { GM, warn: (...a) => warns.push(a) });
    return { postJSON, warns };
}

test('postJSON: a Promise-returning GM.xmlHttpRequest gets a rejection handler', () => {
    // GM4-style managers return a Promise that rejects on error/timeout/abort
    // alongside the callbacks; unhandled, each failed retry logs
    // "Uncaught (in promise)" on claude.ai.
    const rejectionHandlers = [];
    const thenable = {
        then(onFulfilled, onRejected) {
            if (typeof onRejected === 'function') rejectionHandlers.push(onRejected);
            return thenable;
        },
        catch(onRejected) {
            if (typeof onRejected === 'function') rejectionHandlers.push(onRejected);
            return thenable;
        },
    };
    const { postJSON, warns } = postJSONWith(thenable);
    postJSON('http://localhost:27812/snapshot', { a: 1 });
    assert.strictEqual(rejectionHandlers.length, 1);
    assert.doesNotThrow(() => rejectionHandlers[0](new Error('network')));
    assert.deepStrictEqual(warns, []);
});

test('postJSON: a real rejected Promise does not surface as an unhandled rejection', async () => {
    let unhandled = null;
    const onUnhandled = (reason) => { unhandled = reason; };
    process.on('unhandledRejection', onUnhandled);
    try {
        const { postJSON } = postJSONWith(Promise.reject(new Error('timeout')));
        postJSON('http://localhost:27812/snapshot', { a: 1 });
        await new Promise(resolve => setImmediate(resolve));
        await new Promise(resolve => setImmediate(resolve));
    } finally {
        process.removeListener('unhandledRejection', onUnhandled);
    }
    assert.strictEqual(unhandled, null);
});

test('postJSON: legacy non-Promise return values are left alone', () => {
    for (const ret of [undefined, null, { abort() {} }]) {
        const { postJSON, warns } = postJSONWith(ret);
        assert.doesNotThrow(() => postJSON('http://localhost:27812/snapshot', {}));
        assert.deepStrictEqual(warns, []);
    }
});
