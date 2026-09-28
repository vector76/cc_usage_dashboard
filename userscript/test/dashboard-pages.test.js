'use strict';

// The dashboard and report pages keep their fetch/poll logic in an inline
// <script>, so it is not require()-able like grouping.js and summary.js.
// These tests pull that inline block out of the HTML and run it in a vm
// context against a minimal fake DOM, a scripted fetch and hand-driven
// timers — enough to exercise what the page does when a request fails,
// stalls, or answers out of order. Rendering details (chart geometry, table
// layout) are deliberately not asserted here.

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const STATIC_DIR = path.join(__dirname, '..', '..', 'internal', 'dashboard', 'static');

// --- fake DOM -------------------------------------------------------------

class FakeText {
    constructor(text) {
        this.textContent = String(text);
        this.parentNode = null;
    }
}

class FakeNode {
    constructor(tagName) {
        this.tagName = tagName;
        this.childNodes = [];
        this.parentNode = null;
        this.attrs = {};
        this.style = {};
        this.dataset = {};
        this.listeners = {};
        this.className = '';
        this.hidden = false;
        this.value = '';
        this.open = false;
        this.title = '';
    }

    get firstChild() { return this.childNodes[0] || null; }

    appendChild(c) {
        if (c.parentNode) c.parentNode.removeChild(c);
        this.childNodes.push(c);
        c.parentNode = this;
        return c;
    }

    removeChild(c) {
        const i = this.childNodes.indexOf(c);
        if (i !== -1) this.childNodes.splice(i, 1);
        c.parentNode = null;
        return c;
    }

    setAttribute(k, v) {
        this.attrs[k] = String(v);
        if (k === 'class') this.className = String(v);
    }

    getAttribute(k) {
        return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
    }

    addEventListener(type, fn) {
        (this.listeners[type] = this.listeners[type] || []).push(fn);
    }

    dispatch(type) {
        for (const fn of this.listeners[type] || []) fn({ target: this, type });
    }

    get classList() {
        const self = this;
        const list = () => self.className.split(/\s+/).filter(Boolean);
        const set = (names) => { self.className = names.join(' '); };
        return {
            contains: (c) => list().includes(c),
            add: (c) => { if (!list().includes(c)) set(list().concat(c)); },
            remove: (c) => set(list().filter((x) => x !== c)),
            toggle: (c, force) => {
                const has = list().includes(c);
                const want = force === undefined ? !has : Boolean(force);
                if (want && !has) set(list().concat(c));
                if (!want && has) set(list().filter((x) => x !== c));
                return want;
            },
        };
    }

    get textContent() {
        return this.childNodes.map((c) => c.textContent).join('');
    }

    set textContent(v) {
        for (const c of this.childNodes) c.parentNode = null;
        this.childNodes = [];
        const s = v == null ? '' : String(v);
        if (s !== '') this.appendChild(new FakeText(s));
    }
}

// makeDocument knows every element the page declares with an id="...",
// seeded with its class and hidden attribute; anything else is null, as in
// a browser.
function makeDocument(html) {
    const byId = new Map();
    for (const m of html.matchAll(/<(\w+)\b([^>]*)>/g)) {
        const attrs = m[2];
        const id = attrs.match(/\bid="([^"]+)"/);
        if (!id) continue;
        const n = new FakeNode(m[1]);
        n.id = id[1];
        const cls = attrs.match(/\bclass="([^"]*)"/);
        if (cls) n.className = cls[1];
        if (/\bhidden\b/.test(attrs)) n.hidden = true;
        byId.set(id[1], n);
    }
    return {
        getElementById: (id) => byId.get(id) || null,
        createElement: (t) => new FakeNode(t),
        createElementNS: (ns, t) => new FakeNode(t),
        createTextNode: (t) => new FakeText(t),
        querySelectorAll(sel) {
            const m = sel.match(/^#(\S+) (\w+)$/);
            assert.ok(m, `fake querySelectorAll cannot parse ${sel}`);
            const host = byId.get(m[1]);
            return host ? host.childNodes.filter((c) => c.tagName === m[2]) : [];
        },
    };
}

// --- timers and fetch -------------------------------------------------------

