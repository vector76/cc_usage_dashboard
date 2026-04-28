'use strict';

const test = require('node:test');
const assert = require('node:assert');

const {
    STATE_STORAGE_KEY,
    loadState,
    recordSentState,
    _setStorageForTests,
} = require('../lib/state');

function makeMemoryStorage(initial) {
    const data = new Map(Object.entries(initial || {}));
    return {
        data,
        getItem(key) {
            return data.has(key) ? data.get(key) : null;
        },
        setItem(key, value) {
            data.set(key, String(value));
        },
        removeItem(key) {
            data.delete(key);
        },
    };
}

function makeThrowingStorage(method) {
    const stub = makeMemoryStorage();
    stub[method] = () => {
        throw new Error('storage exploded');
    };
    return stub;
}

test.afterEach(() => {
    _setStorageForTests(null);
});

test('round-trip: recordSentState then loadState returns the record', () => {
    const stub = makeMemoryStorage();
    _setStorageForTests(stub);

    const record = {
        sentAtMs: 1714200000000,
        percent: 42,
        resetText: 'Resets in 3 hr 33 min',
        windowEndsMs: 1714212600000,
    };
    recordSentState(record);

    assert.deepStrictEqual(loadState(), {
        lastSentAtMs: record.sentAtMs,
        lastPercent: record.percent,
        lastResetText: record.resetText,
        lastWindowEndsMs: record.windowEndsMs,
    });
});

test('loadState returns null when storage value is malformed JSON', () => {
    const stub = makeMemoryStorage({ [STATE_STORAGE_KEY]: '{not json' });
    _setStorageForTests(stub);

    assert.strictEqual(loadState(), null);
});

test('loadState returns null when storage is empty', () => {
    _setStorageForTests(makeMemoryStorage());

    assert.strictEqual(loadState(), null);
});

test('loadState ignores values written under a different version key', () => {
    const olderKey = 'claude-usage-snapshot.state.v0';
    const stub = makeMemoryStorage({
        [olderKey]: JSON.stringify({
            lastSentAtMs: 1,
            lastPercent: 1,
            lastResetText: 'x',
            lastWindowEndsMs: 2,
        }),
    });
    _setStorageForTests(stub);

    assert.strictEqual(loadState(), null);
});

test('loadState returns null when getItem throws, without escaping', () => {
    _setStorageForTests(makeThrowingStorage('getItem'));

    assert.doesNotThrow(() => {
        const result = loadState();
        assert.strictEqual(result, null);
    });
});

test('recordSentState swallows setItem exceptions', () => {
    _setStorageForTests(makeThrowingStorage('setItem'));

    assert.doesNotThrow(() => {
        recordSentState({
            sentAtMs: 1,
            percent: 2,
            resetText: 'r',
            windowEndsMs: 3,
        });
    });
});

test('loadState returns null when persisted record is missing lastSentAtMs', () => {
    const stub = makeMemoryStorage({
        [STATE_STORAGE_KEY]: JSON.stringify({ lastPercent: 50 }),
    });
    _setStorageForTests(stub);

    assert.strictEqual(loadState(), null);
});
