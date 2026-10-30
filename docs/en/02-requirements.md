> Current safe-document preview and remaining scope: [document extraction](../progress/2026-10-30-documents.en.md). Full import and whole Task9/R0 remain incomplete.

> Current package approval, export and second-installation status: [installation transfer](../progress/2026-10-29-transfer.en.md). Whole Task9/R0 remain incomplete.

# Requirements and Implementation Status

Current scope (2026-10-29): Tasks1–8 retain scoped local acceptance. Task9 includes persistent isolated import, revision approval, static export/download and second-installation publication. Safe documents, complete Skill sharing and whole Task9/R0 remain incomplete. Working contract1.9.0:75 operations/197 schemas; migrations0001–0010. See [current package verification](../progress/2026-10-29-transfer.en.md).

Loreviva — The AI Platform for Living Worlds.

This document presents the current product requirements as a public reading view, distinguishing the locally approved Tasks 1–8 implementation from workflows yet to be implemented. It adds no contracts or acceptance conclusions; fields are based on [OpenAPI](../../contracts/source/openapi.yaml) and the [content package Schema](../../contracts/source/wepkg.schema.json).

## Implemented in this batch

| Work | Implemented scope | What this still does not establish |
| --- | --- | --- |
| Task1 foundations | Go application, configuration, migrations, health/version/readiness endpoints, and synthetic initialization | Complete installation wizard, Studio, or production deployment |
| Task2 persistent kernel | Command idempotency, task takeover, authorization barriers, commit receipts, and basic budget/dispatch infrastructure | Real supplier procurement or a complete billing workflow |
| Task3 content authoring | Drafts/revisions, association impacts, commands, publication checks, immutable releases, private files, and pinned dependencies/routes | Editing assistants, complete rehearsals, or content package import/export |
| Task4 world runtime | Instances/actor instances, control, declarative action execution rules, and compatible migration | Player natural-language interaction or general industry client integration |
| Task5 memory governance | Scope and sources, actor acquisition, foundations for memory/relationship updates, use policies, and correction/deletion cleanup | Real AI long-term companionship or recall of all historical copies |
| Task6 Mock Harness | Persistent bounded interaction, original request recovery, source context, necessary cost verification, isolated rehearsals, draft candidates, and safe task reads | Real model effects, player HTTP/SSE integration, GUI, complete SDK, or real billing |

Each row describes that task’s own evidence scope; an earlier task’s limitation does not negate the subsequently approved Task7.

| Work | Implemented scope | What this still does not establish |
| --- | --- | --- |
| Task7 Studio | Native Vue editing/scene view, optional Mock assistance, original-task queries, local settings, members, and self-consent entry points | Human outcomes, real models, complete customer SDK, billing, or deployment |

Current configuration accepts only Mock. Tasks1–8 retain their scoped local acceptance. Task9 implements body admission, local matching/preview, atomic durable isolated import and original-receipt recovery. Revision approval, static export/download and second-installation publication are implemented. Safe documents, complete Skill sharing, Tasks10–12 and whole R0 remain incomplete. Historical approved contract:1.4.0 (58/156); working contract:1.9.0 (75/197), migrations0001–0010. Reserved operations do not establish mounted handlers.

## Authoring and related operations

The intended GUI should provide consistent create, edit, save, cancel, copy, archive, delete, and association impact feedback for worlds, scenes, rules, characters, roles, Skills, knowledge, and resources.

- AI suggestions show differences first; adoption still requires explicit saving, and cancellation preserves the original saved version.
- Drafts may save incomplete content and return precise issues; publication must recheck the current revision, dependencies, rights, and supported scope.
- Template copies omit player identities, private chats, private memories/relationships, runtime facts, tasks, and accounting.
- Publication creates a pinned release; instances continue using their original version, and upgrades have separate preview and execution.

Task7 native Vue forms and the SVG scene view share one draft and actual administrative services, providing save/cancel, association-impact confirmation, optional Mock assistance, checks, and publication. Approved controlled automated workflows do not establish human experience or acceptance of every authoring goal; unsupported subsequent capabilities remain unavailable.

## World rules and instance control

Rules need executable, verifiable conditions and actions. Content proposed by an editing assistant is only a candidate rule; the program checks permissions, resources, audiences, and current state before committing. A character saying "handed over" does not establish an item fact. The current optional assistant is limited to Mock; real background understanding and rule quality remain unaccepted.

