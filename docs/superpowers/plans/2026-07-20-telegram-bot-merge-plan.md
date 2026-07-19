# Слияние Telegram-бота в pingachock3 — Implementation Plan (Часть 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Перенести `pingachock-2.0` (Telegram-бот) в `bot/` внутри `pingachock3`, переключить его
на новый бекенд (в т.ч. `POST /api/v1/server-ping` и `nodes.blocked`/`platform` из Части 1) вместо
старого "Router Ping API", и добавить сервис в docker-compose.

**Architecture:** Часть 2 из `docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md`,
выполняется после Части 1 (уже смёржена в `main`). UI/меню-код бота (`index.ts`, ~3100 строк)
почти не трогается: он всегда работал через плоский формат результата
(`{ip, resolved_ip, status, ICMP, port_N, ...}`), и новый клиент отдаёт тот же формат — меняется
только то, ЧТО ходит по сети под капотом. Единственные содержательные правки в `index.ts`:
уборка per-user токенов (общий `api_key` вместо них, per design spec §4) и замена "удалить
роутер" на "заблокировать" (у pingachock нет hard delete узлов, per design spec §5).

**Каждый шаг этого плана уже был реально выполнен и проверен в scratch-копии** (реальный
`tsc --noEmit`, реальные тесты `node:test` против живого dev-бекенда + фейковый агент,
эмулирующий протокол poll/results) — код ниже не гипотетический, он компилируется и проходит
тесты как есть.

**Tech Stack:** TypeScript/Node 22+, `telegraf`, `@seald-io/nedb`, `tsx` (уже установлен),
встроенный `node:test`. Бекенд не меняется — использует Часть 1 как есть.

---

## Before starting

Локальный dev-бекенд должен быть поднят (см. `README.md` корня репозитория) — тесты в Task 2
реально ходят на `http://localhost:8080`:

```sh
docker compose up -d
DATABASE_URL="postgres://pingachock:pingachock@localhost:5433/pingachock?sslmode=disable" \
ADMIN_TOKEN="dev-admin-token" \
go run ./cmd/server
```

Заведи account + api_key через admin API (см. корневой `README.md`, раздел 3) — понадобится
для тестов Task 2 и ручной проверки в Task 5.

---

### Task 1: Перенести донорский репозиторий в `bot/`

**Files:**
- Move: содержимое `https://github.com/hydrargyrum0/pingachock-2.0` → `bot/`

- [ ] **Step 1: Склонировать донора и перенести файлы**

```sh
git clone --depth 1 https://github.com/hydrargyrum0/pingachock-2.0.git /tmp/pingachock-bot-donor
mkdir -p bot
cp -r /tmp/pingachock-bot-donor/src bot/src
cp /tmp/pingachock-bot-donor/package.json bot/package.json
cp /tmp/pingachock-bot-donor/tsconfig.json bot/tsconfig.json
cp /tmp/pingachock-bot-donor/Dockerfile bot/Dockerfile
cp /tmp/pingachock-bot-donor/.env.example bot/.env.example
cp /tmp/pingachock-bot-donor/.gitignore bot/.gitignore
cp /tmp/pingachock-bot-donor/README.md bot/README.md
rm -rf /tmp/pingachock-bot-donor
```

Не переносить: `astroping-openapi.json`, `remnawave-openapi.json`, `vultr-openapi.json`,
`package-lock.json`, `docker-compose.yml`, `data/` — спека доноров, лок-файл и compose больше не
актуальны (npm install в Task 4 сгенерирует новый lock; docker-compose Часть 2 добавляет
отдельно в Task 4).

- [ ] **Step 2: Установить зависимости**

```sh
cd bot
npm install
cd ..
```

- [ ] **Step 3: Commit**

```sh
git add bot/
git commit -m "Move Telegram bot repo into bot/, drop old API artifacts"
```

---

### Task 2: Новый клиент `bot/src/pingachock-client.ts`

**Files:**
- Create: `bot/src/pingachock-client.ts`
- Create: `bot/src/pingachock-client.test.ts`
- Modify: `bot/src/db.ts`
- Delete: `bot/src/api-client.ts` (заменяется, ничего больше на него не ссылается после Task 3)

Это самая содержательная часть переноса. Клиент отдаёт **тот же плоский формат результата**,
что ожидает существующий `index.ts` (`formatPingResultLine`/`formatPingReport` и т.д. не
меняются) — так что дальше в Task 3 меняются только точки входа (импорт, per-user токены,
типы ID роутера), а не сама логика рендеринга.

- [ ] **Step 1: Обновить `db.ts` — убрать per-user токен, добавить `api_key` в настройки**

В `bot/src/db.ts`, `UserDoc` — убрать `token`:

```typescript
export type UserDoc = {
  telegram_id: number;
  has_access: boolean;
  created_at: string;
};
```

`userRepo.addUser` — убрать параметр токена, `setToken`/`getToken` — удалить целиком:

