import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// Pure logic - see classification.test.ts's comment on why db.ts still
// needs pointing at a throwaway dir before import.
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pingachock-bot-test-'));
process.env.DB_PATH = path.join(tmpDir, 'users.db');
process.env.SETTINGS_DB_PATH = path.join(tmpDir, 'settings.db');

const { formatIcmpSummary } = require('./pingachock-client') as typeof import('./pingachock-client');

test('all packets received shows loss count and real latency', () => {
  assert.equal(formatIcmpSummary(4, 4, 45), '4 из 4, 45 ms');
});

test('partial loss still shows the received packets\' average latency', () => {
  assert.equal(formatIcmpSummary(4, 3, 52), '3 из 4, 52 ms');
});

test('latency gets rounded to a whole number', () => {
  assert.equal(formatIcmpSummary(4, 4, 45.7), '4 из 4, 46 ms');
});

test('total loss shows 0 of N with no latency', () => {
  assert.equal(formatIcmpSummary(4, 0, null), '0 из 4');
});

test('total loss with an error message appends it', () => {
  assert.equal(formatIcmpSummary(4, 0, null, 'exit status 1'), '0 из 4 (exit status 1)');
});

test('no sent count at all (counts unavailable) falls back to just the error', () => {
  assert.equal(formatIcmpSummary(0, 0, null, 'exit status 1'), 'exit status 1');
});

test('no sent count and no error falls back to "no reply"', () => {
  assert.equal(formatIcmpSummary(0, 0, null), 'no reply');
});

test('received but no latency value at all still shows the loss count', () => {
  assert.equal(formatIcmpSummary(4, 4, undefined), '4 из 4');
});
