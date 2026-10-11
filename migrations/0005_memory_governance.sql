-- R0 retained caps enforced by typed memory UoWs: 1000 sources, 1000 nodes,
-- 2000 acquisitions, 2000 grants, depth 32, 64 combined parent/source refs,
-- aggregate serialized JSON <=16 MiB, 1000 lineage rows including unbound stages.
-- Overflow refuses the whole transaction. Retained history is never pruned.
-- Independent journal limit is 32 MiB. G0 reserves 512 KiB per completion.
CREATE TABLE IF NOT EXISTS memory_installation (
 singleton TINYINT NOT NULL PRIMARY KEY, installation_id VARBINARY(64) NOT NULL,
 generation BIGINT UNSIGNED NOT NULL, database_binding VARBINARY(64) NOT NULL,
 files_binding VARBINARY(64) NOT NULL, coordinator_binding VARBINARY(64) NOT NULL,
 CHECK(singleton=1 AND generation>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS memory_aggregates (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL, data_subject_id VARBINARY(64) NOT NULL,
 aggregate_id VARBINARY(64) NOT NULL, revision BIGINT UNSIGNED NOT NULL,
 document JSON NOT NULL,
 PRIMARY KEY(tenant_id,product_id,world_instance_id,data_subject_id),
 UNIQUE KEY memory_aggregate_identity(tenant_id,product_id,world_instance_id,aggregate_id),
 FOREIGN KEY(tenant_id,product_id,world_instance_id) REFERENCES world_controls(tenant_id,product_id,world_instance_id),
 FOREIGN KEY(tenant_id,product_id,data_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 CHECK(revision>0)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS memory_subject_permissions (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL,
 caller_subject_id VARBINARY(64) NOT NULL, data_subject_id VARBINARY(64) NOT NULL,
 action VARBINARY(64) NOT NULL, allowed_use VARBINARY(32) NOT NULL,
 expires_at DATETIME(6) NOT NULL, enabled BOOLEAN NOT NULL,
 PRIMARY KEY(tenant_id,product_id,caller_subject_id,data_subject_id,action,allowed_use),
 FOREIGN KEY(tenant_id,product_id,caller_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id),
 FOREIGN KEY(tenant_id,product_id,data_subject_id) REFERENCES subject_governance(tenant_id,product_id,subject_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS memory_governance (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL, data_subject_id VARBINARY(64) NOT NULL,
 owner_subject_id VARBINARY(64) NOT NULL, request_hash BINARY(32) NOT NULL,
 binding JSON NOT NULL, preservation VARCHAR(32) NOT NULL, cleanup VARCHAR(32) NOT NULL,
 PRIMARY KEY(tenant_id,product_id,task_id),
 FOREIGN KEY(tenant_id,product_id,task_id) REFERENCES tasks(tenant_id,product_id,task_id),
 CHECK(preservation IN ('PENDING','CONFIRMED','BLOCKED_NEEDS_REPLACEMENT')),
 CHECK(cleanup IN ('PENDING','LOCAL_COMPLETE'))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS memory_replay (
 installation_id VARBINARY(64) NOT NULL, ledger_sequence BIGINT UNSIGNED NOT NULL,
 ledger_hash VARBINARY(64) NOT NULL, task_id VARBINARY(64) NOT NULL,
 state VARCHAR(32) NOT NULL,
 PRIMARY KEY(installation_id,ledger_sequence),
 CHECK(state IN ('CONFIRMED','BLOCKED_NEEDS_REPLACEMENT'))
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS memory_asset_lineage (
 tenant_id VARBINARY(64) NOT NULL, product_id VARBINARY(64) NOT NULL, asset_id VARBINARY(64) NULL,
 storage_key VARBINARY(32) NOT NULL, render_id VARBINARY(64) NOT NULL,
 world_instance_id VARBINARY(64) NOT NULL, data_subject_id VARBINARY(64) NOT NULL,
 actor_id VARBINARY(64) NOT NULL, node_id VARBINARY(64) NOT NULL, node_revision BIGINT UNSIGNED NOT NULL,
 source_epoch VARBINARY(64) NOT NULL, lineage_hash VARBINARY(64) NOT NULL,
 delete_intent BOOLEAN NOT NULL DEFAULT FALSE, purged BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY(tenant_id,product_id,storage_key),
 UNIQUE KEY memory_render_identity(tenant_id,product_id,render_id),
 UNIQUE KEY memory_asset_identity(tenant_id,product_id,asset_id),
 KEY memory_asset_scope(tenant_id,product_id,world_instance_id,data_subject_id),
 FOREIGN KEY(tenant_id,product_id,asset_id) REFERENCES assets(tenant_id,product_id,asset_id)
) ENGINE=InnoDB;

-- G-only seal: positive current-C reopening remains Task11.
CREATE TABLE IF NOT EXISTS memory_open_candidates (
 installation_id VARBINARY(64) NOT NULL, generation BIGINT UNSIGNED NOT NULL,
 candidate_id VARBINARY(32) NOT NULL, command_key VARBINARY(128) NOT NULL,
 ledger_id VARBINARY(64) NOT NULL, anchor_id VARBINARY(64) NOT NULL,
 ledger_sequence BIGINT UNSIGNED NOT NULL, ledger_hash VARBINARY(64) NOT NULL,
 PRIMARY KEY(installation_id,generation), UNIQUE KEY memory_open_identity(candidate_id)
) ENGINE=InnoDB;