```typescript
export const userRepo = {
  async isAuthorized(telegramId: number): Promise<boolean> {
    const doc = await p<UserDoc | null>((cb) =>
      usersDb.findOne({ telegram_id: telegramId, has_access: true }, cb)
    );
    return Boolean(doc);
  },

  async listAuthorized(): Promise<number[]> {
    const docs = await p<UserDoc[]>((cb) =>
      usersDb.find({ has_access: true }).sort({ telegram_id: 1 }).exec(cb)
    );
    return docs.map((d) => d.telegram_id);
  },

  async addUser(telegramId: number): Promise<void> {
    const created_at = new Date().toISOString();
    await pUpdate((cb) =>
      usersDb.update(
        { telegram_id: telegramId },
        { $set: { telegram_id: telegramId, has_access: true, created_at } },
        { upsert: true },
        cb
      )
    );
  },

  async deleteUser(telegramId: number): Promise<void> {
    await pRemove((cb) =>
      usersDb.remove({ telegram_id: telegramId }, { multi: false }, cb)
    );
  }
};
```

В `settingsRepo`, заменить `getClientToken`/`setClientToken` на `getApiKey`/`setApiKey`
(та же структура, что уже есть у `getAdminToken`/`setAdminToken` прямо над этим):

```typescript
  async getApiKey(): Promise<string | null> {
    const doc = await p<SettingDoc | null>((cb) => settingsDb.findOne({ key: 'api_key' }, cb));
    return doc?.value ?? null;
  },

  async setApiKey(token: string): Promise<void> {
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: 'api_key' },
        { $set: { key: 'api_key', value: token, updated_at } },
        { upsert: true },
        cb
      )
    );
  }
```

(Остальные методы `settingsRepo` — `getHealthReportConfig`, `*RemnawaveConfig`,
`*VultrConfig`, `*PeriodicHealthConfig` — не трогать, они не завязаны на бекенд.)

- [ ] **Step 2: Написать падающие тесты клиента**

`bot/src/pingachock-client.test.ts`:

```typescript
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// Указываем db.ts на одноразовую nedb-директорию *до* импорта чего-либо,
// что её трогает - чтобы тест никогда не читал/писал реальную data/ бота.
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pingachock-bot-test-'));
process.env.DB_PATH = path.join(tmpDir, 'users.db');
process.env.SETTINGS_DB_PATH = path.join(tmpDir, 'settings.db');

// require(), не import: последний всегда всплывает выше env-настроек выше,
// которые db.ts должен прочитать в момент загрузки модуля.
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
    assert.equal(r.status, true);
    assert.equal(r.port_80, 'open');
    assert.equal(r.router_name, fakeNode!.name);
  }
});
```

- [ ] **Step 3: Поднять фейкового агента для теста узлового пути**

Тест последнего шага (`ping` через конкретный роутер) требует узла, который реально забирает и
завершает задания — без него `pollCheckUntilDone` в клиенте будет честно ждать реальный
таймаут. Настоящий Go-агент для этого избыточен; фейковый агент на Python, говорящий тем же
протоколом `poll`/`results`, работает и проще в отладке (см. `internal/api/agent/handler.go` за
точным контрактом).

Зарегистрировать тестовый узел и запустить фейкового агента (в отдельном терминале/фоне,
держать открытым, пока идёт Task 2):

```sh
NODE=$(curl -sS -X POST http://localhost:8080/api/v1/nodes \
  -H "Authorization: Bearer dev-admin-token" -H "Content-Type: application/json" \
  -d '{"name":"fake-agent-node","isp":"test","city":"test"}')
NODE_ID=$(echo "$NODE" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
NODE_SECRET=$(echo "$NODE" | python3 -c "import sys,json;print(json.load(sys.stdin)['secret'])")
echo "NODE_ID=$NODE_ID"
```

`fake_agent.py` (положить рядом, не коммитить в репозиторий - чисто локальный тестовый
инструмент):

```python
import json
import time
import urllib.request

URL = "http://localhost:8080"
SECRET = "<вставить NODE_SECRET из шага выше>"


def call(path, body):
    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        URL + path,
        data=data,
        method="POST",
        headers={"Authorization": f"Bearer {SECRET}", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req) as resp:
        raw = resp.read()
        return json.loads(raw) if raw else None


print("fake agent starting", flush=True)
while True:
    try:
        poll_resp = call("/api/v1/agent/poll", {"agent_version": "fake-1.0", "platform": "linux"})
        jobs = poll_resp.get("jobs", [])
        if jobs:
            results = []
            for j in jobs:
                print(f"completing {j['type']} {j['target']} ({j['check_run_id']})", flush=True)
                results.append(
                    {
                        "check_run_id": j["check_run_id"],
                        "success": True,
                        "latency_ms": 5,
                        "raw": {"resolved_target": j["target"]},
                    }
                )
            call("/api/v1/agent/results", {"results": results})
    except Exception as e:
        print(f"fake agent error: {e}", flush=True)
    time.sleep(1)
```

```sh
python3 fake_agent.py &
```

**Важно про JSON:** строй тело запроса POST через `json.dumps` (как выше), не через ручную
склейку строк bash - склейка легко порождает синтаксически битый JSON, который бекенд молча
отклонит, а `> /dev/null` в фоновом скрипте так же молча спрячет ошибку. (Реальная причина
90-секундного зависания при первой попытке этого шага - см. историю разработки.)

- [ ] **Step 4: Запустить тесты, убедиться что падают**

```sh
cd bot
export TEST_API_KEY=<реальный api_key>
export TEST_FAKE_NODE_ID=<NODE_ID из шага 3>
npx tsx --test src/pingachock-client.test.ts
```

Ожидаемо: падение с ошибкой о том, что `./pingachock-client` не найден (модуль ещё не создан).

- [ ] **Step 5: Реализовать клиент**

`bot/src/pingachock-client.ts`:

