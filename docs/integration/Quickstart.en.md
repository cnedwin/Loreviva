# Loreviva integration and reference client

The reference client connects to a same-origin backend that maps a local login to an already registered customer, player, world and platform credentials. The platform remains responsible for current permissions, source eligibility, tasks, accounting and final commits on every request. The reference client validates this integration chain; it does not establish deployment readiness or real-model, human-experience or commercial acceptance.

## Environment and entry points

The workspace uses Node 24, its declared pnpm version and the existing lockfile. SDK runtime imports use explicit `.ts` extensions; generated validators use `.cjs`. Running TypeScript in Node does not replace strict type checking. The SDK preserves its original configuration API. Capability discovery is separate from the 65 machine-source operations: a typed operation does not imply that a server has mounted it, an account can use it, or permission has been granted.

From a workspace with dependencies already prepared:

```sh
pnpm check:reference
pnpm test:reference
pnpm build:reference
```

`check:reference` checks the SDK, backend, integration types, browser target and Vue. `test:reference` uses the reference-client's actual Vitest projects to collect Node protocol tests and jsdom page-state tests; it does not run the real-platform browser target. The build is written to `apps/reference-client/dist`. The backend copies this output into a bounded static-resource map rather than exposing a general file proxy. The build currently has a large main JavaScript chunk; release review must record its actual size and loading impact.

Platform and customer databases require separate, explicitly owned test identities and lifecycles. The real end-to-end target is `TestReferenceClientBrowserFixture`. It starts the original platform, customer program, backend and `reference-player-real-loopback` page target and checks actual commits in both databases. Running the browser test directly does not create these prerequisites. The capture owner also enforces budgets, generated-material scanning, process return and private evidence. Build or jsdom success is not real-browser success.

## Backend initialization and startup

The entry point is `examples/thin-backend/reference-bff.ts`. `initialize` reads one closed `JournalOptions` JSON line from standard input and creates a new private journal installation. `serve` reads one `ReferenceBffOptions` JSON line and keeps standard input open. After that first line it accepts only `STOP` followed by a newline; closing standard input also initiates cleanup. The commands are:

```sh
pnpm reference:initialize
pnpm reference:serve
```

The runtime owner supplies real configuration through a private pipe. These commands do not prompt interactively or create platform authority. Exported types in that file and `command-journal.ts` define the exact structure:

| Configuration | Responsibility |
| --- | --- |
| `platformOrigin`, `port` | Platform HTTP(S) origin; the primary backend listens only on `127.0.0.1:27185` and fails if occupied |
| `journal.root`, `installation` | Independent private installation directory and identity; initialization is distinct from reopening |
| `journal.windows` | Actual Windows helper path, byte fingerprint, native file identity and current user SID; mode bits or a boolean cannot replace DACL verification |
| `users` | Verified login-credential digest and exact tenant/product/customer/serviceSubject/player/externalSubject/entitlement/world/profile/actions mapping |
| `serviceKey` | Backend custody only; never browser state, page storage or public configuration |
| Optional `customer` | Actual pinned customer executable and private database connection for each customer/world; exposes only real confirmed counts and states |

Keep the initialized key and journal as one installation. Reopening does not recreate a missing key, original record or association; an unrecoverable original operation remains UNKNOWN. The journal implements a single writer, sequence and authenticated encryption, size/record caps, and actual write/sync/Close before first send. It does not prove power-loss durability, detection of complete deletion or valid-prefix rollback, or network exactly-once delivery.

## Login, personal permissions and mutations

The browser holds only a same-origin HttpOnly login cookie and current CSRF context. Platform Service keys and CLIENT bearers remain in the backend. Requests validate Host, Origin, CSRF and the current person mapping. Logout, account changes, revocation and view invalidation cancel old requests, clear private caches and revoke media URLs. Late results cannot populate a new identity's view.

Obtain the current personal session and capabilities before INITIAL, RENEW or REVOKE. A declared `PRIVATE_RESPONSE` label is not permission to use private content. Memory lists default to currently visible personal metadata. Explicit provenance content additionally requires acquired sources and current content permission. A default single-item response is `Memory` for `public_v1` and `PlayerMemoryMetadata` for `integration_r0_v1`; explicit `include=provenance_v1` returns `MemoryProvenanceV1`.

