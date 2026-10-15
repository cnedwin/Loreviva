> Product design v3.0.2. This document defines goals and requirements. This branch still contains Tasks1–7 / contract1.4.0 source, without the in-development1.5.0 features. See [repository support scope](00-status.md) for implemented capabilities and setup.

# Loreviva Setup and Integration Guide

Design v3.0.2; R0 execution baseline v3.0.1. Development is in progress and the complete product has not passed acceptance. See [Project State](00-status.md) for progress, supported scope and verification boundaries. Living-character (LC) capabilities are later design.

Loreviva — The AI Platform for Living Worlds  
Create worlds, and let them keep growing.

This guide explains setup and integration for design v3.0.2 and the R0 v3.0.1 execution baseline. It shares product behavior and technical constraints with the [Product Overview](01-product-overview.md), [Product Requirements](02-requirements.md), [Technical Architecture](03-architecture.md) and [Implementation Plan](05-roadmap.md). Machine fields use the sole contract source.

## 01 Confirm What Can Be Done Now

Engineering code and generated types do not by themselves establish an installable product. A deployment must verify its exact application, database, dependencies, contract hash and target environment. The steps below describe the required workflow; actual support is limited to the accepted artifact and measured scope.

## 02 Current Design and Contract Checking Paths

Use the [Product Requirements](02-requirements.md) for behavior, the [Technical Architecture](03-architecture.md) for invariants, and the [Implementation Plan](05-roadmap.md) for task dependencies. Design v3.0.2 and the R0 v3.0.1 execution baseline have different roles. Prior documents remain traceability material rather than alternative current specifications.

Before integration, check object and scope completeness, acceptance/generation/adjudication/commit boundaries, approved packets, idempotency, pause/revocation/UNKNOWN recovery, package permissions and the authority retained by customers. The only machine sources are `contracts/source/openapi.yaml` and `contracts/source/wepkg.schema.json`. Static document/Schema checking does not prove server connectivity or product acceptance.

The engineering root is `<repository>`. Shared `contracts/generated`, Go types, TypeScript SDK and contract documentation are generated from the sole `contracts/source` source, with root/source-hash checks. Do not maintain copied contracts, a second DTO set or another public protocol authority. The SDK, Studio, reference client and thin backend use `packages/sdk-ts`, `apps/studio`, `apps/reference-client` and `examples/thin-backend` respectively. A directory or generated type is not evidence of complete installation or release.

## 03 Target Setup Flow and Current Verification Boundary

This section describes the target installable-release workflow. M1 covers the engineering root, common contracts, configuration, migrations and startup skeleton. Its health/version checks do not replace M2–M4 creator/player flows or non-maintainer installation acceptance.

Default components are one Loreviva application and one MySQL Community LTS database. Vue static administration ships with the application; private local files use its authenticated proxy. Linux community Engine or native databases are options, without mandatory Docker Desktop, paid cloud or platform accounts. The engineering baseline uses Go1.27.2/MySQL9.7.2, Node24 LTS and Vue3.5.43; exact compatibility follows verified artifacts and lockfiles. Pin platform artifacts and container digests for each release, without `latest`. Existing hashes and locks do not prove a new-machine installation.

Obtain verified source or release artifacts with SHA256, versions, SBOM and rights files. Confirm the operating system, ports, disk capacity, database compatibility and permissions, then follow the artifact-specific installation instructions. A source repository is not an accepted product installer. Create project-specific databases and accounts; migration and application roles use only their required permissions. Do not reuse unrelated databases or real private conversations as seed.

The following configuration example contains nonsecret defaults and secret references. It is a design example, not a verified loadable installation configuration; the released `.env.example` defines actual supported keys:

```dotenv
LOREVIVA_HTTP_ADDR=127.0.0.1:8080
LOREVIVA_DB_ADDR=127.0.0.1:3306
LOREVIVA_DB_NAME=loreviva_demo
LOREVIVA_DB_USER=loreviva_demo
LOREVIVA_DB_PASSWORD_REF=env:LOREVIVA_DB_PASSWORD
LOREVIVA_FILES_ROOT=./var/private-files
LOREVIVA_MODEL_MODE=mock
LOREVIVA_BILLING_MODE=mock
LOREVIVA_PUBLIC_BASE_URL=http://127.0.0.1:8080
```

