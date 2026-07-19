import 'dotenv/config';
import { Markup, Telegraf, session, type Context } from 'telegraf';
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

  awaitingPingInput?: boolean;
  pingRouterIndex?: number;
  pingPortsIndex?: number;
  pingRouters?: Router[];

  awaitingHealthListInput?: boolean;
  healthRouterIndex?: number;
  healthPortsIndex?: number;
  healthRouters?: Router[];

  awaitingRemnaUrl?: boolean;
  awaitingRemnaToken?: boolean;
  awaitingRemnaIgnoreList?: boolean;

  awaitingVultrToken?: boolean;
  awaitingVultrTag?: boolean;

  awaitingHealthPeriodicChatId?: boolean;
  awaitingHealthPeriodicInterval?: boolean;

  remnaRouterIndex?: number;
  remnaPortsIndex?: number;
  remnaRouters?: Router[];

  vultrRouterIndex?: number;
  vultrPortsIndex?: number;
  vultrRouters?: Router[];
};

type MyContext = Context & {
  session: MySession;
};

const BOT_TOKEN = process.env.telegram_bot_token ?? process.env.BOT_TOKEN;
const ADMIN_CHAT_IDS_RAW =
  process.env.telegram_bot_admin_id ??
  process.env.ADMIN_CHAT_ID ??
  process.env.telegram_bot_admin_ids ??
  process.env.ADMIN_CHAT_IDS;

if (!BOT_TOKEN) {
  throw new Error('Missing env var: telegram_bot_token (or BOT_TOKEN)');
}
if (!ADMIN_CHAT_IDS_RAW) {
  throw new Error(
    'Missing env var: telegram_bot_admin_id (or ADMIN_CHAT_ID). You can provide multiple ids comma-separated.'
  );
}

function parseAdminIds(raw: string): Set<string> {
  const parts = raw
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
  return new Set(parts.map(String));
}

const ADMIN_IDS = parseAdminIds(ADMIN_CHAT_IDS_RAW);

const bot = new Telegraf<MyContext>(BOT_TOKEN);

const TELEGRAM_SAFE_TEXT_LIMIT = 3072;

bot.use(
  session({
    defaultSession: (): MySession => ({})
  })
);

function isAdmin(ctx: MyContext): boolean {
  const chatId = ctx.chat?.id;
  if (chatId == null) return false;
  return ADMIN_IDS.has(String(chatId));
}

async function isAuthorizedUser(ctx: MyContext): Promise<boolean> {
  // Allow main admin to use user flows even if not added to the DB
  if (ctx.from?.id != null && ADMIN_IDS.has(String(ctx.from.id))) return true;

  const telegramId = ctx.from?.id;
  if (telegramId == null) return false;
  return userRepo.isAuthorized(telegramId);
}

function adminRootKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('Управлять пользователями', 'admin:users')],
    [Markup.button.callback('Рассылка авторизованным', 'admin:broadcast')],
    [Markup.button.callback('API URL', 'admin:api_url')],
    [Markup.button.callback('admin_token', 'admin:admin_token')],
    [Markup.button.callback('Роутеры', 'admin:routers')]
  ]);
}

function adminCancelToRootKeyboard() {
  return Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:root')]]);
}

function adminBroadcastConfirmKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('✅ Отправить', 'admin:broadcast_send')],
    [Markup.button.callback('✏️ Изменить', 'admin:broadcast_edit')],
    [Markup.button.callback('◀️ Назад', 'admin:root')]
  ]);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function mainMenuKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🔍 Ping', 'menu:ping')],
    [Markup.button.callback('📊 Health report', 'menu:health')]
  ]);
}

function healthMenuKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🔶 Принудительный Health Report', 'health:force')],
    [Markup.button.callback('🔷 Принудительный Remnawave Report', 'health:remna_force')],
    [Markup.button.callback('🔽 Принудительный Vultr Report', 'health:vultr_force')],
    [Markup.button.callback('⏰ Периодические проверки', 'health:periodic')],
    [Markup.button.callback('🟧 Кастомный список', 'health:custom')],
    [Markup.button.callback('🟦 Интеграция Remnawave', 'health:remna')],
    [Markup.button.callback('⏹️ Интеграция Vultr', 'health:vultr')],
    [Markup.button.callback('◀️ Назад', 'menu:root')]
  ]);
}

function isTelegramChatId(value: string): boolean {
  return /^-?\d+$/.test(value.trim());
}

function healthPeriodicKeyboard(active: boolean) {
  const toggleLabel = active ? 'Активен🟢' : 'Деактивирован🔴';
  return Markup.inlineKeyboard([
    [Markup.button.callback(toggleLabel, 'healthp:toggle')],
    [Markup.button.callback('ChatID', 'healthp:chat')],
    [Markup.button.callback('Периодичность', 'healthp:interval')],
    [Markup.button.callback('◀️ Назад', 'menu:health')]
  ]);
}

async function showHealthPeriodicMenu(ctx: MyContext) {
  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrIgnore(ctx, '📊 Меню Health Report:', healthMenuKeyboard());
    return;
  }

  const cfg = await settingsRepo.getPeriodicHealthConfig(telegramId);
  const chatId = cfg.chat_id ? cfg.chat_id : '—';
  const interval = Number.isFinite(cfg.interval_sec) ? String(cfg.interval_sec) : '—';

  const text =
    `Настройка периодических Health Report\n` +
    `Чат для уведомлений: ${chatId}\n` +
    `Периодичность: ${interval} секунд`;

  await safeEditOrIgnore(ctx, text, healthPeriodicKeyboard(cfg.active));
}

async function fetchRemnawaveHosts(params: {
  baseUrl: string;
  apiToken: string;
}): Promise<Array<{ address: string; host: string | null; tag: string | null; isDisabled: boolean; isHidden: boolean }>> {
  const url = new URL('/api/hosts', params.baseUrl).toString();
  const response = await fetch(url, {
    method: 'GET',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${params.apiToken}`
    }
  });

  if (!response.ok) {
    let extra = '';
    try {
      const data = await response.json();
      if (data && typeof data === 'object') {
        const msg = (data as any).message;
        if (typeof msg === 'string' && msg) extra = `: ${msg}`;
      }
    } catch {
      // ignore
    }
    throw new Error(`Remnawave HTTP ${response.status} ${response.statusText}${extra}`);
  }

  const data: any = await response.json();
  const arr = Array.isArray(data?.response) ? data.response : Array.isArray(data) ? data : [];

  const out: Array<{ address: string; host: string | null; tag: string | null; isDisabled: boolean; isHidden: boolean }> = [];
  for (const item of arr) {
    if (!item || typeof item !== 'object') continue;
    const address = typeof (item as any).address === 'string' ? (item as any).address : '';
    const host = typeof (item as any).host === 'string' ? (item as any).host : null;
    const tag = typeof (item as any).tag === 'string' ? (item as any).tag : null;
    const isDisabled = Boolean((item as any).isDisabled);
    const isHidden = Boolean((item as any).isHidden);
    out.push({ address, host, tag, isDisabled, isHidden });
  }
  return out;
}

async function fetchVultrInstances(params: {
  apiToken: string;
}): Promise<Array<{ id: string; label: string; main_ip: string }>> {
  const out: Array<{ id: string; label: string; main_ip: string }> = [];
  const seen = new Set<string>();

  let cursor: string | null = null;
  for (let page = 0; page < 20; page++) {
    const url = new URL('https://api.vultr.com/v2/instances');
    url.searchParams.set('per_page', '500');
    if (cursor) url.searchParams.set('cursor', cursor);

    const response = await fetch(url.toString(), {
      method: 'GET',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${params.apiToken}`
      }
    });

    if (!response.ok) {
      let extra = '';
      try {
        const data = await response.json();
        const msg = (data as any)?.error;
        if (typeof msg === 'string' && msg) extra = `: ${msg}`;
      } catch {
        // ignore
      }
      throw new Error(`Vultr HTTP ${response.status} ${response.statusText}${extra}`);
    }

    const data: any = await response.json();
    const instances: any[] = Array.isArray(data?.instances) ? data.instances : [];
    for (const it of instances) {
      if (!it || typeof it !== 'object') continue;
      const id = typeof (it as any).id === 'string' ? (it as any).id : '';
      const label = typeof (it as any).label === 'string' ? (it as any).label : '';
      const main_ip = typeof (it as any).main_ip === 'string' ? (it as any).main_ip : '';
      if (!id || seen.has(id)) continue;
      seen.add(id);
      out.push({ id, label, main_ip });
    }

    const next = (data as any)?.meta?.links?.next;
    if (typeof next === 'string' && next.trim()) {
      cursor = next.trim();
      continue;
    }
    break;
  }

  return out;
}

function isValidVultrTag(value: string): boolean {
  const v = value.trim();
  if (!v) return false;
  if (v.length > 64) return false;
  return !/\s/.test(v);
}

function filterVultrInstancesByTag(params: {
  instances: Array<{ id: string; label: string; main_ip: string }>;
  tag: string;
}): Array<{ id: string; label: string; main_ip: string }> {
  const tag = params.tag.trim();
  const suffix = `-${tag}`.toLowerCase();
  return params.instances.filter((i) => {
    const label = (i.label ?? '').trim();
    if (!label) return false;
    return label.toLowerCase().endsWith(suffix);
  });
}

function extractRemnawaveTargets(params: {
  hosts: Array<{ address: string; host: string | null; tag: string | null }>;
  ignoreSet: Set<string>;
}): { addressTargets: string[]; hostTargets: string[] } {
  const seenAddressPairs = new Set<string>();
  const seenHostPairs = new Set<string>();

  const addressSeen = new Set<string>();
  const hostSeen = new Set<string>();

  const addressTargets: string[] = [];
  const hostTargets: string[] = [];

  for (const h of params.hosts) {
    const tagKey = (h.tag ?? '').trim().toLowerCase();

    const addressRaw = (h.address ?? '').trim();
    if (addressRaw) {
      const addressKey = addressRaw.toLowerCase();
      const pairKey = `${addressKey}||${tagKey}`;
      if (!seenAddressPairs.has(pairKey)) {
        seenAddressPairs.add(pairKey);

        if (!params.ignoreSet.has(addressKey) && (isIpv4(addressRaw) || isDomain(addressRaw))) {
          if (!addressSeen.has(addressKey)) {
            addressSeen.add(addressKey);
            addressTargets.push(addressRaw);
          }
        }
      }
    }

    const hostRaw = (h.host ?? '').trim();
    if (hostRaw) {
      const hostKey = hostRaw.toLowerCase();
      const pairKey = `${hostKey}||${tagKey}`;
      if (!seenHostPairs.has(pairKey)) {
        seenHostPairs.add(pairKey);

        if (!params.ignoreSet.has(hostKey) && (isIpv4(hostRaw) || isDomain(hostRaw))) {
          if (!hostSeen.has(hostKey)) {
            hostSeen.add(hostKey);
            hostTargets.push(hostRaw);
          }
        }
      }
    }
  }

  return { addressTargets, hostTargets };
}

function remnaMenuKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('URL', 'remna:url')],
    [Markup.button.callback('API токен', 'remna:token')],
    [Markup.button.callback('🚫 Список игнорирования', 'remna:ignore')],
    [Markup.button.callback('⚙️ Настройки', 'remna:settings')],
    [Markup.button.callback('◀️ Назад', 'menu:health')]
  ]);
}

function getStatusEmoji(status: string): string {
  return status === 'online' ? '🟢' : '🔴';
}

function getRemnaRouterOption(session: MySession): { label: string; value: string } {
  const routers = session.remnaRouters ?? [];
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    { label: pingRouterLabels.all, value: '__all__' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];

  const index = session.remnaRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}

function getRemnaPortsOption(session: MySession): { label: string; value: string } {
  const index = session.remnaPortsIndex ?? 0;
  return pingPortsOptions[Math.max(0, Math.min(index, pingPortsOptions.length - 1))];
}

function remnaSettingsKeyboard(session: MySession) {
  const routerOpt = getRemnaRouterOption(session);
  const portsOpt = getRemnaPortsOption(session);
  return Markup.inlineKeyboard([
    [Markup.button.callback(routerOpt.label, 'remna:toggle_router')],
    [Markup.button.callback(portsOpt.label, 'remna:toggle_ports')],
    [Markup.button.callback('◀️ Назад', 'remna:settings:back')]
  ]);
}