```typescript
import { settingsRepo } from './db';

export type Router = {
  id: string;
  name: string;
  token?: string;
  status: string;
  platform: string;
  blocked: boolean;
  last_seen: string | null;
  created_at?: string;
};

export type ApiStatus = {
  routers_online?: number;
  routers_connected?: number;
};

class ApiClientError extends Error {
  constructor(
    public statusCode: number,
    public statusText: string,
    message: string
  ) {
    super(message);
    this.name = 'ApiClientError';
  }
}

type TokenType = 'admin' | 'api';

async function fetchWithAuth(path: string, method: string, body: unknown, tokenType: TokenType): Promise<unknown> {
  const apiUrl = await settingsRepo.getApiUrl();
  if (!apiUrl) {
    throw new Error('API URL is not configured. Set it via /admin -> API URL');
  }

  const token = tokenType === 'admin' ? await settingsRepo.getAdminToken() : await settingsRepo.getApiKey();
  if (!token) {
    throw new Error(
      tokenType === 'admin'
        ? 'admin_token is not configured. Set it via /admin -> admin_token'
        : 'api_key is not configured. Set it via /admin -> API key'
    );
  }

  const fullUrl = new URL(path, apiUrl).toString();
  const headers: Record<string, string> = { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` };
  const options: RequestInit = { method, headers };
  if (body !== undefined) {
    options.body = JSON.stringify(body);
  }

  const response = await fetch(fullUrl, options);
  if (response.status === 204) {
    return null;
  }

  if (!response.ok) {
    let errorMsg = `HTTP ${response.status} ${response.statusText}`;
    try {
      const data = await response.json();
      if (data && typeof data === 'object' && 'error' in data) {
        errorMsg += `: ${(data as { error: unknown }).error}`;
      }
    } catch {
      // ignore parse error
    }
    throw new ApiClientError(response.status, response.statusText, errorMsg);
  }

  try {
    return await response.json();
  } catch {
    return null;
  }
}

function toRouter(n: any): Router {
  return {
    id: String(n.id),
    name: String(n.name ?? ''),
    status: n.online ? 'online' : 'offline',
    platform: String(n.platform ?? ''),
    blocked: Boolean(n.blocked),
    last_seen: n.last_seen_at ? String(n.last_seen_at) : null,
    created_at: n.created_at ? String(n.created_at) : undefined
  };
}

export async function listRouters(): Promise<Router[]> {
  const data = (await fetchWithAuth('/api/v1/nodes', 'GET', undefined, 'api')) as any;
  const nodes = Array.isArray(data?.nodes) ? data.nodes : [];
  return nodes.map(toRouter);
}

export async function getRouter(id: string): Promise<Router> {
  const data = await fetchWithAuth(`/api/v1/nodes/${id}`, 'GET', undefined, 'api');
  return toRouter(data);
}

export async function getStatus(): Promise<ApiStatus> {
  const routers = await listRouters();
  const online = routers.filter((r) => r.status === 'online').length;
  return { routers_online: online, routers_connected: online };
}

export async function createRouter(name: string, isp = '', city = ''): Promise<Router> {
  const data = (await fetchWithAuth('/api/v1/nodes', 'POST', { name, isp, city }, 'admin')) as any;
  return { ...toRouter(data), token: data.secret };
}

// blockRouter replaces the old "delete router" concept: pingachock has no
// hard node deletion by design (blocking keeps check history, see
// docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md section 5).
export async function blockRouter(id: string): Promise<void> {
  await fetchWithAuth(`/api/v1/nodes/${id}`, 'PUT', { blocked: true }, 'admin');
}

function parseCheckPorts(checkPorts: string): { icmp: boolean; ports: string[] } {
  const tokens = checkPorts
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean);
  if (tokens.length === 0) {
    return { icmp: true, ports: [] };
  }
  return { icmp: tokens.includes('icmp'), ports: tokens.filter((t) => t !== 'icmp' && /^\d+$/.test(t)) };
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function resolveNodeId(routerName: string): Promise<{ id: string; name: string }> {
  const routers = await listRouters();
  if (routerName === 'auto') {
    const online = routers.find((r) => r.status === 'online' && !r.blocked);
    if (!online) throw new Error('No online routers available for "auto"');
    return { id: online.id, name: online.name };
  }
  const match = routers.find((r) => r.name === routerName);
  if (!match) throw new Error(`Router "${routerName}" not found`);
  return { id: match.id, name: match.name };
}

async function serverPing(targets: string[], icmp: boolean, ports: string[]): Promise<{ results: any[] }> {
  const portList: string[] = [...(icmp ? ['icmp'] : []), ...ports];
  const data = (await fetchWithAuth('/api/v1/server-ping', 'POST', { targets, ports: portList }, 'api')) as any;
  const results = (data?.results ?? []).map((r: any) => {
    const out: any = {
      ip: r.target,
      resolved_ip: r.resolved_ip || r.target,
      status: Boolean(r.icmp?.success) || Object.values(r.ports ?? {}).some((v) => v === 'open'),
      router_name: 'server'
    };
    if (r.icmp) {
      out.ICMP = r.icmp.success ? `${r.icmp.latency_ms ?? '?'} ms` : r.icmp.error || 'no reply';
    }
    for (const [port, state] of Object.entries(r.ports ?? {})) {
      out[`port_${port}`] = state;
    }
    return out;
  });
  return { results };
}

const NODE_POLL_INTERVAL_MS = 2000;
const NODE_POLL_TIMEOUT_MS = 90000;

