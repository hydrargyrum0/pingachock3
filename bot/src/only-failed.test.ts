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

const { onlyFailed } = require('./pingachock-client') as typeof import('./pingachock-client');

test('a real reachability failure is kept', () => {
  const results = [{ ip: '1.1.1.1', status: false, blocked: false }];
  assert.deepEqual(onlyFailed(results), results);
});

test('a successful, unblocked result is dropped', () => {
  const results = [{ ip: '1.1.1.1', status: true, blocked: false }];
  assert.deepEqual(onlyFailed(results), []);
});

// This is the regression case: a DNS-poisoned domain whose loopback
// address happens to answer on the checked port (from the backend's own
// local perspective) computes status: true, but is still a censorship
// event that must not silently vanish from the periodic report.
test('a blocked result is kept even when status computed true', () => {
  const results = [{ ip: 'pornhub.com', resolved_ip: '127.0.0.1', status: true, blocked: true }];
  assert.deepEqual(onlyFailed(results), results);
});

test('mixed results keep only the failing and the blocked ones', () => {
  const ok = { ip: 'good.com', status: true, blocked: false };
  const down = { ip: 'down.com', status: false, blocked: false };
  const blocked = { ip: 'poisoned.com', status: true, blocked: true };
  assert.deepEqual(onlyFailed([ok, down, blocked]), [down, blocked]);
});