function vultrMenuKeyboard() {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🔐 API Токен', 'vultr:token')],
    [Markup.button.callback('⚙️ Настройки', 'vultr:settings')],
    [Markup.button.callback('📌Тег', 'vultr:tag')],
    [Markup.button.callback('◀️ Назад', 'menu:health')]
  ]);
}

function getVultrRouterOption(session: MySession): { label: string; value: string } {
  const routers = session.vultrRouters ?? [];
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    { label: pingRouterLabels.all, value: '__all__' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];

  const index = session.vultrRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}

function getVultrPortsOption(session: MySession): { label: string; value: string } {
  const index = session.vultrPortsIndex ?? 0;
  return pingPortsOptions[Math.max(0, Math.min(index, pingPortsOptions.length - 1))];
}

function vultrSettingsKeyboard(session: MySession) {
  const routerOpt = getVultrRouterOption(session);
  const portsOpt = getVultrPortsOption(session);
  return Markup.inlineKeyboard([
    [Markup.button.callback(routerOpt.label, 'vultr:toggle_router')],
    [Markup.button.callback(portsOpt.label, 'vultr:toggle_ports')],
    [Markup.button.callback('◀️ Назад', 'vultr:settings:back')]
  ]);
}

async function showVultrMenu(ctx: MyContext) {
  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrIgnore(ctx, '⏹️ Меню Интеграции Vultr', vultrMenuKeyboard());
    return;
  }

  const cfg = await settingsRepo.getVultrConfig(telegramId);
  const tokenLine = cfg.api_token ? 'API токен: установлен' : 'API токен: —';
  const tagLine = cfg.tag ? `Тег: ${cfg.tag} (label заканчивается на -${cfg.tag})` : 'Тег: —';

  const text = `⏹️ Меню Интеграции Vultr\n\n${tokenLine}\n${tagLine}`;
  await safeEditOrIgnore(ctx, text, vultrMenuKeyboard());
}

async function showVultrSettingsEditor(ctx: MyContext) {
  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrIgnore(ctx, '⏹️ Меню Интеграции Vultr', vultrMenuKeyboard());
    return;
  }

  const cfg = await settingsRepo.getVultrConfig(telegramId);

  if (!ctx.session.vultrRouters) {
    try {
      const allRouters = await apiClient.listRouters();
      ctx.session.vultrRouters = allRouters.filter((r) => r.status === 'online');
    } catch {
      ctx.session.vultrRouters = [];
    }
  }

  if (ctx.session.vultrRouterIndex == null) {
    const routerNames = ['auto', '__all__', ...(ctx.session.vultrRouters?.map((r) => r.name) ?? [])];
    const idx = routerNames.findIndex((v) => String(v) === String(cfg.router_value ?? 'auto'));
    ctx.session.vultrRouterIndex = idx >= 0 ? idx : 0;
  }

  if (ctx.session.vultrPortsIndex == null) {
    const idx = pingPortsOptions.findIndex((p) => String(p.value) === String(cfg.ports_value ?? 'icmp'));
    ctx.session.vultrPortsIndex = idx >= 0 ? idx : 0;
  }

  const routerOpt = getVultrRouterOption(ctx.session);
  const portsOpt = getVultrPortsOption(ctx.session);

  const text =
    `⏹️ Меню Интеграции Vultr\n\n` +
    `⚙️ Настройки\n` +
    `Роутер: ${routerOpt.label}\n` +
    `Порты: ${portsOpt.label}`;

  await safeEditOrIgnore(ctx, text, vultrSettingsKeyboard(ctx.session));
}

function parseIgnoreList(input: string): string[] | null {
  const tokens = input
    .split(/\r?\n/)
    .flatMap((line) => line.split(/[\s,]+/))
    .map((t) => t.trim())
    .filter(Boolean);

  if (!tokens.length) return null;

  const out: string[] = [];
  const seen = new Set<string>();
  for (const t of tokens) {
    if (!isIpv4(t) && !isDomain(t)) return null;
    if (seen.has(t)) continue;
    seen.add(t);
    out.push(t);
  }
  return out;
}

async function showRemnaMenu(ctx: MyContext) {
  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrIgnore(ctx, '🟦 Меню Интеграции Remnawave', remnaMenuKeyboard());
    return;
  }

  const cfg = await settingsRepo.getRemnawaveConfig(telegramId);
  const urlLine = cfg.url ? `URL: ${cfg.url}` : 'URL: —';
  const tokenLine = cfg.api_token ? 'API токен: установлен' : 'API токен: —';

  const text = `🟦 Меню Интеграции Remnawave\n\n${urlLine}\n${tokenLine}`;
  await safeEditOrIgnore(ctx, text, remnaMenuKeyboard());
}

async function showRemnaIgnoreEditor(ctx: MyContext) {
  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrIgnore(ctx, '🟦 Меню Интеграции Remnawave', remnaMenuKeyboard());
    return;
  }

  const cfg = await settingsRepo.getRemnawaveConfig(telegramId);
  const list = cfg.ignore_list ?? [];

  ctx.session.awaitingRemnaIgnoreList = true;
  ctx.session.awaitingRemnaUrl = false;
  ctx.session.awaitingRemnaToken = false;

  const text =
    `🟦 Меню Интеграции Remnawave\n\n` +
    `🚫 Текущий список игнорирования (${list.length}):\n` +
    `${formatTargetsPreview(list)}\n\n` +
    `Отправьте новый список (IP/домены, каждый с новой строки).`;

  await safeEditOrIgnore(ctx, text, Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'health:remna')]]));
}

async function showRemnaSettingsEditor(ctx: MyContext) {
  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrIgnore(ctx, '🟦 Меню Интеграции Remnawave', remnaMenuKeyboard());
    return;
  }

  const cfg = await settingsRepo.getRemnawaveConfig(telegramId);

  if (!ctx.session.remnaRouters) {
    try {
      const allRouters = await apiClient.listRouters();
      ctx.session.remnaRouters = allRouters.filter((r) => r.status === 'online');
    } catch {
      ctx.session.remnaRouters = [];
    }
  }

  if (ctx.session.remnaRouterIndex == null) {
    const routerNames = ['auto', '__all__', ...(ctx.session.remnaRouters?.map((r) => r.name) ?? [])];
    const idx = routerNames.findIndex((v) => String(v) === String(cfg.router_value ?? 'auto'));
    ctx.session.remnaRouterIndex = idx >= 0 ? idx : 0;
  }

  if (ctx.session.remnaPortsIndex == null) {
    const idx = pingPortsOptions.findIndex((p) => String(p.value) === String(cfg.ports_value ?? 'icmp'));
    ctx.session.remnaPortsIndex = idx >= 0 ? idx : 0;
  }

  const routerOpt = getRemnaRouterOption(ctx.session);
  const portsOpt = getRemnaPortsOption(ctx.session);

  const text =
    `🟦 Меню Интеграции Remnawave\n\n` +
    `⚙️ Настройки\n` +
    `Роутер: ${routerOpt.label}\n` +
    `Порты: ${portsOpt.label}`;

  await safeEditOrIgnore(ctx, text, remnaSettingsKeyboard(ctx.session));
}

function getHealthRouterOption(session: MySession): { label: string; value: string } {
  const routers = session.healthRouters ?? [];
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    { label: pingRouterLabels.all, value: '__all__' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];

  const index = session.healthRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}

function getHealthPortsOption(session: MySession): { label: string; value: string } {
  const index = session.healthPortsIndex ?? 0;
  return pingPortsOptions[Math.max(0, Math.min(index, pingPortsOptions.length - 1))];
}

function healthCustomKeyboard(session: MySession) {
  const routerOpt = getHealthRouterOption(session);
  const portsOpt = getHealthPortsOption(session);
  return Markup.inlineKeyboard([
    [Markup.button.callback(routerOpt.label, 'health:toggle_router')],
    [Markup.button.callback(portsOpt.label, 'health:toggle_ports')],
    [Markup.button.callback('◀️ Назад', 'health:cancel')]
  ]);
}

function formatTargetsPreview(targets: string[], maxLines: number = 30): string {
  if (!targets.length) return '— пусто —';
  const head = targets.slice(0, maxLines);
  const rest = targets.length - head.length;
  const suffix = rest > 0 ? `\n… и ещё ${rest}` : '';
  return `${head.join('\n')}${suffix}`;
}

async function showHealthMenu(ctx: MyContext) {
  await safeEditOrIgnore(ctx, '📊 Меню Health Report:', healthMenuKeyboard());
}

async function showHealthCustomListEditor(ctx: MyContext) {
  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrIgnore(ctx, 'Ошибка: не удалось определить пользователя.', healthMenuKeyboard());
    return;
  }

  const cfg = (await settingsRepo.getHealthReportConfig(telegramId)) ?? {
    targets: [],
    router_value: 'auto',
    ports_value: 'icmp'
  };

  // Load routers for the selector (only when missing, filter for online only)
  if (!ctx.session.healthRouters) {
    try {
      const allRouters = await apiClient.listRouters();
      ctx.session.healthRouters = allRouters.filter((r) => r.status === 'online');
    } catch {
      ctx.session.healthRouters = [];
    }
  }

  // Align indices with saved values only on first open (so toggles keep working)
  if (ctx.session.healthRouterIndex == null) {
    const routerNames = ['auto', '__all__', ...(ctx.session.healthRouters?.map((r) => r.name) ?? [])];
    const idx = routerNames.findIndex((v) => String(v) === String(cfg.router_value));
    ctx.session.healthRouterIndex = idx >= 0 ? idx : 0;
  }

  if (ctx.session.healthPortsIndex == null) {
    const idx = pingPortsOptions.findIndex((p) => String(p.value) === String(cfg.ports_value));
    ctx.session.healthPortsIndex = idx >= 0 ? idx : 0;
  }

  ctx.session.awaitingHealthListInput = true;

  const routerOpt = getHealthRouterOption(ctx.session);
  const portsOpt = getHealthPortsOption(ctx.session);
  const text =
    `📊 Меню Health Report:\n\n` +
    `Текущий кастомный список (${cfg.targets.length}):\n` +
    `${formatTargetsPreview(cfg.targets)}\n\n` +
    `Роутер: ${routerOpt.label}\n` +
    `Порты: ${portsOpt.label}\n\n` +
    `Отправьте новый список (каждый объект с новой строки).`;

  await safeEditOrIgnore(ctx, text, healthCustomKeyboard(ctx.session));
}

async function getRoutersOnlineCount(): Promise<number | null> {
  try {
    const status = await apiClient.getStatus();
    const n = (status as any).routers_connected ?? (status as any).routers_online;
    return typeof n === 'number' && Number.isFinite(n) ? n : null;
  } catch {
    return null;
  }
}

async function renderMainMenuText(): Promise<string> {
  const routersConnected = await getRoutersOnlineCount();
  const marker = routersConnected != null && routersConnected > 1 ? '✅' : '‼️';
  const value = routersConnected != null ? String(routersConnected) : '—';

  return `🏠 Главное меню\n💻 Роутеров онлайн: ${value} ${marker}\n\nВыберите действие:`;
}

const pingRouterLabels = {
  auto: 'Auto',
  all: 'ALL'
};

const pingPortsOptions: Array<{ label: string; value: string }> = [
  { label: 'ICMP', value: 'icmp' },
  { label: '80', value: '80' },
  { label: '443', value: '443' },
  { label: 'ICMP,22,80,443', value: 'icmp,22,80,443' },
  { label: 'ICMP,22,80,443,8000,8080', value: 'icmp,22,80,443,8000,8080' }
];

function getPingRouterOption(session: MySession): { label: string; value: string } {
  const routers = session.pingRouters ?? [];
  const options: Array<{ label: string; value: string }> = [
    { label: pingRouterLabels.auto, value: 'auto' },
    { label: pingRouterLabels.all, value: '__all__' },
    ...routers.map((r) => ({ label: r.name, value: r.name }))
  ];

  const index = session.pingRouterIndex ?? 0;
  return options[Math.max(0, Math.min(index, options.length - 1))];
}

function getPingPortsOption(session: MySession): { label: string; value: string } {
  const index = session.pingPortsIndex ?? 0;
  return pingPortsOptions[Math.max(0, Math.min(index, pingPortsOptions.length - 1))];
}

