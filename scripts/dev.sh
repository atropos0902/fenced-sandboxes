#!/bin/sh
# Starts everything locally. Secrets are generated once into .dev/ and stay there.
set -eu
cd "$(dirname "$0")/.."
mkdir -p .dev
[ -f .dev/app.pem ] || openssl genrsa -traditional -out .dev/app.pem 2048 2>/dev/null || openssl genrsa -out .dev/app.pem 2048
chmod 644 .dev/app.pem
[ -f .dev/env ] || printf 'API_TOKEN=%s\nPREVIEW_KEY=%s\n' "$(openssl rand -hex 24)" "$(openssl rand -hex 32)" > .dev/env
export APP_KEY="$PWD/.dev/app.pem"
docker build -q -t fenced-sandbox:dev --target sandbox -f deploy/Dockerfile . >/dev/null
exec docker compose --env-file .dev/env -f deploy/compose.yaml "${@:-up}"