function makeTimers() {
    let next = 1;
    const timeouts = new Map();
    const intervals = new Map();
    return {
        setTimeout: (fn, ms) => { const id = next++; timeouts.set(id, { fn, ms }); return id; },
        clearTimeout: (id) => { timeouts.delete(id); },
        setInterval: (fn, ms) => { const id = next++; intervals.set(id, { fn, ms }); return id; },
        clearInterval: (id) => { intervals.delete(id); },
        // tick runs every registered interval callback once, as if its period
        // had elapsed.
        tick() { for (const { fn } of [...intervals.values()]) fn(); },
        // expireTimeouts fires every pending timeout, as if the longest had
        // elapsed.
        expireTimeouts() {
            const all = [...timeouts.values()];
            timeouts.clear();
            for (const { fn } of all) fn();
        },
    };
}

function jsonResponse(body, status = 200) {
    return { ok: status >= 200 && status < 300, status, json: async () => body };
}

function textResponse(status) {
    return {
        ok: status >= 200 && status < 300,
        status,
        json: async () => { throw new SyntaxError('Unexpected token p in JSON at position 0'); },
    };
}

function deferred() {
    let resolve, reject;
    const promise = new Promise((a, b) => { resolve = a; reject = b; });
    return { promise, resolve, reject };
}

function networkError() {
    return new TypeError('Failed to fetch');
}

async function flush() {
    for (let i = 0; i < 30; i++) await new Promise((r) => setImmediate(r));
}

// loadPage evaluates a page's external scripts and then its inline script in
// one shared vm context. route(url) answers each fetch with a response, a
// promise of one, or by throwing (a network failure); a pending promise
// rejects with AbortError when the request's signal aborts, like fetch.
function loadPage(page, route, storage = {}) {
    const html = fs.readFileSync(path.join(STATIC_DIR, page), 'utf8');
    const inline = html.match(/<script>([\s\S]*?)<\/script>/);
    assert.ok(inline, `no inline <script> in ${page}`);

    const document = makeDocument(html);
    const timers = makeTimers();
    const calls = [];
    const fetch = (url, opts) => {
        calls.push(url);
        return new Promise((resolve, reject) => {
            const signal = opts && opts.signal;
            if (signal) {
                signal.addEventListener('abort', () => {
                    const e = new Error('The operation was aborted.');
                    e.name = 'AbortError';
                    reject(e);
                });
            }
            Promise.resolve().then(() => route(url)).then(resolve, reject);
        });
    };
    const ctx = vm.createContext({
        document,
        fetch,
        console: { log() {}, warn() {}, error() {} },
        localStorage: {
            getItem: (k) => (Object.prototype.hasOwnProperty.call(storage, k) ? storage[k] : null),
            setItem: (k, v) => { storage[k] = String(v); },
        },
        AbortController,
        URLSearchParams,
        Element: FakeNode,
        setTimeout: timers.setTimeout,
        clearTimeout: timers.clearTimeout,
        setInterval: timers.setInterval,
        clearInterval: timers.clearInterval,
    });
    for (const m of html.matchAll(/<script src="\/([\w.]+)"><\/script>/g)) {
        vm.runInContext(fs.readFileSync(path.join(STATIC_DIR, m[1]), 'utf8'), ctx, { filename: m[1] });
    }
    vm.runInContext(inline[1], ctx, { filename: page });
    return { document, timers, calls, byId: (id) => document.getElementById(id) };
}

// --- fixtures ---------------------------------------------------------------

function goodState() {
    return {
        now: '2026-09-28T12:00:00Z',
        paused: false,
        slack_release_recommended: true,
        last_snapshot_age_seconds: 5,
        session: {
            kind: 'session',
            started_at: '2026-09-28T10:00:00Z',
            ends_at: '2026-09-28T15:00:00Z',
            series: [{ observed_at: '2026-09-28T11:00:00Z', percent_used: 10, continuous_with_prev: false }],
            volume: [],
            bucket_secs: 900,
        },
        weekly: {
            kind: 'weekly',
            started_at: '2026-09-25T10:00:00Z',
            ends_at: '2026-10-02T10:00:00Z',
            series: [{ observed_at: '2026-09-28T11:00:00Z', percent_used: 30, continuous_with_prev: false }],
            volume: [],
            bucket_secs: 21600,
        },
        slack_profiles: {},
    };
}

function consumption(period, usd) {
    return { period, consumed_usd_equivalent: usd, consumed_session_pct: 50, consumed_weekly_pct: 20 };
}

function feedback(warnings) {
    const w = [];
    for (let i = 0; i < warnings; i++) w.push({ time: '2026-09-28T11:00:00Z', level: 'WARN', message: 'w' + i });
    return {
        warnings: w,
        unknown_models: [],
        parse_errors: [],
        summary: { warnings, unknown_models: 0, unknown_model_events: 0, parse_errors: 0 },
    };
}