function pingKeyboard(session: MySession) {
  const routerOpt = getPingRouterOption(session);
  const portsOpt = getPingPortsOption(session);

  return Markup.inlineKeyboard([
    [Markup.button.callback(routerOpt.label, 'ping:toggle_router')],
    [Markup.button.callback(portsOpt.label, 'ping:toggle_ports')],
    [Markup.button.callback('◀️ Назад', 'menu:root')]
  ]);
}

function isIpv4OrCidr(value: string): boolean {
  const m = value.match(
    /^(?<a>25[0-5]|2[0-4]\d|1?\d?\d)\.(?<b>25[0-5]|2[0-4]\d|1?\d?\d)\.(?<c>25[0-5]|2[0-4]\d|1?\d?\d)\.(?<d>25[0-5]|2[0-4]\d|1?\d?\d)(?:\/(?<mask>3[0-2]|[12]?\d))?$/
  );
  return Boolean(m);
}

function isIpv4(value: string): boolean {
  return /^(25[0-5]|2[0-4]\d|1?\d?\d)\.(25[0-5]|2[0-4]\d|1?\d?\d)\.(25[0-5]|2[0-4]\d|1?\d?\d)\.(25[0-5]|2[0-4]\d|1?\d?\d)$/.test(
    value
  );
}

function ipv4ToInt(ip: string): number {
  const parts = ip.split('.').map((x) => Number(x));
  if (parts.length !== 4 || parts.some((p) => !Number.isInteger(p) || p < 0 || p > 255)) {
    throw new Error(`Invalid IPv4: ${ip}`);
  }
  // Use unsigned arithmetic
  return (((parts[0] << 24) | (parts[1] << 16) | (parts[2] << 8) | parts[3]) >>> 0) as number;
}

function intToIpv4(n: number): string {
  const a = (n >>> 24) & 255;
  const b = (n >>> 16) & 255;
  const c = (n >>> 8) & 255;
  const d = n & 255;
  return `${a}.${b}.${c}.${d}`;
}

function isIpv4Range(value: string): boolean {
  // e.g. 1.2.3.4-1.2.3.10
  const parts = value.split('-').map((p) => p.trim());
  if (parts.length !== 2) return false;
  return isIpv4(parts[0]) && isIpv4(parts[1]);
}

function expandIpv4Range(value: string, limit: number): string[] {
  const [startStr, endStr] = value.split('-').map((p) => p.trim());
  const start = ipv4ToInt(startStr);
  const end = ipv4ToInt(endStr);
  const lo = Math.min(start, end);
  const hi = Math.max(start, end);
  const count = hi - lo + 1;
  if (count > limit) {
    throw new Error(`Range too large (${count}). Limit is ${limit}.`);
  }
  const out: string[] = [];
  for (let n = lo; n <= hi; n++) out.push(intToIpv4(n >>> 0));
  return out;
}

function expandCidr(value: string, limit: number): string[] {
  const [ipStr, maskStr] = value.split('/');
  if (!ipStr || !maskStr) throw new Error(`Invalid CIDR: ${value}`);
  if (!isIpv4(ipStr)) throw new Error(`Invalid CIDR IP: ${ipStr}`);

  const maskBits = Number(maskStr);
  if (!Number.isInteger(maskBits) || maskBits < 0 || maskBits > 32) {
    throw new Error(`Invalid CIDR mask: ${value}`);
  }

  const ip = ipv4ToInt(ipStr);
  const size = maskBits === 32 ? 1 : 2 ** (32 - maskBits);
  if (size > limit) {
    throw new Error(`Subnet too large (${size}). Limit is ${limit}.`);
  }

  const mask = maskBits === 0 ? 0 : ((0xffffffff << (32 - maskBits)) >>> 0);
  const start = (ip & mask) >>> 0;
  const end = (start + size - 1) >>> 0;

  const out: string[] = [];
  for (let n = start; n <= end; n++) out.push(intToIpv4(n >>> 0));
  return out;
}

function isDomain(value: string): boolean {
  // Basic domain validation: labels 1-63, no leading/trailing '-', at least one dot
  if (value.length > 253) return false;
  const domainRe = /^(?=.{1,253}$)(?!-)[A-Za-z0-9-]{1,63}(?<!-)(\.(?!-)[A-Za-z0-9-]{1,63}(?<!-))+$/;
  return domainRe.test(value);
}

function parseTargetsMultiline(input: string): { targets: string[]; expanded: boolean } | null {
  // User provides one target per line. To be tolerant, also split by commas/spaces.
  const tokens = input
    .split(/\r?\n/)
    .flatMap((line) => line.split(/[\s,]+/))
    .map((t) => t.trim())
    .filter(Boolean);

  if (!tokens.length) return null;

  const MAX_EXPAND_PER_TOKEN = 5000;
  const out: string[] = [];
  let expanded = false;

  for (const t of tokens) {
    if (isIpv4Range(t)) {
      out.push(...expandIpv4Range(t, MAX_EXPAND_PER_TOKEN));
      expanded = true;
      continue;
    }

    if (isIpv4OrCidr(t)) {
      if (t.includes('/')) {
        out.push(...expandCidr(t, MAX_EXPAND_PER_TOKEN));
        expanded = true;
      } else {
        out.push(t);
      }
      continue;
    }

    if (isDomain(t)) {
      out.push(t);
      continue;
    }

    return null;
  }

  // De-dup while preserving order
  const seen = new Set<string>();
  const unique: string[] = [];
  for (const t of out) {
    if (seen.has(t)) continue;
    seen.add(t);
    unique.push(t);
  }

  return { targets: unique, expanded };
}

function chunkArray<T>(items: T[], chunkSize: number): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < items.length; i += chunkSize) out.push(items.slice(i, i + chunkSize));
  return out;
}

function safeFilenameDate(date: Date): string {
  // YYYY-MM-DD_HH-MM-SS (UTC) — filename-safe on Windows
  // Example: 2026-01-20_01-41-17
  return date.toISOString().slice(0, 19).replace('T', '_').replaceAll(':', '-');
}

function formatHumanDate(date: Date): string {
  // Example: 21.01.2026, 14:05:33 (runtime locale/timezone)
  return date.toLocaleString('ru-RU', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false
  });
}

function isPortsConfiguredFromLabel(label: string): boolean {
  const normalized = String(label ?? '')
    .trim()
    .toLowerCase();
  return normalized !== '' && normalized !== 'icmp';
}

function parseRequestedPorts(checkPortsValue: string): string[] {
  const raw = String(checkPortsValue ?? '')
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean);

  const ports = raw.filter((t) => /^\d+$/.test(t));
  // De-dup while preserving order
  const seen = new Set<string>();
  const out: string[] = [];
  for (const p of ports) {
    if (seen.has(p)) continue;
    seen.add(p);
    out.push(p);
  }
  return out;
}

function isPortsConfiguredFromValue(checkPortsValue: string): boolean {
  return parseRequestedPorts(checkPortsValue).length > 0;
}

function formatPortsSuffix(label: string): string {
  return isPortsConfiguredFromLabel(label) ? ` (${label})` : '';
}

function onlyFailed(results: any[]): any[] {
  return results.filter((r) => r && typeof r === 'object' && (r as any).status === false);
}

function buildFailuresReport(params: {
  title: string;
  executedAt: Date;
  sections: Array<{ name: string; lines: string[] }>;
}): { text: string; failuresCount: number } {
  const lines: string[] = [];
  lines.push(params.title);
  lines.push(`Время проверки: ${formatHumanDate(params.executedAt)}`);
  lines.push('');

  let failuresCount = 0;
  for (const s of params.sections) {
    if (!s.lines.length) continue;
    lines.push(`=== ${s.name} ===`);
    lines.push(...s.lines);
    lines.push('');
    failuresCount += s.lines.length;
  }

  return { text: lines.join('\n').trim(), failuresCount };
}

function startPeriodicHealthScheduler(bot: Telegraf<MyContext>) {
  const state = new Map<number, { nextRunAt: number; intervalSec: number; running: boolean }>();

  const tick = async () => {
    const now = Date.now();
    let configs: Array<{ telegramId: number; config: any }> = [];
    try {
      configs = await settingsRepo.listPeriodicHealthConfigs();
    } catch {
      return;
    }

    const activeIds = new Set<number>();
    for (const { telegramId, config } of configs) {
      if (!config?.active) continue;
      activeIds.add(telegramId);

      const intervalSec = typeof config.interval_sec === 'number' ? Math.floor(config.interval_sec) : 0;
      if (!Number.isFinite(intervalSec) || intervalSec < 10) continue;
      const chatId = typeof config.chat_id === 'string' ? config.chat_id.trim() : '';
      if (!chatId) continue;

      const prev = state.get(telegramId);
      if (!prev) {
        state.set(telegramId, { nextRunAt: now + intervalSec * 1000, intervalSec, running: false });
        continue;
      }

      if (prev.intervalSec !== intervalSec) {
        prev.intervalSec = intervalSec;
        prev.nextRunAt = now + intervalSec * 1000;
      }

      if (prev.running) continue;
      if (now < prev.nextRunAt) continue;

      prev.running = true;
      prev.nextRunAt = now + intervalSec * 1000;

      (async () => {
        try {
          const perUserToken = await userRepo.getToken(telegramId);
          if (!perUserToken) return;

          const executedAt = new Date();
          const sections: Array<{ name: string; lines: string[] }> = [];

          // 1) Custom Health Report (local targets)
          const healthCfg = await settingsRepo.getHealthReportConfig(telegramId);
          if (healthCfg?.targets?.length) {
            const targets = healthCfg.targets;
            const batches = chunkArray(targets, 100);

            const portsOpt = pingPortsOptions.find((p) => p.value === healthCfg.ports_value) ?? {
              label: healthCfg.ports_value,
              value: healthCfg.ports_value
            };

              const allowedPortsOrder = parseRequestedPorts(portsOpt.value);
              const allowedPorts = new Set(allowedPortsOrder);

            const runPingBatches = async (routerName: string): Promise<any[]> => {
              const results: any[] = [];
              for (const batch of batches) {
                const ip_pool = batch.join(',');
                const data = await apiClient.ping(
                  { ip_pool, router_name: routerName, check_ports: portsOpt.value },
                  perUserToken
                );
                if (data && typeof data === 'object' && Array.isArray((data as any).results)) {
                  results.push(...(data as any).results);
                }
              }
              return results;
            };

            if (healthCfg.router_value === '__all__') {
              const onlineRouters = (await apiClient.listRouters()).filter((r) => r.status === 'online').map((r) => r.name);
              for (const routerName of onlineRouters) {
                const failed = onlyFailed(await runPingBatches(routerName));
                const lines = failed.map((r) => formatPingResultLine(r, { allowedPorts, allowedPortsOrder }));
                if (lines.length) {
                  sections.push({
                    name: `${routerName} — Кастомный список${formatPortsSuffix(portsOpt.label)}`,
                    lines
                  });
                }
              }
            } else {
              const routerName = healthCfg.router_value || 'auto';
              const failed = onlyFailed(await runPingBatches(routerName));
              const lines = failed.map((r) => formatPingResultLine(r, { allowedPorts, allowedPortsOrder }));
              if (lines.length) {
                sections.push({ name: `${routerName} — Кастомный список${formatPortsSuffix(portsOpt.label)}`, lines });
              }
            }
          }

          // 2) Remnawave Health Report (hosts -> targets)
          const remnaCfg = await settingsRepo.getRemnawaveConfig(telegramId);
          if (remnaCfg.url && remnaCfg.api_token) {
            const hosts = await fetchRemnawaveHosts({ baseUrl: remnaCfg.url, apiToken: remnaCfg.api_token });
            const ignoreSet = new Set((remnaCfg.ignore_list ?? []).map((v) => v.trim().toLowerCase()).filter(Boolean));

            const { addressTargets, hostTargets } = extractRemnawaveTargets({ hosts, ignoreSet });
            if (addressTargets.length || hostTargets.length) {
              const portsValue = remnaCfg.ports_value ?? 'icmp';
              const portsOpt = pingPortsOptions.find((p) => p.value === portsValue) ?? {
                label: portsValue,
                value: portsValue
              };

              const allowedPortsOrder = parseRequestedPorts(portsOpt.value);
              const allowedPorts = new Set(allowedPortsOrder);

              const routerValue = remnaCfg.router_value ?? 'auto';

              const runBatchedPing = async (params: {
                targetsBatches: string[][];
                routerName: string;
                targetKind?: 'host';
              }): Promise<any[]> => {
                const results: any[] = [];
                for (const batch of params.targetsBatches) {
                  const ip_pool = batch.join(',');
                  const data = await apiClient.ping(
                    { ip_pool, router_name: params.routerName, check_ports: portsOpt.value },
                    perUserToken
                  );
                  if (data && typeof data === 'object' && Array.isArray((data as any).results)) {
                    const batchResults = (data as any).results;
                    if (params.targetKind === 'host') {
                      for (const r of batchResults) results.push({ ...r, __targetKind: 'host' });
                    } else {
                      results.push(...batchResults);
                    }
                  }
                }
                return results;
              };

              // Address failures (configured routing)
              if (addressTargets.length) {
                const batches = chunkArray(addressTargets, 100);
                if (routerValue === '__all__') {
                  const onlineRouters = (await apiClient.listRouters()).filter((r) => r.status === 'online').map((r) => r.name);
                  for (const routerName of onlineRouters) {
                    const failed = onlyFailed(await runBatchedPing({ targetsBatches: batches, routerName }));
                    const lines = failed.map((r) => formatPingResultLine(r, { allowedPorts, allowedPortsOrder }));
                    if (lines.length) {
                      sections.push({
                        name: `${routerName} — Remnawave address${formatPortsSuffix(portsOpt.label)}`,
                        lines
                      });
                    }
                  }
                } else {
                  const routerName = routerValue || 'auto';
                  const failed = onlyFailed(await runBatchedPing({ targetsBatches: batches, routerName }));
                  const lines = failed.map((r) => formatPingResultLine(r, { allowedPorts, allowedPortsOrder }));
                  if (lines.length) {
                    sections.push({
                      name: `${routerName} — Remnawave address${formatPortsSuffix(portsOpt.label)}`,
                      lines
                    });
                  }
                }
              }

              // Host failures (forced via Server only)
              if (hostTargets.length) {
                const batches = chunkArray(hostTargets, 100);
                const routerName = 'server';
                const failed = onlyFailed(
                  await runBatchedPing({ targetsBatches: batches, routerName, targetKind: 'host' })
                );
                const lines = failed.map((r) => formatPingResultLine(r, { allowedPorts, allowedPortsOrder }));
                if (lines.length) {
                  sections.push({
                    name: `${routerName} — Remnawave HOST${formatPortsSuffix(portsOpt.label)}`,
                    lines
                  });
                }
              }
            }
          }

          const report = buildFailuresReport({
            title: '📊 Периодический Health Report (проблемы)',
            executedAt,
            sections
          });

          if (report.failuresCount <= 0) return;

          const shouldSendAsFile = report.text.length > TELEGRAM_SAFE_TEXT_LIMIT;
          if (shouldSendAsFile) {
            const filename = `${safeFilenameDate(executedAt)}.txt`;
            await bot.telegram.sendDocument(chatId, { source: Buffer.from(report.text, 'utf8'), filename });
          } else {
            await bot.telegram.sendMessage(chatId, report.text);
          }
        } catch {
          // Silent failure: periodic checks should not crash the bot.
        } finally {
          const cur = state.get(telegramId);
          if (cur) cur.running = false;
        }
      })();
    }

    // Drop state for users that are no longer active
    for (const id of Array.from(state.keys())) {
      if (!activeIds.has(id)) state.delete(id);
    }
  };

  setInterval(() => {
    void tick();
  }, 5000);
}