Current CLIENT private-body and provenance proof covers initial material, TURN input accepted by the owning writer, committed derived memories and their provable CORRECT chains. Input and derived batches receive body-free birth evidence in the original acceptance or commit transaction, bound to the original task, receipt, complete membership and acquired sources. Reads still require current personal consent, body permission, use and source eligibility, and final verification. Under those conditions a player can inspect memories after interaction, then correct, renew consent or delete. RENEW neither grants missing permission nor restores cleared text or revoked sources.

An older instance without original producer evidence is not backfilled from current memory snapshots, IDs or later requests. Permitted metadata and necessary governance keep their original eligibility; explicit body and provenance requests reject when proof cannot be completed. Evidence retained after deletion or correction contains governed commitments and relationships, without recovering old text; body-free records remain private data. This commitment chain relies on the original owning writer and database trust boundary, rather than an external signature against coordinated rewriting of the entire database.

CREATE, CONTROL, quote, TURN, personal consent, memory governance and cancellation all use the same broker. After trusted mapping and source-type validation, the backend freezes the final method, target, key, profile, raw body and hash. It sends those same bytes only after journal persistence. Callers cannot supply an arbitrary platform URL, raw-body callback or reconstructed sending permit. A backend association does not expand CLIENT authority.

Memory correction and deletion use the original MEMORY_GOVERNANCE_ACCEPT → PROGRAM_OWNED chain and display the actual accepted TaskView. Necessary cleanup may continue after positive-use revocation when current personal governance eligibility permits it. Governance tasks do not inherit the ordinary TURN cancel button. Without an original task association, the result remains UNKNOWN; private SQL lookup or automatic resending is not a recovery path.

## Lost responses and view recovery

Before first send, the browser retains the original operation/profile/key. If the entire first response is lost, `POST /bff/find-original` looks up only an existing exact journal association for the current person. It creates no record, performs no platform business query and sends no first request. A missing-record 404 remains UNKNOWN. An association leads to the existing original/queryOriginal path. Lookup and original queries share a budget of at most 20 reads, at least one second apart, respecting a longer Retry-After. Changing keys, refreshing the page or restarting the backend must not manufacture a new business send.

VIEW_RESET discards the old view and cursor and requires a fresh snapshot. Polling does not extend cursor, task or session deadlines. SSE advances accepted position only after complete-frame validation. A partial-frame EOF, invalid UTF-8, oversized frame or late event does not update the view. JSON errors, SSE and media binary responses have separate decoders; 416 is the source-defined JSON Error.

## Media and customer delivery

Every playback fetches bytes under current authorization. Service keys never enter an `<audio src>`, and no additional signed URL is created. Playback binds authorized bytes to the current identity epoch, uses a Blob URL, and revokes it on stop, account change, revocation or close. Private responses use no-store. A 206 fragment does not establish a complete-body hash.

Task COMMITTED, visible text, media READY and customer confirmation are separate states. Customer Inbox and Message first commit in one customer-database transaction; only then is the original receipt sent to the platform. The UI may display only actual confirmed counts and states. Customer-database UNKNOWN or platform-receipt UNKNOWN must query the original identity rather than creating another Message, media item or notification.

## Verification and known limits

The real-browser evidence for this package is limited to the actually verified Chrome version on Windows with a dedicated temporary profile. It does not establish compatibility with every browser or environment. Model output and media are synthetic test materials, without real-model purchases or human acceptance.

Release review must still retain termination and explanation of RUNNING after an accepted small quote causes the original worker to return `HARNESS_CONTEXT`, and main JavaScript chunk size. Validation of TURN input and original Mock empty and nonempty derived batches does not establish arbitrary external models, unknown producers or every recovery branch; missing legacy evidence still denies body access as described above. Missing and cross-person original lookups return the same opaque local 404 without exposing an original record. That outcome is not upstream success and cannot trigger a business resend.

Source-byte checking uses an explicit inventory. After edits, additions or removals, the controlled `scope:refresh` command and explicit delta produce a new immutable revision, followed by independent `check:bytes`. Checking never silently recomputes expectations. This is not a Git commit bridge, public-distribution approval or product acceptance.
