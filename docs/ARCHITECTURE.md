# Архитектура

Распределённый сервис проверки доступности IP/доменов из Туркменистана: центральный бекенд + произвольное количество узлов-агентов.

## Компоненты

- **Backend** — Debian-сервер, своя БД (Postgres), REST API. Отдаёт задания узлам (polling), принимает результаты, отдаёт данные наружу (внешним приложениям и будущей веб-морде).
- **Node agent** — бинарник на Go, крутится на машинах в Туркменистане. Опрашивает бекенд раз в ~30 сек, выполняет проверки напрямую к целям, шлёт результаты обратно.
- **Cloud Run fronting proxy** — опциональный слой перед бекендом на случай блокировки прямого доступа к Debian-серверу. Агент пробует Direct → фолбэк на Fronted.

## Доменная модель

```
accounts (1) ──< api_keys
accounts (1) ──< checks (1) ──< check_runs (1) ──< results
nodes (1) ──< check_runs
```

- **account** — владелец API-ключа (внешнее приложение или ты сам). С первого дня, даже если пока один аккаунт — чтобы не переделывать схему, когда появится второй потребитель API.
- **api_key** — токен доступа к публичному API. Привязан к account, со scope и возможностью отзыва.
- **node** — физический узел. Не привязан к account — узлы принадлежат платформе, а не конкретному API-потребителю.
- **check** — заявка на проверку (что проверяем, каким типом, на каких узлах). Один check может разойтись на N узлов.
- **check_run** — конкретный запуск check на конкретном node. Единица диспетчеризации и статуса.
- **result** — результат одного check_run. Отделён от check_run, чтобы можно было хранить как структурированную сводку (success, latency_ms), так и сырой protocol-specific JSON.

Разделение check → check_run → result — намеренное: это единственная структура, которая естественно поддерживает "проверить один таргет с 10 разных узлов/провайдеров и сравнить результаты" — основную ценность продукта — без миграций схемы при росте числа узлов.

### Таблицы (Postgres)

```sql
accounts (
  id, name, created_at
)

api_keys (
  id, account_id FK, key_hash, label,
  scopes text[], created_at, last_used_at, revoked_at
)

nodes (
  id, name, isp, city, country DEFAULT 'TM',
  agent_version, last_heartbeat_at,   -- обновляется на каждый POST /agent/poll
  secret_hash, tags jsonb, metadata jsonb,
  created_at
)
-- статус online/offline не хранится: online = last_heartbeat_at > now() - 3×poll_interval.
-- Вычисляется на чтении (в API-ответах и при dispatch) — нет риска "залипшего" статуса
-- из-за забытой фоновой джобы.

checks (
  id, account_id FK,
  batch_id uuid,              -- nullable; группирует checks, созданные одним POST /checks с targets[]
  type enum('ping','tcp','http','dns','tls','traceroute'),
  target text, params jsonb,
  node_selector jsonb,        -- {"node_ids":[...]}                              — явный выбор узла(ов),
                               --   check_run создаётся даже если узел сейчас offline (ждёт его возврата)
                               -- {"tags":["ashgabat"], "include_offline": false} — по умолчанию берёт
                               --   только online-узлы на момент создания; include_offline форсирует всех
                               -- {"all":true, "include_offline": false}         — та же логика для всего флота
  callback_url text,          -- опциональный вебхук по завершении
  status enum('pending','running','completed','partial','failed','cancelled'),
  created_at, completed_at
)

check_runs (
  id, check_id FK, node_id FK,
  status enum('queued','dispatched','running','done','error','timeout'),
  dispatched_at, completed_at
)
-- 'timeout' проставляется фоновым воркером бекенда (тикер каждые ~30с), если check_run
-- висит в queued/dispatched дольше грейс-периода (по умолчанию 10 мин) — например узел
-- выбран явно по node_ids, но не выходит на связь. Без этого check.status мог бы
-- зависнуть в 'pending' навсегда.

results (
  id, check_run_id FK,
  success bool, latency_ms int,
  status_code text, error_message text,
  raw jsonb,                  -- полный protocol-specific результат
  created_at
)
```

`node.tags`/`metadata` как jsonb — чтобы фильтровать узлы по провайдеру/городу без миграций, когда узлов станет больше.

