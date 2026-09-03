#!/usr/bin/env bash
# Bring up Dendrite and wait for the client API to answer. Used by the
# integration test, the demo and CI, so all three wait the same way.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docker compose -f "$here/docker-compose.yml" up -d --wait 2>/dev/null || docker compose -f "$here/docker-compose.yml" up -d
for i in $(seq 1 90); do
  if curl -fsS "http://localhost:${YGO_MATRIX_PORT:-8008}/_matrix/client/versions" >/dev/null 2>&1; then
    echo "dendrite ready after ${i}s on port ${YGO_MATRIX_PORT:-8008}"
    exit 0
  fi
  sleep 1
done
echo "dendrite did not become ready" >&2
docker compose -f "$here/docker-compose.yml" logs --tail=40 >&2
exit 1