This batch implements the following closed set of control transitions:

| Operation | Initial state | On success |
| --- | --- | --- |
| START | CREATED | RUNNING |
| PAUSE | RUNNING | PAUSED |
| RESUME | PAUSED | RUNNING |
| END | CREATED / RUNNING / PAUSED | ENDED |

ENDED cannot restart. Retrying the original idempotency key queries the original result without incrementing the control version again. Pause, cancellation, external request completion, and cost verification are different states and cannot replace one another.

Compatible migration is limited to PAUSED instances within the same tenant, product, world project, and definition lineage. Preview and execution bind to the original state and target release; incompatible personality/rule changes must be blocked. Established events, identities, memories, commitments, relationships, and cost history cannot be reset by an upgrade.

## Sources, acquisition, and privacy

World facts, character beliefs, player statements, and memory sources remain separate. Private experiences are isolated by instance, actor, and target subject; to use a source, a character needs both current eligibility and an actual acquisition record.

Administrative memory metadata queries, private body reads, governance, and use policies require distinct permissions. Access to backend tasks or metadata grants neither private body access nor model use rights. Correction/deletion should first block use, then clean up through persistent tasks; status must be truthful, and "accepted" must not be presented as "all deletion complete."

## Intended requirements for models, roles, and Skills

Each generation module should have model inheritance/selection, capability checks, the actual resolved source, budgets, and outbound data scope. Programmatic roles/Skills do not become text generators; where applicable, `NONE` disables generation without silently falling back to another model.

Roles and Skills support versioned bindings, input/output Schema, capability/tool allowlists, licenses, dependencies, parameters, triggers, and failure strategies. Missing dependencies must be traced to the binding, not silently removed from configuration. Methods cannot authorize tools themselves, recursively reset allowances, or purchase extra model calls.

Task7 implements the native editing GUI for the corresponding Mock path; real supplier execution remains a subsequent task. This batch pins content/routes and checks online Skill eligibility; `NONE` stops generation, current permission and actual acquisition must both hold, and the program enforces parent-task call limits and the shared extra slot. Only the Mock path is currently supported; no data is sent to real suppliers.

## Intended requirements for integration, recovery, and metering

The Integration Kit has TypeScript transport, prepared-command ownership, BFF recovery, media and SSE components, plus a thin backend and Vue reference client. Customer identity remains the customer's responsibility. Task8 has scoped local acceptance; see [current gaps](../progress/2026-10-23.en.md).

Task results, service completion, supplier costs, and risk reservations need separate presentation. `UNKNOWN` means the original result awaits confirmation; it does not mean failure, zero cost, or permission to procure again automatically. Successful request acceptance also does not establish a world action. Within synthetic Mock scope, Task6 verifies recovery by querying the original attempt, necessary costs, late costs, and their separation from service results; UNKNOWN keeps querying the original identity without automatically purchasing again. Candidates are scoped to their original draft and source permissions; adoption still requires explicit saving. Rehearsal creates an isolated temporary world and retains FAIL/NOT_RUN and cleanup progress. Administrative metadata visibility does not grant private result body access. Task7 includes local settings interfaces; this engineering evidence does not establish completed metering management pages, customer player workflows, or real commercial billing.

Customers retain authority over player identities, business eligibility, industry goals, and player payments/refunds. Future platform service allowances are separate from player wallets; platform service refunds do not automatically trigger player refunds.

## Required before subsequent releases

| Scope | Work still required |
| --- | --- |
| Complete community workflow | SDK/reference-client source exists; complete integration acceptance, import/sharing and non-maintainer validation remain required. Mock/browser checks do not establish R0 or human acceptance. |
| Real AI | Supplier adaptation, current permission/source checks, real calls/recovery, and character/rule quality evaluation |
| Paid hosting | Metering/accounting, concurrent allowance control, payment/refunds, industry integration, and operational maintenance verification |
| Broader releases | Independent evidence for the corresponding multi-character, media, notification, shared-community, or industry capabilities |

Backup/recovery must protect the database, private files, and independent governance records together; existing governance mechanisms do not mean complete recovery tools are available. Unverified capabilities cannot be promised publicly as "general support."

Return to the [product overview](01-product-overview.md) · View [connected endpoints](03-architecture.md#http-routes-actually-connected)