Installers inject actual database passwords through controlled environments or secret-management mechanisms, never source, share packages, screenshots, logs, or examples. Real model credentials are also server-side secret references only; business objects reference connection IDs. New installations default to local-only listening. Network exposure separately configures TLS, trusted proxies, and authentication. Startup checks complete configuration, no public listing of private directories, database Schema compatibility, and deletion-record watermark. Failures remain unready and errors give correlation IDs without passwords.

The following command interfaces are design examples, not verified installation commands. Use an option only when it exists in the chosen artifact and its release instructions verify it:

```powershell
Set-Location '<repository>'
pnpm --dir apps/studio install --frozen-lockfile
pnpm --dir apps/studio build
go run ./cmd/loreviva migrate status
go run ./cmd/loreviva migrate plan
go run ./cmd/loreviva migrate up --plan-id <已核验计划ID>
go run ./cmd/loreviva seed coffee --mock --tenant demo-local
go run ./cmd/loreviva --mode all
```

Independent engineering should report local Git top level as `<repository>`, with readable dependency locks, migration plans, Schema version, application version, and contract hash. seed must be idempotent: repeated identical synthetic seed versions return original records; a new independent world requires explicit copying. Unknown default administrator passwords are prohibited. Initial administrator setup uses a local one-time process. Credentials never enter seed/content packages; later operations need login and explicit scope.

all mode provides both API and persistent-task execution for single-machine use. Target split commands as needed are `go run ./cmd/loreviva --mode api` and `--mode worker`. Both processes share database/domain contracts; api serves static assets. Splitting processes changes no lease, fence, idempotency, or common governance barrier and does not immediately require Redis, queues, or microservices. Shared storage is assessed for multiple nodes. Copying processes does not make local directories a shared-resource guarantee.

<a id="04-the-first-world-task-after-r0-completion-not-executed"></a>

## 04 The First World Task After R0 Completion

A novice chooses the synthetic “rainy-night café” template from Workbench and creates their own world project. The object tree contains café, entrance, seats, wooden sign, and two actors Alex and Mia. The creator confirms background text, then checks the sign's ownership, Alex's custodial authority, and Mia's proposal-only constraint in rule forms; these cannot be combined into a non-executable prompt paragraph.

On stage, change seats, entrances, and visible regions; on character cards, change goals/expression and inspect reference effects. Current R0 uses Mock candidates clearly labeled “synthetic result,” with field differences previewed before adoption. Normal property forms remain available. Saving creates draft revision, cancel discards unsaved modifications, and rehearsal uses isolated instances. Creators may create/copy/edit/archive/delete objects. Copies cannot take actual player relationships, accounting, tasks, credentials, or approval status.

Publication checks list required responsibilities, positive/negative rules, references, Skill versions, rights, audience, Mock routes, call limits, and fallback. Publication creates immutable release. Starting an instance is the next step, without automatic running on publication. Old instances keep old release; new draft edits do not rewrite historical facts. Default operation has one main respondent actor and one main text candidate jointly outputting behavior, dialogue, presentation intent, and memory/relationship candidates. Structural repair/state-conflict recomputation jointly share at most one extra slot. R0 verifies this with Mock counts, without actual model procurement yet.

Players see authorized snapshots/characters before interaction. Café scripts cover private disclosure “I prefer quiet,” public sign repair, refused borrowing without custodian permission, sourced promises, and return. Dialogue “I have given you the sign” does not transfer items itself. Successful operation presentation references established events. Alex's private cognition does not become Mia's/public knowledge. Players may view/correct their preferences, forget related experiences, and see governance receipts.

On pause, the interface explains that new narrative generation/commits stop while already-sent costs/external receipts may still register. Reentry after closing the browser first queries original turn, then gets a snapshot or continues valid cursor, without another identical unknown interaction. Task cancellation, client playback stopping, world pause, and refund each have independent results. Demonstrations record creator completion rate, assistance, duration, failures, and disconnection recovery. One internal demonstration cannot replace non-maintainer installation/novice acceptance.

## 05 Switching from Mock to Real Models

