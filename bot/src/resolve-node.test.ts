import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pingachock-bot-test-'));
process.env.DB_PATH = path.join(tmpDir, 'users.db');
process.env.SETTINGS_DB_PATH = path.join(tmpDir, 'settings.db');

const { pickAutoRouter } = require('./pingachock-client') as typeof import('./pingachock-client');

test('never picks a virtual node, even if it sorts first', () => {
  const routers = [
    { id: 'virtual-1', name: 'server', status: 'online', blocked: false, is_virtual: true, platform: 'server', token: undefined, last_seen: null },
    { id: 'real-1', name: 'rebro', status: 'online', blocked: false, is_virtual: false, platform: 'linux', token: undefined, last_seen: null }
  ];
  const got = pickAutoRouter(routers);
  assert.equal(got?.id, 'real-1');
});

test('skips blocked and offline routers, same as before', () => {
  const routers = [
    { id: 'a', name: 'a', status: 'offline', blocked: false, is_virtual: false, platform: '', token: undefined, last_seen: null },
    { id: 'b', name: 'b', status: 'online', blocked: true, is_virtual: false, platform: '', token: undefined, last_seen: null },
    { id: 'c', name: 'c', status: 'online', blocked: false, is_virtual: false, platform: '', token: undefined, last_seen: null }
  ];
  const got = pickAutoRouter(routers);
  assert.equal(got?.id, 'c');
});

test('returns null when nothing qualifies (no real online routers, or only a virtual one)', () => {
  assert.equal(
    pickAutoRouter([{ id: 'virtual-1', name: 'server', status: 'online', blocked: false, is_virtual: true, platform: '', token: undefined, last_seen: null }]),
    null
  );
  assert.equal(pickAutoRouter([]), null);
});
