'use strict';

const test = require('node:test');
const assert = require('node:assert');

test('sanity: node:test runner is wired up', () => {
  assert.equal(1 + 1, 2);
});
