import net from 'node:net';
import { settingsRepo } from './db';

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
    is_virtual: Boolean(n.is_virtual),
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

export function parseCheckPorts(checkPorts: string): { icmp: boolean; ports: string[] } {
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

// resolveNodeId is exported so index.ts's tracked-scan flow
// (runTrackedNodeScan needs an already-resolved nodeId, not a router name)
// can resolve once up front, the same way ping()'s own node-routed branch
// below already does internally.
export async function resolveNodeId(routerName: string): Promise<{ id: string; name: string }> {
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
      out.ICMP = formatIcmpSummary(r.icmp.packets_sent ?? 0, r.icmp.packets_recv ?? 0, r.icmp.latency_ms, translateCheckError(r.icmp.error));
    }
    for (const [port, state] of Object.entries(r.ports ?? {})) {
      out[`port_${port}`] = state;
    }
    out.blocked = classifyBlocked(out.ip, out.resolved_ip);
    return out;
  });
  return { results };
}

const NODE_POLL_INTERVAL_MS = 2000;
// Deliberately below telegraf's handlerTimeout (120_000ms, see index.ts) -
// must always resolve (even if just "gave up, here's what we have") before
// telegraf's own watchdog could fire. A real incident: this used to be
// 90_000ms, exactly telegraf's *default* handlerTimeout, and colliding with
// it crashed the whole bot for every user over one slow ping (telegraf's
// default error handler re-throws - see the bot.catch() comment in
// index.ts). See docs/superpowers/specs/2026-07-19-telegram-bot-merge-design.md.
//
// Raised from 60_000 to 100_000 (still a healthy 20s under the 120_000
// ceiling) alongside internal/poller streaming results back per-check
// instead of once per whole tick (see poller.go's streamResults) - a
// large batch's slowest straggler used to make this deadline the actual
// cause of "big batches come back failed, small ones succeed" (a real
// user report), since not even the fast checks in that batch got
// reported until the slow one finished too. Streaming fixes the typical
// case on its own; this extra margin is defense-in-depth for a very large
// batch, where a check queued behind MaxConcurrent's limit can still take
// a while just to get a turn to run at all, independent of how promptly
// results get reported once it does.
const NODE_POLL_TIMEOUT_MS = 100000;

type CheckSpec = { kind: 'icmp' } | { kind: 'port'; port: string };

// createBatchedCheck also hands back batchId (null whenever the backend
// didn't assign one - only happens for a single-target dispatch, since
// CreateCheck only sets batch_id when len(targets) > 1) so callers that
// need batch-level progress (runTrackedNodeScan, below) don't have to
// re-derive it - nodePing itself only ever needed checkIds and keeps
// ignoring batchId.
async function createBatchedCheck(spec: CheckSpec, targets: string[], nodeId: string): Promise<{ batchId: string | null; checkIds: string[] }> {
  const body =
    spec.kind === 'icmp'
      ? { type: 'ping', targets, node_selector: { node_ids: [nodeId] } }
      : { type: 'tcp', targets, params: { port: Number(spec.port) }, node_selector: { node_ids: [nodeId] } };
  const created = (await fetchWithAuth('/api/v1/checks', 'POST', body, 'api')) as any;
  if (created.batch_id) {
    return { batchId: String(created.batch_id), checkIds: created.checks.map((c: any) => String(c.id)) };
  }
  return { batchId: null, checkIds: [String(created.id)] };
}

// isCheckStillPending mirrors pollCheckUntilDone's own terminal-status
// check (status !== 'pending' && status !== 'running') - pulled out so
// mergeNodeResults can tell "this check genuinely finished with no result"
// (shouldn't normally happen, but see its own fallback) apart from "we
// stopped waiting on it while the agent was still working" - the two used
// to collapse into the same "no reply"/"closed" rendering, which is what
// made a slow-but-working node's checks look identical to genuinely dead
// ones. See mergeNodeResults' own doc comment for the full story.
function isCheckStillPending(check: any): boolean {
  const status = check?.status;
  return status === 'pending' || status === 'running';
}

