#!/bin/sh
# Checks the module layout that keeps the core at Go 1.23 (CI runs it; run
# it after tools/go-work.sh):
#   - the modules built in the workspace carry no replace directive (a
#     command module with one cannot be `go install`ed);
#   - every nested module requires the core at v1.47.0 or later: v1.46.0
#     and before still hold persist/sqlite, gomobile and the commands, so
#     an older requirement allows `ambiguous import`;
#   - gomobile imports only the core's public API, so a later core MVS
#     pairs it with cannot break it;
#   - the core module's graph holds no SQLite.
set -e
cd "$(dirname "$0")/.."
split=v1.47.0
fail() { echo "check-modules: $*" >&2; exit 1; }
for d in persist/sqlite gomobile cmd/yserve cmd/ygo-server; do
	if grep -q '^replace' "$d/go.mod"; then fail "$d/go.mod has a replace directive"; fi
done
for d in persist/sqlite gomobile cmd/yserve cmd/ygo-server examples/collab-server examples/offline-first; do
	v=$(awk '$1 == "github.com/Deln0r/ygo" { print $2; exit }' "$d/go.mod")
	[ -n "$v" ] || fail "$d/go.mod does not require the core"
	low=$(printf '%s\n%s\n' "$v" "$split" | sort -V | head -1)
	[ "$low" = "$split" ] || fail "$d requires the core at $v, before $split"
done
if (cd gomobile && go list -f '{{join .Imports "\n"}}' .) | grep -q '^github.com/Deln0r/ygo/internal/'; then
	fail "gomobile imports a core internal package"
fi
if GOWORK=off go list -m all | grep -q '^modernc.org/sqlite '; then
	fail "the core module's graph holds modernc.org/sqlite"
fi
echo "check-modules: ok"
