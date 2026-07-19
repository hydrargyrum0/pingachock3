import fs from 'node:fs';
import path from 'node:path';
import Datastore from '@seald-io/nedb';

export type UserDoc = {
  telegram_id: number;
  has_access: boolean;
  created_at: string;
  token?: string;
};

export type SettingDoc = {
  key: string;
  value: string;
  updated_at: string;
};

export type HealthReportConfig = {
  targets: string[];
  router_value: string; // 'auto' | '__all__' | router name
  ports_value: string; // e.g. 'icmp' | '80' | '443' | 'icmp,22,80,443'
};

export type RemnawaveConfig = {
  url?: string;
  api_token?: string;
  ignore_list?: string[];
  router_value?: string; // 'auto' | '__all__' | router name
  ports_value?: string; // same values as pingPortsOptions
};

export type VultrConfig = {
  api_token?: string;
  tag?: string;
  router_value?: string; // 'auto' | '__all__' | router name
  ports_value?: string; // same values as pingPortsOptions
};

export type PeriodicHealthConfig = {
  active: boolean;
  chat_id: string; // Telegram chat id (user or group)
  interval_sec: number;
};

function ensureDirForFile(filePath: string) {
  const dir = path.dirname(filePath);
  fs.mkdirSync(dir, { recursive: true });
}

const dbPath = process.env.DB_PATH ?? './data/users.db';
ensureDirForFile(dbPath);

const usersDb = new Datastore<UserDoc>({
  filename: dbPath,
  autoload: true
});

usersDb.ensureIndex({ fieldName: 'telegram_id', unique: true });
usersDb.ensureIndex({ fieldName: 'has_access' });

const settingsPath = process.env.SETTINGS_DB_PATH ?? './data/settings.db';
ensureDirForFile(settingsPath);

const settingsDb = new Datastore<SettingDoc>({
  filename: settingsPath,
  autoload: true
});

settingsDb.ensureIndex({ fieldName: 'key', unique: true });

function p<T>(fn: (cb: (err: Error | null, result?: T) => void) => void): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    fn((err, result) => {
      if (err) reject(err);
      else resolve(result as T);
    });
  });
}

function pVoid(fn: (cb: (err: Error | null) => void) => void): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    fn((err) => {
      if (err) reject(err);
      else resolve();
    });
  });
}

function pUpdate(
  fn: (cb: (err: Error | null, numberOfUpdated: number, affectedDocuments: unknown, upsert: boolean | null) => void) => void
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    fn((err) => {
      if (err) reject(err);
      else resolve();
    });
  });
}

function pRemove(fn: (cb: (err: Error | null, numRemoved?: number) => void) => void): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    fn((err) => {
      if (err) reject(err);
      else resolve();
    });
  });
}

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

  async addUser(telegramId: number, token?: string): Promise<void> {
    const created_at = new Date().toISOString();
    await pUpdate((cb) =>
      usersDb.update(
        { telegram_id: telegramId },
        { $set: { telegram_id: telegramId, has_access: true, created_at, ...(token ? { token } : {}) } },
        { upsert: true },
        cb
      )
    );
  },

  async setToken(telegramId: number, token: string): Promise<void> {
    await pUpdate((cb) =>
      usersDb.update(
        { telegram_id: telegramId },
        { $set: { token } },
        { upsert: false },
        cb
      )
    );
  },

  async getToken(telegramId: number): Promise<string | null> {
    const doc = await p<UserDoc | null>((cb) => usersDb.findOne({ telegram_id: telegramId }, cb));
    return doc?.token ?? null;
  },

  async deleteUser(telegramId: number): Promise<void> {
    await pRemove((cb) =>
      usersDb.remove({ telegram_id: telegramId }, { multi: false }, cb)
    );
  }
};