R0 defaults to no external sends. Before enabling actual models, freeze tested purpose, data audience, provider/actual model, input/output bounds, procurement responsibility, budget, unknown-cost recovery capability, retention policy, and human-evaluation tasks. Enable only after users or authorized customers confirm budgets/material external-send scope.

Connection capability checks cover structured output, context, tools, multimodality, cancellation, original-attempt querying, usage, and costs; “compatible with a general API” is not proof of every capability. Ordinary actor routing resolves task override, main actor, world, then workspace. Shared director/other methods merge into context; uncalled models cannot be shown as executed this turn. Missing capabilities block corresponding features. Permitted fallback may be chosen only after confirming the main connection unavailable before first send.

Actual requests reserve call slots/budgets and obtain send authorization in a short transaction under the complete dispatch governance barrier. Before networking, send_started_at persists under the same barrier. Only a definite commit receipt and exit from locks allow sending; automatic SDK retry is disabled. SENT means entering a possible-send interval, not provider receipt. After a crash, even if bytes might never have left, query original attempt rather than automatically resending. AUTHORIZED alone with no send marker cannot access the network. Takeover rechecks the barrier under original attempt; slots/reservations do not reset. Claiming a worker lease is only a short task/fence update transaction. After releasing it, dispatch barrier must be entered again; a lease is not external-send permission. Pause/revocation committed first blocks later authorization. Requests authorized first may still incur costs; local result commits recheck and cannot guarantee cross-system recall. Timeout, 5xx, or lost receipts enter original-attempt query/UNKNOWN flows without automatically switching model and buying again. Actual character quality discloses model/content/language/device scope and evaluates stable positions, different experiences, refusal, sourced memory, correction/deletion, promises, and return. Working flows still prove only corresponding engineering flows.

## 06 External Customer Identity and Thin Server Integration

Customers retain authority over login, product eligibility, business permissions, player wallets, and charging. Loreviva operates in tenant/product/world-instance/actor-instance/player/relationship scopes. An external user ID is not a default cross-product personality key. Identity mapping uses explicitly specified external-ID rules within tenant/product. Empty values are rejected, case/byte comparison rules frozen, and complete unique constraints/query filters consistent.

Target integration: customer clients log into their own business; thin customer backends verify sessions, eligibility, allowed instances/actions; backend service identity requests `POST /world/v1/client-sessions`; Loreviva issues short-lived sessions bound to subject/product/instance/actions/expiry; clients consume approved interfaces with generated SDK. Clients hold no customer server/model credentials and do not write wallets, inventories, official grades, or world facts. Token-exchange examples demonstrate synthetic identity only; customers verify real login within their own systems.

OpenAPI 3.1.2 and JSON Schema 2020-12 define public contracts; Go/TypeScript types and schemas are generated with source hashes. SDK and thin-backend support follows the actual accepted release. No published npm package is assumed: do not run `npm install loreviva` unless that package and version have been verified. HTTP examples below use synthetic objects. An authorized backend creates and STARTs an instance, obtains a Mock quote, then exchanges a player session bound to it. Service credentials never enter browsers; actual IDs and versions replace examples.

<a id="real-r0-customer-create-prerequisites-and-current-support-boundary"></a>

### R0 customer-create prerequisites and support boundary

Customer creation follows InitializeLocal/explicit bootstrap, genuine Operator/init registrar, customer/service registration and authorized WORLD/dependency publication, REGISTER_PLAYER_ACCESS, genuine SERVICE PRIVATE single-player creation, then a Session. Publication precedes player-access registration. Do not fabricate release, participant, permission or receipt rows through SQL. Create neither STARTs the world nor grants positive use; its SERVICE response contains no initial private body.

Prepare legitimate SYNTHETIC/OWNED PRODUCT_READERS World/Character and KNOWLEDGE/NOTE content with COPY/PUBLISH/REDISTRIBUTE, no expires_at, empty refs and no extraction locations. Preserve actual origins rather than erasing old provenance to make fixtures pass. Complex copies, community-licensed roots and real document extraction remain unsupported by this bounded initial-copy bridge and fail the whole create with safe field locations until their genuine source bridges exist. Old Admin semantics remain.

