import { test } from 'node:test';
import assert from 'node:assert/strict';
import { classifyBlocked } from './pingachock-client';

// Pure logic, no network/db - unlike pingachock-client.test.ts this needs
// no live backend or env vars, so a plain `import` (not `require`) is fine.

test('domain resolving to 127.0.0.1 is blocked', () => {
  assert.equal(classifyBlocked('fnyk.ru', '127.0.0.1'), true);
});

test('domain resolving elsewhere in 127.0.0.0/8 is blocked', () => {
  assert.equal(classifyBlocked('fnyk.ru', '127.0.0.5'), true);
});

test('domain resolving to ::1 is blocked', () => {
  assert.equal(classifyBlocked('fnyk.ru', '::1'), true);
});

test('domain resolving to a real IP is not blocked', () => {
  assert.equal(classifyBlocked('example.com', '93.184.216.34'), false);
});

test('raw IPv4 target is exempt even if somehow "resolved" to loopback', () => {
  assert.equal(classifyBlocked('127.0.0.1', '127.0.0.1'), false);
});

test('raw IPv6 target is exempt', () => {
  assert.equal(classifyBlocked('::1', '::1'), false);
});

test('no resolved IP yet (still just the original hostname) is not blocked', () => {
  assert.equal(classifyBlocked('fnyk.ru', 'fnyk.ru'), false);
});

test('empty resolved IP is not blocked', () => {
  assert.equal(classifyBlocked('fnyk.ru', ''), false);
});
