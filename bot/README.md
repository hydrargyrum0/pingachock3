## Pingachock Telegram Bot

### Запуск локально
- Установи Node.js 20+
- Создай `.env` по примеру `.env.example`
- Установи зависимости: `npm install`
- Dev режим: `npm run dev`

### Запуск в Docker
Бот — один из сервисов корневого `docker-compose.prod.yml` (см. `README.md` в корне репозитория,
раздел "Продакшн-деплой"). `TELEGRAM_BOT_TOKEN`/`TELEGRAM_BOT_ADMIN_ID` задаются в `.env` в корне
репозитория, не в `bot/.env`.

Примечание: у Telegram long-polling может работать только в одном экземпляре — не запускай бота
одновременно локально и в Docker.

### Переменные окружения
- `telegram_bot_token` — токен бота
- `telegram_bot_admin_id` — chat id администратора(ов) (для `/admin`), можно несколько через запятую
- `DB_PATH` (опционально) — путь к файлу базы пользователей (по умолчанию `./data/users.db`)
- `SETTINGS_DB_PATH` (опционально) — путь к файлу настроек (по умолчанию `./data/settings.db`)

API URL, admin_token и api_key бекенда pingachock настраиваются не через env, а через `/admin` в
самом боте (после первого запуска, только для `telegram_bot_admin_id`):
- **API URL** — адрес бекенда pingachock, например `https://pingachock.rapeer.com:30031/`
  (со слэшем в конце)
- **admin_token** — тот же `ADMIN_TOKEN`, что и на бекенде: управление узлами/аккаунтами
  (создание, блокировка роутера)
- **api_key** — ключ для пинга/проверок (`POST /accounts/{id}/api-keys` на бекенде), один общий
  ключ на весь бот, не per-user
