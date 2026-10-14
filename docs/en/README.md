Draft reading view of the locally approved Tasks 1–7 source. Public distribution and rights review remain pending; this document is not release acceptance.

# Loreviva

**The AI Platform for Living Worlds**  
Create a world. Let it keep growing.

Loreviva aims to let creators combine world rules, characters, roles, Skills, knowledge, and model configurations to make evolving interactive content, with customers connecting through their own clients.

This draft is based on the locally approved **Tasks 1–7**: content authoring, world instance control, compatible migration, memory governance, a persistent Mock Harness, candidates and isolated rehearsals, plus native Vue Studio and local settings administration. Only synthetic Mock models and Mock billing are currently allowed. The complete customer SDK, installation release, long-term experience with real AI, and production SaaS have not been delivered or accepted; final distribution scope must be pinned by a `PUBLICATION.json` that passes the public gates.

## Current scope

| Status | Capability |
| --- | --- |
| Implemented | Single-application API/Worker, persistent task foundations, idempotent commands, commit receipts, and current authorization checks |
| Implemented | Content drafts, revisions, association impact checks, publication preflight, immutable releases, and pinned dependencies/model routes |
| Implemented | Private file upload, authenticated download, and recoverable cleanup tasks |
| Implemented | World and actor instances; start/pause/resume/end; declarative action validation and commit; compatible content migration |
| Implemented | Scoped memory, sources and acquisition, private relationship boundaries, use policies, and correction/deletion governance |
| Implemented | Bounded Mock Harness, recovery by querying the original attempt, draft candidates and isolated rehearsals, necessary cost records, and separation of results |
| Implemented | Native Vue Studio, limited administrative client, local settings, and member/self-consent entry points |
| Planned | Complete client SDK and integration examples |
| Planned | Content package import/export, metering management, complete backup/recovery, and release validation for real models/paid hosting |

The Go application serves native Vue Studio at `/studio/`; `/` retains its startup-inspection role. The machine contract is **1.4.0, 58 operations / 156 schemas**, with embedded migrations 0001–0007. Reserved OpenAPI operations do not mean registered, usable endpoints; an implemented internal TURN does not mean customer player HTTP interaction is connected. See [architecture and endpoint owners](03-architecture.md) for the actual boundary.

The sustained character growth and deeper memory requirements in development design 3.0.2 remain future design work; the 12 LC requirements and 24 scenarios have not been implemented or run. The complete R0 product workflow, human experience, R1 real models, billing, and deployment remain unaccepted.

## Local startup

Requires **Go 1.27.2, Kratos 3, MySQL Community LTS, Node.js 24, and pnpm 11.25.0**. Studio uses Vue; after frontend changes, run `pnpm build:studio` to regenerate the real embedded assets before building the Go application.

First follow the [setup and integration guide](04-setup-and-integration.md) to configure the database, private file directory, and secret references in environment variables. The application does not automatically read `.env`, and there is no default administrator password.

To rebuild Studio from source, first run from the repository root:

```powershell
pnpm install --frozen-lockfile
pnpm build:studio
go build ./cmd/loreviva
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

`bootstrap` is only for a new synthetic installation; it cannot replace recovery or credential retrieval. The coffee shop seed defines Alex, Mia, and the wooden sign custody rule and creates a content project/draft/release. It **does not create a runtime instance, player session, or real model request**.

## Documentation and development entry points

- [Product overview](01-product-overview.md): vision, character relationships, and lightweight scope.
- [Requirements and implementation status](02-requirements.md): completed kernels and unfinished product workflows.
- [Technical architecture](03-architecture.md): modules, persistence, and connected HTTP capabilities.
- [Setup and integration](04-setup-and-integration.md): configuration, initialization, administrative APIs, and SDK boundaries.
- [Sole maintained OpenAPI source](../../contracts/source/openapi.yaml) and [content package Schema](../../contracts/source/wepkg.schema.json): the basis for fields; the [TypeScript package](../../packages/sdk-ts/src/index.ts) contains generated types, configuration wrappers, and a limited administrative client, not the complete customer SDK.

These public documents are curated distribution views of the maintainer's current product specifications, not a separate authoritative knowledge base. Product goals and this implementation batch's status are described separately; generated artifacts must not be edited by hand to create another field source.

```powershell
pnpm install --frozen-lockfile
pnpm generate:contracts
pnpm test
go test ./...
```

Public contract tests use two fixed historical baseline fixtures in the repository, retaining comparisons against the original 18 operations/43 DTO and 1.2's 49 operations/123 Schema. Database integration tests additionally require a privately configured restricted test account and disposable databases; see [test boundaries](04-setup-and-integration.md#development-checks). Each published copy is governed by its corresponding check results.

Original code uses [Apache-2.0](../../LICENSE); see [contributing](../../CONTRIBUTING.md), [security reporting](../../SECURITY.md), and [third-party licenses](../../THIRD_PARTY_NOTICES.md).

Task8–12 are outside the approved scope of this batch and have not been accepted; LC, real models, human experience, the complete release, billing, and deployment remain pending.