async function pollCheckUntilDone(checkId: string, timeoutMs: number = NODE_POLL_TIMEOUT_MS): Promise<any> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const check = await fetchWithAuth(`/api/v1/checks/${checkId}?expand=runs`, 'GET', undefined, 'api');
    const status = (check as any)?.status;
    if (status !== 'pending' && status !== 'running') return check;
    if (Date.now() > deadline) return check; // give up, report whatever we have
    await sleep(NODE_POLL_INTERVAL_MS);
  }
}

// mergeNodeResults' finalAttempt controls what a target whose check is
// still pending/running when we stop waiting on it renders as - see
// PendingFollowUp/awaitPendingFollowUp below for the two-pass scheme this
// supports: nodePing's own first pass (finalAttempt left false/undefined)
// renders it as "⏳ ещё выполняется" (see index.ts's pingResultIcon) and
// hands the still-open check IDs back as a PendingFollowUp instead of
// silently reporting them failed - the real fix for a real user report:
// a batch big enough (or a node underpowered enough) that some checks
// legitimately don't finish within NODE_POLL_TIMEOUT_MS used to come back
// completely indistinguishable from a real "no reply", even for targets
// the node was still actively, successfully going to answer for a few
// seconds later. No fixed budget (raising NODE_POLL_TIMEOUT_MS,
// MaxConcurrentChecks, anything else agent- or bot-side) can fully rule
// that out for an arbitrarily large batch or an arbitrarily slow node -
// so instead of gambling on a bigger number being enough, index.ts's
// caller now polls a second time in the background (not bound by
// Telegraf's own handlerTimeout, since the initial reply has already been
// sent) and sends a follow-up message once those specific checks actually
// finish. finalAttempt=true is that second pass's own last call: if a
// check is *still* pending even then, mergeNodeResults reports it as a
// real, final failure (status stays false) with a distinct label instead
// of "⏳" again - honest about having genuinely given up, not stuck
// implying a follow-up is still coming when it isn't.
export function mergeNodeResults(
  targets: string[],
  nodeId: string,
  routerName: string,
  finished: Array<{ spec: CheckSpec; checks: any[] }>,
  finalAttempt = false
): any[] {
  return targets.map((target) => {
    const out: any = { ip: target, resolved_ip: target, status: false, pending: false, router_name: routerName };
    for (const { spec, checks } of finished) {
      const check = checks.find((c) => c.target === target);
      const run = check?.runs?.find((r: any) => r.node_id === nodeId);
      const result = run?.result;
      const stillWaiting = !result && isCheckStillPending(check);
      const givingUpNow = stillWaiting && finalAttempt;
      if (stillWaiting && !givingUpNow) out.pending = true;

      const fields = parseRawFields(result?.raw);

      if (spec.kind === 'icmp') {
        if (result) {
          out.ICMP = formatIcmpSummary(fields.sent, fields.recv, result.latency_ms, translateCheckError(result.error_message));
          if (result.success) out.status = true;
        } else if (givingUpNow) {
          out.ICMP = 'нет ответа от узла (не дождались)';
        } else if (stillWaiting) {
          out.ICMP = 'ещё выполняется';
        }
      } else {
        if (result) {
          const state = result.success ? 'open' : 'closed';
          out[`port_${spec.port}`] = state;
          if (state === 'open') out.status = true;
        } else {
          out[`port_${spec.port}`] = givingUpNow ? 'таймаут' : stillWaiting ? 'ожидание' : 'unknown';
        }
      }

      if (fields.resolvedTarget) out.resolved_ip = fields.resolvedTarget;
    }
    out.blocked = classifyBlocked(out.ip, out.resolved_ip);
    return out;
  });
}

// isLoopbackIp matches the whole 127.0.0.0/8 block (not just 127.0.0.1) plus
// its IPv6 equivalent - any of them is equally impossible for a real public
// DNS record to legitimately resolve to.
function isLoopbackIp(ip: string): boolean {
  return ip.startsWith('127.') || ip === '::1';
}