function formatPingResultLine(
  r: any,
  opts?: { allowedPorts?: Set<string>; allowedPortsOrder?: string[] }
): string {
  const kindPrefix = r?.__targetKind === 'host' ? 'HOST ' : '';
  const ok = r?.status ? '✅' : '❌';
  const ip = r?.ip ?? '';
  const resolved = r?.resolved_ip ? ` -> ${r.resolved_ip}` : '';
  const icmp = r?.ICMP ? ` ICMP: ${r.ICMP}` : '';

  const ports: string[] = [];
  if (r && typeof r === 'object') {
    if (opts?.allowedPorts && opts.allowedPortsOrder?.length) {
      for (const port of opts.allowedPortsOrder) {
        if (!opts.allowedPorts.has(port)) continue;
        const k = `port_${port}`;
        const v = (r as any)[k];
        if (typeof v === 'string' && v) ports.push(`${port}:${v}`);
      }
    } else {
      for (const [k, v] of Object.entries(r)) {
        if (!k.startsWith('port_')) continue;
        const port = k.replace('port_', '');
        if (opts?.allowedPorts && !opts.allowedPorts.has(port)) continue;
        if (typeof v === 'string' && v) ports.push(`${port}:${v}`);
      }
    }
  }
  const portsPart = ports.length ? ` ports[${ports.join(', ')}]` : '';
  return `${kindPrefix}${ok} ${ip}${resolved}${icmp}${portsPart}`;
}

function formatPingReport(params: {
  executedAt: Date;
  routerLabel: string;
  checkPortsLabel: string;
  checkPortsValue: string;
  targetsCount: number;
  results: any[];
  includeTargetsLine?: boolean;
  includePortsLine?: boolean;
}): string {
  const headerLines: string[] = [];
  headerLines.push(`Время проверки: ${formatHumanDate(params.executedAt)}`);
  const includePortsLine = params.includePortsLine ?? true;
  if (includePortsLine && isPortsConfiguredFromValue(params.checkPortsValue)) {
    headerLines.push(`Порты: ${params.checkPortsLabel}`);
  }


  const allowedPortsOrder = parseRequestedPorts(params.checkPortsValue);
  const allowedPorts = new Set(allowedPortsOrder);
  const lines = params.results.map((r) => formatPingResultLine(r, { allowedPorts, allowedPortsOrder }));
  return `${headerLines.join('\n')}\n\n${lines.join('\n')}`.trim();
}

function chunkText(text: string, chunkSize: number = TELEGRAM_SAFE_TEXT_LIMIT): string[] {
  if (text.length <= chunkSize) return [text];
  const chunks: string[] = [];
  let i = 0;
  while (i < text.length) {
    chunks.push(text.slice(i, i + chunkSize));
    i += chunkSize;
  }
  return chunks;
}

function summarizePingResponse(data: any): string {
  if (data && typeof data === 'object' && Array.isArray(data.results)) {
    const lines = data.results.map((r: any) => {
      const ip = r.ip ?? '';
      const resolved = r.resolved_ip ? ` -> ${r.resolved_ip}` : '';
      const ok = r.status ? '✅' : '❌';
      const icmp = r.ICMP ? ` ICMP: ${r.ICMP}` : '';
      return `${ok} ${ip}${resolved}${icmp}`;
    });
    return lines.join('\n');
  }

  try {
    return JSON.stringify(data, null, 2);
  } catch {
    return String(data);
  }
}

function usersListKeyboard(userIds: number[]) {
  const rows: Array<ReturnType<typeof Markup.button.callback>[]> = [];

  const perRow = 2;
  for (let i = 0; i < userIds.length; i += perRow) {
    const slice = userIds.slice(i, i + perRow);
    rows.push(slice.map((id) => Markup.button.callback(String(id), `admin:user:${id}`)));
  }

  rows.push([Markup.button.callback('➕ Добавить пользователя', 'admin:add')]);
  return Markup.inlineKeyboard(rows);
}

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

async function safeEditOrReply(ctx: MyContext, text: string, keyboard?: ReturnType<typeof Markup.inlineKeyboard>) {
  try {
    await (ctx as any).editMessageText(text, keyboard ? keyboard : undefined);
  } catch {
    await ctx.reply(text, keyboard ? keyboard : undefined);
  }
}

async function safeEditOrIgnore(
  ctx: MyContext,
  text: string,
  keyboard?: ReturnType<typeof Markup.inlineKeyboard>
) {
  try {
    await (ctx as any).editMessageText(text, keyboard ? keyboard : undefined);
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    if (msg.includes('message is not modified')) return;
    // For inline toggle UIs we do not send a new message on edit failures.
  }
}

function escapeHtml(text: string): string {
  return text
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;');
}

async function safeEditOrReplyHtml(
  ctx: MyContext,
  html: string,
  keyboard?: ReturnType<typeof Markup.inlineKeyboard>
) {
  const extra = {
    parse_mode: 'HTML' as const,
    ...(keyboard ? (keyboard as any) : {})
  };

  try {
    await (ctx as any).editMessageText(html, extra);
  } catch {
    await ctx.reply(html, extra as any);
  }
}

async function showUsersList(ctx: MyContext, header?: string) {
  const users = await userRepo.listAuthorized();
  const text =
    (header ? `${header}\n\n` : '') +
    (users.length ? 'Пользователи с доступом:' : 'Пользователей с доступом пока нет.');

  await safeEditOrReply(ctx, text, usersListKeyboard(users));
}

async function showRoutersList(ctx: MyContext, header?: string) {
  try {
    const routers = await apiClient.listRouters();
    const rows: Array<ReturnType<typeof Markup.button.callback>[]> = [];

    for (const router of routers) {
      const label = `${getStatusEmoji(router.status)} ${router.name}`;
      rows.push([Markup.button.callback(label, `admin:router:${router.id}`)]);
    }

    rows.push([Markup.button.callback('➕ Добавить роутер', 'admin:add_router')]);

    const text = (header ? `${header}\n\n` : '') + (routers.length ? 'Роутеры:' : 'Нет роутеров.');
    await safeEditOrReply(ctx, text, Markup.inlineKeyboard(rows));
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(
      ctx,
      `Ошибка загрузки роутеров:\n${errMsg}`,
      adminCancelToRootKeyboard()
    );
  }
}

function routerDetailsKeyboard(routerId: number) {
  return Markup.inlineKeyboard([
    [Markup.button.callback('🗑 Удалить роутер', `admin:confirm_delete_router:${routerId}`)],
    [Markup.button.callback('◀️ Назад', 'admin:routers')]
  ]);
}

function formatRouterLastSeen(lastSeen: unknown): string {
  if (lastSeen == null) return 'нет данных';
  if (typeof lastSeen === 'string') return lastSeen;
  if (typeof lastSeen === 'object') {
    const maybeTime = (lastSeen as any).Time;
    if (typeof maybeTime === 'string') {
      return maybeTime;
    }
  }
  try {
    return JSON.stringify(lastSeen);
  } catch {
    return String(lastSeen);
  }
}

// /admin — только админ. Остальным не отвечаем.
bot.command('admin', async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingPingInput = false;
  await ctx.reply('Админ-панель:', adminRootKeyboard());
});

bot.action('admin:root', async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingPingInput = false;
  await ctx.answerCbQuery();
  await safeEditOrReply(ctx, 'Админ-панель:', adminRootKeyboard());
});

bot.action('admin:users', async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingPingInput = false;
  await ctx.answerCbQuery();
  await showUsersList(ctx);
});

bot.action('admin:broadcast', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingBroadcastText = true;
  ctx.session.broadcastDraftText = undefined;

  await safeEditOrReply(
    ctx,
    'Отправь текст рассылки одним сообщением.\n\nПосле этого я покажу предпросмотр и попрошу подтверждение.',
    adminCancelToRootKeyboard()
  );
});

bot.action('admin:broadcast_edit', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingBroadcastText = true;
  await safeEditOrReply(ctx, 'Отправь новый текст рассылки одним сообщением.', adminCancelToRootKeyboard());
});

bot.action('admin:broadcast_send', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  const text = ctx.session.broadcastDraftText;
  if (!text || !text.trim()) {
    ctx.session.awaitingBroadcastText = true;
    ctx.session.broadcastDraftText = undefined;
    await safeEditOrReply(
      ctx,
      'Нет текста для рассылки. Отправь текст одним сообщением.',
      adminCancelToRootKeyboard()
    );
    return;
  }

  const recipients = await userRepo.listAuthorized();
  if (!recipients.length) {
    await safeEditOrReply(ctx, 'Нет пользователей с доступом для рассылки.', adminRootKeyboard());
    return;
  }

  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;

  await safeEditOrReply(ctx, `⏳ Рассылка запущена. Получателей: ${recipients.length}`);

  const sendAsFile = text.length > TELEGRAM_SAFE_TEXT_LIMIT;
  const filename = `broadcast_${safeFilenameDate(new Date())}.txt`;

  let ok = 0;
  let failed = 0;
  for (const telegramId of recipients) {
    try {
      if (sendAsFile) {
        await ctx.telegram.sendDocument(telegramId, { source: Buffer.from(text, 'utf8'), filename });
      } else {
        await ctx.telegram.sendMessage(telegramId, text);
      }
      ok++;
    } catch {
      failed++;
    }
    await sleep(35);
  }

  await ctx.reply(`✅ Рассылка завершена. Успешно: ${ok}, ошибок: ${failed}`);
  await ctx.reply('Админ-панель:', adminRootKeyboard());
});

