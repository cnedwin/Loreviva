# Loreviva

**The AI Platform for Living Worlds.**

Loreviva lets creators compose world rules, characters, roles, skills, knowledge
and model routes into interactive content that keeps developing, and lets clients
connect through their own applications.

> **Status: development snapshot, not a release.** Tasks 1–7 are implemented and
> reviewed; Task 8 integration work is in progress; R0 is not accepted. Only the
> synthetic Mock model and Mock billing are enabled. Working contract **1.6.0
> (67 operations / 183 schemas)**; approved baseline **1.4.0 (58 / 156)**. An
> operation declared in the contract is not evidence that a handler is mounted,
> and publishing source is not publishing a product.
>
> 中文说明见 [README.zh-CN.md](README.zh-CN.md)。

## What exists today

| State | Capability |
| --- | --- |
| Implemented | Single application API/worker, durable tasks, idempotent commands, commit receipts, current-authority checks |
| Implemented | Drafts, revisions, impact checks, publish preflight, immutable releases with pinned dependencies and model routes |
| Implemented | Private file upload, authorised download, recoverable cleanup |
| Implemented | World and actor instances, start/pause/resume/end, declarative action validation, compatible content migration |
| Implemented | Scoped memory, sources and acquisition, private relationship boundaries, use policy, correction and deletion |
| Implemented | Bounded Mock harness, original-attempt recovery, candidate drafts, isolated rehearsals |
| Implemented | Native Vue Studio, limited admin client, local settings, member and self-consent entry points |
| Implemented | `.wepkg` static preflight: container, manifest, integrity, references, rights, primitives, deterministic report |
| Planned | Package import (isolated draft), rehearsal, local publish, export |
| Planned | Metering, complete backup/restore, real model and hosted billing validation |

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/`, `internal/` | Go application, store, workers, harness |
| `contracts/source/` | Single source of truth: `openapi.yaml`, `wepkg.schema.json` |
| `contracts/generated/`, `internal/generated/`, `packages/sdk-ts/src/generated/` | Generated artefacts, never hand-edited |
| `migrations/` | Embedded numbered migrations |
| `apps/studio/` | Native Vue Studio |
| `tests/` | Contract, SDK and integration suites |
| `docs/design/en/` | Product design, English (primary); Chinese beside it |
| `docs/en/`, `docs/` | Operations and integration docs, English and Chinese |
| `scripts/` | Generation, byte policy and verification tooling |

## Quick start

Requirements: **Go 1.27.2**, **MySQL Community 9.x** on loopback, **Node.js 24**,
**pnpm 11.25.0**.

```bash
pnpm install --frozen-lockfile
pnpm generate:contracts          # must produce zero diff
go build ./...
```

Integration tests need a real, disposable MySQL through `LOREVIVA_TEST_DSN`; each
test creates and drops its own randomly named database. See
[docs/en/04-setup-and-integration.md](docs/en/04-setup-and-integration.md).

Verification ladder — run the smallest gate that can catch your change:

```bash
go build ./... && go vet ./... && node scripts/check-generated.mjs   # seconds
go test ./internal/...                                               # unit
go test ./tests/integration -run '<affected targets>'                # minutes
node scripts/run-integration-shards.mjs --shards 4                   # full gate, ~80 min
```

The sharded runner changes nothing about the assertions: every integration test
opens its own database, so shards share nothing but the server. Failures are
retried serially and reported separately, so a load-sensitive result is never
silently counted as a pass.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md), [GOVERNANCE.md](GOVERNANCE.md) and
[VERSIONING.md](VERSIONING.md) first. Interface changes start in
`contracts/source/`; generated files are committed only after regeneration.
Security reports follow [SECURITY.md](SECURITY.md).

## Licence

Original code is [Apache-2.0](LICENSE). Third-party material keeps its own
licence; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