// classifyBlocked implements Section A of
// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md:
// a *domain* target whose DNS resolved to a loopback address is
// Turkmenistan's DNS-poisoning signature (not a real network failure) -
// reported as blocked regardless of whether that loopback address itself
// answers pings. Raw IP targets are exempt: ICMP-filtered-but-TCP-open is
// normal behavior for a bare IP and must not be misread as censorship.
export function classifyBlocked(target: string, resolvedIp: string): boolean {
  if (!resolvedIp) return false;
  const isDomainTarget = net.isIP(target) === 0;
  return isDomainTarget && isLoopbackIp(resolvedIp);
}

// CHECK_ERROR_TRANSLATIONS maps the stable, language-neutral classification
// tokens internal/checks emits (see classifyNetError/classifyPingError in
// internal/checks/checks.go and ping.go) to the Russian text bot users
// actually see. The Go layer deliberately stays English/stable since it's
// also the public API's ErrorMessage contract (internal/api/openapi.yaml)
// - this is where that gets localized. Anything not in this table (a
// future token, or something unexpected) falls through unchanged in
// translateCheckError below rather than disappearing - still far better
// than the raw Go error text ("exit status 1") this replaced, see
// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md
// Section B item 1.
const CHECK_ERROR_TRANSLATIONS: Record<string, string> = {
  'no reply': 'нет ответа',
  timeout: 'таймаут',
  'dns resolution failed': 'домен не резолвится',
  'ping failed': 'ошибка проверки',
  'connection refused': 'соединение отклонено',
  'connection failed': 'не удалось подключиться',
  'certificate verification failed': 'ошибка проверки сертификата',
  // TLSChecker's supplementary ICMP probe (internal/checks/tls.go,
  // diagnoseUnreachable) - fired only for the ambiguous "timeout"/
  // "connection failed" buckets above, telling apart "whole host is down"
  // from "just this port is filtered, host answers pings fine".
  'ip unreachable': 'IP адрес недоступен (не отвечает на ping)',
  'port unreachable': 'порт недоступен (хост отвечает на ping)',
  // A node's operator-pinned interface disappeared mid-check (cable
  // unplugged, Wi-Fi off, adapter removed) - see
  // docs/superpowers/specs/2026-08-13-vpn-resilient-node-networking-design.md
  // Part 3. Distinct wording on purpose: this is an agent/host problem, not
  // a signal about the target's own reachability, so it shouldn't read like
  // every check on the node just started failing for censorship reasons.
  'network interface unavailable': 'сетевой интерфейс узла недоступен (нужно заново запустить configure)'
};

export function translateCheckError(message: string | null | undefined): string | undefined {
  if (!message) return message ?? undefined;
  return CHECK_ERROR_TRANSLATIONS[message] ?? message;
}

// onlyFailed keeps results a periodic health report should surface: a real
// reachability failure (status === false), or a DNS-poisoning-blocked
// target (see classifyBlocked) even when status computed true - e.g. the
// poisoned loopback address happens to answer on the exact port being
// checked, from the backend's own local perspective. Filtering on status
// alone used to silently drop censorship events from the very report the
// 🚫 icon (see index.ts's pingResultIcon) was built to surface. A pending
// result (see mergeNodeResults' own doc comment) is excluded even though
// status is false for it too - it isn't a confirmed failure, just not
// answered *yet*, and a periodic health digest calling it a failure would
// be exactly the false negative this whole two-pass scheme exists to stop
// making.
export function onlyFailed(results: any[]): any[] {
  return results.filter(
    (r) => r && typeof r === 'object' && !(r as any).pending && ((r as any).status === false || (r as any).blocked === true)
  );
}