bot.action('admin:api_url', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = true;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingPingInput = false;

  const current = await settingsRepo.getApiUrl();
  const currentLine = current ? `Текущий API URL: ${current}\n\n` : '';

  await safeEditOrReply(
    ctx,
    `${currentLine}Отправь адрес API в формате: http(s)://example.com:port/ (обязательно порт и слэш в конце).\n\nЧтобы отменить — нажми «Отмена».`,
    adminCancelToRootKeyboard()
  );
});

bot.action('admin:admin_token', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = true;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingPingInput = false;

  await safeEditOrReply(
    ctx,
    'Отправь admin_token одним сообщением.\n\nЧтобы отменить — нажми «Отмена».',
    adminCancelToRootKeyboard()
  );
});

bot.action(/^admin:user:(\d+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;

  const match = ctx.match as RegExpMatchArray;
  const telegramId = Number(match[1]);

  ctx.session.selectedUserTelegramId = telegramId;
  ctx.session.selectedClientId = undefined;
  ctx.session.awaitingClientDeleteConfirm = false;

  await ctx.answerCbQuery();

  try {
    const client = await apiClient.getClientByName(String(telegramId));
    ctx.session.selectedClientId = client.id;

    const createdAt = client.created_at ? String(client.created_at) : '—';
    const blocked = typeof client.blocked === 'boolean' ? String(client.blocked) : '—';
    const token = client.token ? String(client.token) : '';

    const html =
      `Пользователь: ${escapeHtml(String(telegramId))}\n\n` +
      `ID: ${escapeHtml(String(client.id))}\n` +
      `Имя: ${escapeHtml(String(client.name))}\n` +
      `Токен: ${token ? `<code>${escapeHtml(token)}</code>` : '—'}\n` +
      `Ограничен: ${escapeHtml(blocked)}\n` +
      `Создан: ${escapeHtml(createdAt)}\n\n` +
      `Выбери действие:`;

    await safeEditOrReplyHtml(ctx, html, userActionsKeyboard(telegramId));
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    const html =
      `Пользователь: ${escapeHtml(String(telegramId))}\n\n` +
      `⚠️ Не удалось получить клиента из API:\n${escapeHtml(errMsg)}\n\n` +
      `Выбери действие:`;

    await safeEditOrReplyHtml(ctx, html, userActionsKeyboard(telegramId));
  }
});

bot.action('admin:cancel', async (ctx) => {
  if (!isAdmin(ctx)) return;
  ctx.session.awaitingAddUser = false;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  ctx.session.awaitingRouterName = false;
  ctx.session.awaitingRouterDeleteConfirm = null;
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
  ctx.session.awaitingRouterDeleteConfirm = null;
  await ctx.answerCbQuery();
  await showRoutersList(ctx);
});