type CheckSpec = { kind: 'icmp' } | { kind: 'port'; port: string };

async function createBatchedCheck(spec: CheckSpec, targets: string[], nodeId: string): Promise<string[]> {
  const body =
    spec.kind === 'icmp'
      ? { type: 'ping', targets, node_selector: { node_ids: [nodeId] } }
      : { type: 'tcp', targets, params: { port: Number(spec.port) }, node_selector: { node_ids: [nodeId] } };
  const created = (await fetchWithAuth('/api/v1/checks', 'POST', body, 'api')) as any;
  if (created.batch_id) {
    return created.checks.map((c: any) => c.id);
  }
  return [created.id];
}

async function pollCheckUntilDone(checkId: string): Promise<any> {
  const deadline = Date.now() + NODE_POLL_TIMEOUT_MS;
  for (;;) {
    const check = await fetchWithAuth(`/api/v1/checks/${checkId}?expand=runs`, 'GET', undefined, 'api');
    const status = (check as any)?.status;
    if (status !== 'pending' && status !== 'running') return check;
    if (Date.now() > deadline) return check; // give up, report whatever we have
    await sleep(NODE_POLL_INTERVAL_MS);
  }
}

function mergeNodeResults(
  targets: string[],
  nodeId: string,
  routerName: string,
  finished: Array<{ spec: CheckSpec; checks: any[] }>
): any[] {
  return targets.map((target) => {
    const out: any = { ip: target, resolved_ip: target, status: false, router_name: routerName };
    for (const { spec, checks } of finished) {
      const check = checks.find((c) => c.target === target);
      const run = check?.runs?.find((r: any) => r.node_id === nodeId);
      const result = run?.result;

      if (spec.kind === 'icmp') {
        if (result) {
          out.ICMP = result.success ? `${result.latency_ms ?? '?'} ms` : result.error_message || 'no reply';
          if (result.success) out.status = true;
        }
      } else {
        const state = result ? (result.success ? 'open' : 'closed') : 'unknown';
        out[`port_${spec.port}`] = state;
        if (state === 'open') out.status = true;
      }

      const rawResolved = extractResolvedTarget(result?.raw);
      if (rawResolved) out.resolved_ip = rawResolved;
    }
    return out;
  });
}

function extractResolvedTarget(raw: unknown): string | null {
  if (!raw) return null;
  try {
    const parsed = typeof raw === 'string' ? JSON.parse(raw) : raw;
    return typeof parsed?.resolved_target === 'string' ? parsed.resolved_target : null;
  } catch {
    return null;
  }
}

async function nodePing(targets: string[], nodeId: string, routerName: string, icmp: boolean, ports: string[]): Promise<{ results: any[] }> {
  const specs: CheckSpec[] = [...(icmp ? [{ kind: 'icmp' as const }] : []), ...ports.map((port) => ({ kind: 'port' as const, port }))];
  if (specs.length === 0) return { results: [] };

  const dispatched = await Promise.all(
    specs.map(async (spec) => ({ spec, checkIds: await createBatchedCheck(spec, targets, nodeId) }))
  );

  const finished = await Promise.all(
    dispatched.map(async ({ spec, checkIds }) => ({
      spec,
      checks: await Promise.all(checkIds.map((id) => pollCheckUntilDone(id)))
    }))
  );

  return { results: mergeNodeResults(targets, nodeId, routerName, finished) };
}

// ping mirrors the old astroping API's GET /api/ping contract exactly
// (same params, same flat per-target result shape) so the ~3000 lines of
// existing bot UI code that consume it don't need to change - only this
// client does. router_name="server" is synchronous (no node involved);
// anything else resolves to a node and goes through the async
// checks/check_runs poll loop. Never takes a per-user token - the bot now
// authenticates with one shared api_key (see the design spec, section 4).
export async function ping(params: { ip_pool: string; router_name?: string; check_ports?: string }): Promise<{ results: any[] }> {
  const targets = params.ip_pool
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
  if (targets.length === 0) return { results: [] };

  const { icmp, ports } = parseCheckPorts(params.check_ports ?? 'icmp');
  const routerName = params.router_name ?? 'auto';

  if (routerName === 'server') {
    return serverPing(targets, icmp, ports);
  }

  const { id: nodeId, name: resolvedName } = await resolveNodeId(routerName);
  return nodePing(targets, nodeId, resolvedName, icmp, ports);
}
```

- [ ] **Step 6: Запустить тесты, убедиться что проходят**

```sh
npx tsx --test src/pingachock-client.test.ts
```

Ожидаемо: все 5 тестов `PASS`, узловой тест укладывается в 2-3 секунды (не 90 - если он
занимает ~90с, значит check_run никогда не увидел `done`, см. заметку про JSON в Step 3).

- [ ] **Step 7: Удалить старый клиент, type-check всего бота**

```sh
rm src/api-client.ts
npx tsc --noEmit
```

На этом шаге `tsc` покажет ошибки во ВСЕХ местах `index.ts`, которые ещё ссылаются на старый
`api-client` / убранные поля - это ожидаемо и есть чек-лист для Task 3. Не переходить к Task 3,
пока этот список ошибок не в руках.

- [ ] **Step 8: Commit**

```sh
git add src/pingachock-client.ts src/pingachock-client.test.ts src/db.ts
git rm src/api-client.ts
git commit -m "Add pingachock-client.ts, replacing the old astroping API client"
```

---

### Task 3: Адаптировать `index.ts`

**Files:**
- Modify: `bot/src/index.ts`

Каждое изменение ниже — точечная правка в существующем файле, привязанная к конкретному месту.
Порядок правок ниже соответствует тому, в котором их логично делать (импорт и типы первыми, тела
хендлеров последними), но `tsc --noEmit` из Task 2 Step 7 — источник истины по полному списку
мест, а не этот список сам по себе.

- [ ] **Step 1: Импорт и типы сессии**

Было:

```typescript
import { settingsRepo, userRepo } from './db';
import { apiClient, type Router } from './api-client';