// formatIcmpSummary is the "3 из 4" packet-loss display, plus the real
// average latency of just the packets that actually came back (not the
// wall-clock time of the whole multi-packet run - see
// docs/superpowers/specs/2026-07-25-ping-result-classification-design.md
// Section B, and the matching fix in internal/checks/ping.go's
// averageReplyTimeMs).
export function formatIcmpSummary(sent: number, recv: number, latencyMs: number | null | undefined, errorMessage?: string): string {
  const lossLabel = sent > 0 ? `${recv} из ${sent}` : '';
  if (recv > 0) {
    const latencyPart = latencyMs != null ? `, ${Math.round(latencyMs)} ms` : '';
    return lossLabel ? `${lossLabel}${latencyPart}` : `${Math.round(latencyMs ?? 0)} ms`;
  }
  if (lossLabel) {
    return errorMessage ? `${lossLabel} (${errorMessage})` : lossLabel;
  }
  return errorMessage || 'no reply';
}

// parseRawFields reads the subset of a check_run's Raw JSON (see
// internal/checks/ping.go and tcp.go) mergeNodeResults cares about, in one
// parse - it used to JSON.parse the exact same raw string twice (once each
// for packet counts and resolved_target), mirroring the mistake
// internal/api/public/serverping.go's own pingRawFields/parsePingRaw was
// introduced specifically to avoid on the Go side.
function parseRawFields(raw: unknown): { sent: number; recv: number; resolvedTarget: string | null } {
  const empty = { sent: 0, recv: 0, resolvedTarget: null };
  if (!raw) return empty;
  try {
    const parsed = typeof raw === 'string' ? JSON.parse(raw) : raw;
    return {
      sent: typeof parsed?.packets_sent === 'number' ? parsed.packets_sent : 0,
      recv: typeof parsed?.packets_recv === 'number' ? parsed.packets_recv : 0,
      resolvedTarget: typeof parsed?.resolved_target === 'string' ? parsed.resolved_target : null
    };
  } catch {
    return empty;
  }
}

// PendingCheck/PendingFollowUp/awaitPendingFollowUp implement the
// two-pass scheme mergeNodeResults' own doc comment describes - one
// PendingCheck per (target, spec) pair whose check_run hadn't finished by
// the time nodePing's own poll gave up.
export type PendingCheck = { spec: CheckSpec; checkId: string; target: string };
export type PendingFollowUp = { nodeId: string; routerName: string; items: PendingCheck[] };

// specKey groups PendingCheck entries back into mergeNodeResults' expected
// Array<{spec, checks}> shape - "icmp" vs "port:N" is the only distinction
// that matters (two different ports need two separate groups so a target
// present in both doesn't get one spec's check silently dropped).
function specKey(spec: CheckSpec): string {
  return spec.kind === 'icmp' ? 'icmp' : `port:${spec.port}`;
}

async function nodePing(
  targets: string[],
  nodeId: string,
  routerName: string,
  icmp: boolean,
  ports: string[]
): Promise<{ results: any[]; pendingFollowUp?: PendingFollowUp }> {
  const specs: CheckSpec[] = [...(icmp ? [{ kind: 'icmp' as const }] : []), ...ports.map((port) => ({ kind: 'port' as const, port }))];
  if (specs.length === 0) return { results: [] };

  const dispatched = await Promise.all(
    specs.map(async (spec) => ({ spec, checkIds: (await createBatchedCheck(spec, targets, nodeId)).checkIds }))
  );

  const finished = await Promise.all(
    dispatched.map(async ({ spec, checkIds }) => ({
      spec,
      checks: await Promise.all(checkIds.map((id) => pollCheckUntilDone(id)))
    }))
  );

  const items: PendingCheck[] = [];
  for (const { spec, checks } of finished) {
    for (const check of checks) {
      if (isCheckStillPending(check)) {
        items.push({ spec, checkId: String(check.id), target: String(check.target) });
      }
    }
  }

  return {
    results: mergeNodeResults(targets, nodeId, routerName, finished),
    pendingFollowUp: items.length > 0 ? { nodeId, routerName, items } : undefined
  };
}