Two worlds for one player follow real E1→W1→a new E2 on the original mapping→W2, without renewing mapping/identity/permissions/expiry. Multiple unbound same-release candidates are a real conflict, with no extra API selector or LIMIT1 selection. Unknown original-key ACKs query the original result rather than inventing new keys.

<a id="s67-actual-norm-adoption-and-unimplemented-boundary"></a>

### Registration time-hash compatibility

REGISTER_CUSTOMER/REGISTER_PLAYER_ACCESS use complete UTC commands under domain loreviva.registry.registration.utc.v2; other kinds are unchanged. Existing V1 keys match only original typed raw input without a V1UTC fallback. Hashes, deadlines and revisions remain immutable, without renewal or new authority. Complete historical proof supports known old-UTC V1/V2UTC. Missing-command old-non-UTC history is UNVERIFIABLE and automatic R1/CREATE reconstruction is NOT_SUPPORTED; safe queries and exact-input replay remain separate.

Server creates an instance; `scope.world_instance_id` in the 201 receipt supplies the later path ID. Initial state is CREATED:

```http
POST /world/v1/instances
Authorization: Bearer <server_service_credential>
Idempotency-Key: demo-create-1
Content-Type: application/json

{
  "product_id": "product_coffee",
  "release_id": "release_mock_1",
  "mode": "PRIVATE",
  "initial_participants": [
    "player_demo"
  ],
  "client_capabilities": [
    "TEXT"
  ]
}
```

Server issues START; the 202 receipt is TaskReceipt. Use its task_id with the same ServiceBearer for `GET /world/v1/tasks/{task_id}`. After task_state is COMMITTED, query `GET /world/v1/instances/{instance_id}` to confirm RUNNING/read current versions, then continue quotes/session exchange. operator queries return only authorized state/version/permission metadata, not private player packets. 202 acceptance is not already running:

```http
POST /world/v1/instances/instance_demo/controls
Authorization: Bearer <server_service_credential>
Idempotency-Key: demo-start-1
Content-Type: application/json

{
  "command": "START",
  "expected_control_version": "0"
}
```

Server requests a synthetic quote and uses returned quote_id/frozen bound; the example is not an actual rate:

```http
POST /world/v1/quotes
Authorization: Bearer <server_service_credential>
Idempotency-Key: demo-quote-1
Content-Type: application/json

{
  "scope": {
    "tenant_id": "tenant_demo",
    "product_id": "product_coffee",
    "world_instance_id": "instance_demo"
  },
  "service_kind": "TURN_TEXT",
  "primary_actor_id": "actor_alex",
  "input_upper_bound": "1000",
  "output_upper_bound": "200",
  "purchase_responsibility": "MOCK"
}
```

After verifying its player's session/entitlements, the thin backend exchanges a short-lived token. tenant derives from service credentials; bodies cannot impersonate another tenant. Player sessions have no START/END permission; PAUSE/RESUME apply only to authorized private instances:

```http
POST /world/v1/client-sessions
Authorization: Bearer <server_service_credential>
Idempotency-Key: demo-session-1
Content-Type: application/json

{
  "product_id": "product_coffee",
  "world_instance_id": "instance_demo",
  "external_subject_id": "synthetic-player-1",
  "entitlement_reference": "entitlement_demo",
  "requested_actions": [
    "READ",
    "TURN",
    "CONTROL_PAUSE",
    "CONTROL_RESUME",
    "MEMORY_VIEW",
    "MEMORY_GOVERN"
  ]
}
```

Clients use short-lived tokens to obtain authorized snapshots from `GET /world/v1/instances/instance_demo/snapshot`, then submit interaction. world_revision/control_version use exact snapshot strings; expected_versions does not include privacy authorization:

```http
POST /world/v1/instances/instance_demo/turns
Authorization: Bearer <restricted_client_session>
Idempotency-Key: demo-turn-1
Content-Type: application/json

{
  "expected_versions": {
    "world_revision": "1",
    "control_version": "1"
  },
  "primary_actor_id": "actor_alex",
  "input": {
    "kind": "TEXT",
    "text": "我想坐在安静的位置"
  },
  "quote_id": "quote_demo",
  "requested_audience": "PRIVATE_RELATIONSHIP"
}
```

