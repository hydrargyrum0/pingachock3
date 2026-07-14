# pingachock

Распределённый сервис проверки доступности IP/доменов из Туркменистана.
Архитектура: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Локальный запуск (dev)

### 1. Поднять Postgres

```sh
docker compose up -d
```

Поднимается на `localhost:5433` (не 5432 - чтобы не конфликтовать с локальным Postgres, если он уже установлен).

### 2. Запустить сервер

Миграции из `migrations/` применяются автоматически при старте.

```sh
DATABASE_URL="postgres://pingachock:pingachock@localhost:5433/pingachock?sslmode=disable" \
ADMIN_TOKEN="дай-любой-секрет-для-локальной-разработки" \
go run ./cmd/server
```

Переменные окружения сервера (все опциональны, кроме `ADMIN_TOKEN` для `POST /nodes`):

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `DATABASE_URL` | `postgres://pingachock:pingachock@localhost:5433/pingachock?sslmode=disable` | DSN Postgres |
| `LISTEN_ADDR` | `:8080` | адрес, на котором слушает HTTP |
| `MIGRATIONS_DIR` | `./migrations` | путь к SQL-миграциям |
| `ADMIN_TOKEN` | *(пусто)* | токен для `POST /api/v1/nodes` |
| `NODE_ONLINE_THRESHOLD_SECONDS` | `90` | после какого простоя узел считается offline |
| `POLL_BATCH_LIMIT` | `50` | макс. заданий за один `/agent/poll` |
| `SWEEP_INTERVAL_SECONDS` | `30` | как часто проверяются зависшие check_runs |
| `SWEEP_GRACE_SECONDS` | `600` | через сколько зависший check_run таймаутится |

### 3. Завести тестовый аккаунт и API-ключ

Пока нет self-serve эндпоинта для ключей (см. ARCHITECTURE.md) - создаются вручную:

```sh
psql "postgres://pingachock:pingachock@localhost:5433/pingachock?sslmode=disable" -c \
  "INSERT INTO accounts (name) VALUES ('my-account') RETURNING id;"

# hash = sha256(token), храним только хэш
TOKEN="мой-секретный-ключ"
HASH=$(printf '%s' "$TOKEN" | shasum -a 256 | awk '{print $1}')
psql "postgres://pingachock:pingachock@localhost:5433/pingachock?sslmode=disable" -c \
  "INSERT INTO api_keys (account_id, key_hash, label) VALUES ('<account_id>', '$HASH', 'dev');"
```

### 4. Завести узел

```sh
curl -X POST http://localhost:8080/api/v1/nodes \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"ashgabat-1","isp":"Altyn Asyr","city":"Ashgabat"}'
```

Ответ содержит `secret` (показывается один раз) - он идёт в конфиг агента.

### 5. Настроить и запустить агент

Агент - сам себе установщик, рассчитан на "ПКМ → Запуск от имени администратора" без
предварительных танцев с терминалом.

**Самый простой путь** - просто запустить бинарник без аргументов (двойной клик на Windows,
или ПКМ → "Запуск от имени администратора"): спросит сетевой интерфейс (важно, если на
машине несколько интерфейсов и/или поднят VPN - иначе можно случайно тестировать не то,
что реально видит провайдер), определит DNS именно этого интерфейса, спросит
`node_secret`/`direct_url` - и сам установит и запустит себя как системный сервис. Окно
консоли не закроется само, ждёт Enter, чтобы результат было видно.

```sh
./pingachock-agent
# то же самое явно:
./pingachock-agent setup
```

**Для массовой раскатки на несколько машин** - все ответы можно забить прямо в командную
строку (например, в "Объект" ярлыка), тогда `setup` не спросит вообще ничего:

```sh
./pingachock-agent setup -node-secret=<secret> -direct-url=https://pingachock.rapeer.com:30031 -interface=auto
```

`-interface=auto` берёт первый интерфейс со статусом "up"; можно указать конкретное имя
(увидишь список, если запустить без `-interface`).

Отдельные команды, если нужно управлять сервисом руками:

```sh
./pingachock-agent configure        # только сохранить конфиг, не устанавливать сервис
./pingachock-agent install/start/stop/uninstall
./pingachock-agent run -config agent.json   # в форграунде, для отладки
```

Кросс-компиляция под все целевые ОС разом - `scripts/build-agent.sh`, бинарники в `bin/`.
На Windows это единственный `.exe`, ничего дополнительно ставить не нужно (`CGO_ENABLED=0`,
статическая линковка).

### 6. Создать проверку

```sh
curl -X POST http://localhost:8080/api/v1/checks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"type":"ping","target":"1.1.1.1","node_selector":{"all":true}}'
```

```sh
curl http://localhost:8080/api/v1/checks/<id>?expand=runs \
  -H "Authorization: Bearer $TOKEN"
```

Или через браузер: `http://localhost:8080/docs` - интерактивная документация (Swagger UI),
можно жать "Authorize" и дёргать эндпоинты через "Try it out" без curl.

## Продакшн-деплой бекенда (Docker, на своём сервере)

