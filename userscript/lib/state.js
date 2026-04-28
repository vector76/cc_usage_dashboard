'use strict';

// Persistent record of the most recent successfully-sent observation.
// Read by downstream dedup/continuity logic to decide whether the next
// observation is a duplicate or a fresh datapoint after a wall-clock gap.
//
// This file is the single source of truth. Its function bodies are also
// inlined into ../claude-usage-snapshot.user.js so Tampermonkey runs them
// without a build step; tests load them via require() from here.

const STATE_STORAGE_KEY = 'claude-usage-snapshot.state.v1';

let _storageOverride = null;

function _resolveStorage() {
    if (_storageOverride) return _storageOverride;
    if (typeof globalThis !== 'undefined' && globalThis.localStorage) {
        return globalThis.localStorage;
    }
    return null;
}

// Test seam — never called from the userscript IIFE. Tests pass an
// in-memory stub so we never have to mutate globalThis.localStorage.
function _setStorageForTests(stub) {
    _storageOverride = stub;
}

function loadState() {
    try {
        const storage = _resolveStorage();
        if (!storage) return null;
        const raw = storage.getItem(STATE_STORAGE_KEY);
        if (raw == null) return null;
        const parsed = JSON.parse(raw);
        if (!parsed || typeof parsed !== 'object') return null;
        if (typeof parsed.lastSentAtMs !== 'number') return null;
        const result = {
            lastSentAtMs: parsed.lastSentAtMs,
            lastPercent: parsed.lastPercent,
            lastResetText: parsed.lastResetText,
            lastWindowEndsMs: parsed.lastWindowEndsMs,
        };
        if (parsed.lastSessionActive !== undefined) result.lastSessionActive = parsed.lastSessionActive;
        return result;
    } catch (_) {
        return null;
    }
}

function recordSentState({ sentAtMs, percent, resetText, windowEndsMs, sessionActive }) {
    try {
        const storage = _resolveStorage();
        if (!storage) return;
        const record = {
            lastSentAtMs: sentAtMs,
            lastPercent: percent,
            lastResetText: resetText,
            lastWindowEndsMs: windowEndsMs,
        };
        if (sessionActive !== undefined) record.lastSessionActive = sessionActive;
        storage.setItem(STATE_STORAGE_KEY, JSON.stringify(record));
    } catch (_) {
        // Persistence is best-effort; never throw out of the dispatch path.
    }
}

if (typeof module !== 'undefined') {
    module.exports = {
        STATE_STORAGE_KEY,
        loadState,
        recordSentState,
        _setStorageForTests,
    };
}