This TypeScript example illustrates wrapper calls; exact supported signatures follow the selected artifact and generated contract. It is not a standalone executable integration example. Provider cost, service quota and customer player entitlements remain separate.

```typescript
// instanceId and quoteId come from the authorized thin backend; use the released call signatures.
const sdk = createLorevivaClient({ baseUrl, sessionToken });
const snapshot = await sdk.getSnapshot({ path: { instance_id: instanceId } });
const accepted = await sdk.submitTurn({
  path: { instance_id: instanceId },
  headers: { "Idempotency-Key": turnKey },
  body: {
    expected_versions: {
      world_revision: snapshot.versions.world_revision,
      control_version: snapshot.versions.control_version
    },
    primary_actor_id: "actor_alex",
    input: { kind: "TEXT", text: "我想坐在安静的位置" },
    quote_id: quoteId,
    requested_audience: "PRIVATE_RELATIONSHIP"
  }
});
const turn = await sdk.getTurn({ path: { turn_id: accepted.turn_id } });
// 202仅受理；消费approved_packets、deliveries和billing的独立状态。
```

Public contracts include instance creation, controls, snapshot, turns, original-task queries, events, cancel, and restricted memory governance. Instance creation returns 201 without automatic start. Read instance ID from `scope.world_instance_id`. Control bodies use uppercase `START/PAUSE/RESUME/END` and exact string `expected_control_version`. Interaction returns 202 without success implication; receipts contain turn_id, task, quote_id. Idempotency key is request Header `Idempotency-Key`, scoped by tenant/subject/operation/instance/key. Same key/same normalized payload returns original task; same key/changed parameters returns 409 `IDEMPOTENCY_CONFLICT`. Network retries by the same player reuse the original key. Reperforming requires a new explicit business action.

## 07 Disconnection, Concurrency, SSE, and Private Views

After request timeout, query original turn first. Tasks may be ACCEPTED, RUNNING, WAITING_EXTERNAL, UNKNOWN, COMMITTED, FAILED, CANCELLED, or REVOKED. A successful HTTP query only means current state was found. COMMITTED actions cannot be resubmitted because clients did not play them or media failed. `EXTERNAL_UNKNOWN` requires original-attempt queries, `ALREADY_COMMITTED` requires displaying established results, and `BUDGET_EXCEEDED` requires stopping new costs. A generic “retry” button must not change keys and duplicate procurement.

Snapshot/starting cursor share one consistency boundary. cursor is opaque and bound to subject, tenant, product, instance, view epoch, and validity. Continuation safely advances even without visible events, without exposing raw sequences/private gaps. `VIEW_RESET` or expired cursor clears old view caches and fetches authorized snapshots anew. Keeping old caches displayed after revocation is wrong. `SNAPSHOT_STALE` requires checking latest state; clients do not independently decide action recomputation.

Target SSE carries only safe notifications or approved delivery updates. After packet loss, original-task queries/cursor recovery remain authoritative. SSE is not the sole fact source; disconnects do not trigger duplicate generation. Deduplicate events by event identity, actions by packet/action identity, and delivery updates by delivery_id/packet_revision. Older versions cannot overwrite newer ones. Late audio updates are not discarded because text already appeared. Manual replay is not another world action.

When two players compete for the unique wooden sign, the server establishes only one result under current permission/state/version. Preferences privately told to Alex, derived invitations, and relationship conditions inherit original audiences. Permission for public actions does not automatically permit publishing the source. Resources use authenticated proxies; private directories cannot map to unauthenticated static URLs. Client-saved bytes cannot be guaranteed remotely recallable; deletion commitments must state this boundary.

## 08 .wepkg, Skills, and Extension Tasks

.wepkg format version 1.0 saves logical definitions, rules, Schema, knowledge/assets, fixed dependencies, provenance rights, and synthetic tests. It carries no local model addresses, secrets, absolute paths, actual identities, private chats, runtime states, or bills. Import proceeds through upload, static preflight, content/rights preview, ID mapping, isolated draft, Mock rehearsal, local publication. Preflight does not call models, install tools, or start worlds. Package instructions cannot obtain system privileges.

