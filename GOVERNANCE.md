# Governance

This document explains who decides what, how decisions are recorded, and how
someone becomes a maintainer. It is deliberately small; governance grows with the
contributor base.

## Roles

| Role | Responsibilities |
| --- | --- |
| **Maintainer** | Merges to `main`, owns the contract, moderates conduct, publishes releases |
| **Reviewer** | Reviews pull requests in an area of expertise |
| **Contributor** | Proposes and implements changes through pull requests |

The maintainer of record is the repository owner. The live list is
[`.github/CODEOWNERS`](.github/CODEOWNERS).

## Decisions

1. **Reversible technical decisions** are made in the pull request; the
   description and review thread are the record.
2. **Contract, schema and security decisions** need a written specification and
   two independent reviews before implementation. The specification records the
   problem, the alternatives considered and the accepted trade-off.
3. **Release decisions** belong to the maintainer. A release requires a green full
   gate, a current changelog, and an acceptance boundary in `PUBLICATION.json`
   that states what is and is not accepted.
4. **Conflicts of interest** are disclosed in the pull request; a maintainer with
   a conflict does not approve that change.

## Becoming a maintainer

Sustained, high-quality contributions over several months — reviews, fixes and
design work — are the usual path. A maintainer proposes the addition and existing
maintainers decide by consensus. There is no fixed quota.

## Review and merge policy

- `main` is protected: pull requests only, linear history, no force pushes, no
  branch deletion.
- One approval merges ordinary changes; contract, schema and security changes need
  two.
- Generated artefacts are reviewed as evidence of regeneration, never edited.
- A change that weakens an assertion to pass a gate is rejected regardless of who
  authored it.

## Changes to this document

Proposed by pull request and accepted with two approvals.
