# Setup and Integration Guide

Loreviva — The AI Platform for Living Worlds.

This guide applies to the locally approved Tasks 1–7 synthetic Mock baseline and introduces the CLI, native Studio, and administrative APIs. It is not installation acceptance for an actual public copy or a tutorial for complete customer player integration, real AI, or production hosting. Public documents are curated distribution views of the current specifications.

## Prepare the environment

- Go 1.27.2, MySQL Community LTS / InnoDB.
- Node.js 24 and pnpm 11.25.0 for contract generation and TypeScript development checks.
- A restricted local development database account, plus separate private file and governance record directories.

Kratos 3 is pinned by [go.mod](../../go.mod). The Go application serves native Vue Studio at `/studio/`; `/` retains its startup-inspection role. After frontend changes, regenerate the real embedded assets before building the Go application; container deployment and complete recovery remain outside the completed scope.

## Configuration

Refer to [.env.example](../../.env.example) and set variables in the startup process environment. The application **does not automatically load `.env`**. Secrets resolve through explicit `env:` references; do not commit passwords, Operator tokens, or test connection strings to the repository.

| Variable | Configuration meaning |
| --- | --- |
| `LOREVIVA_MODE` | `all` / `api` / `worker`, default `all` |
| `LOREVIVA_HTTP_ADDR` | HTTP listening address, defaults to local port 8080 |
| `LOREVIVA_DB_ADDR` / `LOREVIVA_DB_NAME` / `LOREVIVA_DB_USER` | Database address, name, and account; all required |
| `LOREVIVA_DB_PASSWORD_REF` | For example, `env:LOREVIVA_DB_PASSWORD`; the environment variable's value must be provided privately |
| `LOREVIVA_FILES_ROOT` | Private file root; do not place it in a public static directory |
| `LOREVIVA_GOVERNANCE_ROOT` | Independent governance record root, derived from the file root by default |
| `LOREVIVA_MODEL_MODE` / `LOREVIVA_BILLING_MODE` | Currently only `mock` is allowed |
| `LOREVIVA_PUBLIC_BASE_URL` | HTTP(S) base address without embedded credentials |
| `LOREVIVA_OPERATOR_TENANT` / `LOREVIVA_OPERATOR_PRODUCT` / `LOREVIVA_OPERATOR_SUBJECT` / `LOREVIVA_OPERATOR_CREDENTIAL_ID` | Operator scope and registered identity; the example uses demo-local/coffee/creator/owner |
| `LOREVIVA_OPERATOR_SECRET_REF` | Explicit secret reference resolving to a 32–512 byte token; preserve the same private credential across restarts |

Configuration rules are based on [config.go](../../internal/config/config.go). The database name must pass the existing local scope validation; initial development may use the template's `loreviva_dev`. There is no default administrator password or secret-reading endpoint.

## New synthetic installation

To rebuild Studio from source, first run from the repository root:

```powershell
pnpm install --frozen-lockfile
pnpm build:studio
pnpm build:server ./cmd/loreviva
```

Run from the repository root:

```powershell
go run ./cmd/loreviva migrate plan
go run ./cmd/loreviva migrate up --plan-id <plan返回的plan_id>
go run ./cmd/loreviva migrate status
go run ./cmd/loreviva bootstrap --mock --tenant demo-local
go run ./cmd/loreviva seed coffee --mock --tenant demo-local
go run ./cmd/loreviva --mode all
```

Migrations can apply only the current pinned plan. Existing installations reject ordinary `migrate up`; do not treat these new-installation commands as upgrade/recovery tools.

`bootstrap` explicitly registers credential hashes, scoped grants, and initial governance records for a new synthetic installation. It does not overwrite existing credentials or invent missing watermarks for restored data. Lost credentials or missing governance records need a subsequent audited recovery workflow; re-running bootstrap cannot bypass checks.

The coffee shop seed idempotently creates a project, draft, and release. It defines Alex, Mia, and the wooden sign custody rule without creating runtime instances, sessions, or real supplier calls. Re-running it should return the original content identity.

Use `/health`, `/version`, `/ready`, and `/` to inspect startup status. Readiness does not establish a complete authoring workflow, real model quality, or production capacity. `--mode api` and `--mode worker` can run in separate processes, sharing the same database and domain code.

## Studio and local settings

Open `/studio/` with a privately provided, registered Operator credential. Studio keeps the current bearer only in page memory, so reloading requires login; there is no default administrator password. Save/cancel, association-impact confirmation, optional Mock assistance, checks, and publication reuse actual administrative services; adopting a candidate still requires explicit saving.

