> 当前安全文档预览与剩余范围见[文档提取](progress/2026-10-30-documents.zh.md)。完整导入和Task9/R0仍未验收。

# Documentation

English is the primary language; every English document has a Chinese companion.

## Design

| Chapter | English | 中文 |
| --- | --- | --- |
| Support boundary | [00-status](design/en/00-status.md) | [00-status](design/00-status.md) |
| Product overview | [01-product-overview](design/en/01-product-overview.md) | [01-product-overview](design/01-product-overview.md) |
| Requirements | [02-requirements](design/en/02-requirements.md) | [02-requirements](design/02-requirements.md) |
| Architecture | [03-architecture](design/en/03-architecture.md) | [03-architecture](design/03-architecture.md) |
| Setup and integration | [04-setup-and-integration](design/en/04-setup-and-integration.md) | [04-setup-and-integration](design/04-setup-and-integration.md) |
| Roadmap | [05-roadmap](design/en/05-roadmap.md) | [05-roadmap](design/05-roadmap.md) |
| Documentation style | [06-documentation-style](design/en/06-documentation-style.md) | [06-documentation-style](design/06-documentation-style.md) |
| Concurrent worlds | [07-concurrent-worlds](design/en/07-concurrent-worlds.md) | [07-concurrent-worlds](design/07-concurrent-worlds.md) |

Current package workflow: [English](progress/2026-10-29-transfer.en.md) · [中文](progress/2026-10-29-transfer.zh.md).

## Operations

| Topic | English | 中文 |
| --- | --- | --- |
| Requirements and status | [02-requirements](en/02-requirements.md) | [02-requirements](02-requirements.md) |
| Architecture | [03-architecture](en/03-architecture.md) | [03-architecture](03-architecture.md) |
| Setup and integration | [04-setup-and-integration](en/04-setup-and-integration.md) | [04-setup-and-integration](04-setup-and-integration.md) |
| Progress notes | `progress/*.en.md` | `progress/*.zh.md` |

## Contract

The single source of truth is `contracts/source/openapi.yaml` together with
`contracts/source/wepkg.schema.json`. Everything under `contracts/generated/`,
`internal/generated/` and `packages/sdk-ts/src/generated/` is generated and is
never edited by hand.

## Documentation rules

- English first; the Chinese document is a companion, not a second meaning.
- State the supported range and the acceptance boundary, not an ambition.
- Distinguish "declared in the contract" from "implemented and verified".
- Reference files and operations rather than describing them in prose.
