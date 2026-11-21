#!/bin/sh
# Writes go.work (git-ignored) for working on the core and the modules that
# require it at a version not yet published: persist/sqlite, gomobile, the
# two server commands and the two SQLite examples carry no replace
# directives (a command module with one cannot be `go install`ed), so
# outside a workspace they build against the published core. The NATS and
# Matrix adapters are left out: they carry their own replace directives,
# and a workspace module may not be replaced; build them with GOWORK=off.
# Checks of the core's Go floor run with GOWORK=off GOTOOLCHAIN=local too.
set -e
cd "$(dirname "$0")/.."
rm -f go.work go.work.sum
go work init . ./persist/sqlite ./gomobile ./cmd/yserve ./cmd/ygo-server ./examples/collab-server ./examples/offline-first
# The nested modules require the core (and persist/sqlite) at the version
# being released, which may not be published yet; the go command reads a
# required version's go.mod even for a workspace module, so that version
# is replaced by the checkout too.
# Every version any nested module requires, as each may be the unpublished
# one.
for v in $(awk '$1 == "github.com/Deln0r/ygo" { print $2 }' persist/sqlite/go.mod gomobile/go.mod cmd/*/go.mod examples/*/go.mod | sort -u); do
	go work edit -replace "github.com/Deln0r/ygo@$v=./"
done
for v in $(awk '$1 == "github.com/Deln0r/ygo/persist/sqlite" { print $2 }' gomobile/go.mod cmd/*/go.mod examples/*/go.mod | sort -u); do
	go work edit -replace "github.com/Deln0r/ygo/persist/sqlite@$v=./persist/sqlite"
done
