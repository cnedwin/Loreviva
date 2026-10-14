Draft reading view of the locally approved Tasks 1–7 source. Public distribution and rights review remain pending; this document is not release acceptance.

# Loreviva Product Overview

**The AI Platform for Living Worlds — Create a world. Let it keep growing.**

Loreviva is for interactive worlds with explicit backgrounds and rules: characters have stable personalities and develop memories, commitments, and relationships from experiences they have actually acquired; the program maintains world facts, while AI produces inspectable candidate content.

This document is a curated public view of the current product specifications. The vision below does not mean every workflow is available; see [requirements](02-requirements.md) for this implementation batch's status and [setup and integration](04-setup-and-integration.md) for the installation scope.

## What creators need to accomplish

The intended workflow is: select a template or describe a background → edit the world/characters/rules → bind roles, Skills, knowledge, and models → rehearse in a sandbox → pin a release version → create and run instances.

World Studio goals include forms, an object tree, property panels, and a 2D/2.5D Creator Canvas. Task7 implements native Vue forms and an SVG scene view for draft editing, scene positions, resource mappings, and association impacts; this does not establish acceptance of every intended canvas capability. It does not provide a complete 3D engine, general-purpose MMO, or VR toolchain. Character previews must still distinguish fixed personality and appearance settings, historical approved results, and new random interpretations without secretly changing runtime facts.

Each generation module should offer an optional editing assistant and model configuration, while ordinary forms remain usable on their own. When AI converts background text into rules or proposes changes, the creator first reviews the differences and impacts, then still explicitly saves after adoption; saving a draft, publishing a version, and creating an instance are separate operations.

Task7 connects draft editing, optional Mock editing assistance, checks, publication, instance operations, and isolated rehearsal to native Studio, with local settings, member actions, and a separate self-consent entry point. Controlled automated browser workflows use actual local services and synthetic Mock data; they do not establish human usability, real-model quality, or complete R0 acceptance. Candidates remain constrained by current eligibility, routes, sources, and input validation and do not automatically save drafts; rehearsals do not rewrite official runtime instances.

## Boundaries of four object types

| Object | Meaning |
| --- | --- |
| World project/definition | An authoring container and its background, scenes, rules, characters, and other content |
| release | A pinned version of content, dependencies, and generation routes; draft edits do not rewrite it |
| World instance | The actual runtime state, events, and control state of a release |
| Character template/actor instance | A reusable personality definition, and that character's presence in a particular world instance |

Copying a template or placing the same character in another world does not copy player identity, private memories, relationships, runtime facts, or accounting. Instance upgrades require separate preview and execution; resetting history cannot remove incompatibility.

## Characters, memory, and relationships

A character's personality includes values, goals, expression style, knowledge boundaries, and reasonable refusal. What a player tells a character, public events the character observes, and the character's own inferences need to retain their sources; they must not be merged into unconditional facts.

Shared worlds can share an actor's public identity and public experiences already witnessed; each player still has a separate private relationship layer. A relationship expresses "how the character understands the target subject," not the player's actual psychology, and does not automatically create a reverse relationship.

Read permission does not mean the character has acquired the information, nor that it may express it publicly. Memories and goals derived from private sources inherit their private audience; revocation, correction, and deletion must block subsequent use and continue cleanup. Recall of copies already delivered cannot be promised.

Tasks 1–6 implement scope, acquisition, governance, and a bounded Mock interaction kernel; automatic natural relationship generation, sustained personality growth, and real memory effects still need R1 real models and human evidence. Synthetic Mock tests cannot replace acceptance of those effects.

## Roles and Skills

Intended presets include director, scriptwriter, actor, makeup/styling, performance/voice, stage operations, memory/relationships, and quality review. Roles can be combined, replaced, or performed by the program; covering responsibilities does not require every role to call a model.

A Skill is a method with a version, inputs/outputs, source permissions, capabilities, and cost boundaries. General and specialized methods can bind to different roles; community imports cannot install arbitrary tools, add permissions, or bypass the parent task's budget.

The intended default for online generation is one primary responder's single main text generation; necessary structural repair or state-conflict recomputation share at most one extra text call. Other routine planning, review, and memory organization cannot each add text calls. This batch's Mock Harness has been verified against this boundary; real model calls and their effects remain unaccepted.

## Content composition and sharing

Worlds, characters, directors/roles, Skills, knowledge, presentation resources, and multiple model configurations combine into different works. Model connections are mappable capability requirements; a content package cannot require another installation to send data to the original author's address.

The planned `.wepkg` contains logical definitions, assets, licenses, dependencies, synthetic examples, and assertions, while excluding secrets, player data, and runtime state. Import first performs static preflight, then creates an isolated draft for rehearsal; the first phase focuses on sharing files and repository directories, with a commercial plugin marketplace deferred. A [format Schema](../../contracts/source/wepkg.schema.json) is available now; the complete import/export workflow is not yet implemented.

## Industries and customer responsibilities

| Target customer | Business boundaries to clarify during integration |
| --- | --- |
| Game platforms/interactive film and games | Confirm world actions, game assets, branch outcomes, and client presentation separately |
| Emotional companionship apps | Private relationships, consent, withdrawal, frequency, and contact information boundaries |
| Online education | Learning goals, evaluation rules, audiences, and data purposes; character dialogue must not be treated as authoritative grading |
| Virtual communities | Shared facts versus personal relationships, membership eligibility, content governance, and notification channels |

The engine aims to handle worlds, AI, memory, budgets, and recovery so customers can focus on interactive products. Customers remain responsible for authentication, user eligibility, industry rules, player orders/wallets/payments/refunds, and authoritative receipts from their original business services. Generic kernel tests cannot replace industry adaptation and its acceptance checks.

## Community and hosting roadmap

The aim is for the self-hosted community core and hosted services to share a kernel. Self-hosting does not require platform top-ups or mandatory cloud services; hosted metering must distinguish raw supplier usage, platform service allowances, and customer player business balances.

Results, necessary costs, and allowance records for original Mock requests have been verified separately; local settings and member interfaces are included in Task7. Real models, metering management pages, billing/payment workflows, complete recovery, and the first paid release gate remain to be implemented and verified. The current Mock environment provides neither production SaaS nor player billing capabilities. Task8–12 implementation, LC long-term effects, human validation, and the complete release remain pending.

Return to the [project home](README.md) · View [architecture](03-architecture.md)

Task8–12 are outside the approved scope of this batch and have not been accepted; LC, real models, human experience, the complete release, billing, and deployment remain pending.
