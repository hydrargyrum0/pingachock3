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

const { mapUpgradeScanResults } = require('./pingachock-client') as typeof import('./pingachock-client');

test('maps a normal response', () => {
  const data = { results: [{ target: '1.2.3.4', matched: true }, { target: '5.6.7.8', matched: false }] };
  assert.deepEqual(mapUpgradeScanResults(data), [
    { target: '1.2.3.4', matched: true },
    { target: '5.6.7.8', matched: false }
  ]);
});

test('missing results array yields an empty list, not a throw', () => {
  assert.deepEqual(mapUpgradeScanResults({}), []);
  assert.deepEqual(mapUpgradeScanResults(null), []);
});

test('missing matched field on an entry defaults to false, not undefined', () => {
  assert.deepEqual(mapUpgradeScanResults({ results: [{ target: '1.2.3.4' }] }), [{ target: '1.2.3.4', matched: false }]);
});
