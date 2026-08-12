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

const { parseTlsHandshakeTarget, mapTlsHandshakeResult } = require('./pingachock-client') as typeof import('./pingachock-client');

test('parses "IP SNI" separated by a space', () => {
  assert.deepEqual(parseTlsHandshakeTarget('123.123.123.123 pingachock.com'), {
    ip: '123.123.123.123',
    sni: 'pingachock.com'
  });
});

test('parses "IP, SNI" separated by a comma', () => {
  assert.deepEqual(parseTlsHandshakeTarget('123.123.123.123, pingachock.com'), {
    ip: '123.123.123.123',
    sni: 'pingachock.com'
  });
});

test('rejects a domain in the IP position', () => {
  assert.equal(parseTlsHandshakeTarget('example.com pingachock.com'), null);
});

test('rejects fewer or more than two tokens', () => {
  assert.equal(parseTlsHandshakeTarget('123.123.123.123'), null);
  assert.equal(parseTlsHandshakeTarget('123.123.123.123 pingachock.com extra'), null);
});

test('accepts a literal IPv6 address too', () => {
  assert.deepEqual(parseTlsHandshakeTarget('2001:db8::1 pingachock.com'), {
    ip: '2001:db8::1',
    sni: 'pingachock.com'
  });
});

test('successful run extracts latency_ms from the matching node_id run', () => {
  const check = {
    runs: [
      { node_id: 'other-node', result: { success: true, latency_ms: 999 } },
      { node_id: 'my-node', result: { success: true, latency_ms: 245 } }
    ]
  };
  const got = mapTlsHandshakeResult(check, 'my-node');
  assert.equal(got.success, true);
  assert.equal(got.latencyMs, 245);
});

test('failed run carries a translated error message, not the raw token', () => {
  const check = { runs: [{ node_id: 'my-node', result: { success: false, error_message: 'timeout' } }] };
  const got = mapTlsHandshakeResult(check, 'my-node');
  assert.equal(got.success, false);
  assert.equal(got.errorMessage, 'таймаут');
});

test('no matching run for this node_id is treated as no response', () => {
  const check = { runs: [{ node_id: 'other-node', result: { success: true, latency_ms: 1 } }] };
  const got = mapTlsHandshakeResult(check, 'my-node');
  assert.equal(got.success, false);
  assert.equal(got.errorMessage, 'нет ответа от узла');
});
