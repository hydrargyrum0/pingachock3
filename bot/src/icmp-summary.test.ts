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

const { formatIcmpSummary, translateCheckError } = require('./pingachock-client') as typeof import('./pingachock-client');

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

// translateCheckError: internal/checks emits stable English tokens (see
// classifyNetError/classifyPingError in the Go backend) instead of raw
// error text - this is where they become the Russian text bot users see.
test('translateCheckError maps known Go classification tokens to Russian', () => {
  assert.equal(translateCheckError('no reply'), 'нет ответа');
  assert.equal(translateCheckError('timeout'), 'таймаут');
  assert.equal(translateCheckError('dns resolution failed'), 'домен не резолвится');
});

// The TLS Handshake check's supplementary ICMP diagnosis (internal/checks/
// tls.go diagnoseUnreachable) - tells "the whole host is down" apart from
// "just this port is filtered, host answers pings fine", per the user
// report that TLS Handshake results were unclear about why a check failed.
test('translateCheckError maps the TLS Handshake ICMP-diagnosis tokens to Russian', () => {
  assert.equal(translateCheckError('ip unreachable'), 'IP адрес недоступен (не отвечает на ping)');
  assert.equal(translateCheckError('port unreachable'), 'порт недоступен (хост отвечает на ping)');
});

// A pinned interface disappearing (internal/netiface.ErrInterfaceUnavailable,
// surfaced via internal/checks.classifyNetError) is an agent/host problem,
// not a target-reachability one - distinct wording so an operator doesn't
// mistake it for every check target suddenly going down at once.
test('translateCheckError maps the pinned-interface-gone token to Russian', () => {
  assert.equal(
    translateCheckError('network interface unavailable'),
    'сетевой интерфейс узла недоступен (нужно заново запустить configure)'
  );
});

test('translateCheckError passes through an unrecognized token unchanged rather than dropping it', () => {
  assert.equal(translateCheckError('some future token'), 'some future token');
});

test('translateCheckError passes through nullish values unchanged', () => {
  assert.equal(translateCheckError(undefined), undefined);
  assert.equal(translateCheckError(null), undefined);
});

test('full pipeline: a raw error token comes out as friendly Russian text, never the old raw Go error', () => {
  assert.equal(formatIcmpSummary(4, 0, null, translateCheckError('no reply')), '0 из 4 (нет ответа)');
  assert.doesNotMatch(formatIcmpSummary(4, 0, null, translateCheckError('no reply')), /exit status/);
});