Repeated imports of identical package ID/version/hash return original records. Same version/different hash shows conflict; a new version produces a difference draft. Missing models/tools/assets locate objects; users choose compatible local mappings and reverify, without silently switching providers. Importers reject out-of-bounds paths, disguised types, excessive total size, wrong hashes, cycles, and unknown executable primitives. Actual limits are disclosed by implementation-time contracts/release configuration. Runtime backups use another controlled format and cannot circulate as community templates.

Declarative Skills contain ID, version, hash, input/output Schema, purpose, capabilities, license, tool allowlist, dependencies, parameters, trigger, failure strategy, cost, and cases. Community authors may write methods/synthetic examples within registered primitives, without text installing programs or broadening authority. Skills/child Skills share parent calls, time, and costs; recursion cannot reset quotas. Added executable tools undergo separate source review, registration, and regressions.

Model extensions implement capability descriptors, Generate, QueryAttempt, and usage-receipt interfaces. Presentation extensions read approved packets and declare text/action/audio capabilities. Model adapters cannot directly commit worlds; presentation adapters cannot write facts through playback ACK. 3D and other client adapters receive separate acceptance under declared capabilities and are not R0 defaults. Community self-hosting needs no platform top-up, online license, or paid cloud.

## 09 Five-Industry Synthetic Examples and Independent Verification

R0 defines the following synthetic specification examples, without claiming five-industry production support. Each fixes initial state, input, expected results, permissions, recovery, and client responsibilities. All assertions report independently; a single café success cannot cover other industries.

| Synthetic industry example | Differences requiring testing | Authority retained by customers |
| --- | --- | --- |
| Game: sign-borrowing task NPC | Same-item competition, advice versus established action, unknown external inventory results | Login, gameplay outcomes, inventory, economy |
| Interactive film/game: rainy-night branch | Choice window, branch saves, shot intent, asset fallback, return | Editing, devices, assets, interaction windows |
| Companionship: quiet seat | Private preferences, correction/deletion, reasonable refusal, disabling invitations, relationship continuity | Relationship boundaries, proactive channels, subscriptions, charges |
| Education: café language practice | Course provenance, teacher/student isolation, advisory feedback does not write official grades | Graders, grades, classes, authorization |
| Community: shared activity | Public/private views, eligibility, trigger deduplication, members leaving do not pause the world | Membership, public resources, content, capacity |

Real industry support executes difference specifications at its release gate. If the first paying customer needs sharing, educational grading, or external side effects, corresponding verification moves before R2; sold capabilities cannot wait until R3 for completion.

## 10 Upgrades, Backups, and Recovery After Deletion

Application patches, database Schema migrations, and content-instance upgrades have separate plans/receipts. Before maintenance, record version, contract hash, DDL version, file adapter, and effects; stop new narrative/cost work while recording already-sent attempt receipts/governance. First generate migration preflight, compatibility/capacity reports, then paired database/file backups; verify before execution. Existing content instances retain old release and do not automatically change personality on application upgrades.

Target M4 maintenance commands are unexecuted: `backup create --output ./var/backups` creates paired records of database snapshot, private-file manifest, hashes, Schema, and governance watermark; `restore preflight --backup <ID> --deletion-ledger <受控路径>` checks pairing, compatibility, latest watermark, available space; `restore apply --plan-id <ID>` executes only verified plans. Authentication and plan-ID formats of concrete commands must be confirmed through engineering contracts before publication.

Latest effective revocations/deletions in DeletionLedger are preserved independently of old backups. Recovery first keeps access closed, obtains records no lower than current authority watermark, replays post-backup deletions/revocations, cleans original text, summaries, indexes, relationships, and media derivatives, then verifies DB/file pairing, permissions, retrieval, and proxies do not redisplay deleted material. Missing new watermarks, damaged file manifests, or incompatible Schema block reopening. A backup's own Ledger cannot prove it is sufficiently current.

Missing COMMIT receipts query stable commit keys on authoritative primary databases. Unknown transactions are not failed transactions warranting another model call. Only explicit database failure rolls back the whole transaction and permits bounded retry under stable keys. Configuration rollback cannot erase established events, costs, payments, or deletions. Irreversible migrations use previously rehearsed recovery paths. Backups contain private material; access, retention, export, and destruction require corresponding permissions. Drills use synthetic data first.

