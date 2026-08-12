import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pingachock-bot-test-'));
process.env.DB_PATH = path.join(tmpDir, 'users.db');
process.env.SETTINGS_DB_PATH = path.join(tmpDir, 'settings.db');

const { mergeUpgradeScanChecks } = require('./pingachock-client') as typeof import('./pingachock-client');

test('merges one check-with-one-run per target, in target order', () => {
  const targets = ['1.2.3.4', '5.6.7.8'];
  const checks = [
    { runs: [{ node_id: 'node-1', result: { success: true } }] },
    { runs: [{ node_id: 'node-1', result: { success: false } }] }
  ];
  const got = mergeUpgradeScanChecks(targets, checks, 'node-1');
  assert.deepEqual(got, [
    { target: '1.2.3.4', matched: true },
    { target: '5.6.7.8', matched: false }
  ]);
});

test('a check with no run for this node_id counts as not matched, not a throw', () => {
  const targets = ['1.2.3.4'];
  const checks = [{ runs: [{ node_id: 'other-node', result: { success: true } }] }];
  const got = mergeUpgradeScanChecks(targets, checks, 'node-1');
  assert.deepEqual(got, [{ target: '1.2.3.4', matched: false }]);
});

test('a check with no result at all counts as not matched', () => {
  const targets = ['1.2.3.4'];
  const checks = [{ runs: [{ node_id: 'node-1' }] }];
  const got = mergeUpgradeScanChecks(targets, checks, 'node-1');
  assert.deepEqual(got, [{ target: '1.2.3.4', matched: false }]);
});