bot.action(/^admin:router:(\d+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  const match = ctx.match as RegExpMatchArray;
  const routerId = Number(match[1]);
  ctx.session.selectedRouterId = routerId;

  try {
    const router = await apiClient.getRouter(routerId);
    const lastSeen = formatRouterLastSeen((router as any).last_seen);
    const createdAt = typeof (router as any).created_at === 'string' ? (router as any).created_at : 'нет данных';

    const html =
      `Роутер: ${escapeHtml(String(router.name))}\n` +
      `ID: ${escapeHtml(String(router.id))}\n` +
      `Токен: <code>${escapeHtml(String(router.token))}</code>\n` +
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

bot.action(/^admin:confirm_delete_router:(\d+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  const match = ctx.match as RegExpMatchArray;
  const routerId = Number(match[1]);
  ctx.session.awaitingRouterDeleteConfirm = routerId;

  let routerName = 'неизвестно';
  try {
    const router = await apiClient.getRouter(routerId);
    routerName = String(router.name);
  } catch {
    // ignore; we can still confirm by id
  }

  await safeEditOrReply(
    ctx,
    `Вы удаляете роутер ${routerName} с ID: ${routerId}\n\nДля подтверждения нажмите кнопку ниже.`,
    Markup.inlineKeyboard([
      [Markup.button.callback('✓ Да, удалить', `admin:delete_router_confirm:${routerId}`)],
      [Markup.button.callback('◀️ Назад', 'admin:routers')]
    ])
  );
});

bot.action(/^admin:delete_router_confirm:(\d+)$/, async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  const match = ctx.match as RegExpMatchArray;
  const routerId = Number(match[1]);

  try {
    await apiClient.deleteRouter(routerId);
    ctx.session.awaitingRouterDeleteConfirm = null;
    await showRoutersList(ctx, `Роутер ${routerId} удалён`);
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка удаления:\n${errMsg}`, adminCancelToRootKeyboard());
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
  ctx.session.selectedClientId = undefined;
  ctx.session.awaitingClientDeleteConfirm = true;

  let clientIdText = '—';
  try {
    const client = await apiClient.getClientByName(String(telegramId));
    ctx.session.selectedClientId = client.id;
    clientIdText = String(client.id);
  } catch {
    // still allow revoking local access even if API client is missing
  }

  await ctx.answerCbQuery();
  await safeEditOrReply(
    ctx,
    `Вы удаляете клиента пользователя ${telegramId}.\nID клиента: ${clientIdText}\n\nПродолжить?`,
    clientDeleteConfirmKeyboard()
  );
});

bot.action('admin:delete_confirm', async (ctx) => {
  if (!isAdmin(ctx)) return;

  const telegramId = ctx.session.selectedUserTelegramId;
  if (!telegramId) {
    await ctx.answerCbQuery('Пользователь не выбран', { show_alert: true });
    return;
  }

  const clientIdFromSession = ctx.session.selectedClientId;
  ctx.session.awaitingClientDeleteConfirm = false;

  await ctx.answerCbQuery();

  let apiDeleteError: string | null = null;
  try {
    let clientId = clientIdFromSession;
    if (typeof clientId !== 'number' || !Number.isFinite(clientId)) {
      const client = await apiClient.getClientByName(String(telegramId));
      clientId = client.id;
    }

    if (typeof clientId === 'number' && Number.isFinite(clientId)) {
      await apiClient.deleteClient(clientId);
    }
  } catch (err) {
    apiDeleteError = err instanceof Error ? err.message : String(err);
  }

  await userRepo.deleteUser(telegramId);

  const header = apiDeleteError
    ? `Удалён пользователь: ${telegramId}\n⚠️ Ошибка удаления клиента в API: ${apiDeleteError}`
    : `Удалён пользователь: ${telegramId}`;

  await showUsersList(ctx, header);
});

bot.action('admin:add', async (ctx) => {
  if (!isAdmin(ctx)) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingAddUser = true;
  ctx.session.awaitingApiUrl = false;
  ctx.session.awaitingAdminToken = false;
  ctx.session.awaitingBroadcastText = false;
  ctx.session.broadcastDraftText = undefined;
  await safeEditOrReply(
    ctx,
    'Отправь telegram_id пользователя одним сообщением (только цифры).\n\nЧтобы отменить — нажми «Отмена».',
    Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:cancel')]])
  );
});

bot.on('text', async (ctx, next) => {
  // Admin broadcast: ожидаем текст рассылки
  if (isAdmin(ctx) && ctx.session.awaitingBroadcastText) {
    const draft = ctx.message.text;
    if (!draft || !draft.trim()) {
      await ctx.reply('Текст не должен быть пустым. Пришли текст одним сообщением.', adminCancelToRootKeyboard());
      return;
    }

    ctx.session.awaitingBroadcastText = false;
    ctx.session.broadcastDraftText = draft;

    if (draft.length > TELEGRAM_SAFE_TEXT_LIMIT) {
      const filename = `broadcast_preview_${safeFilenameDate(new Date())}.txt`;
      await ctx.reply('Текст длинный — предпросмотр отправляю файлом. Подтвердить рассылку?', adminBroadcastConfirmKeyboard());
      await (ctx as any).replyWithDocument({ source: Buffer.from(draft, 'utf8'), filename });
      return;
    }

    await ctx.reply(`Предпросмотр рассылки:\n\n${draft}\n\nПодтвердить отправку?`, adminBroadcastConfirmKeyboard());
    return;
  }

  // Создание нового роутера: только админ и только когда ждём имя
  if (isAdmin(ctx) && ctx.session.awaitingRouterName) {
    const routerName = ctx.message.text.trim();

    if (!routerName) {
      await ctx.reply('Имя не должно быть пустым.', adminCancelToRootKeyboard());
      return;
    }

    try {
      const newRouter = await apiClient.createRouter(routerName);
      ctx.session.awaitingRouterName = false;
      await ctx.reply(`✓ Роутер создан:\n\nID: ${newRouter.id}\nИмя: ${newRouter.name}\nТокен: ${newRouter.token}`);
      await showRoutersList(ctx);
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Ошибка создания роутера:\n${errMsg}`, adminCancelToRootKeyboard());
    }
    return;
  }

  // Настройка API URL: только админ и только когда ждём url
  if (isAdmin(ctx) && ctx.session.awaitingApiUrl) {
    const text = ctx.message.text.trim();

    let parsed: URL;
    try {
      parsed = new URL(text);
    } catch {
      await ctx.reply(
        'Не похоже на URL. Формат: https://example.com:port/ (со слэшем в конце).',
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:cancel')]])
      );
      return;
    }

    if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') {
      await ctx.reply(
        'Поддерживаются только http:// или https://.',
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:cancel')]])
      );
      return;
    }

    if (!parsed.port) {
      await ctx.reply(
        'Нужен URL с портом, пример: https://example.com:8000/.',
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:cancel')]])
      );
      return;
    }

    if (!text.endsWith('/')) {
      await ctx.reply(
        'Нужен слэш в конце, пример: https://example.com:8000/.',
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:cancel')]])
      );
      return;
    }

    if (parsed.protocol === 'http:') {
      await ctx.reply('Рекомендуется использовать HTTPS ‼️');
    }

    await settingsRepo.setApiUrl(text);
    ctx.session.awaitingApiUrl = false;
    await ctx.reply(`API URL сохранён: ${text}`);
    await ctx.reply('Админ-панель:', adminRootKeyboard());
    return;
  }

  // Настройка admin_token: только админ и только когда ждём токен
  if (isAdmin(ctx) && ctx.session.awaitingAdminToken) {
    const token = ctx.message.text.trim();

    if (!token) {
      await ctx.reply('Токен не должен быть пустым.', adminCancelToRootKeyboard());
      return;
    }

    await settingsRepo.setAdminToken(token);
    ctx.session.awaitingAdminToken = false;

    await ctx.reply('admin_token сохранён.');
    await ctx.reply('Админ-панель:', adminRootKeyboard());
    return;
  }

  // Добавление пользователя: только админ и только когда ждём id
  if (isAdmin(ctx) && ctx.session.awaitingAddUser) {
    const text = ctx.message.text.trim();
    const telegramId = Number(text);

    if (!Number.isSafeInteger(telegramId) || telegramId <= 0) {
      await ctx.reply(
        'Нужен числовой telegram_id. Попробуй ещё раз или нажми «Отмена».',
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'admin:cancel')]])
      );
      return;
    }

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
    }
    ctx.session.awaitingAddUser = false;

    await ctx.reply(`Добавлен пользователь: ${telegramId}`);
    await showUsersList(ctx);
    return;
  }

  // Health Report: сохранение кастомного списка (ожидаем ввод)
  if (ctx.session.awaitingHealthListInput && (await isAuthorizedUser(ctx))) {
    const telegramId = ctx.from?.id;
    if (telegramId == null) return next();

    const input = ctx.message.text.trim();
    let parsed: { targets: string[]; expanded: boolean } | null = null;
    try {
      parsed = parseTargetsMultiline(input);
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Ошибка валидации целей: ${errMsg}`, healthCustomKeyboard(ctx.session));
      return;
    }

    if (!parsed) {
      await ctx.reply(
        'Неверный формат. Пришли IPv4, подсеть (CIDR), диапазон IPv4 или домен. Каждый объект с новой строки.',
        healthCustomKeyboard(ctx.session)
      );
      return;
    }

    const routerOpt = getHealthRouterOption(ctx.session);
    const portsOpt = getHealthPortsOption(ctx.session);

    await settingsRepo.setHealthReportConfig(telegramId, {
      targets: parsed.targets,
      router_value: routerOpt.value,
      ports_value: portsOpt.value
    });

    ctx.session.awaitingHealthListInput = false;
    await ctx.reply('✅ Кастомный список сохранён.');
    await showHealthMenu(ctx);
    return;
  }

  // Remnawave URL
  if (ctx.session.awaitingRemnaUrl && (await isAuthorizedUser(ctx))) {
    const telegramId = ctx.from?.id;
    if (telegramId == null) return next();

    const text = ctx.message.text.trim();
    let parsed: URL;
    try {
      parsed = new URL(text);
    } catch {
      await ctx.reply('Не похоже на URL. Формат: https://example.remna.com/', remnaMenuKeyboard());
      return;
    }

    if (parsed.protocol !== 'https:') {
      await ctx.reply('URL должен начинаться с https://', remnaMenuKeyboard());
      return;
    }

    if (!text.endsWith('/')) {
      await ctx.reply('Нужен слэш в конце, пример: https://example.remna.com/', remnaMenuKeyboard());
      return;
    }

    await settingsRepo.setRemnawaveUrl(telegramId, text);
    ctx.session.awaitingRemnaUrl = false;
    await ctx.reply('✅ URL сохранён.');
    await showRemnaMenu(ctx);
    return;
  }

  // Remnawave API token
  if (ctx.session.awaitingRemnaToken && (await isAuthorizedUser(ctx))) {
    const telegramId = ctx.from?.id;
    if (telegramId == null) return next();

    const token = ctx.message.text.trim();
    if (!token) {
      await ctx.reply('Токен не должен быть пустым.', remnaMenuKeyboard());
      return;
    }

    await settingsRepo.setRemnawaveApiToken(telegramId, token);
    ctx.session.awaitingRemnaToken = false;
    await ctx.reply('✅ API токен сохранён.');
    await showRemnaMenu(ctx);
    return;
  }

  // Remnawave ignore list
  if (ctx.session.awaitingRemnaIgnoreList && (await isAuthorizedUser(ctx))) {
    const telegramId = ctx.from?.id;
    if (telegramId == null) return next();

    const input = ctx.message.text.trim();
    const list = parseIgnoreList(input);
    if (!list) {
      await ctx.reply(
        'Неверный формат. Пришли IPv4 или домены. Каждый объект с новой строки.',
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'health:remna')]])
      );
      return;
    }

    await settingsRepo.setRemnawaveIgnoreList(telegramId, list);
    ctx.session.awaitingRemnaIgnoreList = false;
    await ctx.reply('✅ Список игнорирования сохранён.');
    await showRemnaMenu(ctx);
    return;
  }

  // Vultr API token
  if (ctx.session.awaitingVultrToken && (await isAuthorizedUser(ctx))) {
    const telegramId = ctx.from?.id;
    if (telegramId == null) return next();

    const token = ctx.message.text.trim();
    if (!token) {
      await ctx.reply('Токен не должен быть пустым.', vultrMenuKeyboard());
      return;
    }

    await settingsRepo.setVultrApiToken(telegramId, token);
    ctx.session.awaitingVultrToken = false;
    await ctx.reply('✅ Vultr API токен сохранён.');
    await showVultrMenu(ctx);
    return;
  }

  // Vultr TAG
  if (ctx.session.awaitingVultrTag && (await isAuthorizedUser(ctx))) {
    const telegramId = ctx.from?.id;
    if (telegramId == null) return next();

    const tag = ctx.message.text.trim();
    if (!isValidVultrTag(tag)) {
      await ctx.reply('Неверный TAG. Требования: не пустой, без пробелов, до 64 символов.', vultrMenuKeyboard());
      return;
    }

    await settingsRepo.setVultrTag(telegramId, tag);
    ctx.session.awaitingVultrTag = false;
    await ctx.reply('✅ TAG сохранён.');
    await showVultrMenu(ctx);
    return;
  }

  // Periodic Health Report: chat id
  if (ctx.session.awaitingHealthPeriodicChatId && (await isAuthorizedUser(ctx))) {
    const telegramId = ctx.from?.id;
    if (telegramId == null) return next();

    const chatId = ctx.message.text.trim();
    if (!isTelegramChatId(chatId)) {
      await ctx.reply(
        'Неверный chat id. Пришлите число (например 123456789 или -1001234567890).',
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'health:periodic')]])
      );
      return;
    }

    await settingsRepo.setPeriodicHealthChatId(telegramId, chatId);
    ctx.session.awaitingHealthPeriodicChatId = false;
    await ctx.reply('✅ ChatID сохранён.');
    await showHealthPeriodicMenu(ctx);
    return;
  }

  // Periodic Health Report: interval
  if (ctx.session.awaitingHealthPeriodicInterval && (await isAuthorizedUser(ctx))) {
    const telegramId = ctx.from?.id;
    if (telegramId == null) return next();

    const raw = ctx.message.text.trim();
    const n = Number(raw);
    if (!Number.isInteger(n) || n < 10 || n > 86400) {
      await ctx.reply(
        'Неверный интервал. Пришлите целое число секунд (от 10 до 86400).',
        Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'health:periodic')]])
      );
      return;
    }

    await settingsRepo.setPeriodicHealthInterval(telegramId, n);
    ctx.session.awaitingHealthPeriodicInterval = false;
    await ctx.reply('✅ Периодичность сохранена.');
    await showHealthPeriodicMenu(ctx);
    return;
  }

  // Ping: для авторизованных пользователей (ожидаем ввод цели)
  if (ctx.session.awaitingPingInput && (await isAuthorizedUser(ctx))) {
    const input = ctx.message.text.trim();
    let parsed: { targets: string[]; expanded: boolean } | null = null;
    try {
      parsed = parseTargetsMultiline(input);
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Ошибка валидации целей: ${errMsg}`, pingKeyboard(ctx.session));
      return;
    }

    if (!parsed) {
      await ctx.reply(
        'Неверный формат. Пришли IPv4 (например 1.2.3.4), подсеть (например 1.2.3.0/24), диапазон (например 1.2.3.4-1.2.3.10) или домен (example.com). Можно несколько — каждый с новой строки.',
        pingKeyboard(ctx.session)
      );
      return;
    }

    const targets = parsed.targets;
    const totalTargets = targets.length;
    const batches = chunkArray(targets, 100);

    const routerOpt = getPingRouterOption(ctx.session);
    const portsOpt = getPingPortsOption(ctx.session);

    const fromId = ctx.from?.id;
    const perUserToken = fromId != null ? await userRepo.getToken(fromId) : null;
    if (!perUserToken) {
      ctx.session.awaitingPingInput = false;
      await ctx.reply('⚠️ Не найден client token. Обратитесь к администратору!');
      await ctx.reply('Вы в главном меню', mainMenuKeyboard());
      return;
    }

    ctx.session.awaitingPingInput = false;
    await ctx.reply(`⏳ Выполняю ping... (${totalTargets} целей)`);

    const executedAt = new Date();

    const runPingBatches = async (routerName: string): Promise<any[]> => {
      const results: any[] = [];
      for (const batch of batches) {
        const ip_pool = batch.join(',');
        const data = await apiClient.ping(
          {
            ip_pool,
            router_name: routerName,
            check_ports: portsOpt.value
          },
          perUserToken
        );
        if (data && typeof data === 'object' && Array.isArray((data as any).results)) {
          results.push(...(data as any).results);
        }
      }
      return results;
    };

    try {
      if (routerOpt.value === '__all__') {
        const routerNames = (ctx.session.pingRouters ?? []).map((r) => r.name);
        if (!routerNames.length) {
          await ctx.reply('Нет роутеров для режима ALL.');
          await ctx.reply('Вы в главном меню', mainMenuKeyboard());
          return;
        }

        const sections: string[] = [];
        for (const name of routerNames) {
          const results = await runPingBatches(name);
          sections.push(
            `=== ${name} ===\n` +
              formatPingReport({
                executedAt,
                routerLabel: name,
                checkPortsLabel: portsOpt.label,
                checkPortsValue: portsOpt.value,
                targetsCount: totalTargets,
                results
              })
          );
        }

        const reportText = sections.join('\n\n');

        const sendAsFile = reportText.length > TELEGRAM_SAFE_TEXT_LIMIT;
        if (sendAsFile) {
          const filename = `${safeFilenameDate(executedAt)}.txt`;
          await (ctx as any).replyWithDocument({ source: Buffer.from(reportText, 'utf8'), filename });
        } else {
          await ctx.reply(reportText);
        }
      } else {
        const routerName = routerOpt.value;
        const results = await runPingBatches(routerName);
        const reportText =
          `=== ${routerName} ===\n` +
          formatPingReport({
            executedAt,
            routerLabel: routerName,
            checkPortsLabel: portsOpt.label,
            checkPortsValue: portsOpt.value,
            targetsCount: totalTargets,
            results
          });

        const sendAsFile = reportText.length > TELEGRAM_SAFE_TEXT_LIMIT;
        if (sendAsFile) {
          const filename = `${safeFilenameDate(executedAt)}.txt`;
          await (ctx as any).replyWithDocument({ source: Buffer.from(reportText, 'utf8'), filename });
        } else {
          await ctx.reply(reportText);
        }
      }

      await ctx.reply('Вы в главном меню', mainMenuKeyboard());
    } catch (err) {
      const errMsg = err instanceof Error ? err.message : String(err);
      await ctx.reply(`Ошибка ping:\n${errMsg}`);
      await ctx.reply('Вы в главном меню', mainMenuKeyboard());
    }

    return;
  }

  // Иначе — отдаём управление следующим обработчикам (например, /start)
  return next();
});

bot.action('menu:health', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingHealthListInput = false;
  ctx.session.awaitingRemnaUrl = false;
  ctx.session.awaitingRemnaToken = false;
  ctx.session.awaitingRemnaIgnoreList = false;
  ctx.session.awaitingVultrToken = false;
  ctx.session.awaitingVultrTag = false;
  ctx.session.awaitingHealthPeriodicChatId = false;
  ctx.session.awaitingHealthPeriodicInterval = false;
  await ctx.answerCbQuery();
  await showHealthMenu(ctx);
});

bot.action('health:periodic', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingHealthListInput = false;
  ctx.session.awaitingRemnaUrl = false;
  ctx.session.awaitingRemnaToken = false;
  ctx.session.awaitingRemnaIgnoreList = false;
  ctx.session.awaitingVultrToken = false;
  ctx.session.awaitingVultrTag = false;
  ctx.session.awaitingHealthPeriodicChatId = false;
  ctx.session.awaitingHealthPeriodicInterval = false;
  await ctx.answerCbQuery();
  await showHealthPeriodicMenu(ctx);
});

bot.action('healthp:toggle', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  const telegramId = ctx.from?.id;
  if (telegramId == null) return;

  const cfg = await settingsRepo.getPeriodicHealthConfig(telegramId);
  await settingsRepo.setPeriodicHealthActive(telegramId, !cfg.active);
  await showHealthPeriodicMenu(ctx);
});

bot.action('healthp:chat', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingHealthPeriodicChatId = true;
  ctx.session.awaitingHealthPeriodicInterval = false;
  await safeEditOrIgnore(
    ctx,
    'Настройка периодических Health Report\n\nОтправьте telegram chat id (пользователь или группа).',
    Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'health:periodic')]])
  );
});

bot.action('healthp:interval', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingHealthPeriodicChatId = false;
  ctx.session.awaitingHealthPeriodicInterval = true;
  await safeEditOrIgnore(
    ctx,
    'Настройка периодических Health Report\n\nОтправьте периодичность проверок в секундах (например 300).',
    Markup.inlineKeyboard([[Markup.button.callback('◀️ Назад', 'health:periodic')]])
  );
});

bot.action('health:remna', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingHealthListInput = false;
  ctx.session.awaitingRemnaUrl = false;
  ctx.session.awaitingRemnaToken = false;
  ctx.session.awaitingRemnaIgnoreList = false;
  ctx.session.awaitingVultrToken = false;
  ctx.session.awaitingVultrTag = false;
  await ctx.answerCbQuery();
  await showRemnaMenu(ctx);
});

