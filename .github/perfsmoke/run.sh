#!/usr/bin/env bash
# Builds .github/perfsmoke against BASE and HEAD and runs the two binaries
# by turns, ROUNDS times each, alternating which goes first; compare.py
# then fails when HEAD is slower than BASE beyond the threshold on any
# workload. The driver source is always HEAD's, so both builds time the
# same workloads.
#
# usage: run.sh BASE HEAD [ROUNDS]
#   PERF_THRESHOLD  ratio that fails (default 1.30)
#   PERF_WAIVED     "1" turns a failure into a warning
set -euo pipefail
base=$1
head=$2
rounds=${3:-9}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
work=$(mktemp -d)
cleanup() {
	git -C "$repo" worktree remove --force "$work/src-base" 2>/dev/null || true
	git -C "$repo" worktree remove --force "$work/src-head" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

build() { # build <sha> <name>
	local src=$work/src-$2 mod=$work/mod-$2
	git -C "$repo" worktree add -q --detach "$src" "$1"
	mkdir -p "$mod"
	cp "$here/main.go" "$mod/"
	cat > "$mod/go.mod" <<MOD
module perfsmoke

go 1.26

require github.com/Deln0r/ygo v0.0.0

replace github.com/Deln0r/ygo => $src
MOD
	(cd "$mod" && go mod tidy >/dev/null && go build -o "$work/$2" .)
}
build "$base" base
build "$head" head
echo "perf smoke: base $(git -C "$repo" rev-parse --short "$base"), head $(git -C "$repo" rev-parse --short "$head"), $rounds rounds"
# Each workload runs in a process of its own, base and head back to back,
# the order alternating by round, so drift on the machine hits both sides
# of a pair alike.
for r in $(seq 1 "$rounds"); do
	if (( r % 2 )); then order="base head"; else order="head base"; fi
	for w in w1 w2 w3 w4 w5; do
		for b in $order; do
			GOMAXPROCS=2 GOGC=100 "$work/$b" -only "$w" | sed "s/^/$b /" >> "$work/results"
		done
	done
done
python3 "$here/compare.py" "$work/results" "${PERF_THRESHOLD:-1.30}" "${PERF_WAIVED:-0}"
