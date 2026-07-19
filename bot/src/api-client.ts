import { settingsRepo } from './db';

export type RouterLastSeen =
  | {
      Time: string;
      Valid: boolean;
    }
  | string
  | null;

export type Router = {
  id: number;
  name: string;
  token: string;
  status: string;
  platform: string;
  blocked: boolean;
  last_seen: RouterLastSeen;
  created_at?: string;
};

export type Client = {
  id: number;
  name: string;
  token: string;
  blocked: boolean;
  created_at: string;
};

export type ApiStatus = {
  status?: string;
  routers_online?: number;
  routers_connected?: number;
  clients_connected?: number;
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

type TokenType = 'admin' | 'client';

async function fetchWithAuth(
  url: string,
  method: string = 'GET',
  body?: unknown,
  tokenType: TokenType = 'admin',
  extraHeaders?: Record<string, string>,
  tokenOverride?: string | null
): Promise<unknown> {
  const apiUrl = await settingsRepo.getApiUrl();
  if (!apiUrl) {
    throw new Error('API URL is not configured. Set it via /admin -> API URL');
  }

  const token =
    tokenOverride ??
    (tokenType === 'admin' ? await settingsRepo.getAdminToken() : await settingsRepo.getClientToken());

  if (!token) {
    if (tokenType === 'admin') {
      throw new Error('admin token is not configured. Set it via /admin -> admin_token');
    }
    throw new Error('client token is not configured. Re-add the user in /admin -> Управлять пользователями.');
  }

  const fullUrl = new URL(url, apiUrl).toString();
  const headers: Record<string, string> = {
    'Content-Type': 'application/json'
  };

  if (extraHeaders) {
    for (const [k, v] of Object.entries(extraHeaders)) {
      headers[k] = v;
    }
  }

  if (token) {
    headers['Authorization'] = `Bearer ${token}`;
  }

  const options: RequestInit = {
    method,
    headers
  };

  if (body) {
    options.body = JSON.stringify(body);
  }

  const response = await fetch(fullUrl, options);

  // 204 No Content is success for DELETE
  if (response.status === 204) {
    return null;
  }

  if (!response.ok) {
    let errorMsg = `HTTP ${response.status} ${response.statusText}`;
    try {
      const data = await response.json();
      if (typeof data === 'object' && data !== null && 'error' in data) {
        errorMsg += `: ${data.error}`;
      }
    } catch {
      // ignore parse error
    }
    throw new ApiClientError(response.status, response.statusText, errorMsg);
  }

  // Return null for 204
  if (response.status === 204) {
    return null;
  }

  try {
    return await response.json();
  } catch {
    return null;
  }
}

export const apiClient = {
  async getStatus(): Promise<ApiStatus> {
    const result = await fetchWithAuth('/api/status', 'GET', undefined, 'admin');
    return (result ?? {}) as ApiStatus;
  },

  async listRouters(): Promise<Router[]> {
    const result = await fetchWithAuth('/api/admin/routers', 'GET', undefined, 'admin');

    // API may return either an array of routers OR an envelope: { count, routers: [...] }
    if (Array.isArray(result)) return result as Router[];

    if (typeof result === 'object' && result !== null) {
      const maybeRouters = (result as any).routers;
      if (Array.isArray(maybeRouters)) return maybeRouters as Router[];
    }

    return [];
  },

  async getRouter(id: number): Promise<Router> {
    const routers = await this.listRouters();
    const router = routers.find((r) => r.id === id);
    if (!router) {
      throw new Error(`Router with id ${id} not found`);
    }
    return router;
  },

  async createRouter(name: string): Promise<Router> {
    const result = await fetchWithAuth('/api/admin/routers', 'POST', { name }, 'admin');
    return result as Router;
  },

  async deleteRouter(id: number): Promise<void> {
    await fetchWithAuth('/api/admin/routers', 'DELETE', { id }, 'admin');
  },

  async updateRouter(id: number, updates: { name?: string; blocked?: boolean }): Promise<Router> {
    const result = await fetchWithAuth('/api/admin/routers', 'PUT', { id, ...updates }, 'admin');
    return result as Router;
  },

  async createClient(name: string): Promise<Client> {
    const result = await fetchWithAuth('/api/admin/clients', 'POST', { name }, 'admin');
    return result as Client;
  },

  async listClients(): Promise<Client[]> {
    const result = await fetchWithAuth('/api/admin/clients', 'GET', undefined, 'admin');

    // API may return either an array of clients OR an envelope: { count, clients: [...] }
    if (Array.isArray(result)) return result as Client[];

    if (typeof result === 'object' && result !== null) {
      const maybeClients = (result as any).clients;
      if (Array.isArray(maybeClients)) return maybeClients as Client[];
    }

    return [];
  },

  async getClientByName(name: string): Promise<Client> {
    const clients = await this.listClients();
    const client = clients.find((c) => String(c.name) === String(name));
    if (!client) {
      throw new Error(`Client with name ${name} not found`);
    }
    return client;
  },

  async getClientById(id: number): Promise<Client> {
    const clients = await this.listClients();
    const client = clients.find((c) => Number(c.id) === Number(id));
    if (!client) {
      throw new Error(`Client with id ${id} not found`);
    }
    return client;
  },

  async deleteClient(id: number): Promise<void> {
    // Spec says body {id}; user asked to also pass ID in headers
    await fetchWithAuth('/api/admin/clients', 'DELETE', { id }, 'admin', { ID: String(id) });
  },

  async ping(params: {
    ip_pool: string;
    router_name?: string;
    check_ports?: string;
  }, clientToken?: string): Promise<unknown> {
    const query = new URLSearchParams();
    query.set('ip_pool', params.ip_pool);
    if (params.router_name) query.set('router_name', params.router_name);
    if (params.check_ports) query.set('check_ports', params.check_ports);

    const path = `/api/ping?${query.toString()}`;
    return fetchWithAuth(path, 'GET', undefined, 'client', undefined, clientToken ?? null);
  }
};