// awaitPendingFollowUp is the second pass: re-polls exactly the checks
// nodePing's own first pass gave up on, this time with timeoutMs of fresh
// patience (the caller - index.ts, not bound by Telegraf's handlerTimeout
// once it's already sent the first reply - can afford to be far more
// generous here than NODE_POLL_TIMEOUT_MS). Returns results only for the
// targets that were actually still pending, in mergeNodeResults'
// finalAttempt=true mode - see that function's doc comment for what that
// changes. Returns [] immediately (no network calls at all) when there was
// nothing pending, so an always-fast batch never pays for this.
export async function awaitPendingFollowUp(followUp: PendingFollowUp, timeoutMs: number): Promise<any[]> {
  const { nodeId, routerName, items } = followUp;
  if (items.length === 0) return [];

  const rechecked = await Promise.all(
    items.map(async (item) => ({ spec: item.spec, check: await pollCheckUntilDone(item.checkId, timeoutMs) }))
  );

  const grouped = new Map<string, { spec: CheckSpec; checks: any[] }>();
  for (const { spec, check } of rechecked) {
    const key = specKey(spec);
    const group = grouped.get(key) ?? { spec, checks: [] };
    group.checks.push(check);
    grouped.set(key, group);
  }

  const targets = [...new Set(items.map((i) => i.target))];
  return mergeNodeResults(targets, nodeId, routerName, [...grouped.values()], true);
}

// --- Tracked node scans (live progress instead of a fixed poll budget) ---
//
// nodePing/ping above dispatch, then block until every check is done or a
// per-call timeout gives up - fine for the bot's other, smaller
// apiClient.ping() call sites (periodic health checks etc., left
// untouched), wrong for the interactive /ping command's own large scans:
// no fixed timeout is ever really "big enough" for an arbitrarily large
// subnet on an arbitrarily slow node (see mergeNodeResults' own doc
// comment), and there was no way to show progress while it ran anyway.
// runTrackedNodeScan below is index.ts's real fix for that: dispatch once,
// poll cheap *batch-level* progress on its own schedule (no per-check HTTP
// calls at all - see the N+1 problem this avoids in
// internal/store/results.go's ListRunsForChecks doc comment), report it
// via onProgress, and only fetch full results once, in bulk, at the end.

// DISPATCH_CHUNK_SIZE mirrors ListChecksFilter's own 200-row page cap
// (internal/store/checks.go) - one progress "page" per dispatched chunk.
// DISPATCH_CONCURRENCY bounds how many POST /api/v1/checks calls run at
// once: firing all of them simultaneously for a huge scan would just move
// the bottleneck from "one slow sequential loop" (the old behavior) to "a
// thundering herd hitting the backend at once" instead of fixing anything.
const DISPATCH_CHUNK_SIZE = 200;
const DISPATCH_CONCURRENCY = 4;

// SCAN_PROGRESS_POLL_INTERVAL_MS is how often runTrackedNodeScan checks in
// on progress (and index.ts, in turn, edits the Telegram message) - well
// under Telegram's rate limits for editing one message repeatedly.
// SCAN_OVERALL_CEILING_MS is the one fixed budget this whole scheme still
// has, deliberately generous (30 real minutes, not tuned to any specific
// batch size or node speed) rather than tight like the old
// NODE_POLL_TIMEOUT_MS - whatever's still open when it's hit gets
// mergeNodeResults' finalAttempt=true treatment, same as
// awaitPendingFollowUp's own final pass.
export const SCAN_PROGRESS_POLL_INTERVAL_MS = 5000;
export const SCAN_OVERALL_CEILING_MS = 30 * 60 * 1000;

export function chunkTargets<T>(items: T[], size: number): T[][] {
  const chunks: T[][] = [];
  for (let i = 0; i < items.length; i += size) {
    chunks.push(items.slice(i, i + size));
  }
  return chunks;
}

// runWithConcurrency runs `items` through `worker` with at most `limit` in
// flight at once, preserving input order in the returned array regardless
// of which one finishes first.
export async function runWithConcurrency<T, R>(items: T[], limit: number, worker: (item: T) => Promise<R>): Promise<R[]> {
  const results: R[] = new Array(items.length);
  let next = 0;
  async function runNext(): Promise<void> {
    const i = next++;
    if (i >= items.length) return;
    results[i] = await worker(items[i]);
    return runNext();
  }
  const workers = Array.from({ length: Math.min(Math.max(limit, 1), items.length) }, () => runNext());
  await Promise.all(workers);
  return results;
}

