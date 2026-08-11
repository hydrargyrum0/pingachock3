import net from 'node:net';
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
const NODE_POLL_TIMEOUT_MS = 60000;

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

      const fields = parseRawFields(result?.raw);

      if (spec.kind === 'icmp') {
        if (result) {
          out.ICMP = formatIcmpSummary(fields.sent, fields.recv, result.latency_ms, translateCheckError(result.error_message));
          if (result.success) out.status = true;
        }
      } else {
        const state = result ? (result.success ? 'open' : 'closed') : 'unknown';
        out[`port_${spec.port}`] = state;
        if (state === 'open') out.status = true;
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
  'certificate verification failed': 'ошибка проверки сертификата'
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
// 🚫 icon (see index.ts's pingResultIcon) was built to surface.
export function onlyFailed(results: any[]): any[] {
  return results.filter((r) => r && typeof r === 'object' && ((r as any).status === false || (r as any).blocked === true));
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

export type UpgradeScanResult = { target: string; matched: boolean };

// mapUpgradeScanResults is split out from scanUpgrade so the response
// mapping itself is unit-testable without a live backend - mirrors
// toRouter's role for listRouters.
export function mapUpgradeScanResults(data: any): UpgradeScanResult[] {
  const results = Array.isArray(data?.results) ? data.results : [];
  return results.map((r: any) => ({ target: String(r?.target ?? ''), matched: Boolean(r?.matched) }));
}

// scanUpgrade: HTTP 101 (websocket upgrade) check, always against the
// backend itself (see /api/v1/server-upgrade-scan) - there is no
// node-routed equivalent, this check only ever makes sense from the
// server's own vantage point. See
// docs/superpowers/specs/2026-08-09-http-101-upgrade-check-design.md.
export async function scanUpgrade(targets: string[]): Promise<UpgradeScanResult[]> {
  const data = await fetchWithAuth('/api/v1/server-upgrade-scan', 'POST', { targets }, 'api');
  return mapUpgradeScanResults(data);
}
