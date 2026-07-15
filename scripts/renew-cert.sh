#!/bin/sh
# Manual TLS cert renewal: port 80 belongs to another site on this server
# most of the time, so docker-compose.prod.yml doesn't publish it. This
# temporarily adds it back (docker-compose.renew.yml) so Caddy can do the
# ACME HTTP-01 challenge, then removes it again. Run this yourself when the
# cert is actually getting close to expiry - see the openssl check in
# README ("Ручное продление сертификата").
set -eu

cd "$(dirname "$0")/.."

echo "Открываю порт 80 для продления - убедись, что он сейчас реально свободен"
echo "(останови то, что на нём висит, если что-то есть)."
echo
docker compose -f docker-compose.prod.yml -f docker-compose.renew.yml up -d caddy

echo
echo "Жду, пока Caddy попробует продлить сертификат..."
sleep 10
docker compose -f docker-compose.prod.yml logs caddy --tail 30

echo
echo "Смотри в логах выше на строку про успешное продление. Если сертификат ещё"
echo "не в окне продления (обычно ~30 дней до истечения) - Caddy просто ничего"
echo "не сделает, это нормально, тогда пробовать рано."
echo
printf "Нажми Enter, чтобы закрыть порт 80 обратно (освободить его под твой сайт)... "
read -r _

docker compose -f docker-compose.prod.yml up -d caddy
echo "Порт 80 освобождён, Caddy снова слушает только 30031."