Задел на будущее без переделки схемы: `monitors` (периодические check, генерирующие checks по расписанию) — checks уже не привязаны к тому, кто их создал (человек через API или планировщик), так что фича добавляется отдельной таблицей + воркером, без изменения текущих сущностей.

## API (REST, `/api/v1`, JSON)

### Публичный API — для внешних приложений и будущей веб-морды

Auth: `Authorization: Bearer <api_key>`

| Метод | Путь | Назначение |
|---|---|---|
| POST | `/checks` | создать проверку. `target` (один) либо `targets` (массив) — при массиве создаётся по одному check на таргет с общим `batch_id`, декартово произведение с `node_selector` |
| GET | `/checks` | список проверок (пагинация, фильтры по статусу/дате/target/`batch_id`) |
| GET | `/checks/{id}` | статус + результаты (с `?expand=runs` — детализация по узлам) |
| DELETE | `/checks/{id}` | отменить pending/running проверку |
| GET | `/nodes` | список узлов, статус, теги (провайдер/город) — чтобы выбирать конкретные узлы при создании check |
| GET | `/nodes/{id}` | детали/здоровье узла |
| POST | `/nodes` | завести новый узел (admin-scope; возвращает node_id + secret для конфига агента) |
| POST | `/accounts` | завести аккаунт (admin-scope) |
| GET | `/accounts` | список аккаунтов (admin-scope) |
| POST | `/accounts/{id}/api-keys` | выпустить api_key для аккаунта (admin-scope; токен отдаётся один раз, хранится только хэш) |
| GET | `/accounts/{id}/api-keys` | список ключей аккаунта, без самих токенов (admin-scope) |
| DELETE | `/api-keys/{id}` | отозвать ключ (admin-scope) |

Изначально управление api_key было отложено ("ты единственный оператор, добавится когда появится второй потребитель") и ключи заводились вручную через psql — оказалось неудобно уже на первом реальном разворачивании, так что admin-эндпоинты для account/api_key добавлены сразу вместо `POST /nodes`-подобного костыля. Тот же admin-scope (`ADMIN_TOKEN`), что и у `POST /nodes` — управление инфраструктурой одним общим оператором, не self-serve для внешних потребителей.

### Протокол агента — узел↔бекенд

Auth: секрет узла (не пересекается со scope публичных api_key)

| Метод | Путь | Назначение |
|---|---|---|
| POST | `/agent/poll` | heartbeat + получение новых check_runs за один вызов (не два отдельных эндпоинта), отдаёт массив, но не больше N штук за раз (напр. 50) — остаток разбирается на следующих опросах |
| POST | `/agent/results` | отправка результатов (батчем, если check_runs несколько) |

Всего 14 эндпоинтов v1 — покрывает полный цикл (заявка → диспетчеризация → результат → доступ извне) плюс управление доступом, без раздутия.

### Поток диспетчеризации и выбор узла

1. `POST /checks` резолвит `node_selector` в конкретный список `node_id` **в момент создания** (для `tags`/`all` — только online, если не указан `include_offline: true`; для `node_ids` — все указанные, независимо от статуса) и сразу создаёт `check_runs` в статусе `queued` для каждого.
2. Если среди явно указанных `node_ids` есть offline-узлы — check всё равно создаётся, но в ответе API возвращается `warnings: ["node X offline since ..."]`, чтобы вызывающая сторона сразу знала, а не гадала по факту таймаута.
3. Узел ничего не "получает" активно — на очередном `POST /agent/poll` бекенд находит `check_runs` со своим `node_id` и статусом `queued`, помечает их `dispatched` и отдаёт узлу в ответе.
4. Фоновый тикер переводит зависшие `queued`/`dispatched` check_runs в `timeout` по истечении грейс-периода (см. выше) — гарантирует, что `check.status` не виснет бесконечно, если узел так и не вышел на связь.

### Параметры по типам проверки (`checks.params`)

- `ping`: `{count, timeout_ms}`
- `tcp`: `{port, timeout_ms}`
- `http`: `{method, follow_redirects, timeout_ms, expect_status}`
- `dns`: `{record_type, resolver}`
- `tls`: `{port, verify_chain}`
- `traceroute`: `{max_hops, timeout_ms}`

