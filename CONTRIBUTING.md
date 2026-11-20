# Contributing to ygo

Thanks for your interest. This document explains how to contribute.

## Developer Certificate of Origin (DCO)

All commits must be signed off under the [Developer Certificate of Origin](https://developercertificate.org/). This is **not** a CLA. It is a per-commit attestation that you have the right to contribute the code under the project's license.

Add `Signed-off-by: Your Name <your.email@example.com>` to each commit:

```bash
git commit -s -m "your message"
```

The `-s` flag adds the trailer automatically using your `git config user.name` and `user.email`.

## Code style

- Follow standard Go conventions (`gofmt`, `golangci-lint`).
- Keep public APIs minimal. Prefer adding methods over exposing fields.
- Document exported types/functions with `// PackagePrefix ...` comments.
- Tests required for any non-trivial change.

## Wire format compatibility

**Binary protocol compatibility with [yjs](https://github.com/yjs/yjs) v13.x V1 update format is non-negotiable.** Any change that breaks round-trip with the JS reference is a regression even if all Go tests pass.

If your change touches encoding/decoding paths:
1. Add a fixture from JS Yjs in `testdata/`.
2. Verify round-trip in Go.
3. Document any divergence (there should not be any).

## Filing issues

- Search existing issues first.
- Include `yjs` JS reproduction code when reporting protocol divergence.
- Include Go version, `ygo` version (commit SHA), and OS.

## Pull requests

- One logical change per PR.
- Reference any related issue.
- Run `make check` before submitting (gofmt, vet, the race suite and lint in every module).
- The core module (Go 1.23) does not contain the SQLite store and its users: `persist/sqlite`, `gomobile`, `cmd/yserve` and `cmd/ygo-server` are modules of their own (Go 1.26) that require the core at the version being released. Build them against your checkout in a workspace: `./tools/go-work.sh` writes a git-ignored `go.work`. The examples that use SQLite and the NATS and Matrix adapters build through their own `replace` directives (`GOWORK=off`). Check the core's floor with `GOWORK=off GOTOOLCHAIN=local` on Go 1.23.
- Be patient with review (best-effort, response within 14 days).

## Communication

- GitHub Discussions for design questions and Q&A.
- GitHub Issues for bugs and concrete proposals.
- Mention `@Deln0r` for maintainer attention.

## License

By contributing, you agree that your contributions will be licensed under the MIT License (see [LICENSE](LICENSE)).
