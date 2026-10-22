# Versioning and releases

Loreviva carries three independent versions. Confusing them is the most common
source of wrong claims about the project, so each has one owner and one home.

| Version | Meaning | Where it lives |
| --- | --- | --- |
| **Product design** | Product scope and intended behaviour | `docs/design/*` (`00-status.md` states the supported range) |
| **Contract** | The machine contract: operations and schemas | `info.version` in `contracts/source/openapi.yaml` |
| **Package format** | `.wepkg` manifest format | `format_version` in `contracts/source/wepkg.schema.json` |

The runtime has no fourth version of its own: the contract version is the
compatibility promise.

## Contract versioning

- **Minor** — additive operations, additive optional fields, new response enum
  members a client must tolerate. Every generated artefact is regenerated in the
  same change.
- **Patch** — clarifications and fixes that do not change the wire shape.
- **Major** — removals, renames, newly required fields, changed meanings. Needs a
  migration note and a deprecation window.

Adding an operation is never "just a route": it changes the operation count, the
capability table, the capability source hash, the profile hashes and the SDK
types. They move together, and `node scripts/check-generated.mjs` must report zero
diff.

## Package format versioning

`format_version` is `1.0`. A change to required members, enumerations or the
content digest definition is a format change: bump the format, keep a reader for
the previous version, document the migration.

## Tags and releases

- Tags are annotated `vMAJOR.MINOR.PATCH`, created **only** for accepted releases,
  on a commit that passed the full gate.
- No tag exists today: R0 is not accepted, the build is a synthetic-Mock
  development snapshot, and tagging it would claim more than the evidence shows.
- A release also updates `CHANGELOG.md`, the acceptance boundary in
  `PUBLICATION.json`, and the status chapter of the product design.

## Changelog discipline

`CHANGELOG.md` follows Keep a Changelog. Unreleased entries land with the pull
request that changes behaviour; the maintainer moves them under a version at
release time.

## Deprecation

A deprecated operation or field stays functional for at least one minor version, is
marked in the contract description, and is listed in the changelog. Removal waits
for the next major.