R0 maintenance implementation scope (current rescue and continuity rules, actual Task11 testing pending): on trusted current original database/private root, close and drain every participant, seal complete current rescue set C, then reconstruct and catch up latest governance G/verify original Mock requests J. Open after both sealing conditions. Old B serves only isolated drills or repairing same-hash files currently specified by C; deletion logs cannot restore post-backup world/accounting progress. If the current database is lost without trusted complete terminal material, show BLOCKED_POST_SNAPSHOT_EFFECTS_UNPROVEN, without zeroing calls or repurchasing. Independent preservation domains are not overwritten with DB/files. C is private governed material. Concrete original plan/key queries, C sealing, DB/files stages, and all/api/worker drain commands await actual Task11 CLI freeze; this guide invents no executable flags. It claims neither complete disaster recovery nor recovery of never-preserved history; missing non-maintainer evidence remains unaccepted.

## 11 Fault Diagnosis and Submittable Evidence

| Symptom | Check first | Correct next step |
| --- | --- | --- |
| Startup unready | Configuration, DB connection, Schema, private directory, Ledger watermark | Fix prerequisites and retain error correlation ID, without exposing secrets |
| Created world does not respond | Started instance, control version, task state, worker lease | Query original task; paused state sends no new generation |
| Same key returns 409 | Original payload hash, operation, instance | Query original result or explicitly new action; do not alter original idempotency key |
| Task UNKNOWN | attempt, provider-query capability, commit key, cost state | Query original request/commit, without automatic repurchase |
| Missing events/audio | cursor, VIEW_RESET, delivery version, media state | Rebuild authorized views/fetch updates, degrade to approved text |
| Private resource 404 | Current subject, product, audience, governance version | Treat invisible, without resource-existence details |
| import failure | Object location, hash, dependency, path, license | Modify isolated draft/map dependencies, leaving old release |
| Still closed after recovery | Pair verification/latest DeletionLedger replay | Obtain authoritative new watermark and reverify; do not force opening |

Support evidence includes application/contract versions, synthetic reproduction steps, task/event/attempt/commit correlation IDs, control/privacy epoch, fence, public error codes, and sanitized logs. Do not submit Token, Cookie, database passwords, model keys, or original private bodies. Logs describe related runtime facts; a log excerpt cannot establish long-term character quality or commercial effects.

## 12 Acceptance Before Enabling SaaS Three-Layer Tokens

R0 supplies only cost substitutes/synthetic ledgers; real top-ups, payments, and collections are disabled. Provider raw usage, platform Tokens, and customer player entitlements use three ledgers/authoritative sources without shared balances. Platform Tokens are service quotas and customers hold player wallets. BYOK does not resell customer-direct model payments again; self-hosting requires no platform top-up.

Before R2, fix customer service SKU, versioned rates, input/output/media categories, included quantities, exact amounts/rounding, quote bounds, settlement triggers, reservation_settle_by, risk responsibility/limits, authoritative payment receipts, refund/chargeback/renewal rules. Amounts use DECIMAL or integer smallest units, APIs exact strings, service events idempotent settlement. Actual duplicate provider charges are individually retained as anomalies. Routes without trusted bounds, payment verification, or finite reservation deadlines cannot automatically send externally with charging.

At UNKNOWN customer-reservation deadline, authorized accounting appends platform-risk posting, releases customer reservation, and creates reconciliation tickets in one transaction. Provider safety budget remains occupied; attempt/call counts do not reset. Late costs default to platform without secret extra deductions. Late bodies still recheck current permission, pause, cancellation, and deletion. Refunds, provider refunds, and player refunds do not automatically link. Enable corresponding charging scope only after duplicate/out-of-order callbacks, unknown COMMIT, revocation, recovery, capacity, reconciliation, and actually supported industries all pass acceptance.

<a id="13-administration-configuration-preflight-and-authorized-task-directory-s59-task6-locally-approved"></a>

## 13 Administration Configuration Preflight and Authorized Task Directory

