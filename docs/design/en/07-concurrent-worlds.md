# 07 · Turn verification in a concurrent world

## Premise

Several tasks advance inside one world instance and affect each other: a memory or world action committed by task B changes what task A sees afterwards. "The world at commit time differs from the world at intake" is therefore **normal**, not an error.

## Verification model

An original attempt's generation proof is frozen at send time and made of named parts:

| Part | Covers | Requirement at commit |
| --- | --- | --- |
| Source origin | Input source, turn command hash | Exactly equal |
| Actor core | Stable core of the actor (mutable presentation excluded) | Exactly equal |
| Skill inputs | Skill set allowed for this turn | Exactly equal |
| Memory set | The memories this turn **actually used**, with body, sources and custodian | Recomputed over the original node set, then exactly equal |
| World perception | Perceivable state of the world instance | Equal, or the original perception is still allowed |
| Route | Generation route | Exactly equal |

The total hash (`ContextHash`) is still recorded and exposed, but it is **no longer the only equality check**: it also changes when another task adds a memory this turn never used.

## Drift classes

| Class | Condition | Handling |
| --- | --- | --- |
| No drift | Every part above is equal | Normal commit |
| Unrelated drift | Only memories or sources this turn never used were added | Normal commit; no recovery |
| Cited drift | A cited memory or world resource changed and the candidate no longer holds | Candidate resource conflict, then recompute and approval |
| Identity or authority drift | Authorization, lease, budget, perception no longer allowed | Refused (403/409 semantics unchanged) |

## Recovery

- Recovery recomputes from the frozen provider result and consumes one further call, which the approval record must show explicitly.
- **R0 limitation**: recovery requires no third-party commit in between. A third commit between approval and commit still yields a conflict refusal. Lifting that needs drift generations, deferred to a later version and not claimed for R0.
- The birth record accepts a recovery base that shares the same origin (source, core, route), because the drift comes from the same concurrency premise.

## Privacy and governance

- Cited memories are still verified node by node, body included; a rewritten body under an unchanged revision **must** be detected and refused.
- Consent, permission and audience are re-evaluated against database time on every read, and are not relaxed by this model.

## Status

Implemented in the 1.5.0 working code (component proof, same-origin birth check) and covered by the positive case of `TestIntegrationTurnMemoryProofFinalAndRecovery` (real competitor → conflict → recompute → birth record) together with its negative control (a rewritten cited body must be refused).