`/studio/settings` provides local settings, member actions, and a separate self-consent entry point; see the seven [settings routes](03-architecture.md#contracts-and-extensions). These actions still check current eligibility; ordinary settings do not depend on model-provider readiness. Member actions grant neither private body access nor model use rights, and notification preferences send no external messages. Initializing local administration in an existing installation must explicitly use its configured owning credential and cannot create a new owner; complete upgrade/recovery releases remain unaccepted.

## Existing administrative APIs

Requests use `Authorization: Bearer <私下解析的Operator token>` and provide `product_id` according to the contract; the server checks current registered identity and action eligibility. Mutation requests use `Idempotency-Key`; relevant save/control/migration requests also require the expected version or preview binding from the original contract.

Understand existing calls in this order:

1. Create or query a draft, save content revisions, and inspect association impacts and precise validation issues.
2. Submit a release after publication preflight; publication rechecks revisions, dependencies, and rights.
3. Create an instance from the pinned release, then submit control commands such as START.
4. Query original registered tasks and instances, and read actor and memory metadata within permitted scope.
5. Where needed, perform compatible migration, memory correction/deletion, or use-policy governance and query each completion status.

Methods, request bodies, and responses are governed by [OpenAPI](../../contracts/source/openapi.yaml) and [actual route owners](03-architecture.md#http-routes-actually-connected). Draft `generation-resolution` reads the current resolution result. `rehearsals` and `candidates` return 202 and start persistent tasks when their Mock owners/dependencies are ready and eligibility, source, route, and input validation pass; missing dependencies are still rejected. 202 does not mean generation or rehearsal is complete; query results, cost verification, and cleanup progress through the original task. Candidates do not automatically save drafts, and rehearsals do not rewrite official worlds. Safe task lists and result bodies have separate eligibility; other reserved client contracts do not imply existing handlers.

Private uploads use multipart, with complete `metadata` followed by `file`; size, hash, type, and content are checked. Downloads use the authenticated content endpoint without returning storage paths. The existing cleanup command is:

```powershell
go run ./cmd/loreviva maintenance cleanup-files --key <稳定幂等键>
```

This task only handles eligible registered staging files; it is not a general file deletion tool. The Worker also handles world control, instance migration, memory governance, and Mock TURN, candidates, and isolated rehearsals when their corresponding owners are ready.

## Retry, pagination, and governance

When a commit result is unclear, query/replay with the original key and input; the same key with different input conflicts, and current authorization is still rechecked. 202 means acceptance, not completion of a world action, migration, or all cleanup; cancelling a task does not promise rollback of established facts or withdrawal of delivered content.

Draft, revision, and release lists use `limit=1..100` and opaque cursors. Subsequent queries keep the original filters and page size; if a cursor expires, clear it and query from the first page. Related instance lists also support pagination; check exact parameters against the contract and handlers rather than interpreting internal sequence numbers yourself.

Memory metadata, private bodies, governance, and use policies have separate eligibility. Querying backend tasks does not grant private body access or model use rights. Recall of copies already delivered cannot be promised; complete backup/recovery remains to be implemented.

## Next steps for customer integration

The [TypeScript package](../../packages/sdk-ts/src/index.ts) exports administrative/integration clients, prepared commands, BFF recovery, media and SSE types. The [thin backend](../../examples/thin-backend/) and [reference client](../../apps/reference-client/) provide reference integration. See [Quickstart](../integration/Quickstart.en.md) for prerequisites. Never send Operator credentials to players. Whole Task8/R0 remain unaccepted.

The planned Integration Kit will provide restricted sessions, interaction, snapshot/event recovery, media updates, a reference client, and thin backend adapters. Customer backends remain responsible for player identities, business eligibility, industry rules, wallets/orders/payments/refunds, and receipts from original business services; clients do not hold Operator or model secrets.

Real models, SaaS metering management, and billing switches are not yet connected. The current Mock publication cannot be labeled real AI or a paid production service.

## Development checks

```powershell
pnpm install --frozen-lockfile
pnpm generate:contracts
pnpm test
go test ./...
```

Contract sources are maintained in `contracts/source/`; the working contract is 1.6.0 / 67 operations / 181 schemas (the approved baseline remains 1.4.0 / 58 / 156). Generated files must not be manually turned into a second source. The original 18 operations/43 DTO and source1.2 49 operations/123 Schema fixtures in `tests/fixtures/` retain their respective historical comparison meanings; source1.3 is 51/131. Root `pnpm test` covers contract/SDK scripts and includes Studio contract and CSP checks; Studio unit tests, Vue type checking, and the production build use the separate commands below, not an old check total as a substitute for actual results. Embedded migrations are 0001–0008; complete product installation/upgrade releases remain unaccepted.

```powershell
pnpm --dir apps/studio test
pnpm --dir apps/studio typecheck
pnpm build:studio
```

`pnpm build:studio` runs the Vue type check and production build, then records complete source inputs and output bytes. `pnpm check:studio` performs a read-only comparison of Studio, SDK, contracts, lockfile, build configuration and embedded files; added, removed or changed bytes require a rebuild. `pnpm build:server ./cmd/loreviva` checks these inputs before compiling the server. Plain `go build` only compiles the supplied embedded bytes and does not prove frontend source freshness. The server separately validates the embedded contract and file integrity, returning 503 for an invalid artifact.

`go test ./...` does not automatically enable browser fixtures. `TestStudioBrowser` and `TestStudioSettingsEntryBrowser` require explicit `LOREVIVA_STUDIO_BROWSER=1`, verified `LOREVIVA_TEST_NODE` and `LOREVIVA_TEST_PNPM`, and a private `LOREVIVA_STUDIO_EVIDENCE` output location. Without opt-in they SKIP, which is not browser success. The fixtures start actual local services with synthetic data and run the pinned Playwright projects; invoking `pnpm test:e2e` alone cannot replace those service and credential prerequisites. Controlled browser success is not human or real-model acceptance.

These commands define verification entry points; each result is bound to its source and artifact bytes. A successful build does not establish database, browser or complete release acceptance. Actual database verification uses an exclusive restricted window and retains prerequisite failures and unrun results. Consult the verification record for the corresponding version's accepted scope.

Database integration tests require TCP `127.0.0.1:33267` and a non-root account. A privately provided `LOREVIVA_TEST_DSN` creates and cleans up `loreviva_test_<24位小写十六进制>` test databases. Without that variable, tests fail rather than automatically skipping; the account should have permissions only within this restricted test scope. Actual CLI tests also use `LOREVIVA_TEST_GO_EXE` to specify the executor. These are test-fixture restrictions; the service's `LOREVIVA_DB_ADDR` is set through configuration. Check results are specific to the published copy.

Return to [home](README.md) · View [requirements and status](02-requirements.md)
