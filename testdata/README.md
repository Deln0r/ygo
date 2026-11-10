# testdata

Captured fixtures from the JavaScript Yjs and lib0 reference implementations.
ygo's tests assert byte-for-byte compatibility against these.

## Layout

- `lib0.json` — fixture cases for `internal/lib0` varint/varstring/varbuffer/float encoding.
- `any-value-fixtures.json` — documents from yjs holding undefined, BigInt and -0 (`gen/gen-any-values.mjs`); values are stored in a tagged form, since JSON cannot hold them.
- (planned) `yjs/*.bin` — captured V1 update streams for `Y.Map`, `Y.Array`, `Y.Text` operation sequences.
- (planned) `yjs/*.json` — sidecar metadata describing each `.bin` (operation log, expected final state).

## Regenerating fixtures

Fixtures are checked into the repo so a fresh clone runs tests without Node.js.
To regenerate after updating the JS reference version:

```bash
cd testdata/gen
npm install
node gen-lib0.mjs
```

CI runs the regeneration on every PR and asserts `git diff --exit-code testdata/`
to catch silent format drift.

## Versioning

Each fixture file's top-level `generator` field records the JS package version
(e.g. `lib0@0.2.117`, `yjs@13.6.31`). Bump these in `testdata/gen/package.json`
and regenerate when adopting a new upstream release.