type DispatchedChunk = { spec: CheckSpec; batchId: string | null; checkIds: string[] };

export type ScanProgress = { done: number; total: number };

// aggregateScanProgress is the pure reducer behind fetchScanProgress below
// - split out so the "sum done/total across however many chunks a big
// scan needed" math is unit-testable without any network involved.
export function aggregateScanProgress(perChunk: ScanProgress[]): ScanProgress {
  return perChunk.reduce((acc, p) => ({ done: acc.done + p.done, total: acc.total + p.total }), { done: 0, total: 0 });
}

// fetchChunkProgress costs one paginated ListChecks read per
// DISPATCH_CHUNK_SIZE checks (i.e. one call per chunk, almost always -
// see DISPATCH_CHUNK_SIZE's own comment), not one per check. The
// batchId === null branch is the single-target-dispatch edge case
// (createBatchedCheck's own doc comment) - one direct GET for that one
// check, never a batch_id filter that wouldn't exist for it.
async function fetchChunkProgress(chunk: DispatchedChunk): Promise<ScanProgress> {
  const total = chunk.checkIds.length;
  if (!chunk.batchId) {
    const check = (await fetchWithAuth(`/api/v1/checks/${chunk.checkIds[0]}`, 'GET', undefined, 'api')) as any;
    return { done: isCheckStillPending(check) ? 0 : 1, total: 1 };
  }

  let done = 0;
  let offset = 0;
  for (;;) {
    const page = (await fetchWithAuth(
      `/api/v1/checks?batch_id=${encodeURIComponent(chunk.batchId)}&limit=${DISPATCH_CHUNK_SIZE}&offset=${offset}`,
      'GET',
      undefined,
      'api'
    )) as any;
    const rows: any[] = Array.isArray(page?.checks) ? page.checks : [];
    for (const c of rows) {
      if (!isCheckStillPending(c)) done++;
    }
    if (rows.length < DISPATCH_CHUNK_SIZE) break;
    offset += DISPATCH_CHUNK_SIZE;
  }
  return { done, total };
}

// fetchChunkChecksWithRuns is fetchChunkProgress's counterpart for the
// final, one-time full-result fetch - same pagination, expand=runs this
// time (see internal/store/results.go's ListRunsForChecks doc comment for
// why that's one bulk query server-side too, not an N+1).
async function fetchChunkChecksWithRuns(chunk: DispatchedChunk): Promise<any[]> {
  if (!chunk.batchId) {
    const check = await fetchWithAuth(`/api/v1/checks/${chunk.checkIds[0]}?expand=runs`, 'GET', undefined, 'api');
    return [check];
  }

  const out: any[] = [];
  let offset = 0;
  for (;;) {
    const page = (await fetchWithAuth(
      `/api/v1/checks?batch_id=${encodeURIComponent(chunk.batchId)}&limit=${DISPATCH_CHUNK_SIZE}&offset=${offset}&expand=runs`,
      'GET',
      undefined,
      'api'
    )) as any;
    const rows: any[] = Array.isArray(page?.checks) ? page.checks : [];
    out.push(...rows);
    if (rows.length < DISPATCH_CHUNK_SIZE) break;
    offset += DISPATCH_CHUNK_SIZE;
  }
  return out;
}