type MySession = {
  awaitingAddUser?: boolean;
  awaitingApiUrl?: boolean;
  awaitingAdminToken?: boolean;
  awaitingBroadcastText?: boolean;
  broadcastDraftText?: string;
  awaitingRouterName?: boolean;
  awaitingRouterDeleteConfirm?: number | null;
  selectedRouterId?: number;

  selectedUserTelegramId?: number;
  selectedClientId?: number;
  awaitingClientDeleteConfirm?: boolean;
```

Стало:

```typescript
import { settingsRepo, userRepo } from './db';
import * as apiClient from './pingachock-client';
import type { Router } from './pingachock-client';

type MySession = {
  awaitingAddUser?: boolean;
  awaitingApiUrl?: boolean;
  awaitingAdminToken?: boolean;
  awaitingApiKey?: boolean;
  awaitingBroadcastText?: boolean;
  broadcastDraftText?: string;
  awaitingRouterName?: boolean;
  awaitingRouterBlockConfirm?: string | null;
  selectedRouterId?: string;

  selectedUserTelegramId?: number;
  awaitingUserDeleteConfirm?: boolean;
```

`import * as apiClient` (а не `{ apiClient }`) специально: `pingachock-client.ts` экспортирует
функции напрямую (`export function listRouters`, ...), а не единый объект `apiClient = {...}`
как было в старом клиенте. `import *` даёт тот же `apiClient.listRouters(...)`,
`apiClient.ping(...)` и т.д. по всему файлу без переписывания вызовов.

- [ ] **Step 2: Добавить пункт "api_key" в админ-меню**

В `adminRootKeyboard()`:

```typescript
    [Markup.button.callback('API URL', 'admin:api_url')],
    [Markup.button.callback('admin_token', 'admin:admin_token')],
    [Markup.button.callback('api_key', 'admin:api_key')],
    [Markup.button.callback('Роутеры', 'admin:routers')]
```

- [ ] **Step 3: Добавить хендлер `admin:api_key`, обновить `admin:admin_token`**

Сразу после существующего `bot.action('admin:admin_token', ...)` (добавить строку
`ctx.session.awaitingApiKey = false;` в его тело, и новый пояснительный текст) добавить:

```typescript
bot.action('admin:admin_token', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = true;
  ctx.session.awaitingApiKey = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingPingInput = false;

  await safeEditOrReply(
    ctx,
    'Отправь admin_token одним сообщением.\n\nЧто это: секрет для управления узлами/аккаунтами pingachock (создание, блокировка) - тот же ADMIN_TOKEN, что и на бекенде.\n\nЧтобы отменить — нажми «Отмена».',
    adminCancelToRootKeyboard()
  );
});

bot.action('admin:api_key', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingApiKey = true;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingPingInput = false;

  await safeEditOrReply(
    ctx,
    'Отправь api_key одним сообщением.\n\nЧто это: ключ для пинга/проверок (POST /accounts/{id}/api-keys на бекенде) - один общий ключ на весь бот, не путать с admin_token.\n\nЧтобы отменить — нажми «Отмена».',
    adminCancelToRootKeyboard()
  );
});
```

- [ ] **Step 4: Добавить обработку ввода api_key в `bot.on('text', ...)`**

Сразу после блока `if (isAdmin(ctx) && ctx.session.awaitingAdminToken) { ... }` (сохранение
`admin_token` через текстовый ввод) добавить аналогичный блок:

```typescript
  // Настройка api_key: только админ и только когда ждём ключ
  if (isAdmin(ctx) && ctx.session.awaitingApiKey) {
    const token = ctx.message.text.trim();

    if (!token) {
      await ctx.reply('Ключ не должен быть пустым.', adminCancelToRootKeyboard());
      return;
    }

    await settingsRepo.setApiKey(token);
    ctx.session.awaitingApiKey = false;

    await ctx.reply('api_key сохранён.');
    await ctx.reply('Админ-панель:', adminRootKeyboard());
    return;
  }
```

- [ ] **Step 5: Добавить сброс `awaitingApiKey` во все общие reset-блоки**

По всему файлу есть блоки вида:

```typescript
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
```

(при переключении между экранами админки/меню, сбрасывают "ожидание ввода"). В каждый такой
блок добавить четвёртую строку сразу за ним:

```typescript
  ctx.session.awaitingApiKey = false;
```

На момент написания плана таких блоков 12 - самый быстрый способ найти все: искать по
литеральной строке `ctx.session.awaitingAdminToken = false;` и вставлять `awaitingApiKey`
следом за каждым вхождением (одинаковый текст, безопасно для replace-all в редакторе).

- [ ] **Step 6: `userActionsKeyboard`/`routerDetailsKeyboard` — новые подписи и типы**

Было:

```typescript
function userActionsKeyboard(telegramId: number) {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🗑 Удалить клиента', `admin:delete:${telegramId}`)],
    [Markup.button.callback('◀️ Назад', 'admin:cancel')]
  ]);
}

function clientDeleteConfirmKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('✓ Да, удалить', 'admin:delete_confirm')],
    [Markup.button.callback('◀️ Назад', 'admin:cancel')]
  ]);
}
```

Стало (переименовать `clientDeleteConfirmKeyboard` → `userDeleteConfirmKeyboard`, обновить
единственный вызов ниже в Step 8):

```typescript
function userActionsKeyboard(telegramId: number) {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🗑 Удалить пользователя', `admin:delete:${telegramId}`)],
    [Markup.button.callback('◀️ Назад', 'admin:cancel')]
  ]);
}

function userDeleteConfirmKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('✓ Да, удалить', 'admin:delete_confirm')],
    [Markup.button.callback('◀️ Назад', 'admin:cancel')]
  ]);
}
```

Было:

```typescript
function routerDetailsKeyboard(routerId: number) {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🗑 Удалить роутер', `admin:confirm_delete_router:${routerId}`)],
    [Markup.button.callback('◀️ Назад', 'admin:routers')]
  ]);
}
```

Стало:

```typescript
function routerDetailsKeyboard(routerId: string) {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🚫 Заблокировать роутер', `admin:confirm_block_router:${routerId}`)],
    [Markup.button.callback('◀️ Назад', 'admin:routers')]
  ]);
}
```

- [ ] **Step 7: Переписать блок деталей пользователя/роутера и блокировки/удаления**

Найти блок из шести хендлеров: `admin:user:(\d+)`, `admin:cancel`, `admin:routers`,
`admin:router:(\d+)`, `admin:confirm_delete_router:(\d+)`, `admin:delete_router_confirm:(\d+)`,
`admin:add_router`, `admin:delete:(\d+)`, `admin:delete_confirm` (идут подряд одним куском).
Заменить целиком на:

```typescript
bot.action(/^admin:user:(\d+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;

  const match = ctx.match as RegExpMatchArray;
  const telegramId = Number(match[1]);

  ctx.session.selectedUserTelegramId = telegramId;
  ctx.session.awaitingUserDeleteConfirm = false;

  await ctx.answerCbQuery();

  // No per-user entity in pingachock to fetch (see design spec section 4) -
  // being in this list already means they have access, nothing more to show.
  const html = `Пользователь: ${escapeHtml(String(telegramId))}\n\n` + `Доступ: разрешён\n\n` + `Выбери действие:`;
  await safeEditOrReplyHtml(ctx, html, userActionsKeyboard(telegramId));
});

bot.action('admin:cancel', async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingRouterName = false;
  ctx.session.awaitingRouterBlockConfirm = null;
  await ctx.answerCbQuery();
  await showUsersList(ctx);
});

bot.action('admin:routers', async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingRouterName = false;
  ctx.session.awaitingRouterBlockConfirm = null;
  await ctx.answerCbQuery();
  await showRoutersList(ctx);
});

bot.action(/^admin:router:([0-9a-fA-F-]+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  const match = ctx.match as RegExpMatchArray;
  const routerId = match[1];
  ctx.session.selectedRouterId = routerId;

  try {
    const router = await apiClient.getRouter(routerId);
    const lastSeen = formatRouterLastSeen(router.last_seen);
    const createdAt = typeof router.created_at === 'string' ? router.created_at : 'нет данных';

    // No token/secret here by design - GET /nodes/{id} never returns it,
    // only the one-time POST /nodes creation response does (see
    // admin:add_router below).
    const html =
      `Роутер: ${escapeHtml(String(router.name))}\n` +
      `ID: ${escapeHtml(String(router.id))}\n` +
      `Платформа: ${escapeHtml(router.platform || '—')}\n` +
      `Статус: ${escapeHtml(String(router.status))}\n` +
      `Заблокирован: ${router.blocked ? 'true' : 'false'}\n` +
      `Последний онлайн: ${escapeHtml(String(lastSeen))}\n` +
      `Создан: ${escapeHtml(String(createdAt))}`;

    await safeEditOrReplyHtml(ctx, html, routerDetailsKeyboard(routerId));
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка:\n${errMsg}`, adminCancelToRootKeyboard());
  }
});

bot.action(/^admin:confirm_block_router:([0-9a-fA-F-]+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  const match = ctx.match as RegExpMatchArray;
  const routerId = match[1];
  ctx.session.awaitingRouterBlockConfirm = routerId;

  let routerName = 'неизвестно';
  try {
    const router = await apiClient.getRouter(routerId);
    routerName = String(router.name);
  } catch {
    // ignore; we can still confirm by id
  }

  await safeEditOrReply(
    ctx,
    `Вы блокируете роутер ${routerName} с ID: ${routerId}\n\n` +
      `Он перестанет получать новые проверки, история сохранится.\n\n` +
      `Для подтверждения нажмите кнопку ниже.`,
    Markup.inlineKeyboard([
      [Markup.button.callback('✓ Да, заблокировать', `admin:block_router_confirm:${routerId}`)],
      [Markup.button.callback('◀️ Назад', 'admin:routers')]
    ])
  );
});

bot.action(/^admin:block_router_confirm:([0-9a-fA-F-]+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  const match = ctx.match as RegExpMatchArray;
  const routerId = match[1];

  try {
    await apiClient.blockRouter(routerId);
    ctx.session.awaitingRouterBlockConfirm = null;
    await showRoutersList(ctx, `Роутер ${routerId} заблокирован`);
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка блокировки:\n${errMsg}`, adminCancelToRootKeyboard());
  }
});

