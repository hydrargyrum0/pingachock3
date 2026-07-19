import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// Point db.ts at a throwaway nedb directory *before* importing anything
// that touches it, so this test never reads/writes the bot's real data/.
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pingachock-bot-test-'));
process.env.DB_PATH = path.join(tmpDir, 'users.db');
process.env.SETTINGS_DB_PATH = path.join(tmpDir, 'settings.db');

// require(), not import: the latter is always hoisted above the env-var
// setup above, which db.ts needs to read at module-load time.
const { settingsRepo } = require('./db');
const client = require('./pingachock-client') as typeof import('./pingachock-client');

const API_URL = process.env.TEST_API_URL ?? 'http://localhost:8080';
const ADMIN_TOKEN = process.env.TEST_ADMIN_TOKEN ?? 'dev-admin-token';
const API_KEY = process.env.TEST_API_KEY;
const FAKE_NODE_ID = process.env.TEST_FAKE_NODE_ID;

before(async () => {
  assert.ok(API_KEY, 'TEST_API_KEY env var must be set (a real pingachock api_key)');
  await settingsRepo.setApiUrl(API_URL);
  await settingsRepo.setAdminToken(ADMIN_TOKEN);
  await settingsRepo.setApiKey(API_KEY!);
});

test('listRouters returns routers with string ids and blocked/platform fields', async () => {
  const routers = await client.listRouters();
  assert.ok(Array.isArray(routers));
  assert.ok(routers.length > 0, 'expected at least one node to exist in the dev backend');
  for (const r of routers) {
    assert.equal(typeof r.id, 'string');
    assert.equal(typeof r.blocked, 'boolean');
  }
});

test('createRouter then blockRouter round-trips through the real admin API', async () => {
  const created = await client.createRouter(`client-test-${Date.now()}`, 'isp', 'city');
  assert.equal(typeof created.id, 'string');
  assert.ok(created.token, 'createRouter must return the plaintext secret as .token');
  assert.equal(created.blocked, false);

  await client.blockRouter(created.id);
  const after = await client.getRouter(created.id);
  assert.equal(after.blocked, true);
});

test('getStatus counts online routers', async () => {
  const status = await client.getStatus();
  assert.equal(typeof status.routers_online, 'number');
  assert.equal(status.routers_online, status.routers_connected);
});

test('ping with router_name="server" runs synchronously against a real TCP port', async () => {
  const { createServer } = await import('node:net');
  const srv = createServer((sock) => sock.end());
  await new Promise<void>((resolve) => srv.listen(0, '127.0.0.1', resolve));
  const port = (srv.address() as any).port;

  try {
    const { results } = await client.ping({
      ip_pool: '127.0.0.1',
      router_name: 'server',
      check_ports: `${port}`
    });
    assert.equal(results.length, 1);
    assert.equal(results[0][`port_${port}`], 'open');
    assert.equal(results[0].status, true);
    assert.equal(results[0].router_name, 'server');
  } finally {
    srv.close();
  }
});

test('ping with an explicit router name dispatches a real check_run and merges icmp+port results', async (t) => {
  if (!FAKE_NODE_ID) {
    t.skip('TEST_FAKE_NODE_ID not set - skipping node-routed ping test');
    return;
  }
  const routers = await client.listRouters();
  const fakeNode = routers.find((r) => r.id === FAKE_NODE_ID);
  assert.ok(fakeNode, 'the fake-agent test node must exist');

  const { results } = await client.ping({
    ip_pool: '1.2.3.4,5.6.7.8',
    router_name: fakeNode!.name,
    check_ports: 'icmp,80'
  });

  assert.equal(results.length, 2);
  for (const r of results) {
    // The fake agent (see the running Monitor task) reports success:true
    // with latency_ms:5 for every job, regardless of type.
    assert.equal(r.status, true);
    assert.equal(r.ICMP, '5 ms');
    assert.equal(r.port_80, 'open');
    assert.equal(r.router_name, fakeNode!.name);
  }
});