Всё контейнеризовано намеренно - сервер общий, чтобы ничего не задеть. Наружу торчит
только Caddy: порт `80` (только для выпуска/продления TLS-сертификата, см. ниже) и порт
`30031` (реальный трафик на `pingachock.rapeer.com`). Postgres и сам бекенд доступны
только внутри docker-сети, без портов на хосте.

```
docker-compose.prod.yml   — postgres + backend + caddy
Dockerfile                — образ бекенда (миграции применяются сами при старте)
Caddyfile                 — pingachock.rapeer.com:30031 → backend:8080
```

### Разворачивание

1. Скопировать репозиторий на сервер.
2. Завести `.env` из примера и заполнить случайными значениями:

   ```sh
   cp .env.example .env
   # POSTGRES_PASSWORD и ADMIN_TOKEN - например так:
   openssl rand -hex 32
   ```

3. Убедиться, что порты `80` и `30031` на сервере ничем не заняты (порт `80` - хотя бы на
   момент запуска, см. ниже про продление).

4. Поднять:

   ```sh
   docker compose -f docker-compose.prod.yml up -d --build
   ```

5. Проверить, что сертификат реально выпустился:

   ```sh
   docker compose -f docker-compose.prod.yml logs caddy -f
   ```

   Ищи строку про успешный `certificate obtained`. Если DNS `pingachock.rapeer.com` ещё не
   успел разъехаться или порт 80 занят - Caddy будет ретраить сам, ничего вручную дёргать
   не нужно.

6. Проверить снаружи:

   ```sh
   curl https://pingachock.rapeer.com:30031/healthz
   ```

   Интерактивная документация API (Swagger UI, с возможностью выполнять запросы прямо из
   браузера через "Try it out") - `https://pingachock.rapeer.com:30031/docs`.

7. Дальше — как в локальном разделе выше: завести аккаунт/api-ключ через `psql`
   (`docker compose -f docker-compose.prod.yml exec postgres psql -U pingachock -d pingachock`),
   завести узлы через `POST /api/v1/nodes` с `ADMIN_TOKEN`.

8. Если уже поднят Cloud Run fronting-прокси (см. ниже) - обновить его `BACKEND_URL` на
   `https://pingachock.rapeer.com:30031`.

### Важно: порт 80 нужен не только один раз

Let's Encrypt сертификаты живут ~90 дней, Caddy продлевает их автоматически заранее — но
ему для этого снова понадобится на секунду порт 80 (HTTP-01 challenge), причём периодически,
не разово. Если после первого запуска порт 80 на сервере займёт что-то другое — продление
молча не пройдёт, и через ~90 дней HTTPS отвалится без явной ошибки в моменте. Порт 80 на
этом сервере стоит держать зарезервированным за Caddy на постоянной основе, не только на
момент первого деплоя.

## Fronting-прокси на Cloud Run

`deploy/fronting-proxy/` - реверс-прокси для узлов, у которых заблокирован прямой доступ к
Debian-серверу (см. docs/ARCHITECTURE.md, "Dual connectivity"). Тот же паттерн, что и в
твоих `novavpn_backend_proxy_gcp`/`vless-reverse` - стоковый nginx + один envsubst-шаблон,
без своего бинарника. Отличия под наш кейс: без WebSocket (мы на polling), и проксирует
только `/api/v1/agent/*` + `/healthz` - остальное 404 (публичный API и веб-морда сюда не
ходят, у них нет нужды в фронтинге).

Локальная проверка:

```sh
docker build -t pingachock-fronting-proxy ./deploy/fronting-proxy
docker run -d -e PORT=8080 -e BACKEND_URL=http://<ip-бекенда>:8089 -p 8091:8080 pingachock-fronting-proxy
curl http://localhost:8091/healthz
```

Деплой на Cloud Run:

```sh
gcloud run deploy pingachock-fronting-proxy \
  --source ./deploy/fronting-proxy \
  --allow-unauthenticated \
  --set-env-vars BACKEND_URL=https://<домен-debian-сервера>
```

`PORT` подставляет сам Cloud Run - руками не задавать. В конфиге узла (`agent.json`)
после этого прописать `front_domain` (домен для маскировки SNI) и `front_real_host`
(хостнейм этого Cloud Run сервиса) - агент использует их как fallback, если `direct_url`
недоступен (см. `internal/transport/fronted.go`).

## Структура

```
cmd/server/     - точка входа бекенда
cmd/agent/      - точка входа узла
internal/store/ - слой БД (Postgres)
internal/api/   - HTTP-хендлеры (public + agent)
internal/auth/  - api_key / node secret / admin token
internal/dispatch/ - резолв node_selector
internal/sweeper/  - таймаут зависших check_runs
internal/transport/ - Direct/Fronted транспорт узла
internal/checks/    - реализации проверок (ping/tcp/http/dns)
internal/poller/    - цикл опроса узла
internal/config/    - конфиг агента
migrations/     - SQL-миграции
docs/ARCHITECTURE.md - архитектура и решения
```

## Поддержанные типы проверок

`ping`, `tcp`, `http`, `dns` - реализованы в `internal/checks/`.
`tls`, `traceroute` - в схеме БД зарезервированы, агент их пока не умеет
(при получении такого задания вернёт явную ошибку "check type not supported").
