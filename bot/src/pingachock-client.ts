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
