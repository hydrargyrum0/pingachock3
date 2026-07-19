## Pingachock Telegram Bot

### Запуск локально
- Установи Node.js 20+
- Создай `.env` по примеру `.env.example`
- Установи зависимости: `npm install`
- Dev режим: `npm run dev`

### Запуск в Docker
- Проверь, что `.env` лежит рядом с `docker-compose.yml`
- `docker compose up -d --build`

Примечание: у Telegram long-polling может работать только в одном экземпляре — не запускай бота одновременно локально и в Docker.

### Переменные окружения
- `telegram_bot_token` — токен бота
- `telegram_bot_admin_id` — chat id администратора(ов) (для `/admin`), можно несколько через запятую
- `DB_PATH` (опционально) — путь к файлу базы пользователей (по умолчанию `./data/users.db`)