Fixed OperatorBearer/content.read preflight shows approved configuration, supported fields, and per-item bounds. RESOLVED does not mean generation permission; actual AI assistance uses original content.assist/current execution eligibility. task.read directory contains only safe ADMIN/currently authorized ordinary WORLD TURN metadata; detail bodies, source use, and cancellation have independent authorization. product_id is required, optional exact task_id; exact queries cannot also carry cursor. limit defaults to 50, maximum 100. Empty items with next_cursor require continuation. Each page reauthorizes under current eligibility. Identity/governance/filter binding changes reject old cursor; restart invalidates old cursor and clients query from the first page. This is live keyset; seeing new items before an already-passed key requires requerying first page. WORLD query_url returns to the same directory with actual product_id/task_id; ADMIN returns original detail operation. New fields/generated SDK follow actual frozen sole contracts/source, without guide-private fields or a third interface.

<a id="14-permission-consistency-from-administrative-task-directory-to-detail-s60-task6-locally-approved"></a>

## 14 Permission Consistency from Administrative Task Directory to Detail

Existing getAdminTask offers safe metadata only for currently authorized actually registered ADMIN, requiring task.read/current owner/exact product_id/installation governance. can_execute=0 does not prevent progress viewing. Actual TURN/isolated children cannot use ADMIN detail queries; WORLD directory links return to exact product_id/task_id in the original directory. Original intake-registration proof must hold. Committed result references additionally require actual business-commit/asset-binding proof. result_asset_id is only an original-result reference. Opening bodies, generation, and cancellation retain separate permissions. Missing/unauthorized details give safe 404; global identity errors 401/403; infrastructure failures cannot masquerade as empty/success. Detail reading adds no independent interface or private DTO.

## 15 Creating Living Characters and Integrating Personalized Experiences

This later-design workflow follows PRD section26 and whitepaper section16. Unfrozen LC fields and operations are not current SDK calls. Implement only under a specific versioned contract and verify the actual supported scope.

Creators first design stable character core/differentiation examples, then choose currently supported change policies: which experiences change which states, who is affected, recovery, and what cannot change. Configure an interaction with clear consequences and a rejectable opportunity. Each uses normal property forms and synthetic positive/negative examples. AI may propose candidates but cannot save/publish/change actual instances on creators' behalf. Before publishing, check policy/primitive support tables and actual rehearsal evidence.

Use synthetic players to test first entry, shared experiences, a preference correction, refusal of an opportunity, and return. Compare core, current understanding, unfinished matters, valid world consequences, and responses while checking other players/actors cannot see private information. Consequence preview separates proposal, establishment, and display; stage animation is not world-operation completion. Draft saving, fixed-version publication, and instance start remain separate actions.

Customer integration first confirms current identity/product/instance through a thin backend, then consumes sole-source generated types. Clients offer current-turn expression choices, authorized experience/preference viewing, correction/forgetting, opportunity controls, pause/return, and precise feedback. They cannot maintain another relationship score/cognition/provenance authority. Stop playback affects only this endpoint; world pause/material governance use separate commands. Account switching clears old views/caches; late results retain original identity/current permission checks.

Instance/theme/world capability vocabulary is mapped/negotiated by actual support first. Text is the base projection. Missing voice/stage capabilities degrade under approved rules; unknown mandatory capabilities cannot be enabled solely by client self-report. World establishment, persistent customer messages, resource availability, and endpoint playback show separate states. History synchronization does not replay by default; late audio does not repeat world events. Original-result replay/new generation have different procurement meanings.

Issue feedback defaults to categories/precise delivery references. Sharing bodies separately explains purpose, expiry, and audience. Authors prefer original synthetic reproductions; personalization corrections/global content revisions take separate paths. Every change fixes tested characters, rules, Skills, models, assets, and policies, compares deterministic failures/actual generation quality, then publishes. Existing personality changes first inspect semantic-migration conflicts; appearance-update paths cannot reset experiences.

Integration claims state actual tested capabilities separately: private text, specific voice routes/devices, shared-membership policies, choice branches, generated media. Unexecuted items are NOT_RUN; template examples, saved configurations, or reference-code existence are not support. Complete workflow/non-maintainer operations still undergo actual tests under existing acceptance contracts. Original private material never enters community packages/public issue attachments.