## Структура Go-агента

```
cmd/agent/main.go       — точка входа: configure/install/start/stop/run через kardianos/service
internal/config/        — node_id, secret, backend URLs (direct+fronted), выбранный интерфейс, интервал
internal/netiface/      — список сетевых интерфейсов + их DNS-сервера (per-OS: linux/darwin/windows)
internal/transport/     — интерфейс Transport{Poll, PostResults} + direct.go, fronted.go
internal/checks/        — интерфейс Checker{Run(ctx, netCfg, target, params) Result}
                           + ping.go, tcp.go, http.go, dns.go, tls.go, traceroute.go
internal/poller/        — цикл опроса (джиттер, чтобы не синхронизировать все узлы),
                           воркер-пул для параллельного выполнения check_runs из одного ответа
```

Новый тип проверки = новый файл в `internal/checks/` + регистрация в мапе типов, без изменений в остальном коде — это и есть расширяемость, о которой просили ("хотим всё что только можно придумать").

### Привязка к сетевому интерфейсу и его DNS

Компьютер-узел часто имеет несколько сетевых интерфейсов и может быть за VPN с кастомным
DNS — если гонять проверки через дефолтный маршрут ОС и системный резолвер, результат
будет отражать не то, что реально видит провайдер в Туркменистане, а то, что видно через
VPN. Поэтому:

- `pingachock-agent configure` — интерактивно перечисляет интерфейсы (`internal/netiface.List`),
  оператор выбирает нужный; агент определяет DNS-сервера именно этого интерфейса
  (`internal/netiface.DNSServers`, per-OS: `resolvectl`/`resolv.conf` на Linux, `scutil --dns`
  на macOS, `ipconfig /all` на Windows) и сохраняет выбор в `agent.json`
  (`interface_name`, `local_addr`, `dns_servers`).
- Всё это превращается в `checks.NetConfig{LocalAddr, Resolver}`, который прокидывается в
  каждый `Checker.Run` через `poller.Poller` — tcp/http биндят исходящее соединение через
  `net.Dialer.LocalAddr` и резолвят через `net.Dialer.Resolver`; `ping` (шеллаут в системный
  `ping`, который сам резолвит через системный DNS) — сначала резолвится вручную через
  `netCfg.Resolver`, а системному `ping` передаётся уже IP плюс флаг `-S`/`-I` с адресом
  интерфейса.
- Транспорт узел↔бекенд (`internal/transport`) тоже биндится на выбранный интерфейс —
  весь трафик агента идёт через один и тот же путь, не только тестовый.
- Если оператор не запускал `configure` (или интерфейс определить не удалось) — `LocalAddr`/
  `Resolver` остаются `nil`, и поведение не отличается от системного умолчания (fail-safe,
  не ломает работу агента, просто не даёт гарантии "видим ровно то, что видит ISP").

Деплой — один статический бинарник + systemd unit (Linux) / launchd plist (macOS) / Windows Service (`kardianos/service`), простой install-скрипт под каждую ОС.

## Структура бекенда

```
cmd/server/main.go
internal/api/public/    — обработчики публичного API
internal/api/agent/     — обработчики протокола агента
internal/store/         — репозитории (checks, nodes, results, api_keys)
internal/dispatch/      — node_selector → список node_id (с учётом online/include_offline),
                           создание check_runs, агрегация статуса check по его check_runs
internal/auth/          — валидация api_key и node secret (middleware)
internal/sweeper/       — фоновый тикер: queued/dispatched check_runs старше грейс-периода → timeout
```

Бекенд полностью stateless (polling, не WebSocket) — можно поднять несколько инстансов за балансировщиком позже без sticky-сессий, когда нагрузка вырастет.

## Почему это масштабируется

1. **check → check_run → result** — фан-аут на N узлов без изменения схемы.
2. **node.tags/metadata jsonb** — фильтрация по провайдеру/городу без миграций.
3. **accounts/api_keys с первого дня** — второй внешний потребитель API = новая строка, не рефакторинг.
4. **callback_url на check** — внешние приложения могут не поллить тебя, а получать вебхук.
5. **Checker interface в агенте** — новый тип проверки не трогает остальной код.
6. **Stateless backend** — горизонтальное масштабирование без изменения архитектуры.
