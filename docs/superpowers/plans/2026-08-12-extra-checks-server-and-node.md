# Дополнительные проверки: Server + Node Everywhere Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** All three "➕ Дополнительные проверки" bot flows (HTTP 101 check, VLESS Speedtest, TLS Handshake) run against a real node or the backend ("server"), explicitly and correctly - and fix a bug where "Auto" node selection could silently pick the virtual server node instead of a real one.

**Architecture:** `Router` gains an `is_virtual` field (already in the API response, just unmapped); `resolveNodeId`'s `'auto'` branch excludes it - one shared fix used everywhere `resolveNodeId` is called. VLESS Speedtest and TLS Handshake's router pickers get an explicit `🖥 Server` option (they already dispatch correctly to it, just weren't labeling it). HTTP 101 check gains a router picker and a node-dispatch path in `scanUpgrade`, reusing the same batched-check/poll/merge pattern `ping`'s node-routed path already uses.

**Tech Stack:** TypeScript/Telegraf (bot only - no Go changes, see the design doc's Scope section for why).

**Spec:** `docs/superpowers/specs/2026-08-12-extra-checks-server-and-node-design.md`

---

## Task 1: Fix the `is_virtual`-unaware "Auto" bug

**Files:**
- Modify: `bot/src/pingachock-client.ts`
- Create: `bot/src/resolve-node.test.ts`

- [ ] **Step 1: Write the failing test**

`resolveNodeId` itself isn't exported, and mocking `fetchWithAuth`'s
underlying `fetch` + `settingsRepo` (API URL/token) just to exercise its
`'auto'` branch is more machinery than this bug needs - the actual buggy
logic is a small, pure "pick the first online/unblocked/non-virtual
router" rule. Step 3 pulls that out as its own exported function,
`pickAutoRouter`, so it's directly testable without any network/auth
plumbing.

Create `bot/src/resolve-node.test.ts`:

```ts
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run (from `bot/`): `npx tsx --test src/resolve-node.test.ts`
Expected: FAIL - `pickAutoRouter is not a function`.

- [ ] **Step 3: Add `is_virtual` to `Router`, extract `pickAutoRouter`, fix `resolveNodeId`**

Open `bot/src/pingachock-client.ts`. Change the `Router` type:

```ts
export type Router = {
  id: string;
  name: string;
  token?: string;
  status: string;
  platform: string;
  blocked: boolean;
  is_virtual: boolean;
  last_seen: string | null;
  created_at?: string;
};
```

Find `toRouter` and add the new field:

```ts
function toRouter(n: any): Router {
  return {
    id: String(n.id),
    name: String(n.name ?? ''),
    status: n.online ? 'online' : 'offline',
    platform: String(n.platform ?? ''),
    blocked: Boolean(n.blocked),
    is_virtual: Boolean(n.is_virtual),
    last_seen: n.last_seen_at ? String(n.last_seen_at) : null,
    created_at: n.created_at ? String(n.created_at) : undefined
  };
}
```

Find `resolveNodeId` and replace it, adding the new exported helper right
above it:

```ts
// pickAutoRouter is resolveNodeId's "auto" logic pulled out as a pure,
// testable function - the bug this fixes: it used to have no is_virtual
// awareness at all, so "auto" could silently resolve to the backend's own
// virtual "server" node instead of a real one if that node happened to
// sort first (GET /api/v1/nodes orders by created_at, and the virtual
// node is provisioned at backend startup - plausible it sorts early). The
// Go backend's own dispatch-time auto-selection
// (internal/dispatch/dispatch.go's filterAvailable) already excludes
// is_virtual correctly; this bot-side "auto" is a separate concept that
// never got the same protection until now.
export function pickAutoRouter(routers: Router[]): Router | null {
  return routers.find((r) => r.status === 'online' && !r.blocked && !r.is_virtual) ?? null;
}

async function resolveNodeId(routerName: string): Promise<{ id: string; name: string }> {
  const routers = await listRouters();
  if (routerName === 'auto') {
    const online = pickAutoRouter(routers);
    if (!online) throw new Error('No online routers available for "auto"');
    return { id: online.id, name: online.name };
  }
  const match = routers.find((r) => r.name === routerName);
  if (!match) throw new Error(`Router "${routerName}" not found`);
  return { id: match.id, name: match.name };
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run (from `bot/`): `npx tsx --test src/resolve-node.test.ts`
Expected: PASS (3 tests, 0 failures).

- [ ] **Step 5: Type-check and regression-test the whole bot**

Run (from `bot/`): `npx tsc --noEmit && npx tsx --test src/*.test.ts`
Expected: clean type-check; every test passes except the live-backend-only
`pingachock-client.test.ts`.

- [ ] **Step 6: Commit**

```bash
git add bot/src/pingachock-client.ts bot/src/resolve-node.test.ts
git commit -m "Bot: fix 'auto' router selection silently picking the virtual server node

Router now carries is_virtual (already in the API response, was never
mapped). pickAutoRouter is resolveNodeId's 'auto' logic pulled out as a
pure, testable function, now excluding is_virtual - matches what
internal/dispatch/dispatch.go's filterAvailable already does on the Go
side for its own auto-selection; the bot's separate client-side notion of
'auto' never had the same protection.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 2: Explicit "🖥 Server" option for VLESS Speedtest and TLS Handshake

**Files:**
- Modify: `bot/src/index.ts`

No new unit test - this is a router-picker label/filter change in
Telegraf UI code, verified via `tsc --noEmit` + manual runthrough, same as
every other menu flow in this file. The underlying dispatch behavior
(`checkVlessSpeed`/`checkTlsHandshake` called with router value
`'server'`) is unchanged and already covered by those features' own
existing tests.

- [ ] **Step 1: Update `getVlessRouterOption`**

Read `bot/src/index.ts`'s current `getVlessRouterOption` first, then
replace it:

```ts
function getVlessRouterOption(session: MySession): { label: string; value: string } {
  const routers = (session.vlessRouters ?? []).filter((r) => !r.is_virtual);
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    { label: '🖥 Server', value: 'server' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];
  const index = session.vlessRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}
```

- [ ] **Step 2: Update `getTlsHandshakeRouterOption`**

Read the current `getTlsHandshakeRouterOption` first, then replace it the
same way:

```ts
function getTlsHandshakeRouterOption(session: MySession): { label: string; value: string } {
  const routers = (session.tlsHandshakeRouters ?? []).filter((r) => !r.is_virtual);
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    { label: '🖥 Server', value: 'server' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];
  const index = session.tlsHandshakeRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}
```

(The `optionsLen` calculations in `extra:vless_toggle_router` and
`extra:tls_toggle_router` already compute `1 + (routers?.length ?? 0)` -
this undercounts by one now that there are two fixed entries (`Auto` and
`🖥 Server`) instead of one. Fixed in Step 3.)

- [ ] **Step 3: Fix the toggle handlers' option-count math**

Find `bot.action('extra:vless_toggle_router', ...)` and
`bot.action('extra:tls_toggle_router', ...)`. In both, change:
```ts
const optionsLen = 1 + (ctx.session.vlessRouters?.length ?? 0);
```
to:
```ts
const optionsLen = 2 + (ctx.session.vlessRouters ?? []).filter((r) => !r.is_virtual).length;
```
(and the TLS Handshake handler's equivalent line, using
`ctx.session.tlsHandshakeRouters` instead of `ctx.session.vlessRouters`).
This must match `getVlessRouterOption`/`getTlsHandshakeRouterOption`'s own
`options.length` exactly (`Auto` + `🖥 Server` + non-virtual routers) or
the toggle can land on an out-of-range index.

- [ ] **Step 4: Type-check**

Run (from `bot/`): `npx tsc --noEmit`
Expected: no output, exit code 0.

- [ ] **Step 5: Production build and full regression test**

Run (from `bot/`): `npm run build && npx tsx --test src/*.test.ts`
Expected: clean build; all tests pass except the live-backend-only
`pingachock-client.test.ts`.

- [ ] **Step 6: Commit**

```bash
git add bot/src/index.ts
git commit -m "Bot: explicit '🖥 Server' option for VLESS Speedtest and TLS Handshake

Both already dispatched correctly to the virtual server node if its raw
name ('server') happened to be selected from the generic router list -
just never labeled as anything special, easy to miss or pick by accident.
Now filtered out of the generic list (via is_virtual) and re-added as an
explicit, clearly-labeled option right after Auto.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 3: HTTP 101 check gains a router picker + node dispatch

**Files:**
- Modify: `bot/src/pingachock-client.ts`
- Modify: `bot/src/index.ts`
- Create: `bot/src/upgrade-scan-router.test.ts`

- [ ] **Step 1: Write the failing test**

Create `bot/src/upgrade-scan-router.test.ts`:

```ts
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run (from `bot/`): `npx tsx --test src/upgrade-scan-router.test.ts`
Expected: FAIL - `mergeUpgradeScanChecks is not a function`.

- [ ] **Step 3: Add `mergeUpgradeScanChecks` and the node-dispatch branch to `scanUpgrade`**

Open `bot/src/pingachock-client.ts`. Find `mapUpgradeScanResults` and add
the new helper right after it, then replace `scanUpgrade`:

```ts
// mergeUpgradeScanChecks merges the node-dispatch path's one-check-per-target
// results back into the same UpgradeScanResult[] shape
// mapUpgradeScanResults produces for the server path, so
// bot/src/index.ts's rendering code doesn't need to know which path ran.
export function mergeUpgradeScanChecks(targets: string[], checks: any[], nodeId: string): UpgradeScanResult[] {
  return targets.map((target, i) => {
    const run = checks[i]?.runs?.find((r: any) => r.node_id === nodeId);
    return { target, matched: Boolean(run?.result?.success) };
  });
}

// scanUpgrade: routerName 'server' (the default) keeps using the
// synchronous /api/v1/server-upgrade-scan endpoint unchanged; any other
// value dispatches an "upgrade" check to that node through the same
// generic /api/v1/checks path ping's own node-routed dispatch uses. See
// docs/superpowers/specs/2026-08-12-extra-checks-server-and-node-design.md.
export async function scanUpgrade(targets: string[], routerName: string = 'server'): Promise<UpgradeScanResult[]> {
  if (routerName === 'server') {
    const data = await fetchWithAuth('/api/v1/server-upgrade-scan', 'POST', { targets }, 'api');
    return mapUpgradeScanResults(data);
  }

  const { id: nodeId } = await resolveNodeId(routerName);
  const created = (await fetchWithAuth(
    '/api/v1/checks',
    'POST',
    { type: 'upgrade', targets, node_selector: { node_ids: [nodeId] } },
    'api'
  )) as any;
  const checkIds: string[] = created.batch_id ? created.checks.map((c: any) => c.id) : [created.id];
  const checks = await Promise.all(checkIds.map((id: string) => pollCheckUntilDone(id)));
  return mergeUpgradeScanChecks(targets, checks, nodeId);
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run (from `bot/`): `npx tsx --test src/upgrade-scan-router.test.ts`
Expected: PASS (3 tests, 0 failures).

- [ ] **Step 5: Add the router-picker UI to `bot/src/index.ts`**

Add session state - find the `MySession` type's
`awaitingTlsHandshakeTarget`/`tlsHandshakeRouterIndex`/`tlsHandshakeRouters`
block and add a sibling block for HTTP 101 check right after it:

```ts
  upgradeScanRouterIndex?: number;
  upgradeScanRouters?: Router[];
```

(`awaitingUpgradeScanTargets` already exists from the original HTTP 101
check feature - reused as-is, it already gates the target-list text
input.)

Add the router option resolver + keyboard right after
`getTlsHandshakeRouterOption`/`tlsHandshakeKeyboard`:

```ts
// getUpgradeScanRouterOption/upgradeScanRouterPickerKeyboard: same router
// list shape as VLESS/TLS Handshake's own pickers, but with an extra
// "✅ Продолжить" confirm button - HTTP 101 check's existing target-list
// prompt keyboard is extraChecksKeyboard(), not a router toggle, so
// grafting the toggle directly onto that same message would silently
// change what tapping it does mid-flow. An explicit confirm step avoids
// that ambiguity; VLESS/TLS Handshake don't need it because their toggle
// and their target/config prompt already live on the same message
// throughout.
function getUpgradeScanRouterOption(session: MySession): { label: string; value: string } {
  const routers = (session.upgradeScanRouters ?? []).filter((r) => !r.is_virtual);
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    { label: '🖥 Server', value: 'server' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];
  const index = session.upgradeScanRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}

function upgradeScanRouterOptionsLen(session: MySession): number {
  return 2 + (session.upgradeScanRouters ?? []).filter((r) => !r.is_virtual).length;
}

function upgradeScanRouterPickerKeyboard(session: MySession) {
  const routerOpt = getUpgradeScanRouterOption(session);
  return Markup.inlineKeyboard([
    [Markup.button.callback(routerOpt.label, 'extra:upgrade_toggle_router')],
    [Markup.button.callback('✅ Продолжить', 'extra:upgrade_confirm_router')],
    [Markup.button.callback('◀️ Назад', 'menu:root')]
  ]);
}
```

Find `bot.action('extra:http101', ...)` and replace its body - it
currently jumps straight to `awaitingUpgradeScanTargets = true` with no
router step. Change it to show the router picker first:

```ts
bot.action('extra:http101', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  ctx.session.upgradeScanRouterIndex = ctx.session.upgradeScanRouterIndex ?? 0;
  try {
    const allRouters = await apiClient.listRouters();
    ctx.session.upgradeScanRouters = allRouters.filter((r) => r.status === 'online');
    const optionsLen = upgradeScanRouterOptionsLen(ctx.session);
    if (optionsLen > 0 && (ctx.session.upgradeScanRouterIndex ?? 0) >= optionsLen) {
      ctx.session.upgradeScanRouterIndex = 0;
    }
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка:\n${errMsg}`, extraChecksKeyboard());
    return;
  }

  await safeEditOrReply(
    ctx,
    'Выбери узел (кнопка ниже), затем нажми ещё раз для подтверждения.',
    upgradeScanRouterPickerKeyboard(ctx.session)
  );
});

bot.action('extra:upgrade_toggle_router', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  const optionsLen = upgradeScanRouterOptionsLen(ctx.session);
  ctx.session.upgradeScanRouterIndex = ((ctx.session.upgradeScanRouterIndex ?? 0) + 1) % Math.max(1, optionsLen);
  await safeEditOrIgnore(
    ctx,
    'Выбери узел (кнопка ниже), затем нажми ещё раз для подтверждения.',
    upgradeScanRouterPickerKeyboard(ctx.session)
  );
});

bot.action('extra:upgrade_confirm_router', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingUpgradeScanTargets = true;
  await safeEditOrReply(
    ctx,
    'Пришли список хостов для HTTP 101 check (IPv4, CIDR, диапазон или домен, по одному на строку либо через запятую). Порт 443, протокол websocket. Максимум 100 целей.',
    extraChecksKeyboard()
  );
});
```

- [ ] **Step 6: Wire `routerOpt.value` through to `scanUpgrade` in the target-list handler**

Find the `awaitingUpgradeScanTargets && (await isAuthorizedUser(ctx))`
text handler block. It currently calls:
```ts
const results = await apiClient.scanUpgrade(parsed.targets);
```
Change it to pass the router chosen in Step 5:
```ts
const routerOpt = getUpgradeScanRouterOption(ctx.session);
const results = await apiClient.scanUpgrade(parsed.targets, routerOpt.value);
```
(Read the surrounding block first to place this correctly - the router
must be read *before* any later code resets `upgradeScanRouterIndex`, but
nothing in this block does that, so placing it right before the
`scanUpgrade` call is safe.)

- [ ] **Step 7: Reset the new session fields alongside the existing ones**

Find `bot.action('menu:root', ...)` and `bot.action('menu:extra', ...)`.
Neither needs a new flag reset (there's no new `awaiting*` flag for this
router-selection sub-step - `awaitingUpgradeScanTargets` already existed
and is still the only gating flag), but both should reset
`upgradeScanRouterIndex` so a stale toggle position doesn't carry over
into a fresh run. Add one line to each:

```ts
bot.action('menu:root', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingUpgradeScanTargets = false;
  ctx.session.awaitingVlessConfig = false;
  ctx.session.awaitingTlsHandshakeTarget = false;
  ctx.session.upgradeScanRouterIndex = 0;
  await ctx.answerCbQuery();
  await safeEditOrReply(ctx, await renderMainMenuText(), mainMenuKeyboard());
});
```

```ts
bot.action('menu:extra', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingUpgradeScanTargets = false;
  ctx.session.awaitingVlessConfig = false;
  ctx.session.awaitingTlsHandshakeTarget = false;
  ctx.session.upgradeScanRouterIndex = 0;
  await safeEditOrReply(ctx, 'Дополнительные проверки:', extraChecksKeyboard());
});
```

- [ ] **Step 8: Type-check**

Run (from `bot/`): `npx tsc --noEmit`
Expected: no output, exit code 0.

- [ ] **Step 9: Production build and full regression test**

Run (from `bot/`): `npm run build && npx tsx --test src/*.test.ts`
Expected: clean build; all tests pass except the live-backend-only
`pingachock-client.test.ts`.

- [ ] **Step 10: Commit**

```bash
git add bot/src/pingachock-client.ts bot/src/index.ts bot/src/upgrade-scan-router.test.ts
git commit -m "Bot: HTTP 101 check gains a router picker (Auto / Server / node)

scanUpgrade(targets, routerName='server') - 'server' keeps the existing
synchronous /api/v1/server-upgrade-scan path unchanged; any node routes
through /api/v1/checks (type: upgrade) using the same batched-check +
poll-per-check-id + merge pattern ping's own node-routed dispatch already
uses. UI: toggle-then-confirm (not toggle-then-type like VLESS/TLS
Handshake) since HTTP 101 check's existing target-list prompt keyboard
would otherwise collide with a live router toggle on the same message.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

## Task 4: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full bot verification**

Run (from `bot/`): `npx tsc --noEmit && npm run build && npx tsx --test src/*.test.ts`
Expected: clean type-check, clean build, all tests pass except the
live-backend-only `pingachock-client.test.ts`.

- [ ] **Step 2: Full Go verification (regression check - this plan touches no Go code)**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: BUILD OK, VET OK, every package's tests pass (confirms nothing
in Task 1-3 accidentally touched Go code, matching the design doc's own
"no new Go code" scope statement).

- [ ] **Step 3: Manual smoke test, if a reachable backend + online node are available**

This needs live infrastructure this repo doesn't stand up for automated
tests. If available, exercise all three "Дополнительные проверки" flows
through the bot, picking `🖥 Server` for one run and a real node for
another, confirming both produce a sensible result and that `Auto` picks
a real node (not `server`) when both are online. If nothing is reachable
in this environment, skip this step and say so explicitly when reporting
completion - do not fabricate a result.

- [ ] **Step 4: Update the design spec's status**

In `docs/superpowers/specs/2026-08-12-extra-checks-server-and-node-design.md`,
change:
```
Status: APPROVED, ready for implementation planning.
```
to:
```
Status: DONE. Implemented per docs/superpowers/plans/2026-08-12-extra-checks-server-and-node.md.
```

- [ ] **Step 5: Commit and push**

```bash
git add docs/superpowers/specs/2026-08-12-extra-checks-server-and-node-design.md
git commit -m "Mark 'extra checks: server + node everywhere' design DONE

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
git push origin main
```
