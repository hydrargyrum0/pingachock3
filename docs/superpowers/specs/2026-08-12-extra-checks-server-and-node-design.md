# Дополнительные проверки: server + node everywhere — design

Status: DONE. Implemented per docs/superpowers/plans/2026-08-12-extra-checks-server-and-node.md.

## Problem

All three "➕ Дополнительные проверки" bot flows (HTTP 101 check, VLESS
Speedtest, TLS Handshake) should be runnable against either a real node or
the backend itself ("server"). Investigating this surfaced two separate
findings:

1. VLESS Speedtest and TLS Handshake's router pickers already include the
   virtual "server" node - it appears in `GET /api/v1/nodes` like any
   other node (no `is_virtual` filtering anywhere in the bot), is always
   online (`serveragent.Runner` touches its heartbeat every 30s), and
   `resolveNodeId(routerName)` treats any non-`'auto'` name as an exact
   router-name match - so picking the entry literally named `"server"`
   already dispatches correctly today. It's just not *labeled* as
   anything special, mixed in among real node names, easy to select by
   accident or never notice as an option.
2. **A real bug**, found while investigating (1): the bot's own `'auto'`
   resolution (`resolveNodeId`'s `'auto'` branch) has no `is_virtual`
   awareness at all - it just picks the first `online && !blocked` router
   from `GET /api/v1/nodes`. If the virtual "server" node happens to sort
   before real nodes (`ORDER BY created_at` - plausible, since it's
   provisioned at backend startup), "Auto" can silently resolve to the
   backend instead of a real node, for Ping and both new checks alike.
   The Go backend's own dispatch-time auto-selection
   (`internal/dispatch/dispatch.go`'s `filterAvailable`) already excludes
   `n.IsVirtual` correctly - the bot's separate, client-side notion of
   "auto" never got the same protection.

HTTP 101 check is different from the other two: it has no router picker
at all today, hardcoded to the dedicated synchronous
`/api/v1/server-upgrade-scan` endpoint (server only, no node dispatch
path exists).

## Scope

**In scope:**
- `Router` gains an `is_virtual` field (already present in the API
  response, just never mapped in the bot); `resolveNodeId`'s `'auto'`
  branch excludes it. One shared fix, used by every caller of
  `resolveNodeId` (`ping`, `checkVlessSpeed`, `checkTlsHandshake`, and the
  new node-dispatch path for HTTP 101 check below) - no per-feature
  special-casing needed.
- VLESS Speedtest and TLS Handshake's router-toggle lists: filter out the
  virtual node from the generic `routers.map(...)` entries (via
  `is_virtual`, not a fragile name comparison - an admin could legitimately
  name a real node "server"), and add an explicit `🖥 Server` option right
  after `Auto`. No changes to `checkVlessSpeed`/`checkTlsHandshake`
  themselves - passing router value `'server'` already resolves and
  dispatches correctly (that's finding (1) above).
- HTTP 101 check gains the same router-toggle UI shape as the other two
  (`Auto` / `🖥 Server` / named nodes). `scanUpgrade` gains a `routerName`
  parameter: `'server'` keeps using `/api/v1/server-upgrade-scan`
  unchanged; any node routes through `/api/v1/checks` with
  `type: "upgrade"`, the same batched-check-per-target +
  poll-each-check-id + merge pattern `ping`'s node-routed path
  (`createBatchedCheck`/`pollCheckUntilDone`/`mergeNodeResults` in
  `bot/src/pingachock-client.ts`) already uses for `ping`/`tcp`.

**Out of scope:** no new Go code anywhere - `internal/checks`'s `upgrade`
Checker already works identically whether dispatched via the dedicated
server-ping-style endpoint or the generic `/api/v1/checks` +
`node_selector` path (same `Checker` interface, same registry, this was
already proven true for `tls`/`vless` in the two earlier designs). No
`ALL`-nodes fan-out for any of the three checks - unchanged from the
existing designs' own scope decisions.

## Architecture

### `Router` type + `resolveNodeId` (`bot/src/pingachock-client.ts`)

```ts
export type Router = {
  id: string;
  name: string;
  token?: string;
  status: string;
  platform: string;
  blocked: boolean;
  is_virtual: boolean;   // new
  last_seen: string | null;
  created_at?: string;
};
```
`toRouter()` maps `is_virtual: Boolean(n.is_virtual)`. `resolveNodeId`'s
`'auto'` branch becomes:
```ts
const online = routers.find((r) => r.status === 'online' && !r.blocked && !r.is_virtual);
```

### VLESS Speedtest / TLS Handshake router pickers (`bot/src/index.ts`)

`getVlessRouterOption`/`getTlsHandshakeRouterOption` change from:
```ts
const routers = session.vlessRouters ?? [];
const options = [{ label: 'Auto', value: 'auto' }, ...routers.map(...)];
```
to:
```ts
const routers = (session.vlessRouters ?? []).filter((r) => !r.is_virtual);
const options = [
  { label: pingRouterLabels.auto, value: 'auto' },
  { label: '🖥 Server', value: 'server' },
  ...routers.map((r) => ({ label: r.name, value: r.name }))
];
```
Selecting the `🖥 Server` option calls `checkVlessSpeed(config, 'server')`
/ `checkTlsHandshake(ip, sni, 'server')` exactly as today - `resolveNodeId`
still finds the router literally named `"server"` (still present in the
unfiltered `listRouters()` result the client fetches internally), no
special-casing needed there.

### HTTP 101 check: router picker + node dispatch

New `getUpgradeScanRouterOption`/`upgradeScanKeyboard` in `bot/src/index.ts`,
same shape as the other two (`Auto` / `🖥 Server` / named nodes, own
session state `upgradeScanRouterIndex`/`upgradeScanRouters`).

`scanUpgrade` (`bot/src/pingachock-client.ts`):
```ts
export async function scanUpgrade(targets: string[], routerName: string = 'server'): Promise<UpgradeScanResult[]> {
  if (routerName === 'server') {
    const data = await fetchWithAuth('/api/v1/server-upgrade-scan', 'POST', { targets }, 'api');
    return mapUpgradeScanResults(data);
  }
  const { id: nodeId } = await resolveNodeId(routerName);
  const created = (await fetchWithAuth(
    '/api/v1/checks', 'POST',
    { type: 'upgrade', targets, node_selector: { node_ids: [nodeId] } },
    'api'
  )) as any;
  const checkIds: string[] = created.batch_id ? created.checks.map((c: any) => c.id) : [created.id];
  const checks = await Promise.all(checkIds.map((id: string) => pollCheckUntilDone(id)));
  return checks.map((check: any, i: number) => {
    const run = check?.runs?.find((r: any) => r.node_id === nodeId);
    return { target: targets[i], matched: Boolean(run?.result?.success) };
  });
}
```
`routerName` defaults to `'server'` so the function's existing signature
stays backward compatible for any caller that doesn't pass it (none do
today, but keeps the change minimal/safe). Result shape
(`UpgradeScanResult[]`) is unchanged either way, so
`bot/src/index.ts`'s existing report-rendering code for HTTP 101 check
needs no changes beyond adding the router prompt/toggle step before
asking for the target list.

## Testing

- `resolveNodeId`'s `'auto'`-excludes-virtual fix: new unit test using a
  fake `listRouters` fixture with a virtual node sorted first, asserting
  `'auto'` never resolves to it. (`resolveNodeId` is currently unexported
  - export it for this test, mirroring how other previously-internal
  helpers in this file already got exported for testability.)
- `scanUpgrade`'s new node-dispatch branch: the response-merging logic
  (`checks.map((check, i) => ...)`) is straightforward enough to unit-test
  directly given a fake array of `check` objects, without needing
  `pollCheckUntilDone`/network mocking - assert target-order and
  `matched` mapping.
- Router-picker UI changes (`bot/src/index.ts`) - no isolated unit test,
  same as every other menu flow in this bot; verified via `tsc --noEmit` +
  manual runthrough.

## Next steps

Invoke `superpowers:writing-plans` to turn this into an implementation
plan.