bot.action('health:vultr', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  ctx.session.awaitingHealthListInput = false;
  ctx.session.awaitingRemnaUrl = false;
  ctx.session.awaitingRemnaToken = false;
  ctx.session.awaitingRemnaIgnoreList = false;
  ctx.session.awaitingVultrToken = false;
  ctx.session.awaitingVultrTag = false;
  await ctx.answerCbQuery();
  await showVultrMenu(ctx);
});

bot.action('remna:url', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingRemnaUrl = true;
  ctx.session.awaitingRemnaToken = false;
  ctx.session.awaitingRemnaIgnoreList = false;
  await safeEditOrIgnore(ctx, '🟦 Меню Интеграции Remnawave\n\nОтправьте URL в формате: https://example.remna.com/', remnaMenuKeyboard());
});

bot.action('remna:token', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingRemnaUrl = false;
  ctx.session.awaitingRemnaToken = true;
  ctx.session.awaitingRemnaIgnoreList = false;
  await safeEditOrIgnore(ctx, '🟦 Меню Интеграции Remnawave\n\nОтправьте API токен одним сообщением.', remnaMenuKeyboard());
});

bot.action('remna:ignore', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingRemnaUrl = false;
  ctx.session.awaitingRemnaToken = false;
  ctx.session.awaitingRemnaIgnoreList = false;
  await showRemnaIgnoreEditor(ctx);
});

bot.action('remna:settings', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingRemnaUrl = false;
  ctx.session.awaitingRemnaToken = false;
  ctx.session.awaitingRemnaIgnoreList = false;

  // reset indices so editor aligns to saved values on entry
  ctx.session.remnaRouterIndex = undefined;
  ctx.session.remnaPortsIndex = undefined;
  await showRemnaSettingsEditor(ctx);
});

bot.action('remna:toggle_router', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  if (!ctx.session.remnaRouters) {
    try {
      const allRouters = await apiClient.listRouters();
      ctx.session.remnaRouters = allRouters.filter((r) => r.status === 'online');
    } catch {
      ctx.session.remnaRouters = [];
    }
  }

  const len = 2 + (ctx.session.remnaRouters?.length ?? 0);
  const current = ctx.session.remnaRouterIndex ?? 0;
  ctx.session.remnaRouterIndex = len > 0 ? (current + 1) % len : 0;
  await showRemnaSettingsEditor(ctx);
});

bot.action('remna:toggle_ports', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  const len = pingPortsOptions.length;
  const current = ctx.session.remnaPortsIndex ?? 0;
  ctx.session.remnaPortsIndex = len > 0 ? (current + 1) % len : 0;
  await showRemnaSettingsEditor(ctx);
});

bot.action('remna:settings:back', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  const telegramId = ctx.from?.id;
  if (telegramId != null) {
    const routerOpt = getRemnaRouterOption(ctx.session);
    const portsOpt = getRemnaPortsOption(ctx.session);
    await settingsRepo.setRemnawaveSettings(telegramId, {
      router_value: routerOpt.value,
      ports_value: portsOpt.value
    });
  }

  await showRemnaMenu(ctx);
});

bot.action('vultr:token', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingVultrToken = true;
  ctx.session.awaitingVultrTag = false;
  await safeEditOrIgnore(ctx, '⏹️ Меню Интеграции Vultr\n\nОтправьте Vultr API токен одним сообщением.', vultrMenuKeyboard());
});

bot.action('vultr:tag', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingVultrToken = false;
  ctx.session.awaitingVultrTag = true;
  await safeEditOrIgnore(
    ctx,
    '⏹️ Меню Интеграции Vultr\n\nОтправьте TAG. Будут выбраны инстансы, у которых label заканчивается на -{TAG}.\nПример: label=server123-prod → TAG=prod',
    vultrMenuKeyboard()
  );
});

bot.action('vultr:settings', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  ctx.session.awaitingVultrToken = false;
  ctx.session.awaitingVultrTag = false;

  // reset indices so editor aligns to saved values on entry
  ctx.session.vultrRouterIndex = undefined;
  ctx.session.vultrPortsIndex = undefined;
  await showVultrSettingsEditor(ctx);
});

bot.action('vultr:toggle_router', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  if (!ctx.session.vultrRouters) {
    try {
      const allRouters = await apiClient.listRouters();
      ctx.session.vultrRouters = allRouters.filter((r) => r.status === 'online');
    } catch {
      ctx.session.vultrRouters = [];
    }
  }

  const len = 2 + (ctx.session.vultrRouters?.length ?? 0);
  const current = ctx.session.vultrRouterIndex ?? 0;
  ctx.session.vultrRouterIndex = len > 0 ? (current + 1) % len : 0;
  await showVultrSettingsEditor(ctx);
});

bot.action('vultr:toggle_ports', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();
  const len = pingPortsOptions.length;
  const current = ctx.session.vultrPortsIndex ?? 0;
  ctx.session.vultrPortsIndex = len > 0 ? (current + 1) % len : 0;
  await showVultrSettingsEditor(ctx);
});

bot.action('vultr:settings:back', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  const telegramId = ctx.from?.id;
  if (telegramId != null) {
    const routerOpt = getVultrRouterOption(ctx.session);
    const portsOpt = getVultrPortsOption(ctx.session);
    await settingsRepo.setVultrSettings(telegramId, {
      router_value: routerOpt.value,
      ports_value: portsOpt.value
    });
  }

  await showVultrMenu(ctx);
});

bot.action('health:custom', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  await ctx.answerCbQuery();
  // Reset indices so editor aligns to saved values on entry
  ctx.session.healthRouterIndex = undefined;
  ctx.session.healthPortsIndex = undefined;
  await showHealthCustomListEditor(ctx);
});

bot.action('health:toggle_router', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  if (!ctx.session.healthRouters) {
    try {
      const allRouters = await apiClient.listRouters();
      ctx.session.healthRouters = allRouters.filter((r) => r.status === 'online');
    } catch {
      ctx.session.healthRouters = [];
    }
  }

  const len = 2 + (ctx.session.healthRouters?.length ?? 0);
  const current = ctx.session.healthRouterIndex ?? 0;
  ctx.session.healthRouterIndex = len > 0 ? (current + 1) % len : 0;

  await showHealthCustomListEditor(ctx);
});

bot.action('health:toggle_ports', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  const len = pingPortsOptions.length;
  const current = ctx.session.healthPortsIndex ?? 0;
  ctx.session.healthPortsIndex = len > 0 ? (current + 1) % len : 0;

  await showHealthCustomListEditor(ctx);
});

bot.action('health:cancel', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await showHealthMenu(ctx);
    return;
  }

  // Save current router/ports selection even if user didn't send new targets
  const existing = (await settingsRepo.getHealthReportConfig(telegramId)) ?? {
    targets: [],
    router_value: 'auto',
    ports_value: 'icmp'
  };

  const routerOpt = getHealthRouterOption(ctx.session);
  const portsOpt = getHealthPortsOption(ctx.session);

  await settingsRepo.setHealthReportConfig(telegramId, {
    targets: existing.targets,
    router_value: routerOpt.value,
    ports_value: portsOpt.value
  });

  ctx.session.awaitingHealthListInput = false;
  await showHealthMenu(ctx);
});

bot.action('health:force', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrReply(ctx, 'Ошибка: не удалось определить пользователя.', healthMenuKeyboard());
    return;
  }

  const cfg = await settingsRepo.getHealthReportConfig(telegramId);
  if (!cfg || !cfg.targets?.length) {
    await safeEditOrReply(ctx, '⚠️ Кастомный список пуст. Сначала заполните его в «Кастомный список».', healthMenuKeyboard());
    return;
  }

  const fromId = ctx.from?.id;
  const perUserToken = fromId != null ? await userRepo.getToken(fromId) : null;
  if (!perUserToken) {
    await safeEditOrReply(ctx, '⚠️ Не найден client token. Обратитесь к администратору!', healthMenuKeyboard());
    return;
  }

  // Load routers list for ALL and for label calculations
  let routers: string[] = [];
  try {
    const rs = await apiClient.listRouters();
    routers = rs.filter((r) => r.status === 'online').map((r) => r.name);
  } catch {
    routers = [];
  }

  const targets = cfg.targets;
  const totalTargets = targets.length;
  const batches = chunkArray(targets, 100);
  const executedAt = new Date();

  const portsOpt = pingPortsOptions.find((p) => p.value === cfg.ports_value) ?? {
    label: cfg.ports_value,
    value: cfg.ports_value
  };

  const runPingBatches = async (routerName: string): Promise<any[]> => {
    const results: any[] = [];
    for (const batch of batches) {
      const ip_pool = batch.join(',');
      const data = await apiClient.ping(
        {
          ip_pool,
          router_name: routerName,
          check_ports: portsOpt.value
        },
        perUserToken
      );
      if (data && typeof data === 'object' && Array.isArray((data as any).results)) {
        results.push(...(data as any).results);
      }
    }
    return results;
  };

  await safeEditOrReply(ctx, `⏳ Выполняю Health Report... (${totalTargets} целей)`, healthMenuKeyboard());

  try {
    if (cfg.router_value === '__all__') {
      if (!routers.length) {
        await safeEditOrReply(ctx, 'Нет роутеров для режима ALL.', healthMenuKeyboard());
        return;
      }

      const sections: string[] = [];
      for (const name of routers) {
        const results = await runPingBatches(name);
        sections.push(
          `=== ${name} ===\n` +
            formatPingReport({
              executedAt,
              routerLabel: name,
              checkPortsLabel: portsOpt.label,
              checkPortsValue: portsOpt.value,
              targetsCount: totalTargets,
              results,
              includeTargetsLine: false,
              includePortsLine: false
            })
        );
      }

      const reportText = sections.join('\n\n');
      const sendAsFile = reportText.length > TELEGRAM_SAFE_TEXT_LIMIT;
      if (sendAsFile) {
        const filename = `${safeFilenameDate(executedAt)}.txt`;
        await (ctx as any).replyWithDocument({ source: Buffer.from(reportText, 'utf8'), filename });
      } else {
        await ctx.reply(reportText);
      }
    } else {
      const routerName = cfg.router_value || 'auto';
      const results = await runPingBatches(routerName);
      const reportText =
        `=== ${routerName} ===\n` +
        formatPingReport({
          executedAt,
          routerLabel: routerName,
          checkPortsLabel: portsOpt.label,
          checkPortsValue: portsOpt.value,
          targetsCount: totalTargets,
          results
        });

      const sendAsFile = reportText.length > TELEGRAM_SAFE_TEXT_LIMIT;
      if (sendAsFile) {
        const filename = `${safeFilenameDate(executedAt)}.txt`;
        await (ctx as any).replyWithDocument({ source: Buffer.from(reportText, 'utf8'), filename });
      } else {
        await ctx.reply(reportText);
      }
    }

    await showHealthMenu(ctx);
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка Health Report:\n${errMsg}`, healthMenuKeyboard());
  }
});

bot.action('health:remna_force', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrReply(ctx, 'Ошибка: не удалось определить пользователя.', healthMenuKeyboard());
    return;
  }

  const remnaCfg = await settingsRepo.getRemnawaveConfig(telegramId);
  if (!remnaCfg.url) {
    await safeEditOrReply(
      ctx,
      '⚠️ Не задан Remnawave URL. Откройте «🟦 Интеграция Remnawave» и сохраните URL.',
      healthMenuKeyboard()
    );
    return;
  }

  if (!remnaCfg.api_token) {
    await safeEditOrReply(
      ctx,
      '⚠️ Не задан Remnawave API токен. Откройте «🟦 Интеграция Remnawave» и сохраните токен.',
      healthMenuKeyboard()
    );
    return;
  }

  const perUserToken = await userRepo.getToken(telegramId);
  if (!perUserToken) {
    await safeEditOrReply(ctx, '⚠️ Не найден client token. Обратитесь к администратору!', healthMenuKeyboard());
    return;
  }

  await safeEditOrReply(ctx, '⏳ Получаю список хостов из Remnawave...', healthMenuKeyboard());

  try {
    const hosts = await fetchRemnawaveHosts({
      baseUrl: remnaCfg.url,
      apiToken: remnaCfg.api_token
    });

    const ignoreSet = new Set((remnaCfg.ignore_list ?? []).map((v) => v.trim().toLowerCase()).filter(Boolean));

    const { addressTargets, hostTargets } = extractRemnawaveTargets({ hosts, ignoreSet });

    if (!addressTargets.length && !hostTargets.length) {
      await safeEditOrReply(
        ctx,
        '⚠️ В Remnawave нет подходящих целей (или все отфильтрованы/в игноре).',
        healthMenuKeyboard()
      );
      return;
    }

    // Load routers list for ALL and for label calculations
    let routers: string[] = [];
    try {
      const rs = await apiClient.listRouters();
      routers = rs.filter((r) => r.status === 'online').map((r) => r.name);
    } catch {
      routers = [];
    }

    const totalTargets = addressTargets.length + hostTargets.length;
    const addressBatches = chunkArray(addressTargets, 100);
    const hostBatches = chunkArray(hostTargets, 100);
    const executedAt = new Date();

    const portsValue = remnaCfg.ports_value ?? 'icmp';
    const portsOpt = pingPortsOptions.find((p) => p.value === portsValue) ?? {
      label: portsValue,
      value: portsValue
    };

    const routerValue = remnaCfg.router_value ?? 'auto';

    const runBatchedPing = async (params: {
      targetsBatches: string[][];
      routerName: string;
      targetKind?: 'host';
    }): Promise<any[]> => {
      const results: any[] = [];
      for (const batch of params.targetsBatches) {
        const ip_pool = batch.join(',');
        const data = await apiClient.ping(
          {
            ip_pool,
            router_name: params.routerName,
            check_ports: portsOpt.value
          },
          perUserToken
        );
        if (data && typeof data === 'object' && Array.isArray((data as any).results)) {
          const batchResults = (data as any).results;
          if (params.targetKind === 'host') {
            for (const r of batchResults) results.push({ ...r, __targetKind: 'host' });
          } else {
            results.push(...batchResults);
          }
        }
      }
      return results;
    };

    await safeEditOrReply(ctx, `⏳ Выполняю Remnawave Report... (${totalTargets} целей)`, healthMenuKeyboard());

    const sections: string[] = [];

    // 1) Address ping (existing behavior)
    if (addressTargets.length) {
      if (routerValue === '__all__') {
        if (!routers.length) {
          await safeEditOrReply(ctx, 'Нет роутеров для режима ALL.', healthMenuKeyboard());
          return;
        }

        for (const name of routers) {
          const results = await runBatchedPing({ targetsBatches: addressBatches, routerName: name });
          sections.push(
            `=== ${name} (address) ===\n` +
              formatPingReport({
                executedAt,
                routerLabel: name,
                checkPortsLabel: portsOpt.label,
                checkPortsValue: portsOpt.value,
                targetsCount: addressTargets.length,
                results
              })
          );
        }
      } else {
        const routerName = routerValue || 'auto';
        const results = await runBatchedPing({ targetsBatches: addressBatches, routerName });
        sections.push(
          `=== ${routerName} (address) ===\n` +
            formatPingReport({
              executedAt,
              routerLabel: routerName,
              checkPortsLabel: portsOpt.label,
              checkPortsValue: portsOpt.value,
              targetsCount: addressTargets.length,
              results
            })
        );
      }
    }

    // 2) Host ping (forced via Server only)
    if (hostTargets.length) {
      const routerName = 'server';
      const results = await runBatchedPing({ targetsBatches: hostBatches, routerName, targetKind: 'host' });
      sections.push(
        `=== ${routerName} (HOST) ===\n` +
          formatPingReport({
            executedAt,
            routerLabel: routerName,
            checkPortsLabel: portsOpt.label,
            checkPortsValue: portsOpt.value,
            targetsCount: hostTargets.length,
            results
          })
      );
    }

    const reportText = sections.join('\n\n');
    const sendAsFile = reportText.length > TELEGRAM_SAFE_TEXT_LIMIT;
    if (sendAsFile) {
      const filename = `${safeFilenameDate(executedAt)}.txt`;
      await (ctx as any).replyWithDocument({ source: Buffer.from(reportText, 'utf8'), filename });
    } else {
      await ctx.reply(reportText);
    }

    await showHealthMenu(ctx);
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка Remnawave Report:\n${errMsg}`, healthMenuKeyboard());
  }
});