// dashboardRoutes answers every endpoint successfully unless overridden.
function dashboardRoutes(over = {}) {
    return (url) => {
        for (const [prefix, fn] of Object.entries(over)) {
            if (url.startsWith(prefix)) return fn(url);
        }
        if (url.startsWith('/api/dashboard/state')) return jsonResponse(goodState());
        if (url.startsWith('/consumption')) {
            const period = new URL(url, 'http://x').searchParams.get('period');
            return jsonResponse(consumption(period, 1));
        }
        if (url.startsWith('/api/feedback')) return jsonResponse(feedback(0));
        throw new Error('unexpected fetch ' + url);
    };
}

// --- dashboard (index.html) -------------------------------------------------

// U07-1: once the server stops answering, the page must say so rather than
// go on showing the last good poll — in particular a "yes" slack flag and a
// server-supplied snapshot age that no longer moves.
test('dashboard marks its state stale while polls fail, and recovers', async () => {
    let down = false;
    const fail = (url) => { if (down) throw networkError(); return dashboardRoutes()(url); };
    const page = loadPage('index.html', fail);
    await flush();

    assert.strictEqual(page.byId('slack-release').textContent, 'yes');
    const note = page.byId('stale-note');
    assert.ok(note, 'index.html has no #stale-note element to report a failed poll');
    assert.strictEqual(note.hidden, true, 'stale note shown while polls succeed');

    down = true;
    page.timers.tick();
    await flush();

    assert.strictEqual(note.hidden, false, 'no stale note after a failed poll');
    assert.match(note.textContent, /unreachable/i);
    assert.notStrictEqual(page.byId('slack-release').textContent, 'yes',
        'a stale "yes" is still shown as the slack verdict');
    assert.ok(page.byId('snapshot-line').classList.contains('stale'), 'snapshot line not dimmed');
    assert.match(page.byId('consumed-line').textContent, /unavailable/,
        'consumption keeps showing the last good numbers after its fetch failed');

    down = false;
    page.timers.tick();
    await flush();

    assert.strictEqual(note.hidden, true, 'stale note not cleared after the server recovered');
    assert.strictEqual(page.byId('slack-release').textContent, 'yes');
    assert.ok(!page.byId('snapshot-line').classList.contains('stale'));
});

// U07-2: a 500 with a JSON error body parses fine, so it must be rejected by
// status, not drawn as state/feedback with every field missing.
test('dashboard treats an HTTP error body as a failure, not as empty data', async () => {
    const page = loadPage('index.html', dashboardRoutes({
        '/api/dashboard/state': () => jsonResponse({ error: 'state computation failed' }, 500),
        '/api/feedback': () => jsonResponse({ error: 'database error' }, 500),
    }));
    await flush();

    assert.doesNotMatch(page.byId('snapshot-line').textContent, /none yet/,
        'a 500 is reported as "the userscript never delivered a snapshot"');
    const note = page.byId('stale-note');
    assert.ok(note, 'index.html has no #stale-note element');
    assert.strictEqual(note.hidden, false);
    assert.match(note.textContent, /500/);

    assert.notStrictEqual(page.byId('fb-badge').textContent, '(none)',
        'a feedback 500 reads as a clean install');
    assert.match(page.byId('fb-badge').textContent, /unavailable/);

    const panel = page.byId('feedback');
    panel.open = true;
    panel.dispatch('toggle');
    await flush();
    const body = page.byId('fb-body').textContent;
    assert.doesNotMatch(body, /every model was priced/, 'error body rendered as an empty feedback list');
    assert.match(body, /Could not load/);
});

test('dashboard does not overwrite cached feedback with an error body', async () => {
    let broken = false;
    const page = loadPage('index.html', dashboardRoutes({
        '/api/feedback': () => (broken
            ? jsonResponse({ error: 'database error' }, 500)
            : jsonResponse(feedback(2))),
    }));
    await flush();
    assert.match(page.byId('fb-badge').textContent, /2 warnings/);

    broken = true;
    page.timers.tick();
    await flush();

    // The panel renders the cache synchronously on expand, before its own
    // refetch fails — so the cache must still be the last good payload.
    const panel = page.byId('feedback');
    panel.open = true;
    const origFetchCount = page.calls.length;
    panel.dispatch('toggle');
    const bodyAtExpand = page.byId('fb-body').textContent;
    assert.ok(page.calls.length > origFetchCount);
    assert.match(bodyAtExpand, /Recent warnings \(2\)/, 'cached feedback was replaced by the error body');
});