// runTrackedNodeScan is the /ping command's real dispatch path now (see
// this section's own intro comment). Never rejects on a slow/large batch -
// the only way it stops early is SCAN_OVERALL_CEILING_MS, and even then it
// still returns a full result set (mergeNodeResults' finalAttempt=true for
// whatever's still open), never throws just because something was slow.
export async function runTrackedNodeScan(params: {
  targets: string[];
  nodeId: string;
  routerName: string;
  icmp: boolean;
  ports: string[];
  onProgress?: (progress: ScanProgress, elapsedMs: number) => void;
}): Promise<{ results: any[] }> {
  const { targets, nodeId, routerName, icmp, ports, onProgress } = params;
  const specs: CheckSpec[] = [...(icmp ? [{ kind: 'icmp' as const }] : []), ...ports.map((port) => ({ kind: 'port' as const, port }))];
  if (specs.length === 0 || targets.length === 0) return { results: [] };

  const dispatchJobs = specs.flatMap((spec) => chunkTargets(targets, DISPATCH_CHUNK_SIZE).map((chunkOfTargets) => ({ spec, chunkOfTargets })));
  const chunks = await runWithConcurrency(dispatchJobs, DISPATCH_CONCURRENCY, async (job) => {
    const { batchId, checkIds } = await createBatchedCheck(job.spec, job.chunkOfTargets, nodeId);
    return { spec: job.spec, batchId, checkIds } as DispatchedChunk;
  });

  const startedAt = Date.now();
  let ceilingHit = false;
  for (;;) {
    const perChunk = await Promise.all(chunks.map(fetchChunkProgress));
    const progress = aggregateScanProgress(perChunk);
    onProgress?.(progress, Date.now() - startedAt);
    if (progress.total === 0 || progress.done >= progress.total) break;
    if (Date.now() - startedAt > SCAN_OVERALL_CEILING_MS) {
      ceilingHit = true;
      break;
    }
    await sleep(SCAN_PROGRESS_POLL_INTERVAL_MS);
  }

  const perChunkChecks = await Promise.all(chunks.map(fetchChunkChecksWithRuns));
  const grouped = new Map<string, { spec: CheckSpec; checks: any[] }>();
  chunks.forEach((chunk, i) => {
    const key = specKey(chunk.spec);
    const group = grouped.get(key) ?? { spec: chunk.spec, checks: [] };
    group.checks.push(...perChunkChecks[i]);
    grouped.set(key, group);
  });

  return { results: mergeNodeResults(targets, nodeId, routerName, [...grouped.values()], ceilingHit) };
}