bot.action('health:vultr_force', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  const telegramId = ctx.from?.id;
  if (telegramId == null) {
    await safeEditOrReply(ctx, 'Ошибка: не удалось определить пользователя.', healthMenuKeyboard());
    return;
  }

  const vultrCfg = await settingsRepo.getVultrConfig(telegramId);
  if (!vultrCfg.api_token) {
    await safeEditOrReply(
      ctx,
      '⚠️ Не задан Vultr API токен. Откройте «⏹️ Интеграция Vultr» и сохраните токен.',
      healthMenuKeyboard()
    );
    return;
  }
  if (!vultrCfg.tag) {
    await safeEditOrReply(
      ctx,
      '⚠️ Не задан TAG. Откройте «⏹️ Интеграция Vultr» → «📌Тег» и сохраните TAG.',
      healthMenuKeyboard()
    );
    return;
  }

  const perUserToken = await userRepo.getToken(telegramId);
  if (!perUserToken) {
    await safeEditOrReply(ctx, '⚠️ Не найден client token. Обратитесь к администратору!', healthMenuKeyboard());
    return;
  }

  await safeEditOrReply(ctx, '⏳ Получаю список инстансов из Vultr...', healthMenuKeyboard());

  try {
    const allInstances = await fetchVultrInstances({ apiToken: vultrCfg.api_token });
    const instances = filterVultrInstancesByTag({ instances: allInstances, tag: vultrCfg.tag });

    const targets: string[] = [];
    const seen = new Set<string>();
    for (const inst of instances) {
      const ip = (inst.main_ip ?? '').trim();
      if (!ip) continue;
      if (!isIpv4(ip)) continue;
      if (seen.has(ip)) continue;
      seen.add(ip);
      targets.push(ip);
    }

    if (!targets.length) {
      await safeEditOrReply(
        ctx,
        `⚠️ Нет подходящих инстансов Vultr по TAG "${vultrCfg.tag}" (label должен заканчиваться на -${vultrCfg.tag}).`,
        healthMenuKeyboard()
      );
      return;
    }

    // Load routers list for ALL
    let routers: string[] = [];
    try {
      const rs = await apiClient.listRouters();
      routers = rs.filter((r) => r.status === 'online').map((r) => r.name);
    } catch {
      routers = [];
    }

    const totalTargets = targets.length;
    const batches = chunkArray(targets, 100);
    const executedAt = new Date();

    const portsValue = vultrCfg.ports_value ?? 'icmp';
    const portsOpt = pingPortsOptions.find((p) => p.value === portsValue) ?? {
      label: portsValue,
      value: portsValue
    };

    const routerValue = vultrCfg.router_value ?? 'auto';

    const runPingBatches = async (routerName: string): Promise<any[]> => {
      const results: any[] = [];
      for (const batch of batches) {
        const ip_pool = batch.join(',');
        const data = await apiClient.ping(
          {
            ip_pool,
            router_name: routerName,
            check_ports: portsOpt.value
          },
          perUserToken
        );
        if (data && typeof data === 'object' && Array.isArray((data as any).results)) {
          results.push(...(data as any).results);
        }
      }
      return results;
    };

    await safeEditOrReply(ctx, `⏳ Выполняю Vultr Report... (${totalTargets} целей)`, healthMenuKeyboard());

    if (routerValue === '__all__') {
      if (!routers.length) {
        await safeEditOrReply(ctx, 'Нет роутеров для режима ALL.', healthMenuKeyboard());
        return;
      }

      const sections: string[] = [];
      for (const name of routers) {
        const results = await runPingBatches(name);
        sections.push(
          `=== ${name} ===\n` +
            formatPingReport({
              executedAt,
              routerLabel: name,
              checkPortsLabel: portsOpt.label,
              checkPortsValue: portsOpt.value,
              targetsCount: totalTargets,
              results
            })
        );
      }

      const reportText = `Vultr Report\n\n${sections.join('\n\n')}`;
      const sendAsFile = reportText.length > TELEGRAM_SAFE_TEXT_LIMIT;
      if (sendAsFile) {
        const filename = `${safeFilenameDate(executedAt)}.txt`;
        await (ctx as any).replyWithDocument({ source: Buffer.from(reportText, 'utf8'), filename });
      } else {
        await ctx.reply(reportText);
      }
    } else {
      const routerName = routerValue || 'auto';
      const results = await runPingBatches(routerName);
      const reportText =
        `Vultr Report\n\n` +
        `=== ${routerName} ===\n` +
        formatPingReport({
          executedAt,
          routerLabel: routerName,
          checkPortsLabel: portsOpt.label,
          checkPortsValue: portsOpt.value,
          targetsCount: totalTargets,
          results,
          includeTargetsLine: false,
          includePortsLine: false
        });

      const sendAsFile = reportText.length > TELEGRAM_SAFE_TEXT_LIMIT;
      if (sendAsFile) {
        const filename = `${safeFilenameDate(executedAt)}.txt`;
        await (ctx as any).replyWithDocument({ source: Buffer.from(reportText, 'utf8'), filename });
      } else {
        await ctx.reply(reportText);
      }
    }

    await showHealthMenu(ctx);
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка Vultr Report:\n${errMsg}`, healthMenuKeyboard());
  }
});

// /menu — только авторизованные. Остальным не отвечаем.
bot.command('menu', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  await ctx.reply(await renderMainMenuText(), mainMenuKeyboard());
});

// /start — главное меню для авторизованных. Остальным не отвечаем.
bot.command('start', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  await ctx.reply(await renderMainMenuText(), mainMenuKeyboard());
});

bot.action('menu:root', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  ctx.session.awaitingPingInput = false;
  await ctx.answerCbQuery();
  await safeEditOrReply(ctx, await renderMainMenuText(), mainMenuKeyboard());
});

bot.action('menu:ping', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  ctx.session.awaitingPingInput = true;
  ctx.session.pingRouterIndex = ctx.session.pingRouterIndex ?? 0;
  ctx.session.pingPortsIndex = ctx.session.pingPortsIndex ?? 0;

  try {
    const allRouters = await apiClient.listRouters();
    ctx.session.pingRouters = allRouters.filter((r) => r.status === 'online');

    const optionsLen = 2 + (ctx.session.pingRouters?.length ?? 0);
    if (optionsLen > 0 && (ctx.session.pingRouterIndex ?? 0) >= optionsLen) {
      ctx.session.pingRouterIndex = 0;
    }

    await safeEditOrReply(
      ctx,
      '💬 Введите IP/домен/подсеть (можно несколько, каждый с новой строки):',
      pingKeyboard(ctx.session)
    );
  } catch (err) {
    const errMsg = err instanceof Error ? err.message : String(err);
    await safeEditOrReply(ctx, `Ошибка:\n${errMsg}`, mainMenuKeyboard());
  }
});

bot.action('ping:toggle_router', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  if (!ctx.session.pingRouters) {
    try {
      const allRouters = await apiClient.listRouters();
      ctx.session.pingRouters = allRouters.filter((r) => r.status === 'online');
    } catch {
      ctx.session.pingRouters = [];
    }
  }

  const len = 2 + (ctx.session.pingRouters?.length ?? 0);
  const current = ctx.session.pingRouterIndex ?? 0;
  ctx.session.pingRouterIndex = len > 0 ? (current + 1) % len : 0;
  await safeEditOrReply(
    ctx,
    '💬 Введите IP/домен/подсеть (можно несколько, каждый с новой строки):',
    pingKeyboard(ctx.session)
  );
});

bot.action('ping:toggle_ports', async (ctx) => {
  if (!(await isAuthorizedUser(ctx))) return;
  await ctx.answerCbQuery();

  const len = pingPortsOptions.length;
  const current = ctx.session.pingPortsIndex ?? 0;
  ctx.session.pingPortsIndex = len > 0 ? (current + 1) % len : 0;
  await safeEditOrReply(
    ctx,
    '💬 Введите IP/домен/подсеть (можно несколько, каждый с новой строки):',
    pingKeyboard(ctx.session)
  );
});

startPeriodicHealthScheduler(bot);

bot.launch().catch((err) => {
  // Most common: 409 Conflict when another instance is already polling getUpdates
  console.error('Bot launch failed:', err);
  process.exitCode = 1;
});

process.once('SIGINT', () => bot.stop('SIGINT'));
process.once('SIGTERM', () => bot.stop('SIGTERM'));