bot.action('admin:add_router', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingRouterName = true;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;

  await safeEditOrReply(
    ctx,
    'Отправь имя нового роутера одним сообщением.\n\nЧтобы отменить — нажми «Отмена».',
    adminCancelToRootKeyboard()
  );
});

bot.action(/^admin:delete:(\d+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;

  const match = ctx.match as RegExpMatchArray;
  const telegramId = Number(match[1]);

  ctx.session.selectedUserTelegramId = telegramId;
  ctx.session.awaitingUserDeleteConfirm = true;

  await ctx.answerCbQuery();
  await safeEditOrReply(ctx, `Вы удаляете доступ пользователя ${telegramId}.\n\nПродолжить?`, userDeleteConfirmKeyboard());
});

bot.action('admin:delete_confirm', async (ctx) => {
  if (!isAdmin(ctx)) return;

  const telegramId = ctx.session.selectedUserTelegramId;
  if (!telegramId) {
    await ctx.answerCbQuery('Пользователь не выбран', { show_alert: true });
    return;
  }

  ctx.session.awaitingUserDeleteConfirm = false;
  await ctx.answerCbQuery();

  // Purely local: no per-user entity in pingachock to clean up (see design
  // spec section 4).
  await userRepo.deleteUser(telegramId);

  await showUsersList(ctx, `Удалён пользователь: ${telegramId}`);
});
```

- [ ] **Step 8: Упростить `admin:add` (создание пользователя)**

Внутри текстового хендлера `if (isAdmin(ctx) && ctx.session.awaitingAddUser) { ... }`, было:

```typescript
    // Create API client for this telegram user and store its token
    try {
      const client = await apiClient.createClient(String(telegramId));
      await userRepo.addUser(telegramId, client.token);
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(
        `Ошибка при создании клиента в API:\n${errMsg}`,
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:cancel')]])
      );
      return;
```

Стало:

```typescript
    // Purely local: everyone authorized shares the bot's one api_key (see
    // design spec section 4), nothing to provision per-user in pingachock.
    try {
      await userRepo.addUser(telegramId);
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(
        `Ошибка при добавлении пользователя:\n${errMsg}`,
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:cancel')]])
      );
      return;
```

- [ ] **Step 9: Убрать per-user токен из шести мест, где гоняется `ping`**

В каждом из следующих мест (все найдутся через `grep -n perUserToken src/index.ts`):

1. Периодический шедулер, кастомный список (внутри `runPingBatches`)
2. Периодический шедулер, Remnawave (внутри `runBatchedPing`)
3. Ручной пинг из главного меню (`bot.on('text', ...)`, ветка `awaitingPingInput`)
4. `health:force`
5. `health:remna_force`
6. `health:vultr_force`

Убрать блок вида:

```typescript
    const fromId = ctx.from?.id;
    const perUserToken = fromId != null ? await userRepo.getToken(fromId) : null;
    if (!perUserToken) {
      ...
      await ctx.reply('⚠️ Не найден client token. Обратитесь к администратору!');
      ...
      return;
    }
```

(вариации: где-то `fromId != null ? ... : null`, где-то напрямую `await userRepo.getToken(telegramId)` -
любой вариант убрать целиком, включая `if (!perUserToken) {...return;}`), и убрать
`perUserToken` как последний аргумент `apiClient.ping(...)`:

```typescript
// было
const data = await apiClient.ping(
  { ip_pool, router_name: routerName, check_ports: portsOpt.value },
  perUserToken
);

// стало
const data = await apiClient.ping({ ip_pool, router_name: routerName, check_ports: portsOpt.value });
```

(в двух местах параметр называется `params.routerName` вместо `routerName` - использовать то
имя переменной, что уже в коде на этом месте).

- [ ] **Step 10: Type-check, чинить по списку ошибок**

```sh
npx tsc --noEmit
```

Проходить по каждой ошибке до чистого прогона (`exit 0`). Если всплывает ошибка не из списка
выше - скорее всего, ещё одна ссылка на `createClient`/`deleteClient`/`getClientByName`/
`getClientById`/`listClients`/`deleteRouter`/`selectedClientId`/`awaitingClientDeleteConfirm` -
все они убраны из `pingachock-client.ts` и типов сессии намеренно (нет соответствующей сущности
в pingachock, см. design spec §4-5).

- [ ] **Step 11: Commit**

```sh
git add src/index.ts
git commit -m "Adapt index.ts to pingachock: shared api_key, string node ids, block instead of delete"
```

---

### Task 4: docker-compose и переменные окружения

**Files:**
- Modify: `docker-compose.prod.yml`
- Modify: `bot/.env.example`
- Modify: `bot/README.md`

- [ ] **Step 1: Добавить сервис `bot` в `docker-compose.prod.yml`**

```yaml
  bot:
    build: ./bot
    restart: unless-stopped
    environment:
      telegram_bot_token: ${TELEGRAM_BOT_TOKEN:?set in .env}
      telegram_bot_admin_id: ${TELEGRAM_BOT_ADMIN_ID:?set in .env}
      DB_PATH: /app/data/users.db
      SETTINGS_DB_PATH: /app/data/settings.db
    volumes:
      - bot_data:/app/data
    depends_on:
      - backend
    networks:
      - internal
```

Добавить `bot_data:` в `volumes:` в конце файла (рядом с `pgdata`/`caddy_data`/`caddy_config`).
Порт наружу не публикуется - Telegram long-polling, входящих подключений к боту не требуется
(design spec §7).

- [ ] **Step 2: Обновить `bot/.env.example`**

```
# Telegram
telegram_bot_token=PUT_YOUR_BOT_TOKEN_HERE
telegram_bot_admin_id=123456789,987654321

# Optional
# DB_PATH=./data/users.db
# SETTINGS_DB_PATH=./data/settings.db
```

(без изменений по сути - `api_url`/`admin_token`/`api_key` настраиваются через `/admin` в самом
боте, не через env, как и раньше `admin_token` не был env-переменной).

- [ ] **Step 3: Обновить `bot/README.md`**

Старый README не документирует `admin_token`/`client_token` как переменные окружения вообще
(они настраиваются через `/admin` в самом боте) - менять по сути нечего, кроме добавления
`SETTINGS_DB_PATH` в раздел "Переменные окружения" (уже используется в `db.ts`, но не был
задокументирован и в донорском репозитории) и одной уточняющей строки про `/admin`. Раздел
"Переменные окружения" - было:

```
### Переменные окружения
- `telegram_bot_token` — токен бота
- `telegram_bot_admin_id` — chat id администратора(ов) (для `/admin`), можно несколько через запятую
- `DB_PATH` (опционально) — путь к файлу базы пользователей (по умолчанию `./data/users.db`)
```

Стало:

```
### Переменные окружения
- `telegram_bot_token` — токен бота
- `telegram_bot_admin_id` — chat id администратора(ов) (для `/admin`), можно несколько через запятую
- `DB_PATH` (опционально) — путь к файлу базы пользователей (по умолчанию `./data/users.db`)
- `SETTINGS_DB_PATH` (опционально) — путь к файлу настроек (по умолчанию `./data/settings.db`)

API URL, admin_token и api_key бекенда pingachock настраиваются не через env, а через `/admin`
в самом боте (после первого запуска, только для telegram_bot_admin_id).
```

- [ ] **Step 4: Commit**

```sh
git add docker-compose.prod.yml bot/.env.example bot/README.md
git commit -m "Add bot service to docker-compose, update env docs"
```

---

### Task 5: Ручная сквозная проверка

Разговорный флоу бота (реальные нажатия кнопок в Telegram) не автоматизируется в рамках этого
плана - Task 2 уже покрыл тестами всю новую сетевую логику (`pingachock-client.ts`), это
единственное, что реально меняет поведение. Ниже - контрольный список ручных проверок перед тем,
как считать перенос завершённым.

- [ ] **Step 1: Запустить бота локально против dev-бекенда**

```sh
cd bot
telegram_bot_token=<токен от @BotFather> telegram_bot_admin_id=<свой telegram id> npm run dev
```

- [ ] **Step 2: Первичная настройка через `/admin`**

В Telegram: `/admin` → API URL → `http://localhost:8080/` (со слэшем в конце, бот это
проверяет) → admin_token → `dev-admin-token` → api_key → реальный ключ из `POST
/accounts/{id}/api-keys`.

- [ ] **Step 3: Пользователи**

`/admin` → «Управлять пользователями» → «➕ Добавить пользователя» → свой telegram_id.
Проверить: `/start` теперь показывает главное меню (было бы недоступно без доступа).

- [ ] **Step 4: Пинг через сервер**

Главное меню → 🔍 Ping → переключить роутер на "server" (кнопка-тумблер) → отправить
`1.1.1.1`. Ожидаемо: ответ приходит за секунды (не за 20-40с), `status: true`/задержка есть.

- [ ] **Step 5: Пинг через узел**

Поднять реальный (или фейковый, как в Task 2 Step 3) агент, зарегистрированный на этот же
dev-бекенд. Главное меню → 🔍 Ping → роутер = имя этого узла → отправить `1.1.1.1`. Ожидаемо:
ответ приходит за ~20-40с (реальный цикл poll/dispatch/results), результат непустой.

- [ ] **Step 6: Роутеры в админке**

`/admin` → «Роутеры» → выбрать роутер → «🚫 Заблокировать роутер» → подтвердить. Проверить через
API (`GET /api/v1/nodes/{id}` с admin_token), что `blocked: true`. Повторно запустить пинг через
этот роутер - ожидаемо: ошибка `"Router \"...\" not found"` (заблокированный роутер не
возвращается в `listRouters()` для 'auto', но всё ещё виден в списке через `getRouter` по
прямому ID - поведение соответствует design spec §5, "исключает из диспетчеризации").

- [ ] **Step 7: Health report (кастомный список)**

Главное меню → 📊 Health report → 🟧 Кастомный список → отправить пару целей → 🔶 Принудительный
Health Report. Ожидаемо: отчёт приходит, содержит только упавшие цели (`onlyFailed`).

- [ ] **Step 8: Remnawave/Vultr (если есть доступ к тестовому стенду)**

Если под рукой нет реального Remnawave/Vultr - пропустить, эта интеграция не менялась в этом
плане вообще (design spec §8, "что не меняется") - риск только в том, что она передаёт данные в
`apiClient.ping`, что уже покрыто Step 4-5 выше.

---

## После этого плана

Перенос завершён. Старый репозиторий `pingachock-2.0` можно архивировать (организационное
решение, не техническое - design spec, "Открытые вопросы").