// ping mirrors the old astroping API's GET /api/ping contract exactly
// (same params, same flat per-target result shape) so the ~3000 lines of
// existing bot UI code that consume it don't need to change - only this
// client does. router_name="server" is synchronous (no node involved);
// anything else resolves to a node and goes through the async
// checks/check_runs poll loop. Never takes a per-user token - the bot now
// authenticates with one shared api_key (see the design spec, section 4).
export async function ping(params: {
  ip_pool: string;
  router_name?: string;
  check_ports?: string;
}): Promise<{ results: any[]; pendingFollowUp?: PendingFollowUp }> {
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

export type UpgradeScanResult = { target: string; matched: boolean };

// mapUpgradeScanResults is split out from scanUpgrade so the response
// mapping itself is unit-testable without a live backend - mirrors
// toRouter's role for listRouters.
export function mapUpgradeScanResults(data: any): UpgradeScanResult[] {
  const results = Array.isArray(data?.results) ? data.results : [];
  return results.map((r: any) => ({ target: String(r?.target ?? ''), matched: Boolean(r?.matched) }));
}

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

export type VlessSpeedTestResult = { success: boolean; mbps?: number; errorMessage?: string };

function parseVlessRaw(raw: unknown): { mbps: number | null } {
  if (!raw) return { mbps: null };
  try {
    const parsed = typeof raw === 'string' ? JSON.parse(raw) : raw;
    return { mbps: typeof parsed?.mbps === 'number' ? parsed.mbps : null };
  } catch {
    return { mbps: null };
  }
}

// mapVlessSpeedTestResult is split out from checkVlessSpeed so the
// response-mapping itself is unit-testable without a live backend or a
// real VLESS server - mirrors mapUpgradeScanResults's role for scanUpgrade.
export function mapVlessSpeedTestResult(check: any, nodeId: string): VlessSpeedTestResult {
  const run = check?.runs?.find((r: any) => r.node_id === nodeId);
  const result = run?.result;
  if (!result) {
    return { success: false, errorMessage: 'нет ответа от узла' };
  }
  if (!result.success) {
    return { success: false, errorMessage: translateCheckError(result.error_message) ?? 'ошибка' };
  }
  const { mbps } = parseVlessRaw(result.raw);
  return { success: true, mbps: mbps ?? undefined };
}

// checkVlessSpeed: dispatches a "vless" check to one node - always a real
// node, never "server" (there is no server-side equivalent for this
// check, it only means something from a node's own network vantage
// point). config is the full xray-core config.json the caller already
// validated as parseable JSON. 90000ms: comfortably above the Go side's
// own 30s vlessOverallTimeout plus poll-interval slack, while staying
// under Telegraf's handlerTimeout: 120_000 configured in
// bot/src/index.ts - see NODE_POLL_TIMEOUT_MS's own doc comment for why
// that headroom matters (a real past incident, not a hypothetical one).
// See docs/superpowers/specs/2026-08-12-vless-speedtest-check-design.md.
export async function checkVlessSpeed(config: unknown, routerName: string): Promise<VlessSpeedTestResult> {
  const { id: nodeId } = await resolveNodeId(routerName);
  const created = (await fetchWithAuth(
    '/api/v1/checks',
    'POST',
    { type: 'vless', targets: ['vless-speedtest'], params: { config }, node_selector: { node_ids: [nodeId] } },
    'api'
  )) as any;
  const checkId = created.batch_id ? created.checks[0].id : created.id;
  const check = await pollCheckUntilDone(checkId, 90000);
  return mapVlessSpeedTestResult(check, nodeId);
}

// parseTlsHandshakeTarget: "IP SNI" (or "IP, SNI"), exactly two tokens.
// The first must be a literal IP (net.isIP) - matches the design's own
// scenario (dialing a relay's real IP directly), not a domain. The second
// is the SNI, accepted as-is with no further validation - it's just a
// hostname the caller is asserting their own relay/DNS config makes sense
// of, this package has no way to check that.
export function parseTlsHandshakeTarget(input: string): { ip: string; sni: string } | null {
  const tokens = input.trim().split(/[\s,]+/).filter(Boolean);
  if (tokens.length !== 2) return null;
  const [ip, sni] = tokens;
  if (net.isIP(ip) === 0) return null;
  return { ip, sni };
}

export type TlsHandshakeResult = { success: boolean; latencyMs?: number; errorMessage?: string };

// mapTlsHandshakeResult is split out from checkTlsHandshake so the
// response-mapping itself is unit-testable without a live backend -
// mirrors mapVlessSpeedTestResult's role for checkVlessSpeed.
export function mapTlsHandshakeResult(check: any, nodeId: string): TlsHandshakeResult {
  const run = check?.runs?.find((r: any) => r.node_id === nodeId);
  const result = run?.result;
  if (!result) {
    return { success: false, errorMessage: 'нет ответа от узла' };
  }
  if (!result.success) {
    return { success: false, errorMessage: translateCheckError(result.error_message) ?? 'ошибка' };
  }
  return { success: true, latencyMs: typeof result.latency_ms === 'number' ? result.latency_ms : undefined };
}

// checkTlsHandshake: dispatches a "tls" check to one node - always a real
// node, never "server" (there is no server-side equivalent, this only
// means something from a node's own network vantage point).
// allow_insecure is fixed true - only handshake speed matters here, not
// certificate trust (the placeholder's own TLS termination is the user's
// relay, not this repo). See
// docs/superpowers/specs/2026-08-12-tls-handshake-check-design.md.
export async function checkTlsHandshake(ip: string, sni: string, routerName: string): Promise<TlsHandshakeResult> {
  const { id: nodeId } = await resolveNodeId(routerName);
  const created = (await fetchWithAuth(
    '/api/v1/checks',
    'POST',
    {
      type: 'tls',
      targets: [ip],
      params: { port: 443, sni, allow_insecure: true },
      node_selector: { node_ids: [nodeId] }
    },
    'api'
  )) as any;
  const checkId = created.batch_id ? created.checks[0].id : created.id;
  const check = await pollCheckUntilDone(checkId);
  return mapTlsHandshakeResult(check, nodeId);
}
