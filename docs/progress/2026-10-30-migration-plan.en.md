# Complete migration-plan confirmation

Migration confirmation now binds the current database and every pinned migration hash in its complete ordered sequence. The previous implementation enumerated ten migrations but hashed only the first nine, omitting0010. The repaired identity is derived from the whole collection. Versions must be consecutive and hashes valid; applied/unapplied flags represent progress and do not change content identity.

An earlier incomplete confirmation ID is rejected. Read a fresh plan and explicitly confirm that current ID before invoking upgrade. No upgrade is started automatically. Known missing marker table1146 remains supported for planning; other unavailable schema-status reads do not produce a confirmable ID. This batch changes no migration SQL, table, public contract or model behavior. Contract1.11.0 and migrations0001–0010 remain unchanged.

Actual verification first reproduced the omission through the existing MigrationPlan entry on a dedicated synthetic MySQL installation, then passed with all ten original raw-source hashes. The old nine-item confirmation is refused; the already-applied current plan is a no-op. A missing-marker policy check uses the owned synthetic fixture and does not establish a full new installation, data upgrade or restore. Pure checks cover every hash, database identity, order, append/remove, malformed sequences, applied-progress invariance and known-vs-unknown status failures.

This is a repair of existing confirmation integrity before future source-lineage migrations. Protected document provenance/save, persistent extraction, Task11 paired backup/restore and complete upgrade acceptance, Tasks9–12 and whole R0 remain incomplete. Real-model, human, performance, charging and deployment acceptance is unchanged.
