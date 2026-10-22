# Contributing to Loreviva

Thanks for taking the time to contribute. This document describes how work moves
from an idea to `main`, and what a reviewable change looks like here.
中文版见 [CONTRIBUTING.zh-CN.md](CONTRIBUTING.zh-CN.md).

## What we can accept

| Change | Accepted when |
| --- | --- |
| Bug fix with a regression test | The test fails before the fix and passes after |
| Contract or schema change | Starts in `contracts/source/`, regenerated artefacts committed, counts and capability tables updated |
| New runtime capability | Has a written spec, two independent reviews, and real verification against a disposable database |
| Documentation | Improves accuracy or translation; English is primary, Chinese is the companion |
| Tooling change | Keeps the byte policy, supply-chain policy and zero-diff generation green |

We cannot accept: fabricated test results, assertions weakened to make a gate
pass, credentials or private data, database dumps, generated runtime artefacts,
or acceptance claims the evidence does not support.

## Workflow

1. **Discuss first** for anything larger than a bug fix: open an issue describing
   the creator or integrator problem, not only the proposed solution.
2. **Fork and branch.** Branch names: `feat/<topic>`, `fix/<topic>`, `docs/<topic>`.
3. **Change the source of truth first.** Interface changes begin in
   `contracts/source/`; run `pnpm generate:contracts` and commit the generated
   files in the same change.
4. **Test what you touched.** Unit tests for logic; integration tests against a
   real disposable MySQL for anything that reads or writes state. Never use SQL to
   manufacture a state the product cannot produce.
5. **Run the gate that matches the change** (see the README). The full sharded
   gate runs before merge to `main`.
6. **Open a pull request** using the template: behaviour change, verification
   evidence, remaining limitations.

## Review

- One maintainer approval merges ordinary changes; contract, schema and security
  changes need two.
- Reviewers check behaviour and evidence, not style preferences.
- A red gate is a red gate: fix the cause, do not widen the assertion.

## Commits and history

Conventional-commit subjects (`feat:`, `fix:`, `test:`, `docs:`, `chore:`) with a
body that explains **why**. History on `main` is linear; pull requests are rebased,
never merge-committed.

## Licence

Copyright 2026 Loreviva contributors. Contributions are accepted under
[Apache-2.0](LICENSE); you keep your copyright. Third-party material keeps its own
licence and must be recorded in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
