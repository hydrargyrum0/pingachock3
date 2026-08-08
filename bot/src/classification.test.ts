import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// Pure logic, no live backend needed - but importing pingachock-client.ts
// eagerly imports db.ts (nedb), which writes into ./data/ relative to cwd
// as a side effect of module load. Point it at a throwaway dir first, same
// as pingachock-client.test.ts, so this never touches the bot's real data/
// or races another test file over the same file. require(), not import:
// the latter is always hoisted above the env-var setup below.
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pingachock-bot-test-'));
process.env.DB_PATH = path.join(tmpDir, 'users.db');
process.env.SETTINGS_DB_PATH = path.join(tmpDir, 'settings.db');

const { classifyBlocked } = require('./pingachock-client') as typeof import('./pingachock-client');

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