// U07-3: the period picker fires its own request while the poll's may still
// be in flight; the stale one must not land last and win.
test('consumption renders only the latest period request', async () => {
    const slow30d = deferred();
    const page = loadPage('index.html', dashboardRoutes({
        '/consumption?period=30d': () => slow30d.promise,
        '/consumption?period=24h': () => jsonResponse(consumption('24h', 1)),
    }), { 'cc-dashboard-consumption-period': '30d' });
    await flush();
    assert.ok(page.calls.includes('/consumption?period=30d'));

    const sel = page.byId('period');
    sel.value = '24h';
    sel.dispatch('change');
    await flush();
    assert.match(page.byId('consumed-line').textContent, /\$1\.00/);

    slow30d.resolve(jsonResponse(consumption('30d', 30)));
    await flush();
    assert.match(page.byId('consumed-line').textContent, /\$1\.00/,
        'the superseded 30d response replaced the 24h numbers under a "last 24 hours" picker');
});

// U07-6: a stalled server must not accumulate one pending poll per tick, and
// a stalled request must eventually give up and surface as stale.
test('poll skips ticks while a cycle is in flight and times out a stalled request', async () => {
    let stall = false;
    const page = loadPage('index.html', dashboardRoutes({
        '/api/dashboard/state': () => (stall ? new Promise(() => {}) : jsonResponse(goodState())),
    }));
    await flush();

    stall = true;
    const stateCalls = () => page.calls.filter((u) => u === '/api/dashboard/state').length;
    const before = stateCalls();
    page.timers.tick();
    await flush();
    page.timers.tick();
    await flush();
    page.timers.tick();
    await flush();
    assert.strictEqual(stateCalls() - before, 1, 'each tick started another state request while one hung');

    page.timers.expireTimeouts();
    await flush();
    const note = page.byId('stale-note');
    assert.ok(note, 'index.html has no #stale-note element');
    assert.strictEqual(note.hidden, false, 'a timed-out poll is not reported');
    assert.match(note.textContent, /timed out/);

    page.timers.tick();
    await flush();
    assert.strictEqual(stateCalls() - before, 2, 'polling did not resume after the timeout');
});

test('an older feedback response does not overwrite a newer one', async () => {
    let held = null;
    const page = loadPage('index.html', dashboardRoutes({
        '/api/feedback': () => {
            if (held === null) return jsonResponse(feedback(0));
            if (held === 'arm') {
                held = deferred();
                return held.promise;
            }
            return jsonResponse(feedback(3));
        },
    }));
    await flush();

    held = 'arm';
    page.timers.tick(); // poll-driven feedback request hangs
    await flush();
    const pollReq = held;
    assert.ok(pollReq && pollReq.promise, 'poll did not request feedback');

    const panel = page.byId('feedback');
    panel.open = true;
    panel.dispatch('toggle'); // newer request answers at once
    await flush();
    assert.match(page.byId('fb-badge').textContent, /3 warnings/);

    pollReq.resolve(jsonResponse(feedback(1)));
    await flush();
    assert.match(page.byId('fb-badge').textContent, /3 warnings/,
        'the older poll-driven feedback response replaced the newer one');
});

// --- report (report.html) ---------------------------------------------------

function breakdown(total, start, end) {
    return {
        start, end,
        total_cost_usd: total,
        measured_cost_usd: total,
        estimated_cost_usd: 0,
        events_total: 1,
        events_without_cost: 0,
        models: [{
            model: 'claude-opus-4-8', family: 'opus', estimated: false,
            input_tokens: 1, output_tokens: 1, cache_creation_5m_tokens: 0,
            cache_creation_1h_tokens: 0, cache_read_tokens: 0,
            events: 1, events_without_cost: 0, cost_usd: total,
        }],
    };
}

function localInput(iso) {
    const d = new Date(iso);
    const p = (n) => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
        'T' + p(d.getHours()) + ':' + p(d.getMinutes());
}

function presetButton(page, id) {
    const b = page.byId('presets').childNodes.find((c) => c.dataset.preset === id);
    assert.ok(b, `no preset button ${id}`);
    return b;
}

