# Changelog

## Reference validation and source inventory

- Resolve the reference UI contract fixture relative to its module from either launch directory.
- Include byte-preserved upstream licenses in explicit source scopes and reconcile the published file inventory.
- Keep full Task8 and R0 acceptance pending; historical expiry results retain their source applicability.

Notable changes to this repository. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[VERSIONING.md](VERSIONING.md).

## Unreleased

### Added

- `.wepkg` static preflight, end to end: container inspection (encryption,
  symbolic links, special files, headers that lie), path safety, manifest and
  schema validation, file coverage and digest verification, `content_sha256`
  recomputed exactly as the format defines it, reference and dependency checks,
  registered capabilities, declarative rule primitives, and a deterministic report
  whose digest ignores archive packaging.
- Contract **1.6.0**: `POST /world/v1/admin/content-packages/preflight` and
  `GET /world/v1/admin/content-packages/capabilities`, with package report,
  finding, limits and capability schemas (67 operations / 181 schemas).
- Concurrency model: an original attempt's proof is split into named parts, so a
  memory another task committed no longer invalidates the attempt, while a cited
  memory that changed is still refused.
- `scripts/run-integration-shards.mjs`: the integration package runs in concurrent
  shards with per-shard temporary directories, retries failures serially and
  labels load-sensitive results instead of hiding them.
- Governance: English-first contribution, security, conduct, governance, versioning
  and support documents, each with a Chinese companion; issue and pull request
  templates; code owners.

### Changed

- A recovery commit compares the conflict subject rather than the whole conflict
  document, so an approved recovery survives a further third-party commit.
- `/capabilities` and task cancellation no longer require every worker adapter to
  be ready; they are control-plane reads and controls.
- The schema migration lock is scoped to its database, so independent databases no
  longer serialise behind one server-wide lock.

### Fixed

- Integration shards reject failed processes/packages, incomplete or inconsistent
  results; JSON serial retries require explicit pass evidence and return exit 2
  for recovered failures. Separate logs are flushed before reading a full summary.
- Bilingual status now distinguishes implemented SDK/preflight code from pending
  Task8/R0 acceptance, with 67 operations and 181 schemas in the working contract.

- Three migration assertions that assumed the newest schema was 7.
- `/capabilities` answered 403 for a runtime with a missing rehearsal adapter.
- An approved recovery was refused with a generic conflict when the cited resource
  moved again.

## 1.5.0 — development snapshot

Task 8 integration, SDK, reference client and personal-memory governance. Working
contract 1.5.0 (65 operations / 177 schemas). Not a release.

## 1.4.0 — approved foundation

Tasks 1–7: content authoring, world instance control, compatible migration, memory
governance, bounded Mock harness, candidate and isolated rehearsal, native Vue
Studio and local settings. Contract 1.4.0 (58 operations / 156 schemas).
