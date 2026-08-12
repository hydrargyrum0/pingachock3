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

const { mapVlessSpeedTestResult } = require('./pingachock-client') as typeof import('./pingachock-client');

test('successful run extracts mbps from the matching node_id run', () => {
  const check = {
    runs: [
      { node_id: 'other-node', result: { success: true, raw: '{"mbps":50}' } },
      { node_id: 'my-node', result: { success: true, raw: '{"mbps":123.45,"bytes_downloaded":10000000,"duration_ms":647}' } }
    ]
  };
  const got = mapVlessSpeedTestResult(check, 'my-node');
  assert.equal(got.success, true);
  assert.equal(got.mbps, 123.45);
});

test('failed run carries a translated error message, not the raw token', () => {
  const check = {
    runs: [{ node_id: 'my-node', result: { success: false, error_message: 'timeout' } }]
  };
  const got = mapVlessSpeedTestResult(check, 'my-node');
  assert.equal(got.success, false);
  assert.equal(got.errorMessage, 'таймаут');
});

test('no matching run for this node_id is treated as no response', () => {
  const check = { runs: [{ node_id: 'other-node', result: { success: true, raw: '{"mbps":50}' } }] };
  const got = mapVlessSpeedTestResult(check, 'my-node');
  assert.equal(got.success, false);
  assert.equal(got.errorMessage, 'нет ответа от узла');
});

test('missing raw on an otherwise-successful result still reports success with no mbps', () => {
  const check = { runs: [{ node_id: 'my-node', result: { success: true } }] };
  const got = mapVlessSpeedTestResult(check, 'my-node');
  assert.equal(got.success, true);
  assert.equal(got.mbps, undefined);
});