// reportPage loads report.html with every breakdown request answered by the
// next handler in `queue` (the arrival load's 24h request included).
function reportPage(queue) {
    return loadPage('report.html', (url) => {
        assert.ok(url.startsWith('/api/usage/breakdown?'), 'unexpected fetch ' + url);
        const next = queue.shift();
        assert.ok(next, 'unexpected extra breakdown request ' + url);
        return next(url);
    });
}

const R24 = ['2026-09-27T12:00:00Z', '2026-09-28T12:00:00Z'];
const R30 = ['2026-08-29T12:00:00Z', '2026-09-28T12:00:00Z'];

// U07-4: the last request made wins, not the last response to arrive.
test('report renders the latest request, not the slowest one', async () => {
    const slow = deferred();
    const page = reportPage([
        () => jsonResponse(breakdown(1, ...R24)),
        () => slow.promise, // Last 30d
        () => jsonResponse(breakdown(2, ...R24)), // Last 24h
    ]);
    await flush();

    presetButton(page, '30d').dispatch('click');
    await flush();
    presetButton(page, '24h').dispatch('click');
    await flush();
    assert.match(page.byId('totals').textContent, /\$2\.00/);

    slow.resolve(jsonResponse(breakdown(30, ...R30)));
    await flush();
    assert.match(page.byId('totals').textContent, /\$2\.00/,
        'the superseded 30d response replaced the 24h result');
    assert.strictEqual(page.byId('from').value, localInput(R24[0]),
        'the superseded response overwrote the From input');
    assert.strictEqual(presetButton(page, '24h').getAttribute('aria-pressed'), 'true');
});

test('report ignores a late failure from a superseded request', async () => {
    const slow = deferred();
    const page = reportPage([
        () => jsonResponse(breakdown(1, ...R24)),
        () => slow.promise,
        () => jsonResponse(breakdown(2, ...R24)),
    ]);
    await flush();

    presetButton(page, '30d').dispatch('click');
    await flush();
    presetButton(page, '24h').dispatch('click');
    await flush();
    slow.reject(networkError());
    await flush();

    assert.strictEqual(page.byId('range-err').hidden, true,
        'a superseded request\'s failure is shown over the newer result');
    assert.match(page.byId('totals').textContent, /\$2\.00/);
});

// U07-5: a failed request must not leave the previous range's numbers under
// the newly pressed preset.
test('report clears the previous result when a request fails', async () => {
    const page = reportPage([
        () => jsonResponse(breakdown(1, ...R24)),
        () => { throw networkError(); }, // This week
    ]);
    await flush();
    assert.match(page.byId('totals').textContent, /\$1\.00/);

    presetButton(page, 'week').dispatch('click');
    await flush();
    assert.strictEqual(page.byId('range-err').hidden, false);
    assert.strictEqual(page.byId('totals').textContent, '', 'previous totals left under the failed preset');
    assert.strictEqual(page.byId('table-host').textContent, '', 'previous table left under the failed preset');
});

test('report says the server answered when an error body is not JSON', async () => {
    const page = reportPage([
        () => jsonResponse(breakdown(1, ...R24)),
        () => textResponse(404),
    ]);
    await flush();

    presetButton(page, '7d').dispatch('click');
    await flush();
    const err = page.byId('range-err');
    assert.strictEqual(err.hidden, false);
    assert.notStrictEqual(err.textContent, 'Could not reach the server.',
        'a plain-text 404 is reported as the server being unreachable');
    assert.match(err.textContent, /404/);
    assert.strictEqual(page.byId('totals').textContent, '');
});

test('report still shows a JSON error message from the server', async () => {
    const page = reportPage([
        () => jsonResponse(breakdown(1, ...R24)),
        () => jsonResponse({ error: 'invalid range' }, 400),
    ]);
    await flush();

    presetButton(page, '7d').dispatch('click');
    await flush();
    assert.strictEqual(page.byId('range-err').textContent, 'Request rejected: invalid range');
});

// A snapshot age below zero means the reading's received_at is ahead of the
// server clock by more than the slack gate tolerates, so the gate calls it
// stale. The page must say so rather than print a negative "ago".
test('dashboard flags a snapshot timestamped in the future', async () => {
    const page = loadPage('index.html', dashboardRoutes({
        '/api/dashboard/state': () => jsonResponse({ ...goodState(), last_snapshot_age_seconds: -300 }),
    }));
    await flush();

    const line = page.byId('snapshot-line').textContent;
    assert.match(line, /in the future/i, 'future snapshot not flagged: ' + line);
    assert.doesNotMatch(line, /-\S*\s*ago/, 'negative age rendered as "ago": ' + line);
});