export const settingsRepo = {
  async getApiUrl(): Promise<string | null> {
    const doc = await p<SettingDoc | null>((cb) => settingsDb.findOne({ key: 'api_url' }, cb));
    return doc?.value ?? null;
  },

  async setApiUrl(url: string): Promise<void> {
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: 'api_url' },
        { $set: { key: 'api_url', value: url, updated_at } },
        { upsert: true },
        cb
      )
    );
  },

  async getAdminToken(): Promise<string | null> {
    const doc = await p<SettingDoc | null>((cb) => settingsDb.findOne({ key: 'admin_token' }, cb));
    return doc?.value ?? null;
  },

  async setAdminToken(token: string): Promise<void> {
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: 'admin_token' },
        { $set: { key: 'admin_token', value: token, updated_at } },
        { upsert: true },
        cb
      )
    );
  },

  async getClientToken(): Promise<string | null> {
    const doc = await p<SettingDoc | null>((cb) => settingsDb.findOne({ key: 'client_token' }, cb));
    return doc?.value ?? null;
  },

  async setClientToken(token: string): Promise<void> {
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: 'client_token' },
        { $set: { key: 'client_token', value: token, updated_at } },
        { upsert: true },
        cb
      )
    );
  }

  ,

  async getHealthReportConfig(telegramId: number): Promise<HealthReportConfig | null> {
    const doc = await p<SettingDoc | null>((cb) =>
      settingsDb.findOne({ key: `health_report:${telegramId}` }, cb)
    );
    if (!doc?.value) return null;
    try {
      const parsed = JSON.parse(doc.value) as Partial<HealthReportConfig>;
      if (!parsed || !Array.isArray(parsed.targets)) return null;

      return {
        targets: parsed.targets.map((t) => String(t)),
        router_value: parsed.router_value ? String(parsed.router_value) : 'auto',
        ports_value: parsed.ports_value ? String(parsed.ports_value) : 'icmp'
      };
    } catch {
      return null;
    }
  },

  async setHealthReportConfig(telegramId: number, config: HealthReportConfig): Promise<void> {
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `health_report:${telegramId}` },
        {
          $set: {
            key: `health_report:${telegramId}`,
            value: JSON.stringify(config),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  }

  ,

  async getRemnawaveConfig(telegramId: number): Promise<RemnawaveConfig> {
    const doc = await p<SettingDoc | null>((cb) =>
      settingsDb.findOne({ key: `remnawave:${telegramId}` }, cb)
    );
    if (!doc?.value) return {};
    try {
      const parsed = JSON.parse(doc.value) as Partial<RemnawaveConfig>;
      return {
        url: parsed.url ? String(parsed.url) : undefined,
        api_token: parsed.api_token ? String(parsed.api_token) : undefined,
        ignore_list: Array.isArray(parsed.ignore_list)
          ? parsed.ignore_list.map((x) => String(x))
          : undefined,
        router_value: parsed.router_value ? String(parsed.router_value) : undefined,
        ports_value: parsed.ports_value ? String(parsed.ports_value) : undefined
      };
    } catch {
      return {};
    }
  },

  async setRemnawaveUrl(telegramId: number, url: string): Promise<void> {
    const current = await this.getRemnawaveConfig(telegramId);
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `remnawave:${telegramId}` },
        {
          $set: {
            key: `remnawave:${telegramId}`,
            value: JSON.stringify({ ...current, url }),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  },

  async setRemnawaveApiToken(telegramId: number, apiToken: string): Promise<void> {
    const current = await this.getRemnawaveConfig(telegramId);
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `remnawave:${telegramId}` },
        {
          $set: {
            key: `remnawave:${telegramId}`,
            value: JSON.stringify({ ...current, api_token: apiToken }),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  }

  ,

  async setRemnawaveIgnoreList(telegramId: number, ignoreList: string[]): Promise<void> {
    const current = await this.getRemnawaveConfig(telegramId);
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `remnawave:${telegramId}` },
        {
          $set: {
            key: `remnawave:${telegramId}`,
            value: JSON.stringify({ ...current, ignore_list: ignoreList }),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  },

  async setRemnawaveSettings(
    telegramId: number,
    settings: { router_value: string; ports_value: string }
  ): Promise<void> {
    const current = await this.getRemnawaveConfig(telegramId);
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `remnawave:${telegramId}` },
        {
          $set: {
            key: `remnawave:${telegramId}`,
            value: JSON.stringify({ ...current, ...settings }),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  }

  ,

  async getVultrConfig(telegramId: number): Promise<VultrConfig> {
    const doc = await p<SettingDoc | null>((cb) => settingsDb.findOne({ key: `vultr:${telegramId}` }, cb));
    if (!doc?.value) return {};
    try {
      const parsed = JSON.parse(doc.value) as Partial<VultrConfig>;
      return {
        api_token: parsed.api_token ? String(parsed.api_token) : undefined,
        tag: parsed.tag ? String(parsed.tag) : undefined,
        router_value: parsed.router_value ? String(parsed.router_value) : undefined,
        ports_value: parsed.ports_value ? String(parsed.ports_value) : undefined
      };
    } catch {
      return {};
    }
  },

  async setVultrApiToken(telegramId: number, apiToken: string): Promise<void> {
    const current = await this.getVultrConfig(telegramId);
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `vultr:${telegramId}` },
        {
          $set: {
            key: `vultr:${telegramId}`,
            value: JSON.stringify({ ...current, api_token: apiToken }),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  },

  async setVultrTag(telegramId: number, tag: string): Promise<void> {
    const current = await this.getVultrConfig(telegramId);
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `vultr:${telegramId}` },
        {
          $set: {
            key: `vultr:${telegramId}`,
            value: JSON.stringify({ ...current, tag }),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  },

  async setVultrSettings(telegramId: number, settings: { router_value: string; ports_value: string }): Promise<void> {
    const current = await this.getVultrConfig(telegramId);
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `vultr:${telegramId}` },
        {
          $set: {
            key: `vultr:${telegramId}`,
            value: JSON.stringify({ ...current, ...settings }),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  }

  ,

  async getPeriodicHealthConfig(telegramId: number): Promise<PeriodicHealthConfig> {
    const doc = await p<SettingDoc | null>((cb) =>
      settingsDb.findOne({ key: `health_periodic:${telegramId}` }, cb)
    );

    const defaults: PeriodicHealthConfig = {
      active: false,
      chat_id: String(telegramId),
      interval_sec: 300
    };

    if (!doc?.value) return defaults;

    try {
      const parsed = JSON.parse(doc.value) as Partial<PeriodicHealthConfig>;
      const active = Boolean(parsed.active);
      const chat_id = parsed.chat_id != null ? String(parsed.chat_id) : defaults.chat_id;
      const interval_sec =
        typeof (parsed as any).interval_sec === 'number' && Number.isFinite((parsed as any).interval_sec)
          ? Math.floor((parsed as any).interval_sec)
          : defaults.interval_sec;

      return {
        active,
        chat_id,
        interval_sec
      };
    } catch {
      return defaults;
    }
  },

  async setPeriodicHealthConfig(telegramId: number, config: PeriodicHealthConfig): Promise<void> {
    const updated_at = new Date().toISOString();
    await pUpdate((cb) =>
      settingsDb.update(
        { key: `health_periodic:${telegramId}` },
        {
          $set: {
            key: `health_periodic:${telegramId}`,
            value: JSON.stringify(config),
            updated_at
          }
        },
        { upsert: true },
        cb
      )
    );
  },

  async setPeriodicHealthActive(telegramId: number, active: boolean): Promise<void> {
    const current = await this.getPeriodicHealthConfig(telegramId);
    await this.setPeriodicHealthConfig(telegramId, { ...current, active });
  },

  async setPeriodicHealthChatId(telegramId: number, chatId: string): Promise<void> {
    const current = await this.getPeriodicHealthConfig(telegramId);
    await this.setPeriodicHealthConfig(telegramId, { ...current, chat_id: chatId });
  },

  async setPeriodicHealthInterval(telegramId: number, intervalSec: number): Promise<void> {
    const current = await this.getPeriodicHealthConfig(telegramId);
    await this.setPeriodicHealthConfig(telegramId, { ...current, interval_sec: intervalSec });
  },

  async listPeriodicHealthConfigs(): Promise<Array<{ telegramId: number; config: PeriodicHealthConfig }>> {
    const docs = await p<SettingDoc[]>((cb) =>
      settingsDb
        .find({ key: /^health_periodic:\d+$/ })
        .sort({ key: 1 })
        .exec(cb)
    );

    const out: Array<{ telegramId: number; config: PeriodicHealthConfig }> = [];
    for (const d of docs) {
      const m = d.key.match(/^health_periodic:(\d+)$/);
      if (!m) continue;
      const telegramId = Number(m[1]);
      if (!Number.isFinite(telegramId)) continue;

      try {
        const parsed = JSON.parse(d.value) as Partial<PeriodicHealthConfig>;
        const cfg: PeriodicHealthConfig = {
          active: Boolean(parsed.active),
          chat_id: parsed.chat_id != null ? String(parsed.chat_id) : String(telegramId),
          interval_sec:
            typeof (parsed as any).interval_sec === 'number' && Number.isFinite((parsed as any).interval_sec)
              ? Math.floor((parsed as any).interval_sec)
              : 300
        };
        out.push({ telegramId, config: cfg });
      } catch {
        // ignore invalid JSON
      }
    }

    return out;
  }
};
