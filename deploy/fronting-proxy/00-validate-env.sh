#!/bin/sh
# Runs before nginx's own envsubst step (20-envsubst-on-templates.sh) as part
# of the official nginx image's docker-entrypoint.d chain. Without this, a
# missing BACKEND_URL turns into `set $backend ;` (invalid nginx syntax) -
# nginx exits immediately, and Cloud Run's edge just serves its own generic
# 404 page with zero indication of why. This makes the real cause show up in
# `docker logs` / Cloud Run's log viewer instead.
set -e

if [ -z "$BACKEND_URL" ]; then
  echo "FATAL: BACKEND_URL is not set. Example: BACKEND_URL=https://backend.example.com (scheme+host, no trailing path)." >&2
  exit 1
fi

case "$BACKEND_URL" in
  http://*|https://*) ;;
  *)
    echo "FATAL: BACKEND_URL must start with http:// or https:// (got: $BACKEND_URL)" >&2
    exit 1
    ;;
esac

echo "BACKEND_URL is set to $BACKEND_URL"
